#!/usr/bin/env bash
# docs/site/media/broker/rig.sh
#
# The secrets broker's demo rig: every surface the broker's screenshots and walkthrough show,
# running on one machine on example data. It starts
#
#   - the broker (packages/envoy/cmd/broker) on a local rules file and its fake secrets file
#     (internal/broker/secrets' development store), holding one secret, DEMO_API_KEY, whose value
#     is made up, in a database of its own beside DATABASE_URL's, created and dropped by this run;
#   - the Dispatch e2e harness (packages/dispatch/e2e: fake Envoy, fake GitHub, run-server.sh) on
#     DATABASE_URL, emptied, pointed at that broker; its signed-in human is `alice`;
#   - an agent machine whose hostname is example-host-build: agent-secrets-helper (the host side of
#     the broker) for the operator `alice`, alone in a UTS namespace of its own, with the agent's
#     shells on this machine.
#
# It prints how to drive it and stays in the foreground; Ctrl-C (or the exit of the command given
# after --) stops all of it.
#
#   DATABASE_URL=<url> bash docs/site/media/broker/rig.sh                  # interactive
#   DATABASE_URL=<url> bash docs/site/media/broker/rig.sh -- <command...>  # run <command>, then stop
#
# The command runs with BROKER_RIG_DISPATCH_URL, Dispatch's address, and BROKER_RIG_AGENT_EXEC, a
# script that runs its arguments on the agent machine, in its demo directory, with its environment
# (an interactive shell when stdin is a terminal: `agent-exec bash`); flow.ts reads both.
#
# DATABASE_URL, the one input, names a Postgres database on this machine (a loopback host or a unix
# socket directory) that this run may empty, as the e2e harness requires; the rig refuses any other
# before it runs a single psql. The broker's database is created beside it under a name of this
# run's own and dropped on exit. Every port is the run's own: the harness's three are picked
# (scripts/e2e/lib/rig.sh's pick_port), and the broker binds one the kernel assigns.
#
# Needs go, bun, psql, curl, openssl and setsid, and passwordless sudo with unshare and setpriv,
# which give the helper its hostname; nothing else from the machine: no credential, private
# hostname or production service.
set -euo pipefail

# Nothing but DATABASE_URL decides where this run's SQL and seed go: libpq's psql also follows
# PGHOSTADDR (which replaces the URL's host), PGHOST and PGSERVICE, and the e2e harness's seed
# truncates PLAYWRIGHT_DATABASE_URL and calls PLAYWRIGHT_BASE_URL with E2E_AGENT_TOKEN whenever
# PLAYWRIGHT_BASE_URL is set (packages/dispatch/e2e/psql.ts, api.ts), as a deployed run's shell has.
unset PGHOSTADDR PGHOST PGSERVICE PLAYWRIGHT_BASE_URL PLAYWRIGHT_DATABASE_URL E2E_AGENT_TOKEN

