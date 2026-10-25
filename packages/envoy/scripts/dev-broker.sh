#!/usr/bin/env bash
# packages/envoy/scripts/dev-broker.sh
#
# The local dev surface a human can drive without AWS, Dispatch or a cluster. Boots Postgres
# (the dispatch-pg container, through dev-postgres.sh), builds the
# broker, its three clients (agent-secrets, agent-secrets-helper and agent-secrets-devrelay) and
# dev-oidc-issuer, writes a fake secrets file standing in for Secrets Manager (DEMO_READ_TOKEN,
# tagged owner=APPROVER_LOGIN tier=agent, so APPROVER_LOGIN's sessions get it without asking, and
# DEMO_API_KEY, tagged tier=human, which APPROVER_LOGIN approves), then starts cmd/broker against
# it. Prints the exports a second shell needs to drive the clients against it.
#
# The broker's approval routes, and its introspection route, take the calling service's own
# Kubernetes ServiceAccount token (BROKER_API_ACCOUNTS, verified against BROKER_K8S_OIDC_ISSUER),
# and so does the RFC 8693 token exchange's subject token. With no cluster to mint one, this
# script starts cmd/dev-oidc-issuer, the Go tests' local issuer (internal/oidc/oidctest) served on
# a loopback port, which keeps two tokens in WORK_DIR and rewrites each before it lapses, as a
# kubelet does a pod's projected token: account-token, for API_ACCOUNT, the caller "dispatch" the
# broker lists, which agent-secrets-devrelay presents in Dispatch's place to decide a machine
# login; and exchange-token, for EXCHANGE_ACCOUNT's pod, which the broker's exchange takes for a
# Dispatch access token. The broker trusts that issuer for both.
#
# A secret request is approved on the broker's own page, <broker>/approve/<record>, by a person
# signed in to the broker (BROKER_SIGNIN_*). The same issuer process stands in for that person's
# identity provider: it serves the broker's sign-in client, whose secret this script generates
# into WORK_DIR, and signs a person in as the request's login_hint (the approver) with no page to
# fill in, so a browser on this machine approves a secret request on that page.
#
# Each invocation creates and drops its own isolated Postgres database (named from this run's own
# WORK_DIR, below) and binds an OS-assigned ephemeral port (BROKER_LISTEN_ADDR=127.0.0.1:0), so two
# agent sessions each running their own dev broker stack never see each other's enrollments,
# requests or grants. The database lives in the shared dispatch-pg container, which
# dev-postgres.sh creates, starts or reuses, and refuses when it runs an image other than that
# script's pinned pgvector one, or, with DEV_BROKER_POSTGRES_URL set
# (postgres://<user>@<host>:<port>/<database>, a role that may create databases), on that server
# through psql, with no Docker at all. cmd/broker binds before it reports anything and
# logs the address it actually bound; this script waits for that line and reads the real port from
# it, so a curl success can only ever mean this instance's own broker answered. The broker is
# started with BROKER_DRAIN_SECONDS=0 so this stack's cleanup stays instant: the default
# five-second drain exists to outlast a Kubernetes EndpointSlice propagation nothing here has, so
# here it would be pure delay on every Ctrl-C and every restart.
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
FAKE_SECRETS_FILE="$WORK_DIR/fake-secrets.json"
# The namespace and the agent-secrets key the fake secrets sit under; the key is an example ARN in
# AWS's documentation account, and nothing here calls AWS.
SECRETS_PREFIX="example/agent-secrets/"
SECRETS_KMS_KEY_ARN="arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
BROKER_LOG="$WORK_DIR/broker.log"
OPERATOR_FILE="$WORK_DIR/operator"
# The local issuer and what it mints for (above): the audience every enrolment and listed caller's
# token carries, the exchange's own audience, and the two accounts, as internal/broker/brokertest's
# rig names them. The exchange entry fixes no session, so a pod names its own.
ISSUER_BIN="$BIN_DIR/dev-oidc-issuer"
ISSUER_LOG="$WORK_DIR/issuer.log"
BROKER_AUDIENCE="legion-broker-pod"
EXCHANGE_AUDIENCE="legion-broker-exchange"
API_ACCOUNT="system:serviceaccount:example-ns:example-dispatch"
EXCHANGE_ACCOUNT="system:serviceaccount:example-ns:example-worker"
EXCHANGE_PROJECT="EXAMPLE"
ACCOUNT_TOKEN_FILE="$WORK_DIR/account-token"
EXCHANGE_TOKEN_FILE="$WORK_DIR/exchange-token"
# The broker's own sign-in client at that issuer (BROKER_SIGNIN_*), through which a person signs in
# to the broker's approval page, the file holding its secret (generated below), and the domain
# every signed-in person's hd claim names, the approver's own.
SIGNIN_CLIENT_ID="dev-broker"
SIGNIN_SECRET_FILE="$WORK_DIR/signin-client-secret"
SIGNIN_DOMAIN="example.com"
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
ISSUER_PID=""
# Set once this run's own create database succeeded: a run whose create failed (the name taken by
# another run) must never drop that other run's database.
CREATED_DB=false
cleanup() {
  # The broker first, then the issuer whose keys it reads.
  local pid
  for pid in "$BROKER_PID" "$ISSUER_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
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
  # --- Postgres: dev-postgres.sh creates, starts or reuses dispatch-pg, and refuses one on an
  # image other than its pinned one before this run creates anything. ---
  echo "dev-broker: Postgres via dev-postgres.sh..." >&2
  "$SCRIPT_DIR/dev-postgres.sh" >&2
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

# --- Build the broker, the local issuer, and the clients a second shell drives it with. ---
echo "dev-broker: building broker, dev-oidc-issuer, agent-secrets, agent-secrets-helper and agent-secrets-devrelay..." >&2
mkdir -p "$BIN_DIR"
for cmd in broker dev-oidc-issuer agent-secrets agent-secrets-helper agent-secrets-devrelay; do
  ( cd "$ENVOY_DIR" && GOTOOLCHAIN=go1.27.1 go build -o "$BIN_DIR/$cmd" "./cmd/$cmd" )
done

# --- Fake Secrets Manager: one agent-tier secret its owner's sessions get without asking, one
# human-tier secret its owner approves. ---
cat > "$FAKE_SECRETS_FILE" <<EOF
{"secrets": [
  {"name": "${SECRETS_PREFIX}demo-read-token", "kms_key_id": "${SECRETS_KMS_KEY_ARN}",
   "tags": {"owner": "${APPROVER_LOGIN}", "tier": "agent"}, "value": "demo-read-token-value"},
  {"name": "${SECRETS_PREFIX}demo-api-key", "kms_key_id": "${SECRETS_KMS_KEY_ARN}",
   "tags": {"owner": "${APPROVER_LOGIN}", "tier": "human"}, "value": "demo-api-key-value"}
]}
EOF
chmod 600 "$FAKE_SECRETS_FILE"
echo "$APPROVER_LOGIN" >"$OPERATOR_FILE"

# --- The broker's sign-in client's secret, which the issuer and the broker each read from this
# file (neither takes it on its command line or in its environment). Written under umask 077, so
# it is never readable by anyone else, even for a moment. ---
( umask 077 && od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$SIGNIN_SECRET_FILE" )

# --- Start the local issuer, teed like the broker below. It writes both token files before it
# logs its URL, so once that line is in ISSUER_LOG the files are there for devrelay and a pod to
# read, and the URL is the issuer the broker discovers at boot, for ServiceAccount tokens and for
# a person's sign-in alike. A sign-in that names no login_hint (a shared secret's, whose approver
# is anyone signed in) signs in as APPROVER_LOGIN. ---
echo "dev-broker: starting dev-oidc-issuer..." >&2
: >"$ISSUER_LOG"
"$ISSUER_BIN" \
  --token "$BROKER_AUDIENCE=$API_ACCOUNT=$ACCOUNT_TOKEN_FILE" \
  --token "$EXCHANGE_AUDIENCE=$EXCHANGE_ACCOUNT=$EXCHANGE_TOKEN_FILE" \
  --sign-in-client "$SIGNIN_CLIENT_ID" \
  --sign-in-client-secret-file "$SIGNIN_SECRET_FILE" \
  --sign-in-domain "$SIGNIN_DOMAIN" \
  --sign-in-default "$APPROVER_LOGIN" \
  > >(trap '' TERM INT; exec tee -a "$ISSUER_LOG" >&2) 2>&1 &
ISSUER_PID=$!
ISSUER_URL=""
for _ in $(seq 1 30); do
  if ! kill -0 "$ISSUER_PID" 2>/dev/null; then
    echo "dev-broker: dev-oidc-issuer exited during startup" >&2
    wait "$ISSUER_PID" || true
    exit 1
  fi
  # `|| true`, as for the broker's address below: no line yet is the normal case, not a failure.
  ISSUER_URL="$(grep -o 'issuer listening url=[^ ]*' "$ISSUER_LOG" 2>/dev/null | head -n1 | cut -d= -f2- || true)"
  if [ -n "$ISSUER_URL" ]; then
    break
  fi
  sleep 0.5
done
if [ -z "$ISSUER_URL" ]; then
  echo "dev-broker: dev-oidc-issuer never logged its URL within 15s" >&2
  exit 1
fi

# --- Start the broker: BROKER_LISTEN_ADDR/BROKER_PUBLIC_URL of port 0 (above); its own log names
# the real address once Listen succeeds. Output is teed to BROKER_LOG (read below) and to this
# script's own stderr, so a human watching this script sees the broker's log lines live (it keeps
# no per-request access log). tee ignores SIGTERM and SIGINT: a signal to this script's process
# group (Ctrl-C, or a kill of the group) reaches the broker and tee together,
# and a tee that died first would leave the broker writing its shutdown into a closed pipe and
# dying of SIGPIPE before it drained. tee ends once the broker exits and closes the pipe. ---
echo "dev-broker: starting broker..." >&2
: >"$BROKER_LOG"
# BROKER_MACHINE_LOGIN_URL is a placeholder: no Dispatch runs here, and devrelay stands in for it
# (it approves a login by its code without a page). BROKER_DRAIN_SECONDS=0 turns the drain off
# (see the header).
BROKER_DATABASE_URL="$POSTGRES_URL" \
BROKER_PUBLIC_URL="$PUBLIC_URL" \
BROKER_MACHINE_LOGIN_URL="http://127.0.0.1/credentials/machine" \
BROKER_SECRETS_PREFIX="$SECRETS_PREFIX" \
BROKER_SECRETS_KMS_KEY_ARN="$SECRETS_KMS_KEY_ARN" \
BROKER_FAKE_SECRETS_FILE="$FAKE_SECRETS_FILE" \
BROKER_DRAIN_SECONDS=0 \
BROKER_LISTEN_ADDR="$LISTEN_ADDR" \
BROKER_K8S_OIDC_ISSUER="$ISSUER_URL" \
BROKER_K8S_OIDC_AUDIENCE="$BROKER_AUDIENCE" \
BROKER_API_ACCOUNTS="dispatch=$API_ACCOUNT" \
BROKER_SIGNIN_ISSUER="$ISSUER_URL" \
BROKER_SIGNIN_CLIENT_ID="$SIGNIN_CLIENT_ID" \
BROKER_SIGNIN_CLIENT_SECRET_FILE="$SIGNIN_SECRET_FILE" \
BROKER_SIGNIN_DOMAIN="$SIGNIN_DOMAIN" \
BROKER_EXCHANGE_K8S_AUDIENCE="$EXCHANGE_AUDIENCE" \
BROKER_EXCHANGE_ACCOUNTS="$EXCHANGE_ACCOUNT=$EXCHANGE_PROJECT=$APPROVER_LOGIN" \
"$BROKER_BIN" > >(trap '' TERM INT; exec tee -a "$BROKER_LOG" >&2) 2>&1 &
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
  export AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$ACCOUNT_TOKEN_FILE
  export AGENT_SECRETS_APPROVER=$APPROVER_LOGIN
  export AGENT_SECRETS_HELPER_SOCK=$WORK_DIR/helper.sock
  export AGENT_SECRETS_OPERATOR_FILE=$OPERATOR_FILE
  export AGENT_SECRETS_MACHINE_FILE=$WORK_DIR/machine.json
  export DEV_BROKER_DIR=$WORK_DIR
  export PATH=$BIN_DIR:\$PATH

  binaries:            $BIN_DIR  (broker, dev-oidc-issuer, agent-secrets, agent-secrets-helper, agent-secrets-devrelay)
  fake secrets file:   $FAKE_SECRETS_FILE  (each secret's tags and value, for confirming a released grant)
  approver login:      $APPROVER_LOGIN (whom the issuer signs in to approve a secret, and whom devrelay names deciding a machine login)
  database:            $DISPLAY_URL  (this instance's own; created and dropped by this script)
  token issuer:        $ISSUER_URL  (dev-oidc-issuer, logging to $ISSUER_LOG)
  sign-in:             $ISSUER_URL  (the broker's client $SIGNIN_CLIENT_ID, hd $SIGNIN_DOMAIN; secret in $SIGNIN_SECRET_FILE)
  caller token:        $ACCOUNT_TOKEN_FILE  ($API_ACCOUNT, the caller "dispatch")
  exchange token:      $EXCHANGE_TOKEN_FILE  ($EXCHANGE_ACCOUNT's pod, audience $EXCHANGE_AUDIENCE)

  One agent-tier secret (DEMO_READ_TOKEN, granted without asking) and one human-tier secret
  (DEMO_API_KEY, approver: $APPROVER_LOGIN), both owned by $APPROVER_LOGIN, whose sessions ask
  for them. A grant lasts until its session ends, or for the --ttl its request names.
  Drive it with agent-secrets and agent-secrets-helper in another shell;
  press Ctrl-C here to stop the broker and its issuer.

  To approve a pending request for DEMO_API_KEY, open the approve_url agent-secrets prints for
  it (${PUBLIC_URL}/approve/<record>) in a browser: the broker sends you to sign in at
  dev-oidc-issuer, which signs you in as the request's approver ($APPROVER_LOGIN) with no page to
  fill in, and brings you back to the record's page, where you Approve or Deny it.

  The broker lists the caller token's account as its caller "dispatch" (BROKER_API_ACCOUNTS) and
  trusts dev-oidc-issuer's ServiceAccount tokens (BROKER_K8S_OIDC_ISSUER); the issuer rewrites
  each token file before its token lapses. agent-secrets-devrelay presents the caller token
  (AGENT_SECRETS_ACCOUNT_TOKEN_FILE) to approve or deny a machine login by its code
  (machine-approve) as Dispatch's server does. The RFC 8693 token exchange (POST /v1/oauth/token)
  takes the exchange token as a pod's subject token, with a DPoP proof of the caller's own key and
  a session name of its choosing, and mints an access token that writes $EXCHANGE_PROJECT for
  $APPROVER_LOGIN.
EOF

wait "$BROKER_PID"
