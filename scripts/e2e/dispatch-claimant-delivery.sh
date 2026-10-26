#!/usr/bin/env bash
# The live proof that Dispatch's outbox publishes a person's answer, comment or untargeted message
# on an issue a session has claimed to that session's own topic, `notifications.agent.<session>`
# (publishClaimantRoute, packages/envoy/internal/dispatch/outbox/publisher.go). It runs the real
# envoy-dispatch binary against a PostgreSQL the caller names, a NATS server of the run's own
# (internal/testnats/cmd/natsd) and the e2e harness's fake Envoy listener
# (packages/dispatch/e2e/fake-envoy.ts), drives it over HTTP as three sessions and one person, and
# reads which `notifications.agent.*` topics carried each event's envelope, `dispatch-<event id>`
# (dispatch-claimant-delivery.ts beside this script). Each case prints `== <case>`, what it read,
# and `ok <case>` or `FAIL <case>`; every case runs, and any FAIL exits 1.
#
#   DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     [DISPATCH_BIN=<envoy-dispatch>] bash scripts/e2e/dispatch-claimant-delivery.sh
#
# DATABASE_URL is required and must name a loopback host: the run drops and recreates its `public`
# schema before the server migrates it, and leaves what the run wrote there for inspection.
# DISPATCH_BIN names a prebuilt envoy-dispatch (the negative control runs one built from the base
# commit); unset, the script builds ./cmd/dispatch from this checkout. The ports are
# CLAIMANT_E2E_DISPATCH_PORT (19051) and CLAIMANT_E2E_ENVOY_PORT (19053), each refused when
# something already answers on it; NATS listens on a port the kernel picks. The work directory, under
# TMPDIR, holds the binaries and every process's log: removed when the run passes, kept and printed
# when it fails.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
started=$SECONDS
check=setup

begin() { check=$1; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }
# What lib/rig.sh reads of its caller: start_process and await_start log under $evidence/logs, the
# work directory's once the build makes it.
evidence=''
timeout_hook=''
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"

# DISPATCH_BIN is this script's input, read before the sweep below drops every DISPATCH_* variable.
dispatch_bin=${DISPATCH_BIN:-}
database_url=${DATABASE_URL:-}
dispatch_port=${CLAIMANT_E2E_DISPATCH_PORT:-19051}
envoy_port=${CLAIMANT_E2E_ENVOY_PORT:-19053}

# A Legion pane exports the real listener (ENVOY_URL), the real Dispatch (DISPATCH_URL,
# DISPATCH_TOKEN_FILE) and an nkey for the real NATS; nothing this run starts inherits any of it.
# psql reads PGHOSTADDR, which would replace the URL's host for the reset below while the server,
# which never reads it, connects to the host the check admitted.
mapfile -t inherited < <(compgen -e)
for name in "${inherited[@]}"; do
  case "$name" in
    DISPATCH_* | ENVOY_* | NATS_* | PGHOSTADDR) unset "$name" ;;
  esac
done

# The work directory is made once the prerequisites hold, so a refused run leaves nothing behind.
work=''
nats_pid=''
fake_envoy_pid=''
dispatch_pid=''

# cleanup stops what the run started and exits with rc: the script's own status for EXIT, 128 plus
# the signal's number for INT and TERM, so an interrupted run never reports success.
cleanup() {
  local rc=$1
  trap - EXIT INT TERM
  set +e
  stop_pid "$dispatch_pid"
  stop_pid "$fake_envoy_pid"
  stop_pid "$nats_pid"
  if [ -z "$work" ]; then
    echo "dispatch claimant delivery e2e: FAIL at $check after $((SECONDS - started))s" >&2
  elif [ "$rc" -eq 0 ]; then
    rm -rf "$work"
    echo "dispatch claimant delivery e2e: PASS in $((SECONDS - started))s"
  else
    rm -rf "${work:?}/bin" "${work:?}/nats-store"
    echo "dispatch claimant delivery e2e: FAIL at $check after $((SECONDS - started))s (logs: $work)" >&2
  fi
  exit "$rc"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

# port_answers PORT succeeds when something accepts a connection on 127.0.0.1:PORT.
port_answers() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

begin prerequisites
[ -n "$database_url" ] ||
  fail "DATABASE_URL is required: it names a loopback PostgreSQL database this run resets (postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable)"
# The host of postgres://[user[:password]@]host[:port]/database[?params]: the run drops the
# database's public schema, so it must be this machine's, as the dev sign-in the server runs with
# also insists. A host given by a parameter, or several hosts, are refused rather than parsed.
case "$database_url" in
  postgres://* | postgresql://*) ;;
  *) fail "DATABASE_URL must be a postgres:// or postgresql:// URL" ;;
