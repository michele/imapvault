# imapvault: plan

## Goal

Back up an IMAP mailbox to a local repository (disk or NAS) with **point-in-time snapshots**,
so that any past state of the mailbox can be inspected and restored. It is developed against
Fastmail but works with any IMAP server. It runs as a one-shot command from cron or a systemd timer.

The scenario it must handle: someone gets into the account, deletes everything and empties
the trash. The next backup run notices, raises an alert, pins the last good snapshot,
and `imapvault restore` puts the mail back.

## Why not existing tools

- **imapsync** copies IMAP to IMAP. It can't write to a local repository and keeps no history.
  If you mirror with `--delete`, a deletion at the source reaches the copy.
- **imap-backup** (Ruby), **imapbackup** (Python), **go-imap-backup** (Go): incremental mbox dumps
  with no snapshots, no flag history and no point-in-time restore.
- **backmail** (git-backed) has point-in-time restore. A git repo of .eml files gets slow at scale,
  and it doesn't understand IMAP flags or UIDVALIDITY.
- **mbsync + restic** is the DIY standard. It works, but restic only sees files: it doesn't detect
  mass deletions, doesn't restore to IMAP, and treats a folder move as delete + add of a file.
- Prior attempt: `../imapogre` (Oct 2025, go-imap v1, YAML config, about 3k lines). It's worth
  skimming for ideas. Don't reuse it: imapvault starts clean with quietbox's conventions.

## Non-goals (for v1)

- Encryption at rest and remote backends (S3/B2). The format must leave room for both (see "Repository format").
- TUI snapshot browser, full-text search, export to mbox/Maildir (see "Later").
- Two-way sync and migration-as-a-feature (restore to another server covers it).
- Contacts and calendars (not IMAP).
- OAuth2 (password / app password only, like quietbox).
- Multiple accounts per repository. One account = one config file = one repository.

## Accounts and config

Same scheme as quietbox: one TOML file per account in `$XDG_CONFIG_HOME/imapvault/`
(`~/.config/imapvault/personal.toml`), selected with `--account personal`. `imapvault accounts`
lists them. Start `internal/config` as a copy of quietbox's (account names, password sources).
If the two projects drift, extract a shared module later.

```toml
[imap]
host = "imap.fastmail.com"
port = 993
username = "michele@example.com"
password_command = "op read op://Personal/fastmail-imapvault/password"
# or password_file = "...", or env IMAPVAULT_IMAP_PASSWORD[_<ACCOUNT>]

[repository]
path = "/volume1/backup/imapvault/personal"

[folders]
include = ["*"]
exclude = ["Spam", "Junk", "+QuietBlackHole"]

[retention]
keep_last    = 3
keep_hourly  = 24
keep_daily   = 14
keep_weekly  = 8
keep_monthly = 24
keep_yearly  = 10
deleted_grace = "180d"   # mail gone from the server stays restorable at least this long

[guard]
max_deleted_percent = 10   # of messages in the previous snapshot
max_deleted_count   = 500
on_alert = "/usr/local/bin/notify-me 'imapvault alert: {account} {summary}'"
```

The repository is bound to the account (host + username) at `init` time and refuses to back up
a different account.

## Repository format

```
<repo>/
  repo.toml            format_version, repo id, account binding, created_at
  objects/ab/cdef…     one blob per unique raw message, named by sha256
  index.db             SQLite (WAL)
  index-backups/       VACUUM INTO copy after each successful run, keep the last 7
  lock                 exclusive while a write (backup, prune, forget) runs
  tmp/                 staging for atomic writes
```

**Blob envelope.** Each object file starts with a 1-byte header: bits for compression (zstd)
and, later, encryption. v1 writes `zstd, no encryption`. Readers must reject unknown bits.
This gives encryption somewhere to go without a format break for old blobs.

**Index (SQLite).** Snapshots are not copies. Each row records an *interval* of validity,
so the index grows with changes rather than with (messages × snapshots).

| Table | Purpose |
|---|---|
| `runs` | id, started_at, finished_at, status (`complete`, `partial`, `failed`), server capabilities seen |
| `snapshots` | id = run id of a complete or partial run, created_at, `pinned`, `flagged`, note, counts |
| `folders` | id, name, delimiter, special-use, uidvalidity, last_uid, highest_modseq, first_seen_run, gone_run |
| `blobs` | sha256, size, internal_date, message_id (header, for display/search), emailid, first_seen_run |
| `entries` | folder_id, uidvalidity, uid, blob, flags, valid_from_run, valid_to_run (NULL = still present) |
| `alerts` | id, run_id, kind, summary, acknowledged_at |

A snapshot at run R = every `entries` row with `valid_from_run <= R` and
(`valid_to_run IS NULL` or `valid_to_run > R`). A flag change closes the old row and opens
a new one. A deletion closes the row. A move closes one row and opens another that points
at the same blob.

## Backup run

