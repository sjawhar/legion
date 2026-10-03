#!/usr/bin/env bash
# packages/envoy/scripts/dev-broker.sh
#
# The secrets broker's local dev surface, one a human can drive without AWS or Dispatch. Boots
# Postgres (dev-postgres.sh, made idempotent here since that script has no guard of its own),
# builds the broker and its three clients (agent-secrets, agent-secrets-helper and
# agent-secrets-devrelay), writes a scratch rules file (DEMO_READ_TOKEN, granted automatically, and
# DEMO_API_KEY, which APPROVER_LOGIN approves, each for a box or host session APPROVER_LOGIN
# operates) and a fake secrets file, then starts cmd/broker against them. Prints the exports a
# second shell needs to drive the clients against it: devrelay stands in for Dispatch's
# credential-request relay, sending the broker's UI routes the UI bearer and the approving human's
# login, as Dispatch does when a signed-in human clicks Approve.
#
# Each invocation creates and drops its own isolated Postgres database (named from this run's own
# WORK_DIR, below) and binds an OS-assigned ephemeral port (BROKER_LISTEN_ADDR=127.0.0.1:0), so two
# agent sessions each running their own dev broker stack never see each other's enrollments,
# requests or grants. The database lives in the shared dispatch-pg container, which this script
# starts in Docker when it is not running, or, with DEV_BROKER_POSTGRES_URL set
# (postgres://<user>@<host>:<port>/<database>, a role that may create databases), on that server
# through psql, with no Docker at all. cmd/broker binds before it reports anything and
# logs the address it actually bound; this script waits for that line and reads the real port from
# it, so a curl success can only ever mean this instance's own broker answered.
#
# The helper a second shell runs keeps its socket, sessions file and log in WORK_DIR too: the
# printed exports name WORK_DIR's helper.sock and operator file (written here), so a walk through
# the stack leaves nothing in the caller's directory.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

POSTGRES_CONTAINER="dispatch-pg"

# The kernel never hands out a port a live listener already holds, so binding 127.0.0.1:0 (an
# OS-assigned ephemeral port) makes a same-port collision between two dev instances impossible.
# BROKER_PUBLIC_URL mirrors it (also port 0): cmd/broker/main.go treats a BROKER_PUBLIC_URL whose
# port is 0 as "derive my public URL from whatever address I actually bind," since
# BROKER_PUBLIC_URL must be set before boot but the real port is only known once Listen succeeds.
# Both roles' real, reachable value is read from the broker's own "broker listening" log line
# once it's running (below).
LISTEN_ADDR="127.0.0.1:0"
PUBLIC_URL="http://127.0.0.1:0"
UI_TOKEN="${BROKER_UI_TOKEN:-dev}"
APPROVER_LOGIN="ada@example.com"

