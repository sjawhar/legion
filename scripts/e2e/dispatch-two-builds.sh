#!/usr/bin/env bash
# The live proof that text an older build of Dispatch writes gets this build's reference grammar
# with no edit and no command run: a bare issue key, which a build that predates this release
# indexes as nothing at all. Two real `envoy-dispatch` binaries run against one PostgreSQL — the
# one built from the commit `--base-commit` names, and the one built from this checkout — the way
# a rolling release serves both at once, and what the older build writes and edits while the newer
# one runs shows in its target's "Referenced by" once the older build has stopped.
#
#   DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/dispatch-two-builds.sh --base-commit <rev>
#
# DATABASE_URL is required and must name a loopback host: the run drops and recreates its `public`
# schema, so the base build migrates it from empty and this build migrates it forward, as a
# deployment's upgrade does. --base-commit is required and names the commit the base build is
# built from, which is the merge base with the default branch for a change under review; its tree
# is taken with `git archive` into the run's own work directory, so the checkout is never touched.
# The ports are TWO_BUILDS_BASE_PORT (19071), TWO_BUILDS_HEAD_PORT (19072) and
# TWO_BUILDS_ENVOY_PORT (19073), each refused when something already answers on it. The work
# directory, under TMPDIR, holds the base tree, both binaries and every process's log: removed
# when the run passes, kept and printed when it fails.
#
# The three cases, in the order the run makes them:
#
#   base-alone     the base build alone writes a comment citing an issue by its bare key, and
#                  that issue's "Referenced by" stays empty for the whole bounded wait: the base
#                  build's own grammar reads no bare key, which is this proof's negative control.
#   overlap        this build starts beside it and migrates the schema forward; the base build,
#                  still serving, writes a second such comment and edits the first. Neither is
#                  referenced by anything the base build indexed.
#   drained        the base build stops, and with no command run this build's own reconciliation
#                  drains the queue those writes enqueued: both comments show in the target's
#                  "Referenced by".
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
started=$SECONDS
check=setup

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}
# What lib/rig.sh reads of its caller: start_process and await_start log under $evidence/logs.
evidence=''
timeout_hook=''
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"

# This script's own inputs, read before the sweep below drops every DISPATCH_* variable.
base_commit=''
while [ $# -gt 0 ]; do
  case "$1" in
    --base-commit)
      [ $# -ge 2 ] || fail "--base-commit takes the commit the base build is built from"
      base_commit=$2
      shift 2
      ;;
    --base-commit=*)
      base_commit=${1#--base-commit=}
      shift
      ;;
    *) fail "unknown argument '$1': this script takes --base-commit <rev> alone" ;;
  esac
done
database_url=${DATABASE_URL:-}
base_port=${TWO_BUILDS_BASE_PORT:-19071}
head_port=${TWO_BUILDS_HEAD_PORT:-19072}
envoy_port=${TWO_BUILDS_ENVOY_PORT:-19073}

# A Legion pane exports the real listener (ENVOY_URL), the real Dispatch (DISPATCH_URL,
# DISPATCH_TOKEN_FILE) and an nkey for the real NATS; nothing this run starts inherits any of it.
# psql reads PGHOSTADDR, which would replace the URL's host for the reset below while a server,
# which never reads it, connects to the host the check admitted.
mapfile -t inherited < <(compgen -e)
for name in "${inherited[@]}"; do
  case "$name" in
    DISPATCH_* | ENVOY_* | NATS_* | PGHOSTADDR) unset "$name" ;;
  esac
done

# The work directory is made once the prerequisites hold, so a refused run leaves nothing behind.
work=''
fake_envoy_pid=''
base_pid=''
head_pid=''

# cleanup stops what the run started and exits with rc: the script's own status for EXIT, 128 plus
# the signal's number for INT and TERM, so an interrupted run never reports success.
cleanup() {
  local rc=$1
  trap - EXIT INT TERM
  set +e
  stop_pid "$head_pid"
  stop_pid "$base_pid"
  stop_pid "$fake_envoy_pid"
  if [ -z "$work" ]; then
    echo "dispatch two builds e2e: FAIL at $check after $((SECONDS - started))s" >&2
  elif [ "$rc" -eq 0 ]; then
    rm -rf "$work"
    echo "dispatch two builds e2e: PASS in $((SECONDS - started))s"
  else
    rm -rf "${work:?}/bin" "${work:?}/base"
    echo "dispatch two builds e2e: FAIL at $check after $((SECONDS - started))s (logs: $work)" >&2
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

agent_token=''

# call METHOD PATH [BODY] prints what the server on $target_url answers an agent bearer, and
# returns non-zero with the answer on stderr when it is not 2xx, so a caller inside a bounded
# wait polls again and a caller outside one fails the check. The body of an agent's write
# carries its actor.
target_url=''
call() {
  local method=$1 path=$2 body=${3:-} out status
  out=$(mktemp "$work/call.XXXXXX")
  if [ -z "$body" ]; then
    status=$(curl -sS -o "$out" -w '%{http_code}' --max-time 20 -X "$method" \
      -H "Authorization: Bearer $agent_token" "$target_url$path") || status=000
  else
    status=$(curl -sS -o "$out" -w '%{http_code}' --max-time 20 -X "$method" \
      -H "Authorization: Bearer $agent_token" -H 'Content-Type: application/json' \
      -d "$body" "$target_url$path") || status=000
  fi
  case "$status" in
    2??)
      cat "$out"
      rm -f "$out"
      ;;
    *)
      echo "$method $path answered $status: $(cat "$out")" >&2
      rm -f "$out"
      return 1
      ;;
  esac
}