Per folder, in a single read-only session:

1. `LIST` with special-use, detect new, renamed (treated as gone + new; blobs dedupe) and vanished folders.
2. `EXAMINE` the folder and compare UIDVALIDITY. If it changed, treat it as a new folder:
   close all entries of the old generation and re-list. Bodies we already have are skipped
   via EMAILID when available, otherwise re-downloaded and deduplicated by hash.
3. **Fast path** (CONDSTORE + QRESYNC, Fastmail has both): `UID FETCH 1:* (FLAGS) (CHANGEDSINCE modseq VANISHED)`
   gives flag changes and expunges since last run.
   **Fallback:** `UID FETCH 1:* (UID FLAGS)` and diff against open entries.
4. New UIDs (> last_uid): fetch `EMAILID RFC822.SIZE INTERNALDATE FLAGS`. Skip the body when the
   EMAILID maps to a known blob. Otherwise `BODY.PEEK[]` in batches, hash, write blob.
5. Commit the folder's changes in one transaction, then update the folder's uidvalidity/last_uid/modseq.

Run-level:
- A run that finishes all folders is `complete`. One that stops partway is `partial`: folders it
  finished are committed, and folders it didn't reach keep their previous entries open.
  It never closes entries it didn't verify.
- After the last folder, run the **guard** (below), write the snapshot, run `VACUUM INTO` for the index backup.
- Consistency is per folder (IMAP has no account-wide atomic view). Document this.

## Mass-deletion guard

