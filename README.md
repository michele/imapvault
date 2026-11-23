# imapvault

An open-source IMAP backup tool: point-in-time **snapshots** of a mailbox in a local
repository, a grace period for deleted mail, mass-deletion alerts, and restore of any
snapshot, folder or message back to an IMAP server.

**Status: pre-alpha (M2, first full backup).** `init`, `keygen`, `backup`, `snapshots`, `ls`, `show`, `doctor` and `check` work. There is no restore, retention or deletion alert yet. See [docs/PLAN.md](docs/PLAN.md).

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
imapvault --account personal backup            # read-only run -> a new snapshot
imapvault --account personal snapshots         # list snapshots
imapvault --account personal ls latest         # folders in a snapshot ("latest" or a number)
imapvault --account personal ls latest INBOX   # messages in a folder: UID, flags, size, date, sender, subject, hash
imapvault --account personal show 3f9a1c2b     # one message by hash prefix; --raw writes the original .eml to stdout
imapvault --account personal check             # index + object presence; exit 1 on errors
imapvault --account personal check --read-data # verify every object (no private key needed)
imapvault --account personal check --decrypt   # decrypt and re-hash everything (needs the key)
```

`init` binds the repository to the account's host and username; `check` and `backup` refuse a
different account. `init` and `check` do not need the IMAP password; `backup` and `doctor` do.

`backup` never changes the mailbox: folders are opened with `EXAMINE` and messages fetched with `BODY.PEEK[]`.
Each run lists every selected folder in full and compares it with the repository, so new mail, deleted mail and
flag changes are all recorded, and a message already stored is not downloaded again. A deleted message stays in
the repository (and in older snapshots); only `prune` (not written yet) removes data. If a run fails, folders it
finished stay stored but no snapshot is written; the next run reuses them. Consistency is per folder, because
IMAP has no atomic view of a whole account. Ctrl-C ends a run the same way.

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
