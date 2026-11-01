#!/usr/bin/env bash
set -euo pipefail
# This rig's NATS is a throwaway server with no users. nats.go refuses an nkey when the server sends
# no nonce ("nats: nkeys not supported by the server"), so no process here inherits an operator's
# NATS_NKEY_SEED or NATS_NKEY_SEED_FILE.
unset NATS_NKEY_SEED NATS_NKEY_SEED_FILE

# The server is a test fixture. It must not inherit a service endpoint,
# credential, data file or configuration from the shell that starts Playwright:
# a Legion pane exports ENVOY_URL (the real listener), DISPATCH_URL and
# DISPATCH_TOKEN_FILE, and a developer's shell can retain a GitHub App, a
# cookie signing key, an Envoy token or a dashboard origin from other work.
#
# DATABASE_URL is the one required caller input. The harness has no fake
# Postgres and e2e/seed.ts truncates the named database before every scenario,
# so silently selecting a shared default would make the destructive write
# target ambiguous. FAKE_ENVOY_PORT and FAKE_GITHUB_PORT (and FAKE_BROKER_PORT
# for the default broker switch below) name the fakes the caller started, whose
# ports e2e/harness-start.ts reads from their reports.
#
# One server (DISPATCH_E2E_PROCESSES unset or 1): DISPATCH_E2E_PORT unset (or
# empty), the server serves on a port the kernel chooses, held from that moment
# until the server serves on it: scripts/e2e/lib/held-port.py opens a listening
# socket on 127.0.0.1:0 and the server inherits it (DISPATCH_LISTEN_FD). The
# server's own line reports it,
#   ... msg="dispatch: listening" addr=127.0.0.1:<port>
# on stderr, the line a caller waits for and reads the port from. Set, the
# server binds exactly that port, and an occupied one fails with "address
# already in use". DISPATCH_E2E_LISTEN_FROM instead names the unix socket of a
# `held-port.py keep` process, for a caller that needs the port before the
# server starts: the server serves on that keeper's port. The server's
# dashboard origin is its own, http://127.0.0.1:<port>.
#
# Two processes (DISPATCH_E2E_PROCESSES=2, e2e/harness-processes.ts): the
# harness runs this script twice, Dispatch A and B, behind the round-robin proxy
# (e2e/round-robin-proxy.ts) on DISPATCH_E2E_PORT, whose origin both serve as
# their dashboard origin. Each run's own port is DISPATCH_E2E_SERVER_PORT when
# its DISPATCH_E2E_PORT_A or _B is set, and otherwise the port of the keeper
# DISPATCH_E2E_LISTEN_FROM names, which e2e/harness-start.ts holds before it
# starts the proxy, so the proxy can name it. B gets a web dist of its own in
# DISPATCH_E2E_SERVER_WEB_DIST (a path from packages/envoy, where the server
# runs), and both one DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY, required here under dev
# sign-in: with a key apiece, a person signed in on one process is signed out by
# the first request the other answers.
#
# Sign-in (DISPATCH_E2E_SIGNIN, e2e/harness-signin.ts): unset (or empty), the
# server mounts its dev sign-in route (DISPATCH_DEV_SIGNIN=1, below). `oidc` is
# the production sign-in instead, at the dev OIDC issuer the harness started
# (e2e/run-signin-issuer.sh) on DISPATCH_E2E_SIGNIN_ISSUER_PORT: the server takes
# it as DISPATCH_SIGNIN_ISSUER, with DISPATCH_E2E_SIGNIN_CLIENT_ID,
# DISPATCH_E2E_SIGNIN_CLIENT_SECRET and DISPATCH_E2E_SIGNIN_GROUP as its client
# and the group it admits, and DISPATCH_E2E_SIGNING_KEY as DISPATCH_SIGNING_KEY,
# one key for every process of the run, all required, and no dev sign-in.
case "${DISPATCH_E2E_PROCESSES:-1}" in
  1)
    own_port_variable=DISPATCH_E2E_PORT
    own_port="${DISPATCH_E2E_PORT:-}"
    dashboard_origin=""
    ;;
  2)
    if [ -z "${DISPATCH_E2E_SIGNIN:-}" ]; then
      : "${DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY:?DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY must give both processes of DISPATCH_E2E_PROCESSES=2 one cookie key}"
    fi
    : "${DISPATCH_E2E_PORT:?DISPATCH_E2E_PORT must name the round-robin proxy (e2e/round-robin-proxy.ts) both processes of DISPATCH_E2E_PROCESSES=2 serve behind}"
    own_port_variable=DISPATCH_E2E_SERVER_PORT
    own_port="${DISPATCH_E2E_SERVER_PORT:-}"
    dashboard_origin="http://127.0.0.1:$DISPATCH_E2E_PORT"
    ;;
  *)
    echo "DISPATCH_E2E_PROCESSES must be 1, 2 or unset, not '$DISPATCH_E2E_PROCESSES'" >&2
    exit 1
    ;;
