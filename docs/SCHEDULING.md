# Running imapvault on a schedule

imapvault is a one-shot command: `backup` makes one snapshot and exits. Run it from systemd timers or cron.
(For a container, see [DEPLOY.md](DEPLOY.md): a compose file where the scheduled command is `docker compose run --rm -T backup`.) Run it hourly if you like: a folder the server reports as
unchanged costs one `EXAMINE`.

The repository lock makes overlap harmless. A second `backup` that starts while one is running exits at once
with an error instead of waiting, so you do not need `flock(1)` around it.

## Exit codes

| Code | Meaning | What to do |
|---|---|---|
| 0 | snapshot written | nothing |
| 1 | error (server unreachable, bad config, repository problem), no snapshot | read the message; the next run retries |
| 2 | **partial**: some folders were not reached; the snapshot is marked partial | the next run finishes; look if it keeps happening |
| 3 | **guard alert**: mass deletion or a vanished folder; snapshot flagged, previous one pinned | `imapvault alerts`, then `diff`, `restore`, `ack` |

`3` wins over `2` when a run is both. Both `2` and `3` make a systemd unit show as failed, on purpose.
Set `guard.on_alert` in the config for an immediate notification (see the README); `OnFailure=` below also
catches exit `1` and `2`.

## What the logs contain

Under a timer, cron or a container, stderr is not a terminal, so `backup` prints plain progress lines (see
"Progress" in the README) before its summary. They are written to the journal or your log file:

```
Bulk [3/9]: fetching 4000 messages (780.8 KiB)
Bulk [3/9]: 2608/4000 (65%), 508.3/780.8 KiB, 16.9 KiB/s, ETA 16s     <- at most every 30 s
Bulk [3/9]: done, full, 4000 messages, +4000 -0 ~0 (780.8 KiB in 46s)
Snapshot 7: 4013 messages in 9 folders
...
```

An hourly run with no new mail prints only the summary. To silence the progress lines use `--progress none`.

## Which machine, which key

A scheduled backup needs the **public** keys only. Put `recipients` in the config and leave `identity_file`
out on the machine that runs `backup`, `forget`, `prune` and `check --read-data`. Keep the private key where you
read or restore mail. See [examples/backup-only.toml](../examples/backup-only.toml).

## systemd (system service)

Files in [`deploy/systemd/`](../deploy/systemd) (also inside the release archives):

| File | Install as |
|---|---|
| `imapvault-backup@.service`, `imapvault-backup@.timer` | `/etc/systemd/system/` |
| `imapvault-prune@.service`, `imapvault-prune@.timer` | `/etc/systemd/system/` |
| `imapvault.sysusers` | `/etc/sysusers.d/imapvault.conf` |
| `user/imapvault-backup@.service`, `.timer` | `~/.config/systemd/user/` (no root, see below) |

One instance per account: `imapvault-backup@fastmail.timer`, `imapvault-backup@work.timer`, ...

| What | Where |
|---|---|
| binary | `/usr/local/bin/imapvault` |
| user and group | `imapvault` (a system user, no login) |
| config for account `NAME` | `/etc/imapvault/NAME.toml` (no secrets in it) |
| IMAP password | `/etc/imapvault/NAME.password` (owner `imapvault`, mode `0600`) |
| repository for `NAME` | `/var/lib/imapvault/NAME` |

### Install

Take the archive for your machine from the releases page, then, as root:

```sh
install -m 0755 imapvault /usr/local/bin/imapvault
imapvault --version

install -D -m 0644 deploy/systemd/imapvault.sysusers /etc/sysusers.d/imapvault.conf
systemd-sysusers                                    # creates the imapvault user and group
install -d -m 0755 /etc/imapvault
install -d -o imapvault -g imapvault -m 0700 /var/lib/imapvault
install -m 0644 deploy/systemd/imapvault-backup@.service deploy/systemd/imapvault-backup@.timer \
                deploy/systemd/imapvault-prune@.service  deploy/systemd/imapvault-prune@.timer \
                /etc/systemd/system/
systemctl daemon-reload
```

### Set up an account

For an account called `fastmail`, write `/etc/imapvault/fastmail.toml` (start from `examples/`). The units expect
two things to match:

```toml
[imap]
host = "imap.fastmail.com"
username = "you@example.com"
password_file = "/etc/imapvault/fastmail.password"

[repository]
path = "/var/lib/imapvault/fastmail"     # the units wait for repo.toml in this folder

[encryption]
recipients = ["age1..."]                  # public keys only on this machine
```

