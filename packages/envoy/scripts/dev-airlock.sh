#!/usr/bin/env bash
# packages/envoy/scripts/dev-airlock.sh
#
# The whole local surface the airlock's checks run on, in one command, with no container runtime
# and no cluster. It starts, and prints the exports for:
#
#   - Postgres, dev-oidc-issuer and that script's own broker, by running scripts/dev-broker.sh
#     and reading what it prints. Everything this script needs of a local stack but the airlock
#     itself is there already - the five built binaries, the local token issuer with a pod's,
#     a registrar's and two listed callers' tokens, the broker's own sign-in client, and the
#     Postgres server - so this script starts none of it a second time. What that costs is one
#     broker process this script does not use, on a database of its own; what it buys is that
#     the identities, the token files and the issuer are the ones dev-broker.sh documents, and
#     that a change there reaches both scripts. The airlock's own brokers get a database of
#     their own (below), so they never share a policy or a secret store with that broker.
#   - cmd/dev-airlock-stands: the two HTTPS services S and T, which record every request they
#     get; the stand-in GitHub and Google endpoints a github-app and a google-login secret mint
#     at; the stand-in Microsoft token endpoint the azure kind exchanges at, which also plays
#     the resource host an access token is spent at; and a stand-in AWS service that verifies
#     Signature Version 4. It writes the certificate authority the proxy signs each bound host
#     with, the AWS key pair its own stand verifies, and the GitHub App key its GitHub stand
#     mints for; this script reads all of it out of the JSON report that command prints.
#   - A rules file (BROKER_AIRLOCK_RULES_FILE) and a fake Secrets Manager (the file
#     BROKER_FAKE_SECRETS_FILE names) that name each other: a stored secret bound to S; a second
#     stored secret bound to S with headers and allow of its own; a github-app secret; a
#     google-login secret; an azure-login secret, kind azure, bound to the Microsoft stand-in
#     and the stand-in resource host; and a stored AWS key pair, kind aws, bound to the AWS
#     stand-in.
#   - TWO broker processes, each with the whole BROKER_AIRLOCK_* block, both on one database and
#     each on two ports of its own (--one-broker starts one).
#   - One TCP front over both brokers' API ports and one over both proxy ports
#     (cmd/dev-airlock-stands front), so a client reaches either process. Each new connection
#     goes to the next broker in turn; round-robin per connection is all the checks ask for,
#     since the two brokers share one database and either answers any call. The front is Go
#     because a Legion issue pod has neither socat nor nginx, and nginx passes no CONNECT.
#   - One enrolled pod session: a service login at POST /v1/launcher-credentials/token under the
#     stack's legion-daemon registrar account, then POST /v1/enrollments of kind pod signed with
#     a launcher proof, which is how the Legion daemon enrols a worker pod. agent-secrets-helper
#     cannot be the identity here: it pins the far end of its socket with SO_PEERPIDFD, which a
#     Legion issue pod's gVisor kernel does not have. The enrolment id is written beside the
#     session key, so AGENT_SECRETS_KEY_DIR names the directory and `agent-secrets self` answers
#     kind: pod.
#
# Every bound host is a name of this stack's own (s.airlock.test and the rest), which no
# resolver answers: BROKER_AIRLOCK_TEST_RESOLVE tells the proxy where each one is and
# BROKER_AIRLOCK_TEST_DIAL_ALLOW lets it dial that one loopback address and port, while every
# other loopback, private and link-local address stays refused - which is what check k4 reads.
# Each of them listens on port 443 of a loopback address of its own (127.0.0.2 and up): the
# airlock refuses a request whose Host header is not its CONNECT target, and an HTTP client
# writes the port into that header for every port but 443, so a bound host anywhere else is
# refused "host header mismatch" before it can prove anything. A machine that will not let this
# user bind 443 cannot run this stack; the stands then fail to start and say so.
#
# The airlock's secrets are owned by a registered service (BROKER_SERVICES), and the pod this
# script enrols runs as that service's account, so its requests for them are granted at once. A
# pod session has no operator, so the azure-login secret, which the broker releases its
# projected token for only at human tier with a person's approval, waits: the exports name the
# approver and the sign-in URL for it, and the broker takes that decision on its own page alone.
#
# Everything this script makes lives under ${TMPDIR:-/tmp}, as dev-broker.sh's work directory
# now does: in a Legion pod /tmp is the container's own small disk, and one run of the checks
# fills it.
#
# Options:
#   --keep-values     BROKER_AIRLOCK_TEST_KEEP_VALUES=true (check k5's negative control: the
#                     proxy keeps a grant's values across a reread and past a minted token's
#                     margin)
#   --no-kind         BROKER_AIRLOCK_TEST_NO_KIND=true (check k6's negative control: every rules
#                     entry reads as having no kind, so the azure exchange and the AWS signing
#                     are off)
#   --idle-seconds N  BROKER_AIRLOCK_IDLE_SECONDS (check k9's control sets 60)
#   --one-broker      start one broker rather than two, each front over that one
#   --help            print this and exit
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
  cat <<'EOF'
usage: dev-airlock.sh [--keep-values] [--no-kind] [--idle-seconds N] [--one-broker]

