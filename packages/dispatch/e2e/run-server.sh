#!/usr/bin/env bash
set -euo pipefail

: "${DATABASE_URL:=postgres://postgres:dispatch@127.0.0.1:55432/dispatch_c?sslmode=disable}"
export DATABASE_URL

cd "$(dirname "$0")/../../envoy"
exec env \
  DISPATCH_IDENTITY=header:X-Dispatch-User \
  DISPATCH_ALLOWED_LOGINS=alice,bob \
  DISPATCH_AGENT_TOKEN=e2e-token \
  DISPATCH_NATS_DISABLED=1 \
  DISPATCH_WEB_DIST=../dispatch/web/dist \
  go run ./cmd/dispatch
