#!/usr/bin/env bash
# shellcheck disable=SC2317 # cleanup is trap-invoked and each reader is invoked by name
# packages/envoy/scripts/dev-stack.sh
#
# The whole local stack a session is driven against, on one machine, with no cluster, no AWS and
# no container runtime: cmd/dev-oidc-issuer (the local stand-in for a cluster's ServiceAccount
# token issuer and for the identity provider a person signs in at), cmd/broker on a fake secrets
# file, internal/testnats/cmd/natsd (one throwaway NATS with JetStream), cmd/listener on that
# NATS, and cmd/dispatch, the real Dispatch server, pointed at that broker and that listener. It
# is dev-broker.sh's broker with the two things that script has no room for -- a real Dispatch and
# a real Envoy listener -- so a session on it gets a broker access token, writes to Dispatch with
# it and sends an Envoy message, all against the processes production runs.
#
# It takes one input, the URL of a Postgres server it may create databases on: --database-url,
# else LEGION_TEST_PG_DSN (a Legion pod has one), else DATABASE_URL. With none it refuses, naming
# all three. The URL's own database is never written: this run creates two databases of its own on
# that server, dev_stack_broker_<suffix> for the broker and dev_stack_dispatch_<suffix> for
# Dispatch, named from this run's own work directory as dev-broker.sh names its one, and drops
# both on exit. They are two because they are two schemas: the broker records its migrations in
# broker_schema_migrations and Dispatch records its own in schema_migrations, each migrator
# applying every migration it does not find in its own table, and production gives them separate
# databases for the same reason.
#
# Every port is chosen by the kernel and read back from the process that holds it: the issuer's
# "issuer listening url=", the broker's "broker listening addr=", natsd's "Listening for client
# connections on", the listener's JSON "envoy-listener listening" addr, and Dispatch's
# "dispatch: listening" addr. So a URL this script prints can only ever name this run's own
# process, never one left over from another. Dispatch's port is the one exception to "bind, then
# report": the broker must name Dispatch's machine-login page before it starts, and Dispatch must
# name the broker's URL before it starts, so scripts/e2e/lib/held-port.py holds a socket on a port
# the kernel chose for the whole run (as docs/site/media/broker/rig.sh does) and Dispatch serves
# on it (DISPATCH_LISTEN_FD) rather than binding one; it still reports that port in its own
# listening line, and that line is what this script reads.
#
# Dispatch runs on its dev sign-in route (DISPATCH_DEV_SIGNIN=1), so a browser on this machine is
# signed in as any person it names with no sign-in pool, and the broker's approval page signs a
# person in at dev-oidc-issuer's code flow, as dev-broker.sh's does. The issuer also serves the
# device authorization grant (--device-flow) for a client with no browser of its own, and approves
# a pending user code at POST <issuer>/_dev/approve-device-code, so a sign-in a person would do at
# Google can be driven from a shell here. Every process is started with env -i and the variables
# it needs alone: a Legion pod exports AGENT_SECRETS_URL, DISPATCH_URL, ENVOY_URL and
# ENVOY_NATS_URL of the real deployment, and none of them may reach a process of this stack.
#
# It prints one line per process as that process reports its port, then "dev-stack: ready." and
# the exports a second shell copies, and nothing else is needed to use the stack:
#
#   PATH                                  this run's binaries first (agent-secrets and the servers)
#   AGENT_SECRETS_URL                     the broker
#   AGENT_SECRETS_ACCOUNT_TOKEN_FILE      the caller "dispatch"'s ServiceAccount token file
#   AGENT_SECRETS_APPROVER                the person the issuer signs in, who approves a secret
#   AGENT_SECRETS_APPROVE_URL             Dispatch, where a machine login's code is typed in
#   AGENT_SECRETS_HELPER_SOCK             agent-secrets-helper's socket, in the work directory
#   AGENT_SECRETS_OPERATOR_FILE           the helper's operator file
#   AGENT_SECRETS_MACHINE_FILE            the machine credential agent-secrets writes
#   AGENT_SECRETS_KEY_DIR                 a session key directory of this run's own
#   DISPATCH_URL, DISPATCH_TOKEN          Dispatch and its shared agent token
#   ENVOY_URL                             the listener
#   NATS_URLS                             this run's NATS
#   DEV_STACK_DIR                         the work directory (logs, tokens, fake secrets)
#   DEV_STACK_ISSUER_URL                  dev-oidc-issuer, for a sign-in or a minted token
#   DEV_STACK_EXCHANGE_TOKEN_FILE         the pod's token for the broker's exchange audience
#   DEV_STACK_POD_TOKEN_FILE              that same pod's token for the broker's own audience
#   DEV_STACK_REGISTRAR_TOKEN_FILE        the service login that registers a pod for a session
#   DEV_STACK_LISTENER_TOKEN_FILE         the caller "listener", which may introspect alone
#
# It then stays in the foreground until it is interrupted or any process it started stops. One
# trap stops every process by the pid this script started it as, drops the two databases it
# created and nothing else, and leaves the logs; it runs on a normal exit, on Ctrl-C, on SIGTERM
# and on a failure part way through, so a start that fails before "ready" leaves nothing running.
#
#   LEGION_TEST_PG_DSN=... bash scripts/dev-stack.sh    # a Legion pod: the variable is already set
#   bash scripts/dev-postgres.sh                        # a box with Docker: start the shared
#   bash scripts/dev-stack.sh \                         # dispatch-pg, then name it here
#     --database-url 'postgres://postgres:dispatch@127.0.0.1:55432/postgres?sslmode=disable'
#
# Both from packages/envoy. It needs go, psql, curl, jq and python3, and nothing else from the
# machine: no credential, no private hostname, no production service.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$ENVOY_DIR/../.." && pwd)"
HELD_PORT="$REPO_ROOT/scripts/e2e/lib/held-port.py"