# DEV_BROKER_POSTGRES_URL is checked before anything is created, so a malformed one leaves nothing
# behind.
if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ] && ! [[ "$DEV_BROKER_POSTGRES_URL" =~ ^(postgres(ql)?://[^/?]+)/[^/?]*(\?.*)?$ ]]; then
  echo "dev-broker: DEV_BROKER_POSTGRES_URL must be postgres://<user>@<host>:<port>/<database>" >&2
  exit 2
fi
POSTGRES_SERVER="${BASH_REMATCH[1]:-}"
POSTGRES_QUERY="${BASH_REMATCH[3]:-}"

WORK_DIR="$(mktemp -d /tmp/agent-secrets-dev.XXXXXX)"
BIN_DIR="$WORK_DIR/bin"
BROKER_BIN="$BIN_DIR/broker"
RULES_FILE="$WORK_DIR/agent-secret-rules.yaml"
FAKE_SECRETS_FILE="$WORK_DIR/fake-secrets.env"
BROKER_LOG="$WORK_DIR/broker.log"
OPERATOR_FILE="$WORK_DIR/operator"
# This instance's own database, named from WORK_DIR's mktemp-generated random suffix (already
# unique per invocation). Postgres unquoted identifiers fold to lowercase anyway, but lowercase
# explicitly rather than rely on that.
DB_NAME="dev_broker_$(printf '%s' "${WORK_DIR##*.}" | tr '[:upper:]' '[:lower:]')"
if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ]; then
  POSTGRES_URL="${POSTGRES_SERVER}/${DB_NAME}${POSTGRES_QUERY}"
  POSTGRES_WHERE="the server DEV_BROKER_POSTGRES_URL names"
  admin_psql() { psql "$DEV_BROKER_POSTGRES_URL" -v ON_ERROR_STOP=1 -q "$@"; }
else
  POSTGRES_URL="postgres://postgres:dispatch@127.0.0.1:55432/${DB_NAME}?sslmode=disable"
  POSTGRES_WHERE="the shared $POSTGRES_CONTAINER container, left running ('docker stop $POSTGRES_CONTAINER' tears it down)"
  admin_psql() { docker exec "$POSTGRES_CONTAINER" psql -U postgres -v ON_ERROR_STOP=1 -q "$@"; }
fi

BROKER_PID=""
# Set once this run's own create database succeeded: a run whose create failed (the name taken by
# another run) must never drop that other run's database.
CREATED_DB=false
cleanup() {
  if [ -n "$BROKER_PID" ] && kill -0 "$BROKER_PID" 2>/dev/null; then
    kill "$BROKER_PID" 2>/dev/null || true
    wait "$BROKER_PID" 2>/dev/null || true
  fi
  if [ "$CREATED_DB" = true ]; then
    # Best-effort: cleanup runs on every exit path and must never itself fail, even if the server
    # is already gone or the drop fails for some other reason.
    admin_psql -c "drop database if exists ${DB_NAME};" 2>/dev/null || true
    echo "dev-broker: dropped this instance's database $DB_NAME in $POSTGRES_WHERE" >&2
  fi
  echo "dev-broker: workdir kept at $WORK_DIR (rm -rf it once its helper has stopped)" >&2
}
trap cleanup EXIT

echo "dev-broker: workdir $WORK_DIR" >&2

if [ -z "${DEV_BROKER_POSTGRES_URL:-}" ]; then
  # --- Postgres: dev-postgres.sh has no idempotency guard of its own, so check first. ---
  if docker ps -a --format '{{.Names}}' | grep -qx "$POSTGRES_CONTAINER"; then
    if ! docker ps --format '{{.Names}}' | grep -qx "$POSTGRES_CONTAINER"; then
      echo "dev-broker: starting existing $POSTGRES_CONTAINER container..." >&2
      docker start "$POSTGRES_CONTAINER" >/dev/null
    else
      echo "dev-broker: $POSTGRES_CONTAINER is already running, reusing it" >&2
    fi
  else
    echo "dev-broker: booting Postgres via dev-postgres.sh..." >&2
    "$SCRIPT_DIR/dev-postgres.sh" >&2
  fi
  for _ in $(seq 1 30); do
    if docker exec "$POSTGRES_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  if ! docker exec "$POSTGRES_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
    echo "dev-broker: $POSTGRES_CONTAINER never became ready" >&2
    exit 1
  fi
fi

# --- Create this instance's own isolated database (never the shared "dispatch" database). ---
admin_psql -c "create database ${DB_NAME};"
CREATED_DB=true
echo "dev-broker: created isolated database $DB_NAME" >&2

# A dbname= in DEV_BROKER_POSTGRES_URL's query string overrides the URL's own database, which
# would point the broker, and its migrations, at a database this run neither created nor drops.
if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ]; then
  connected="$(psql "$POSTGRES_URL" -v ON_ERROR_STOP=1 -Atqc 'select current_database()')"
  if [ "$connected" != "$DB_NAME" ]; then
    echo "dev-broker: refused: the broker's database URL connects to '$connected', not this run's $DB_NAME; drop any dbname= from DEV_BROKER_POSTGRES_URL's query string" >&2
    exit 1
  fi
fi
# The banner names the database without any password the URL carries.
DISPLAY_URL="$(printf '%s' "$POSTGRES_URL" | sed -E 's#^(postgres(ql)?://[^:/@?]+):[^@/?]*@#\1:***@#')"

# --- Build the broker and the clients a second shell drives it with. ---
echo "dev-broker: building broker, agent-secrets, agent-secrets-helper and agent-secrets-devrelay..." >&2
mkdir -p "$BIN_DIR"
for cmd in broker agent-secrets agent-secrets-helper agent-secrets-devrelay; do
  ( cd "$ENVOY_DIR" && GOTOOLCHAIN=go1.26.1 go build -o "$BIN_DIR/$cmd" "./cmd/$cmd" )
done

# --- Scratch rules file: one automatic secret, one approval-required secret. ---
cat > "$RULES_FILE" <<EOF
version: 1
secrets:
  DEMO_READ_TOKEN:
    source: example/agent-secrets/DEMO_READ_TOKEN
    owner: ${APPROVER_LOGIN}
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: box, operator: ${APPROVER_LOGIN}, decision: automatic}
      - {kind: host, operator: ${APPROVER_LOGIN}, decision: automatic}
  DEMO_API_KEY:
    source: example/agent-secrets/DEMO_API_KEY
    owner: ${APPROVER_LOGIN}
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: box, operator: ${APPROVER_LOGIN}, decision: approval, approver: operator}
      - {kind: host, operator: ${APPROVER_LOGIN}, decision: approval, approver: operator}
