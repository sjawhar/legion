#!/usr/bin/env bash
# packages/envoy/scripts/dev-broker.sh
#
# AGENTC-393 Task 12: the local dev surface a human can drive without AWS or a real YubiKey. Boots
# Postgres (dev-postgres.sh, made idempotent here since that script has no guard of its own),
# generates one software approver identity with agent-secrets-devkey (its "register --seed" mode —
# the offline, no-live-broker-yet bootstrap), writes a scratch rules file (one automatic and one
# approval-required secret, an approvers section seeding that identity) and a fake secrets file,
# inserts the matching approver_key_seeds row directly (contract v9 ruling 9's break-glass path —
# the running broker can never do this for its own first key), then starts cmd/broker against all
# of it with -dev-attestation-root trusting devkey's own generated test CA instead of the embedded
# Yubico roots. Prints the exports a second shell needs to drive agent-secrets-devkey against it.
#
# Each invocation creates and drops its own isolated Postgres database inside the shared
# dispatch-pg container (named from this run's own WORK_DIR, below) and binds an OS-assigned
# ephemeral port (BROKER_LISTEN_ADDR=127.0.0.1:0), so two agent sessions each running their own
# dev broker stack never contend: every instance seeds a fresh software key for the same
# hardcoded APPROVER_LOGIN and writes its own scratch rules file naming only its own key, and
# approvers.Service.Reconcile tombstones any persisted approver_keys row a currently-loaded rules
# file doesn't name — sharing one database, each instance's reload would tombstone the other's
# live key out from under it. cmd/broker (AGENTC-833) binds before it reports anything and logs
# the address it actually bound; this script waits for that line and reads the real port from it,
# so a curl success can only ever mean this instance's own broker answered.
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
UI_ORIGIN="${BROKER_UI_ORIGIN:-https://agent-secrets.invalid}"
UI_TOKEN="${BROKER_UI_TOKEN:-dev}"
APPROVER_LOGIN="sjawhar"

WORK_DIR="$(mktemp -d /tmp/agent-secrets-dev.XXXXXX)"
STATE_DIR="$WORK_DIR/devkey-state"
DEVKEY_BIN="$WORK_DIR/agent-secrets-devkey"
BROKER_BIN="$WORK_DIR/broker"
CA_PEM="$WORK_DIR/dev-attestation-root.pem"
RULES_FILE="$WORK_DIR/agent-secret-rules.yaml"
FAKE_SECRETS_FILE="$WORK_DIR/fake-secrets.env"
BROKER_LOG="$WORK_DIR/broker.log"
# This instance's own database, named from WORK_DIR's mktemp-generated random suffix (already
# unique per invocation). Postgres unquoted identifiers fold to lowercase anyway, but lowercase
# explicitly rather than rely on that.
DB_NAME="dev_broker_$(printf '%s' "${WORK_DIR##*.}" | tr '[:upper:]' '[:lower:]')"
POSTGRES_URL="postgres://postgres:dispatch@127.0.0.1:55432/${DB_NAME}?sslmode=disable"

BROKER_PID=""
cleanup() {
  if [ -n "$BROKER_PID" ] && kill -0 "$BROKER_PID" 2>/dev/null; then
    kill "$BROKER_PID" 2>/dev/null || true
    wait "$BROKER_PID" 2>/dev/null || true
  fi
  # Best-effort: cleanup runs on every exit path and must never itself fail, even if the container
  # is already gone or the drop fails for some other reason.
  docker exec "$POSTGRES_CONTAINER" psql -U postgres -v ON_ERROR_STOP=1 -q \
    -c "drop database if exists ${DB_NAME};" 2>/dev/null || true
  echo "dev-broker: workdir kept at $WORK_DIR; dropped this instance's database $DB_NAME (the shared $POSTGRES_CONTAINER container and any other instance's database are left running; 'docker stop $POSTGRES_CONTAINER' to tear down the shared container)" >&2
}
trap cleanup EXIT

echo "dev-broker: workdir $WORK_DIR" >&2

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

# --- Create this instance's own isolated database (never the shared "dispatch" database). ---
echo "dev-broker: created isolated database $DB_NAME" >&2
docker exec "$POSTGRES_CONTAINER" psql -U postgres -v ON_ERROR_STOP=1 -q -c "create database ${DB_NAME};"

# --- Build the two binaries this stack needs. ---
echo "dev-broker: building agent-secrets-devkey and broker..." >&2
( cd "$ENVOY_DIR" && GOTOOLCHAIN=go1.26.8 go build -o "$DEVKEY_BIN" ./cmd/agent-secrets-devkey )
( cd "$ENVOY_DIR" && GOTOOLCHAIN=go1.26.8 go build -o "$BROKER_BIN" ./cmd/broker )

# --- Migrate the fresh database before seeding it. A freshly created database has no schema yet,
# but the break-glass approver_key_seeds insert below must land before the broker's own first
# rules load: rules.NewCurrent's first load is synchronous and fatal on failure, and
# approvers.Service.Reconcile refuses a "seed: true" rules-file key with no matching row, so the
# table must exist and hold the row before "$BROKER_BIN" ever starts for real. -migrate-only runs
# the same store.Migrate the real boot runs (idempotent; the real boot below runs it again as a
# no-op) and exits without loading rules or serving. ---
echo "dev-broker: migrating $DB_NAME..." >&2
BROKER_DATABASE_URL="$POSTGRES_URL" "$BROKER_BIN" -migrate-only

# --- Seed one software approver identity (offline: no live broker yet). ---
echo "dev-broker: seeding a software approver key for $APPROVER_LOGIN..." >&2
SEED_JSON="$("$DEVKEY_BIN" register --seed --login "$APPROVER_LOGIN" --origin "$UI_ORIGIN" --state "$STATE_DIR")"
CREDENTIAL_ID="$(printf '%s' "$SEED_JSON" | jq -r '.credential_id')"
AAGUID="$(printf '%s' "$SEED_JSON" | jq -r '.aaguid')"
NONCE="$(printf '%s' "$SEED_JSON" | jq -r '.challenge_nonce')"
REGISTRATION="$(printf '%s' "$SEED_JSON" | jq -c '.registration')"
printf '%s' "$SEED_JSON" | jq -r '.ca_pem' > "$CA_PEM"

# --- Scratch rules file: one automatic secret, one approval-required secret, the seeded key. ---
cat > "$RULES_FILE" <<EOF
version: 1
secrets:
  AGENT_SECRETS_PROOF_AUTOMATIC:
    source: dev/agent-secrets/AGENT_SECRETS_PROOF_AUTOMATIC
    owner: ${APPROVER_LOGIN}
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: box, operator: ${APPROVER_LOGIN}, decision: automatic}
  AGENT_SECRETS_PROOF_APPROVAL:
    source: dev/agent-secrets/AGENT_SECRETS_PROOF_APPROVAL
    owner: ${APPROVER_LOGIN}
    delivery: inject
    max_lifetime_seconds: 3600
    requesters:
      - {kind: box, operator: ${APPROVER_LOGIN}, decision: approval, approver: operator}