usage() {
  cat <<'EOF'
Usage: dev-stack.sh [--database-url <postgres url>] [--help]

Boot dev-oidc-issuer, the secrets broker, a throwaway NATS, the Envoy listener and the Dispatch
server on one machine, each on a port the kernel chooses, and print the exports a second shell
drives them with. The Postgres URL is this one option, else LEGION_TEST_PG_DSN, else
DATABASE_URL; this run creates and drops two databases of its own on that server.
EOF
}

DATABASE_URL_OPTION=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --database-url)
      if [ "$#" -lt 2 ]; then
        echo "dev-stack: --database-url needs a postgres:// URL" >&2
        exit 2
      fi
      DATABASE_URL_OPTION="$2"
      shift 2
      ;;
    --database-url=*)
      DATABASE_URL_OPTION="${1#--database-url=}"
      shift
      ;;
    --help | -h)
      usage
      exit 0
      ;;
    *)
      echo "dev-stack: unexpected argument $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

# --- The one input, checked before anything is created, so a missing or malformed URL leaves
# nothing behind. The three sources in order, as the header names them. ---
ADMIN_URL="${DATABASE_URL_OPTION:-${LEGION_TEST_PG_DSN:-${DATABASE_URL:-}}}"
if [ -z "$ADMIN_URL" ]; then
  cat >&2 <<'EOF'
dev-stack: no Postgres server to create this run's databases on. Name one with --database-url, or
set LEGION_TEST_PG_DSN (a Legion pod already has it) or DATABASE_URL, as
postgres://<user>@<host>:<port>/<database> naming a login that may create and drop databases. On a
box with Docker, scripts/dev-postgres.sh starts the shared dispatch-pg container and its server is
postgres://postgres:dispatch@127.0.0.1:55432/postgres?sslmode=disable.
EOF
  exit 2
