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

- Remote backends (S3/B2). The format must leave room for them (see "Repository format"). Encryption at rest is **in v1**: see "Encryption".
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

[restore]
allow_append = false       # default. `restore` refuses to write to this account's server unless true

[encryption]               # optional. Decided at `imapvault init`; it cannot be changed later.
recipients = ["age1..."]   # PUBLIC keys (see `imapvault keygen`). Set = the repository is encrypted.
identity_file = "~/.config/imapvault/personal.key"   # PRIVATE key. Only on machines that read mail.
# or identity_command = "op read op://Personal/imapvault-key/private"
# or env IMAPVAULT_IDENTITY[_<ACCOUNT>] holding the key text
```

The repository is bound to the account (host + username) at `init` time and refuses to back up
a different account.

## Repository format

```
<repo>/
  repo.toml            format_version (2), repo id, folder_key, account binding, [encryption], created_at
  objects/ab/cdef…     one blob per unique raw message, named by sha256
  index.db             SQLite (WAL)
  index-backups/       VACUUM INTO copy after each backup run and each prune that changed the index, keep the last 7
  lock                 exclusive while a write (backup, prune, forget) runs
  tmp/                 staging for atomic writes
```

**Blob envelope.** Each object file starts with a 1-byte header: bit 0 = zstd, bit 1 = encrypted.
Readers reject unknown bits and any object without the zstd bit. A plain repository writes `0x01` and the
payload is `zstd(raw)`. An encrypted one writes `0x03` and the payload is `age(zstd(raw))`: compress, then
encrypt. The blob key is always the SHA-256 of the *raw* message, so deduplication works the same in both modes
(age output is randomised, so dedupe keys on the plaintext hash and never on the file).

**Index (SQLite).** Snapshots are not copies. Each row records an *interval* of validity,
so the index grows with changes rather than with (messages × snapshots).

| Table | Purpose |
|---|---|
| `runs` | id, started_at, finished_at, status (`complete`, `partial`, `failed`), server capabilities seen |
| `snapshots` | id = run id of a complete or partial run, created_at, `pinned`, `flagged`, `forgotten`, `note_sealed`, counts |
| `folders` | id, `name_hash`, `name_sealed`, delimiter, special-use, uidvalidity, last_uid, highest_modseq, first_seen_run, gone_run |
| `blobs` | sha256, size, `stored_sha256`, `meta_sealed` (Message-ID, INTERNALDATE, From/To/Subject/Date), emailid, first_seen_run |
| `entries` | folder_id, uidvalidity, uid, blob, flags, valid_from_run, valid_to_run (NULL = still present) |
| `alerts` | id, run_id, kind, `summary_sealed`, created_at, acknowledged_at |

A snapshot at run R = every `entries` row with `valid_from_run <= R` and
(`valid_to_run IS NULL` or `valid_to_run > R`). A flag change closes the old row and opens
a new one. A deletion closes the row. A move closes one row and opens another that points
at the same blob.

## Encryption

Encryption at rest uses [age](https://age-encryption.org) and is chosen once, at `init`. The point of the design
is **asymmetry**: a cron job or NAS that holds only the *public* keys can back up, prune and check integrity, and
cannot read a single message. The *private* key is needed only where mail is read.

| Needs only the public key (recipients) | Needs the private key (identity) |
|---|---|
| `init`, `backup`, `forget`, `prune`, `check`, `check --read-data`, `ack`, `pin`; `snapshots` and `alerts` (without notes and summaries; the `on_alert` hook gets its summary live) | `restore`, `show`, `ls` (names and headers), `diff` (names), snapshot notes and alert summaries, the TUI, `check --decrypt` |

**What is encrypted, and what must stay readable.** Backup reads the index on every run to diff against the
server, so anything it has to read back stays plaintext. Everything descriptive is *sealed*: age-encrypted
into `*_sealed` columns that backup writes and never reads.

| Plaintext | Sealed |
|---|---|
| message bodies are never plaintext: object files are `age(zstd(raw))` | folder names (`name_sealed`) |
| folder id, `name_hash`, delimiter, special-use, UIDVALIDITY, last_uid, MODSEQ | Message-ID, INTERNALDATE, From/To/Subject/Date (`meta_sealed`) |
| entry positions (folder, UIDVALIDITY, UID), blob key, **flags** (the flag diff needs them) | snapshot notes, alert summaries |
| blob size, EMAILID, `stored_sha256` | |
| run and snapshot ids, timestamps, counts | |

**Folder names** are looked up through `name_hash = HMAC-SHA256(folder_key, name)`. `folder_key` is random, lives
in `repo.toml` and is *not* secret from someone holding the whole repository: it only keeps unique names ("Clients/Acme-case")
out of the index for someone who gets the index alone. A person with the full repository can still test guessable names
("INBOX", "Sent"). It lives in the repository on purpose, because a key kept elsewhere that got lost would make every folder
look new and trip the mass-deletion guard.

**What an attacker with the repository (but no private key) still learns.** The number of messages and folders and the
folder tree's shape; message sizes; flags and keywords (for example a `$Label`); EMAILIDs; when each run happened and how
much changed; and whether one specific known message is in the repository, because the blob key is the SHA-256 of the
plaintext. These are the cost of backing up without the private key. They are not message content or metadata.

**Integrity without the private key.** Every blob records `stored_sha256`, the SHA-256 of the exact object file.
`check --read-data` compares each object with it, which finds bit rot and truncation on the backup machine itself. It
cannot see an attacker who rewrites an object *and* its row in the index; only decrypting authenticates data
(age uses authenticated encryption), which is what `check --decrypt` does, with the private key.

**Keys.** `imapvault keygen --out FILE` writes an age identity (mode 0600) and prints the recipient. Add **a second
recipient**, a recovery key kept offline: age encrypts to all of them, any one opens the repository, and losing a single
key file then does not lose the mailbox. Without any identity the data cannot be recovered by anyone. A wrong key is
reported as "the age identity does not match this repository" (a canary in `repo.toml`), not as corruption.
`repo.toml` is the source of truth for the recipients; backup refuses a config whose recipients differ. Changing them is a
later `rekey` command (see "Later").

**Recovery without imapvault.** Objects are plain age files behind a one-byte header, so the stock tools suffice:

```sh
tail -c +2 objects/ab/cdef... | age -d -i personal.key | zstd -d > message.eml
```

**Not encrypted, deliberately.** `repo.toml`, the SQLite structure and the plaintext columns above, file names (the
blob hashes), and anything on the IMAP connection. A repository created without recipients is entirely plaintext, says so
at `init`, and cannot be encrypted later (create a new repository and back up again).

**Format.** Encryption bumped `format_version` to 2. Version 1 was the unreleased pre-release layout (plaintext index,
no encryption); there is no migration, and such a repository is refused with an instruction to re-create it.

## Backup run

Per folder, in a single read-only session:

1. `LIST` with special-use, detect new, renamed (treated as gone + new; blobs dedupe) and vanished folders.
2. `EXAMINE` the folder and compare UIDVALIDITY. If it changed, treat it as a new folder:
   close all entries of the old generation and re-list. Bodies we already have are skipped
   via EMAILID when available, otherwise re-downloaded and deduplicated by hash.
3. **Fast path** (CONDSTORE; QRESYNC is not used because the IMAP library cannot send it yet): skip the folder when UIDVALIDITY,
   UIDNEXT, the message count and HIGHESTMODSEQ are unchanged; otherwise list only UIDs above `last_uid`, `UID FETCH 1:last_uid (UID FLAGS)
   (CHANGEDSINCE modseq)` for flag changes, and find expunges by counting (`UID SEARCH` only when the counts show some).
   **Fallback:** `UID FETCH 1:* (UID FLAGS)` and diff against open entries, also whenever the fast path's numbers do not add up.
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

Pure function in `plan` (`plan.CheckGuard`): (previous snapshot counts, this run's changes) → alert or none.
It runs inside `backup`, after the last folder and before the snapshot is written, over plaintext columns only.

**What counts as deleted.** The entries valid in the previous snapshot and gone now, classified by `plan.DiffSnapshots`
(the same classifier as `diff`): only `Deleted` counts. A move to another folder, a flag change and a UIDVALIDITY
renumbering do not. A message moved into a folder the config excludes does count: the backup can no longer see it. Emptying
Trash shows up as deletions from Trash with no special case.

Trips when deletions are **strictly more** than `max_deleted_percent` of the previous snapshot's messages or more than
`max_deleted_count` (a threshold of 0 is off; a previous snapshot of 0 messages skips the percent check; no previous snapshot
skips the guard), or when **a folder vanished and some of its messages were deleted**, not just moved. That last rule has no
switch, and a plain rename or a merge into another folder does not trip it.

The comparison is with the previous *snapshot*, so deletions that a run which died before writing a snapshot had already
committed are still counted, and after an alert the next run compares with the flagged snapshot and stays quiet.

When it trips, in the same transaction as the snapshot (`index.CompleteRun`):
- The snapshot is still written (you want to see the bad state) but is marked `flagged`.
- The previous snapshot is **auto-pinned**.
- An `alerts` row is created with a sealed summary (counts and folder names, never subjects), then `on_alert` runs, and
  the command exits with code **3** (0 = ok, 1 = error, 2 = partial, 3 = alert; 3 wins over 2, and a partial run is guarded too).
- `prune` refuses to run while unacknowledged alerts exist. `imapvault alerts` lists them, `imapvault ack <id>` acknowledges
  one, and the snapshot an unacknowledged alert pinned cannot be unpinned.

`on_alert` is never run through a shell, because folder names come from the server. The line is split into words, `{account}`
and `{summary}` are substituted into the words after the program name, and the program is executed directly with the same
values in `IMAPVAULT_ACCOUNT`, `IMAPVAULT_SUMMARY`, `IMAPVAULT_ALERT_ID` and `IMAPVAULT_SNAPSHOT`. It has 60 seconds and is
not cancelled by Ctrl-C. A failing hook is a warning: the alert is already recorded.

## Retention: `forget` and `prune`

- `forget` applies the policy (restic semantics: keep_last / hourly / daily / weekly / monthly / yearly, buckets in the
  local time zone, ISO weeks). Pinned snapshots and the latest one are always kept; a policy that keeps nothing is an
  error. It only marks snapshots as forgotten. A forgotten snapshot stays addressable by number (`ls`, `show`, `diff`,
  `restore`) and is listed by `snapshots --all` until prune removes what it showed. It works while alerts are
  unacknowledged.
- `prune` deletes `entries` rows that cover no kept snapshot (not forgotten, or pinned) **and** whose `valid_to_run`
  started before `now - deleted_grace`. Then it deletes blobs no remaining entry references, forgotten snapshot rows
  that are no longer complete (one of their entries went), gone folders nothing shows, orphan blobs and tmp files.
  A forgotten snapshot whose messages are all still open entries is still complete and stays.
  It refuses to run while an alert is unacknowledged, and never resolves an identity.
- The index deletions commit in one transaction (every claim re-checked in SQL: a closed entry, an unreferenced blob, a
  forgotten unpinned snapshot) *before* any file is removed; a crash leaves orphan files, never dangling references.
- Both support `--dry-run` and print counts and bytes. A dry run takes the same exclusive lock and runs the same
  transaction, then rolls it back, so its numbers (folders included) are those of a real run.
- `plan.Retain(snapshots, policy, location)` and `plan.Prune(input)` are pure and table-tested.
- Index copies: `index-backups/index-<UTC time>-<r<run>|prune>.db` after every backup run and after a prune that
  removed something; the newest 7 are kept. There is no journal to rebuild the index from the objects (see Open questions).

## Restore

```sh
imapvault restore --snapshot latest|<id>|latest~N|2026-11-20T10:00 | --deleted-since 7d \
  [--folder INBOX] [--message <sha-prefix>] \
  [--to <other-account>] [--into "Restored/{snapshot}"] [--in-place] [--max-messages N] [--dry-run] [--confirm TOKEN]
