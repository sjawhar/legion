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
# DISPATCH_E2E_PORT unset (or empty), the server serves on a port the kernel
# chooses, held from that moment until the server serves on it:
# scripts/e2e/lib/held-port.py opens a listening socket on 127.0.0.1:0 and the
# server inherits it (DISPATCH_LISTEN_FD). The server's own line reports it,
#   ... msg="dispatch: listening" addr=127.0.0.1:<port>
# on stderr, the line a caller waits for and reads the port from. Set, the
# server binds exactly that port, and an occupied one fails with "address
# already in use".
: "${DATABASE_URL:?DATABASE_URL must name an isolated Dispatch e2e database}"
: "${FAKE_ENVOY_PORT:?FAKE_ENVOY_PORT must name the fake Envoy (e2e/fake-envoy.ts) this server reads}"
: "${FAKE_GITHUB_PORT:?FAKE_GITHUB_PORT must name the fake GitHub (e2e/fake-github.ts) this server reads}"
database_url="$DATABASE_URL"
fake_envoy_port="$FAKE_ENVOY_PORT"
fake_github_port="$FAKE_GITHUB_PORT"
held_port="$(cd "$(dirname "$0")/../../.." && pwd)/scripts/e2e/lib/held-port.py"
if [ -n "${DISPATCH_E2E_PORT:-}" ]; then
  listen=()
  port_env=(DISPATCH_PORT="$DISPATCH_E2E_PORT" DISPATCH_SERVER_URL="http://127.0.0.1:$DISPATCH_E2E_PORT")
else
  listen=(python3 "$held_port" exec --fd-env DISPATCH_LISTEN_FD
    --env 'DISPATCH_PORT={port}' --env 'DISPATCH_SERVER_URL=http://127.0.0.1:{port}' --)
  port_env=()
fi

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
# alone, so no key is passed and nothing reaches the caller's persistent data
# dir; DISPATCH_INSECURE_COOKIE=1 keeps the cookie usable over plain HTTP in
# every engine. DISPATCH_SERVER_URL stays http://127.0.0.1:<port>, the port the
# server serves on: it is the origin the CSRF guard compares writes against,
# the only Host the router serves under the flag, and the Playwright config's
# baseURL. The flag also refuses a DATABASE_URL whose host is not
# loopback or a unix socket, and any DISPATCH_SIGNIN_* setting.
app_pem_b64="$(openssl genrsa 2048 2>/dev/null | base64 -w0)"

cd "$(dirname "$0")/../../envoy"
# Build first, in the caller's toolchain environment, and exec the binary itself. A server
# started as `go run` is the go command's child; the go command ignores only SIGINT and SIGQUIT,
# so a SIGTERM to it ends the go command and leaves the server listening. Playwright kills the
# whole process group and the skill-scenarios rig the whole process tree, but a caller that
# signals the one pid it started (`kill $!`) needs that pid to be the server. One binary per
# checkout, built under a lock so two harnesses starting at once serialise: Go rewrites it only
# when the source changed, and a running server keeps the inode it started from. The port is
# chosen only after the build, and held-port.py execs `env`, which execs the server, so the pid a
# caller started is still the server's.
flock ./.dispatch-e2e.lock go build -o ./dispatch-e2e ./cmd/dispatch
exec "${listen[@]}" env \
  "${broker_env[@]}" \
  "${file_store_env[@]}" \
  DATABASE_URL="$database_url" \
  DISPATCH_AGENT_TOKEN=e2e-token \
  DISPATCH_APP_CLIENT_ID=Iv1.e2efake \
  DISPATCH_APP_PEM_B64="$app_pem_b64" \
  DISPATCH_DEV_SIGNIN=1 \
  DISPATCH_GITHUB_API_BASE="http://127.0.0.1:$fake_github_port" \
  DISPATCH_IDENTITY=cookie \
  DISPATCH_INSECURE_COOKIE=1 \
  DISPATCH_LISTEN_HOST=127.0.0.1 \
  DISPATCH_NATS_DISABLED=1 \
  "${port_env[@]}" \
  DISPATCH_TEST_HOOKS=1 \
  DISPATCH_WEB_DIST=../dispatch/web/dist \
  ENVOY_URL="http://127.0.0.1:$fake_envoy_port" \
  HOME=/nonexistent \
  XDG_CACHE_HOME=/nonexistent \
  XDG_CONFIG_HOME=/nonexistent \
  XDG_DATA_HOME=/nonexistent \
  ./dispatch-e2e