# json_field FIELD reads one string member of a JSON object on stdin. bun is already required by
# the harness this run borrows its fake Envoy from.
json_field() {
  # shellcheck disable=SC2016 # the JavaScript below is bun's, not this shell's
  bun -e 'const field = process.argv[1]; let data = ""; process.stdin.on("data", (chunk) => { data += chunk; }); process.stdin.on("end", () => { const value = JSON.parse(data)[field]; if (typeof value !== "string" || value === "") { console.error(`no ${field} in ${data}`); process.exit(1); } console.log(value); });' "$1"
}

# references_hold TARGET SOURCE... reads the target's "Referenced by" (GET
# /api/v1/references?to=TARGET, whose edges are the incoming ones) and answers 0 when exactly
# the references SOURCE names point at it, in any order, 1 when others do, and 2 when the read
# itself failed — which a bounded wait takes for "not yet" and a direct caller for a failure.
references_hold() {
  local target=$1
  shift
  local wanted found answer
  wanted=$(printf '%s\n' "$@" | sort)
  answer=$(call GET "/api/v1/references?to=$target") || return 2
  found=$(printf '%s' "$answer" |
    bun -e 'let data = ""; process.stdin.on("data", (chunk) => { data += chunk; }); process.stdin.on("end", () => { for (const edge of JSON.parse(data).edges ?? []) console.log(edge.node?.ref ?? ""); });' |
    sort) || return 2
  [ "$found" = "$wanted" ]
}

begin prerequisites
[ -n "$database_url" ] ||
  fail "DATABASE_URL is required: it names a loopback PostgreSQL database this run resets (postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable)"
[ -n "$base_commit" ] ||
  fail "--base-commit is required: it names the commit the base build is built from (the merge base with the default branch)"
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
for tool in go bun curl psql git openssl; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
  fail "--base-commit '$base_commit' names no commit of this repository"
for port in "$base_port" "$head_port" "$envoy_port"; do
  if ! [[ "$port" =~ ^[1-9][0-9]{0,4}$ ]] || [ "$port" -gt 65535 ]; then
    fail "'$port' is not a port"
  fi
  port_answers "$port" && fail "something already answers on 127.0.0.1:$port"
done
if [ "$base_port" = "$head_port" ] || [ "$base_port" = "$envoy_port" ] || [ "$head_port" = "$envoy_port" ]; then
  fail "the base, head and fake Envoy ports must differ ($base_port, $head_port, $envoy_port)"
fi
note "database host $db_host (URL not printed)"
note "base commit $base_sha"
pass

begin build
work=$(mktemp -d "${TMPDIR:-/tmp}/dispatch-two-builds.XXXXXXXX")
evidence=$work
note "work directory $work"
mkdir -p "$work/bin" "$work/base" "$work/web-dist" "$work/logs"
# The base tree is the commit's own, taken out of the repository rather than checked out, so this
# working copy is never touched.
git -C "$root" archive "$base_sha" | tar -x -C "$work/base" ||
  fail "could not take the tree of $base_sha"
go -C "$work/base/packages/envoy" build -o "$work/bin/dispatch-base" ./cmd/dispatch ||
  fail "the base build's ./cmd/dispatch did not build"
go -C "$root/packages/envoy" build -o "$work/bin/dispatch-head" ./cmd/dispatch ||
  fail "this checkout's ./cmd/dispatch did not build"
note "built the base build from $base_sha and this build from $root"
pass

begin reset-database
# Empty, migrations included: the base build migrates the new schema at boot, and this build
# migrates it forward, which is the upgrade this proof is about.
psql "$database_url" -X -q -v ON_ERROR_STOP=1 -c 'set client_min_messages = warning' \
  -c 'drop schema if exists public cascade' -c 'create schema public' >/dev/null ||
  fail "could not reset the database's public schema"
pass

