#!/usr/bin/env bash
# docs/site/media/broker/rig.sh
#
# The secrets broker's demo rig: every surface the broker's screenshots and walkthrough show,
# running on one machine on example data. It starts
#
#   - Postgres: a throwaway container, or the server DATABASE_URL names;
#   - the broker (packages/envoy/cmd/broker) on a local rules file and its fake secrets file
#     (internal/broker/secrets' development store), holding one secret, DEMO_API_KEY, whose value
#     is made up;
#   - the Dispatch e2e harness (packages/dispatch/e2e: fake Envoy, fake GitHub, run-server.sh)
#     pointed at that broker, with the e2e workspace seeded; its signed-in human is `alice`;
#   - an agent machine whose hostname is example-host-build, running agent-secrets-helper (the
#     host side of the broker) for the operator `alice`.
#
# It prints how to drive it and stays in the foreground; Ctrl-C (or the exit of the command given
# after --) stops all of it.
#
#   bash docs/site/media/broker/rig.sh                  # interactive
#   bash docs/site/media/broker/rig.sh -- <command...>  # run <command> against the rig, then stop
#
# The command runs with BROKER_RIG_STATE naming the rig's state file (agent.ts reads it), which
# names agent-exec: a script that runs its arguments on the agent machine, in its demo directory,
# with its environment (an interactive shell when stdin is a terminal: `agent-exec bash`).
#
# Inputs, all optional:
#   DATABASE_URL     a Postgres database this run may truncate for the Dispatch workspace, as the
#                    e2e harness requires; the broker's database is created beside it
#                    (<name>_broker) and dropped on exit. Unset, the rig runs its own Postgres.
#   DISPATCH_E2E_PORT, FAKE_ENVOY_PORT, FAKE_GITHUB_PORT
#                    the harness's ports (packages/dispatch/e2e/harness-ports.ts).
#   BROKER_RIG_NAME  the prefix of the containers it runs (default legion-docs-broker).
#   BROKER_RIG_AGENT_RUNTIME
#                    how the agent machine gets its hostname: `docker` (the default), a container
#                    on the host network; or `unshare`, the helper alone in a UTS namespace of its
#                    own (passwordless sudo; for a machine where containers are unavailable),
#                    with the sessions on this machine.
#
# Needs go, bun, psql, curl, openssl and setsid on PATH, and docker for its own Postgres or the
# docker agent runtime; nothing else from the machine: no credential, private hostname or
# production service.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"
NAME="${BROKER_RIG_NAME:-legion-docs-broker}"
AGENT_RUNTIME="${BROKER_RIG_AGENT_RUNTIME:-docker}"
OPERATOR="alice"
AGENT_HOST="example-host-build"
AGENT_IMAGE="debian:bookworm-slim"
POSTGRES_IMAGE="postgres:16"

export DISPATCH_E2E_PORT="${DISPATCH_E2E_PORT:-8777}"
export FAKE_ENVOY_PORT="${FAKE_ENVOY_PORT:-9021}"
export FAKE_GITHUB_PORT="${FAKE_GITHUB_PORT:-9022}"
DISPATCH_URL="http://127.0.0.1:${DISPATCH_E2E_PORT}"

command_after=()
if [ "${1:-}" = "--" ]; then
  shift
  command_after=("$@")
elif [ "$#" -gt 0 ]; then
  echo "rig: unexpected argument $1 (usage: rig.sh [-- <command...>])" >&2
  exit 2
fi

tools=(go bun psql curl openssl setsid)
case "$AGENT_RUNTIME" in
  docker) tools+=(docker) ;;
  unshare) tools+=(sudo unshare setpriv) ;;
  *)
    echo "rig: BROKER_RIG_AGENT_RUNTIME must be docker or unshare, not $AGENT_RUNTIME" >&2
    exit 2
    ;;
esac
[ -n "${DATABASE_URL:-}" ] || tools+=(docker)
for tool in "${tools[@]}"; do
  command -v "$tool" >/dev/null || { echo "rig: $tool is required on PATH" >&2; exit 1; }
done
if [ "$AGENT_RUNTIME" = unshare ] && ! sudo -n true 2>/dev/null; then
  echo "rig: BROKER_RIG_AGENT_RUNTIME=unshare needs passwordless sudo" >&2
  exit 1