```

Restore is the only command that writes to a server, and the server is the source of truth, so writing is gated:

1. **Preview first.** Without approval restore only prints a preview: target `user@host` (with a DIFFERENT ACCOUNT banner
   when it is not the repository's account), folders to create, existing folders it would append into, message and byte
   counts, how many are skipped as already present, and "Nothing is deleted, moved or re-flagged on the server."
2. **Plan token.** The preview ends with a token, a hash of the whole plan (target, source, mode, folders to create, every
   append). At a terminal restore asks for the token to be typed; elsewhere it needs `--confirm TOKEN`. There is no `--yes`.
   The plan is recomputed from the live server before writing, so a token that no longer matches (the target changed since the
   preview) writes nothing and prints the new preview.
3. **Config opt-in.** The target account's config needs `[restore] allow_append = true`; otherwise restore previews and refuses.
4. **Size cap.** `--max-messages` (default 10000, 0 for none): a plan that adds more is previewed, flagged and refused whatever
   approved it, in the CLI and again in `ApplyRestore`.
5. **Fenced writer.** Only `internal/imaptarget` can send `CREATE`/`APPEND`, and its writer refuses any folder or count outside
   the approved plan. Build-time tests keep go-imap and `imaptarget` imports out of every other package.

- Default target: same account, under `Restored/<snapshot>/<original folder>`. `--in-place` appends into the original
  folders (creating them if missing); the preview marks each existing folder. `--in-place` and `--into` are exclusive.
- `APPEND` with the original flags (minus `\Recent` and `\Deleted`) and INTERNALDATE.
- Idempotent: a message already in the target folder (Message-ID + size, or size + INTERNALDATE without a Message-ID) is skipped,
  matches consumed one-for-one. A run with nothing to append prints "Nothing to restore".
- `--deleted-since` restores messages whose last entry closed within the window and that exist nowhere on the server now. This is
  the "undo the hack" command.
- Needs the private key (it decrypts blobs and folder names). Writes nothing to the repository.

## Commands (v1)

| Command | What it does |
|---|---|
| `keygen --out FILE` | generate an age key pair; print the recipient and a config snippet |
| `init` | create the repository for an account (encrypted when `encryption.recipients` is set) |
| `backup [--full]` | one backup run → snapshot. Exit 2 if it stopped partway (snapshot marked partial). `--full` lists every folder in full |
| `snapshots [--all]` | list snapshots (id, time, counts, pinned/flagged); `--all` includes forgotten ones |
| `ls <snapshot> [folder]` | folders, or messages in a folder (date, from, size, hash). No bodies |
| `show <hash>` | headers of one message. `--raw` writes the .eml to stdout |
| `diff <a> <b>` | added / deleted / moved / re-flagged / renumbered counts per folder; `latest~N` accepted; no private key needed (folders show as `#id` without one) |
| `pin` / `unpin <snapshot>` | protect a snapshot from retention |
| `alerts`, `ack <id>` | guard alerts |
| `forget`, `prune` | retention (`--dry-run`, `--json`); prune is refused while an alert is unacknowledged |
| `restore` | see above |
| `check [--read-data] [--decrypt]` | index consistency. `--read-data` verifies every object (no key needed). `--decrypt` decrypts and re-hashes everything (needs the private key) |
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

