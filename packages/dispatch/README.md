# Dispatch

Dispatch is the React single-page application for the native Dispatch workspace. A
workspace issue holds its status, primary specification, open asks, comments and
suggestions, artifacts, and durable event history. The Go server in
`packages/envoy/cmd/dispatch` serves the production build from `web/dist`.

## Application shape

`AuthGate` requests `GET /auth/whoami` and sends unauthenticated visitors to the
Google sign-in flow. All application requests are same-origin. React Router serves
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
variable, so neither a shell setting nor `~/.config/legion/envoy.json` /
`~/.local/share/dispatch` can redirect it. The one way in from outside is the
broker switch below: a URL in `DISPATCH_E2E_AGENT_SECRETS_URL`, with its bearer
in `DISPATCH_E2E_AGENT_SECRETS_TOKEN_FILE`, points the server's credential
requests at a broker the caller runs, as the secrets broker's docs rig
(`docs/site/media/broker/rig.sh`) does.

`DATABASE_URL` is required and must name an isolated loopback database:
`e2e/seed.ts` truncates it before every scenario and never selects a shared
default, and the dev sign-in flag refuses a non-loopback host. psql runs without
`PGHOSTADDR` (`e2e/psql.ts`), which would otherwise send it somewhere the server
never checked. The harness ports `DISPATCH_E2E_PORT` (default `8777`),
`FAKE_ENVOY_PORT` (default `9021`), `FAKE_GITHUB_PORT` (default `9022`),
`PLAIN_HTTP_PORT` (default `9023`) and `FAKE_BROKER_PORT` (default `9024`) are
its other inputs, resolved for the whole suite by `e2e/harness-ports.ts`. A
local run starts its own servers on those five ports and refuses before any of
them starts if one is taken, so it never
truncates the database behind a server it did not start;
`DISPATCH_E2E_REUSE_SERVERS=1` is the opt-in for running against a harness you
started yourself. `AGENTS.md`'s end-to-end section states that rule in full —
the accepted values, what a bad or duplicated port does, and which invocations
skip the probe. The harness starts `e2e/fake-envoy.ts` on `FAKE_ENVOY_PORT`
and that listener is the only Envoy the server ever talks to; tests seed its
live sessions with `setLiveSessions` from `e2e/agents.ts`. It starts
`e2e/fake-broker.ts` on `FAKE_BROKER_PORT` as the secrets broker, so the
credential-request feature is on, and tests seed its pending requests with
`setPendingCredentialRequests` from `e2e/fake-broker-helpers.ts`; an empty
`DISPATCH_E2E_AGENT_SECRETS_URL` runs the suite with no broker instead, as a
deployment that configures none (`AGENTS.md` has the switch). It also starts
`e2e/plain-http-proxy.ts` on `PLAIN_HTTP_PORT` for the plain-HTTP project.

Run the local harness with its isolated database available:

```bash
cd packages/dispatch
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  bun run e2e
```

## Local instance

`bun run dispatch:local`, from the repository root or from this package, brings up a seeded
Dispatch signed in as a person, to prove a change to a surface only a human acts on (the Inbox and
its snooze, the Agents page and its direct messages, a broadcast) with no production step. It is
the end-to-end harness, left running: it builds the SPA, starts every listener the Playwright
config starts (the fake Envoy, the plain-HTTP proxy, the fake GitHub, the fake secrets broker and
`e2e/run-server.sh`) on the same ports, empties the database, seeds `e2e/workspace.ts`, checks over
HTTP that a cookie from the dev sign-in route reads `/auth/whoami`, the Inbox, the broadcasts and
the agents, and prints:

- the URL that opens the dashboard signed in as `alice` (and one for `bob`),
- the seeded broadcast's page,
- the agent bearer (`e2e-token`) a local Oh My Pi session reaches this instance with,
- the database, the fake Envoy's session list and its reseed endpoint,
- the command that runs a spec against it.

Open exactly the printed `127.0.0.1` origin: `localhost` is another `Host`, and the server refuses
it (`421 HOST_MISMATCH`). Ctrl-C stops every process it started, and so does a failure at any
step; the database's container stays.

Its inputs are the harness's own. `DATABASE_URL`, when set, is used as given: it must be a single
`postgres://` URL whose host (its `host` query parameter when it has one, as for libpq) is
`127.0.0.1`, `localhost`, `[::1]` or a Unix socket directory, and it is truncated and reseeded on
every start. Unset, the command runs `packages/envoy/scripts/dev-postgres.sh`, which creates,
starts or reuses the `dispatch-pg` container, and creates database `dispatch_local` in it once.
That script refuses a `dispatch-pg` on any image but its pinned pgvector one, since Dispatch's
migrations need the `vector` extension, and prints the commands that move the container's data
volume onto that image, quoted so they paste as they read. The harness ports
(`DISPATCH_E2E_PORT`, `FAKE_ENVOY_PORT`, `PLAIN_HTTP_PORT`, `FAKE_GITHUB_PORT`,
`FAKE_BROKER_PORT`) move the instance, and the command refuses before anything starts when one is
taken. psql runs without `PGHOSTADDR` (`e2e/psql.ts`), and a Ctrl-C ends a psql in flight too.

