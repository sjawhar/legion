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
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

POSTGRES_CONTAINER="dispatch-pg"
POSTGRES_URL="postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable"

LISTEN_ADDR="127.0.0.1:13380"
PUBLIC_URL="http://${LISTEN_ADDR}"
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

BROKER_PID=""
cleanup() {
  if [ -n "$BROKER_PID" ] && kill -0 "$BROKER_PID" 2>/dev/null; then
    kill "$BROKER_PID" 2>/dev/null || true
    wait "$BROKER_PID" 2>/dev/null || true
  fi
  echo "dev-broker: workdir kept at $WORK_DIR (Postgres left running; 'docker stop $POSTGRES_CONTAINER' to tear it down)" >&2
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

# --- Build the two binaries this stack needs. ---
echo "dev-broker: building agent-secrets-devkey and broker..." >&2
( cd "$ENVOY_DIR" && GOTOOLCHAIN=go1.26.1 go build -o "$DEVKEY_BIN" ./cmd/agent-secrets-devkey )
( cd "$ENVOY_DIR" && GOTOOLCHAIN=go1.26.1 go build -o "$BROKER_BIN" ./cmd/broker )

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
docker exec -i "$POSTGRES_CONTAINER" psql -U postgres -d dispatch -v ON_ERROR_STOP=1 -q \
  -c "insert into approver_key_seeds (login, credential_id) values ('${APPROVER_LOGIN}', '${CREDENTIAL_ID}') on conflict do nothing;"

# --- Start the broker. ---
echo "dev-broker: starting broker on $LISTEN_ADDR..." >&2
BROKER_DATABASE_URL="$POSTGRES_URL" \
BROKER_PUBLIC_URL="$PUBLIC_URL" \
BROKER_UI_ORIGIN="$UI_ORIGIN" \
BROKER_UI_TOKEN="$UI_TOKEN" \
BROKER_RULES_FILE="$RULES_FILE" \
BROKER_FAKE_SECRETS_FILE="$FAKE_SECRETS_FILE" \
BROKER_LISTEN_ADDR="$LISTEN_ADDR" \
"$BROKER_BIN" -dev-attestation-root "$CA_PEM" &
BROKER_PID=$!

ready=false
for _ in $(seq 1 30); do
  if ! kill -0 "$BROKER_PID" 2>/dev/null; then
    echo "dev-broker: broker exited during startup" >&2
    wait "$BROKER_PID" || true
    exit 1
  fi
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
  database:            $POSTGRES_URL

  One automatic secret (AGENT_SECRETS_PROOF_AUTOMATIC) and one approval-required secret
  (AGENT_SECRETS_PROOF_APPROVAL, approver: $APPROVER_LOGIN) are configured. Drive it with
  agent-secrets and \$DEVKEY_BIN in another shell; press Ctrl-C here to stop the broker.
EOF

wait "$BROKER_PID"
