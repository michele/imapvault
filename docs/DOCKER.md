# Running imapvault in Docker (NAS, home server)

imapvault is a one-shot command, so the image has no daemon: you start a container per run
(`backup`, `forget`, `prune`, `check`...) and it exits. A host timer or cron job does the scheduling
(see [SCHEDULING.md](SCHEDULING.md) for exit codes).

Image: `ghcr.io/michele/imapvault` (tags `latest`, `X.Y.Z`, `sha-<short>`; linux/amd64 and linux/arm64).
It is one static binary on distroless, running as uid/gid **65532**. Nothing is baked in: no config, no
password, no key, no repository. The image sets `XDG_CONFIG_HOME=/config`, so `--account NAME` reads
`/config/imapvault/NAME.toml`.

## Layout on the host

```
/srv/imapvault/
  config/imapvault/fastmail.toml     mounted read-only at /config/imapvault
  config/imapvault/fastmail.password the IMAP app password, one line
  repo/                              mounted at /repo, must be writable by uid 65532
```

```sh
install -d -o 65532 -g 65532 -m 0700 /srv/imapvault/repo
```

```toml
# /srv/imapvault/config/imapvault/fastmail.toml
[imap]
host = "imap.fastmail.com"
username = "you@example.com"
password_file = "/config/imapvault/fastmail.password"

[repository]
path = "/repo/fastmail"

[encryption]
recipients = ["age1..."]      # public keys only: this container never needs the private key
```

Keep the password file at mode `0600` on a host with other users. imapvault warns when a password file is
readable by others; in a container with a single-purpose mount this is only a warning.

## Run

```sh
docker run --rm \
  -v /srv/imapvault/config:/config:ro -v /srv/imapvault/repo:/repo \
  ghcr.io/michele/imapvault:latest --account fastmail init

# the first backup can take hours; run it by hand once
docker run --rm -v /srv/imapvault/config:/config:ro -v /srv/imapvault/repo:/repo \
  ghcr.io/michele/imapvault:latest --account fastmail backup
```

The container's exit code is imapvault's (`0` ok, `1` error, `2` partial, `3` guard alert).
Schedule it from the host:

```cron
17 * * * *  docker run --rm -v /srv/imapvault/config:/config:ro -v /srv/imapvault/repo:/repo ghcr.io/michele/imapvault:latest --account fastmail backup
23 4 * * 0  docker run --rm -v /srv/imapvault/config:/config:ro -v /srv/imapvault/repo:/repo ghcr.io/michele/imapvault:latest --account fastmail forget
33 4 * * 0  docker run --rm -v /srv/imapvault/config:/config:ro -v /srv/imapvault/repo:/repo ghcr.io/michele/imapvault:latest --account fastmail prune
```

An overlapping run is refused by the repository lock, so a slow first backup is not doubled.

Compose, if you prefer (`docker compose run --rm imapvault --account fastmail backup`):

```yaml
services:
  imapvault:
    image: ghcr.io/michele/imapvault:latest
    volumes:
      - /srv/imapvault/config:/config:ro
      - /srv/imapvault/repo:/repo
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
```

## Reading and restoring mail

`ls`, `show`, `diff` (with names), `restore` and `check --decrypt` need the private key. Do not put it on
the backup host's scheduled container. Run those from a machine you trust, with the repository mounted (or
copied) and `identity_file` set in that machine's config. `restore` also needs `[restore] allow_append = true`
in the target account's config (see the README).

## NAS notes

- Permissions: uid 65532 must own the repository folder. On Synology and similar, create a share folder
  and set it with `chown 65532:65532` over SSH, or run the container with `--user $(id -u):$(id -g)`.
- The repository lock uses `flock(2)`. Local disks and bind mounts of local disks are fine. A network
  filesystem (NFS, SMB) mounted into the container may not honour it; keep the repository on the NAS's own
  volume and mount that, not a mount of a mount. This is untested.
- Back up the repository folder too (restic, rsync, a snapshot). imapvault protects mail from the server;
  it does not protect the repository from the disk.

## Build it yourself

```sh
make docker-build                       # imapvault:local for this machine
make docker-buildx PLATFORMS=linux/amd64,linux/arm64
```

## What was verified

`make docker-build` and a run of `init`, `backup` (twice: the second one read nothing), `snapshots` and
`check --read-data` from the image against the dovecot test server. The pinned base images are the ones
quietbox uses. The NAS and NFS notes are not verified on real hardware.