Starts the airlock's whole local stack - dev-broker.sh's Postgres, issuer and broker, the
stand-in services, a certificate authority, a rules file, two brokers behind one API front and
one proxy front, and an enrolled pod session - and prints the exports a check needs.

  --keep-values     BROKER_AIRLOCK_TEST_KEEP_VALUES=true, check k5's negative control
  --no-kind         BROKER_AIRLOCK_TEST_NO_KIND=true, check k6's negative control
  --idle-seconds N  BROKER_AIRLOCK_IDLE_SECONDS, between 10 and 3600; check k9's control sets 60
  --one-broker      start one broker rather than two
  --help            print this
EOF
}

KEEP_VALUES=false
NO_KIND=false
IDLE_SECONDS=300
BROKER_COUNT=2
while [ $# -gt 0 ]; do
  case "$1" in
    --keep-values) KEEP_VALUES=true ;;
    --no-kind) NO_KIND=true ;;
    --idle-seconds)
      if [ $# -lt 2 ] || ! [[ "${2:-}" =~ ^[0-9]+$ ]]; then
        echo "dev-airlock: --idle-seconds wants a number of seconds" >&2
        exit 2
      fi
      IDLE_SECONDS="$2"
      if [ "$IDLE_SECONDS" -lt 10 ] || [ "$IDLE_SECONDS" -gt 3600 ]; then
        echo "dev-airlock: --idle-seconds is between 10 and 3600, the bounds BROKER_AIRLOCK_IDLE_SECONDS takes" >&2
        exit 2
      fi
      shift
      ;;
    --one-broker) BROKER_COUNT=1 ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "dev-airlock: unexpected argument $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

# --- This run's own directory, under TMPDIR as dev-broker.sh's is. ---
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/agent-secrets-airlock.XXXXXX")"
BIN_DIR="$WORK_DIR/bin"
STANDS_BIN="$BIN_DIR/dev-airlock-stands"
STANDS_DIR="$WORK_DIR/stands"
STANDS_JSON="$WORK_DIR/stands.json"
STANDS_LOG="$WORK_DIR/stands.log"
DEV_BROKER_LOG="$WORK_DIR/dev-broker.log"
RULES_FILE="$WORK_DIR/airlock.json"
FAKE_SECRETS_FILE="$WORK_DIR/fake-secrets.json"
AZURE_TOKEN_FILE="$WORK_DIR/azure-identity-token"
KEY_DIR="$WORK_DIR/session-key"
ENROLL_JSON="$WORK_DIR/enrollment.json"
API_FRONT_JSON="$WORK_DIR/api-front.json"
API_FRONT_LOG="$WORK_DIR/api-front.log"
API_BACKENDS="$WORK_DIR/api-backends"
PROXY_FRONT_JSON="$WORK_DIR/proxy-front.json"
PROXY_FRONT_LOG="$WORK_DIR/proxy-front.log"
PROXY_BACKENDS="$WORK_DIR/proxy-backends"

# The namespace and the key this run's fake secrets sit under. The key is an example ARN in AWS's
# documentation account, and nothing here calls AWS; a secret on any other key is refused by the
# policy, so the two must agree.
SECRETS_PREFIX="example/agent-secrets/"
SECRETS_KMS_KEY_ARN="arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
# The one service name this stack uses: the name the registrar account logs in as
# (BROKER_SERVICE_LOGINS), the name the enrolled pod's own account is bound to
# (BROKER_SERVICES), and the owner of the airlock's secrets. A pod's session acts for a service
# only when both halves name that one service, as a deployment's daemon and its worker pods do,
# so a request of the pod's for one of those secrets is granted at once.
SERVICE="legion-daemon"
# The names a check asks for, the stand-in hosts the rules bind them to, and where each one
# listens. Each host is a .test name no resolver answers: BROKER_AIRLOCK_TEST_RESOLVE tells the
# proxy where it is and BROKER_AIRLOCK_TEST_DIAL_ALLOW lets it dial that one address.
#
# Every bound host listens on port 443 of a loopback address of its own, rather than on one
# address and a port the system picks. The airlock refuses a request whose Host header is not
# its CONNECT target, and a client writes the port in that header for every port but 443, so a
# bound host on any other port is refused "host header mismatch" before anything else it could
# prove. 127.0.0.0/8 is a whole /8 of loopback addresses, so each stand takes one.
S_HOST="s.airlock.test"
S_ADDRESS="127.0.0.2:443"
T_HOST="t.airlock.test"
T_ADDRESS="127.0.0.3:443"
MICROSOFT_HOST="microsoft.airlock.test"
AZURE_RESOURCE_HOST="azure-resource.airlock.test"
MICROSOFT_ADDRESS="127.0.0.4:443"
AWS_HOST="aws.airlock.test"
AWS_ADDRESS="127.0.0.5:443"
GITHUB_OWNER="acme-org"
GITHUB_INSTALLATION=7
DEMO_SECRET="AIRLOCK_DEMO_TOKEN"
SCOPED_SECRET="AIRLOCK_SCOPED_TOKEN"
GITHUB_SECRET="AIRLOCK_GITHUB_APP"
GOOGLE_SECRET="AIRLOCK_GOOGLE_LOGIN"
AZURE_SECRET="AIRLOCK_AZURE_LOGIN"
AWS_SECRET="AIRLOCK_AWS_KEY"
# The values the fake store holds for the two stored secrets. They are this script's own strings
# and open nothing; a check compares what a stand-in was sent against them by length and prefix.
DEMO_VALUE="the-real-airlock-demo-token-9f3a7c"
SCOPED_VALUE="the-real-airlock-scoped-token-4b1e"
# The session token of the stack's AWS key pair. One string for both halves of the stand: the
# fake store hands it to the broker as part of the stored value, and --aws-session-token tells
# the stand-in AWS service to require exactly it, so a call the proxy signed again verifies.
AWS_SESSION_TOKEN="the-stand-in-aws-session-token"
# A federated token in the shape the azure-login minter reads: a JWT whose exp claim is
# 4102444800 (2100-01-01). It is signed by nobody and the stand-in Microsoft endpoint takes any
# assertion.
AZURE_FEDERATED_TOKEN="eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjQxMDI0NDQ4MDB9.c2lnbmF0dXJl"

