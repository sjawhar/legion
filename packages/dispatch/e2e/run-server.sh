#!/usr/bin/env bash
set -euo pipefail

# The server is a test fixture. It must not inherit a service endpoint,
# credential, data file or configuration from the shell that starts Playwright:
# a Legion pane exports ENVOY_URL (the real listener), DISPATCH_URL and
# DISPATCH_TOKEN_FILE, and a developer's shell can retain a GitHub App, a
# cookie signing key, an Envoy token or a dashboard origin from other work.
#
# DATABASE_URL is the one required caller input. The harness has no fake
# Postgres and e2e/seed.ts truncates the named database before every scenario,
# so silently selecting a shared default would make the destructive write
# target ambiguous. The three ports are shared harness inputs because the
# Playwright config and its helpers resolve them too.
: "${DATABASE_URL:?DATABASE_URL must name an isolated Dispatch e2e database}"
database_url="$DATABASE_URL"
e2e_port="${DISPATCH_E2E_PORT:-8777}"
fake_envoy_port="${FAKE_ENVOY_PORT:-9021}"
fake_github_port="${FAKE_GITHUB_PORT:-9022}"

# Resolve the concrete Go executable before hiding HOME. A mise shim can use
# the caller's configuration here, but the hermetic server process invokes the
# resolved Go binary directly and never asks mise to choose a version.
go_binary="$(go env GOROOT)/bin/go"

# `go run` still compiles the concrete server command, so provide its build
# and module caches explicitly rather than letting Go derive them from HOME.
go_cache="${GOCACHE:-$(go env GOCACHE)}"
go_mod_cache="${GOMODCACHE:-$(go env GOMODCACHE)}"

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
# credentials meet header identity. The fresh signing key makes the cookie
# layer just as isolated; nothing reaches the caller's persistent data dir.
app_pem_b64="$(openssl genrsa 2048 2>/dev/null | base64 -w0)"
signing_key="$(openssl rand -hex 32)"

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
  DISPATCH_SIGNING_KEY="$signing_key" \
  DISPATCH_TEST_HOOKS=1 \
  DISPATCH_WEB_DIST=../dispatch/web/dist \
  ENVOY_URL="http://127.0.0.1:$fake_envoy_port" \
  HOME=/nonexistent \
  GOCACHE="$go_cache" \
  GOMODCACHE="$go_mod_cache" \
  GOENV=off \
  XDG_CACHE_HOME=/nonexistent \
  XDG_CONFIG_HOME=/nonexistent \
  XDG_DATA_HOME=/nonexistent \
  "$go_binary" run ./cmd/dispatch