```sh
install -o imapvault -g imapvault -m 0600 /dev/null /etc/imapvault/fastmail.password
# put the app password in it, one line (for example: sudoedit, or `systemd-ask-password | sudo tee`)
```

### First run

`init` and the first backup are done by hand, as the service user, so a mistake shows on your terminal:

```sh
sudo -u imapvault imapvault --config /etc/imapvault/fastmail.toml init
sudo -u imapvault imapvault --config /etc/imapvault/fastmail.toml doctor
sudo -u imapvault imapvault --config /etc/imapvault/fastmail.toml backup     # the first one can take hours
systemctl enable --now imapvault-backup@fastmail.timer imapvault-prune@fastmail.timer
```

Run the commands as `imapvault`, never as root: root would leave root-owned files in the repository that the
timer could not write.

### Day to day

```sh
systemctl list-timers 'imapvault-*'
journalctl -u imapvault-backup@fastmail -n 50
systemctl start imapvault-backup@fastmail.service        # run one now
sudo -u imapvault imapvault --config /etc/imapvault/fastmail.toml snapshots
```

`imapvault-prune@.service` runs `forget` then `prune` weekly. `prune` refuses while a guard alert is
unacknowledged, so the unit fails until you have looked (`imapvault alerts`) and run `imapvault ack ID`.

### Notify on failure

```ini
# /etc/systemd/system/imapvault-alert@.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/notify-me "imapvault backup %i failed: journalctl -u imapvault-backup@%i"
```

then uncomment `OnFailure=imapvault-alert@%i.service` in `imapvault-backup@.service` (or set it in a drop-in:
`systemctl edit imapvault-backup@.service`).

### Repository somewhere else (NAS, another disk)

Point `[repository] path` at it and add a drop-in so the sandbox lets the service write there:

```sh
systemctl edit imapvault-backup@fastmail.service
#   [Service]
#   ReadWritePaths=/mnt/nas/imapvault/fastmail
#   [Unit]
#   ConditionPathExists=/mnt/nas/imapvault/fastmail/repo.toml
```

Do the same for `imapvault-prune@fastmail.service`. The repository lock uses `flock(2)`; check that your
network filesystem supports it before putting a repository on NFS or SMB (see "NAS notes" in DEPLOY.md).

### No root: a user service

```sh
mkdir -p ~/.config/systemd/user ~/.local/bin
install -m 0755 imapvault ~/.local/bin/imapvault
cp deploy/systemd/user/imapvault-backup@.* ~/.config/systemd/user/
imapvault --account fastmail init          # config in ~/.config/imapvault/fastmail.toml
imapvault --account fastmail backup
systemctl --user daemon-reload
systemctl --user enable --now imapvault-backup@fastmail.timer
loginctl enable-linger "$USER"             # keep timers running when you are logged out
```

## cron

```cron
# m h dom mon dow  command
17 * * * *  /usr/local/bin/imapvault --config /etc/imapvault/fastmail.toml backup >>/var/log/imapvault.log 2>&1
23 4 * * 0  /usr/local/bin/imapvault --config /etc/imapvault/fastmail.toml forget && /usr/local/bin/imapvault --config /etc/imapvault/fastmail.toml prune >>/var/log/imapvault.log 2>&1
```

Cron mails or logs whatever the command prints, but it does not look at the exit code. To be told about
exit `1`, `2` and `3`, wrap the line: `... backup || notify-me "imapvault failed ($?)"`, or rely on
`guard.on_alert` for mass deletions.

## launchd (macOS)

A `StartCalendarInterval` agent that runs `imapvault --account NAME backup` hourly works the same way
(`~/Library/LaunchAgents`). Use a password source launchd can reach: `password_command` calling the keychain
(`security find-generic-password -s imapvault -w`) works; an unlocked 1Password CLI usually does not.

## What was verified

The system and user units pass `systemd-analyze verify` on systemd 257 (Debian 13). They have **not** been run as
live services under every sandbox option on every distribution. The hardening lines (`MemoryDenyWriteExecute`,
`SystemCallFilter`, `ProtectProc`) are the ones quietbox runs under; if your systemd rejects one, remove it in a
drop-in (`systemctl edit`) and report it. The NFS/SMB `flock(2)` caveat above is untested.
