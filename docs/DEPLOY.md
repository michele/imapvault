# Running imapvault in Docker (NAS, home server)

This guide runs imapvault from the container image with the compose file in [`deploy/`](../deploy), on a NAS
(Synology, QNAP, Unraid, TrueNAS), a Raspberry Pi or any Linux box with Docker or Podman. Output quoted below
was produced by the image against the dovecot test server unless a section says otherwise; see
[Not verified](#not-verified) for what was not tried.

Files: [`Dockerfile`](../Dockerfile), [`deploy/docker-compose.yml`](../deploy/docker-compose.yml),
[`deploy/docker-compose.build.yml`](../deploy/docker-compose.build.yml),
[`deploy/imapvault.example.toml`](../deploy/imapvault.example.toml), [`deploy/.env.example`](../deploy/.env.example).

## How it fits together

- **imapvault is not a daemon.** `backup` makes one snapshot and exits. Nothing happens until something starts
  it: you, or a scheduler on the host ([Scheduling it](#scheduling-it)). `docker compose up` runs one backup and
  stops; it does not keep the container running.
- **One compose project per mailbox account** (or one block of services per account, see
  [Several accounts](#several-accounts)). Each has its own config file, its own repository folder and its own
  password secret.
- **One service per command**, with the command fixed in `entrypoint` so that what you type after the service
  name is *added* to it:

  | Run | Does |
  |---|---|
  | `docker compose run --rm init` | creates the repository (once) |
  | `docker compose run --rm backup` | one backup; `up` does the same |
  | `docker compose run --rm forget` | applies the retention policy to snapshots |
  | `docker compose run --rm prune` | removes data nothing keeps any more; look first with `prune --dry-run` |
  | `docker compose run --rm check` | `check --read-data`: reads every object back and verifies its hash |
  | `docker compose run --rm imapvault --account NAME snapshots` | any other command; you type all of it |

  So `docker compose run --rm backup --full` and `docker compose run --rm prune --dry-run` work as you would
  expect. Only `backup` is started by `docker compose up`; the others are in the `tools` profile and start when
  you name them.
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
   docker compose -f docker-compose.yml -f docker-compose.build.yml build backup   # tags imapvault:local
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
programs to run. Act on the exit code instead ([Exit codes](#exit-codes)). The rest of the options are in the
README and `examples/`.

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
docker compose run --rm init
docker compose run --rm backup
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
with `docker compose run --rm backup --progress log`, or silence them with `--progress none`. A folder with
nothing new prints nothing, so folder numbers skip. There is no account-wide total: the bar covers one folder at
a time.

A second backup right after reads nothing and prints only the summary:

```
Snapshot 2: 9725 messages in 9 folders
  messages  +0 new, -0 deleted, ~0 flag changes
  storage   0 blob(s) written (0 B), 0 already stored
  read      0 body(ies) downloaded; folders: 9 unchanged, 0 incremental, 0 full
```

## Scheduling it

Something on the host has to start the container. Hourly `backup`, and weekly `forget` then `prune`, is a
reasonable start (see [SCHEDULING.md](SCHEDULING.md) for the reasoning). A folder the server reports as unchanged
costs one `EXAMINE`, so an hourly run with no new mail is cheap.

**From a scheduler, run `docker compose run --rm -T <service>`.** `-T` says there is no terminal. Docker Compose
v2 notices that by itself (a run with no terminal worked without it); compose v1 stops with "the input device is
not a TTY" without `-T`, and it does no harm with v2.

Cron on a Linux host:

```cron
17 * * * *  cd /srv/imapvault && docker compose run --rm -T backup >>/var/log/imapvault.log 2>&1
23 4 * * 0  cd /srv/imapvault && docker compose run --rm -T forget >>/var/log/imapvault.log 2>&1
33 4 * * 0  cd /srv/imapvault && docker compose run --rm -T prune  >>/var/log/imapvault.log 2>&1
```

**Synology:** Control Panel > Task Scheduler > Create > Scheduled Task > User-defined script, run as `root`, with
the same `cd ... && docker compose run --rm -T backup` line (use the full path `/usr/local/bin/docker`
if the task cannot find it). The task can email you when a run ends with a non-zero code.

A slow run does not get doubled: the repository lock refuses a second one at once, and that run exits `1`:

```
error: repository is locked: exclusive lock refused (held by pid 1 on 0cccbd6bca84 since 2026-11-24T13:13:50Z)
```

While the first backup is still going, the hourly runs fail this way. That is expected, not damage.

### Exit codes

The container's exit code is imapvault's, and `docker compose run` passes it on.

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
docker compose run --rm -T backup
rc=$?
[ "$rc" -eq 3 ] && echo "imapvault: GUARD ALERT, check the mailbox before the next prune" >&2
exit "$rc"
```

The alert text is sealed in an encrypted repository, so reading it (`alerts`) needs the private key, which this
host does not have. Until you `ack` it, `prune` keeps what the alert pinned.

**systemd** instead of cron: a `Type=oneshot` service with `WorkingDirectory=/srv/imapvault` and
`ExecStart=/usr/bin/docker compose run --rm -T backup`, started by a timer with `OnCalendar=hourly` and
`Persistent=true`, behaves the same, and a non-zero exit shows as a failed unit. (Written from the systemd
documentation, not run.)

## Reading and restoring

`ls`, `show`, `diff` with names, `alerts`, `restore` and `check --decrypt` need the private key. Do not put it on
this host. Run them from a machine you trust, with the repository mounted or copied and `identity_file` set in
that machine's config. `restore` also needs `[restore] allow_append = true` in the target account's config, and it
only ever appends (see the README). `snapshots`, `check` and `check --read-data` do not need the key.

## Day-2

### Logs

`docker compose logs --tail 50 backup` shows the last run of that service (plain progress lines and the summary).
Logs rotate: 3 files of 10 MB.

### Updating

```sh
# 1. copy the repository first (below): a newer imapvault may migrate it, and an older one then refuses to open it
docker compose pull
docker compose run --rm backup          # the next run uses the new image
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
it already downloaded and records a partial snapshot (as Ctrl-C does), and the next run continues from there.
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
  backup-work:
    <<: *imapvault
    entrypoint: ["/imapvault", "--account", "work", "backup"]
    secrets:
      - source: work_imap_password
        target: imap_password

secrets:
  work_imap_password:
    file: ./secrets/work_imap_password
```

with `path = "/repo/work"` in `config/work.toml`, and the same for `init`, `forget` and `prune` if you want them
by name (otherwise `docker compose run --rm imapvault --account work init`). Two accounts never share a
repository: each one is bound to its host and username at `init`.

## NAS notes

- **Paths:** use a folder on a local volume, e.g. Synology `/volume1/docker/imapvault`, QNAP
  `/share/Container/imapvault`, Unraid `/mnt/user/appdata/imapvault`. Never a mounted NFS/SMB share: the
  repository lock uses `flock(2)`, which a network filesystem may not honour.
- **PUID/PGID:** SSH in as the user who owns the folder and run `id`. On Synology the first user is often
  `uid=1026`, group `users` = `100`; check, do not assume. Put the values in `.env` and `chown` `repo/` and
  `secrets/imap_password` to them.
- **Synology Container Manager:** the project's start button would run `backup` once, which is fine after `init`,
  but run `init` over SSH first (`sudo docker compose run --rm init` in the folder). Tick "Start the project once
  it is created" only after that. A project's containers show as "exited" afterwards; that is normal for a
  one-shot tool.
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

- **`no config for account "NAME": expected /config/imapvault/NAME.toml ...`**: the file is not where the
  container looks. On the host it is `config/NAME.toml` next to the compose file; `IMAPVAULT_ACCOUNT` must be `NAME`.
- **`required variable IMAPVAULT_ACCOUNT is missing a value`**: no `.env`, or it does not set the account.
- **`... open /run/secrets/imap_password: permission denied`**: the [uid pitfall](#1-compose-secrets--password_file-what-the-compose-file-does).
- **`create /repo/.../objects: permission denied`**: `repo/` is not writable by `PUID:PGID`.
- **`repository is locked: exclusive lock refused`**: another run is in progress (a slow first backup, or a
  `check --read-data`). It ends by itself; the next scheduled run retries.
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
- **`-t` or `tty: true`** giving the live bar inside a container (the bar was checked in a pseudo-terminal on the host).
- **arm64 hardware**: the image is built for both architectures; it was run here only on the host's.
- **NFS/SMB `flock`**: network filesystems remain unsupported.