# DEV_BROKER_POSTGRES_URL is read exactly as dev-broker.sh reads it, and checked here before
# anything is created, so a malformed one leaves nothing behind. dev-broker.sh checks it again
# for its own database.
if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ] && ! [[ "$DEV_BROKER_POSTGRES_URL" =~ ^(postgres(ql)?://[^/?]+)/[^/?]*(\?.*)?$ ]]; then
  echo "dev-airlock: DEV_BROKER_POSTGRES_URL must be postgres://<user>@<host>:<port>/<database>" >&2
  exit 2
fi
POSTGRES_SERVER="${BASH_REMATCH[1]:-}"
POSTGRES_QUERY="${BASH_REMATCH[3]:-}"

# The brokers' own database, named from this run's own mktemp suffix, created and dropped here.
# It is never dev-broker.sh's: that broker reads another rules file (none) and another secret
# store, so a policy of this stack's must not be a policy of its.
DB_NAME="dev_airlock_$(printf '%s' "${WORK_DIR##*.}" | tr '[:upper:]' '[:lower:]')"
POSTGRES_CONTAINER="dispatch-pg"
if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ]; then
  POSTGRES_URL="${POSTGRES_SERVER}/${DB_NAME}${POSTGRES_QUERY}"
  POSTGRES_WHERE="the server DEV_BROKER_POSTGRES_URL names"
  admin_psql() { psql "$DEV_BROKER_POSTGRES_URL" -v ON_ERROR_STOP=1 -q "$@"; }
else
  POSTGRES_URL="postgres://postgres:dispatch@127.0.0.1:55432/${DB_NAME}?sslmode=disable"
  POSTGRES_WHERE="the shared $POSTGRES_CONTAINER container dev-broker.sh started"
  admin_psql() { docker exec "$POSTGRES_CONTAINER" psql -U postgres -v ON_ERROR_STOP=1 -q "$@"; }
fi

DEV_BROKER_PID=""
STANDS_PID=""
API_FRONT_PID=""
PROXY_FRONT_PID=""
BROKER_PIDS=()
CREATED_DB=false

cleanup() {
  # This stack's own processes first, then dev-broker.sh, whose own EXIT trap stops its broker
  # and its issuer and drops its database.
  local pid
  for pid in "${BROKER_PIDS[@]+"${BROKER_PIDS[@]}"}" "$API_FRONT_PID" "$PROXY_FRONT_PID" "$STANDS_PID" "$DEV_BROKER_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [ "$CREATED_DB" = true ]; then
    # Best-effort: cleanup runs on every exit path and must never itself fail.
    admin_psql -c "drop database if exists ${DB_NAME};" 2>/dev/null || true
    echo "dev-airlock: dropped this instance's database $DB_NAME in $POSTGRES_WHERE" >&2
  fi
  echo "dev-airlock: workdir kept at $WORK_DIR (rm -rf it once nothing reads it)" >&2
}
trap cleanup EXIT

echo "dev-airlock: workdir $WORK_DIR" >&2

# --- dev-broker.sh: Postgres, dev-oidc-issuer, the five binaries and its own broker. Its output
# is teed to a log of this run's own and to this script's stderr, so a human sees it live and
# this script can read the identities and addresses out of it. tee ignores SIGTERM and SIGINT so
# a signal to the process group reaches dev-broker.sh and tee together. ---
echo "dev-airlock: starting dev-broker.sh (Postgres, dev-oidc-issuer, its broker)..." >&2
: >"$DEV_BROKER_LOG"
bash "$SCRIPT_DIR/dev-broker.sh" > >(trap '' TERM INT; exec tee -a "$DEV_BROKER_LOG" >&2) 2>&1 &
DEV_BROKER_PID=$!

# await <what> <seconds> <log file> <grep -E pattern> [pid to watch]
# Waits for a line one process logs, and fails the run the moment that process dies instead of
# looping until the wait gives up.
await() {
  local what="$1" seconds="$2" file="$3" pattern="$4" watch="${5:-}" waited=0
  while [ "$waited" -lt "$((seconds * 2))" ]; do
    if grep -qE "$pattern" "$file" 2>/dev/null; then
      return 0
    fi
    if [ -n "$watch" ] && ! kill -0 "$watch" 2>/dev/null; then
      echo "dev-airlock: $what exited during startup (see $file)" >&2
      exit 1
    fi
    sleep 0.5
    waited=$((waited + 1))
  done
  echo "dev-airlock: $what did not happen within ${seconds}s (see $file)" >&2
  exit 1
}

await "dev-broker.sh" 300 "$DEV_BROKER_LOG" '^dev-broker: ready\.$' "$DEV_BROKER_PID"

# --- What dev-broker.sh left behind. Its work directory is the one export this script needs by
# name; the files under it are the ones its banner documents, and each is checked here rather
# than assumed, so a rename there fails loudly instead of enrolling nothing. ---
# `|| true`: under pipefail a grep that finds nothing would end the whole run before the
# caller's own check could say what was missing.
first_match() { grep -oEm1 "$1" "$2" 2>/dev/null | head -n1 || true; }

BASE_DIR="$(first_match 'export DEV_BROKER_DIR=[^[:space:]]+' "$DEV_BROKER_LOG" | cut -d= -f2-)"
if [ -z "$BASE_DIR" ] || [ ! -d "$BASE_DIR" ]; then
  echo "dev-airlock: dev-broker.sh printed no DEV_BROKER_DIR" >&2
  exit 1
fi
BASE_BIN_DIR="$BASE_DIR/bin"
BROKER_BIN="$BASE_BIN_DIR/broker"
AGENT_SECRETS_BIN="$BASE_BIN_DIR/agent-secrets"
ACCOUNT_TOKEN_FILE="$BASE_DIR/account-token"
LISTENER_TOKEN_FILE="$BASE_DIR/listener-token"
REGISTRAR_TOKEN_FILE="$BASE_DIR/registrar-token"
POD_TOKEN_FILE="$BASE_DIR/pod-token"
SIGNIN_SECRET_FILE="$BASE_DIR/signin-client-secret"
for needed in "$BROKER_BIN" "$AGENT_SECRETS_BIN" "$ACCOUNT_TOKEN_FILE" "$LISTENER_TOKEN_FILE" \
  "$REGISTRAR_TOKEN_FILE" "$POD_TOKEN_FILE" "$SIGNIN_SECRET_FILE"; do
  if [ ! -s "$needed" ]; then
    echo "dev-airlock: dev-broker.sh left no $needed; this script reads its work directory" >&2
    exit 1
  fi
done

APPROVER_LOGIN="$(first_match 'export AGENT_SECRETS_APPROVER=[^[:space:]]+' "$DEV_BROKER_LOG" | cut -d= -f2-)"
ISSUER_URL="$(first_match 'issuer listening url=[^[:space:]]+' "$DEV_BROKER_LOG" | cut -d= -f2-)"
if [ -z "$APPROVER_LOGIN" ] || [ -z "$ISSUER_URL" ]; then
  echo "dev-airlock: dev-broker.sh printed no approver or no issuer URL" >&2
  exit 1
fi
# dev-broker.sh's sign-in client and the domain it holds a person to, which is the approver's
# own: the issuer puts each signed-in person's email domain in their hd claim.
SIGNIN_CLIENT_ID="dev-broker"
SIGNIN_DOMAIN="${APPROVER_LOGIN#*@}"

# dev-oidc-issuer's own line for each token file it wrote:
#   dev-oidc-issuer: <file> holds <account>'s token for audience <audience> (pod <uid>)
# It is where the accounts, the broker's audience and the pod's uid come from, so this stack's
# brokers list exactly the identities that issuer minted for.
issuer_field() {
  local line
  line="$(grep -m1 -F "dev-oidc-issuer: $1 holds " "$DEV_BROKER_LOG" || true)"
  if [ -z "$line" ]; then
    echo "dev-airlock: dev-oidc-issuer wrote no line for $1" >&2
    exit 1
  fi
  case "$2" in
    account) printf '%s\n' "$line" | sed -E "s/.* holds (.*)'s token for audience .*/\1/" ;;
    audience) printf '%s\n' "$line" | sed -E 's/.* for audience ([^ ]+) \(pod .*/\1/' ;;
    uid) printf '%s\n' "$line" | sed -E 's/.*\(pod ([^)]*)\)$/\1/' ;;
  esac
}
API_ACCOUNT="$(issuer_field "$ACCOUNT_TOKEN_FILE" account)"
LISTENER_ACCOUNT="$(issuer_field "$LISTENER_TOKEN_FILE" account)"
REGISTRAR_ACCOUNT="$(issuer_field "$REGISTRAR_TOKEN_FILE" account)"
POD_ACCOUNT="$(issuer_field "$POD_TOKEN_FILE" account)"
BROKER_AUDIENCE="$(issuer_field "$POD_TOKEN_FILE" audience)"
POD_UID="$(issuer_field "$POD_TOKEN_FILE" uid)"

