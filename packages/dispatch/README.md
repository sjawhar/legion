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

The desktop shell has a sidebar, issue content, and review margin. Issue content has Spec,
Conversation, Children, and Artifacts tabs; the Spec document renders and edits through
`@legion/proof-editor`, with Yjs presence and margin-linked marks. The margin holds Comments and
Pinned. Each issue-owned anchored comment thread is one card with flat replies, inline reply and
author-edit controls, mark-aligned cross-hover, and resolved history behind `Resolved (N)`.
Project-document thread replies are handled by the margin-by-owner surface. Below the `xl`
breakpoint, navigation is a drawer and the margin is a bottom sheet; opening a thread uses a
full-height view with a bottom-pinned composer. Controls use 44 px minimum touch targets.

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
`PLAYWRIGHT_BASE_URL` selects a deployed server. The script builds the server
with the caller's `go` into `packages/envoy/dispatch-e2e` and execs it, so a
SIGTERM to the pid it hands its caller stops the server. It pins every server
setting: cookie identity, the production mode, with `alice` and `bob` signed in through the
server's dev sign-in route (`DISPATCH_DEV_SIGNIN=1`, fenced to a loopback
origin, listener and database, with a signing key the server generates for its
process), `DISPATCH_NATS_DISABLED=1`, the fake GitHub origin, a throwaway App
key, a loopback listen host and the suite's dashboard origin. Address the
harness as `127.0.0.1:<port>`, never `localhost`: under that flag the router
refuses any other `Host`. The server process has no caller Home or XDG
directory and receives no inherited `DISPATCH_*`, `ENVOY_*` or `NATS_*`
variable, so neither a shell setting nor `~/.config/opencode/envoy.json` /
`~/.local/share/dispatch` can redirect it.

`DATABASE_URL` is required and must name an isolated loopback database:
`e2e/seed.ts` truncates it before every scenario and never selects a shared
default, and the dev sign-in flag refuses a non-loopback host. psql runs without
`PGHOSTADDR` (`e2e/psql.ts`), which would otherwise send it somewhere the server
never checked. The harness ports `DISPATCH_E2E_PORT` (default `8777`),
`FAKE_ENVOY_PORT` (default `9021`) and `FAKE_GITHUB_PORT` (default `9022`) are
its other inputs, resolved for the whole suite by `e2e/harness-ports.ts`. A run
starts its own servers on those three ports and refuses before any of them
starts if one is taken, so it never truncates the database behind a server it
did not start;
`DISPATCH_E2E_REUSE_SERVERS=1` is the opt-in for running against a harness you
started yourself. `AGENTS.md`'s end-to-end section states that rule in full —
the accepted values, what a bad or duplicated port does, and which invocations
skip the probe. The harness starts `e2e/fake-envoy.ts` on `FAKE_ENVOY_PORT`
and that listener is the only Envoy the server ever talks to; tests seed its
live sessions with `setLiveSessions` from `e2e/agents.ts`.

Run the local harness with its isolated database available:

```bash
cd packages/dispatch
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  bun run e2e
```

## Acceptance run against the deployed image

The `acceptance` Compose profile runs Playwright against a locally built Dispatch image with its
own Postgres volume and database. It signs `alice` and `bob` in through the dev sign-in route, as
the local harness does, so the image must be built from a tree that has the route. It also uses an
acceptance-only agent token, disabled NATS, and a private loopback fake Envoy with no token; the
server's required `DISPATCH_ACCEPTANCE_ENVOY_URL` must name the same port that
`e2e:deployed` starts with `FAKE_ENVOY_PORT`. The fake Envoy makes subscriber, Agents-page, and
fixture-hook rows exercise the deployed server instead of a real session. Fake GitHub rows still
skip because the acceptance profile does not configure that listener. The profile starts only the
named acceptance service and its database dependency. Compose still interpolates every service in
the file, and the production `dispatch` service requires five variables the acceptance run never
uses, so the recipe passes placeholders for them.

```bash
cd packages/envoy/deploy/compose
unused=(DATABASE_URL=unused DISPATCH_AGENT_TOKEN=unused DISPATCH_ALLOWED_LOGINS=unused DISPATCH_SERVER_URL=unused NATS_URLS=unused)
acceptance_envoy_port=19021
acceptance_envoy_url="http://127.0.0.1:${acceptance_envoy_port}"
env "${unused[@]}" DISPATCH_ACCEPTANCE_PG_PORT=55516 DISPATCH_ACCEPTANCE_ENVOY_URL="$acceptance_envoy_url" ENVOY_IMAGE_TAG=pr4-local docker compose -f dispatch.compose.yml build dispatch
env "${unused[@]}" DISPATCH_ACCEPTANCE_PG_PORT=55516 DISPATCH_ACCEPTANCE_ENVOY_URL="$acceptance_envoy_url" ENVOY_IMAGE_TAG=pr4-local docker compose -p dispatch-acceptance -f dispatch.compose.yml --profile acceptance up -d dispatch-acceptance
cd ../../../dispatch
FAKE_ENVOY_PORT="$acceptance_envoy_port" \
PLAYWRIGHT_BASE_URL=http://127.0.0.1:8767 \
PLAYWRIGHT_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55516/dispatch_acceptance?sslmode=disable' \
E2E_AGENT_TOKEN=acceptance-token \
bun run e2e:deployed
cd ../envoy/deploy/compose
env "${unused[@]}" DISPATCH_ACCEPTANCE_PG_PORT=55516 DISPATCH_ACCEPTANCE_ENVOY_URL="$acceptance_envoy_url" ENVOY_IMAGE_TAG=pr4-local docker compose -p dispatch-acceptance -f dispatch.compose.yml --profile acceptance down -v
docker rmi ghcr.io/sjawhar/legion/envoy:pr4-local
```

`e2e/seed.ts` truncates its database before each scenario. Always set
`PLAYWRIGHT_DATABASE_URL` to an isolated test database when using a deployed URL.
The suite has five projects. `chromium` and `iphone` run every spec; the iPhone project uses
Chromium with iPhone 13 viewport, touch, and user-agent emulation. `webkit` runs
`e2e/collab-cursor.e2e.ts`, since where a caret lands beside a collaborator's cursor differs by
engine, and `firefox` runs `e2e/code-line-replace.e2e.ts`, since Firefox's own editing
mishandles text typed over what follows a block's last line break; both also run
`e2e/keyboard-agents-picker.e2e.ts`, whose keyboard rule rests on each engine's select dispatch.
`webkit-iphone` runs the live view's two phone-layout rows of `e2e/agent-view.e2e.ts` in WebKit
with the iPhone 13 profile, since iOS Safari is the engine its keyboard cap exists for.
`bun run e2e:install` installs all three browsers.

## Phone check

Browser emulation covers responsive layout. A phone acceptance check runs against
a tailnet-reachable Dispatch server: configure `DISPATCH_LISTEN_HOST` with the
host's Tailscale IPv4 address (`tailscale ip -4`), use that tailnet address and
`DISPATCH_PORT`, open the Inbox from the phone, and answer an open ask. Set
`DISPATCH_INSECURE_COOKIE=1` only for HTTP; HTTPS keeps the normal secure cookie
setting.