esac
: "${DATABASE_URL:?DATABASE_URL must name an isolated Dispatch e2e database}"
: "${FAKE_ENVOY_PORT:?FAKE_ENVOY_PORT must name the fake Envoy (e2e/fake-envoy.ts) this server reads}"
: "${FAKE_GITHUB_PORT:?FAKE_GITHUB_PORT must name the fake GitHub (e2e/fake-github.ts) this server reads}"
database_url="$DATABASE_URL"
fake_envoy_port="$FAKE_ENVOY_PORT"
fake_github_port="$FAKE_GITHUB_PORT"
held_port="$(cd "$(dirname "$0")/../../.." && pwd)/scripts/e2e/lib/held-port.py"
if [ -n "$own_port" ] && [ -n "${DISPATCH_E2E_LISTEN_FROM:-}" ]; then
  echo "run-server.sh: set $own_port_variable or DISPATCH_E2E_LISTEN_FROM, not both" >&2
  exit 2
fi
if [ -n "$own_port" ]; then
  listen=()
  port_env=(DISPATCH_PORT="$own_port" DISPATCH_SERVER_URL="${dashboard_origin:-http://127.0.0.1:$own_port}")
else
  listen=(python3 "$held_port" exec)
  if [ -n "${DISPATCH_E2E_LISTEN_FROM:-}" ]; then
    listen+=(--from "$DISPATCH_E2E_LISTEN_FROM")
  fi
  # held-port.py writes the port it holds in for {port}.
  own_origin='http://127.0.0.1:{port}'
  listen+=(--fd-env DISPATCH_LISTEN_FD
    --env 'DISPATCH_PORT={port}' --env "DISPATCH_SERVER_URL=${dashboard_origin:-$own_origin}" --)
  port_env=()
fi

# This process's own web dist and sign-in, read before the sweep below: web/dist and dev sign-in
# with a cookie key of its own unless two-process mode or DISPATCH_E2E_SIGNIN gave it others
# (above).
web_dist="${DISPATCH_E2E_SERVER_WEB_DIST:-../dispatch/web/dist}"
case "${DISPATCH_E2E_SIGNIN:-}" in
  "")
    signin_env=(DISPATCH_DEV_SIGNIN=1)
    if [ -n "${DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY:-}" ]; then
      signin_env+=(DISPATCH_DEV_SIGNIN_SHARED_KEY="$DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY")
    fi
    ;;
  oidc)
    : "${DISPATCH_E2E_SIGNIN_ISSUER_PORT:?DISPATCH_E2E_SIGNIN_ISSUER_PORT must name the dev OIDC issuer (e2e/run-signin-issuer.sh) DISPATCH_E2E_SIGNIN=oidc signs in at}"
    : "${DISPATCH_E2E_SIGNIN_CLIENT_ID:?DISPATCH_E2E_SIGNIN_CLIENT_ID must name the Dispatch client at the dev OIDC issuer}"
    : "${DISPATCH_E2E_SIGNIN_CLIENT_SECRET:?DISPATCH_E2E_SIGNIN_CLIENT_SECRET must give the secret of that client}"
    : "${DISPATCH_E2E_SIGNIN_GROUP:?DISPATCH_E2E_SIGNIN_GROUP must name the sign-in pool group the server admits}"
    : "${DISPATCH_E2E_SIGNING_KEY:?DISPATCH_E2E_SIGNING_KEY must give every process of DISPATCH_E2E_SIGNIN=oidc one cookie key}"
    signin_env=(
      DISPATCH_SIGNIN_ISSUER="http://127.0.0.1:$DISPATCH_E2E_SIGNIN_ISSUER_PORT"
      DISPATCH_SIGNIN_CLIENT_ID="$DISPATCH_E2E_SIGNIN_CLIENT_ID"
      DISPATCH_SIGNIN_CLIENT_SECRET="$DISPATCH_E2E_SIGNIN_CLIENT_SECRET"
      DISPATCH_SIGNIN_GROUP="$DISPATCH_E2E_SIGNIN_GROUP"
      DISPATCH_SIGNING_KEY="$DISPATCH_E2E_SIGNING_KEY"
    )
    ;;
  *)
    echo "DISPATCH_E2E_SIGNIN must be oidc or unset, not '$DISPATCH_E2E_SIGNIN'" >&2
    exit 1
    ;;
