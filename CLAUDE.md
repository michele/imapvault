# CLAUDE.md

Working name: **imapvault** (rename freely). An open-source IMAP backup tool:
a Go CLI that keeps point-in-time **snapshots** of a mailbox in a local repository,
holds deleted mail for a grace period, notices mass deletions, and restores any
snapshot, folder or message back to an IMAP server.

Read `docs/PLAN.md` before starting any task. Work on one milestone at a time.

## Stack

- Go (latest stable), single static binary, no CGO
- IMAP: `github.com/emersion/go-imap/v2` (same version as quietbox)
- Index: SQLite via `modernc.org/sqlite` (pure Go, keeps the build static), WAL mode
- Compression: zstd via `github.com/klauspost/compress/zstd` (pure Go)
- Encryption: age via `filippo.io/age` (pure Go), imported only by `internal/seal`
- CLI: `github.com/spf13/cobra`
- Config: one TOML file per account (`github.com/pelletier/go-toml/v2`), same scheme as quietbox:
  `$XDG_CONFIG_HOME/imapvault/<account>.toml`, selected with `--account NAME`
- Ask before adding any other dependency.

## Commands

```sh
make test          # go test ./... -race
make lint          # go vet ./... && golangci-lint run
make itest         # integration tests against the IMAP server in docker-compose.test.yml
make build         # static binary in ./bin
```

## Layout

```
cmd/imapvault/        cobra commands only: parse flags, call engine
internal/config/      TOML config, accounts, password sources (start as a copy of quietbox's)
internal/mail/        Source interface + Folder/Message/Flags types (no IMAP imports here)
internal/imapsource/  implementation of mail.Source using go-imap v2 (read-only + APPEND for restore)
internal/repo/        repository on disk: layout, format version, lock, init/open
internal/seal/        the only age importer: seal/open index values, blob encryption, folder-name HMAC, canary, keygen
internal/objects/     content-addressed blob store: put/get/has/verify, atomic writes
internal/imaptarget/  the only code allowed to write to a server (CREATE, APPEND), fenced to an approved restore plan
internal/index/       SQLite schema, migrations, queries (folders, entries, snapshots, alerts)
internal/plan/        PURE functions: (index state, server listing) -> sync plan; retention policy -> keep set;
                      guard thresholds -> alert. No I/O.
internal/engine/      orchestration: backup, prune, restore, check
```

`plan` must stay pure so it can be table-tested exhaustively (sync diffing, retention,
mass-deletion guard). Commands never touch the repository or the server directly; they go through `engine`.

## Safety rules (non-negotiable)

1. **Backup is read-only on the source.** Open folders with `EXAMINE`, fetch with `BODY.PEEK[]`.
   Never `STORE`, `EXPUNGE`, `MOVE`, `COPY`, `DELETE` or `APPEND` on the source during backup.
2. **Durability order:** a blob is fsynced and renamed into place *before* the index row
   that references it is committed. A crash may leave orphan blobs, never dangling references.
3. **Only `prune` removes data from the repository**, and it always honours pins, the deletion
   grace period and unacknowledged alerts. `prune` supports `--dry-run` and prints what it would remove.
4. **Restore only appends.** It never deletes or modifies existing mail on the target, defaults to
   a separate folder prefix (`Restored/<snapshot>/…`), supports `--dry-run`, and is idempotent.
5. Tests never touch a real mailbox. Integration tests use the docker test server only.
6. Never log message bodies, subjects or credentials. Log hashes, UIDs and folder names.
7. Any change to the on-disk format bumps `format_version` in the repo config and ships a migration
   (or a clear refusal to open newer/older repos).
8. **Backup never needs the private key.** The backup, prune and `check --read-data` paths must not resolve or
   use an identity. What they must read back is plaintext; everything descriptive is written sealed and read only
   by commands that hold the key. Never log or put into an error the bytes of an object or a decrypt failure.

9. **No write to an IMAP server without an approved plan.** Only `internal/imaptarget` may send `CREATE` or `APPEND`,
   and only `internal/engine/restore.go` may import it (a test enforces both, and that nothing else imports go-imap).
   The writer is fenced to the approved plan: it refuses any folder or count the plan did not list. A plan is approved
   by its token (`plan.Approve`), typed at a terminal or passed as `--confirm`; there is no `--yes`. The target
   account's config must set `[restore] allow_append = true`. Restore never sends `STORE`, `EXPUNGE`, `MOVE`, `COPY`,
   `DELETE` or `RENAME`, and strips `\Deleted` from appended flags.

## Identity model

- **Blob key** = SHA-256 of the raw RFC 822 bytes. Same message in two folders, or moved between
  folders, is stored once.
- **Server position** = (folder, UIDVALIDITY, UID). Positions change, blobs don't.
- `EMAILID` (RFC 8474 OBJECTID) is a *download-avoidance hint* only: if a new UID carries an EMAILID
  we already have a blob for, skip fetching the body. Never use it as the blob key.

## Definition of done

A task is done only when ALL of these are true:
- unit tests written first (or alongside) and passing with `-race`
- `make lint` is clean
- the feature has been run end to end against the docker test server
  and you have shown the command output
- README / `docs/PLAN.md` updated if behaviour, config or on-disk format changed

If you cannot verify something, say so explicitly. Do not report it as working.

## Working style

- Start each task in plan mode: propose the approach, list the files you'll touch, wait for approval.
- Small commits, one logical change each, conventional commit messages.
- When you make the same mistake twice, propose a line to add to this file.