**M1.5 Encryption at rest (done).** age, pulled forward from "Later" while nothing real was stored and the format was free
to change. `internal/seal` (the only importer of age), encrypted blob envelope with a ciphertext hash, sealed index columns,
`repo.toml` format 2 with `folder_key` and an encryption binding, `keygen`, `check --decrypt`, `[encryption]` config.
*Done when* an encrypted repository can be created, written and checked with no private key, and read back with it.
*Findings:* age's parse errors quote the first bytes of the data they cannot parse, so `internal/seal` replaces them with a
coarse reason (safety rule 6); a test pins this. The stock `age` CLI plus `zstd -d` recovers a blob (see "Encryption"). Hybrid
post-quantum recipients (`age1pq1...`) parse too, but age refuses to mix them with classic ones in one repository.

**M2 First full backup (done).** `mail.Source` + `imapsource` (EXAMINE, BODY.PEEK), engine writes entries + snapshot (sealing
folder names and message metadata with the repository's recipients from `repo.toml`, looking folders up by `FolderHash`, and
never calling the identity code: a backup must work with no private key), `snapshots`, `ls`, `show`, `doctor`. *Done when* a
seeded dovecot mailbox backs up and `ls` matches the server exactly.
*Findings:* every run is already a **full rescan** (`UID FETCH 1:* (UID FLAGS RFC822.SIZE INTERNALDATE)` diffed against the open
entries by the pure `plan.DiffMessages`), so a second run is correct and records flag changes and deletions; M3 adds the CONDSTORE/QRESYNC
fast path in front of it, not the diff itself. go-imap v2.0.0-beta.9 cannot request EMAILID, so `blobs.emailid` stays empty and the
EMAILID download skip needs either a newer go-imap or raw command support (M3); `doctor` says so. A folder commits in one
transaction after its blobs are durable, so a crash leaves only orphan blobs (`check` warns, the next run reuses them), but it also
means a killed first backup of a huge folder re-downloads that folder: per-batch commits and `partial` runs are M3. A folder that
disappears (or is newly excluded by config) is closed *last*, after every remaining folder was read, so a failed run never records
a deletion. A changed UIDVALIDITY fails the run for now (M3). `BeginRun` marks leftover `running` runs `failed` (the exclusive lock
proves they are dead), and `CompleteRun` writes the run's `complete` status and its snapshot in one transaction. `backup` refuses a
config whose `encryption.recipients` differ from `repo.toml`. `ls` and `show` print message headers to a terminal, so control
characters are replaced; `--json` is not altered. `imapsource` has an AST test that fails on any write command, a read-write
`SELECT`, or a body fetch without `Peek`, so rule 1 is enforced by the build. The `Tricky` seed folder (8-bit body, bare-LF
line endings, two different messages with one Message-ID) confirms the hash is over the exact server bytes. `make itest` needs docker
or podman (the Makefile falls back to podman).