fi
if ! [[ "$ADMIN_URL" =~ ^(postgres(ql)?://[^/?]+)/[^/?]*(\?.*)?$ ]]; then
  echo "dev-stack: the Postgres URL must be postgres://<user>@<host>:<port>/<database>" >&2
  exit 2
fi
POSTGRES_SERVER="${BASH_REMATCH[1]}"
POSTGRES_QUERY="${BASH_REMATCH[3]:-}"

for tool in go psql curl jq python3; do
  command -v "$tool" >/dev/null || {
    echo "dev-stack: $tool is required on PATH" >&2
    exit 1
  }
done

# --- This run's own directory: every binary, log, token and socket lives under it, so a walk
# through the stack leaves nothing in the caller's directory. TMPDIR, where a pod keeps the disk
# that is allowed to grow. ---
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/dev-stack.XXXXXX")"
RUN_SUFFIX="$(printf '%s' "${WORK_DIR##*.}" | tr '[:upper:]' '[:lower:]')"
BIN_DIR="$WORK_DIR/bin"
LOG_DIR="$WORK_DIR/logs"
mkdir -p "$BIN_DIR" "$LOG_DIR" "$WORK_DIR/keys" "$WORK_DIR/dispatch-home"

ISSUER_LOG="$LOG_DIR/dev-oidc-issuer.log"
BROKER_LOG="$LOG_DIR/broker.log"
NATS_LOG="$LOG_DIR/natsd.log"
LISTENER_LOG="$LOG_DIR/listener.log"
DISPATCH_LOG="$LOG_DIR/dispatch.log"
KEEPER_LOG="$LOG_DIR/dispatch-port-keeper.log"
KEEPER_SOCKET="$WORK_DIR/dispatch-port.sock"
KEEPER_PORT_FILE="$WORK_DIR/dispatch-port"

# The issuer and what it mints for, as dev-broker.sh names them (internal/broker/brokertest's rig):
# the audience every listed caller's and every enrolment's token carries, the exchange's own
# audience, and the accounts. The pod's two token files are one pod, since dev-oidc-issuer binds
# one pod uid per service account.
BROKER_AUDIENCE="legion-broker-pod"
EXCHANGE_AUDIENCE="legion-broker-exchange"
API_ACCOUNT="system:serviceaccount:example-ns:example-dispatch"
LISTENER_ACCOUNT="system:serviceaccount:example-ns:example-listener"
EXCHANGE_ACCOUNT="system:serviceaccount:example-ns:example-worker"
REGISTRAR_ACCOUNT="system:serviceaccount:example-ns:example-daemon"
EXCHANGE_PROJECT="EXAMPLE"
EXCHANGE_SESSION="example-worker-session"
ACCOUNT_TOKEN_FILE="$WORK_DIR/account-token"
LISTENER_TOKEN_FILE="$WORK_DIR/listener-token"
EXCHANGE_TOKEN_FILE="$WORK_DIR/exchange-token"
REGISTRAR_TOKEN_FILE="$WORK_DIR/registrar-token"
POD_TOKEN_FILE="$WORK_DIR/pod-token"
APPROVER_LOGIN="ada@example.com"
OPERATOR_FILE="$WORK_DIR/operator"
FAKE_SECRETS_FILE="$WORK_DIR/fake-secrets.json"
# The namespace the fake secrets sit under and an example KMS key in AWS's documentation account;
# nothing here calls AWS.
SECRETS_PREFIX="example/agent-secrets/"
SECRETS_KMS_KEY_ARN="arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
# The broker's own sign-in client at that issuer, through which a person signs in to its approval
# page, and the device-grant client a shell signs in through; each secret is generated below.
SIGNIN_CLIENT_ID="dev-stack-broker"
SIGNIN_SECRET_FILE="$WORK_DIR/signin-client-secret"
SIGNIN_DOMAIN="${APPROVER_LOGIN#*@}"
DEVICE_CLIENT_ID="dev-stack-device"
DEVICE_SECRET_FILE="$WORK_DIR/device-client-secret"

BROKER_DB="dev_stack_broker_${RUN_SUFFIX}"
DISPATCH_DB="dev_stack_dispatch_${RUN_SUFFIX}"
BROKER_DATABASE_URL="${POSTGRES_SERVER}/${BROKER_DB}${POSTGRES_QUERY}"
DISPATCH_DATABASE_URL="${POSTGRES_SERVER}/${DISPATCH_DB}${POSTGRES_QUERY}"

admin_psql() { psql "$ADMIN_URL" -v ON_ERROR_STOP=1 -q "$@"; }

KEEPER_PID=""
ISSUER_PID=""
BROKER_PID=""
NATS_PID=""
LISTENER_PID=""
DISPATCH_PID=""
# Each is set once this run's own create database succeeded: a run whose create failed (the name
# taken by another run) must never drop that other run's database.
BROKER_DB_CREATED=false
DISPATCH_DB_CREATED=false

# cleanup stops what this run started and exits with rc, the status the trap that ran it passes:
# this script's own for EXIT, and the conventional 128 plus the signal's number for INT and TERM,
# so an interrupted run never reports success. Each process is stopped by the pid this script
# started it as, in the reverse of the order they were started, and each database is dropped with
# (force) once its server has no connection of this run's left to lose.
cleanup() {
  local rc="$1" pid
  trap - EXIT INT TERM
  set +e
  for pid in "$DISPATCH_PID" "$LISTENER_PID" "$NATS_PID" "$BROKER_PID" "$ISSUER_PID" "$KEEPER_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null
      wait "$pid" 2>/dev/null
    fi
  done
  # Best-effort: cleanup runs on every exit path and must never itself fail, even if the server is
  # already gone or a drop fails for some other reason.
  if [ "$BROKER_DB_CREATED" = true ]; then
    admin_psql -c "drop database if exists ${BROKER_DB} with (force);" >/dev/null 2>&1
    echo "dev-stack: dropped this run's broker database $BROKER_DB" >&2
  fi
  if [ "$DISPATCH_DB_CREATED" = true ]; then
    admin_psql -c "drop database if exists ${DISPATCH_DB} with (force);" >/dev/null 2>&1
    echo "dev-stack: dropped this run's Dispatch database $DISPATCH_DB" >&2
  fi
  echo "dev-stack: stopped; logs kept in $LOG_DIR" >&2
  exit "$rc"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

echo "dev-stack: workdir $WORK_DIR" >&2

# --- The two databases, created before anything is started so a server that refuses them costs
# no process. ---
admin_psql -c "create database ${BROKER_DB};"
BROKER_DB_CREATED=true
admin_psql -c "create database ${DISPATCH_DB};"
DISPATCH_DB_CREATED=true
echo "dev-stack: created databases $BROKER_DB and $DISPATCH_DB" >&2

# A dbname= in the URL's query string overrides the URL's own database, which would point the
# broker and Dispatch, and their migrations, at a database this run neither created nor drops.
for url in "$BROKER_DATABASE_URL" "$DISPATCH_DATABASE_URL"; do
  want="${url%%\?*}"
  want="${want##*/}"
  connected="$(psql "$url" -v ON_ERROR_STOP=1 -Atqc 'select current_database()')"
  if [ "$connected" != "$want" ]; then
    echo "dev-stack: refused: a database URL of this run connects to '$connected', not $want; drop any dbname= from the Postgres URL's query string" >&2
    exit 1
  fi
done
# The banner names the databases without any password their URL carries.
DISPLAY_SERVER="$(printf '%s' "$POSTGRES_SERVER" | sed -E 's#^(postgres(ql)?://[^:/@?]+):[^@/?]*@#\1:***@#')"

# --- Every binary this stack runs, built into this run's own directory. One go build: the
# packages share most of their closure, so building them together is one compile of it. ---
echo "dev-stack: building broker, dev-oidc-issuer, dispatch, listener, natsd, agent-secrets and agent-secrets-helper..." >&2
(
  cd "$ENVOY_DIR"
  GOTOOLCHAIN=go1.27.1 go build -o "$BIN_DIR/" \
    ./cmd/broker ./cmd/dev-oidc-issuer ./cmd/dispatch ./cmd/listener \
    ./cmd/agent-secrets ./cmd/agent-secrets-helper ./internal/testnats/cmd/natsd
)

# --- Each process's readiness is read from its own log, never from a port this script chose:
# await runs one reader until it prints a value, and fails the moment the process it watches is
# gone. The readers each end in `|| true` territory: under this script's pipefail a grep that has
# not matched yet is the normal case on every pass before the line is written, not a failure. ---
awaited=""
await() {
  local name="$1" pid="$2" log="$3" source="$4" what="$5"
  shift 5
  local deadline=$((SECONDS + 60))
  awaited=""
  while ((SECONDS < deadline)); do
    awaited="$("$@" "$source" 2>/dev/null || true)"
    if [ -n "$awaited" ]; then
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "dev-stack: $name exited during startup" >&2
      cat "$log" >&2 || true
      return 1
    fi
    sleep 0.25
  done
  echo "dev-stack: $name did not report $what within 60s" >&2
  cat "$log" >&2 || true
  return 1
}

# until_true runs a command until it succeeds, for the reason await exists: nothing here waits on
# a fixed delay.
until_true() {
  local name="$1"
  shift
  local deadline=$((SECONDS + 60))
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.25
  done
  echo "dev-stack: $name did not answer within 60s" >&2
  return 1
}

issuer_url_in() { grep -o 'issuer listening url=[^ ]*' "$1" | head -n1 | cut -d= -f2-; }
broker_addr_in() { grep -o 'broker listening addr=[^ ]*' "$1" | head -n1 | cut -d= -f2-; }
nats_port_in() {
  grep -q 'Server is ready' "$1" || return 1
  sed -n 's/.*Listening for client connections on 127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' "$1" | head -n1
}
listener_addr_in() { jq -Rnr '[inputs | fromjson? | select(.msg == "envoy-listener listening") | .addr][0] // empty' "$1"; }
dispatch_addr_in() { sed -n 's/.*msg="dispatch: listening" addr=\([^ ]*\).*/\1/p' "$1" | head -n1; }
kept_port_in() { grep -o '^[0-9][0-9]*$' "$1" | head -n1; }

# --- Dispatch's port, held from the kernel's choice until Dispatch serves on it. The broker names
# Dispatch's machine-login page to every machine that logs in, and Dispatch names the broker's URL
# at its own boot, so one of the two must know the other's port before either starts; a keeper
# holds Dispatch's, and Dispatch serves on a copy of its socket (held-port.py keep/exec). ---
echo "dev-stack: holding a port for Dispatch..." >&2
python3 "$HELD_PORT" keep --host 127.0.0.1 --port-file "$KEEPER_PORT_FILE" --socket "$KEEPER_SOCKET" \
  >"$KEEPER_LOG" 2>&1 &
KEEPER_PID=$!
# The keeper writes its port file only once it is serving its socket, so that file naming a port
# is also the word that Dispatch's start below can be handed the socket.
await 'the Dispatch port keeper' "$KEEPER_PID" "$KEEPER_LOG" "$KEEPER_PORT_FILE" 'the port it holds' kept_port_in
DISPATCH_PORT="$awaited"
DISPATCH_URL="http://127.0.0.1:${DISPATCH_PORT}"

# --- The local issuer. It writes every token file before it logs its URL, so once that line is
# there the files are there to be read. Every sign-in it serves, through the code flow or the
# device grant, signs the approver in: nothing here queues another person at its
# POST /_dev/sign-in-as control route. The two client secrets are written under umask 077, so
# neither is readable by anyone else even for a moment, and neither process takes one on its
# command line. ---
(umask 077 && od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$SIGNIN_SECRET_FILE")
(umask 077 && od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$DEVICE_SECRET_FILE")
echo "dev-stack: starting dev-oidc-issuer..." >&2
: >"$ISSUER_LOG"
env -i PATH="$PATH" "$BIN_DIR/dev-oidc-issuer" \
  --token "$BROKER_AUDIENCE=$API_ACCOUNT=$ACCOUNT_TOKEN_FILE" \
  --token "$BROKER_AUDIENCE=$LISTENER_ACCOUNT=$LISTENER_TOKEN_FILE" \
  --token "$EXCHANGE_AUDIENCE=$EXCHANGE_ACCOUNT=$EXCHANGE_TOKEN_FILE" \
  --token "$BROKER_AUDIENCE=$EXCHANGE_ACCOUNT=$POD_TOKEN_FILE" \
  --token "$BROKER_AUDIENCE=$REGISTRAR_ACCOUNT=$REGISTRAR_TOKEN_FILE" \
  --code-flow "$SIGNIN_CLIENT_ID=$SIGNIN_SECRET_FILE" \
  --device-flow "$DEVICE_CLIENT_ID=$DEVICE_SECRET_FILE" \
  --sign-in-as "$APPROVER_LOGIN" \
  >"$ISSUER_LOG" 2>&1 &
ISSUER_PID=$!
await dev-oidc-issuer "$ISSUER_PID" "$ISSUER_LOG" "$ISSUER_LOG" 'its URL' issuer_url_in
ISSUER_URL="$awaited"
echo "dev-stack: dev-oidc-issuer $ISSUER_URL" >&2

# --- The broker, on a fake Secrets Manager holding one agent-tier secret its owner's sessions get
# without asking and one human-tier secret its owner approves, both owned by the person the issuer
# signs in. BROKER_LISTEN_ADDR and BROKER_PUBLIC_URL are port 0: cmd/broker reads a public URL
# whose port is 0 as "derive it from whatever address I bind", and the real one is read from its
# own listening line below. BROKER_DRAIN_SECONDS=0 turns off the five-second drain that exists to
# outlast a Kubernetes EndpointSlice propagation nothing here has. ---
cat >"$FAKE_SECRETS_FILE" <<EOF
{"secrets": [
  {"name": "${SECRETS_PREFIX}demo-read-token", "kms_key_id": "${SECRETS_KMS_KEY_ARN}",
   "tags": {"owner": "${APPROVER_LOGIN}", "tier": "agent"}, "value": "demo-read-token-value"},
  {"name": "${SECRETS_PREFIX}demo-api-key", "kms_key_id": "${SECRETS_KMS_KEY_ARN}",
   "tags": {"owner": "${APPROVER_LOGIN}", "tier": "human"}, "value": "demo-api-key-value"}
]}
EOF
chmod 600 "$FAKE_SECRETS_FILE"
echo "$APPROVER_LOGIN" >"$OPERATOR_FILE"
echo "dev-stack: starting broker..." >&2
: >"$BROKER_LOG"
env -i PATH="$PATH" \
  BROKER_DATABASE_URL="$BROKER_DATABASE_URL" \
  BROKER_LISTEN_ADDR="127.0.0.1:0" \
  BROKER_PUBLIC_URL="http://127.0.0.1:0" \
  BROKER_MACHINE_LOGIN_URL="$DISPATCH_URL/credentials/machine" \
  BROKER_SECRETS_PREFIX="$SECRETS_PREFIX" \
  BROKER_SECRETS_KMS_KEY_ARN="$SECRETS_KMS_KEY_ARN" \
  BROKER_FAKE_SECRETS_FILE="$FAKE_SECRETS_FILE" \
  BROKER_DRAIN_SECONDS=0 \
  BROKER_K8S_OIDC_ISSUER="$ISSUER_URL" \
  BROKER_K8S_OIDC_AUDIENCE="$BROKER_AUDIENCE" \
  BROKER_API_ACCOUNTS="dispatch=$API_ACCOUNT listener=$LISTENER_ACCOUNT" \
  BROKER_SIGNIN_ISSUER="$ISSUER_URL" \
  BROKER_SIGNIN_CLIENT_ID="$SIGNIN_CLIENT_ID" \
  BROKER_SIGNIN_CLIENT_SECRET_FILE="$SIGNIN_SECRET_FILE" \
  BROKER_SIGNIN_DOMAIN="$SIGNIN_DOMAIN" \
  BROKER_EXCHANGE_K8S_AUDIENCE="$EXCHANGE_AUDIENCE" \
  BROKER_EXCHANGE_ACCOUNTS="$EXCHANGE_ACCOUNT=$EXCHANGE_PROJECT=$APPROVER_LOGIN" \
  BROKER_SERVICE_LOGINS="legion-daemon=$REGISTRAR_ACCOUNT" \
  "$BIN_DIR/broker" >"$BROKER_LOG" 2>&1 &
BROKER_PID=$!
await broker "$BROKER_PID" "$BROKER_LOG" "$BROKER_LOG" 'its bound address' broker_addr_in
BROKER_URL="http://$awaited"
until_true "the broker's health check" curl -sf "$BROKER_URL/healthz"
echo "dev-stack: broker $BROKER_URL" >&2

# --- This stack's pod, registered for the session name it exchanges under. The broker binds a
# session name to the listed account and never to a pod, so what keeps two pods of one account
# apart is this row, which a deployment's daemon writes as it starts a pod; here the registrar
# account stands in for that daemon, and the body carries the pod's own broker-audience token.
# Without it the exchange is refused invalid_grant, naming the session. ---
if ! curl -sf -X POST "$BROKER_URL/v1/oauth/exchange/pod-sessions" \
  -H "Authorization: Bearer $(cat "$REGISTRAR_TOKEN_FILE")" \
  -H 'Content-Type: application/json' \
  -d "{\"pod_token\": \"$(cat "$POD_TOKEN_FILE")\", \"session\": \"$EXCHANGE_SESSION\"}" >/dev/null; then
  echo "dev-stack: registering the exchange pod for $EXCHANGE_SESSION failed" >&2
  exit 1
fi

# --- The bus: one nats-server in a process of this run's own (internal/testnats/cmd/natsd), with
# JetStream in the work directory, since an agent's machine has no container runtime. -1 is
# nats-server's own "choose a port". ---
echo "dev-stack: starting natsd..." >&2
: >"$NATS_LOG"
env -i PATH="$PATH" "$BIN_DIR/natsd" -p -1 -store_dir "$WORK_DIR/nats-store" >"$NATS_LOG" 2>&1 &
NATS_PID=$!
await natsd "$NATS_PID" "$NATS_LOG" "$NATS_LOG" 'its client port' nats_port_in
NATS_URL="nats://127.0.0.1:$awaited"
echo "dev-stack: natsd $NATS_URL" >&2

# --- The listener, on that bus. ENVOY_HOST_BRIDGE=127.0.0.1 because every session of this stack
# runs on this machine, and PORT=0 for the kernel's choice, which it reports in its own listening
# line. It serves /v1 with no credential, which the listener allows on loopback alone. ---
echo "dev-stack: starting listener..." >&2
: >"$LISTENER_LOG"
env -i PATH="$PATH" \
  PORT=0 \
  ENVOY_LISTEN_HOST=127.0.0.1 \
  ENVOY_MACHINE_ID="dev-stack-$RUN_SUFFIX" \
  ENVOY_HOST_BRIDGE=127.0.0.1 \
  NATS_URLS="$NATS_URL" \
  "$BIN_DIR/listener" >"$LISTENER_LOG" 2>&1 &
LISTENER_PID=$!
await listener "$LISTENER_PID" "$LISTENER_LOG" "$LISTENER_LOG" 'its address' listener_addr_in
LISTENER_URL="http://$awaited"
until_true "the listener's health check" curl -sf "$LISTENER_URL/healthz"
echo "dev-stack: listener $LISTENER_URL" >&2

# --- Dispatch, on the keeper's socket (DISPATCH_LISTEN_FD=3, which held-port.py puts the socket
# on), pointed at this run's broker, listener and NATS. Its dev sign-in route stands in for the
# sign-in pool, and its fence requires exactly what this stack is: a loopback listener, a loopback
# database, a loopback broker and listener, no DISPATCH_SIGNIN_* and no signing key of a
# deployment's. HOME and the XDG directories are this run's own, so the server reads no app.json
# and writes no key where a developer keeps theirs. ---
DISPATCH_TOKEN_VALUE="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
echo "dev-stack: starting dispatch at $DISPATCH_URL..." >&2
: >"$DISPATCH_LOG"
python3 "$HELD_PORT" exec --from "$KEEPER_SOCKET" --fd-env DISPATCH_LISTEN_FD -- \
  env -i PATH="$PATH" \
  HOME="$WORK_DIR/dispatch-home" \
  XDG_CONFIG_HOME="$WORK_DIR/dispatch-home/config" \
  XDG_DATA_HOME="$WORK_DIR/dispatch-home/data" \
  XDG_CACHE_HOME="$WORK_DIR/dispatch-home/cache" \
  DATABASE_URL="$DISPATCH_DATABASE_URL" \
  DISPATCH_LISTEN_FD=3 \
  DISPATCH_LISTEN_HOST=127.0.0.1 \
  DISPATCH_SERVER_URL="$DISPATCH_URL" \
  DISPATCH_IDENTITY=cookie \
  DISPATCH_DEV_SIGNIN=1 \
  DISPATCH_INSECURE_COOKIE=1 \
  DISPATCH_AGENT_TOKEN="$DISPATCH_TOKEN_VALUE" \
  DISPATCH_AGENT_SECRETS_URL="$BROKER_URL" \
  DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$ACCOUNT_TOKEN_FILE" \
  DISPATCH_WEB_DIST="$REPO_ROOT/packages/dispatch/web/dist" \
  ENVOY_URL="$LISTENER_URL" \
  NATS_URLS="$NATS_URL" \
  "$BIN_DIR/dispatch" >"$DISPATCH_LOG" 2>&1 &
DISPATCH_PID=$!
await dispatch "$DISPATCH_PID" "$DISPATCH_LOG" "$DISPATCH_LOG" 'the address it serves on' dispatch_addr_in
DISPATCH_URL="http://$awaited"
until_true "Dispatch's route index" curl -sf "$DISPATCH_URL/api/v1"
echo "dev-stack: dispatch $DISPATCH_URL" >&2

cat <<EOF

dev-stack: ready.

  export PATH=$BIN_DIR:\$PATH
  export AGENT_SECRETS_URL=$BROKER_URL
  export AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$ACCOUNT_TOKEN_FILE
  export AGENT_SECRETS_APPROVER=$APPROVER_LOGIN
  export AGENT_SECRETS_APPROVE_URL=$DISPATCH_URL
  export AGENT_SECRETS_HELPER_SOCK=$WORK_DIR/helper.sock
  export AGENT_SECRETS_OPERATOR_FILE=$OPERATOR_FILE
  export AGENT_SECRETS_MACHINE_FILE=$WORK_DIR/machine.json
  export AGENT_SECRETS_KEY_DIR=$WORK_DIR/keys
  export DISPATCH_URL=$DISPATCH_URL
  export DISPATCH_TOKEN=$DISPATCH_TOKEN_VALUE
  export ENVOY_URL=$LISTENER_URL
  export NATS_URLS=$NATS_URL
  export DEV_STACK_DIR=$WORK_DIR
  export DEV_STACK_ISSUER_URL=$ISSUER_URL
  export DEV_STACK_EXCHANGE_TOKEN_FILE=$EXCHANGE_TOKEN_FILE
  export DEV_STACK_POD_TOKEN_FILE=$POD_TOKEN_FILE
  export DEV_STACK_REGISTRAR_TOKEN_FILE=$REGISTRAR_TOKEN_FILE
  export DEV_STACK_LISTENER_TOKEN_FILE=$LISTENER_TOKEN_FILE

  binaries:   $BIN_DIR
  logs:       $LOG_DIR  (one per process)
  databases:  $BROKER_DB and $DISPATCH_DB on $DISPLAY_SERVER (created and dropped by this run)
  fake secrets: $FAKE_SECRETS_FILE  (DEMO_READ_TOKEN at the agent tier, DEMO_API_KEY at the human tier, both owned by $APPROVER_LOGIN)

  Sign in to Dispatch at $DISPATCH_URL/auth/_dev/signin?login=$APPROVER_LOGIN, and approve a
  secret request on the broker's own page, $BROKER_URL/approve/<record>, which signs the same
  person in at dev-oidc-issuer with no page to fill in. The pod $EXCHANGE_ACCOUNT exchanges
  $EXCHANGE_TOKEN_FILE's token for an access token that writes $EXCHANGE_PROJECT as
  $APPROVER_LOGIN, under the session $EXCHANGE_SESSION this run registered it for. A device
  sign-in: POST $ISSUER_URL/device_authorization as the client $DEVICE_CLIENT_ID, then
  POST $ISSUER_URL/_dev/approve-device-code with {"user_code": "..."} for the person who would
  type that code in at Google.

  Press Ctrl-C to stop every process and drop both databases.
EOF

# The stack stands until it is interrupted or any one of its processes stops; either way the trap
# above stops the rest and drops the databases.
status=0
wait -n || status=$?
echo "dev-stack: a process of this stack stopped (status $status); stopping the rest" >&2
exit "$status"
