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
holder still becomes a `delivery_failed` exception, and a busy one no longer does (LEGION-101). A
frame that cannot be decoded is still never acknowledged, and a receipt that fails to publish is
logged while delivery continues.

## Daemon contract

`package.json` declares `legion.daemonApiVersion`, the daemon/plugin contract number this build
speaks (currently 4). It covers the `LegionDaemonApi` HTTP request and response shapes the
extension validates strictly (`@legion/contracts`), and the pane contract — every environment
variable the daemon sets on a pane that this extension reads or writes: `LEGION_GRANT_FILE`,
`LEGION_BOOT_TOKEN_FILE`, `LEGION_CONTROLLER_SECRET_FILE`, `LEGION_CONTROL_SUBJECT`,
`LEGION_DAEMON_URL`, `DISPATCH_URL`, `DISPATCH_TOKEN_FILE`, `ENVOY_NATS_URL`, `ENVOY_URL`,
`ENVOY_TOKEN_FILE` (the listener bearer, read by `@legion/envoy-client` ahead of `ENVOY_TOKEN`;
contract 3, LEGION-25), and
the `LEGION_*` identity variables `LEGION_TREE`/`LEGION_ISSUE`/`LEGION_ROLE`/`LEGION_GENERATION`/
`LEGION_WORKSPACE`/`LEGION_STATE_DIR`/`LEGION_CONTROLLER` (read by `src/legion/classify.ts` and
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
have no field and are refused as `contract none`.

Contract 3 adds `ENVOY_TOKEN_FILE` to the daemon/pane contract (LEGION-25). Contract 4 adds the
plugin-minted UUID `requestId` to `spawn_worker` and `workerAdmission` to the state response
(LEGION-102). The first release built from this commit declares 4; a release declaring 3 is
refused as `speaks daemon API contract 3; this daemon requires 4`.

## Native Dispatch tools

The fourteen native Dispatch tools — `dispatch_issue`, `dispatch_ask`, `dispatch_edit_ask`, `dispatch_resolve_ask`,
`dispatch_comment`, `dispatch_suggest`, `dispatch_message`, `dispatch_doc_edit`,
`dispatch_doc_read`, `dispatch_request_approval`, `dispatch_artifact`, `dispatch_read`, `dispatch_search`, and
`dispatch_open_asks` — register only when the shared configuration resolves a URL and bearer token at load; the URL
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
the shared devbox fallback, not a token to configure for an individual agent. A
successful mutation returns `details.topic`, and the `tool_result` hook subscribes to that exact retained
Dispatch topic — a new subscription also tells the model (`pi.sendMessage`
with `deliverAs: "steer"`, the same channel `deliver` uses for inbound
envelopes, since the host does not let a `tool_result` handler amend what the
model already saw); an already-subscribed write stays quiet. Reads
(`dispatch_read`, `dispatch_doc_read`) return only `details.issue`: surveying
the board never subscribes the session. A `subscription.removed` notice (a
human unsubscribed a session from the dashboard) reaches both the issue's own
topic and the removed session's agent topic directly; only the session the
payload names renders it and drops the matching local NATS subscription
(so the dead-connection recovery path does not resurrect it) — every other
subscriber ignores it.
`dispatch_issue` accepts optional initial labels; project-document arguments resolve the document's artifact id, slug, or filename.
`dispatch_ask` can set `kind: "action"` for a human to-do with fixed `Done` / `Can't` answers. It does not expose `approval`, which is opened only through `dispatch_request_approval`.
Quote anchors returned from Dispatch include nullable `block_id`: new anchors are pinned to the
lowest block containing their complete quote, while top-level cross-block and legacy anchors remain
unpinned.

`dispatch_doc_edit` may retype an identified paragraph into any schema-declared typed block with
`{ op: "retype", block, type, attributes }`. A question about a document is written as an `ask`
block through that tool or a `:::ask` directive, not as an issue-level `dispatch_ask`. The extension
passes the host tool AbortSignal to every Dispatch execution; the shared client also imposes a
60-second HTTP deadline.

`before_agent_start` injects the complete `dispatch_open_asks` summary as agent-attributed context; this before-run
summary is the only automatic ask awareness. The stop-time reminder was removed 2026-09-14 pending a redesign of the
`waiting on a human` trigger. An unavailable open-asks query warns once per session until that session's subsequent
query succeeds.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| OMP extension entries | `extensions/envoy.ts`, `extensions/legion.ts` | Both ship in the published npm package and load in every installed OMP session; `legion.ts` is inert without `LEGION_TREE`/`LEGION_ROLE`/`LEGION_CONTROLLER` in the environment |
| Legion lifecycle modules | `src/legion/` | Classification, daemon client, grant file (`grant-file.ts`: the bash `tool_call` hook mints one grant per command, writes it atomically to the pane's `LEGION_GRANT_FILE` as 0600, and returns `undefined` — it never touches `command` or `env`; the static gh environment is the daemon's pane environment), jj attribution (`jj-attribution.ts`: the `JJ_CONFIG` overlay that adds the `Omp-Session` trailer; the commit identity itself is not the extension's — the daemon puts `JJ_USER`/`JJ_EMAIL` and the Git author/committer variables on the pane, and worker boot writes no jj config), control directives, tools |
| Extension unit tests | `extensions/envoy.test.ts`, `extensions/legion.test.ts` | Mocked Pi and NATS surface; `beforeEach` points `ENVOY_URL` at an unroutable host and stubs `fetch` with the registration echo, so a test that forgets its own stub never registers a `ses_*` fixture on the devbox's real listener |
| Shared HTTP/tool behavior | `../envoy-client/src/` | Do not duplicate it here |
| Event subjects | `../contracts/src/subject.ts` | Canonical subject construction |
| Dispatch tools | `extensions/envoy.ts` (the `registerTool` block), `@legion/contracts` (`dispatchToolSpecs`, `dispatchToolSchema`, `zodSchemaApi`), `@legion/envoy-client/dispatch-execute` (`executeDispatchTool`) | Registers the fourteen native tools only when `resolveDispatchConfig` resolves URL and token. Build each tool schema with `dispatchToolSchema(spec, zodSchemaApi(pi.zod))`, pass the live session id/title and host AbortSignal to `executeDispatchTool`, and subscribe only when a successful result includes `details.topic`. |
| Role session prompts | `roles/*.md` | One file per launched Legion process: `architect-root`, `controller-root`, and one per `LegionRole`; the daemon passes each as the first part of the pane's single `--append-system-prompt` value (OMP's flag is last-wins, so the parts are joined), followed by the addressing fragment (roots and phase workers) and, when the deployment's `legion.yaml` sets `instructions`, `<state_dir>/deployment-instructions.md` as the last part |
| Real end-to-end delivery smoke | `smoke-delivery.sh`, `smoke-btw.sh`, `scripts/README.md` | Manual installed-plugin smokes against live Envoy; `smoke-btw.sh` creates a targeted Dispatch BTW or Steer attempt and verifies its correlated reply |

## Critical conventions

- Register every schema through the injected `pi.zod`. Every field counts, not just the outer object: OMP's converter reads internals (`.ir`) only its own Zod produces, and a field from another Zod instance fails the whole extension load (`undefined is not an object (evaluating 'e.ir.desc')`). The shared Dispatch contract exposes field shapes and cross-field validation through `dispatchToolSchema`; pass it `zodSchemaApi(pi.zod)`. `envoy.test.ts` proves every registered field came from the injected instance. |
- Keep direct NATS subscription lifecycle and Pi delivery adapter-local. Register `["aside", "btw", "steer"]` when `pi.askEphemeral` exists and `["aside", "steer"]` otherwise — both built from the contracts' `DELIVERY_CAPABILITIES`, never spelled here; an advertised `btw` frame never falls back to steering. Deliver targeted **Aside** / **Steer** with `triggerTurn: true`; reject an unparsed targeted frame without primary-turn injection, log it, and post its error to Dispatch whenever it has a reply address.
- Render every inbound envelope through `renderInbound`. Keep its bounded 50-item `envoy_inbox` metadata-only; use the shared `envoy_role_get` transport operation for current role holders.
- The registration heartbeat (`ensureHeartbeat`, `ENVOY_HEARTBEAT_MS`, default 120 s) re-asserts the session's held role after every successful re-registration: it reads `GET /v1/roles/<role>` and issues a soft `POST /v1/roles/set` only when the listener does not name this session as the live holder — a healthy tick writes nothing and appends no `envoy-role-claim` transcript entry. A 409 (a different live holder) drops the local claim, warns once, and ends re-assertion for that role; the newer holder is correct. A regain — or the first healthy tick after a failed registration, when a surviving claim may still have been unresolvable — fires `onEnvoyRoleRegained` detached from the heartbeat chain (a slow daemon never blocks the next re-registration), which re-runs the controller's `/controller/ready` and a root architect's `/process/ready` with bounded retries; a phase worker needs nothing, the daemon's own no-holder recovery prompts its catch-up. `legion.ts` registers that listener on the `LEGION_ROLE_CLAIM_BRIDGE` slot only once it holds a Legion identity (`claimController`, `bootstrapRoot`), since a `task` subagent's re-bound instance shares the process and would otherwise replace it.
- A `task` subagent's session in a Legion process claims no role, calls no daemon route, installs no tool gate, and never exits (`isSubagentSession` in `extensions/legion.ts`). It is recognised by either of two signals: the process-local one — `bootstrapRoot`, `bootstrapWorker`, and the controller's `session_start` record the bootstrapped session's transcript path on `globalThis` under `Symbol.for("legion.pi-envoy.bootstrapped-session")`, and any later `session_start` in the same process with a different transcript path is a subagent — or OMP's on-disk layout for file storage (the parent's `.jsonl` sits beside the subagent's transcript directory). The process-local signal is what holds when the transcript is a SQL row rather than a file (LEGION-80: `OMP_SESSION_STORAGE=sql`); the on-disk check stays as the fallback for a process that has not bootstrapped anything.
- A daemon `403 Invalid session secret` is recovered once per forgotten secret, shared by every request in flight: `src/legion/daemon-client.ts` keeps, per session id, the newest recovered secret and the recovery in flight — a refused request retries with a newer secret already known, else awaits the in-flight recovery, else starts the one `/legion/v1/worker-session` recovery, one retry per request and a second refusal returned to the caller — and `roleDaemon()` in `extensions/legion.ts` hands every caller the same client so that record is shared (LEGION-73).
- `spawnWorker` in `src/legion/daemon-client.ts` carries the caller's `requestId` (minted once per `legion` `spawn_worker` call in `src/legion/tools.ts`) and retries only a `fetch` that rejected — never a `LegionDaemonApiError`, whatever its status, and never a response-shape error — up to `SPAWN_WORKER_ATTEMPTS` (3) with `SPAWN_WORKER_RETRY_DELAYS_MS` between attempts, the same id every time so the daemon's ledger dedupes it; the last rejected fetch is a `LegionDaemonTransportError` naming the cause, attempts, and id. A response whose headers arrived but body cannot be read is not retried because `fetch` fulfilled; it is a `LegionDaemonResponseReadError` with the same request id and `legion state` guidance. The 403 recovery above composes with both paths unchanged (LEGION-102).
- `envoy_list` must report the union of locally live and registry-persisted topics, with each topic marked `live`, `registry`, or `both`.
- Do not alter `~/.omp` from this package. The README documents the local developer symlink.
- `smoke-delivery.sh` and `smoke-btw.sh` are manual, real end-to-end smokes against the installed plugin; never wire either into CI without live Envoy/NATS, Dispatch, and a configured model provider.