A spec runs against the instance with the printed command, which sets
`DISPATCH_E2E_REUSE_SERVERS=1` and the same `DATABASE_URL`; each spec empties the database and the
fake Envoy, so restart the command to reseed it. `bun run build:web` while it runs refreshes the
SPA, which the server reads from `web/dist` on each request. `docker rm -f dispatch-pg` removes the
container and every database in it.

## Acceptance run against the deployed image

The `dispatch.acceptance.compose.yml` Compose file builds a Dispatch image from this tree and runs
it for Playwright with its own Postgres volume and database. It signs `alice` and `bob` in through
the dev sign-in route, as the local harness does. It also uses an acceptance-only agent token,
disabled NATS, and a private loopback fake Envoy with no token. `FAKE_ENVOY_PORT` and
`ENVOY_IMAGE_TAG` are its only required inputs: the acceptance service derives its `ENVOY_URL`
from `FAKE_ENVOY_PORT`, and `e2e:deployed` starts the fake listener on it. Before any row runs,
`e2e:deployed` puts a session in that fake and requires the server's `GET /api/v1/agents` to list
it, so a server reading another Envoy listener refuses the run instead of answering it from the
wrong sessions (`e2e/preflight.ts`). `e2e:deployed` also starts the plain-HTTP proxy on
`PLAIN_HTTP_PORT`. The fake Envoy makes subscriber, Agents-page, and fixture-hook rows exercise
the deployed server instead of a real session. Fake GitHub rows still skip because the acceptance
service does not configure that listener.

```bash
cd packages/envoy/deploy/compose
acceptance_envoy_port=19061
acceptance_plain_http_port=19062
acceptance_dispatch_port=18767
acceptance_pg_port=55660
acceptance_image_tag=dispatch-acceptance-local
acceptance_compose_project=dispatch-acceptance
compose=(env FAKE_ENVOY_PORT="$acceptance_envoy_port" DISPATCH_ACCEPTANCE_PORT="$acceptance_dispatch_port" DISPATCH_ACCEPTANCE_PG_PORT="$acceptance_pg_port" ENVOY_IMAGE_TAG="$acceptance_image_tag" docker compose -p "$acceptance_compose_project" -f dispatch.acceptance.compose.yml)
"${compose[@]}" build dispatch-acceptance
"${compose[@]}" up -d dispatch-acceptance
cd ../../../dispatch
FAKE_ENVOY_PORT="$acceptance_envoy_port" \
PLAIN_HTTP_PORT="$acceptance_plain_http_port" \
PLAYWRIGHT_BASE_URL="http://127.0.0.1:${acceptance_dispatch_port}" \
PLAYWRIGHT_DATABASE_URL="postgres://postgres:dispatch@127.0.0.1:${acceptance_pg_port}/dispatch_acceptance?sslmode=disable" \
E2E_AGENT_TOKEN=acceptance-token \
bun run e2e:deployed
cd ../envoy/deploy/compose
"${compose[@]}" down -v
docker rmi "ghcr.io/sjawhar/legion/envoy:${acceptance_image_tag}"
```

`e2e/seed.ts` truncates its database before each scenario. Always set
`PLAYWRIGHT_DATABASE_URL` to an isolated test database when using a deployed URL.
The suite has six projects. `chromium` and `iphone` run every spec except the two plain-HTTP specs;
the iPhone project uses Chromium with iPhone 13 viewport, touch, and user-agent emulation. `webkit`
runs `e2e/collab-cursor.e2e.ts`, since where a caret lands beside a collaborator's cursor differs by
engine, and `firefox` runs `e2e/code-line-replace.e2e.ts`, since Firefox's own editing
mishandles text typed over what follows a block's last line break; both also run
`e2e/keyboard-agents-picker.e2e.ts`, whose keyboard rule rests on each engine's select dispatch.
`webkit-iphone` runs the live view's two phone-layout rows of `e2e/agent-view.e2e.ts`, and the
rows of `e2e/phone-conversation.e2e.ts` that check what the Conversation's floating pills cover, in
WebKit with the iPhone 13 profile, since iOS Safari is the engine those layouts exist for.
`chromium-plain-http` runs `e2e/plain-http-origin.e2e.ts` and `e2e/plain-http-proxy.e2e.ts`, with
the page opened at `http://dispatch-e2e.test:<PLAIN_HTTP_PORT>` (Chromium maps that name to the
local proxy), a plain-HTTP origin that is not loopback and so not a secure context. The proxy
forwards to the dev sign-in server at its loopback origin. The project proves a document takes a
new paragraph and a comment renders formatted there, where `crypto.randomUUID` does not exist, and
that the proxy refuses any other `Host` as the server's own fence does and keeps no request open
upstream once a page has closed.
`bun run e2e:install` installs all three browsers.

## Phone check

Browser emulation covers responsive layout. A phone acceptance check runs against
a tailnet-reachable Dispatch server: configure `DISPATCH_LISTEN_HOST` with the
host's Tailscale IPv4 address (`tailscale ip -4`), use that tailnet address and
`DISPATCH_PORT`, open the Inbox from the phone, and answer an open ask. Set
`DISPATCH_INSECURE_COOKIE=1` only for HTTP; HTTPS keeps the normal secure cookie
setting.
