# Running imapvault in Docker (NAS, home server)

This guide runs imapvault from the container image with the compose file in [`deploy/`](../deploy), on a NAS
(Synology, QNAP, Unraid, TrueNAS), a Raspberry Pi or any Linux box with Docker or Podman. Output quoted below
was produced by the image against the dovecot test server unless a section says otherwise; see
[Not verified](#not-verified) for what was not tried.

Files: [`Dockerfile`](../Dockerfile), [`deploy/docker-compose.yml`](../deploy/docker-compose.yml),
[`deploy/docker-compose.build.yml`](../deploy/docker-compose.build.yml),
[`deploy/imapvault.example.toml`](../deploy/imapvault.example.toml), [`deploy/.env.example`](../deploy/.env.example).

## How it fits together

- **`run` keeps it going.** `docker compose up -d` starts one container that backs up every hour, tidies old
  snapshots daily, can prune on request, and tells you when something is wrong
  ([Scheduling it](#scheduling-it)). The one-shot commands (`backup`, `forget`, ...) stay available for manual
  use, or for a scheduler of your own on the host.
- **One compose project per mailbox account** (or one block of services per account, see
  [Several accounts](#several-accounts)). Each has its own config file, its own repository folder and its own
  password secret.
- **Two services.** `run` is the daemon: `docker compose up -d` starts it. `imapvault` is the command line for
  everything else, with the account already set: what you type after it is the imapvault command, and the
  container exits when the command does.

  | Run | Does |
  |---|---|
  | `docker compose up -d` | starts `run`: backup on an interval, forget daily, prune if you ask, notifications |
  | `docker compose run --rm imapvault init` | creates the repository (once) |
  | `docker compose run --rm imapvault backup` | one backup, then exit: the first run, a manual one, or from a host scheduler |
  | `docker compose run --rm imapvault forget` | applies the retention policy to snapshots |
  | `docker compose run --rm imapvault prune --dry-run` | shows what prune would remove; leave out `--dry-run` to do it |
  | `docker compose run --rm imapvault check --read-data` | reads every object back and verifies its hash |
  | `docker compose run --rm imapvault snapshots` | any other imapvault command works the same way |
  | `docker compose run --rm imapvault --account work snapshots` | another account: a later `--account` wins over the preset one |

  Only `run` is started by `docker compose up`; `imapvault` is in the `tools` profile and starts when you name
  it. (`run` takes its flags from `.env`, and is not meant to be started by hand.)
- **The container has no key to your mail.** A backup host holds the *public* age keys (`recipients`) and never
  the private one. It can back up, forget, prune and verify; it cannot read the mail back. See
  [Reading and restoring](#reading-and-restoring).
- **The repository needs a local filesystem.** The repository lock uses `flock(2)`. Keep `repo/` on a disk of the
  NAS itself (a normal bind mount or volume), not on an NFS or SMB share.
- **The image** holds one static binary (`/imapvault`) on distroless `static-debian12:nonroot`: CA certificates
  and tzdata, no shell, no other programs. Nothing is baked in: no config, no password, no key, no repository.

## Prerequisites

- Docker with the compose plugin (`docker compose version`, v2), or Podman with `podman compose`.
  Synology DSM 7.2+: Container Manager. Portainer works too (see [NAS notes](#nas-notes)).
- An app password for the mailbox (see [Passwords](#passwords)).
- amd64 or arm64 CPU (most NAS and the Raspberry Pi 4/5 with a 64-bit OS). 32-bit ARM is not built.
- For an encrypted repository: a key pair made elsewhere (`imapvault keygen --out personal.key`). Decide this
  before `init`: encryption cannot be added later.

## Get the image

Pick one.

1. **Registry** (the default in the compose file): `ghcr.io/michele/imapvault`, tags `latest`, `X.Y.Z` and
   `sha-<short>`, for linux/amd64 and linux/arm64. It is published by
   [`docker.yml`](../.github/workflows/docker.yml) when a `v*` tag is pushed. `docker compose pull` fetches it.
2. **Build locally** (needs a checkout on the machine that builds). Set `IMAPVAULT_IMAGE=imapvault:local` in
   `.env` once, then:

   ```sh
   cd deploy
   docker compose -f docker-compose.yml -f docker-compose.build.yml build run   # tags imapvault:local
   ```

   Or `make docker-build` from the checkout, and for another architecture
   `docker build --platform linux/arm64 -t imapvault:local . && docker save imapvault:local | ssh nas docker load`.

## Layout on the host

Copy the `deploy/` folder to the NAS (a folder on a local volume) and work inside it:

```
/volume1/docker/imapvault/
  docker-compose.yml
  .env                      your PUID, PGID, TZ, account name
  config/fastmail.toml      the account's config, no secrets in it
  secrets/imap_password     the IMAP app password, one line
  repo/                     the repository (blobs and index); must be writable by PUID:PGID
```

**The config folder is mounted one level down.** `imapvault --account NAME` reads
`$XDG_CONFIG_HOME/imapvault/NAME.toml`, and the image sets `XDG_CONFIG_HOME=/config`. The compose file therefore
mounts `./config` at `/config/imapvault`, so you keep the file flat on the host and the container finds it:

| On the host | In the container |
|---|---|
| `config/fastmail.toml` | `/config/imapvault/fastmail.toml` |
| `secrets/imap_password` | `/run/secrets/imap_password` |
| `repo/` | `/repo` |

The file name is the account name, and `IMAPVAULT_ACCOUNT` in `.env` must match it. If you mount a folder of
your own, the file must end up at `/config/imapvault/NAME.toml`, not `/config/NAME.toml`. When it is missing:

```
error: no config for account "fastmail": expected /config/imapvault/fastmail.toml. Create it there (see examples/), or set XDG_CONFIG_HOME to a folder that contains imapvault/fastmail.toml. In a container, mount the folder that holds fastmail.toml at /config/imapvault
```

## Configuration

```sh
cp .env.example .env              # then set IMAPVAULT_ACCOUNT, PUID, PGID, TZ
mkdir -p config repo secrets
cp imapvault.example.toml config/fastmail.toml
```

`.env`:

| Variable | Meaning |
|---|---|
| `IMAPVAULT_ACCOUNT` | the account name, i.e. the config file `config/<name>.toml`. Required: without it compose stops with `set IMAPVAULT_ACCOUNT in .env`. |
| `PUID`, `PGID` | the uid and gid the container runs as. Use the owner of `secrets/imap_password` and `repo/` (`id` on the NAS). Default `65532`, the image's user. |
| `TZ` | the time zone for the times imapvault prints. |
| `IMAPVAULT_IMAGE` | the image to run; pin a version here ([Updating](#updating)). |
| `IMAPVAULT_EVERY` | how often `run` backs up (default `1h`). |
| `IMAPVAULT_FORGET_EVERY` | how often `run` applies the retention policy (default `24h`; `0` off). |
| `IMAPVAULT_PRUNE_EVERY` | how often `run` prunes (default `0`: never). Needs forget. |
| `IMAPVAULT_STALE_AFTER` | tell me when no backup has succeeded for this long (default `24h`; `0` off). |

`config/fastmail.toml` (the example file has more comments):

```toml
[imap]
host = "imap.fastmail.com"
username = "you@example.com"
password_file = "/run/secrets/imap_password"

[repository]
path = "/repo/fastmail"

[folders]
exclude = ["Spam", "Junk"]

[encryption]
recipients = ["age1workingkey...", "age1recoverykey..."]   # public keys only
```

Leave `identity_file` out (the private key does not belong here), leave `[restore]` out (this host never writes to
your mail server) and leave `[guard] on_alert` out: the hook runs a program directly, and the image has no
programs to run. To be told about a guard alert or a long run of failed backups, use
[`on_alert_url`](#notifications) instead. The rest of the options are in the README and `examples/`.

## Passwords

Use an **app password**, never your main account password. Fastmail: Settings > Privacy & Security >
Integrations > New app password, access "Mail (IMAP)". Gmail: turn on 2-step verification, then Google Account >
Security > App passwords (IMAP must be enabled in Gmail settings). An app password can be revoked on its own if
the NAS is ever compromised.

### 1. Compose `secrets:` + `password_file` (what the compose file does)

The password lives in one file on the host, outside the config, outside the image and outside the container's
environment (so not in `docker inspect`), and is mounted read-only at `/run/secrets/imap_password`.

```sh
scp laptop:path/to/password-file secrets/imap_password   # or write it with an editor (umask 077 first)
chmod 600 secrets/imap_password
id                                               # e.g. uid=1026(you) gid=100(users): put these in .env
sudo chown 1026:100 secrets/imap_password
sudo chown 1026:100 repo && chmod 700 repo
```

Avoid typing the password on a command line: it lands in the shell history. If you must,
`umask 077; printf '%s\n' 'the-app-password' > secrets/imap_password`.

**The uid pitfall.** Outside Swarm, a compose file secret is a bind mount of the host file: the container sees it
with its host owner and mode. The image runs as uid 65532 by default, so a `0600` file owned by your NAS user is
unreadable inside:

```
error: /config/imapvault/fastmail.toml: imap.password_file /run/secrets/imap_password: open /run/secrets/imap_password: permission denied
```

Fix it one of two ways:

- **Run as the owner (preferred):** set `PUID`/`PGID` in `.env` to the owner of `secrets/imap_password`, and own
  `repo/` by the same uid. The file stays `0600`.
- **Shared group:** keep `PUID=65532`, give the file a group the container is in and make it group-readable
  (`chgrp 100 secrets/imap_password && chmod 0440 secrets/imap_password`, `PGID=100`). imapvault refuses a
  group- or world-readable password file outside a container, but inside one it only warns, on every command
  that reads the password:

  ```
  warning: password file /run/secrets/imap_password is readable by other users (mode 0640); fine for a mounted container secret, not elsewhere
  ```

  Container detection uses `/.dockerenv`, `/run/.containerenv` or `KUBERNETES_SERVICE_HOST`. If it guesses
  wrong, set `IMAPVAULT_CONTAINER=1` (or `0`) under `environment:`.

The same uid must be able to write `repo/`. When it cannot, `init` stops with:

```
error: create /repo/fastmail/objects: mkdir /repo/fastmail/objects: permission denied
```

### 2. The environment

Without `password_file` and `password_command` in the config, imapvault uses `IMAPVAULT_IMAP_PASSWORD_<NAME>`
(the account name in capitals, `-` as `_`) or `IMAPVAULT_IMAP_PASSWORD`. The password is then in the container's
environment: visible with `docker inspect` and in `/proc/<pid>/environ`. Never write it inline under
`environment:` in the compose file (that file gets copied, shared and committed); use an `env_file:` kept in
`secrets/` (`*.env` is ignored by git) and remove `secrets:` and `password_file`.

### 3. `password_command`

Runs a command (30 s limit) and uses its single line of output. The default image has no shell and no secret
tool, so this is for a **custom image** only. That tool needs its own credentials, which moves the problem
rather than solving it on a single NAS; option 1 is simpler.

### Other platforms

- **Docker Swarm:** `docker secret create imap_password -` and `secrets: [imap_password]` with `external: true`;
  Swarm honours `uid`/`gid`/`mode` on a secret, so `uid: "65532"`, `mode: 0400` works.
- **Kubernetes:** a `Secret` mounted as a volume at `/run/secrets` with `password_file = "/run/secrets/imap_password"`.
  Secret volume files are owned by root with group = `fsGroup`, so use `defaultMode: 0440` with `fsGroup: 65532`
  (imapvault only warns about a group-readable file inside a container).
- **systemd without Docker:** see [SCHEDULING.md](SCHEDULING.md) (`LoadCredential=`).

### Rotating the password

1. Create the new app password at the provider (keep the old one for now).
2. Write it into the existing file: `umask 077; printf '%s\n' 'new-password' > secrets/imap_password`.
   This keeps the file's owner and mode.
3. The next run reads it; each run is a fresh container. Then revoke the old one.

## First run

```sh
docker compose run --rm imapvault init
docker compose run --rm imapvault backup
```

`init` binds the repository to the account's host and username and, if you set `recipients`, to encryption.
Read what it prints: `mail NOT encrypted` means no recipients were configured, and that cannot be changed later.

The first backup can take hours. Start it inside `tmux` or `screen` over SSH (or `docker compose run -d ...` and
follow it with `docker compose logs -f`). If it is interrupted (even killed), the next run resumes where it
stopped: downloaded messages are committed in batches.

**What you see while it runs** depends on whether the output is a terminal. `docker compose run` gives the
container a terminal, so you get one live line:

```
[ 3/9] Bulk         ██████░░░░░░  616/1,200  118.1/230.6 KiB  19.2 KiB/s  ETA 6s
```

Without one (`up`, a scheduler, `docker compose logs`) you get plain lines, which is what you want in a log:

```
Bulk [3/9]: fetching 4000 messages (780.8 KiB)
Bulk [3/9]: 2608/4000 (65%), 508.3/780.8 KiB, 16.9 KiB/s, ETA 16s
Bulk [3/9]: done, full, 4000 messages, +4000 -0 ~0 (780.8 KiB in 46s)
```

The compose file does not set `--progress`, so each case gets the right one. Force the plain lines in a terminal
with `docker compose run --rm imapvault backup --progress log`, or silence them with `--progress none`. A folder with
nothing new prints nothing, so folder numbers skip. There is no account-wide total: the bar covers one folder at
a time.

When the first backup has finished, `docker compose up -d` hands over to `run` (next section). A second backup
right after reads nothing and prints only the summary:

```
Snapshot 2: 9725 messages in 9 folders
  messages  +0 new, -0 deleted, ~0 flag changes
  storage   0 blob(s) written (0 B), 0 already stored
  read      0 body(ies) downloaded; folders: 9 unchanged, 0 incremental, 0 full
```

## Scheduling it

### `run`: let imapvault schedule itself

`docker compose up -d` starts the `run` service. It does one job at a time and each job takes the repository lock
only while it runs:

| Job | When | Notes |
|---|---|---|
| backup | at once, then every `IMAPVAULT_EVERY` (default `1h`), counted from the *start* of the previous run | a first backup that takes 3 hours is followed straight away by an incremental one |
| forget | after the first good backup, then every `IMAPVAULT_FORGET_EVERY` (default `24h`) | only marks snapshots; deletes nothing |
| prune | only if `IMAPVAULT_PRUNE_EVERY` is set (for example `7d`) | the usual prune: it still honours pins, the deleted-mail grace period and unacknowledged alerts, and the first one comes a full period after start |

What it does when things go wrong:

- **A backup fails** (server unreachable, bad password): it retries after 30 s, 1 m, 2 m, then every 5 m, and logs the
  error. It never gives up. The config is read again for every backup, so a rotated password file is picked up
  without a restart.
- **Someone else holds the repository** (your manual `prune`): the backup is logged as `busy` and retried in a
  minute. It is not counted as a failure.
- **The guard trips** (a mass deletion): the alert is logged, the notifications below go out, and the loop *keeps
  going*. The previous snapshot is already pinned, later runs compare with the flagged snapshot and stay quiet, and
  `prune` stays refused until you acknowledge the alert. While one is open, `run` repeats a reminder in the log
  after every forget.
- **Backups keep failing:** after `IMAPVAULT_STALE_AFTER` without a success it sends one notification per streak.
  Without exit codes to watch, this is how a daemon that quietly cannot reach your mail gets noticed.
- **The config or repository cannot be opened at start:** it exits `1` at once, and `restart: unless-stopped` turns
  that into a visible restart loop (see [Troubleshooting](#troubleshooting)).

Backing up more often than hourly gains little. Every run writes a copy of the index, and the mass-deletion guard
compares each run with the one before: someone deleting a little at a time stays under its limits when you back up
every few minutes. The shortest `--every` is 5 minutes.

What the log looks like (from a test with intervals of a few seconds, so the waits are short; the long first-run
progress lines are left out):

```
run: started: backup every 6s, forget every 6s, prune every 20s
run: backup started
run: backup done: snapshot 1, 213 messages, +213 new, -0 deleted, ~0 flag changes (3s)
run: forget: kept 1 snapshot(s), forgot 0
run: next backup in 3s
run: backup started
run: backup done: snapshot 3, 63 messages, +0 new, -150 deleted, ~0 flag changes (0s)
run: GUARD ALERT 1 (mass-deletion): 150 of 213 messages deleted (70.4%) since snapshot 2: Bulk 150. Snapshot 3 is flagged and 2 is pinned; read it and acknowledge it from a machine that holds the key
run: 1 unacknowledged guard alert(s): run `imapvault alerts` on a machine that holds the key, then `imapvault ack`; prune stays off until then
run: prune refused: 1 unacknowledged guard alert(s): look at `imapvault alerts`, then `imapvault ack`; prune stays off until then
run: stopping
```

Read it with `docker compose logs -f -t run` (the lines carry no timestamps of their own). A failed backup reads
`run: backup failed: <error>; retrying in 30s`.

**Manual commands still work.** Between jobs, `docker compose run --rm imapvault prune --dry-run`, `check` or `snapshots` run
as usual. During a job, they fail at once, naming who holds the lock:

```
error: repository is locked: exclusive lock refused (held by pid 59221 on babyyoda.4.lan since 2026-11-24T14:47:36Z)
```

**Stopping.** `docker compose stop run` (or `down`, or an update) sends SIGTERM: a backup in progress stops
cleanly, keeps what it already downloaded as a partial snapshot, and `run` exits `0`. The next start continues from
there (tested: it fetched only the messages that were missing).

#### Notifications

`[guard] on_alert_url` makes imapvault POST a plain-text message (with a `Title` header) to a URL when the guard
trips and when backups have been failing for too long. It works with [ntfy](https://ntfy.sh/) and anything else
that accepts a text POST. The URL of a topic works like a password, so keep it in a secret file, like the IMAP
password:

```sh
umask 077; printf '%s\n' 'https://ntfy.sh/your-long-random-topic' > secrets/alert_url
```

```toml
[guard]
on_alert_url_file = "/run/secrets/alert_url"
```

Mount it in the compose file by adding a second secret to the shared `x-imapvault` block and one entry to the
top-level `secrets:` (every service that can run a backup needs it, so put it in the shared block):

```yaml
x-imapvault: &imapvault
  # ...
  secrets:
    - source: imap_password
      target: imap_password
    - source: alert_url
      target: alert_url

secrets:
  imap_password:
    file: ./secrets/imap_password
  alert_url:
    file: ./secrets/alert_url
```

`https` is always accepted; `http` only for a local address (an IP in a private range, `localhost`, or a name without
dots such as a compose service called `ntfy`). A URL that would send the message in clear text across the internet is
refused when the config is loaded. Neither the log nor any error ever contains the URL. A failed delivery is a
warning and never fails a backup. There is also `on_alert_url = "..."` for a URL that is not a secret.
imapvault does not send anything to ntfy.sh or anywhere else unless you configure this. (It was tested against a
local listener, not against ntfy.sh itself.)

### Or schedule it from the host

If you would rather have cron or Task Scheduler decide, leave `run` alone and call the `imapvault` service. Do not do
both: they would take turns on the lock. Hourly `backup`, and weekly `forget` then `prune`, is a reasonable start
(see [SCHEDULING.md](SCHEDULING.md)). A folder the server reports as unchanged costs one `EXAMINE`, so an hourly
run with no new mail is cheap.

**From a scheduler, run `docker compose run --rm -T imapvault <command>`.** `-T` says there is no terminal. Docker Compose
v2 notices that by itself (a run with no terminal worked without it); compose v1 stops with "the input device is
not a TTY" without `-T`, and it does no harm with v2. Use `run --rm`, which removes the container when the command ends (`docker compose
down` ignores services in the `tools` profile, so a container left by `up imapvault` would stay behind).

Cron on a Linux host:

```cron
17 * * * *  cd /srv/imapvault && docker compose run --rm -T imapvault backup >>/var/log/imapvault.log 2>&1
23 4 * * 0  cd /srv/imapvault && docker compose run --rm -T imapvault forget >>/var/log/imapvault.log 2>&1
33 4 * * 0  cd /srv/imapvault && docker compose run --rm -T imapvault prune >>/var/log/imapvault.log 2>&1
```

**Synology:** Control Panel > Task Scheduler > Create > Scheduled Task > User-defined script, run as `root`, with
the same `cd ... && docker compose run --rm -T imapvault backup` line (use the full path `/usr/local/bin/docker`
if the task cannot find it). The task can email you when a run ends with a non-zero code.

A slow run does not get doubled: the repository lock refuses a second one at once, and that run exits `1`:

```
error: repository is locked: exclusive lock refused (held by pid 1 on 0cccbd6bca84 since 2026-11-24T13:13:50Z)
```

While the first backup is still going, the hourly runs fail this way. That is expected, not damage.

### Exit codes

For a command run through the `imapvault` service (a scheduler on the host): the container's exit code is imapvault's, and
`docker compose run` passes it on. (`run` logs these events instead; it only ever exits `0` when stopped or `1` at
start.)

| Code | Meaning | What to do |
|---|---|---|
| 0 | snapshot written | nothing |
| 1 | error (server unreachable, bad config or password, repository locked or unwritable) | read the message; the next run retries |
| 2 | **partial**: some folders were not reached; the snapshot is marked partial | the next run finishes; look if it keeps happening |
| 3 | **guard alert**: mass deletion or a vanished folder; snapshot flagged, the previous one pinned | read the alert from a machine that holds the key, then `diff`, `restore`, `ack` |

A scheduler that mails on a non-zero exit therefore mails on `1`, `2` and `3`. To tell `3` apart:

```sh
#!/bin/sh
cd /srv/imapvault || exit 1
docker compose run --rm -T imapvault backup
rc=$?
[ "$rc" -eq 3 ] && echo "imapvault: GUARD ALERT, check the mailbox before the next prune" >&2
exit "$rc"
```

The alert text is sealed in an encrypted repository, so reading it (`alerts`) needs the private key, which this
host does not have. Until you `ack` it, `prune` keeps what the alert pinned.

**systemd** instead of cron: a `Type=oneshot` service with `WorkingDirectory=/srv/imapvault` and
`ExecStart=/usr/bin/docker compose run --rm -T imapvault backup`, started by a timer with `OnCalendar=hourly` and
`Persistent=true`, behaves the same, and a non-zero exit shows as a failed unit. (Written from the systemd
documentation, not run.)

## Reading and restoring

`ls`, `show`, `diff` with names, `alerts`, `restore` and `check --decrypt` need the private key. Do not put it on
this host. Run them from a machine you trust, with the repository mounted or copied and `identity_file` set in
that machine's config. `restore` also needs `[restore] allow_append = true` in the target account's config, and it
only ever appends (see the README). `snapshots`, `check` and `check --read-data` do not need the key.

## Day-2

### Logs

`docker compose logs -f -t run` follows the daemon (its own lines, plus the per-folder progress lines of each
backup that finds something new). Logs rotate: 3 files of 10 MB. For a one-shot service,
`docker compose logs --tail 50 backup` shows its last run.

### Updating

```sh
# 1. copy the repository first (below): a newer imapvault may migrate it, and an older one then refuses to open it
docker compose pull
docker compose up -d                    # recreates the container; SIGTERM stops the old one cleanly
```

Pin a version (`IMAPVAULT_IMAGE=ghcr.io/michele/imapvault:0.2.0` in `.env`) instead of `latest` if you want
updates only when you choose. A repository written by a newer imapvault is refused by an older one with a clear
message, so rolling back means restoring the copy from step 1. For a local build see [Get the image](#get-the-image).

### Backing up the repository itself

imapvault protects mail from the server; it does not protect the repository from the disk. Copy `repo/` elsewhere
(restic, rsync, a filesystem or NAS snapshot), while no run is active (a running backup holds the lock). Every
backup also writes a consistent copy of the index to `repo/<account>/index-backups/`, which is a safety net for
the index only, not for the blobs.

### Stopping

`docker compose stop` (or `down`, or `docker stop`) sends SIGTERM. A running backup stops cleanly: it keeps what
it already downloaded and records a partial snapshot (as Ctrl-C does), `run` exits `0`, and the next run continues
from there.
Tested: a `stop` in the middle of a 25,000-message folder returned in 0.2 s (well within the 60 s
`stop_grace_period`), and the next run downloaded only the messages that were missing (the snapshot then held all
28,013 messages). The stopped container's own log was not read: it was started with `--rm`.

### Resources

`mem_limit: 1g` and `GOMEMLIMIT: 700MiB` were chosen before measuring. The maintainer's first backup of a real
mailbox (about 100,000 messages, 20 GiB) stayed at roughly 360 MB the whole time, reported from memory and not
recorded, so the limits have room. A message is held in memory while it is hashed, and a full folder listing is
held while it is compared, so a mailbox with very large attachments or far more messages may need more. `GOMEMLIMIT` makes the Go
runtime collect garbage harder before it reaches the container limit (Go does not size its heap to the cgroup on
its own). If a run is OOM-killed (`docker inspect` shows `OOMKilled: true`), raise both together.

### Time zone

`TZ` affects the times imapvault prints (snapshots, alerts), not what it does. The container clock is the host
clock: keep NTP on.

## Several accounts

Give each account its own config file, repository folder and secret, and its own services. The config folder is
shared (`config/fastmail.toml`, `config/work.toml`); YAML anchors keep the services short. Add to the compose file:

```yaml
services:
  run-work:
    <<: *imapvault
    restart: unless-stopped
    entrypoint: ["/imapvault", "--account", "work", "run"]
    command: ["--every=1h", "--forget-every=24h", "--stale-after=24h"]
    secrets:
      - source: work_imap_password
        target: imap_password

secrets:
  work_imap_password:
    file: ./secrets/work_imap_password
```

with `path = "/repo/work"` in `config/work.toml`. Run `docker compose run --rm imapvault --account work init`
once, then `docker compose up -d`. For a manual `backup` of `work`, copy the `imapvault` service the same way, with its own `entrypoint`
(`["/imapvault", "--account", "work"]`) and secret (the `imapvault` service mounts the first account's secret,
which is fine for `init`, which needs no password). Two accounts never share a repository: each one is bound to its host and username at `init`. (If you added an `alert_url` secret to the shared block, list it here too: a `secrets:` key on
a service replaces the shared list.)

## NAS notes

- **Paths:** use a folder on a local volume, e.g. Synology `/volume1/docker/imapvault`, QNAP
  `/share/Container/imapvault`, Unraid `/mnt/user/appdata/imapvault`. Never a mounted NFS/SMB share: the
  repository lock uses `flock(2)`, which a network filesystem may not honour.
- **PUID/PGID:** SSH in as the user who owns the folder and run `id`. On Synology the first user is often
  `uid=1026`, group `users` = `100`; check, do not assume. Put the values in `.env` and `chown` `repo/` and
  `secrets/imap_password` to them.
- **Synology Container Manager:** run `init` over SSH first (`sudo docker compose run --rm imapvault init` in the folder),
  and tick "Start the project once it is created" only after that: started without a repository, `run` exits and
  the restart policy keeps retrying. The project then shows `run` as running; the `imapvault` service stays stopped.
- **Portainer:** Stacks > Add stack, paste the compose file, set the `.env` values as stack environment
  variables. Relative paths like `./config` resolve inside Portainer's own data directory, so use absolute
  paths (`/volume1/docker/imapvault/config`) in a pasted stack.
- **SELinux hosts** (Fedora, RHEL): add `:Z` to the bind mounts or the container may be denied access.
- **Back up the repository too**, see [above](#backing-up-the-repository-itself).

## Security hardening

What the compose file sets, and why:

| Setting | Effect |
|---|---|
| `user: PUID:PGID` (image default `65532:65532`, "nonroot") | imapvault never runs as root. |
| `read_only: true` + `tmpfs: /tmp` | the image cannot be modified; only `/repo` (and a 64 MB `/tmp` in RAM) is writable. |
| `cap_drop: [ALL]` | no Linux capabilities; imapvault needs none (it binds no port, changes no ownership). |
| `security_opt: no-new-privileges:true` | nothing in the container can gain privileges through setuid binaries (there are none either). |
| no `ports:` | nothing listens; the only traffic is the outgoing IMAP connection. |
| `/config` read-only, password as a secret file | a compromised process cannot rewrite its config, and the password is not in the environment. |
| public keys only, no `[restore]` | a stolen NAS reveals no message and can never write to your mail server. |
| `mem_limit` (+ `GOMEMLIMIT`), `pids_limit`, log rotation | a runaway process cannot take the NAS down or fill its disk with logs. |
| distroless base, pinned by digest | no shell, no package manager, no surprise base-image changes. |

The backup path is read-only on the mail server: folders are opened with `EXAMINE`, messages fetched with
`BODY.PEEK[]`, and nothing is stored, expunged, moved or deleted there.

## Troubleshooting

- **`run` restarts over and over** (`docker compose ps` says `Restarting`): it stops at start when it cannot open
  the config or the repository. `docker compose logs --tail 5 run` shows the `error:` line: one of the cases below.
  Stop it (`docker compose stop run`) while you fix things.
- **`run: backup failed: ...; retrying in ...`** is not a crash: the next attempt is already scheduled, and a long
  streak sends the stale notification.
- **`run: backup busy: another imapvault command holds the repository`**: a manual command is running; it clears
  by itself.
- **`no config for account "NAME": expected /config/imapvault/NAME.toml ...`**: the file is not where the
  container looks. On the host it is `config/NAME.toml` next to the compose file; `IMAPVAULT_ACCOUNT` must be `NAME`.
- **`required variable IMAPVAULT_ACCOUNT is missing a value`**: no `.env`, or it does not set the account.
- **`... open /run/secrets/imap_password: permission denied`**: the [uid pitfall](#1-compose-secrets--password_file-what-the-compose-file-does).
- **`create /repo/.../objects: permission denied`**: `repo/` is not writable by `PUID:PGID`.
- **`repository is locked: exclusive lock refused`**: another run is in progress (a slow first backup, or a
  `check --read-data`). It ends by itself; the next scheduled run retries.
- **`guard.on_alert_url_file ...: no such file`** at the start of a backup: the `alert_url` secret is not mounted in
  that service. Add it to the shared `x-imapvault` block ([Notifications](#notifications)).
- **`not an imapvault repository: /repo/NAME has no repo.toml`**: you have not run `init` yet, or the repository
  path in the config differs from the folder you initialised.
- **`warning: password file ... is readable by other users`**: expected with the shared-group setup; `chmod 600`
  plus `PUID` = owner makes it go away.
- **`tls = "none"` refused**: only allowed for a loopback host; use `implicit` (993) or `starttls` for a real server.
- **Exit 2 every time**: some folder cannot be read. The warning in the summary names it.
- **The progress line looks garbled in `docker logs`**: it was started with a terminal (`docker compose run`
  without `-T`, or `tty: true`). Add `--progress log`.

## Not verified

- **A real NAS.** Everything above ran with Podman (a rootful VM on macOS) driving the Docker Compose v2 CLI
  against the dovecot test server; permission errors were produced on the VM's own filesystem, because a macOS
  bind mount does not enforce ownership. Synology Container Manager, Task Scheduler, Portainer and the systemd
  unit are written from their documentation, not run. The maintainer ran the v0.2.0 image on a NAS and reported
  the progress output looked right; nothing from that run was recorded here.
- **Docker Engine itself** (as opposed to Podman behind the Docker Compose CLI), and **Compose v1**.
- **A real provider in the container**: one first backup (about 100,000 messages) ran on the maintainer's NAS and
  stayed near 360 MB; nothing from it was recorded here, and larger mailboxes were not tried.
- **A long-running `run`**: tested for minutes, with intervals of seconds, against the dovecot test server. Days of
  uptime, a real provider dropping connections, and a restart policy over weeks were not.
- **ntfy.sh itself** (and any other notification service): delivery was tested against a local listener only.
- **`-t` or `tty: true`** giving the live bar inside a container (the bar was checked in a pseudo-terminal on the host).
- **arm64 hardware**: the image is built for both architectures; it was run here only on the host's.
- **NFS/SMB `flock`**: network filesystems remain unsupported.
