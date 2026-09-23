# Pi Envoy Extension

Tracked Oh My Pi (`pi-*`) extension package for Envoy messaging.

## Overview

This package owns Pi-specific tool registration, direct NATS subscriptions, targeted Dispatch
delivery, and self-subscription registration for every session. HTTP transport, tool metadata, and
subject construction come from the Envoy core packages. `@legion/envoy-client/delivery` is the sole
inbound renderer: it produces a tolerant TOON block and never exposes raw envelope bytes. A targeted
Dispatch **BTW** frame runs `pi.askEphemeral` and posts its body or error to the correlated delivery
attempt; **Aside** and **Steer** call `pi.sendMessage` with their respective delivery mode. Role claims
are routed by the listener: this extension receives a receipt-backed request on its direct agent
subject instead of subscribing to a role subject itself. The agent pump replies the moment the
envelope is decoded — before the inbox update, any Dispatch call, or the session injection — so the
listener's two-second receipt window measures decoding, not the host's turn: a claimed-but-deaf
holder still becomes a `delivery_failed` exception, and a busy one no longer does (LEGION-101). The
receipt goes only to a role-lane frame — one whose envelope `topic` is not the direct subject, the
shape the listener forwards to the holder. Every other frame carrying a reply inbox on the direct
subject (a Dispatch author route, a peer `envoy_send`) is a JetStream publish whose inbox belongs to
the server's PubAck; an empty receipt there fails the publisher with
`nats: invalid jetstream publish response`. A frame that cannot be decoded is still never
acknowledged, and a receipt that fails to publish is logged while delivery continues.

## Daemon contract

The plugin boots against one of two daemons and speaks each one's contract through its own client,
until Stage 7 of LEGION-208 deletes the TypeScript daemon (`packages/daemon`) together with its
client, `legion.daemonApiVersion`, and `LEGION_DAEMON_API_VERSION`. `session_start` in
`extensions/legion.ts` picks the client by one variable: a pane that carries `LEGION_DAEMON_API=go`
— set only by the Go daemon's tmux runtime (`packages/daemon-go/internal/runtime/tmux/spawn.go`) —
boots through `src/legion/go-bootstrap.ts`, which owns the Go registration and ready sequence and
uses `src/legion/go-daemon-client.ts`; every other pane, whatever else the variable holds, boots
through `src/legion/daemon-client.ts` as before. `package.json` declares one contract number
per daemon.

### The TypeScript daemon: `legion.daemonApiVersion`