EOF

cat > "$FAKE_SECRETS_FILE" <<EOF
example/agent-secrets/DEMO_READ_TOKEN=demo-read-token-value
example/agent-secrets/DEMO_API_KEY=demo-api-key-value
EOF
chmod 600 "$FAKE_SECRETS_FILE"
echo "$APPROVER_LOGIN" >"$OPERATOR_FILE"

# --- Start the broker: BROKER_LISTEN_ADDR/BROKER_PUBLIC_URL of port 0 (above); its own log names
# the real address once Listen succeeds. Output is teed to BROKER_LOG (read below) and to this
# script's own stderr, so a human watching this script still sees the broker's own request logs
# live. ---
echo "dev-broker: starting broker..." >&2
: >"$BROKER_LOG"
BROKER_DATABASE_URL="$POSTGRES_URL" \
BROKER_PUBLIC_URL="$PUBLIC_URL" \
BROKER_UI_TOKEN="$UI_TOKEN" \
BROKER_RULES_FILE="$RULES_FILE" \
BROKER_FAKE_SECRETS_FILE="$FAKE_SECRETS_FILE" \
BROKER_LISTEN_ADDR="$LISTEN_ADDR" \
"$BROKER_BIN" > >(tee -a "$BROKER_LOG" >&2) 2>&1 &
BROKER_PID=$!

# --- Both readiness loops below poll this instance's own process; fail loudly the moment it
# dies instead of looping until either wait's own timeout finally gives up. ---
die_if_broker_exited() {
  if ! kill -0 "$BROKER_PID" 2>/dev/null; then
    echo "dev-broker: broker exited during startup" >&2
    wait "$BROKER_PID" || true
    exit 1
  fi
}

# --- Wait for the broker's own "broker listening" log line: only once Listen has
# actually succeeded does the broker report an address, so this can only ever name this
# instance's own listener, never a different, already-running one. ---
BOUND_ADDR=""
for _ in $(seq 1 30); do
  die_if_broker_exited
  # `|| true`: under this script's own set -o pipefail, grep -o finding nothing yet (the normal
  # case on every iteration before the broker has logged its bound address) exits 1, and since
  # head/cut on empty input both exit 0, that 1 becomes the whole pipeline's status — which would
  # trip this script's own errexit and abort before the loop ever gets to sleep and retry.
  BOUND_ADDR="$(grep -o 'addr=[^ ]*' "$BROKER_LOG" 2>/dev/null | head -n1 | cut -d= -f2- || true)"
  if [ -n "$BOUND_ADDR" ]; then
    break
  fi
  sleep 0.5
done
if [ -z "$BOUND_ADDR" ]; then
  echo "dev-broker: broker never logged its bound address within 15s" >&2
  exit 1
fi
PUBLIC_URL="http://${BOUND_ADDR}"
LISTEN_ADDR="$BOUND_ADDR"

ready=false
for _ in $(seq 1 30); do
  die_if_broker_exited
  if curl -sf "${PUBLIC_URL}/healthz" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.5
done
if [ "$ready" != true ]; then
  echo "dev-broker: broker did not become healthy within 15s" >&2
  exit 1
fi

cat <<EOF

dev-broker: ready.

  export AGENT_SECRETS_URL=$PUBLIC_URL
  export AGENT_SECRETS_UI_TOKEN=$UI_TOKEN
  export AGENT_SECRETS_APPROVER=$APPROVER_LOGIN
  export AGENT_SECRETS_HELPER_SOCK=$WORK_DIR/helper.sock
  export AGENT_SECRETS_OPERATOR_FILE=$OPERATOR_FILE
  export DEV_BROKER_DIR=$WORK_DIR
  export PATH=$BIN_DIR:\$PATH

  binaries:            $BIN_DIR  (broker, agent-secrets, agent-secrets-helper, agent-secrets-devrelay)
  fake secrets file:   $FAKE_SECRETS_FILE  (source -> value, for confirming a released grant)
  rules file:          $RULES_FILE
  approver login:      $APPROVER_LOGIN (devrelay approve/deny --login \$AGENT_SECRETS_APPROVER decides as this human)
  database:            $DISPLAY_URL  (this instance's own; created and dropped by this script)

  One automatic secret (DEMO_READ_TOKEN) and one approval-required secret (DEMO_API_KEY,
  approver: $APPROVER_LOGIN) are configured for a box or host session $APPROVER_LOGIN operates.
  Drive it with agent-secrets, agent-secrets-helper and agent-secrets-devrelay in another shell;
  press Ctrl-C here to stop the broker.
EOF

wait "$BROKER_PID"