esac
authority=${database_url#*://}
authority=${authority%%[/?]*}
hostport=${authority##*@}
if [[ "$hostport" == \[* ]]; then
  db_host=${hostport#\[}
  db_host=${db_host%%\]*}
else
  db_host=${hostport%%:*}
fi
case "$db_host" in
  127.0.0.1 | localhost | ::1) ;;
  *) fail "DATABASE_URL must name 127.0.0.1, localhost or ::1, not '$db_host': this run resets the database" ;;
esac
[[ "$database_url" != *[?\&]host=* && "$database_url" != *[?\&]hostaddr=* ]] ||
  fail "DATABASE_URL must not name its host in a parameter"
for tool in go bun curl psql openssl; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
if [ -n "$dispatch_bin" ]; then
  [ -x "$dispatch_bin" ] || fail "DISPATCH_BIN=$dispatch_bin is not an executable file"
fi
for port in "$dispatch_port" "$envoy_port"; do
  if ! [[ "$port" =~ ^[1-9][0-9]{0,4}$ ]] || [ "$port" -gt 65535 ]; then
    fail "'$port' is not a port"
  fi
  port_answers "$port" && fail "something already answers on 127.0.0.1:$port"
done
[ "$dispatch_port" != "$envoy_port" ] || fail "the Dispatch and fake Envoy ports are both $dispatch_port"
note "database host $db_host (URL not printed)"
pass

begin build
work=$(mktemp -d "${TMPDIR:-/tmp}/dispatch-claimant-delivery.XXXXXXXX")
evidence=$work
note "work directory $work"
mkdir -p "$work/bin" "$work/web-dist" "$work/logs"
if [ -z "$dispatch_bin" ]; then
  dispatch_bin=$work/bin/envoy-dispatch
  go -C "$root/packages/envoy" build -o "$dispatch_bin" ./cmd/dispatch || fail "./cmd/dispatch did not build"
  note "built envoy-dispatch from $root/packages/envoy"
else
  note "envoy-dispatch is DISPATCH_BIN=$dispatch_bin"
fi
go -C "$root/packages/envoy" build -o "$work/bin/natsd" ./internal/testnats/cmd/natsd ||
  fail "./internal/testnats/cmd/natsd did not build"
pass

begin reset-database
# Empty, migrations included: the server migrates the new schema at boot, so a base binary and this
# checkout's each start from a database of their own migrations.
psql "$database_url" -X -q -v ON_ERROR_STOP=1 -c 'set client_min_messages = warning' \
  -c 'drop schema if exists public cascade' -c 'create schema public' >/dev/null ||
  fail "could not reset the database's public schema"
pass

begin nats
# The port is the kernel's pick (`-p -1`), read back from the line the server logs once it listens.
start_process nats "$work/bin/natsd" -p -1 -store_dir "$work/nats-store"
await_start nats "$nats_pid" 0 60 "natsd to be ready" grep -q 'Server is ready' "$work/logs/nats.log" ||
  fail "natsd could not bind its port; see $work/logs/nats.log"
nats_port=$(sed -n 's/.*Listening for client connections on .*:\([0-9][0-9]*\)$/\1/p' "$work/logs/nats.log" | sed -n 1p)
[ -n "$nats_port" ] || fail "natsd logged no client port; see $work/logs/nats.log"
nats_url=nats://127.0.0.1:$nats_port
note "JetStream on $nats_url"
pass

begin fake-envoy
FAKE_ENVOY_PORT=$envoy_port start_process fake_envoy bun "$root/packages/dispatch/e2e/fake-envoy.ts"
await_start fake_envoy "$fake_envoy_pid" 0 60 "the fake Envoy listener to answer /healthz" \
  curl -fsS --max-time 2 "http://127.0.0.1:$envoy_port/healthz" ||
  fail "another process bound 127.0.0.1:$envoy_port first; see $work/logs/fake_envoy.log"
note "fake Envoy listener on http://127.0.0.1:$envoy_port"
pass

begin dispatch
# The server gets this environment and nothing else. Identity is the production cookie, minted at
# the dev sign-in route (DISPATCH_DEV_SIGNIN=1) with a signing key the process generates for itself;
# HOME and the XDG directories name nothing, so no envoy.json, data directory or AWS profile of the
# caller's is read. Sessions authenticate with a token minted for this run.
agent_token=$(openssl rand -hex 24)
dispatch_url=http://127.0.0.1:$dispatch_port
start_process dispatch env -i \
  PATH="$PATH" \
  TMPDIR="$work" \
  HOME=/nonexistent \
  XDG_CACHE_HOME=/nonexistent \
  XDG_CONFIG_HOME=/nonexistent \
  XDG_DATA_HOME=/nonexistent \
  DATABASE_URL="$database_url" \
  DISPATCH_AGENT_TOKEN="$agent_token" \
  DISPATCH_DEV_SIGNIN=1 \
  DISPATCH_IDENTITY=cookie \
  DISPATCH_INSECURE_COOKIE=1 \
  DISPATCH_LISTEN_HOST=127.0.0.1 \
  DISPATCH_PORT="$dispatch_port" \
  DISPATCH_SERVER_URL="$dispatch_url" \
  DISPATCH_WEB_DIST="$work/web-dist" \
  ENVOY_URL="http://127.0.0.1:$envoy_port" \
  NATS_URLS="$nats_url" \
  "$dispatch_bin"
await_start dispatch "$dispatch_pid" 0 60 "envoy-dispatch to answer /healthz" \
  curl -fsS --max-time 2 "$dispatch_url/healthz" ||
  fail "another process bound 127.0.0.1:$dispatch_port first; see $work/logs/dispatch.log"
note "envoy-dispatch on $dispatch_url, NATS on, outbox publishing to $nats_url"
pass

check=cases
status=0
CLAIMANT_E2E_AGENT_TOKEN=$agent_token \
  bun "$root/scripts/e2e/dispatch-claimant-delivery.ts" "$dispatch_url" "$nats_url" "http://127.0.0.1:$envoy_port" ||
  status=$?
kill -0 "$dispatch_pid" 2>/dev/null || fail "envoy-dispatch exited during the run; see $work/logs/dispatch.log"
case "$status" in
  0) ;;
  1) fail "a case failed (above)" ;;
  *) fail "the driver stopped before its cases (status $status)" ;;
esac
