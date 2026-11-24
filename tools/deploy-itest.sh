#!/bin/sh
# Runs deploy/docker-compose.yml, as a user would, against the dovecot test server (make deploy-itest).
# Needs the server up and seeded (the Makefile does that) and the image imapvault:local (make docker-build).
# Everything happens in a throwaway project folder; nothing under deploy/ is touched.
set -eu

COMPOSE=${COMPOSE:-docker compose}
SRC=$(cd "$(dirname "$0")/../deploy" && pwd)
T=$(mktemp -d)
dc() { (cd "$T" && $COMPOSE "$@"); }
cleanup() { dc down -v >/dev/null 2>&1 || true; rm -rf "$T"; }
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; echo "--- last output:" >&2; echo "$out" >&2; exit 1; }
# run CMD...: sets $out and $rc, never aborts.
run() { out=$("$@" 2>&1) && rc=0 || rc=$?; }
expect() { # expect RC PATTERN DESCRIPTION; RC "nonzero" accepts any failure (a compose wrapper may change the code)
	if [ "$1" = nonzero ]; then
		[ "$rc" -ne 0 ] || fail "$3: exit 0, want a failure"
	else
		[ "$rc" -eq "$1" ] || fail "$3: exit $rc, want $1"
	fi
	[ -z "$2" ] || echo "$out" | grep -q -- "$2" || fail "$3: output lacks '$2'"
	echo "ok   $3"
}

cp "$SRC/docker-compose.yml" "$SRC/docker-compose.test.yml" "$T/"
mkdir -p "$T/config" "$T/repo" "$T/secrets"
chmod 700 "$T/repo"
cp "$SRC/.env.example" "$T/.env"
{
	echo "IMAPVAULT_ACCOUNT=it"
	echo "PUID=$(id -u)"
	echo "PGID=$(id -g)"
	echo "IMAPVAULT_IMAGE=imapvault:local"
	echo "COMPOSE_FILE=docker-compose.yml:docker-compose.test.yml"
} >>"$T/.env" # later lines win over the ones copied from .env.example
cat >"$T/config/it.toml" <<'TOML'
[imap]
host = "127.0.0.1"
port = 31143
tls = "none"
username = "me@imapvault.test"
password_file = "/run/secrets/imap_password"

[repository]
path = "/repo/it"
TOML
printf 'test\n' >"$T/secrets/imap_password"
chmod 600 "$T/secrets/imap_password"

run dc config -q;                                   expect 0 "" "compose config validates"
run dc run --rm -T init;                            expect 0 "format " "init creates the repository"
run dc up --exit-code-from backup;                  expect 0 "Snapshot 1:" "up runs one backup and exits 0"
run dc up --exit-code-from backup;                  expect 0 "0 body(ies) downloaded" "a second backup reads nothing"
run dc run --rm -T backup --progress log;           expect 0 "Snapshot 3:" "extra arguments are appended to backup"
run dc run --rm -T prune --dry-run;                 expect 0 "" "prune --dry-run"
run dc run --rm -T check;                           expect 0 "" "check --read-data"
run dc run --rm -T imapvault --account it snapshots; expect 0 "" "free-form command"
echo "$out" | grep -q "^1 " || fail "snapshots does not list snapshot 1"

# What a user sees when something is wrong: the exit code must reach the caller.
printf 'wrong\n' >"$T/secrets/imap_password"
run dc run --rm -T backup;                          expect 1 "" "a wrong password exits 1"
printf 'test\n' >"$T/secrets/imap_password"

mv "$T/config/it.toml" "$T/config/elsewhere.toml"
run dc run --rm -T backup;                          expect 1 "/config/imapvault/it.toml" "a misplaced config says where to put it"
echo "$out" | grep -q "mount" || fail "the container hint is missing"
mv "$T/config/elsewhere.toml" "$T/config/it.toml"

mv "$T/.env" "$T/.env.saved"
echo "COMPOSE_FILE=docker-compose.yml:docker-compose.test.yml" >"$T/.env"
run dc config -q;                                   expect nonzero "set IMAPVAULT_ACCOUNT in .env" "a missing account name is refused with a hint"
mv "$T/.env.saved" "$T/.env" # the cleanup needs the real one to tear the project down
echo "all deploy checks passed"
