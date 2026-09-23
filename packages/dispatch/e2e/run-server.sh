#!/usr/bin/env bash
set -euo pipefail

# The suite owns every setting of the server it starts. Nothing about where that server points
# may come from the caller's environment: a Legion pane exports ENVOY_URL (the real listener) and
# DISPATCH_URL/DISPATCH_TOKEN_FILE, and a developer's shell can carry a GitHub App, a cookie
# signing key, an Envoy token or a dashboard origin left over from other work. So the three
# genuine harness inputs are read first, the whole DISPATCH_*/ENVOY_*/NATS_* namespace is then
# dropped — which also closes the next variable the server learns to read — and the server is
# launched with an environment listed in full below.
#
# DATABASE_URL is an input, not a leak: the harness has no fake Postgres, so the database is
# always the caller's to name (CI's service container, a per-agent database on a shared box), and
# e2e/seed.ts resolves the same value for the fixtures it writes over psql.
database_url="${DATABASE_URL:-postgres://postgres:dispatch@127.0.0.1:55432/dispatch_c?sslmode=disable}"
# The ports are inputs for the same reason: playwright.config.ts reads DISPATCH_E2E_PORT and
# FAKE_ENVOY_PORT/FAKE_GITHUB_PORT too, so the whole harness moves together.
e2e_port="${DISPATCH_E2E_PORT:-8777}"
fake_envoy_port="${FAKE_ENVOY_PORT:-9021}"
fake_github_port="${FAKE_GITHUB_PORT:-9022}"

mapfile -t inherited < <(compgen -e)
for name in "${inherited[@]}"; do
  case "$name" in
    DISPATCH_* | ENVOY_* | NATS_*) unset "$name" ;;
  esac
done

# Architecture-source access checks run against the fake GitHub listener with
# throwaway App credentials: a fresh RSA key per run (nothing secret to
# commit), a dummy client secret because LoadAppFromEnv requires one whenever
# the client id is set, and the trusted-header ack the server demands when App
# credentials meet header identity.
app_pem_b64="$(openssl genrsa 2048 2>/dev/null | base64 -w0)"

cd "$(dirname "$0")/../../envoy"
exec env \
  DATABASE_URL="$database_url" \
  DISPATCH_AGENT_TOKEN=e2e-token \
  DISPATCH_ALLOWED_LOGINS=alice,bob \
  DISPATCH_APP_CLIENT_ID=Iv1.e2efake \
  DISPATCH_APP_CLIENT_SECRET=e2e-dummy-secret \
  DISPATCH_APP_PEM_B64="$app_pem_b64" \
  DISPATCH_GITHUB_API_BASE="http://127.0.0.1:$fake_github_port" \
  DISPATCH_IDENTITY=header:X-Dispatch-User \
  DISPATCH_IDENTITY_HEADER_TRUSTED=1 \
  DISPATCH_LISTEN_HOST=127.0.0.1 \
  DISPATCH_NATS_DISABLED=1 \
  DISPATCH_PORT="$e2e_port" \
  DISPATCH_SERVER_URL="http://127.0.0.1:$e2e_port" \
  DISPATCH_TEST_HOOKS=1 \
  DISPATCH_WEB_DIST=../dispatch/web/dist \
  ENVOY_URL="http://127.0.0.1:$fake_envoy_port" \
  go run ./cmd/dispatch