# --- This stack's own database, on the server dev-broker.sh is already using. ---
admin_psql -c "create database ${DB_NAME};"
CREATED_DB=true
echo "dev-airlock: created isolated database $DB_NAME" >&2
DISPLAY_URL="$(printf '%s' "$POSTGRES_URL" | sed -E 's#^(postgres(ql)?://[^:/@?]+):[^@/?]*@#\1:***@#')"

# --- The one binary dev-broker.sh does not build: the stands, the fronts and the enrolment. ---
echo "dev-airlock: building dev-airlock-stands..." >&2
mkdir -p "$BIN_DIR"
( cd "$ENVOY_DIR" && GOTOOLCHAIN=go1.27.3 go build -o "$STANDS_BIN" ./cmd/dev-airlock-stands )

# --- The stand-ins. Each HTTPS stand's certificate carries the name the rules bind it to, since
# the proxy verifies the name it dialled against the certificate the stand presents. The
# Microsoft stand carries the resource host's name too: it answers the tenant's OpenID
# configuration document, which is the first thing az reads of an authority, and a token
# request at /{tenant}/oauth2/v2.0/token, and leaves every other request to its own body, so
# one process plays the authority and the service an access token is spent at. ---
echo "dev-airlock: starting dev-airlock-stands..." >&2
: >"$STANDS_LOG"
"$STANDS_BIN" --dir "$STANDS_DIR" \
  --address "s=$S_ADDRESS" --host "s=$S_HOST" \
  --address "t=$T_ADDRESS" --host "t=$T_HOST" \
  --address "microsoft=$MICROSOFT_ADDRESS" \
  --host "microsoft=$MICROSOFT_HOST" --host "microsoft=$AZURE_RESOURCE_HOST" \
  --address "aws=$AWS_ADDRESS" --host "aws=$AWS_HOST" \
  --github-owner "$GITHUB_OWNER=$GITHUB_INSTALLATION" \
  --google-lifetime 7m \
  --aws-session-token "$AWS_SESSION_TOKEN" \
  >"$STANDS_JSON" 2> >(trap '' TERM INT; exec tee -a "$STANDS_LOG" >&2) &
