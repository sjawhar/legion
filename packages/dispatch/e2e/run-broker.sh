#!/usr/bin/env bash
set -euo pipefail
# The REAL secrets broker e2e/broker-approval.e2e.ts approves requests on, and the issuer it trusts,
# as two harness listeners (e2e/harness-listeners.ts):
#
#   run-broker.sh issuer   cmd/dev-oidc-issuer on DEV_OIDC_ISSUER_PORT (default 9025)
#   run-broker.sh broker   cmd/broker on REAL_BROKER_PORT (default 9026)
#
# The issuer stands in for two things: the cluster's ServiceAccount token issuer, whose POST
# /dev/mint mints the tokens the seeding step (packages/envoy/internal/broker/brokertest/requestseed)
# logs a service in and enrolls its pods with, and the person's sign-in provider for the broker's
# own client, signing a person in with no page to fill in: the one POST /dev/sign-in-as queued,
# else the authorization request's login_hint (the record's approver), else alice. Dispatch's
# server never reaches this broker: it keeps relaying e2e/fake-broker.ts, whose pending rows a row
# points at this broker's records, so only the browser and the seeding step speak to it.
#
# The ports resolve as e2e/harness-ports.ts resolves them (${VAR:-default}). DATABASE_URL is the
# Dispatch suite's own database, which e2e/seed.ts truncates; the broker never touches it, and
# runs instead in a database of its own on the same server, <that database>_broker, dropped and
# created afresh on every start so each harness begins with no enrollment, request or session.
: "${DATABASE_URL:?DATABASE_URL must name an isolated Dispatch e2e database}"
mode="${1:-}"
e2e_port="${DISPATCH_E2E_PORT:-8777}"
issuer_port="${DEV_OIDC_ISSUER_PORT:-9025}"
broker_port="${REAL_BROKER_PORT:-9026}"
issuer_url="http://127.0.0.1:$issuer_port"
e2e_dir="$(cd "$(dirname "$0")" && pwd)"

# The broker's own OpenID Connect client at the issuer, whose secret both processes read from a
# file: a throwaway value committed beside this script, as e2e/fake-broker-account-token is. Every
# signed-in person's hd claim names the domain the broker requires.
signin_client=dispatch-e2e-broker
signin_secret_file="$e2e_dir/broker-signin-client-secret"
signin_domain=example.com

# The accounts the broker lists and the audience it takes their tokens for. requestseed names the
# same launcher service, launcher account, pod account and audience as its own constants.
pod_audience=example-broker-pod
launcher_service=example-launcher
launcher_account=system:serviceaccount:example-system:example-launcher
dispatch_account=system:serviceaccount:example-ns:example-dispatch

# The namespace and key the fake secrets sit under; the key is an example ARN in AWS's
# documentation account, and nothing here calls AWS.
secrets_prefix=example/agent-secrets/
secrets_kms_key_arn=arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab

case "$mode" in
  issuer | broker) ;;
  *)
    echo "run-broker.sh: want issuer or broker, not '$mode'" >&2
    exit 2
    ;;
esac

cd "$e2e_dir/../../envoy"
# Built first and exec'd, as run-server.sh does its server, so the pid Playwright started is the
# listener's. One build of the three binaries per checkout, under a lock, so the two listeners
# starting at once build them once: the second waits, and Go rewrites nothing that did not change.
# out/ is ignored, so nothing built here is tracked.
bin=out/dispatch-e2e-broker
mkdir -p "$bin"
flock "$bin/.build.lock" go build -o "$bin/" ./cmd/broker ./cmd/dev-oidc-issuer ./internal/broker/brokertest/requestseed

if [ "$mode" = issuer ]; then
  exec "$bin/dev-oidc-issuer" \
    --listen "127.0.0.1:$issuer_port" \
    --sign-in-client "$signin_client" \
    --sign-in-client-secret-file "$signin_secret_file" \
    --sign-in-domain "$signin_domain" \
    --sign-in-default "alice@$signin_domain"
fi