# local_database URL succeeds when every host the postgres:// URL names is this machine: a unix
# socket directory, localhost, 127.0.0.0/8 or ::1, the hosts Dispatch's dev sign-in accepts
# (packages/envoy/cmd/dispatch/main.go, loopbackDatabase). The hosts are the authority's, or the
# host query parameter's, which overrides them for libpq and pgx alike; none at all is the default
# socket. Another form, and a hostaddr or service parameter, are refused. libpq percent-decodes a
# query parameter's key as well as its value, so each is decoded before it is matched.
local_database() {
  local url=$1 rest hosts param key host
  local -a params=() list=()
  case "$url" in postgres://* | postgresql://*) ;; *) return 1 ;; esac
  rest=${url#*://}
  rest=${rest%%[/?]*}
  hosts=${rest##*@}
  if [[ $url == *\?* ]]; then
    IFS='&' read -ra params <<<"${url#*\?}"
    for param in "${params[@]}"; do
      key=${param%%=*}
      key=$(printf '%b' "${key//%/\\x}")
      case "$key" in
        host) hosts=${param#*=} ;;
        hostaddr | service) return 1 ;;
      esac
    done
  fi
  hosts=$(printf '%b' "${hosts//%/\\x}")
  IFS=, read -ra list <<<"$hosts"
  for host in "${list[@]}"; do
    [[ $host =~ ^(\[[^]]*\]|[^:]*)(:[0-9]*)?$ ]] || return 1
    host=${BASH_REMATCH[1],,}
    [[ $host == /* || $host == localhost || $host == "[::1]" || $host =~ ^127(\.[0-9]{1,3}){3}$ ]] ||
      return 1
  done
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$SCRIPT_DIR/../../../.." && pwd)"
OPERATOR="alice"
AGENT_HOST="example-host-build"

if [ "${1:-}" = "--" ]; then
  shift
elif [ "$#" -gt 0 ]; then
  echo "rig: unexpected argument $1 (usage: rig.sh [-- <command...>])" >&2
  exit 2
fi
: "${DATABASE_URL:?DATABASE_URL must name a Postgres database this run may empty}"
local_database "$DATABASE_URL" || {
  echo "rig: DATABASE_URL must be a postgres:// URL naming a database on this machine (a loopback host or a unix socket directory)" >&2
  exit 1
}
for tool in go bun psql curl openssl setsid sudo unshare setpriv; do
  command -v "$tool" >/dev/null || { echo "rig: $tool is required on PATH" >&2; exit 1; }
done
sudo -n true 2>/dev/null || { echo "rig: the agent machine's helper needs passwordless sudo" >&2; exit 1; }

# scripts/e2e/lib/rig.sh's bounded waits, port picks and process start and stop: each service
# logs to $work/logs/<name>.log.
work="$(mktemp -d /tmp/legion-docs-broker.XXXXXX)"
evidence=$work
timeout_hook=
note() { printf 'rig: %s\n' "$*" >&2; }
fail() {
  note "$*"
  exit 1
}
# shellcheck source-path=SCRIPTDIR/../../../.. source=scripts/e2e/lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
mkdir -p "$work/logs" "$work/bin" "$work/agent"

# What cleanup stops: the pids start_process sets (<name>_pid) and the helper's own, and the
# broker's database. The ports are pick_port's.
broker_pid='' fake_envoy_pid='' fake_github_pid='' dispatch_pid='' helper_pid=''
broker_database='' admin_url='' dispatch_port='' fake_envoy_port='' fake_github_port=''
cleanup() {
  local status=$? pid
  if [ -z "$helper_pid" ] && [ -s "$work/agent/helper.pid" ]; then
    helper_pid="$(cat "$work/agent/helper.pid")"
  fi
  # The helper runs as this user under the root-owned sudo and unshare that started it, which exit
  # with it. Every other service leads a process group of its own (setsid), signalled whole so its
  # children go too: run-server.sh's `flock … go build`, before it execs the server, is one.
  stop_pid "$helper_pid"
  for pid in "$dispatch_pid" "$fake_github_pid" "$fake_envoy_pid" "$broker_pid"; do
    [ -z "$pid" ] || kill -TERM -- "-$pid" 2>/dev/null || true
    stop_pid "$pid"
  done
  if [ -n "$broker_database" ]; then
    PGOPTIONS="-c client_min_messages=warning" psql "$admin_url" -q \
      -c "drop database if exists ${broker_database} with (force)" >/dev/null 2>&1 || true
  fi
  note "stopped; logs kept in $work/logs"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# --- Binaries: the broker, and the client and host helper the agent machine runs. -------------
note "building the broker, agent-secrets and agent-secrets-helper"
(cd "$root/packages/envoy" && CGO_ENABLED=0 go build -o "$work/bin/" ./cmd/broker ./cmd/agent-secrets ./cmd/agent-secrets-helper)

# --- The broker's database, beside DATABASE_URL's, named for this run (as dev-broker.sh names
# its own), so a database another run or anyone else made is never the one dropped. -------------
url_base="${DATABASE_URL%%\?*}"
url_query="${DATABASE_URL#"$url_base"}"
admin_url="${url_base%/*}/postgres${url_query}"
run_database="docs_broker_$(printf '%s' "${work##*.}" | tr '[:upper:]' '[:lower:]')"
broker_database_url="${url_base%/*}/${run_database}${url_query}"
PGOPTIONS="-c client_min_messages=warning" psql "$admin_url" -v ON_ERROR_STOP=1 -q \
  -c "create database ${run_database}"
broker_database=$run_database

# --- The broker, on example rules and a made-up secret. -------------------------------------------
cat >"$work/agent-secret-rules.yaml" <<EOF
version: 1
secrets:
  DEMO_API_KEY:
    source: example/agent-secrets/DEMO_API_KEY
    owner: ${OPERATOR}
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: host, operator: ${OPERATOR}, decision: approval, approver: operator}
EOF
printf '%s\n' "example/agent-secrets/DEMO_API_KEY=demo-key-not-a-real-secret-7f3a" >"$work/fake-secrets.env"
chmod 600 "$work/fake-secrets.env"
# The broker's UI bearer, which Dispatch sends it, reaches both through a file, never an argv.
(umask 077 && openssl rand -hex 32 >"$work/broker-ui-token")

note "starting the broker"
start_process broker setsid env -i PATH="$PATH" \
  BROKER_DATABASE_URL="$broker_database_url" \
  BROKER_LISTEN_ADDR=127.0.0.1:0 \
  BROKER_PUBLIC_URL=http://127.0.0.1:0 \
  BROKER_UI_TOKEN_FILE="$work/broker-ui-token" \
  BROKER_RULES_FILE="$work/agent-secret-rules.yaml" \
  BROKER_FAKE_SECRETS_FILE="$work/fake-secrets.env" \
  "$work/bin/broker"
await_start broker "$broker_pid" 0 60 "the broker to report its address" \
  grep -q 'broker listening addr=' "$work/logs/broker.log"
broker_url="http://$(sed -n 's/.*broker listening addr=\([^ ]*\).*/\1/p' "$work/logs/broker.log" | head -n1)"
until_true 30 "the broker's health check" curl -sf "$broker_url/healthz"

# --- Dispatch: the e2e harness's three servers on picked ports, the server pointed at the broker. -
if [ ! -f "$root/packages/dispatch/web/dist/index.html" ]; then
  note "building the Dispatch dashboard"
  (cd "$root" && bun install --frozen-lockfile >/dev/null)
  (cd "$root/packages/dispatch" && bun run build:web >/dev/null)
fi
pick_port dispatch_port
pick_port fake_envoy_port
pick_port fake_github_port
export DISPATCH_E2E_PORT=$dispatch_port FAKE_ENVOY_PORT=$fake_envoy_port FAKE_GITHUB_PORT=$fake_github_port
dispatch_url="http://127.0.0.1:${dispatch_port}"
note "starting Dispatch at $dispatch_url"
start_process fake_envoy setsid bun "$root/packages/dispatch/e2e/fake-envoy.ts"
start_process fake_github setsid bun "$root/packages/dispatch/e2e/fake-github.ts"
DISPATCH_E2E_AGENT_SECRETS_URL="$broker_url" DISPATCH_E2E_AGENT_SECRETS_TOKEN_FILE="$work/broker-ui-token" \
  start_process dispatch setsid bash "$root/packages/dispatch/e2e/run-server.sh"
await_start fake_envoy "$fake_envoy_pid" 0 60 "the fake Envoy" curl -s -o /dev/null "http://127.0.0.1:$fake_envoy_port/"
await_start fake_github "$fake_github_pid" 0 60 "the fake GitHub" curl -s -o /dev/null "http://127.0.0.1:$fake_github_port/"
await_start dispatch "$dispatch_pid" 0 600 "Dispatch" curl -sf "$dispatch_url/"
note "emptying the Dispatch database"
bun "$SCRIPT_DIR/seed.ts"

# --- The agent machine: agent-secrets-helper under the example hostname, and agent-exec. --------
# Only the helper takes the example hostname: the broker learns a host from the helper alone (its
# machine login and each session's runtime id). It drops back to this user before it runs.
printf '%s\n' "$OPERATOR" >"$work/agent/operator"
home="$work/agent/home"
mkdir -p "$home"
cp "$SCRIPT_DIR/agent/bashrc" "$home/.bashrc"
# Ubuntu's /etc/bash.bashrc greets a sudo-group user's every new shell with a sudo hint unless
# $HOME/.hushlogin exists; the agent's shells are this user's.
touch "$home/.hushlogin"
cp -r "$SCRIPT_DIR/agent/demo" "$home/demo"
note "starting the agent machine $AGENT_HOST"
# shellcheck disable=SC2016 # the inner sh expands its own positional parameters
start_process agent sudo -n unshare --uts --fork -- env -i PATH="$work/bin:/usr/bin:/bin" \
  AGENT_SECRETS_URL="$broker_url" \
  AGENT_SECRETS_HELPER_SOCK="$work/agent/helper.sock" \
  AGENT_SECRETS_OPERATOR_FILE="$work/agent/operator" \
  sh -c 'hostname "$1" && echo $$ >"$2" && exec setpriv --reuid="$3" --regid="$4" --init-groups agent-secrets-helper serve' \
  helper "$AGENT_HOST" "$work/agent/helper.pid" "$(id -u)" "$(id -g)"
until_true 30 "the helper's pid" test -s "$work/agent/helper.pid"
helper_pid="$(cat "$work/agent/helper.pid")"
# The helper's own pid, not sudo's: kill -0 cannot probe a root process.
await_start agent "$helper_pid" 0 600 "agent-secrets-helper" \
  grep -q 'agent-secrets-helper listening' "$work/logs/agent.log"
agent_exec="$work/agent-exec"
cat >"$agent_exec" <<EOF
#!/usr/bin/env bash
cd "$home/demo"
exec env -i PATH="$work/bin:/usr/bin:/bin" HOME="$home" TERM="\${TERM:-xterm-256color}" LANG=C.UTF-8 \\
  AGENT_SECRETS_URL="$broker_url" AGENT_SECRETS_HELPER_SOCK="$work/agent/helper.sock" \\
  AGENT_SECRETS_APPROVE_URL="$dispatch_url" "\$@"
EOF
chmod +x "$agent_exec"

cat >&2 <<EOF

rig: ready.

  Dispatch    $dispatch_url  (sign in as $OPERATOR at $dispatch_url/auth/_dev/signin?login=$OPERATOR)
  broker      $broker_url
  agent       $agent_exec bash   (hostname $AGENT_HOST; agent-secrets-helper for $OPERATOR)
  logs        $work/logs

  On the agent machine: agent-secrets launcher login, type its code at $dispatch_url/credentials/machine,
  then agent-secrets register --wait 10 --exec -- bash, then
  agent-secrets DEMO_API_KEY --reason "<why>" -- ./check-demo-key.sh
EOF

if [ "$#" -gt 0 ]; then
  BROKER_RIG_DISPATCH_URL="$dispatch_url" BROKER_RIG_AGENT_EXEC="$agent_exec" "$@"
else
  wait "$broker_pid"
fi