STANDS_PID=$!
await "dev-airlock-stands" 60 "$STANDS_LOG" 'stands listening records=' "$STANDS_PID"

stand() { jq -r "$1" "$STANDS_JSON"; }
CA_CERT_FILE="$(stand .ca_cert_file)"
CA_KEY_FILE="$(stand .ca_key_file)"
RECORDS_URL="$(stand .records_url)"
AWS_KEY_ID="$(stand .aws_key_id)"
AWS_SECRET_KEY="$(stand .aws_secret_key)"
GITHUB_APP_ID="$(stand .github_app_id)"
GITHUB_APP_KEY_FILE="$(stand .github_app_key_file)"
# The GitHub and Google stands are reached by the broker itself, not through the proxy, so
# they keep a port the system picked on 127.0.0.1 and are named by address.
GITHUB_URL="$(stand '.stands.github.url')"
GOOGLE_URL="$(stand '.stands.google.url')"
# Every bound host is reached by its name on 443, which is how its Host header matches its
# CONNECT target, so no URL here carries a port.
S_URL="https://${S_HOST}"
T_URL="https://${T_HOST}"
MICROSOFT_URL="https://${MICROSOFT_HOST}"
AZURE_RESOURCE_URL="https://${AZURE_RESOURCE_HOST}"
AWS_URL="https://${AWS_HOST}"
# What the proxy may dial although it resolves to loopback, and where each name is. The two
# together are what check k4 reads: in one run S and T on loopback are reachable and the
# broker's own loopback port is still refused, which no switch for loopback could give.
DIAL_ALLOW="${S_HOST}:443 ${T_HOST}:443 ${MICROSOFT_HOST}:443 ${AZURE_RESOURCE_HOST}:443 ${AWS_HOST}:443"
RESOLVE="${S_HOST}=${S_ADDRESS%:*} ${T_HOST}=${T_ADDRESS%:*} ${MICROSOFT_HOST}=${MICROSOFT_ADDRESS%:*} ${AZURE_RESOURCE_HOST}=${MICROSOFT_ADDRESS%:*} ${AWS_HOST}=${AWS_ADDRESS%:*}"

# --- The rules file: where each secret's value may go and in which headers. ---
cat >"$RULES_FILE" <<EOF
{"secrets": [
  {"name": "$DEMO_SECRET", "hosts": ["$S_HOST"]},
  {"name": "$SCOPED_SECRET", "hosts": ["$S_HOST"], "headers": ["authorization", "x-key"],
   "allow": [{"method": "GET", "pathPrefix": "/read"}]},
  {"name": "$GITHUB_SECRET", "hosts": ["$S_HOST"]},
  {"name": "$GOOGLE_SECRET", "hosts": ["$S_HOST"]},
  {"name": "$AZURE_SECRET", "hosts": ["$MICROSOFT_HOST", "$AZURE_RESOURCE_HOST"], "kind": "azure"},
  {"name": "$AWS_SECRET", "hosts": ["$AWS_HOST"], "kind": "aws"}
]}
EOF