Pure function in `plan`: (previous snapshot counts, this run's closures) → alert or none.
Trips when deletions exceed `max_deleted_percent` or `max_deleted_count`, or when a folder that
held messages vanished entirely. The trash being emptied counts as deletions from Trash.

When it trips:
- The snapshot is still written (you want to see the bad state) but is marked `flagged`.
- The previous snapshot is **auto-pinned**.
- An `alerts` row is created, `on_alert` runs, and the command exits with code **3**
  (0 = ok, 1 = error, 2 = partial, 3 = alert).
- `prune` refuses to run while unacknowledged alerts exist. `imapvault alerts` lists them,
  `imapvault ack <id>` acknowledges one.

## Retention: `forget` and `prune`

- `forget` applies the policy (restic semantics: keep_last / hourly / daily / weekly / monthly / yearly).
  Pinned snapshots are always kept. It only marks snapshots as forgotten.
- `prune` deletes `entries` rows that cover no kept snapshot **and** whose `valid_to_run` is older than
  `deleted_grace`. Then it deletes blobs no remaining entry references, along with orphan blobs and tmp files.
- Both support `--dry-run`, and both print counts and bytes.
- Retention is a pure function (`plan.Retain(snapshots, policy, now) -> keep set`): table-test it.

## Restore

```sh
imapvault restore --snapshot latest|<id>|2026-11-20T10:00 \
  [--folder INBOX] [--message <sha-prefix>] [--deleted-since 7d] \
  [--to <other-account>] [--into "Restored/{snapshot}"] [--in-place] [--dry-run]
```

- Default target: same account, under `Restored/<snapshot>/<original folder>`. `--in-place` puts
  messages back into their original folders (creating them if missing).
- `APPEND` with the original flags and INTERNALDATE.
- Idempotent: before appending, check the target folder for the same Message-ID + size
  (or EMAILID) and skip duplicates. Re-running a restore must not create copies.
- `--deleted-since` restores only messages whose entries closed within the window. This is the
  "undo the hack" command.

## Commands (v1)

| Command | What it does |
|---|---|
| `init` | create the repository for an account |
| `backup` | one backup run → snapshot |
| `snapshots` | list snapshots (id, time, counts, pinned/flagged) |
| `ls <snapshot> [folder]` | folders, or messages in a folder (date, from, size, hash). No bodies |
| `show <hash>` | headers of one message. `--raw` writes the .eml to stdout |
| `diff <a> <b>` | added / deleted / moved / flag-changed counts per folder |
| `pin` / `unpin <snapshot>` | protect a snapshot from retention |
| `alerts`, `ack <id>` | guard alerts |
| `forget`, `prune` | retention (`--dry-run`) |
| `restore` | see above |
| `check [--read-data]` | index consistency. With `--read-data`, re-hash every blob |
| `doctor` | connect, print server capabilities (CONDSTORE, QRESYNC, OBJECTID, SPECIAL-USE) and folder sizes |
| `accounts` | list configured accounts |

Global flags: `--account`, `--config`, `--verbose`, `--json` for machine-readable output on list commands.

## Milestones

**M0 Skeleton (done).** go.mod, cobra root, `internal/config` copied and adapted from quietbox, Makefile,
golangci config, dovecot `docker-compose.test.yml` (CONDSTORE/QRESYNC enabled), seed script, CI.
*Done when* `make test lint itest` run green on an empty command set.
*Findings:* the dovecot 2.4.5 test server advertises CONDSTORE, QRESYNC, UIDPLUS and SPECIAL-USE but **not OBJECTID**.
M3's EMAILID download skip therefore gets unit tests with a fake `mail.Source`, and the dovecot itests only cover the
no-OBJECTID fallback. Hierarchy delimiter on the test server is `/`. Config decisions: `repository.path` is required (no
default) and must resolve to an absolute path; `folders.include`/`exclude` globs use `*` across the hierarchy delimiter;
a `0` in `guard.max_deleted_*` turns that threshold off.

**M1 Repository (done).** `init`, `repo.toml`, lock, blob store (envelope, zstd, sha256, atomic write + fsync),
SQLite schema + migrations, `check`. *Done when* blob store has property tests (put/get round trip,
corruption detected) and `init` + `check` work against a temp dir.
*Findings:* `format_version` (layout + blob envelope) and the index schema version (`PRAGMA user_version`, embedded
`migrations/NNNN_*.sql`) are separate numbers. Both are 1 until v0.1.0; after the first release any index migration
also bumps `format_version` (safety rule 7). `Open` refuses a newer index and a read-only open refuses an unmigrated one.
`runs.status` gained `running` (a run in progress), and `snapshots` gained `forgotten` for `forget`. Flock-based lock
(unix only), non-blocking: `check` takes it shared and fails at once while a writer runs. `check` reports orphan blobs,
leftovers in `tmp/` and stray files as warnings (a crash may leave them); missing or corrupt data, a damaged index
and dangling references are errors (exit 1). It only re-reads blobs with `--read-data`. `init` needs no password.

**M2 First full backup.** `mail.Source` + imapsource (EXAMINE, BODY.PEEK, batching), engine writes
entries + snapshot, `snapshots`, `ls`, `show`, `doctor`. *Done when* a seeded dovecot mailbox backs up
and `ls` matches the server exactly.

**M3 Incremental.** New UIDs, flag diff, expunges, folder new/gone/rename, UIDVALIDITY reset,
CONDSTORE/QRESYNC fast path with fallback, EMAILID download skip, `partial` runs, `diff`.
*Done when* itests cover each case, including killing the process mid-run and resuming.

**M4 Guard.** Thresholds, flagged snapshots, auto-pin, alerts/ack, `on_alert`, exit codes, `pin`/`unpin`.
*Done when* an itest that deletes everything and expunges Trash produces an alert, exit 3 and a pinned previous snapshot.

**M5 Retention.** `forget` + `prune` with policy, grace period, pins, alert lock, dry-run, index backups.
*Done when* `plan.Retain` is exhaustively table-tested and prune on a fixture repo frees exactly the expected blobs.

**M6 Restore.** Snapshot / folder / message / `--deleted-since`, `--into` and `--in-place`, `--to` another account,
idempotency, dry-run. *Done when* the M4 "hack" itest ends with the mailbox restored and a second
restore appends nothing.

**M7 Release prep.** README, example configs, systemd timer + cron docs, Dockerfile, goreleaser
(reuse quietbox's setup), first test against real Fastmail (read-only backup only), v0.1.0.

## Testing

- Unit: `plan` (sync diff, guard, retention) as table tests. Blob store and index with temp dirs.
- Integration: dovecot in docker, seeded per test. Scenarios: first backup, new mail, flag change,
  move between folders, delete + expunge, empty Trash, folder rename, folder delete, UIDVALIDITY reset
  (recreate folder), mass delete, crash mid-run, restore idempotency.
- A `testdata/` corpus of tricky messages: 8-bit, huge attachments, missing Message-ID, duplicate
  Message-ID with different content, CRLF/LF variants (hash must be over the exact server bytes).

## Open questions

- **Index durability.** SQLite + nightly `VACUUM INTO` copies is the v1 answer. Should runs also write
  an append-only journal into `objects/` so the index can be rebuilt from the repository alone? That's
  safer, but prune gets more complex. Decide before M5.
- **Gmail.** Labels show up as folders, so blobs dedupe for free. Should we default-exclude `[Gmail]/All Mail`,
  or back up only All Mail plus `X-GM-LABELS`? Not needed for Fastmail; decide when someone asks.
- **Shared code with quietbox** (config, password sources, IMAP dialing): copy now, extract a module once both are stable.

## Later

- Encryption at rest (age, using the reserved envelope bits; index encryption TBD)
- Remote backends: S3-compatible (B2, Hetzner, MinIO), or document repo-on-rclone
- Read-only TUI to browse snapshots (bubbletea): folders, message list, headers, diff between snapshots
- Search: SQLite FTS5 over headers (from, to, subject, date)
- `export` a snapshot or folder to Maildir / mbox
- `mount` a snapshot as a read-only Maildir via FUSE (restic-style)
- OAuth2 (Gmail, Microsoft 365), JMAP source for Fastmail
- Notifications beyond `on_alert` (email via SMTP, ntfy, webhook)
