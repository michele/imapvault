# imapvault

An open-source IMAP backup tool: point-in-time **snapshots** of a mailbox in a local
repository, a grace period for deleted mail, mass-deletion alerts, and restore of any
snapshot, folder or message back to an IMAP server.

**Status: pre-alpha (M1, repository).** `init` and `check` work; nothing backs up yet. See [docs/PLAN.md](docs/PLAN.md).

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
imapvault --account personal init              # create the repository at repository.path
imapvault --account personal check             # index + object presence; exit 1 on errors
imapvault --account personal check --read-data # also re-hash every object (slow)
```

`init` binds the repository to the account's host and username; `check` and later `backup` refuse a
different account. Neither command needs the IMAP password.