# --- The fake Secrets Manager the brokers read, written with jq so the GitHub App's PEM and
# each object value are escaped once and correctly. Every value here is this stack's own and
# opens nothing. The two stored secrets, the github-app and the google-login secret are owned
# by the service the enrolled pod runs as, so a request for one is granted at once; the
# azure-login secret is human tier and owned by the approver, because the broker releases its
# projected Azure token only for a human-tier name a person approved. ---
jq -n \
  --arg prefix "$SECRETS_PREFIX" --arg kms "$SECRETS_KMS_KEY_ARN" \
  --arg service "$SERVICE" --arg approver "$APPROVER_LOGIN" \
  --arg demo "$DEMO_VALUE" --arg scoped "$SCOPED_VALUE" \
  --arg appid "$GITHUB_APP_ID" --rawfile appkey "$GITHUB_APP_KEY_FILE" \
  --arg owner "$GITHUB_OWNER" \
  --arg awskey "$AWS_KEY_ID" --arg awssecret "$AWS_SECRET_KEY" --arg awssession "$AWS_SESSION_TOKEN" \
  '{secrets: [
     {name: ($prefix + "airlock-demo-token"), kms_key_id: $kms,
      tags: {owner: $service, tier: "agent"}, value: $demo},
     {name: ($prefix + "airlock-scoped-token"), kms_key_id: $kms,
      tags: {owner: $service, tier: "agent"}, value: $scoped},
     {name: ($prefix + "airlock-github-app"), kms_key_id: $kms,
      tags: {owner: $service, tier: "agent", kind: "github-app", "github-owners": $owner},
      value: ({app_id: $appid, private_key: $appkey} | tojson)},
     {name: ($prefix + "airlock-google-login"), kms_key_id: $kms,
      tags: {owner: $service, tier: "agent", kind: "google-login"},
      value: ({type: "authorized_user", client_id: "the-stand-in-client",
               client_secret: "the-stand-in-client-secret",
               refresh_token: "the-stand-in-refresh-token"} | tojson)},
     {name: ($prefix + "airlock-azure-login"), kms_key_id: $kms,
      tags: {owner: $approver, tier: "human", kind: "azure-login"},
      value: ({tenant_id: "the-stand-in-tenant", client_id: "the-stand-in-application"} | tojson)},
     {name: ($prefix + "airlock-aws-key"), kms_key_id: $kms,
      tags: {owner: $service, tier: "agent"},
      value: ({access_key_id: $awskey, secret_access_key: $awssecret,
               session_token: $awssession} | tojson)}
   ]}' >"$FAKE_SECRETS_FILE"
chmod 600 "$FAKE_SECRETS_FILE"
printf '%s' "$AZURE_FEDERATED_TOKEN" >"$AZURE_TOKEN_FILE"
chmod 600 "$AZURE_TOKEN_FILE"

# --- The two fronts, bound before the brokers exist. Each broker's BROKER_PUBLIC_URL and
# BROKER_AIRLOCK_PROXY_URL must name the front at boot - a session's proof is verified against
# the public URL, so both brokers have to be one address - and a broker's own ports are only
# known once it has bound them. So each front binds first and reads its backends from a file
# this script writes once the brokers report their addresses. ---
# start_front sets FRONT_PID, rather than leaving the caller to read $!, because the wait for
# the front's own log line sits between the two.
FRONT_PID=""
start_front() {
  local name="$1" listen_json="$2" log="$3" backends="$4"
  : >"$log"
  : >"$backends"
  "$STANDS_BIN" front --listen 127.0.0.1:0 --backends-file "$backends" \
    >"$listen_json" 2> >(trap '' TERM INT; exec tee -a "$log" >&2) &
  FRONT_PID=$!
  await "the $name front" 30 "$log" 'front listening addr=' "$FRONT_PID"
}
echo "dev-airlock: starting the API and proxy fronts..." >&2
start_front "API" "$API_FRONT_JSON" "$API_FRONT_LOG" "$API_BACKENDS"
API_FRONT_PID="$FRONT_PID"
start_front "proxy" "$PROXY_FRONT_JSON" "$PROXY_FRONT_LOG" "$PROXY_BACKENDS"
PROXY_FRONT_PID="$FRONT_PID"
API_FRONT_URL="$(jq -r .url "$API_FRONT_JSON")"
PROXY_FRONT_URL="$(jq -r .url "$PROXY_FRONT_JSON")"