**M3 Incremental (done).** New UIDs, flag diff, expunges, folder new/gone/rename, UIDVALIDITY reset, CONDSTORE fast path
with fallback, EMAILID download skip, `partial` runs, `diff`. *Done when* itests cover each case, including killing the
process mid-run and resuming.
*Findings:* **go-imap v2.0.0-beta.9 (the newest release) has CONDSTORE but no QRESYNC/VANISHED and cannot request EMAILID**,
so the fast path is CONDSTORE-only. Per folder `plan.ChooseMode` picks `unchanged` (UIDVALIDITY, UIDNEXT-1 = last_uid, message
count = open entries and HIGHESTMODSEQ all match: one EXAMINE), `incremental` (list `last_uid+1:*`, `FETCH 1:last_uid CHANGEDSINCE
<stored modseq>` for flags, and expunges found by *counting*: the UID list is fetched only when
`open + new - NumMessages > 0`) or `full` (the M2 rescan, still the authority). Unchanged also needs the count and UIDNEXT because
plain CONDSTORE does not promise to bump HIGHESTMODSEQ on EXPUNGE. A server whose modseq or UIDNEXT goes backwards, a
stored modseq of 0, a UIDVALIDITY change or leftover entries of another generation all force `full`. If the incremental
pieces disagree (`plan.ErrInconsistent`: a changed UID with no open entry, counts that cannot be explained, a UID list that
does not match) the folder is rescanned in full in the same run before anything is committed. `SplitGenerations` closes every
open entry of another UIDVALIDITY, which also finishes a reset a killed run left half done. The EMAILID skip is engine code
(`plan.MatchEmailID`: id *and* size must match, it is a hint, never the key) covered by unit tests with the fake; `imapsource`
leaves `EmailID` empty and dovecot has no OBJECTID, so the itests only exercise the fallback (a moved message is fetched again
and its blob reused). Rename needs no special code (old folder gone, new folder new, blobs dedupe); on a server without
OBJECTID its bodies are downloaded again. Fetched bodies commit in batches (`ApplyBatch`, every 100 bodies or 64 MiB) without
moving the folder's last_uid and modseq, which advance only in `ApplyFolder` at the end; a failed fetch commits what it already
holds. A run that stops after at least one folder committed is `partial`: its snapshot holds what the index has at that moment
(a folder interrupted mid-download shows the batches already committed), unreached folders keep their entries open, **no folder
is closed as gone**, and the command exits 2. A run that committed nothing, or a server refusing every folder, fails with no
snapshot. A folder that vanishes between LIST and EXAMINE (`mail.ErrFolderGone`, a server NO) is skipped and the run is partial.
`diff` (plaintext columns only, so it works on a backup machine) classifies the entries between two snapshots as added, deleted,
moved (a blob left one folder and arrived in another, paired in folder-id order), re-flagged and renumbered; folder names show
with a key and as `#<id>` without; `latest~N` is accepted wherever a snapshot is. **Footprint on the server:** dovecot reports
HIGHESTMODSEQ 0 until some client enables CONDSTORE; the backup's `EXAMINE (CONDSTORE)` makes it start tracking mod-sequences
for that mailbox (no message or flag changes). The M2 read-only itest now takes its HIGHESTMODSEQ baseline with CONDSTORE already
on; `imapsource`'s AST test now also fails on any `SelectOptions` without `ReadOnly: true`. `make itest` runs packages one at a
time (`-p 1`) because `TestBackupMatchesServer` backs up every folder on the shared server. The UIDVALIDITY itest sleeps 1.1 s
because dovecot derives UIDVALIDITY from the folder's creation second. A randomised engine test (server mutations including
resets, renames, removals and injected failures; with and without CONDSTORE) checks after every completed run that the snapshot
equals the server; 80 seeds passed during development, the default is 4.