begin fake-envoy
FAKE_ENVOY_PORT=$envoy_port start_process fake_envoy bun "$root/packages/dispatch/e2e/fake-envoy.ts"
await_start fake_envoy "$fake_envoy_pid" 60 "the fake Envoy listener to answer /healthz" \
  curl -fsS --max-time 2 "http://127.0.0.1:$envoy_port/healthz"
note "fake Envoy listener on http://127.0.0.1:$envoy_port"
pass

agent_token=$(openssl rand -hex 24)
base_url=http://127.0.0.1:$base_port
head_url=http://127.0.0.1:$head_port

# start_server NAME BINARY PORT starts one build with this environment and nothing else: identity
# is the production cookie, minted at the dev sign-in route with a signing key the process
# generates for itself, HOME and the XDG directories name nothing, and both processes share one
# agent token, so a session's bearer is good against either.
start_server() {
  local name=$1 binary=$2 port=$3
  start_process "$name" env -i \
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
    DISPATCH_NATS_DISABLED=1 \
    DISPATCH_PORT="$port" \
    DISPATCH_SERVER_URL="http://127.0.0.1:$port" \
    DISPATCH_WEB_DIST="$work/web-dist" \
    ENVOY_URL="http://127.0.0.1:$envoy_port" \
    "$binary"
}

begin base-build
start_server base "$work/bin/dispatch-base" "$base_port"
await_start base "$base_pid" 120 "the base build to answer /healthz" \
  curl -fsS --max-time 2 "$base_url/healthz"
note "the base build serves $base_url on the schema it migrated"
pass

begin seed
target_url=$base_url
actor='"actor":{"kind":"session","id":"two-builds"}'
call POST /api/v1/projects "{\"key\":\"CORE\",\"name\":\"Core\",$actor}" >/dev/null ||
  fail "the base build refused the project"
target_key=$(call POST /api/v1/issues "{\"project\":\"CORE\",\"title\":\"Warm the cache\",$actor}" | json_field key) ||
  fail "the base build refused the target issue"
host_key=$(call POST /api/v1/issues "{\"project\":\"CORE\",\"title\":\"Reference host\",$actor}" | json_field key) ||
  fail "the base build refused the host issue"
[ "$target_key" = "CORE-1" ] || fail "the first issue is $target_key, not CORE-1"
note "the base build made $target_key and $host_key"
# The citation is a bare issue key, which only this release's grammar reads.
written=$(call POST "/api/v1/issues/$host_key/comments" "{\"body\":\"Blocked on $target_key today.\",$actor}" | json_field id) ||
  fail "the base build refused the comment"
note "the base build wrote the comment $written"
pass

begin base-alone
# The base build's own index holds nothing for a bare key, and nothing else is running to drain
# the queue, so the target's "Referenced by" stays empty for the whole wait. This is the control:
# without this release the citation never lands.
waited=0
while [ "$waited" -lt 45 ]; do
  held=0
  references_hold "dispatch://$target_key" || held=$?
  case "$held" in
    0) ;;
    1) fail "the base build indexed the bare key it cannot read: $target_key has an edge after ${waited}s" ;;
    *) fail "the base build's read of $target_key's references failed after ${waited}s" ;;
  esac
  sleep 5
  waited=$((waited + 5))
done
note "after ${waited}s under the base build alone, $target_key is referenced by nothing"
pass

begin head-build
start_server head "$work/bin/dispatch-head" "$head_port"
await_start head "$head_pid" 120 "this build to answer /healthz" \
  curl -fsS --max-time 2 "$head_url/healthz"
kill -0 "$base_pid" 2>/dev/null || fail "the base build exited when this build migrated the schema"
note "this build serves $head_url on the schema it migrated forward, beside the base build"
pass

begin overlap
# The older build, still serving, writes a second comment and edits the first. Neither write is
# indexed with this release's grammar — the build making them does not have it — and both are
# enqueued by the triggers this build's migration installed.
target_url=$base_url
second=$(call POST "/api/v1/issues/$host_key/comments" "{\"body\":\"Also blocked on $target_key.\",$actor}" | json_field id) ||
  fail "the base build refused the second comment"
call PATCH "/api/v1/comments/$written" "{\"body\":\"Still blocked on $target_key today.\",$actor}" >/dev/null ||
  fail "the base build refused the edit"
kill -0 "$base_pid" 2>/dev/null || fail "the base build exited while it was writing"
note "the base build wrote $second and edited $written while this build was serving"
pass

begin drained
# The older build stops, and nothing else is asked of this one: no rebuild command, no edit.
stop_pid "$base_pid"
base_pid=''
target_url=$head_url
until_true 180 "$target_key to be referenced by both comments the base build wrote" \
  references_hold "dispatch://$target_key" \
  "dispatch://$host_key/comment/$written" "dispatch://$host_key/comment/$second"
note "$target_key is referenced by $written and $second, with no command run"
pass
