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
# target ambiguous. The four ports are shared harness inputs because the
# Playwright config and its helpers resolve them too.
: "${DATABASE_URL:?DATABASE_URL must name an isolated Dispatch e2e database}"
database_url="$DATABASE_URL"
e2e_port="${DISPATCH_E2E_PORT:-8777}"
fake_envoy_port="${FAKE_ENVOY_PORT:-9021}"
fake_github_port="${FAKE_GITHUB_PORT:-9022}"
fake_broker_port="${FAKE_BROKER_PORT:-9024}"

# The secrets broker the server relays credential requests to, read before
# the sweep below drops every DISPATCH_* variable. DISPATCH_E2E_AGENT_SECRETS_URL
# is the harness's one switch, which e2e/harness-broker.ts reads the same way
# (no colon, so empty and unset differ): unset is e2e/fake-broker.ts with a
# throwaway UI bearer; empty is no broker, the credential feature off, as in a
# deployment that configures none; a URL is a broker the caller runs, whose
# bearer stays in DISPATCH_E2E_AGENT_SECRETS_TOKEN_FILE, which the server reads
# itself, so no argv carries it.
broker_env=()
if [ -z "${DISPATCH_E2E_AGENT_SECRETS_URL+set}" ]; then
  broker_env=(
    DISPATCH_AGENT_SECRETS_TOKEN=e2e-broker-token
    DISPATCH_AGENT_SECRETS_URL="http://127.0.0.1:$fake_broker_port"
  )
elif [ -n "$DISPATCH_E2E_AGENT_SECRETS_URL" ]; then
  : "${DISPATCH_E2E_AGENT_SECRETS_TOKEN_FILE:?DISPATCH_E2E_AGENT_SECRETS_TOKEN_FILE must accompany DISPATCH_E2E_AGENT_SECRETS_URL}"
  # The server reads the file from packages/envoy, where this script changes directory below, so a
  # path relative to the caller's directory is made absolute first.
  token_file="$DISPATCH_E2E_AGENT_SECRETS_TOKEN_FILE"
  [[ "$token_file" == /* ]] || token_file="$PWD/$token_file"
  broker_env=(
    DISPATCH_AGENT_SECRETS_TOKEN_FILE="$token_file"
    DISPATCH_AGENT_SECRETS_URL="$DISPATCH_E2E_AGENT_SECRETS_URL"
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
# every engine. DISPATCH_SERVER_URL stays
# http://127.0.0.1:$e2e_port: it is the origin the CSRF guard compares writes
# against, the only Host the router serves under the flag, and the Playwright
# config's baseURL. The flag also refuses a DATABASE_URL whose host is not
# loopback or a unix socket, and any DISPATCH_SIGNIN_* setting.
app_pem_b64="$(openssl genrsa 2048 2>/dev/null | base64 -w0)"

cd "$(dirname "$0")/../../envoy"
# Build first, in the caller's toolchain environment, and exec the binary itself. A server
# started as `go run` is the go command's child; the go command ignores only SIGINT and SIGQUIT,
# so a SIGTERM to it ends the go command and leaves the server listening. Playwright kills the
# whole process group and the skill-scenarios rig the whole process tree, but a caller that
# signals the one pid it started (`kill $!`) needs that pid to be the server. One binary per
# checkout, built under a lock so two harnesses starting at once serialise: Go rewrites it only
# when the source changed, and a running server keeps the inode it started from.
flock ./.dispatch-e2e.lock go build -o ./dispatch-e2e ./cmd/dispatch
exec env \
  "${broker_env[@]}" \
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
  DISPATCH_PORT="$e2e_port" \
  DISPATCH_SERVER_URL="http://127.0.0.1:$e2e_port" \
  DISPATCH_TEST_HOOKS=1 \
  DISPATCH_WEB_DIST=../dispatch/web/dist \
  ENVOY_URL="http://127.0.0.1:$fake_envoy_port" \
  HOME=/nonexistent \
  XDG_CACHE_HOME=/nonexistent \
  XDG_CONFIG_HOME=/nonexistent \
  XDG_DATA_HOME=/nonexistent \
  ./dispatch-e2e