**M4 Guard (done).** Thresholds, flagged snapshots, auto-pin, alerts/ack, `on_alert`, exit codes, `pin`/`unpin`.
*Done when* an itest that deletes everything and expunges Trash produces an alert, exit 3 and a pinned previous snapshot.
*Findings:* no schema change and no `format_version` bump: `snapshots.pinned/flagged` and `alerts` were already in the M1
schema. `CompleteRun` takes an optional alert and writes snapshot, flag, pin and alert together, so a crash cannot leave a
flagged snapshot without its alert. The guard reuses `plan.DiffSnapshots`, so `diff` and the guard agree on what a deletion
is. Backup still never needs the private key: it reads only plaintext, and seals the summary with the recipients; a folder that
vanished has no readable name there and shows as `#<id>` (`diff` and `ls` map it back with the key). **`on_alert` is exec'd, not
run by `sh -c`**: PLAN.md's earlier example looked like a shell line, but a hostile folder name (`$(...)`) would have become
code on the backup machine, so the line is split by imapvault and the values go in as arguments and environment (the
hostile-folder test pins this). An `on_alert` that does not parse refuses the run before it starts, not after a mass
deletion. The guard looks at one run against the previous snapshot, so a slow drip of deletions spread over many runs below
the thresholds is not caught; retention's grace period (M5) is what protects against that. `unpin` refuses while an
unacknowledged alert owns the pin, and `ack`/`pin` take the exclusive lock, so they fail at once while a backup runs.