fi

WORK_DIR="$(mktemp -d /tmp/legion-docs-broker.XXXXXX)"
LOG_DIR="$WORK_DIR/logs"
mkdir -p "$LOG_DIR" "$WORK_DIR/bin" "$WORK_DIR/agent"
pids=()
helper_pid=""
owned_postgres=false
broker_database=""
admin_url=""

cleanup() {
  local status=$?
  if [ -z "$helper_pid" ] && [ -s "$WORK_DIR/agent/helper.pid" ]; then
    helper_pid="$(cat "$WORK_DIR/agent/helper.pid")"
  fi
  # Every process the rig starts (but the unshare runtime's sudo) leads a process group of its
  # own, so this reaches its children too: run-server.sh's `go run` leaves the compiled Dispatch
  # server running when only it is signalled.
  for pid in "${pids[@]}"; do
    kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true
  done
  if [ -n "$helper_pid" ]; then
    kill "$helper_pid" 2>/dev/null || true
  fi
  for pid in "${pids[@]}"; do
    wait "$pid" 2>/dev/null || true
  done
  if [ "$AGENT_RUNTIME" = docker ]; then
    docker rm -f "$NAME-agent" >/dev/null 2>&1 || true
  fi
  if [ "$owned_postgres" = true ]; then
    docker rm -f "$NAME-pg" >/dev/null 2>&1 || true
  elif [ -n "$broker_database" ]; then
    PGOPTIONS="-c client_min_messages=warning" psql "$admin_url" -q \
      -c "drop database if exists ${broker_database} with (force)" >/dev/null 2>&1 || true
  fi
  echo "rig: stopped; logs kept in $LOG_DIR" >&2
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# wait_for <seconds> <description> <command...>: polls the command once a second, failing the rig
# naming the description when it never succeeds. A process the rig started that has exited fails
# it at once, with its logs. (ps rather than kill -0, which cannot probe the unshare runtime's
# sudo, a root process.)
wait_for() {
  local seconds="$1" what="$2"
  shift 2
  for _ in $(seq 1 "$seconds"); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    for pid in "${pids[@]}"; do
      if ! ps -p "$pid" >/dev/null; then
        echo "rig: a rig process exited while waiting for $what; logs in $LOG_DIR" >&2
        tail -n 20 "$LOG_DIR"/*.log >&2 || true
        exit 1
      fi
    done
    sleep 1
  done
  echo "rig: $what never came up within ${seconds}s; logs in $LOG_DIR" >&2
  exit 1
}

# --- Binaries: the broker, and the client and host helper the agent machine runs. -------------
echo "rig: building the broker, agent-secrets and agent-secrets-helper" >&2
(cd "$ROOT/packages/envoy" && CGO_ENABLED=0 go build -o "$WORK_DIR/bin/" ./cmd/broker ./cmd/agent-secrets ./cmd/agent-secrets-helper)

# --- Postgres. -----------------------------------------------------------------------------------
if [ -z "${DATABASE_URL:-}" ]; then
  echo "rig: starting a throwaway Postgres ($NAME-pg)" >&2
  docker rm -f "$NAME-pg" >/dev/null 2>&1 || true
  docker run -d --name "$NAME-pg" --tmpfs /var/lib/postgresql/data \
    -e POSTGRES_PASSWORD=demo -e POSTGRES_DB=dispatch -p 127.0.0.1::5432 "$POSTGRES_IMAGE" >/dev/null
  owned_postgres=true
  pg_port="$(docker port "$NAME-pg" 5432/tcp | head -n1 | cut -d: -f2)"
  export DATABASE_URL="postgres://postgres:demo@127.0.0.1:${pg_port}/dispatch?sslmode=disable"
  wait_for 120 "Postgres" psql "$DATABASE_URL" -c "select 1"
fi
url_base="${DATABASE_URL%%\?*}"
url_query="${DATABASE_URL#"$url_base"}"
broker_database="${url_base##*/}_broker"
admin_url="${url_base%/*}/postgres${url_query}"
broker_database_url="${url_base%/*}/${broker_database}${url_query}"
PGOPTIONS="-c client_min_messages=warning" psql "$admin_url" -v ON_ERROR_STOP=1 -q \
  -c "drop database if exists ${broker_database} with (force)" -c "create database ${broker_database}"

# --- The broker, on example rules and a made-up secret. -------------------------------------------
cat >"$WORK_DIR/agent-secret-rules.yaml" <<EOF
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
printf '%s\n' "example/agent-secrets/DEMO_API_KEY=demo-key-not-a-real-secret-7f3a" >"$WORK_DIR/fake-secrets.env"
chmod 600 "$WORK_DIR/fake-secrets.env"
ui_token="$(openssl rand -hex 32)"

echo "rig: starting the broker" >&2
setsid env -i PATH="$PATH" \
  BROKER_DATABASE_URL="$broker_database_url" \
  BROKER_LISTEN_ADDR=127.0.0.1:0 \
  BROKER_PUBLIC_URL=http://127.0.0.1:0 \
  BROKER_UI_TOKEN="$ui_token" \
  BROKER_RULES_FILE="$WORK_DIR/agent-secret-rules.yaml" \
  BROKER_FAKE_SECRETS_FILE="$WORK_DIR/fake-secrets.env" \
  "$WORK_DIR/bin/broker" >"$LOG_DIR/broker.log" 2>&1 &
pids+=("$!")
broker_bound() { grep -q 'broker listening addr=' "$LOG_DIR/broker.log"; }
wait_for 60 "the broker" broker_bound
BROKER_URL="http://$(grep -o 'broker listening addr=[^ ]*' "$LOG_DIR/broker.log" | head -n1 | sed 's/.*addr=//')"
wait_for 30 "the broker's health check" curl -sf "$BROKER_URL/healthz"

# --- Dispatch: the e2e harness's three servers, the server pointed at the broker. ---------------
if [ ! -f "$ROOT/packages/dispatch/web/dist/index.html" ]; then
  echo "rig: building the Dispatch dashboard" >&2
  (cd "$ROOT" && bun install --frozen-lockfile >/dev/null)
  (cd "$ROOT/packages/dispatch" && bun run build:web >/dev/null)
fi
echo "rig: starting Dispatch at $DISPATCH_URL" >&2
if curl -s -o /dev/null "$DISPATCH_URL/"; then
  # Its readiness check below would pass on that server rather than the rig's.
  echo "rig: something already answers at $DISPATCH_URL; stop it or set DISPATCH_E2E_PORT" >&2
  exit 1
fi
setsid bun "$ROOT/packages/dispatch/e2e/fake-envoy.ts" >"$LOG_DIR/fake-envoy.log" 2>&1 &
pids+=("$!")
setsid bun "$ROOT/packages/dispatch/e2e/fake-github.ts" >"$LOG_DIR/fake-github.log" 2>&1 &
pids+=("$!")
DISPATCH_E2E_AGENT_SECRETS_URL="$BROKER_URL" DISPATCH_E2E_AGENT_SECRETS_TOKEN="$ui_token" \
  setsid bash "$ROOT/packages/dispatch/e2e/run-server.sh" >"$LOG_DIR/dispatch.log" 2>&1 &
pids+=("$!")
wait_for 600 "Dispatch" curl -sf "$DISPATCH_URL/"
echo "rig: seeding the e2e workspace" >&2
bun "$SCRIPT_DIR/seed.ts"

# --- The agent machine: agent-secrets-helper under the example hostname, and agent-exec. --------
printf '%s\n' "$OPERATOR" >"$WORK_DIR/agent/operator"
agent_exec="$WORK_DIR/agent-exec"
echo "rig: starting the agent machine $AGENT_HOST ($AGENT_RUNTIME)" >&2
if [ "$AGENT_RUNTIME" = docker ]; then
  docker rm -f "$NAME-agent" >/dev/null 2>&1 || true
  docker run -d --name "$NAME-agent" --network host --hostname "$AGENT_HOST" \
    -e AGENT_SECRETS_URL="$BROKER_URL" \
    -e AGENT_SECRETS_HELPER_SOCK=/run/agent-secrets/helper.sock \
    -e AGENT_SECRETS_OPERATOR_FILE=/etc/agent-secrets/operator \
    -e AGENT_SECRETS_APPROVE_URL="$DISPATCH_URL" \
    -v "$WORK_DIR/bin/agent-secrets:/usr/local/bin/agent-secrets:ro" \
    -v "$WORK_DIR/bin/agent-secrets-helper:/usr/local/bin/agent-secrets-helper:ro" \
    -v "$WORK_DIR/agent/operator:/etc/agent-secrets/operator:ro" \
    -v "$SCRIPT_DIR/agent/bashrc:/root/.bashrc:ro" \
    -v "$SCRIPT_DIR/agent/demo:/root/demo:ro" \
    -w /root/demo \
    "$AGENT_IMAGE" agent-secrets-helper serve >/dev/null
  cat >"$agent_exec" <<EOF
#!/usr/bin/env bash
if [ -t 0 ]; then exec docker exec -it $NAME-agent "\$@"; fi
exec docker exec -i $NAME-agent "\$@"
EOF
  helper_listening() { docker logs "$NAME-agent" 2>&1 | grep -q 'agent-secrets-helper listening'; }
else
  # Only the helper takes the example hostname: the broker learns a host from the helper alone
  # (its machine login and each session's runtime id). It drops back to this user before it runs.
  home="$WORK_DIR/agent/home"
  mkdir -p "$home"
  cp "$SCRIPT_DIR/agent/bashrc" "$home/.bashrc"
  cp -r "$SCRIPT_DIR/agent/demo" "$home/demo"
  sudo -n unshare --uts --fork -- env -i PATH="$WORK_DIR/bin:/usr/bin:/bin" \
    AGENT_SECRETS_URL="$BROKER_URL" \
    AGENT_SECRETS_HELPER_SOCK="$WORK_DIR/agent/helper.sock" \
    AGENT_SECRETS_OPERATOR_FILE="$WORK_DIR/agent/operator" \
    sh -c 'hostname "$1" && echo $$ >"$2" && exec setpriv --reuid="$3" --regid="$4" --init-groups agent-secrets-helper serve' \
    helper "$AGENT_HOST" "$WORK_DIR/agent/helper.pid" "$(id -u)" "$(id -g)" >"$LOG_DIR/helper.log" 2>&1 &
  pids+=("$!")
  wait_for 30 "the helper's pid" test -s "$WORK_DIR/agent/helper.pid"
  helper_pid="$(cat "$WORK_DIR/agent/helper.pid")"
  cat >"$agent_exec" <<EOF
#!/usr/bin/env bash
cd "$home/demo"
exec env -i PATH="$WORK_DIR/bin:/usr/bin:/bin" HOME="$home" TERM="\${TERM:-xterm-256color}" LANG=C.UTF-8 \\
  AGENT_SECRETS_URL="$BROKER_URL" AGENT_SECRETS_HELPER_SOCK="$WORK_DIR/agent/helper.sock" \\
  AGENT_SECRETS_APPROVE_URL="$DISPATCH_URL" "\$@"
EOF
  helper_listening() { grep -q 'agent-secrets-helper listening' "$LOG_DIR/helper.log"; }
fi
chmod +x "$agent_exec"
wait_for 600 "agent-secrets-helper" helper_listening

state_file="$WORK_DIR/rig.env"
cat >"$state_file" <<EOF
BROKER_RIG_AGENT_EXEC=$agent_exec
BROKER_RIG_BROKER_URL=$BROKER_URL
BROKER_RIG_DISPATCH_URL=$DISPATCH_URL
BROKER_RIG_OPERATOR=$OPERATOR
BROKER_RIG_LOG_DIR=$LOG_DIR
EOF

cat >&2 <<EOF

rig: ready.

  Dispatch    $DISPATCH_URL  (signed in by the X-Dispatch-User header; the e2e workspace's human is $OPERATOR)
  broker      $BROKER_URL
  agent       $agent_exec bash   (hostname $AGENT_HOST; agent-secrets-helper for $OPERATOR)
  state       $state_file
  logs        $LOG_DIR

  On the agent machine: agent-secrets launcher login, type its code at $DISPATCH_URL/credentials/machine,
  then agent-secrets register --wait 10 --exec -- bash, then
  agent-secrets DEMO_API_KEY --reason "<why>" -- ./check-demo-key.sh
EOF

if [ "${#command_after[@]}" -gt 0 ]; then
  BROKER_RIG_STATE="$state_file" "${command_after[@]}"
else
  wait "${pids[0]}"
fi
