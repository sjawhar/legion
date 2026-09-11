# Dispatch

Dispatch is the React single-page application for the native Dispatch workspace. A
workspace issue holds its status, primary specification, open asks, comments and
suggestions, artifacts, and durable event history. The Go server in
`packages/envoy/cmd/dispatch` serves the production build from `web/dist`.

## Application shape

`AuthGate` requests `GET /auth/whoami` and sends unauthenticated visitors to the
GitHub sign-in flow. All application requests are same-origin. React Router serves
the Inbox at `/`, an issue workspace at `/issues/:key/*`, and the human-only
repository-to-project settings page at `/settings`; TanStack Query and SSE keep
the issue, Inbox, documents, and sidebar current.

The desktop shell has a sidebar, issue content, and review margin. Issue content
has Spec, Log, Children, and Artifacts tabs; the Spec document renders and edits through
`@sjawhar/proof-editor`, with Yjs presence and margin-linked marks. The margin holds Comments and
Pinned. Below the `xl` breakpoint, navigation is a drawer and the margin is a bottom sheet.
Controls use 44 px minimum touch targets.

## Development

Run commands from this package:

```bash
bun run dev
bun run build:web
bun run typecheck
bun run lint
bun test
```

`bun run build:web` produces `web/dist` for the Dispatch server. `bun run dev`
runs the Vite development server for interface work.

## End-to-end tests

`bun run e2e` builds the SPA and drives Playwright against the real Go Dispatch
server and Postgres. The harness starts `e2e/run-server.sh` unless
`PLAYWRIGHT_BASE_URL` selects a deployed server. Its local defaults are
`DISPATCH_E2E_PORT=8777`, `DATABASE_URL` pointing at `dispatch_c`, trusted
`X-Dispatch-User` identity for `alice` and `bob`, and
`DISPATCH_NATS_DISABLED=1`.

Run the local harness with its isolated database available:

```bash
cd packages/dispatch
bun run e2e
```

## Acceptance run against the deployed image

The `acceptance` Compose profile runs Playwright against a locally built Dispatch image with its
own Postgres volume and database. It uses header identity for `alice` and `bob`, an
acceptance-only agent token, disabled NATS, and port 8767; it starts only the named acceptance
service and its database dependency.

```bash
cd packages/envoy/deploy/compose
DISPATCH_ACCEPTANCE_PG_PORT=55516 ENVOY_IMAGE_TAG=pr4-local docker compose -f dispatch.compose.yml build dispatch
DISPATCH_ACCEPTANCE_PG_PORT=55516 ENVOY_IMAGE_TAG=pr4-local docker compose -p dispatch-acceptance -f dispatch.compose.yml --profile acceptance up -d dispatch-acceptance
cd ../../../dispatch
PLAYWRIGHT_BASE_URL=http://127.0.0.1:8767 \
PLAYWRIGHT_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55516/dispatch_acceptance?sslmode=disable' \
E2E_AGENT_TOKEN=acceptance-token \
bun run e2e:deployed
cd ../envoy/deploy/compose
DISPATCH_ACCEPTANCE_PG_PORT=55516 ENVOY_IMAGE_TAG=pr4-local docker compose -p dispatch-acceptance -f dispatch.compose.yml --profile acceptance down -v
docker rmi ghcr.io/sjawhar/legion/envoy:pr4-local
```

`e2e/seed.ts` truncates its database before each scenario. Always set
`PLAYWRIGHT_DATABASE_URL` to an isolated test database when using a deployed URL.
The suite has `chromium` and `iphone` projects; the iPhone project uses Chromium
with iPhone 13 viewport, touch, and user-agent emulation.

## Phone check

Browser emulation covers responsive layout. A phone acceptance check runs against
a tailnet-reachable Dispatch server: configure `DISPATCH_LISTEN_HOST=0.0.0.0`, use
the host's tailnet address and `DISPATCH_PORT`, open the Inbox from the phone, and
answer an open ask. Set `DISPATCH_INSECURE_COOKIE=1` only for HTTP; HTTPS keeps the
normal secure cookie setting.