**M5 Retention (done).** `forget` + `prune` with policy, grace period, pins, alert lock, dry-run, index backups.
*Done when* `plan.Retain` is exhaustively table-tested and prune on a fixture repo frees exactly the expected blobs.
*Findings:* no schema change and no `format_version` bump (`snapshots.forgotten` was in the M1 schema). Two pure
functions carry the decisions: `plan.Retain` (restic buckets; pinned and the latest snapshot always kept; an all-zero
policy is an error; randomised tests check that pinned and latest survive, that raising a `keep_*` never forgets more,
and that applying the policy twice changes nothing) and `plan.Prune` (an entry goes when it was closed before the
grace cutoff and no kept snapshot lies in `from <= S < to`; a blob goes when every entry referencing it goes; an entry
closed exactly at the cutoff is kept). **A forgotten snapshot is removed when it stops being complete, not when it is
forgotten**: if all its messages are still on the server it stays under `--all`, and `ls 2` still shows exactly what
the server had then. A pinned snapshot that was forgotten before it was pinned counts as kept. `index.ApplyPrune`
re-checks every claim in SQL and rolls everything back on a surprise, so a bug in the plan cannot drop a referenced
blob; a dry run runs the same transaction and rolls it back, which is why it takes the exclusive lock (not a shared
one) and why its counts equal the real run's. `forget` runs while alerts are unacknowledged (it only marks);
`prune` and its dry run refuse. Index backups (`VACUUM INTO` into `tmp/`, fsync, rename, keep 7) are written after
every backup run and after a prune that changed the index, never after an idle prune, which would push useful copies
out. A failed index backup is a warning, not a failed run. **An index copy older than the last prune can reference
objects prune deleted, and backup trusts the index (it does not re-download a message whose blob row exists), so
those stay missing**: use the newest copy and run `check`. The guard's "slow drip" gap (M4) is what the grace period
covers. The itests backdate `runs.started_at` in `index.db` from the test, because the product has no clock hook.

**M6 Restore (done).** Snapshot / folder / message / `--deleted-since`, `--into` and `--in-place`, `--to` another account,
idempotency, dry-run, and the write guard rails. *Done when* the M4 "hack" itest ends with the mailbox restored and a second
restore appends nothing.
*Findings:* no schema change and no `format_version` bump: restore writes nothing to the repository. Four layers keep the
server safe, each working without the others. (1) **Approval:** `plan.Restore` is pure and returns a plan whose `Token()` hashes
the target, repository, source, mode, folders to create and every append; `plan.Approve` is the only way to build the
`ApprovedRestore` that `engine.ApplyRestore` takes, and `PlanRestore` recomputes the plan from the live server, so a stale or
wrong token writes nothing. There is no `--yes`; piping "yes" to stdin is not approval, and `/dev/null` does not count as a
terminal. (2) **Config:** `[restore] allow_append` of the *target* account, re-checked in `ApplyRestore` as well as the CLI.
(3) **Fence:** `mail.Fence` (folders to create, appends per folder) is built from the approved plan; `imaptarget`'s writer refuses
anything outside it, spends a slot before each attempt (an ambiguous failure never becomes two copies), re-LISTs before a CREATE
and drops `\Deleted` and `\Recent` itself. (4) **Walls:** an AST test allows only `CREATE`/`APPEND`, only in `writer.go`, and fails
on any other write command or non-read-only `SELECT`; an import test allows go-imap only in `imapsource` and `imaptarget` (plus the
dev tool `tools/seed`) and `imaptarget` only in `engine/restore.go`. A mutation of `plan.Approve` was caught by four tests.
Messages are appended in original order (internal date, then UID) so the restored UIDs ascend. Duplicate detection is one
read-only pass per existing target folder (Message-ID + size, else size + internal date to the second), consumed one-for-one, so
restore, restore again gives an empty plan; this was a property test over random folders and duplicates. `--deleted-since` takes
the last row of each position's history, closed within the window, whose blob has no open entry anywhere (`index.DeletedSince`):
flag changes, moves and UIDVALIDITY renumbering are not deletions; a move into an excluded folder is, because the backup can no
longer see it. Its prefix is `deleted-<UTC date>`, so a token made before midnight and used after it no longer matches. Snapshot
specs accept a time (`2026-11-20T10:00`, local zone; a bare date means the end of that day). The `\Deleted` flag is not restored
(the preview says how many messages had it). APPEND relies on the server creating missing parent folders for `Restored/1/INBOX`
(dovecot does; RFC 3501 says SHOULD). **Test-harness finding:** closing a test client without LOGOUT left connections the server did
not notice through the container's port forward, so the 10-connection limit starved the next package; `dialTestServer` now logs out.
All itests pass against dovecot, including the "undo the hack" run and every no-approval path leaving the server's folder list,
UIDNEXT, UIDVALIDITY and counts unchanged.

