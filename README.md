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

**Status: pre-alpha (M4, mass-deletion guard).** `init`, `keygen`, `backup`, `snapshots`, `ls`, `show`, `diff`, `alerts`, `ack`, `pin`, `unpin`, `doctor` and `check` work. There is no restore or retention yet. See [docs/PLAN.md](docs/PLAN.md).

## Develop

```sh
make test     # go test ./... -race
make lint     # go vet + golangci-lint
make itest    # integration tests against the dovecot server in docker-compose.test.yml
make build    # static binary in ./bin
```

## Configuration

One TOML file per account in `$XDG_CONFIG_HOME/imapvault/<account>.toml`, selected with
`--account NAME`. See [examples/](examples/). `imapvault accounts` lists them.

## Usage so far

```sh
imapvault keygen --out ~/.config/imapvault/personal.key   # age key pair; prints the public key
imapvault --account personal init              # create the repository at repository.path
imapvault --account personal doctor            # server capabilities, folders, and what would be backed up
imapvault --account personal backup            # read-only run -> a new snapshot (--full: list every folder in full)
imapvault --account personal snapshots         # list snapshots
imapvault --account personal ls latest         # folders in a snapshot ("latest" or a number)
imapvault --account personal ls latest INBOX   # messages in a folder: UID, flags, size, date, sender, subject, hash
imapvault --account personal show 3f9a1c2b     # one message by hash prefix; --raw writes the original .eml to stdout
imapvault --account personal diff latest~1 latest   # per folder: added, deleted, moved, re-flagged, renumbered
imapvault --account personal alerts            # guard alerts nobody acknowledged yet (--all: every alert)
imapvault --account personal ack 1             # acknowledge alert 1 (--all: every open alert)
imapvault --account personal pin latest~1      # protect a snapshot from retention; unpin undoes it
imapvault --account personal check             # index + object presence; exit 1 on errors
imapvault --account personal check --read-data # verify every object (no private key needed)
imapvault --account personal check --decrypt   # decrypt and re-hash everything (needs the key)
```

`init` binds the repository to the account's host and username; `check` and `backup` refuse a
different account. `init` and `check` do not need the IMAP password; `backup` and `doctor` do.

`backup` never changes the mailbox: folders are opened with `EXAMINE` and messages fetched with `BODY.PEEK[]`.
A message already stored is not downloaded again, even when it moved to another folder, its folder was renamed or
UIDVALIDITY changed. A deleted message stays in the repository (and in older snapshots); only `prune` (not written
yet) removes data. Consistency is per folder, because IMAP has no atomic view of a whole account.

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

Exit codes: `0` ok, `1` error, `2` partial backup, `3` guard alert (`3` wins when a run is both).

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
`prune` (not written yet) refuses to run and the pinned snapshot cannot be unpinned.

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
`IMAPVAULT_SNAPSHOT`; if you want a shell, ask for one: `sh -c 'notify "$IMAPVAULT_SUMMARY"'`. The command gets 60
seconds. If it fails the alert is still recorded and the run still exits `3`, with a warning. The summary names
folders and counts, never subjects. The guard compares one run with the one before it, so a slow drip of deletions
spread over many runs is not caught.

**Server footprint.** On a server that does not track mod-sequences until a client asks (dovecot does this), the
first `EXAMINE (CONDSTORE)` makes it start tracking them for that mailbox. No message or flag changes, but it is
more than a plain `EXAMINE`.

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
