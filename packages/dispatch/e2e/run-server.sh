#!/usr/bin/env bash
set -euo pipefail

: "${DATABASE_URL:=postgres://postgres:dispatch@127.0.0.1:55432/dispatch_c?sslmode=disable}"
export DATABASE_URL

: "${DISPATCH_E2E_PORT:=8777}"
export DISPATCH_E2E_PORT

cd "$(dirname "$0")/../../envoy"
exec env \
  DISPATCH_IDENTITY=header:X-Dispatch-User \
  DISPATCH_ALLOWED_LOGINS=alice,bob \
  DISPATCH_AGENT_TOKEN=e2e-token \
  DISPATCH_NATS_DISABLED=1 \
  DISPATCH_WEB_DIST=../dispatch/web/dist \
  DISPATCH_PORT="$DISPATCH_E2E_PORT" \
  go run ./cmd/dispatch
