<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/banner-dark.svg">
    <img src="assets/banner-light.svg" alt="imapvault: snapshots and point-in-time restore for IMAP mail" width="720">
  </picture>
</p>

# imapvault

An open-source IMAP backup tool: point-in-time **snapshots** of a mailbox in a local
repository, a grace period for deleted mail, mass-deletion alerts, and restore of any
snapshot, folder or message back to an IMAP server.

New here? The [getting started guide](https://imapvault.sh/guide/) goes from install to a restored backup, step by step.

**Status: alpha, a brand new project.** The on-disk format has been frozen since v0.1.0: a later change to it ships a migration or a
clear refusal. Everything is tested against a dovecot test server, and read-only backups have run against real Fastmail and Gmail mailboxes; it should work with most IMAP providers. Restore has only been run against the test server: try it on a throwaway account first (see [Restore](#restore)).
Design and decisions: [docs/PLAN.md](docs/PLAN.md).

## Install

```sh
brew install michele/imapvault/imapvault                       # macOS, Linux
curl -fsSL https://imapvault.sh/install.sh | sh   # into ~/.local/bin, checksum verified
docker pull ghcr.io/michele/imapvault                          # see docs/DEPLOY.md
```

Or take the tarball for your platform (linux and macOS, amd64 and arm64) from the
[releases page](https://github.com/michele/imapvault/releases): one static binary, plus the examples and the
systemd units. `imapvault --version` confirms it. Windows is not supported.

## Quick start

```sh
imapvault keygen --out ~/.config/imapvault/personal.key      # prints the public key (the "recipient")
$EDITOR ~/.config/imapvault/personal.toml                    # start from examples/fastmail.toml; set recipients
imapvault --account personal init                            # once; encryption cannot be added later
imapvault --account personal doctor                          # connects, shows capabilities and what would be backed up
imapvault --account personal backup                          # the first run can take hours; resumable
imapvault --account personal snapshots
```

Then keep it running: `imapvault --account personal run` backs up every hour, applies the retention policy daily
and prunes only if you ask (`--prune-every 7d`), and, if you configure a notification, tells you when the guard trips or
backups keep failing ([docs/DEPLOY.md](docs/DEPLOY.md) has a Docker Compose file that starts it). Or schedule `backup` yourself
(systemd timer, cron: [docs/SCHEDULING.md](docs/SCHEDULING.md)) and run `forget` + `prune` weekly. A machine that only runs the schedule needs
the public keys only ([examples/backup-only.toml](examples/backup-only.toml)); keep the private key where you
read or restore mail, and keep a second, offline recovery key. Exit code `3` means the guard saw a mass deletion.

## Providers

| Provider | Status | Config |
|---|---|---|
| Fastmail | tested; app password | [`examples/fastmail.toml`](examples/fastmail.toml) |
| Gmail | tested; IMAP with an [app password](https://myaccount.google.com/apppasswords) (needs 2-Step Verification); keep `[Gmail]/All Mail` in the backup (see [Configuration](#configuration)) | [`examples/gmail.toml`](examples/gmail.toml) |
| Any other IMAP server | should work; tried against Dovecot 2.4.5 only; `imapvault doctor` shows what your server offers | [`examples/generic-imap.toml`](examples/generic-imap.toml) |
| Microsoft 365, Outlook.com | **not supported**: Microsoft does not offer app passwords for IMAP and requires OAuth2, which imapvault does not implement | |

An **app password** is a separate password your provider creates for one application, so you never give
imapvault your real one. Create it in your provider's security settings (Fastmail: Settings, Privacy & Security,
Integrations, New app password, "Mail (IMAP)"), and keep it out of the config file: `password_file`,
`password_command` or the `IMAPVAULT_IMAP_PASSWORD` environment variable.

## Configuration

One TOML file per account in `$XDG_CONFIG_HOME/imapvault/<account>.toml`, selected with
`--account NAME`. See [examples/](examples/). `imapvault accounts` lists them.

**Gmail.** Gmail shows labels as folders, so a message with three labels sits in four places: each label plus
`[Gmail]/All Mail`. Do **not** exclude `[Gmail]/All Mail`. Archived mail (the Inbox label removed) exists only
there, so leaving it out would miss it, and archiving a lot of mail at once would look like a mass deletion to
the guard. Storage is not the cost: a message is stored once however many folders hold it. The cost is time and
quota: imapvault downloads the body once per folder position and only then sees it is a duplicate, so a first
backup downloads a message as many times as it has places, and Gmail limits how much IMAP data one account can
download per day. A big first backup may need several resumed runs (`run` does this by itself). Excluding
`[Gmail]/Spam` is safe; think twice before excluding `[Gmail]/Trash`, which is where deletions land.

## Commands

```sh
imapvault keygen --out ~/.config/imapvault/personal.key   # age key pair; prints the public key
imapvault --account personal init              # create the repository at repository.path
imapvault --account personal doctor            # server capabilities, folders, and what would be backed up
imapvault --account personal backup            # read-only run -> a new snapshot (--full: list every folder in full)
imapvault --account personal run               # keep running: backup every --every (1h), forget daily, prune only with --prune-every
imapvault --account personal snapshots         # list snapshots (--all: forgotten ones too)
imapvault --account personal ls latest         # folders in a snapshot ("latest" or a number)
imapvault --account personal ls latest INBOX   # messages in a folder: UID, flags, size, date, sender, subject, hash
imapvault --account personal show 3f9a1c2b     # one message by hash prefix; --raw writes the original .eml to stdout
imapvault --account personal diff latest~1 latest   # per folder: added, deleted, moved, re-flagged, renumbered
imapvault --account personal alerts            # guard alerts nobody acknowledged yet (--all: every alert)
imapvault --account personal ack 1             # acknowledge alert 1 (--all: every open alert)
imapvault --account personal pin latest~1      # protect a snapshot from retention; unpin undoes it
imapvault --account personal forget --dry-run  # which snapshots the [retention] policy would forget
imapvault --account personal forget            # mark them forgotten (deletes nothing)
imapvault --account personal prune --dry-run   # what prune would remove, with sizes
imapvault --account personal prune             # remove what nothing needs and the grace period no longer holds
imapvault --account personal restore --dry-run # preview what a restore would add; writes nothing
imapvault --account personal restore --deleted-since 7d --in-place   # preview, then approve (see Restore)
imapvault --account personal check             # index + object presence; exit 1 on errors
imapvault --account personal check --read-data # verify every object (no private key needed)
imapvault --account personal check --decrypt   # decrypt and re-hash everything (needs the key)
```

`init` binds the repository to the account's host and username; `check` and `backup` refuse a
different account. `init` and `check` do not need the IMAP password; `backup` and `doctor` do.

`backup` never changes the mailbox: folders are opened with `EXAMINE` and messages fetched with `BODY.PEEK[]`.
A message already stored is not downloaded again, even when it moved to another folder, its folder was renamed or
UIDVALIDITY changed. A deleted message stays in the repository (and in older snapshots); only `prune` removes data. Consistency is per folder, because IMAP has no atomic view of a whole account.

**Incremental runs.** After the first run only what changed is read. A folder the server reports as unchanged
(same UIDVALIDITY, UIDNEXT, message count and mod-sequence) costs one `EXAMINE`. With CONDSTORE, new mail is found
by listing only the UIDs above the last one seen, flag changes with `FETCH CHANGEDSINCE`, and deletions by counting
(the full UID list is asked for only when the counts show something is gone). When the pieces do not add up, or the
server lacks CONDSTORE, or you pass `--full`, the folder is listed in full and compared with the repository. A
changed UIDVALIDITY closes the old UIDs and reads the folder again; bodies already stored are reused.
RFC 8474 EMAILID lets a moved message skip its download when the server has OBJECTID (not every server does, and
the Go IMAP library imapvault uses cannot request it yet, so today every body is hashed from a fresh download).

**Interrupted runs.** Downloaded messages are committed to the repository in batches, so a run that dies (even
`kill -9`) resumes where it stopped. If a run stops after at least one folder was backed up (a folder cannot be
read, the connection drops, Ctrl-C), it writes a snapshot marked **partial**: it holds what was verified, folders
that were not reached keep their previous state, and nothing is recorded as deleted. If nothing could be backed
up, no snapshot is written. Run `backup` again to finish.

**Progress.** `backup` reports progress on stderr, so a first run that takes hours is not silent. On a terminal it
draws one live line (folder `4/23`, messages, bytes, rate, ETA) and keeps a line for every folder that downloaded
something. Anywhere else (cron, systemd, `docker logs`) it prints plain lines instead: one when a folder starts
downloading, a progress line at most every 30 seconds, and one when it is done. A folder with nothing new prints
nothing, so an hourly run with no new mail stays quiet. Folder numbers skip such folders.
`--progress auto|bar|log|none` overrides the choice; `auto` shows nothing with `--json`, so a program that merges
stderr into stdout still gets valid JSON. There is no account-wide total: the server cannot say how many bytes a
whole mailbox holds without reading every folder first, so the bar covers one folder at a time. `-v` adds a line per
message (folder, UID, hash prefix; never subjects or bodies) and works with the bar.

**Running continuously.** `imapvault --account personal run` stays up and does the scheduling: `--every` (default `1h`,
at least `5m`) between backups, counted from the start of the previous one; `--forget-every` (default `24h`) applies the
retention policy; `--prune-every` (off by default, for example `7d`) prunes, with all of prune's usual protections; and
`--stale-after` (default `24h`) sends one notification if no backup has succeeded for that long. One job runs at a
time and each holds the repository lock only while it runs, so a manual command works between them. A failed backup
is retried after 30 s, 1 m, 2 m, then every 5 m. The config is read and the server dialled again for every backup, so
a rotated password file is picked up. After a guard alert it logs, notifies and keeps going. SIGTERM stops it cleanly
(a backup in progress is kept as a partial snapshot) with exit `0`; a config or repository that cannot be opened is
exit `1` at once. It logs one line per event to stderr. The Docker setup in [docs/DEPLOY.md](docs/DEPLOY.md) starts it
with `docker compose up -d`.

Exit codes: `0` ok, `1` error, `2` partial backup, `3` guard alert (`3` wins when a run is both).

## Restore

`restore` is the only command that writes to a server, and the server is the source of truth, so it
cannot write until you have seen exactly what it will do and said yes:

1. **Preview.** It prints the target account, the folders it would create, the existing folders it
   would add to, how many messages and bytes, and how many are skipped because they are already there,
   and ends with a **plan token**. A restore into a different account than the repository's is marked
   `DIFFERENT ACCOUNT`.
2. **Approval.** Type the token when asked (at a terminal), or pass `--confirm TOKEN` (scripts). There
   is no `--yes`: the token is a hash of the whole plan, and the plan is worked out again against the
   server before anything is written, so a token for a plan that no longer holds writes nothing.
3. **Opt-in per account.** The target account's config must contain the lines below. A backup-only
   config (cron, a NAS) can never write, whatever is typed.

```toml
[restore]
allow_append = true
```

Restore only creates folders and appends messages. It never deletes, moves, flags or overwrites
anything, never restores the `\Deleted` flag, and never adds a message that is already in the target
folder: run it twice and the second run says "Nothing to restore". Messages are appended in their
original order with their original flags and dates. It needs the private key of an encrypted repository
and writes nothing to the repository. The build enforces the rest: only `internal/imaptarget` can send
`CREATE` or `APPEND`, only `internal/engine/restore.go` may import it, and its writer refuses any folder
or count outside the approved plan.

```sh
imapvault --account personal restore                       # into Restored/<snapshot>/<folder>, next to your own
imapvault --account personal restore --snapshot 2026-11-20T10:00 --folder INBOX
imapvault --account personal restore --deleted-since 7d --in-place   # undo a mass deletion
imapvault --account personal restore --message 3f9a1c2b --into "Recovered/{snapshot}"
imapvault --account personal restore --to newaccount       # another configured account (needs its own allow_append)
```

**Size cap.** A restore that adds more than `--max-messages` (default 10000, `0` for no limit) is previewed
and flagged but never written, even with a correct token: raise the cap to say you mean it. It is a second
check against approving something far bigger than intended, such as the wrong `--snapshot`.

**Try it on a throwaway account first.** Servers differ in small ways that decide whether a restore round-trips
(how they treat custom flags, whether `CREATE` of a nested folder makes its parents, whether `RFC822.SIZE` matches
the stored bytes, which duplicate detection relies on). Before pointing restore at a mailbox you care about, run it
against a second, disposable account with `--to`, so the real mailbox is never the write target:

1. `restore --to scratch --dry-run`, then a single message (`--folder X --message <hash>`) and approve it.
2. Check the bytes, flags and date in a mail client, then run the same command again: it must say
   "Nothing to restore". If it does not, duplicate detection does not work on that server; stop and report it.
3. Only then restore a folder, and last `--in-place`.

imapvault never deletes anything, so clean up the `Restored/...` folders yourself in your mail client.

`--deleted-since` restores messages deleted from the server within the window that are on it nowhere
now; a message moved to a folder excluded from the backup looks deleted to the backup and is included.

## Mass-deletion guard

Before a snapshot is written, `backup` compares the run with the previous snapshot. The guard **trips** when
more messages were deleted than `guard.max_deleted_percent` (of the previous snapshot) or `guard.max_deleted_count`
allow, or when a folder vanished and some of its mail is gone, not just moved. A threshold of `0` switches that
threshold off; the vanished-folder rule is always on. Moving mail between folders, renaming a folder, changing flags
and a UIDVALIDITY reset are not deletions. Mail moved into a folder you exclude does count: the backup can no longer see it.

When it trips, in one transaction with the snapshot: the new snapshot is **flagged**, the last good one is **pinned**,
and an alert is recorded (sealed, in an encrypted repository). Then `guard.on_alert` runs, and `backup` exits `3`.
The next run compares with the flagged snapshot, so the same loss does not alert twice. `alerts` lists what happened,
`diff <pinned> <flagged>` shows which folders lost mail, and `ack <id>` acknowledges it. Until it is acknowledged,
`prune` refuses to run and the pinned snapshot cannot be unpinned.

```toml
[guard]
max_deleted_percent = 10
max_deleted_count   = 500
on_alert = "/usr/local/bin/notify-me 'imapvault alert: {account} {summary}'"
```

`on_alert` is **not run through a shell**: it is split into words (single and double quotes and backslashes work),
`{account}` and `{summary}` are put into the arguments after the program name, and the program is executed
directly. Folder names come from the server, so a folder called `$(rm -rf ~)` reaches your notifier as plain text.
The same values are in the environment as `IMAPVAULT_ACCOUNT`, `IMAPVAULT_SUMMARY`, `IMAPVAULT_ALERT_ID` and
`IMAPVAULT_SNAPSHOT` (and `IMAPVAULT_EVENT`, `guard` or `stale`); if you want a shell, ask for one: `sh -c 'notify "$IMAPVAULT_SUMMARY"'`. The command gets 60
seconds. If it fails the alert is still recorded and the run still exits `3`, with a warning. The summary names
folders and counts, never subjects. The guard compares one run with the one before it, so a slow drip of deletions
spread over many runs is not caught, which is one reason `run` will not back up more often than every 5 minutes.

**Notifications without a program.** The container image has nothing for `on_alert` to run, and a long-running `run`
has no exit code to watch, so there is a second channel: `on_alert_url = "https://ntfy.sh/your-topic"`, or
`on_alert_url_file = "/run/secrets/alert_url"` to keep the URL out of the config (a topic URL works like a password;
set one of the two, not both). imapvault POSTs the summary as plain text with a `Title` header. `https` is always
accepted; `http` only for a local address (loopback, a private or link-local IP, or a name without dots such as a
compose service). It follows no redirects, gives up after 15 seconds, and neither the log nor any error contains the
URL. A failed delivery is a warning, never a failed backup. `run` uses the same two channels for its "no successful
backup for a while" notice. The URL receives folder names and counts, so choose a service you trust with that.

**Server footprint.** On a server that does not track mod-sequences until a client asks (dovecot does this), the
first `EXAMINE (CONDSTORE)` makes it start tracking them for that mailbox. No message or flag changes, but it is
more than a plain `EXAMINE`.

## Retention: forget and prune

```toml
[retention]
keep_last    = 3
keep_hourly  = 24
keep_daily   = 14
keep_weekly  = 8
keep_monthly = 24
keep_yearly  = 10
deleted_grace = "180d"   # mail gone from the server stays restorable at least this long
```

`forget` applies the policy with restic's semantics (a `keep_*` of `0` switches that rule off; hours, days, ISO weeks,
months and years are counted in the local time zone) and marks the snapshots it does not keep as **forgotten**. It
deletes nothing. Pinned snapshots and the latest snapshot are always kept, and a policy that keeps nothing is refused
rather than read as "forget everything". A forgotten snapshot disappears from `snapshots` (`--all` shows it), but stays
addressable by number in `ls`, `show` and `diff` until `prune` removes what it showed.

`prune` is the only command that removes data. It removes a closed index entry (a message that was deleted from the
server) when **both** hold: it was deleted longer ago than `deleted_grace`, and no kept snapshot (not forgotten, or
pinned) shows it. Then the message files nothing references any more, forgotten snapshots that are no longer
complete, folders nothing shows, and object files and `tmp/` leftovers that an interrupted run left behind. A
forgotten snapshot whose messages are all still on the server is still complete, so it stays until one of them goes.
`prune` refuses to run while a guard alert is unacknowledged. The index is committed before any file is deleted, so
an interruption leaves orphan files that the next prune removes, never a dangling reference. `--dry-run` takes the
same lock, runs the same checks, and rolls the index change back: its numbers are what a real run removes. Neither
`forget` nor `prune` needs the private key, and both fail at once while a backup is running.

**Index backups.** After every `backup` (and after a `prune` that changed something) imapvault writes a consistent
copy of `index.db` to `index-backups/` and keeps the newest 7. They are plain SQLite files with the sealed columns
copied as they are. If `index.db` is lost or damaged, stop everything, move it aside, and copy the newest file from
`index-backups/` to `index.db`, then run `check --read-data`. A copy older than the last prune may reference
messages that prune deleted; `check` reports them as missing objects, and `backup` does not download them again
(it trusts the index), so prefer the newest copy. There is no journal that could rebuild the index from the objects
alone; keep the whole repository in your own backup too.

## Encryption

Add the public key to the account config **before** `init`; it cannot be added later.

```toml
[imap]
host = "imap.example.com"
username = "you@example.com"

[repository]
path = "/backup/imapvault/personal"

[encryption]
recipients = ["age1workingkey...", "age1recoverykey..."]   # public keys; the second is a recovery key kept offline
identity_file = "~/.config/imapvault/personal.key"   # private key; leave out on a backup-only machine
```

With recipients set, message bodies, folder names and message details (Message-ID, subject, sender, dates) are
encrypted. A cron job or NAS that holds only the public keys can run backups and `check --read-data`; the private
key is needed only to read mail (`ls`, `show`, restore, `check --decrypt`). `backup` never reads it, and `snapshots` does not need it. Without any private key the data cannot be
recovered, so back it up. What stays readable, what an attacker with the repository still learns, and how to
recover a message with the stock `age` and `zstd` tools are in [docs/PLAN.md](docs/PLAN.md#encryption).

## Develop

```sh
make test     # go test ./... -race
make lint     # go vet + golangci-lint
make itest    # integration tests against the dovecot server in docker-compose.test.yml
make build    # static binary in ./bin
make release-snapshot   # release archives in ./dist, published nowhere (see docs/RELEASING.md)
make docker-build       # container image imapvault:local
```