approvers:
  origin: ${UI_ORIGIN}
  aaguids: ["${AAGUID}"]
  logins:
    ${APPROVER_LOGIN}:
      keys:
        - credential_id: "${CREDENTIAL_ID}"
          registration:
            challenge_nonce: "${NONCE}"
            response: ${REGISTRATION}
          seed: true
EOF

cat > "$FAKE_SECRETS_FILE" <<EOF
dev/agent-secrets/AGENT_SECRETS_PROOF_AUTOMATIC=automatic-dev-value
dev/agent-secrets/AGENT_SECRETS_PROOF_APPROVAL=approval-dev-value
EOF
chmod 600 "$FAKE_SECRETS_FILE"

# --- Break-glass seed row (ruling 9): only an admin (here, this script) can seed a login's first
# key; the running broker's own Reconcile refuses a "seed: true" file key with no matching row. ---
echo "dev-broker: inserting approver_key_seeds row..." >&2
docker exec -i "$POSTGRES_CONTAINER" psql -U postgres -d "$DB_NAME" -v ON_ERROR_STOP=1 -q \
  -c "insert into approver_key_seeds (login, credential_id) values ('${APPROVER_LOGIN}', '${CREDENTIAL_ID}') on conflict do nothing;"

# --- Start the broker: BROKER_LISTEN_ADDR/BROKER_PUBLIC_URL of port 0 (above); its own log names
# the real address once Listen succeeds. Output is teed to BROKER_LOG (read below) and to this
# script's own stderr, so a human watching this script still sees the broker's own request logs
# live. ---
echo "dev-broker: starting broker..." >&2
: >"$BROKER_LOG"
BROKER_DATABASE_URL="$POSTGRES_URL" \
BROKER_PUBLIC_URL="$PUBLIC_URL" \
BROKER_UI_ORIGIN="$UI_ORIGIN" \
BROKER_UI_TOKEN="$UI_TOKEN" \
BROKER_RULES_FILE="$RULES_FILE" \
BROKER_FAKE_SECRETS_FILE="$FAKE_SECRETS_FILE" \
BROKER_LISTEN_ADDR="$LISTEN_ADDR" \
"$BROKER_BIN" -dev-attestation-root "$CA_PEM" > >(tee -a "$BROKER_LOG" >&2) 2>&1 &
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

# --- Wait for the broker's own "broker listening" log line (AGENTC-833): only once Listen has
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
  export AGENT_SECRETS_UI_ORIGIN=$UI_ORIGIN
  export AGENT_SECRETS_DEVKEY_STATE=$STATE_DIR

  devkey binary:       $DEVKEY_BIN
  fake secrets file:   $FAKE_SECRETS_FILE  (source -> value, for confirming a released grant)
  rules file:          $RULES_FILE
  approver login:      $APPROVER_LOGIN (its software key's material lives at \$AGENT_SECRETS_DEVKEY_STATE)
  database:            $POSTGRES_URL  (this instance's own; created and dropped by this script)

  One automatic secret (AGENT_SECRETS_PROOF_AUTOMATIC) and one approval-required secret
  (AGENT_SECRETS_PROOF_APPROVAL, approver: $APPROVER_LOGIN) are configured. Drive it with
  agent-secrets and \$DEVKEY_BIN in another shell; press Ctrl-C here to stop the broker.
EOF

wait "$BROKER_PID"