esac

# The secrets broker the server relays credential requests to, read before
# the sweep below drops every DISPATCH_* variable. DISPATCH_E2E_AGENT_SECRETS_URL
# is the harness's one switch, which e2e/harness-broker.ts reads the same way
# (no colon, so empty and unset differ): unset is e2e/fake-broker.ts, which
# verifies no caller, so the server presents the throwaway token in
# e2e/fake-broker-account-token; empty is no broker, the credential feature
# off, as in a deployment that configures none; a URL is a broker the caller
# runs, which verifies the ServiceAccount token in
# DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE against its issuer and lists
# that account in its BROKER_API_ACCOUNTS. The server reads the file itself, on
# each call, so no argv carries the token and a rewrite reaches it.
broker_env=()
if [ -z "${DISPATCH_E2E_AGENT_SECRETS_URL+set}" ]; then
  : "${FAKE_BROKER_PORT:?FAKE_BROKER_PORT must name the fake secrets broker (e2e/fake-broker.ts) this server reads}"
  broker_env=(
    DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$(cd "$(dirname "$0")" && pwd)/fake-broker-account-token"
    DISPATCH_AGENT_SECRETS_URL="http://127.0.0.1:$FAKE_BROKER_PORT"
  )
elif [ -n "$DISPATCH_E2E_AGENT_SECRETS_URL" ]; then
  : "${DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE:?DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE must accompany DISPATCH_E2E_AGENT_SECRETS_URL}"
  # The server reads the file from packages/envoy, where this script changes directory below, so a
  # path relative to the caller's directory is made absolute first.
  token_file="$DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"
  [[ "$token_file" == /* ]] || token_file="$PWD/$token_file"
  broker_env=(
    DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$token_file"
    DISPATCH_AGENT_SECRETS_URL="$DISPATCH_E2E_AGENT_SECRETS_URL"
  )
fi

# The file store, read before the sweep below: DISPATCH_E2E_FILE_STORE_ENDPOINT names an
# S3-compatible server the caller started (CI starts one; a developer may), and
# DISPATCH_E2E_FILE_STORE_BUCKET the bucket it holds, so every upload and download the dashboard
# makes goes through the store as production's does. Unset, uploads stay in Postgres. The SDK's
# own variables point it at the server: an IP-literal endpoint (with `localhost` the SDK addresses
# the bucket as a subdomain), a region, static keys the server ignores, and no metadata service.
file_store_env=()
if [ -n "${DISPATCH_E2E_FILE_STORE_ENDPOINT:-}" ]; then
  : "${DISPATCH_E2E_FILE_STORE_BUCKET:?DISPATCH_E2E_FILE_STORE_BUCKET must accompany DISPATCH_E2E_FILE_STORE_ENDPOINT}"
  file_store_env=(
    DISPATCH_FILE_STORE_BUCKET="$DISPATCH_E2E_FILE_STORE_BUCKET"
    AWS_ENDPOINT_URL_S3="$DISPATCH_E2E_FILE_STORE_ENDPOINT"
    AWS_REGION=us-east-1
    AWS_ACCESS_KEY_ID=e2e
    AWS_SECRET_ACCESS_KEY=e2e
    AWS_EC2_METADATA_DISABLED=true
  )
fi

# How long a document room waits after its last live update before it settles, read before the
# sweep below: a settlement is when an `:::ask` block becomes an ask row and when live typing is
# written as a version, so it is what every row that waits for one waits for. 250ms in place of
# the document service's own 2 s default (internal/dispatch/docs/service.go), passed as
# DISPATCH_TEST_SETTLE_DELAY under the DISPATCH_TEST_HOOKS=1 below, which the server requires with
# it. The one class of row it changes is a row that proves a settlement did NOT happen inside a
# window: that window must now be shorter than this delay, which the slow-row work owns. A caller
# that set the variable itself keeps its own value, as it does for DISPATCH_E2E_SERVER_WEB_DIST.
settle_delay="${DISPATCH_TEST_SETTLE_DELAY:-250ms}"

# The two waits a provisional comment, ask or suggestion mark lives by, read before the sweep
# below and passed under the same DISPATCH_TEST_HOOKS=1. A mark no row records is swept one
# unrecorded-mark TTL after the settlement that first saw it: 4s in place of the document
# service's own minute (internal/dispatch/docs/service.go), which is the shortest value that
# still lets a row tell that sweep from a socket reconnect - a reconnect takes about two seconds
# here, and the mark has to outlive it, since a mark its author's browser is back on is kept.
# A mark the browser says it wrote is waited for before the server answers that the document does
# not hold it: 250ms in place of its own 1 s, which is the whole cost of every Send whose mark is
# already gone, and ample for one document update to arrive over the loopback. A caller that set
# either variable itself keeps its own value, as it does for the settle delay above.
mark_wait="${DISPATCH_TEST_MARK_WAIT:-250ms}"
unrecorded_mark_ttl="${DISPATCH_TEST_UNRECORDED_MARK_TTL:-4s}"

# Meaning search's embedder, read before the sweep below. The file-store block above gives this
# process the AWS SDK's own variables so it can reach the lane's S3 server, and Bedrock - the
# default place DISPATCH_EMBEDDER names - takes those same static keys as its credentials and the
# region as a real region: every text the embedding queue picks up, and every search query, then
# makes a real call to bedrock-runtime.<region>.amazonaws.com that AWS refuses 403, retries it,
# and logs it (merge-base run 41085647285 did that 1,522 times with the canary alone). No row
# reads a meaning-search result - the suite has no model service, so search answers keyword-only
# whether the call is made and refused or never made at all - so DISPATCH_TEST_EMBEDDER_OFF=1,
# under the DISPATCH_TEST_HOOKS=1 below, builds no embedder: the queue's Run returns at once and
# no query is embedded. A caller that set the variable itself keeps its own value, as it does for
# the settle delay above, so a run that one day proves the embed path turns it back on with 0.
embedder_off="${DISPATCH_TEST_EMBEDDER_OFF:-1}"

# The Go runtime's view of the machine, read before the sweep below. Go sizes GOMAXPROCS, and with
# it the scheduler's threads and the garbage collector's parallelism, from every CPU it can see,
# and a lane's server sees all of them however many lanes share them: four servers on a four-vCPU
# runner each run a four-way collector. One core each instead, by integer division with a floor of
# one. The lanes are e2e/harness-ports.ts's laneCount read in shell: DISPATCH_E2E_LANES, which
# e2e/run.ts exports for the command it runs, else the DISPATCH_E2E_WORKERS the caller asked for,
# else one - and e2e/run.ts starts the harness before it exports the first, so a lane's own server
# is reached by the second. One lane leaves Go's own default, since nproc / 1 is nproc, and
# two-process mode runs one lane, so its two servers are unchanged too.
gomaxprocs=$(( $(nproc) / ${DISPATCH_E2E_LANES:-${DISPATCH_E2E_WORKERS:-1}} ))
[ "$gomaxprocs" -ge 1 ] || gomaxprocs=1

mapfile -t inherited < <(compgen -e)
for name in "${inherited[@]}"; do
  case "$name" in
    DISPATCH_* | ENVOY_* | NATS_*) unset "$name" ;;
  esac
done

# Architecture-source access checks run against the fake GitHub listener with
# throwaway App credentials: a fresh RSA key per run (nothing secret to
# commit) beside a fake client id. Identity is the production cookie: the server
# mints it for the email a test names at its dev sign-in route
# (DISPATCH_DEV_SIGNIN=1) with a signing key it generates for this process
# alone, or in two-process mode derives from the shared key above, so no
# signing key is passed and nothing reaches the caller's persistent data dir;
# under DISPATCH_E2E_SIGNIN=oidc it mints it at /auth/callback, signed with the
# run's DISPATCH_SIGNING_KEY, which keeps the data dir out of it too.
# DISPATCH_INSECURE_COOKIE=1 keeps the cookie usable over plain HTTP in every
# engine. DISPATCH_SERVER_URL is the dashboard origin above,
# http://127.0.0.1:<port> of the port the server serves on, or in two-process
# mode the round-robin proxy's: it is the origin the CSRF guard compares writes
# against, the only Host the router serves under dev sign-in, the origin of the
# sign-in's /auth/callback, and the Playwright config's baseURL. Dev sign-in
# also refuses a DATABASE_URL whose host is not loopback or a unix socket, and
# any DISPATCH_SIGNIN_* setting.
app_pem="$(openssl genrsa 2048 2>/dev/null)"

cd "$(dirname "$0")/../../envoy"
# Build first, in the caller's toolchain environment, and exec the binary itself. A server
# started as `go run` is the go command's child; the go command ignores only SIGINT and SIGQUIT,
# so a SIGTERM to it ends the go command and leaves the server listening. Playwright kills the
# whole process group and the skill-scenarios rig the whole process tree, but a caller that
# signals the one pid it started (`kill $!`) needs that pid to be the server. One binary per
# checkout, built under a lock so two harnesses, or two-process mode's two servers, starting at
# once serialise: Go rewrites it only when the source changed, and a running server keeps the
# inode it started from. The port is chosen only after the build, and held-port.py execs `env`,
# which execs the server, so the pid a caller started is still the server's.
flock ./.dispatch-e2e.lock go build -o ./dispatch-e2e ./cmd/dispatch
exec "${listen[@]}" env \
  "${broker_env[@]}" \
  "${file_store_env[@]}" \
  "${signin_env[@]}" \
  DATABASE_URL="$database_url" \
  DISPATCH_AGENT_TOKEN=e2e-token \
  DISPATCH_APP_CLIENT_ID=Iv1.e2efake \
  DISPATCH_APP_PEM_B64="$app_pem" \
  DISPATCH_GITHUB_API_BASE="http://127.0.0.1:$fake_github_port" \
  DISPATCH_IDENTITY=cookie \
  DISPATCH_INSECURE_COOKIE=1 \
  DISPATCH_LISTEN_HOST=127.0.0.1 \
  DISPATCH_NATS_DISABLED=1 \
  "${port_env[@]}" \
  DISPATCH_TEST_EMBEDDER_OFF="$embedder_off" \
  DISPATCH_TEST_HOOKS=1 \
  DISPATCH_TEST_MARK_WAIT="$mark_wait" \
  DISPATCH_TEST_ROSTER_UNCACHED=1 \
  DISPATCH_TEST_SETTLE_DELAY="$settle_delay" \
  DISPATCH_TEST_UNRECORDED_MARK_TTL="$unrecorded_mark_ttl" \
  DISPATCH_WEB_DIST="$web_dist" \
  ENVOY_URL="http://127.0.0.1:$fake_envoy_port" \
  GOMAXPROCS="$gomaxprocs" \
  HOME=/nonexistent \
  XDG_CACHE_HOME=/nonexistent \
  XDG_CONFIG_HOME=/nonexistent \
  XDG_DATA_HOME=/nonexistent \
  ./dispatch-e2e
