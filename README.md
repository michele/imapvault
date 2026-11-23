# imapvault

An open-source IMAP backup tool: point-in-time **snapshots** of a mailbox in a local
repository, a grace period for deleted mail, mass-deletion alerts, and restore of any
snapshot, folder or message back to an IMAP server.

**Status: pre-alpha (M1.5, repository and encryption).** `init`, `keygen` and `check` work; nothing backs up yet. See [docs/PLAN.md](docs/PLAN.md).

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
imapvault --account personal check             # index + object presence; exit 1 on errors
imapvault --account personal check --read-data # verify every object (no private key needed)
imapvault --account personal check --decrypt   # decrypt and re-hash everything (needs the key)
```

`init` binds the repository to the account's host and username; `check` and later `backup` refuse a
different account. Neither command needs the IMAP password.

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
key is needed only to read mail (restore, show, `check --decrypt`). Without any private key the data cannot be
recovered, so back it up. What stays readable, what an attacker with the repository still learns, and how to
recover a message with the stock `age` and `zstd` tools are in [docs/PLAN.md](docs/PLAN.md#encryption).
