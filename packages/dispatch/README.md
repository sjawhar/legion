# Dispatch

Dispatch is the React single-page application for the native Dispatch workspace. A
workspace issue holds its status, primary specification, open asks, comments and
suggestions, artifacts, and durable event history. The Go server in
`packages/envoy/cmd/dispatch` serves the production build from `web/dist`.

## Application shape

`AuthGate` requests `GET /auth/whoami` and sends unauthenticated visitors to the
Google sign-in flow. All application requests are same-origin. React Router serves
the Inbox at `/`, an issue workspace at `/issues/:key/*`, and the human-only
repository-to-project settings page at `/settings`; TanStack Query and the live socket's events
feed keep the issue, Inbox, documents, and sidebar current.

The desktop shell has a sidebar, issue content, and review margin. Issue content has Spec,
Conversation, Children, Artifacts, and Activity tabs; the Conversation lists one card per
conversation and the Activity tab holds the issue's whole event history. The Spec document
renders and edits through `@legion/proof-editor`, with Yjs presence and margin-linked marks. The
margin holds Comments and Pinned. Each issue-owned anchored comment thread is one card with flat
replies, inline reply and author-edit controls, mark-aligned cross-hover, and resolved history
behind `Resolved (N)`. Project-document thread replies are handled by the margin-by-owner
surface. Below the `xl` breakpoint, navigation is a drawer and the margin is a bottom sheet;
opening a thread uses a full-height view with a bottom-pinned composer. Controls use 44 px
minimum touch targets.

## Development

Run commands from this package:

```bash
bun run dev
bun run build:web
bun run typecheck
bun run lint
bun run test
```

`bun run build:web` produces `web/dist` for the Dispatch server. `bun run dev`
runs the Vite development server for interface work.

## End-to-end tests

`bun run e2e` builds the SPA and drives Playwright against the real Go Dispatch
server and Postgres. It runs Playwright through `e2e/run.ts`, which starts the
harness (`e2e/harness-start.ts`: the fakes, then `e2e/run-server.sh`, then the
plain-HTTP proxy) unless `PLAYWRIGHT_BASE_URL` selects a deployed server. The
script builds the server
with the caller's `go` into `packages/envoy/dispatch-e2e` and execs it, so a
SIGTERM to the pid it hands its caller stops the server. It pins every server
setting: cookie identity, the production mode, with `alice` and `bob` signed in through the
server's dev sign-in route (`DISPATCH_DEV_SIGNIN=1`, fenced to a loopback
origin, listener and database, with a signing key the server generates for its
process, or one key per run in two-process mode, below),
`DISPATCH_NATS_DISABLED=1`, the fake GitHub origin, a throwaway App
key, a loopback listen host and the suite's dashboard origin. Address the
harness as `127.0.0.1:<port>`, never `localhost`: under that flag the router
refuses any other `Host`. The server process has no caller Home or XDG
directory and receives no inherited `DISPATCH_*`, `ENVOY_*` or `NATS_*`
variable, so neither a shell setting nor `~/.config/legion/envoy.json` /
`~/.local/share/dispatch` can redirect it. The one way in from outside is the
broker switch below: a URL in `DISPATCH_E2E_AGENT_SECRETS_URL`, with the token
of a ServiceAccount that broker lists in
`DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE`, points the server's credential
requests at a broker the caller runs, as the secrets broker's docs rig
(`docs/site/media/broker/rig.sh`) does with the token its local issuer
(`packages/envoy/cmd/dev-oidc-issuer`) keeps.

`DATABASE_URL` is required and must name an isolated loopback database:
`e2e/seed.ts` truncates it before every scenario and never selects a shared
default, and the dev sign-in flag refuses a non-loopback host. psql runs without
`PGHOSTADDR` (`e2e/psql.ts`), which would otherwise send it somewhere the server
never checked. The harness ports `DISPATCH_E2E_PORT`, `FAKE_ENVOY_PORT`,
`FAKE_GITHUB_PORT`, `PLAIN_HTTP_PORT` and `FAKE_BROKER_PORT` are its other
inputs, resolved for the whole suite by `e2e/harness-ports.ts`. Unset, each
server serves on a port the OS chooses and holds until it serves: the Dispatch
server on a socket `scripts/e2e/lib/held-port.py` hands it, each Bun listener by
binding port 0. Each reports its port on its listening line, and `e2e/run.ts`
exports them before Playwright starts, so no run shares a port with another and
none truncates the database behind a server it did not start. Set, a port is
used as given, and an occupied one fails with `address already in use`.
Running `playwright test` without the wrapper refuses, naming `e2e/run.ts`,
unless every port is set and `DISPATCH_E2E_REUSE_SERVERS=1` says they name a
harness you started yourself. `AGENTS.md`'s end-to-end section states that
rule in full — the accepted values and what a bad or duplicated port does. The
harness starts `e2e/fake-envoy.ts` and that listener is the only Envoy the
server ever talks to; tests seed its
live sessions with `setLiveSessions` from `e2e/agents.ts`. It starts
`e2e/fake-broker.ts` as the secrets broker, so the
credential-request feature is on, and tests seed its pending requests with
`setPendingCredentialRequests` from `e2e/fake-broker-helpers.ts`; an empty
`DISPATCH_E2E_AGENT_SECRETS_URL` runs the suite with no broker instead, as a
deployment that configures none (`AGENTS.md` has the switch). It also starts
`e2e/plain-http-proxy.ts` for the plain-HTTP project.