**M7 Release prep (done).** README install and quick start, example configs (incl. `backup-only.toml`), systemd units and
timers, cron/launchd notes, Dockerfile, goreleaser, `install.sh`, SECURITY.md, first real-Fastmail read-only backup, v0.1.0.
*Done when* a tagged release installs from brew, `install.sh` or the image and a backup runs unattended.
*Findings:* v0.1.0 freezes `format_version` 2 / index schema 1. Release plumbing is copied from quietbox: goreleaser builds
linux and darwin for amd64 and arm64 into a **draft** release (plus a Homebrew formula in `michele/homebrew-imapvault`),
`docker.yml` pushes a multi-arch distroless image to ghcr.io, and `docs/RELEASING.md` has the one-time token setup. The image has no
default command (one-shot tool) and sets `XDG_CONFIG_HOME=/config`. Config validation refuses `tls = "none"` for a non-loopback host,
so the container itest shares dovecot's network namespace instead of using its service name. Units run `backup` as `Type=oneshot`
with no `SuccessExitStatus`, so exit 2 (partial) and 3 (alert) show as failed on purpose; the repository lock makes overlapping runs
fail fast, so no external `flock` is needed. A scheduled machine holds public keys only. Verified here: `systemd-analyze verify` on
systemd 257 (not run as live services), the container's init/backup/second backup (all folders unchanged)/check against dovecot,
the goreleaser snapshot archives, and `install.sh` (good, tampered and missing checksum) against local archives. **Not verified:** the
release and docker workflows, the Homebrew publish, `install.sh` against real GitHub, NFS/SMB `flock`. **Real Fastmail (read-only
backup, run by the maintainer, reported "fine", outputs not recorded):** `doctor` reports CONDSTORE, QRESYNC, OBJECTID, SPECIAL-USE and
UIDPLUS. Fastmail has OBJECTID, but go-imap v2.0.0-beta.9 cannot request EMAILID, so a moved message is still downloaded again there
(the M3 skip is unit-tested with a fake only); this is the first thing to fix once the library or raw commands allow it. Restore has
not been run against Fastmail.