# --- The brokers. Both read one database, one rules file and one fake store, and each binds an
# API port and a proxy port the system picks (BROKER_*_LISTEN_ADDR of port 0), so two of them
# never collide. BROKER_DRAIN_SECONDS=0 as dev-broker.sh does: the five-second default exists
# to outlast a Kubernetes EndpointSlice propagation nothing here has. ---
start_broker() {
  local n="$1" log="$2"
  : >"$log"
  BROKER_DATABASE_URL="$POSTGRES_URL" \
  BROKER_PUBLIC_URL="$API_FRONT_URL" \
  BROKER_LISTEN_ADDR="127.0.0.1:0" \
  BROKER_MACHINE_LOGIN_URL="http://127.0.0.1/credentials/machine" \
  BROKER_SECRETS_PREFIX="$SECRETS_PREFIX" \
  BROKER_SECRETS_KMS_KEY_ARN="$SECRETS_KMS_KEY_ARN" \
  BROKER_FAKE_SECRETS_FILE="$FAKE_SECRETS_FILE" \
  BROKER_DRAIN_SECONDS=0 \
  BROKER_K8S_OIDC_ISSUER="$ISSUER_URL" \
  BROKER_K8S_OIDC_AUDIENCE="$BROKER_AUDIENCE" \
  BROKER_API_ACCOUNTS="dispatch=$API_ACCOUNT listener=$LISTENER_ACCOUNT" \
  BROKER_SERVICE_LOGINS="$SERVICE=$REGISTRAR_ACCOUNT" \
  BROKER_SERVICES="$SERVICE=$POD_ACCOUNT" \
  BROKER_SIGNIN_ISSUER="$ISSUER_URL" \
  BROKER_SIGNIN_CLIENT_ID="$SIGNIN_CLIENT_ID" \
  BROKER_SIGNIN_CLIENT_SECRET_FILE="$SIGNIN_SECRET_FILE" \
  BROKER_SIGNIN_DOMAIN="$SIGNIN_DOMAIN" \
  BROKER_AZURE_TOKEN_FILE="$AZURE_TOKEN_FILE" \
  BROKER_AZURE_LOGIN_SECRET="$AZURE_SECRET" \
  BROKER_AIRLOCK_RULES_FILE="$RULES_FILE" \
  BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" \
  BROKER_AIRLOCK_PROXY_URL="$PROXY_FRONT_URL" \
  BROKER_AIRLOCK_CA_CERT_FILE="$CA_CERT_FILE" \
  BROKER_AIRLOCK_CA_KEY_FILE="$CA_KEY_FILE" \
  BROKER_AIRLOCK_IDLE_SECONDS="$IDLE_SECONDS" \
  BROKER_AIRLOCK_TEST_GITHUB_API_BASE="$GITHUB_URL" \
  BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL="$GOOGLE_URL/token" \
  BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE="$CA_CERT_FILE" \
  BROKER_AIRLOCK_TEST_DIAL_ALLOW="$DIAL_ALLOW" \
  BROKER_AIRLOCK_TEST_RESOLVE="$RESOLVE" \
  BROKER_AIRLOCK_TEST_KEEP_VALUES="$KEEP_VALUES" \
  BROKER_AIRLOCK_TEST_NO_KIND="$NO_KIND" \
  "$BROKER_BIN" > >(trap '' TERM INT; exec tee -a "$log" >&2) 2>&1 &
  BROKER_PIDS[n]=$!
}

BROKER_LOGS=()
BROKER_API_ADDRS=()
BROKER_PROXY_ADDRS=()
for n in $(seq 1 "$BROKER_COUNT"); do
  log="$WORK_DIR/broker$n.log"
  BROKER_LOGS[n]="$log"
  echo "dev-airlock: starting broker $n of $BROKER_COUNT..." >&2
  start_broker "$n" "$log"
done
for n in $(seq 1 "$BROKER_COUNT"); do
  await "broker $n" 120 "${BROKER_LOGS[n]}" 'airlock proxy listening addr=' "${BROKER_PIDS[n]}"
  BROKER_API_ADDRS[n]="$(first_match 'broker listening addr=[^[:space:]]+' "${BROKER_LOGS[n]}" | cut -d= -f2-)"
  BROKER_PROXY_ADDRS[n]="$(first_match 'airlock proxy listening addr=[^[:space:]]+' "${BROKER_LOGS[n]}" | cut -d= -f2-)"
  if [ -z "${BROKER_API_ADDRS[n]}" ] || [ -z "${BROKER_PROXY_ADDRS[n]}" ]; then
    echo "dev-airlock: broker $n logged no API or proxy address" >&2
    exit 1
  fi
done

# --- The fronts learn their backends. Each front rereads this file when it changes, so writing
# it here is all it takes. ---
printf '%s\n' "${BROKER_API_ADDRS[@]}" >"$API_BACKENDS"
printf '%s\n' "${BROKER_PROXY_ADDRS[@]}" >"$PROXY_BACKENDS"

ready=false
for _ in $(seq 1 60); do
  if curl -sf "${API_FRONT_URL}/healthz" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.5
done
if [ "$ready" != true ]; then
  echo "dev-airlock: the API front never answered /healthz within 30s" >&2
  exit 1
fi

# --- The pod session. agent-secrets makes the key and prints its thumbprint; the enrolment
# logs the registrar account in and enrols the pod under it, as the Legion daemon does; the
# enrollment id goes beside the key, which is the whole of what AGENT_SECRETS_KEY_DIR holds. ---
echo "dev-airlock: enrolling a pod session..." >&2
mkdir -p "$KEY_DIR"
THUMBPRINT="$("$AGENT_SECRETS_BIN" keygen --out "$KEY_DIR")"
"$STANDS_BIN" enroll-pod \
  --url "$API_FRONT_URL" \
  --service "$SERVICE" \
  --token-file "$REGISTRAR_TOKEN_FILE" \
  --pod-token-file "$POD_TOKEN_FILE" \
  --pod-uid "$POD_UID" \
  --thumbprint "$THUMBPRINT" \
  >"$ENROLL_JSON"