`legion.daemonApiVersion` (currently 8) covers the `LegionDaemonApi` HTTP request and response
shapes the extension validates strictly (`@legion/contracts`), and the pane contract — every
environment variable the TypeScript daemon sets on a pane that this extension reads or writes:
`LEGION_GRANT_FILE`, `LEGION_BOOT_TOKEN_FILE`, `LEGION_CONTROLLER_SECRET_FILE`,
`LEGION_CONTROL_SUBJECT`, `LEGION_DAEMON_URL`, `DISPATCH_URL`, `DISPATCH_TOKEN_FILE`,
`ENVOY_NATS_URL`, `ENVOY_URL`, `ENVOY_TOKEN_FILE` (the listener bearer, read by
`@legion/envoy-client` ahead of `ENVOY_TOKEN`; contract 3, LEGION-25), and the `LEGION_*` identity
variables `LEGION_TREE`/`LEGION_ISSUE`/`LEGION_ROLE`/`LEGION_GENERATION`/`LEGION_WORKSPACE`/
`LEGION_STATE_DIR`/`LEGION_CONTROLLER` (read by `src/legion/classify.ts` and
`extensions/legion.ts`; the Dispatch and Envoy variables by `@legion/envoy-client`; the grant
file is the one the extension writes). The same list, the bump rule, and the contract history are
in the doc comment on `LEGION_DAEMON_API_VERSION` (`packages/contracts/src/legion-daemon-api.ts`)
and in `packages/daemon/src/daemon/AGENTS.md`. The daemon reads the installed manifest at boot
(`verifyLegionPluginContract`, `packages/daemon/src/daemon/boot-probes.ts`) and refuses to start
— before loading state, opening NATS, or serving its API — unless the field equals its
`LEGION_DAEMON_API_VERSION`, naming the manifest path, the package version, and both numbers. A
change to either surface bumps the field and the constant in the same commit
(`src/legion/daemon-api-version.test.ts` pins them equal), and the deployment installs the
release built from that commit before restarting the daemon. Contract 1 was the `runtime` locator
discriminant (LEGION-21). Contract 2 was introduced by LEGION-20 (PR #975) for the `stateGate` and
`GatesRegister` shapes and, from LEGION-52, also covers the pane contract including the credential
file (`LEGION_GRANT_FILE`, LEGION-54); release 1.23.0 is the first to declare 2. Releases 1.14.0
through 1.22.2 declare 1 and are refused as `speaks daemon API contract 1`; releases before 1.14.0
have no field and are refused as `contract none`. Contract 3 adds `ENVOY_TOKEN_FILE` to the
daemon/pane contract (LEGION-25). Contract 4 adds the plugin-minted UUID `requestId` to
`spawn_worker` and `workerAdmission` to the state response (LEGION-102). Contract 5 is LEGION-16
(PR #961): the interactive controller's handshake — `controllerLocator.ompSessionFile` on
`/legion/v1/state`, `ompSessionFile` on `/controller/ready`, and the `/grants` request as a union with
its controller form. Contract 6 is LEGION-25 Part B: the operator-launched controller's external record
on `/legion/v1/state`'s `controllerLocator` (`{runtime:"kubernetes", external:true, sessionId, registeredAt}`,
recorded when a `legion controller start` session calls `/controller/ready` against a kubernetes daemon)
and `POST /legion/v1/controller/secret` (the CLI's call, never this extension's). Contract 7 removes
`merge` from `/gh-token`: Legion never merges (LEGION-19). Contract 8 adds `pluginVersion` to
`/process/started`, `/worker/started`, and `/controller/ready`, and a tree's or role's
`workspaceLost` record to the state response (#1167). The number is re-read against `main` at every
rebase: two branches that each change a surface both take the next number, and the second to land
renumbers above the first.

### The Go daemon: `legion.goDaemonApiVersion`

`legion.goDaemonApiVersion` (currently 1) is the contract with `packages/daemon-go`: the
`POST /legion/v1/claims/register|ready|exit` and `GET /legion/v1/state` shapes
`src/legion/go-daemon-client.ts` parses strictly through `@legion/contracts/legion-go-api` (its
first consumer), and the Go pane's environment — `LEGION_DAEMON_API=go`, the identity variables
above, `LEGION_BOOT_TOKEN_FILE`, `LEGION_DAEMON_URL`, `LEGION_STATE_DIR`, and the Envoy variables.
The Go daemon's boot gate (`internal/daemon/bootgate.go`) refuses to start unless the installed
manifest's field equals its `GoDaemonAPIVersion` (`internal/api/version.go`) — the manifest at the
plugin root Oh My Pi resolves under the environment a pane will get, and the plugin a pane's Oh My
Pi actually loads, which must be that same package. `packages/contracts/fixtures/daemon-api/version.json`,
written by the Go daemon's golden test, is what `src/legion/daemon-api-version.test.ts` pins the
field to, so neither side bumps alone.

**The pane-contract exception.** The TypeScript contract's bump rule makes any change to the pane
contract bump `legion.daemonApiVersion`. The Go pane's variables, `LEGION_DAEMON_API` among them,
are not that contract: a change to them bumps `legion.goDaemonApiVersion` and `GoDaemonAPIVersion`,
and `legion.daemonApiVersion` and `LEGION_DAEMON_API_VERSION` stay where they are. Releases are cut
from `main`, which serves the TypeScript daemon until Stage 7, so the two numbers move
independently until Stage 7 removes the TypeScript one.

The Go boot (`bootstrapGoClaim`) is the same for a root architect and a phase worker — the Go
daemon registers both on one route, a root being the claim whose issue is its tree: the persisted
transcript; `claims/register` with the pane's boot token and this build's `goDaemonApiVersion`
(`pluginContract`), where any 4xx exits the process with one log line naming the route, the
status, and the daemon's sentence (`exitOnGoRegistrationRefusal`) and a 5xx or a transport failure
propagates without exiting; the jj session attribution; the Envoy role, which is the claim token;
`claims/ready`, retried three times a second apart on a 5xx or a transport failure only; a regain
hook that reports ready again; and the capability the `tool_call` role gates read. Left out on
purpose: control directives, which ride `LEGION_CONTROL_SUBJECT`, a variable the Go daemon does
not set; the `legion` tool, whose every operation is a TypeScript-daemon route; a `claims/exit`
report at shutdown, since a suspend the daemon asks for also ends the session and the report would
retire a claim the daemon means to resume; and grant minting — the Go pane carries no
`LEGION_GRANT_FILE`, so the bash grant hook blocks every `bash` call there, naming the file, until
the Go daemon issues grants.

## Native Dispatch tools

The twenty native Dispatch tools — `dispatch_issue`, `dispatch_issue_update`, `dispatch_ask`, `dispatch_edit_ask`, `dispatch_resolve_ask`, `dispatch_resolve_comment`,
`dispatch_follow`, `dispatch_comment`, `dispatch_suggest`, `dispatch_message`, `dispatch_doc_edit`,
`dispatch_doc_read`, `dispatch_request_approval`, `dispatch_artifact`, `dispatch_read`, `dispatch_search`,
`dispatch_issues`, `dispatch_architecture_sync`, `dispatch_open_asks`, and `dispatch_whoami` — register only when the shared configuration resolves a URL and bearer token at load; the URL
and token themselves are re-read
on every call, so a Dispatch that moved (a new `dispatch.serverUrl` in `envoy.json`, or a changed
`DISPATCH_URL`) takes effect in live sessions without `/reload-plugins`, and a file that has since
broken fails the call with its own error rather than using the stale endpoint. Set
`dispatch.enabled: true`, `dispatch.serverUrl`, and `dispatch.token` in
`~/.config/opencode/envoy.json` or `<cwd>/.opencode/envoy.json`. A repository
`dispatch.serverUrl` can use only `dispatch.token` from that same repository
file; override its endpoint only with `DISPATCH_URL` plus `DISPATCH_TOKEN` or
`DISPATCH_TOKEN_FILE`. `DISPATCH_TOKEN_FILE` is a path whose trimmed contents
are the token — how the Legion daemon delivers it to a pane — and never falls
back when unreadable. A human mints a personal token in Dispatch Settings → Agent
tokens, then supplies it through `dispatch.token` or `DISPATCH_TOKEN`; the server
attributes that session's writes to the minting human. `DISPATCH_AGENT_TOKEN` is
the shared devbox fallback, not a token to configure for an individual agent. No
Dispatch write subscribes the session to anything: whole-issue or whole-document
subscription is the model's explicit `envoy_subscribe`, and every write result names
the topic to pass it. What a write does give the session is a *follow* on the ask it
opened or replied to (`details.follows.ask`): the ask's answer and replies then reach
the session's own agent topic directly, server-side, with no NATS subscription to
manage. The `tool_result` hook turns the first `details.follows` for each ask id into
one steer notice (`pi.sendMessage` with `deliverAs: "steer"`, the same channel
`deliver` uses for inbound envelopes, since the host does not let a `tool_result`
handler amend what the model already saw) via `createFollowAnnouncer` from
`@legion/envoy-client/dispatch-subscribe`, the once-per-ask policy over
`dispatchFollowNotice` both hosts share; a later write on the same ask stays quiet, and
reads (`dispatch_read`, `dispatch_doc_read`) return only
owner details. `dispatch_follow` leaves or rejoins an ask. A `subscription.removed`
notice (a human unsubscribed a session from the dashboard) reaches both the issue's
own topic and the removed session's agent topic directly; only the session the payload
names renders it and drops the matching local NATS subscription (so the
dead-connection recovery path does not resurrect it) — every other subscriber ignores
it. `ask.follower_added` / `ask.follower_removed` likewise render only for the session
they name.
`dispatch_issue` accepts optional initial labels and an optional `components` attachment (below); project-document arguments resolve the document's artifact id, slug, or filename.
`dispatch_issue_update` moves an issue's lifecycle `status`, retitles it, replaces `labels`, sets `route`, sets or clears its `parent` (a key in the same project; `""` clears, and the executor sends JSON `null`), attaches it to architecture `components` (`{mode: "inherit" | "explicit" | "none", ids?, reason?}`: `explicit` names bare component ids of the project's imported model, `none` needs a reason, `inherit` returns to the nearest ancestor's attachment; allowed on a closed issue; the result line reads `components -> explicit [a, b]` / `components -> none (reason)` / `components -> inherit`, and `400 COMPONENTS_INPUT` names an unknown, retired, or external id), or links URLs through `external_links` (merged into the existing links by URL, so linking the pull request just opened keeps earlier links); at least one field besides `issue` is required and priority is not exposed. Its one-line result reads `KEY: status a -> b; linked <url> (N links); parent -> KEY` (`parent cleared` on a clear), and a server refusal (`INVALID_STATUS`, `ISSUE_CLOSED`, `EXTERNAL_LINK_TAKEN`, `PARENT_INPUT`, `COMPONENTS_INPUT`) keeps its code at the head of the thrown message. `dispatch_read` of an issue renders a `Components:` line — `a, b (inherited from KEY)`, `none — <reason>`, or `unassigned`, plus `(retired: c)` for ids a re-import retired — and component nodes render like every other reference node at `dispatch://<PROJECT>/component/<id>`.
`dispatch_ask` takes no `kind`: every ask it opens is a question whose options the asker chooses; no label is special to the server (a human to-do is the to-do phrased as the question, with whatever options fit it). `approval` asks are opened only through `dispatch_request_approval`.
`dispatch_comment` accepts `turn: "agent" | "human"` only with `reply_to_ask` (the shared cross-field validation rejects it otherwise): `agent` is a progress note that keeps the ask waiting on the agent in the human's Inbox, `human` (the default) hands the turn to the human. The result text names the resulting state (`ask now waiting on agent` / `human`) and `details.ask_waiting_on` carries it.
Quote anchors returned from Dispatch include nullable `block_id`: new anchors are pinned to the
lowest block containing their complete quote, while top-level cross-block and legacy anchors remain
unpinned.

`dispatch_doc_edit` may retype an identified paragraph or typed block into any schema-declared typed block with
`{ op: "retype", block, type, attributes }`, delete or move a whole block by id with
`{ op: "delete", block }` and `{ op: "move", block, after | before }`, and delete a table row or column in place
with `{ op: "delete_row" | "delete_column", block, index }`. Table `index` is zero-based and its table `block` id
comes from the artifact-UUID `GET /api/v1/artifacts/{id}/blocks` route; that route returns each block's
full-state token, including inline marks. `dispatch_doc_read` returns the whole-document token. Supply an
optional `precondition` with exactly one document token or one-or-more block `{id, token}` entries; a block
guard must cover every content block the resolved batch changes, while unrelated sections stay independent.
Insert and move require the document token because their meaning depends on document order. A stale
precondition returns `PRECONDITION_FAILED` with current tokens and applies no operation; an uncovered block
returns `INVALID_PRECONDITION`. The server refuses a deletion that would remove an open ask or unresolved
comment anchor. A question about a document is written as an `ask` block through that tool or a `:::ask`
directive, not as an issue-level `dispatch_ask`. The extension passes the host tool AbortSignal to every
Dispatch execution; the shared client also imposes a 60-second HTTP deadline.

`before_agent_start` injects the complete `dispatch_open_asks` summary as agent-attributed context; this before-run
summary is the only automatic ask awareness. The stop-time reminder was removed 2026-09-14 pending a redesign of the
`waiting on a human` trigger. An unavailable open-asks query warns once per session until that session's subsequent
query succeeds.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| OMP extension entries | `extensions/envoy.ts`, `extensions/legion.ts` | Both ship in the published npm package and load in every installed OMP session; `legion.ts` is inert without `LEGION_TREE`/`LEGION_ROLE`/`LEGION_CONTROLLER` in the environment |
| Legion lifecycle modules | `src/legion/` | Classification, the two daemon clients (`daemon-client.ts` for the TypeScript daemon, `go-daemon-client.ts` for the Go daemon) and the Go-only bootstrap (`go-bootstrap.ts`; see Daemon contract), grant file (`grant-file.ts`: the bash `tool_call` hook mints one grant per command, writes it atomically to the pane's `LEGION_GRANT_FILE` as 0600, and returns `undefined` — it never touches `command` or `env`; the static gh environment is the daemon's pane environment), jj attribution (`jj-attribution.ts`: the `JJ_CONFIG` overlay that adds the `Omp-Session` trailer; the commit identity itself is not the extension's — the daemon puts `JJ_USER`/`JJ_EMAIL` and the Git author/committer variables on the pane, and worker boot writes no jj config), control directives, tools |
| Controller session | `src/legion/controller-session.ts` | Owns controller identity, its resume transcript, claim and reclaim hooks, and recovery-less grant minting. Its claim reports `ompSessionFile` on `/controller/ready`; the event router writes each returned grant through `grant-file.ts` to `LEGION_GRANT_FILE`. |
| Extension unit tests | `extensions/envoy.test.ts`, `extensions/legion.test.ts` | Mocked Pi and NATS surface; `beforeEach` points `ENVOY_URL` at an unroutable host and stubs `fetch` with the registration echo, so a test that forgets its own stub never registers a `ses_*` fixture on the devbox's real listener |
| Shared HTTP/tool behavior | `../envoy-client/src/` | Do not duplicate it here |
| Event subjects | `../contracts/src/subject.ts` | Canonical subject construction |
| Dispatch tools | `extensions/envoy.ts` (the `registerTool` block), `@legion/contracts` (`dispatchToolSpecs`, `dispatchToolSchema`, `zodSchemaApi`), `@legion/envoy-client/dispatch-execute` (`executeDispatchTool`) | Registers the twenty native tools only when `resolveDispatchConfig` resolves URL and token. Build each tool schema with `dispatchToolSchema(spec, zodSchemaApi(pi.zod))` — deliberately NOT strict: on installed OMP hosts a strict host schema makes the coercion pass delete an unknown key beside valid required fields and hand the executor silently narrowed args, while non-strict preserves unknown root fields so `executeDispatchTool`'s own always-strict parse names the invented field (legion #1242 review; the xd:// half is can1357/oh-my-pi#12871) — register it (and every Envoy tool) with `lenientArgValidation: true` so the host hands raw arguments through and `executeDispatchTool` / `parseEnvoyToolArguments` is the one refusal (a `ToolInputError` naming every problem), pass the live session id/title and host AbortSignal to `executeDispatchTool`, and never subscribe from a tool result: the `tool_result` hook only announces `details.follows` once per ask. |
| Role session prompts | `roles/` | Phase workers compose `core/<role>.md`, `mechanics/headless.md`, and the per-role residue; merger composes headless plus its residue. Root architect, controller, and sub-architect prompts remain single-file. The daemon concatenates the parts into the first portion of its one `--append-system-prompt` value (OMP's flag is last-wins), followed by the addressing fragment (roots and phase workers) and, when the deployment's `legion.yaml` sets `instructions`, `<state_dir>/deployment-instructions.md` as the last part |
| Real end-to-end delivery smoke | `smoke-delivery.sh`, `smoke-btw.sh`, `scripts/README.md` | Manual installed-plugin smokes against live Envoy; `smoke-btw.sh` creates a targeted Dispatch BTW or Steer attempt and verifies its correlated reply |

## Critical conventions

- Register every schema through the injected `pi.zod`. Every field counts, not just the outer object: OMP's converter reads internals (`.ir`) only its own Zod produces, and a field from another Zod instance fails the whole extension load (`undefined is not an object (evaluating 'e.ir.desc')`). The shared Dispatch contract exposes field shapes and cross-field validation through `dispatchToolSchema`; pass it `zodSchemaApi(pi.zod)`. `envoy.test.ts` proves every registered field came from the injected instance. |
- Keep direct NATS subscription lifecycle and Pi delivery adapter-local. Register `["aside", "btw", "steer"]` when `pi.askEphemeral` exists and `["aside", "steer"]` otherwise — both built from the contracts' `DELIVERY_CAPABILITIES`, never spelled here; an advertised `btw` frame never falls back to steering. Deliver targeted **Aside** / **Steer** with `triggerTurn: true`; reject an unparsed targeted frame without primary-turn injection, log it, and post its error to Dispatch whenever it has a reply address.
- Render every inbound envelope through `renderInbound`. Keep its bounded 50-item `envoy_inbox` metadata-only; use the shared `envoy_role_get` transport operation for current role holders.
- The registration heartbeat (`ensureHeartbeat`, `ENVOY_HEARTBEAT_MS`, default 120 s) re-asserts the session's held role after every successful re-registration: it reads `GET /v1/roles/<role>` and issues a soft `POST /v1/roles/set` only when the listener does not name this session as the live holder — a healthy tick writes nothing and appends no `envoy-role-claim` transcript entry. A 409 (a different live holder) drops the local claim, warns once, and ends re-assertion for that role; the newer holder is correct. A regain — or the first healthy tick after a failed registration, when a surviving claim may still have been unresolvable — fires `onEnvoyRoleRegained` detached from the heartbeat chain (a slow daemon never blocks the next re-registration), which re-runs the controller's `/controller/ready` and a root architect's `/process/ready` with bounded retries; a phase worker needs nothing, the daemon's own no-holder recovery prompts its catch-up. `legion.ts` registers that listener on the `LEGION_ROLE_CLAIM_BRIDGE` slot only once it holds a Legion identity (`claimController`, `bootstrapRoot`), since a `task` subagent's re-bound instance shares the process and would otherwise replace it.
- A `task` subagent's session in a Legion process claims no role, calls no daemon route, installs no tool gate, and never exits (`isSubagentSession` in `extensions/legion.ts`). It is recognised by either of two signals: the process-local one — `bootstrapRoot`, `bootstrapWorker`, and the controller's `session_start` record the bootstrapped session's transcript path on `globalThis` under `Symbol.for("legion.pi-envoy.bootstrapped-session")`, and any later `session_start` in the same process with a different transcript path is a subagent — or OMP's on-disk layout for file storage (the parent's `.jsonl` sits beside the subagent's transcript directory). The process-local signal is what holds when the transcript is a SQL row rather than a file (LEGION-80: `OMP_SESSION_STORAGE=sql`); the on-disk check stays as the fallback for a process that has not bootstrapped anything.
- A daemon refusal of the boot registration itself — `/process/started` for a root, `/worker/started` for a phase worker or sub-architect — ends the process (`exitOnRegistrationRefusal` in `extensions/legion.ts`: one log line naming the route, the status, and the daemon's message, then `exitProcess(1)`) for every 4xx: a 400 or 404 (a request or a route the daemon does not have), a 403 (the boot token is stale, consumed, or unknown), and every 409 those routes answer — the same-agent rule (`Worker respawn must resume the same agent session`: this session is not the one the resumed claim recorded; under a database session store that is Oh My Pi having started a fresh session at a path whose row is gone), a stale generation (`Stale process generation` / `Stale worker generation`: the daemon already owns a newer launch of this role), and a tree being closed (`TreeClosingError`). None changes on retry, and a process that stayed up unregistered would sit alive under the daemon's boot watchdog with nothing ever retiring it; exiting hands the outcome to the daemon, which already holds the decision each 409 names (count the launch failure and decide the respawn, keep the newer generation, finish the close). Every other error there — a 5xx, a transport failure — propagates out of `session_start` without exiting, exactly as before (LEGION-81; `extensions/legion.test.ts` pins both routes for 403, 409, and the 500 negative control, and `/worker/started` for 400). The Go daemon's `/legion/v1/claims/register` has the same rule (`exitOnGoRegistrationRefusal`; see Daemon contract).
- A daemon `403 Invalid session secret` is recovered once per forgotten secret, shared by every request in flight: `src/legion/daemon-client.ts` keeps, per session id, the newest recovered secret and the recovery in flight — a refused request retries with a newer secret already known, else awaits the in-flight recovery, else starts the one `/legion/v1/worker-session` recovery, one retry per request and a second refusal returned to the caller — and `roleDaemon()` in `extensions/legion.ts` hands every caller the same client so that record is shared (LEGION-73).
- `spawnWorker` in `src/legion/daemon-client.ts` carries the caller's `requestId` (minted once per `legion` `spawn_worker` call in `src/legion/tools.ts`) and retries only a `fetch` that rejected — never a `LegionDaemonApiError`, whatever its status, and never a response-shape error — up to `SPAWN_WORKER_ATTEMPTS` (3) with `SPAWN_WORKER_RETRY_DELAYS_MS` between attempts, the same id every time so the daemon's ledger dedupes it; the last rejected fetch is a `LegionDaemonTransportError` naming the cause, attempts, and id. A response whose headers arrived but body cannot be read is not retried because `fetch` fulfilled; it is a `LegionDaemonResponseReadError` with the same request id and `legion state` guidance. The 403 recovery above composes with both paths unchanged (LEGION-102).
- `envoy_list` must report the union of locally live and registry-persisted topics, with each topic marked `live`, `registry`, or `both`.
- Do not alter `~/.omp` from this package. The README documents the local developer symlink.
- `smoke-delivery.sh` and `smoke-btw.sh` are manual, real end-to-end smokes against the installed plugin; never wire either into CI without live Envoy/NATS, Dispatch, and a configured model provider.