**M8 Site (built, not deployed).** `site/`: one static page for `imapvault.sh` (inline CSS, no JavaScript, same structure as
quietbox's, navy and amber palette from `assets/`, light and dark), favicons, `og.png`, and `site/install.sh`, now the only copy of the
installer (the repo-root one moved; nothing had published its raw URL). The README install line and the Homebrew `homepage` point at
`imapvault.sh`. Hosting is Cloudflare Pages from `site/` with no build step (setup in `docs/RELEASING.md`).
*Verified:* the page in headless Chrome at 1100 px in dark and light, and at 390 px in light with no page-level overflow (code blocks
scroll inside their box); served content types from a local server; `install.sh` is unchanged apart from its URLs, and its good and
tampered-checksum cases passed earlier against local archives. *Not verified:* the page at 390 px in dark, other browsers, the
Cloudflare project and DNS, and that the live `https://imapvault.sh/install.sh` serves the script. Until that project exists and the
v0.1.0 draft is published, the README's install command fails.

**M9 Backup progress (done).** The engine emits `ProgressEvent`s (`BackupOptions.Progress`: run start, folder start, folder fetch with
message and byte counts, message done, folder done); it draws nothing. `cmd/imapvault/progress.go` renders them to stderr: a live
one-line bar (79 columns, redrawn at most every 200 ms) on a terminal, plain lines elsewhere (one when a folder starts downloading, one
at most every 30 s, one when it is done), nothing for a folder with nothing new. `--progress auto|bar|log|none`; `auto` is `none` with
`--json`. No new dependency, no on-disk change.
*Findings:* the first version printed progress lines under `--json`, and 8 integration tests that merge stderr into the JSON they parse
failed; the same merge happens with `2>&1 | jq` or cron mailing combined output, so `auto` now stays silent with `--json` (an explicit
`--progress log` still prints). Folder names go through `clean()` before they reach a terminal. There is no account-wide total (it would
need a STATUS pass over every folder first), so the bar covers one folder; folder numbers skip folders with nothing new. With `-v` and the
bar, a folder's summary appears twice (the engine's log line and the renderer's); left as is.
*Verified:* `make test`, `make lint`, `make itest` (podman); against dovecot with 4,000 and 9,700 synthetic messages: log mode piped
(including the 30 s line and ETA), second run with nothing new (0 bytes on stderr), `--progress none`, `--json` with stderr merged
(valid JSON), the live bar in a pseudo-terminal (never over 79 columns), `-v` with the bar, Ctrl-C mid-folder (clean line, exit 2, the
next run fetched exactly the remainder), and the image run detached with `podman logs`. *Not verified:* the bar in terminal emulators
other than a pty, `-t` in a container, a multi-GB first run on a real server.

**M10 Docker deployment (done).** `deploy/`: `docker-compose.yml` (one service per command; `backup` is what `up` runs, `init`, `forget`,
`prune`, `check` and a free-form `imapvault` are in a `tools` profile), `docker-compose.build.yml`, `.env.example`, `imapvault.example.toml`,
`secrets/README.md`; `docs/DEPLOY.md` replaces `docs/DOCKER.md` and follows quietbox's guide. imapvault is one-shot, so scheduling stays on
the host (cron, a systemd timer, Synology Task Scheduler); a `run` loop command is a separate, later task.
*Decisions:* the command is fixed in `entrypoint`, not `command`, so `docker compose run --rm prune --dry-run` appends to it instead of
replacing it; `--progress` is not set (auto gives the bar under `run` and plain lines under `up`, cron and `logs`); the host keeps
`config/NAME.toml` flat and compose mounts the folder at `/config/imapvault`, because `--account NAME` reads `$XDG_CONFIG_HOME/imapvault/NAME.toml`
and the image sets `XDG_CONFIG_HOME=/config` (a file put in `/config` was the first thing that went wrong on the NAS). A missing account
config now says where it expected the file and, in a container, what to mount. `[guard] on_alert` cannot work in the image (it runs a
program directly and the image has none), so the guide says to act on exit code 3 from the host scheduler.
*Verified:* `make deploy-itest` drives the real compose file against dovecot (config, init, `up` twice, appended arguments, free-form
command, exit code 1 on a wrong password, the misplaced-config message, the missing-account refusal); `docker compose run` with no terminal
works without `-T` (compose v2); real errors quoted in the guide for an unreadable secret and an unwritable repository (produced on the
VM's own filesystem, because a macOS bind mount does not enforce ownership), for the repository lock (a second run exits 1 while the first
is going), and a SIGTERM mid-folder returns in 0.2 s and the next run resumes; the multi-account snippet renders. *Not verified:* Docker
Engine and Compose v1 (Podman behind the Compose v2 CLI here), Synology Container Manager and Task Scheduler, Portainer, the systemd
snippet, the memory limit on a large mailbox, `-t` giving the bar in a container, arm64 hardware.

## Testing

- Unit: `plan` (sync diff, guard, retention) as table tests. Blob store and index with temp dirs.
- Integration: dovecot in docker, seeded per test. Scenarios: first backup, new mail, flag change,
  move between folders, delete + expunge, empty Trash, folder rename, folder delete, UIDVALIDITY reset
  (recreate folder), mass delete, crash mid-run, restore idempotency.
- A `testdata/` corpus of tricky messages: 8-bit, huge attachments, missing Message-ID, duplicate
  Message-ID with different content, CRLF/LF variants (hash must be over the exact server bytes).

## Open questions

- **Index durability.** *Decided in M5: no journal in v1.* SQLite plus a `VACUUM INTO` copy after every backup
  (newest 7 kept) is the answer; the limit is documented in the README (an index copy older than the last prune can
  reference deleted objects). A journal that rebuilds the index from the repository alone would make prune more
  complex; revisit if someone loses an index.
- **Gmail.** Labels show up as folders, so blobs dedupe for free. Should we default-exclude `[Gmail]/All Mail`,
  or back up only All Mail plus `X-GM-LABELS`? Not needed for Fastmail; decide when someone asks.
- **Shared code with quietbox** (config, password sources, IMAP dialing): copy now, extract a module once both are stable.

## Later

- `rekey`: change a repository's recipients (re-wrap blobs, re-seal the index columns), for lost or rotated keys
- Plugin and SSH-key identities (hardware tokens), if people ask
- Remote backends: S3-compatible (B2, Hetzner, MinIO), or document repo-on-rclone
- Read-only TUI to browse snapshots (bubbletea): folders, message list, headers, diff between snapshots
- Search: SQLite FTS5 over headers (from, to, subject, date)
- `export` a snapshot or folder to Maildir / mbox
- `mount` a snapshot as a read-only Maildir via FUSE (restic-style)
- OAuth2 (Gmail, Microsoft 365), JMAP source for Fastmail
- Notifications beyond `on_alert` (email via SMTP, ntfy, webhook)