ENROLLMENT_ID="$(jq -r .enrollment_id "$ENROLL_JSON")"
if [ -z "$ENROLLMENT_ID" ] || [ "$ENROLLMENT_ID" = null ]; then
  echo "dev-airlock: the enrolment answered no enrollment id" >&2
  exit 1
fi
printf '%s' "$ENROLLMENT_ID" >"$KEY_DIR/enrollment"
echo "dev-airlock: enrolled pod session $ENROLLMENT_ID (pod $POD_UID, service $SERVICE)" >&2

# One banner line per broker, built here rather than substituted into the banner below: a
# command substitution there would lose the last line's newline.
BROKER_BANNER=""
for n in $(seq 1 "$BROKER_COUNT"); do
  if [ -n "$BROKER_BANNER" ]; then
    BROKER_BANNER+=$'\n'
  fi
  BROKER_BANNER+="  broker $n:            api ${BROKER_API_ADDRS[n]}, proxy ${BROKER_PROXY_ADDRS[n]}, pid ${BROKER_PIDS[n]}, log ${BROKER_LOGS[n]}"
done

cat <<EOF

dev-airlock: ready.

  export AGENT_SECRETS_URL=$API_FRONT_URL
  export AGENT_SECRETS_KEY_DIR=$KEY_DIR
  export AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$ACCOUNT_TOKEN_FILE
  export AIRLOCK_PROXY_URL=$PROXY_FRONT_URL
  export AIRLOCK_DIR=$WORK_DIR
  export AIRLOCK_CA_CERT_FILE=$CA_CERT_FILE
  export AIRLOCK_CA_KEY_FILE=$CA_KEY_FILE
  export AIRLOCK_RULES_FILE=$RULES_FILE
  export AIRLOCK_FAKE_SECRETS_FILE=$FAKE_SECRETS_FILE
  export AIRLOCK_RECORDS_URL=$RECORDS_URL
  export AIRLOCK_S_URL=$S_URL
  export AIRLOCK_S_RECORDS=$RECORDS_URL/records/s
  export AIRLOCK_T_URL=$T_URL
  export AIRLOCK_T_RECORDS=$RECORDS_URL/records/t
  export AIRLOCK_GITHUB_URL=$GITHUB_URL
  export AIRLOCK_GITHUB_RECORDS=$RECORDS_URL/records/github
  export AIRLOCK_GOOGLE_URL=$GOOGLE_URL
  export AIRLOCK_GOOGLE_RECORDS=$RECORDS_URL/records/google
  export AIRLOCK_MICROSOFT_URL=$MICROSOFT_URL
  export AIRLOCK_MICROSOFT_RECORDS=$RECORDS_URL/records/microsoft
  export AIRLOCK_AZURE_RESOURCE_URL=$AZURE_RESOURCE_URL
  export AIRLOCK_AWS_URL=$AWS_URL
  export AIRLOCK_AWS_RECORDS=$RECORDS_URL/records/aws
  export AIRLOCK_AWS_KEY_ID=$AWS_KEY_ID
  export AIRLOCK_GITHUB_APP_KEY_FILE=$GITHUB_APP_KEY_FILE
  export AIRLOCK_APPROVER=$APPROVER_LOGIN
  export AIRLOCK_SIGNIN_URL=$ISSUER_URL
  export AIRLOCK_ENROLLMENT=$ENROLLMENT_ID
  export DEV_BROKER_DIR=$BASE_DIR
  export PATH=$BIN_DIR:$BASE_BIN_DIR:\$PATH

$BROKER_BANNER
  secrets:             $DEMO_SECRET and $SCOPED_SECRET (stored, bound to $S_HOST; $SCOPED_SECRET in
                       authorization and x-key, GET /read alone), $GITHUB_SECRET (github-app,
                       installation $GITHUB_INSTALLATION on $GITHUB_OWNER), $GOOGLE_SECRET (google-login),
                       $AZURE_SECRET (kind azure, human tier) and $AWS_SECRET (kind aws)
  approver:            $APPROVER_LOGIN, who approves $AZURE_SECRET on ${API_FRONT_URL}/approve/<record>;
                       the sign-in at $ISSUER_URL signs that person in with no page to fill in,
                       and a POST to ${API_FRONT_URL}/approve needs an Origin header or the page answers 403
  database:            $DISPLAY_URL  (this instance's own; created and dropped by this script)
  stand-ins:           $STANDS_DIR  (each one's certificate; the authority is $CA_CERT_FILE)
  dev-broker.sh:       $BASE_DIR  (its own broker, issuer, tokens and database, logged to $DEV_BROKER_LOG)

  A command behind the airlock gets a placeholder, not a value: ask for one with
  agent-secrets $DEMO_SECRET -- <command>, whose HTTPS_PROXY is the proxy front above, and read
  what $S_HOST was actually sent at $RECORDS_URL/records/s. Press Ctrl-C here to stop
  every process this script started.

  dev-broker.sh printed a banner of its own above, for its own broker: where the two name the
  same variable - AGENT_SECRETS_URL, AGENT_SECRETS_ACCOUNT_TOKEN_FILE, PATH - this one is the
  airlock's stack and the one to export.
EOF

wait "${BROKER_PIDS[1]}"