# The broker's database, beside DATABASE_URL's on the same server. psql runs without PGHOSTADDR,
# as e2e/psql.ts runs it: libpq would otherwise connect to that address instead of the URL's host.
if ! [[ "$DATABASE_URL" =~ ^(postgres(ql)?://[^/?]+)/([^/?]+)(\?.*)?$ ]]; then
  echo "run-broker.sh: DATABASE_URL must be postgres://<user>@<host>:<port>/<database>" >&2
  exit 2
fi
broker_database="${BASH_REMATCH[3]}_broker"
broker_database_url="${BASH_REMATCH[1]}/${broker_database}${BASH_REMATCH[4]}"
env -u PGHOSTADDR psql "$DATABASE_URL" -X -q -v ON_ERROR_STOP=1 \
  -c "drop database if exists \"$broker_database\" with (force)" \
  -c "create database \"$broker_database\""

# The fake Secrets Manager: a human-tier secret each of the suite's people owns, which a pod's
# request waits on that person to approve, and a shared one anyone signed in may approve.
fake_secrets_file="$PWD/$bin/fake-secrets-$broker_port.json"
(
  umask 077
  cat >"$fake_secrets_file" <<EOF
{"secrets": [
  {"name": "${secrets_prefix}alice-deploy-key", "kms_key_id": "${secrets_kms_key_arn}",
   "tags": {"owner": "alice@${signin_domain}", "tier": "human"}, "value": "alice-deploy-value"},
  {"name": "${secrets_prefix}bob-deploy-key", "kms_key_id": "${secrets_kms_key_arn}",
   "tags": {"owner": "bob@${signin_domain}", "tier": "human"}, "value": "bob-deploy-value"},
  {"name": "${secrets_prefix}shared-deploy-key", "kms_key_id": "${secrets_kms_key_arn}",
   "tags": {"owner": "shared", "tier": "human"}, "value": "shared-deploy-value"}
]}
EOF
)

# The broker discovers its token issuer before it listens and refuses to boot without it, and
# Playwright starts both listeners at once, so it waits for the issuer here.
for _ in $(seq 1 120); do
  curl -sf -o /dev/null "$issuer_url/.well-known/openid-configuration" && break
  sleep 0.5
done
curl -sf -o /dev/null "$issuer_url/.well-known/openid-configuration" || {
  echo "run-broker.sh: the dev issuer at $issuer_url did not answer within 60 s" >&2
  exit 1
}

# The broker is a test fixture: it inherits no BROKER_* setting from the shell that starts
# Playwright, and no home directory. BROKER_DRAIN_SECONDS=0 stops it at once, with no load
# balancer to drain from. BROKER_MACHINE_LOGIN_URL is the harness Dispatch's machine-login page.
mapfile -t inherited < <(compgen -e)
for name in "${inherited[@]}"; do
  case "$name" in
    BROKER_*) unset "$name" ;;
  esac
done
exec env \
  BROKER_DATABASE_URL="$broker_database_url" \
  BROKER_LISTEN_ADDR="127.0.0.1:$broker_port" \
  BROKER_PUBLIC_URL="http://127.0.0.1:$broker_port" \
  BROKER_MACHINE_LOGIN_URL="http://127.0.0.1:$e2e_port/credentials/machine" \
  BROKER_SECRETS_PREFIX="$secrets_prefix" \
  BROKER_SECRETS_KMS_KEY_ARN="$secrets_kms_key_arn" \
  BROKER_FAKE_SECRETS_FILE="$fake_secrets_file" \
  BROKER_DRAIN_SECONDS=0 \
  BROKER_K8S_OIDC_ISSUER="$issuer_url" \
  BROKER_K8S_OIDC_AUDIENCE="$pod_audience" \
  BROKER_SERVICE_LOGINS="$launcher_service=$launcher_account" \
  BROKER_API_ACCOUNTS="dispatch=$dispatch_account" \
  BROKER_SIGNIN_ISSUER="$issuer_url" \
  BROKER_SIGNIN_CLIENT_ID="$signin_client" \
  BROKER_SIGNIN_CLIENT_SECRET_FILE="$signin_secret_file" \
  BROKER_SIGNIN_DOMAIN="$signin_domain" \
  HOME=/nonexistent \
  XDG_CACHE_HOME=/nonexistent \
  XDG_CONFIG_HOME=/nonexistent \
  XDG_DATA_HOME=/nonexistent \
  "$bin/broker"
