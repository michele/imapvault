# Security

## Reporting a vulnerability

Please report security problems privately through GitHub: *Security* tab → *Report a vulnerability*
on [michele/imapvault](https://github.com/michele/imapvault/security/advisories/new). Do not open a
public issue for anything that could expose mail, credentials or keys. Expect an answer within a week.
Only the latest release is supported while the project is pre-1.0.

## What imapvault promises

These are enforced by tests, not just by convention:

- **Backup is read-only on the server.** Folders are opened with `EXAMINE`, bodies fetched with `BODY.PEEK[]`.
  An AST test fails the build on any write command in the IMAP reader.
- **Restore only appends.** It never deletes, moves, flags or overwrites mail. Only `internal/imaptarget` can
  send `CREATE` or `APPEND`, only the restore engine may import it, and it needs an approved plan token and
  `[restore] allow_append = true` on the target account.
- **Backup never needs the private key.** Backup, forget, prune and `check --read-data` work with public keys
  only; everything descriptive in the index is sealed with age.
- **Only `prune` deletes data**, and it honours pins, the grace period and unacknowledged alerts.
- **No message bodies, subjects or credentials in logs or errors.**

## What it does not protect

An attacker with the repository but not the private key still sees message counts, folder tree shape, sizes,
flags, timestamps and whether one known message is present (see the Encryption section of
[docs/PLAN.md](docs/PLAN.md)). A repository created without `encryption.recipients` is plaintext. The IMAP
password lives wherever you configure it (`password_file`, `password_command`, environment).
