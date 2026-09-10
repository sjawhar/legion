#!/usr/bin/env bash
set -euo pipefail

: "${DATABASE_URL:=postgres://postgres:dispatch@127.0.0.1:55432/dispatch_c?sslmode=disable}"
export DATABASE_URL

: "${DISPATCH_E2E_PORT:=8777}"
export DISPATCH_E2E_PORT

export DISPATCH_NATS_DISABLED=1

cd "$(dirname "$0")/../../envoy"
exec env \
  DISPATCH_IDENTITY=header:X-Dispatch-User \
  DISPATCH_ALLOWED_LOGINS=alice,bob \
  DISPATCH_AGENT_TOKEN=e2e-token \
  DISPATCH_WEB_DIST=../dispatch/web/dist \
  DISPATCH_PORT="$DISPATCH_E2E_PORT" \
  DISPATCH_TEST_HOOKS=1 \
  go run ./cmd/dispatch