Run the local harness with its isolated database available:

```bash
cd packages/dispatch
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  bun run e2e
```

Arguments after the config go to Playwright, as in
`bun e2e/run.ts -- bunx playwright test --config e2e/playwright.config.ts --project=chromium e2e/broadcast.e2e.ts`
(`bun run e2e` runs the same after the SPA build). `e2e/run.ts` stops the harness
when Playwright exits and exits with Playwright's status.

`e2e/run-server.sh` on its own is the server for a shell caller: with
`DATABASE_URL`, `FAKE_ENVOY_PORT`, `FAKE_GITHUB_PORT` and the broker switch's
input (`FAKE_BROKER_PORT`, or an empty `DISPATCH_E2E_AGENT_SECRETS_URL`) set and
`DISPATCH_E2E_PORT` unset, it writes
`msg="dispatch: listening" addr=127.0.0.1:<port>` to stderr once the server
serves; read the port from that line. `$!` of a backgrounded run is the server's
pid. A caller that needs the port before the server starts (the broker docs rig
names it in the broker's settings) starts `scripts/e2e/lib/held-port.py keep`
first and sets `DISPATCH_E2E_LISTEN_FROM` to the keeper's unix socket: the
server serves on the keeper's port.

Two-process mode runs two Dispatch servers against the one database, behind a round-robin proxy,
the way a rolling release serves a browser from two builds at once:

```bash
cd packages/dispatch
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  bun run e2e:multiprocess
```

It builds the dashboard twice (`web/dist`, and `web/dist-b` under another build id), then runs
Playwright through `e2e/run.ts` with `DISPATCH_E2E_PROCESSES=2`: Dispatch A on
`DISPATCH_E2E_PORT_A` serving `web/dist`, Dispatch B on `DISPATCH_E2E_PORT_B` serving
`DISPATCH_E2E_WEB_DIST_B` (here `web/dist-b`; default A's), and `e2e/round-robin-proxy.ts` on
`DISPATCH_E2E_PORT`, which alternates requests and websockets between them. As every harness
port, each is used as given when set and chosen by the OS when unset; the proxy starts before
the servers, since both serve its origin, so A's and B's ports are held first (a
`held-port.py keep` each) and each server serves on its keeper's port. Both servers sign
cookies with one key the run generates, so a person stays signed in whichever answers. It runs
the `*.multiprocess.e2e.ts` specs but the sign-in spec below, in Chromium, which the default run
leaves out; arguments after `--` reach Playwright
(`bun run e2e:multiprocess -- <name>.multiprocess.e2e.ts`). A spec
pins a request to one process with the `X-Dispatch-E2E-Process: a|b` header or a whole browser
context with the `dispatch-e2e-process` cookie, and reads which process served what from the
proxy's `/_proxy/requests` and `/_proxy/sockets`; `AGENTS.md`'s end-to-end section has the whole
contract. Any `DISPATCH_E2E_PROCESSES` other than `1`, `2` or unset is refused.

`DISPATCH_E2E_SIGNIN=oidc` signs people in as a deployment does instead of through dev sign-in:
the harness also starts `packages/envoy/cmd/dev-oidc-issuer` (`e2e/run-signin-issuer.sh`), and
both servers take it as their sign-in issuer, under one `DISPATCH_SIGNING_KEY` per run. Only
`e2e/signin.multiprocess.e2e.ts` runs then, which starts a sign-in at `/auth/start` on Dispatch A
and completes it at `/auth/callback` on B:

```bash
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  DISPATCH_E2E_SIGNIN=oidc bun run e2e:multiprocess
```

It needs `DISPATCH_E2E_PROCESSES=2`, which `e2e:multiprocess` sets; any value other than `oidc`
or unset is refused.

## Local instance

`bun run dispatch:local`, from the repository root or from this package, brings up a seeded
Dispatch signed in as a person, to prove a change to a surface only a human acts on (the Inbox and
its snooze, the Agents page and its direct messages, a broadcast) with no production step. It is
the end-to-end harness, left running: it builds the SPA, starts the harness through
`e2e/harness-start.ts` as `e2e/run.ts` does (the fake Envoy, the fake GitHub, the fake secrets
broker, `e2e/run-server.sh` and the plain-HTTP proxy) on ports the OS chooses, empties the
database, seeds `e2e/workspace.ts`, checks over
HTTP that a cookie from the dev sign-in route reads `/auth/whoami`, the Inbox, the broadcasts and
the agents, and prints:

- the URL that opens the dashboard signed in as `alice` (and one for `bob`),
- the seeded broadcast's page,
- the agent bearer (`e2e-token`) a local Oh My Pi session reaches this instance with,
- the database, the fake Envoy's session list and its reseed endpoint,
- the command that runs a spec against it, with the ports it serves on.

Open exactly the printed `127.0.0.1` origin: `localhost` is another `Host`, and the server refuses
it (`421 HOST_MISMATCH`). Ctrl-C stops every process it started, and so does a failure at any
step; the database's container stays.

Its inputs are the harness's own. `DATABASE_URL`, when set, is used as given: it must be a single
`postgres://` URL whose host (its `host` query parameter when it has one, as for libpq) is
`127.0.0.1`, `localhost`, `[::1]` or a Unix socket directory, and it is truncated and reseeded on
every start. Unset, the command runs `packages/envoy/scripts/dev-postgres.sh`, which creates,
starts or reuses the `dispatch-pg` container, and creates database `dispatch_local` in it once.
That script runs the container from its pinned image, pgvector on PostgreSQL 18, the version
Dispatch's tests and dev databases run; Dispatch's migrations need its `vector` extension. It
refuses a `dispatch-pg` on any other image or major and prints the commands that dump its
databases and restore them into a new container on the pinned image, quoted so they paste as they
read. The harness ports (`DISPATCH_E2E_PORT`, `FAKE_ENVOY_PORT`, `PLAIN_HTTP_PORT`,
`FAKE_GITHUB_PORT`, `FAKE_BROKER_PORT`) are unset unless you need a fixed one: unset, the OS
chooses each, so two instances never collide; set, the port is used as given, and an occupied one
fails with `address already in use`. psql runs without `PGHOSTADDR` (`e2e/psql.ts`), and a Ctrl-C
ends a psql in flight too.

A spec runs against the instance with the printed command, which sets
`DISPATCH_E2E_REUSE_SERVERS=1`, the five ports and the same `DATABASE_URL`; each spec empties the
database and the
fake Envoy, so restart the command to reseed it. `bun run build:web` while it runs refreshes the
SPA, which the server reads from `web/dist` on each request. `docker rm -f -v dispatch-pg` removes
the container, its anonymous data volume and every database in it.

## Acceptance drivers

`e2e/acceptance/block-graph.sh` drives the block graph end to end against a running server, as an
agent sees it, through the HTTP API and the real `dispatch` CLI
(`packages/envoy-client/bin/dispatch.ts`, run in an environment the script builds, so a calling
shell's Dispatch URL, token file and Envoy configuration never reach it). It covers a comment
citing a block and the block's backlinks, a deleted and restored block, a pinned block reference
that changes and is removed, `GET /api/v1/block-links` between the blocks of two documents, each
block's changed version, every block `part_of` its document, a link in a list item, the six facts
every node carries (`made_by`, `made_at`, `changed_by`, `changed_at`, `parent`, `parent_version`)
for an issue, a document, a file, a picture, a block, a removed pinned block, an ask, a comment, a
message, a session and a component, read by its own reference, as an edge's other end and on the
`Node:` line of `dispatch read`, and the older `GET /api/v1/artifacts/{id}/references` route. It
also covers the blocks of comments and messages: a mention in a comment starts at the comment's
block holding it, a node of kind `block`, id `comment/<uuid>#b<n>`, whose `parent` is the comment
(A10);
a comment or message reply that names a block (`--reply-to <comment>#b-b2`,
`--in-reply-to <message>#b-b2`) is a `replies_to` edge to that block, and `dispatch read` quotes it
(A7); the facts of a comment's and a message's block (A11); a block's id kept through a person's
move, reword, split, deletion and restore over `PATCH /api/v1/comments/{id}` with carried ids, and
a body holding block HTML read back byte for byte (A13); and a link pinned to a comment's block that
reads `changed`, then `removed` (A15). Each passing check prints `PASS: <section> <what>`; the first
failure prints `FAIL: <section> <what>: <observed>` and exits 1. Its inputs:

- `DISPATCH_URL`, the server;
- `DISPATCH_TOKEN`, an agent bearer, or `DISPATCH_TOKEN_FILE`, a file holding one, which the script
  reads itself and never prints;
- `PROJECT`, an existing project key;
- `COMPONENT_SOURCE`, optional, where the component nodes come from (unset, the script reads no
  component node and prints one
  `SKIP: R12 component: COMPONENT_SOURCE is unset, so no component node is read; …` line):
  - `fixture` seeds the harness's fake GitHub on `FAKE_GITHUB_PORT` (9022 under
    `bun run dispatch:local`) with a two-component model in `acme/r12-arch` (`web`, part of
    `platform`), replacing every repository the fake served, adds that repository as an
    architecture source of `PROJECT`, syncs it, and removes the source on exit, also when a check
    fails;
  - `existing` reads the first project whose architecture holds a component with a parent, and
    writes nothing to it; a server with none is a `FAIL`;
  - `none` proves `GET /api/v1/settings/architecture-sources` is `[]` and prints
    `PASS: R12 component: this server has no architecture source, so it holds no component node`;
    a source present is a `FAIL` naming `COMPONENT_SOURCE=existing`.
- `DEV_SIGNIN_LOGIN`, optional, a login the server's dev sign-in route (`/auth/_dev/signin`) signs
  in: `bun run dispatch:local` has the route, a deployed server does not. Editing a comment is a
  person's write, by its author alone, so the A13 and A15 comment sections sign that person in with a
  cookie jar, write the comments that person edits, and send each edit with the dashboard's origin.
  Unset, each of those two sections prints one
  `SKIP: … PATCH /api/v1/comments/{id} is human-only and this server has no dev sign-in: DEV_SIGNIN_LOGIN is unset; …`
  line, and every other section runs.

It writes two issues in `PROJECT`: the first holds the spec, comments, messages and the documents
`cites.md`, `decisions.md`, `changes.md` and `list.md`; the second is its child, retitled, holding an
ask the script rewords, a comment on a quote of its spec and a reply, a message and a reply, and the
uploads `notes.txt` and `pixel.png`. It resolves the ask and closes both issues, also when a check
fails. Against `bun run dispatch:local`:

```bash
DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
  COMPONENT_SOURCE=fixture FAKE_GITHUB_PORT=9022 DEV_SIGNIN_LOGIN=alice \
  bash packages/dispatch/e2e/acceptance/block-graph.sh
```

`e2e/acceptance/block-coverage.sh` takes the first three inputs, with `PROJECT` optional (unset,
every project), and only reads. For every document it can list (each project's documents and each
issue's documents), it counts the document as not indexed when `GET /api/v1/artifacts/{id}/blocks`
lists blocks while `GET /api/v1/references?to=<document>&kind=part_of` lists none, and prints
`documents=<n> indexed=<n> not_indexed=<n>`, then each not-indexed document's reference. A document
with no blocks counts as indexed, since it owes no nodes.

Both source `e2e/acceptance/lib.sh`, the preamble they share: it refuses an unset input or a
missing command with exit 2, and reads the bearer from `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`
without printing it, unsetting both so no process the script starts inherits it.

## Acceptance run against the deployed image

The `dispatch.acceptance.compose.yml` Compose file builds a Dispatch image from this tree and runs
it for Playwright with its own Postgres volume and database. It signs `alice` and `bob` in through
the dev sign-in route, as the local harness does. It also uses an acceptance-only agent token,
disabled NATS, and a private loopback fake Envoy with no token. `FAKE_ENVOY_PORT` and
`ENVOY_IMAGE_TAG` are its only required inputs: the acceptance service derives its `ENVOY_URL`
from `FAKE_ENVOY_PORT`, so it is the one harness port this recipe sets, and `e2e:deployed` starts
the fake listener on it. Before any row runs,
`e2e:deployed` puts a session in that fake and requires the server's `GET /api/v1/agents` to list
it, so a server reading another Envoy listener refuses the run instead of answering it from the
wrong sessions (`e2e/preflight.ts`). `e2e:deployed` runs Playwright through `e2e/run.ts`, which
with `PLAYWRIGHT_BASE_URL` set starts only the fake Envoy and the plain-HTTP proxy, the latter on a
port the OS chooses. The fake Envoy makes subscriber, Agents-page, and fixture-hook rows exercise
the deployed server instead of a real session. Fake GitHub rows still skip because the acceptance
service does not configure that listener. Its Postgres is PostgreSQL 18 on the volume
`dispatch-acceptance-pg18`; a volume `dispatch-acceptance-pg` left from before holds PostgreSQL 16
data no longer used, and `docker volume rm <project>_dispatch-acceptance-pg` removes it.

```bash
cd packages/envoy/deploy/compose
acceptance_envoy_port=19061
acceptance_dispatch_port=18767
acceptance_pg_port=55660
acceptance_image_tag=dispatch-acceptance-local
acceptance_compose_project=dispatch-acceptance
compose=(env FAKE_ENVOY_PORT="$acceptance_envoy_port" DISPATCH_ACCEPTANCE_PORT="$acceptance_dispatch_port" DISPATCH_ACCEPTANCE_PG_PORT="$acceptance_pg_port" ENVOY_IMAGE_TAG="$acceptance_image_tag" docker compose -p "$acceptance_compose_project" -f dispatch.acceptance.compose.yml)
"${compose[@]}" build dispatch-acceptance
"${compose[@]}" up -d dispatch-acceptance
cd ../../../dispatch
FAKE_ENVOY_PORT="$acceptance_envoy_port" \
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
The suite has seven projects. `chromium` runs every spec except the two plain-HTTP specs. `iphone`
runs, from those same specs, only the tests tagged `@phone` (written at the end of the test's or
its `test.describe`'s title); which tests take the tag is in `packages/dispatch/AGENTS.md`, "Phone
acceptance". An untagged test drops out of `iphone` with no failure. The iPhone project uses
Chromium with iPhone 13 viewport, touch, and user-agent emulation. A deployed run
(`bun run e2e:deployed`) uses the same projects, so its `iphone` project also runs only the
`@phone` tests. `webkit` runs `e2e/collab-cursor.e2e.ts`, since where a caret lands beside a
collaborator's cursor differs by engine, and `firefox` runs `e2e/code-line-replace.e2e.ts`, since
Firefox's own editing mishandles text typed over what follows a block's last line break; both also
run `e2e/deep-links.e2e.ts`, `e2e/keyboard-agents-picker.e2e.ts`,
`e2e/keyboard-session-pickers.e2e.ts` and `e2e/offline-edits.e2e.ts`, and `firefox` runs
`e2e/picture-link-click.e2e.ts` too. `firefox-download-navigation` runs the one row of
`e2e/shell.e2e.ts` that follows a Download link before a chunk fails. `webkit-iphone` runs the
phone-layout rows of `e2e/agent-view.e2e.ts`, `e2e/agent-view-settings.e2e.ts` and
`e2e/delivery-live.e2e.ts`, and the rows of `e2e/phone-conversation.e2e.ts` that check what the
Conversation's floating pills cover, selected by title, in WebKit with the iPhone 13 profile, since
iOS Safari is the engine those layouts exist for.
`chromium-plain-http` runs `e2e/plain-http-origin.e2e.ts` and `e2e/plain-http-proxy.e2e.ts`, with
the page opened at `http://dispatch-e2e.test:<PLAIN_HTTP_PORT>` (Chromium maps that name to the
local proxy), a plain-HTTP origin that is not loopback and so not a secure context. The proxy
forwards to the dev sign-in server at its loopback origin. The project proves a document takes a
new paragraph and a comment renders formatted there, where `crypto.randomUUID` does not exist, and
that the proxy refuses any other `Host` as the server's own fence does and keeps no request open
upstream once a page has closed. `chromium-plain-http-sign-out` runs the sign-out rows of
`e2e/keyed-writes.e2e.ts` on that same origin, where the browser has no `navigator.locks`, so a
tab holds the person's writes for another tab's sign-out on a renewed lease rather than on that
tab's lock.
`bun run e2e:install` installs all three browsers.

## Phone check

Browser emulation covers responsive layout. A phone acceptance check runs against
a tailnet-reachable Dispatch server: configure `DISPATCH_LISTEN_HOST` with the
host's Tailscale IPv4 address (`tailscale ip -4`), use that tailnet address and
`DISPATCH_PORT`, open the Inbox from the phone, and answer an open ask. Set
`DISPATCH_INSECURE_COOKIE=1` only for HTTP; HTTPS keeps the normal secure cookie
setting.
