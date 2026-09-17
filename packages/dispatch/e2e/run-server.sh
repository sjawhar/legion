#!/usr/bin/env bash
set -euo pipefail

: "${DATABASE_URL:=postgres://postgres:dispatch@127.0.0.1:55432/dispatch_c?sslmode=disable}"
export DATABASE_URL

: "${DISPATCH_E2E_PORT:=8777}"
export DISPATCH_E2E_PORT

export DISPATCH_NATS_DISABLED=1
export ENVOY_URL="${ENVOY_URL:-http://127.0.0.1:${FAKE_ENVOY_PORT:-9021}}"

# Architecture-source access checks run against the fake GitHub listener with
# throwaway App credentials: a fresh RSA key per run (nothing secret to
# commit), a dummy client secret because LoadAppFromEnv requires one whenever
# the client id is set, and the trusted-header ack the server demands when App
# credentials meet header identity.
export DISPATCH_GITHUB_API_BASE="${DISPATCH_GITHUB_API_BASE:-http://127.0.0.1:${FAKE_GITHUB_PORT:-9022}}"
export DISPATCH_APP_CLIENT_ID="${DISPATCH_APP_CLIENT_ID:-Iv1.e2efake}"
export DISPATCH_APP_CLIENT_SECRET="${DISPATCH_APP_CLIENT_SECRET:-e2e-dummy-secret}"
if [ -z "${DISPATCH_APP_PEM_B64:-}" ]; then
  DISPATCH_APP_PEM_B64="$(openssl genrsa 2048 2>/dev/null | base64 -w0)"
fi
export DISPATCH_APP_PEM_B64
export DISPATCH_IDENTITY_HEADER_TRUSTED=1

cd "$(dirname "$0")/../../envoy"
exec env \
  DISPATCH_IDENTITY=header:X-Dispatch-User \
  DISPATCH_ALLOWED_LOGINS=alice,bob \
  DISPATCH_AGENT_TOKEN=e2e-token \
  DISPATCH_WEB_DIST=../dispatch/web/dist \
  DISPATCH_PORT="$DISPATCH_E2E_PORT" \
  DISPATCH_TEST_HOOKS=1 \
  go run ./cmd/dispatch
