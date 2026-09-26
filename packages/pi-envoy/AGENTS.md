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
— set only by the Go daemon's runtimes (`packages/daemon-go/internal/runtime/tmux/spawn.go`,
`internal/runtime/sandbox/manifest.go`) and by the Go `legion controller start` — boots through
`src/legion/go-bootstrap.ts`, which owns the Go registration and ready sequence and uses
`src/legion/go-daemon-client.ts` (a controller session through its Go adapter there); every other pane, whatever else the variable holds, boots
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

`legion.goDaemonApiVersion` (currently 6) is the contract with `packages/daemon-go`: the claim,
credential, workflow, controller, and state shapes `src/legion/go-daemon-client.ts` parses strictly
through `@legion/contracts/legion-go-api` (its first consumer), and the Go pane's environment —
`LEGION_DAEMON_API=go`, the identity variables above, `LEGION_BOOT_TOKEN_FILE`,
`LEGION_GRANT_FILE`, `LEGION_DAEMON_URL`, `LEGION_STATE_DIR`, the Envoy variables, and
`DISPATCH_URL`/`DISPATCH_TOKEN_FILE` when the daemon has `dispatch_url` configured.
Contract 4 adds the operator-launched controller: `POST /legion/v1/controller/secret` (the CLI's
call, never this extension's), a controller registration on `claims/register` answered with the
role `controller` and no tree or issue, the `/grants` controller-session form
(`{sessionId, secret}`), and `controllerLocator` (`{runtime, external: true, sessionId,
registeredAt}`) on `/legion/v1/state`.
Contract 5 adds `LEGION_GRANT_FILE` to the Go pane's environment, tmux pane and Sandbox pod alike
(LEGION-262): Oh My Pi copies its environment once for every `gh` it runs to serve a `pr://` or
`issue://` read or its `github` tool, so the pointer has to be there from its start, and this
extension no longer sets it after the claim registers. On a pane a daemon at 4 launched, a plugin
at 5 refuses every bash command and every call Oh My Pi serves with `gh`, answering
`LEGION_GRANT_FILE is not set on this pane: …`. Restarting the daemon at 5 does not clear it, since
a restarted daemon re-adopts a live pane without relaunching it; relaunching the pane does (under
the Go daemon, `legion claims suspend` and then `legion claims resume` on its claim).
Contract 6 adds `phase` to a claim's pending delivery on `/legion/v1/state` — the issue phase the
task was queued for, absent for a task of no phase — and `unrecorded` as the `phase` and `status`
the state route reads for an issue the workflow does not record, where an operator's claim exists
and an issue does not (#1345). No request names `unrecorded`: the phase-backward request takes the
workflow's phases alone. The handoff completion request is unchanged: a completion names no run,
and the daemon attributes it to the run of the task the worker took, reading the claim in three
steps — the task whose turn is running; or, once that turn ends and retires it, the run the claim
is left serving, which is what answers for a worker woken by a notice; or, for a claim that has
served no run at all, a task it holds that the agent may have read. A task the agent refused is
not one it read, so it answers for none of them. `POST /legion/v1/handoff/complete` answers 409
`HANDOFF_NO_RUN` to a claim that has taken no task at all, and the workflow refuses
`HANDOFF_STALE_GENERATION` for a run the issue has left. The pane's `LEGION_GENERATION` is the
claim's launch counter and says nothing about the run; nothing reads it for this.
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
(`pluginContract`), where any 4xx exits the process with one log line naming the route, status,
and daemon sentence (`exitOnGoRegistrationRefusal`) and a 5xx or transport failure propagates
without exiting; jj session attribution; the Envoy role, which is the claim token; a persisted
notice-topic subscription (the architect's tree root or a worker's issue); and `claims/ready`,
retried three times a second apart on a 5xx or transport failure only. The tool-call hook mints a
fresh grant into the pane's `LEGION_GRANT_FILE` before every call that redeems one (see the grant
file row below). It also registers the Go `legion` tool: architects register gates, release
children, request a backward move, choose retry or escalation, sign off, and read records; phase
workers request a backward move and read records.
Control directives remain out because the Go daemon does not set `LEGION_CONTROL_SUBJECT`, as
does a `claims/exit` report at shutdown because a daemon-requested suspend ends the session but
keeps its claim for resumption.

The Go daemon launches no controller: the operator starts one with `legion controller start`, which
fetches the controller capability with the operator's bearer and runs Oh My Pi with
`LEGION_CONTROLLER=1`, `LEGION_DAEMON_API=go`, and `LEGION_CONTROLLER_SECRET_FILE`. No boot gate
checks the operator's machine, so the plugin's contract is held there twice: before its one daemon
call, `legion controller start` launches Oh My Pi as the controller will run — its launch prefix and
invocation, the controller's environment, in `<state_dir>/controller` — with the boot gate's load
probe, and refuses when that Oh My Pi loads no pi-legion-envoy, or loads one whose
`goDaemonApiVersion` is not its own; the mint revokes the incumbent controller, so a plugin that
would refuse the new session is found before that. The manifest it reads is the one Oh My Pi reports
loading, so whatever moves the plugin root (a dotenv file, the launch prefix, a project plugin root,
a symlinked state directory) moves the check with it. And the daemon refuses a controller
registration whose `pluginContract` is not its `GoDaemonAPIVersion` with 409, naming both. That
session goes through the controller session (`src/legion/controller-session.ts`) with the Go adapter
(`goControllerDaemon`, `go-bootstrap.ts`), not `bootstrapGoClaim`, and gets no Go `legion` tool:
`claims/register` with the capability in place of a boot token, answered with
`api.ControllerRegisterResponse` (`LegionGoControllerRegisterResponse`), then the Envoy role
`legion-<project>-controller`, then a controller grant per credentialed tool call from the `/grants`
controller-session form with the secret the registration was issued. A later
`legion controller start` mints a new capability, so the earlier session's grants stop working.
`legion status <issue> <status>` in that session reads the grant file; from an operator shell it
takes `--operator-token-file`, which buys a controller grant over the operator's bearer and, like
`legion claims`, is refused when its group or others can read it.

## Phase workers' handoff actions and the phase-stall follow-up

A worker's handoff operations are actions of the `legion` tool, never shell text:
`handoff_write`, `handoff_read`, and `handoff_complete` (`src/legion/handoff-actions.ts`). Both
daemons' tools carry them (`src/legion/tools.ts`, now registered for every TypeScript-daemon worker,
and `src/legion/go-tools.ts`), for every session but the root architect. Each action runs the
daemon's own `legion handoff ...` command, `legion` found on the pane's PATH (the tmux
`<state_dir>/bin/legion` launcher, or the image's binary in a pod), in `LEGION_WORKSPACE`;
`handoff_complete` first mints a grant into `LEGION_GRANT_FILE`, as the tool-call hook does before a
shell command. `legion gh` and `legion credential` stay shell commands: git and gh call them. What a
later phase needs goes in the handoff; a question for another live role goes to its role topic with
`envoy_publish`.

`handoff_write` sends its payload on the command's stdin, which both CLIs read when `--data` is
omitted: one argv string is capped at 128 KiB (Linux's `MAX_ARG_STRLEN`), and a tester's handoff that
accumulates review rounds outgrows it.

The shell's completion is closed: the tool_call hook refuses `legion handoff complete` (by name or by
a path ending `/legion`) in a phase-worker pane, a sub-architect's included, and a root architect's,
ahead of every role gate so that it binds a `task` subagent too — a `bash` command in any position of
a chain, and `eval` code or a `hub` process start by a plain-text rule, exactly as it refuses the jj
operation-log rewrites (`PANE_RULES` in `extensions/legion.ts`). A completion run from the shell
would never reach the phase stall below. `legion handoff write` and `read` stay open to the shell:
they leave no phase open, a root architect reads committed handoffs with `legion handoff read`, and
a worker can pipe a handoff built from the one on disk to `legion handoff write` on stdin
(`skills/legion-worker/SKILL.md`, the handoff write section).

In a phase-worker session (planner, implementer, tester, reviewer, merger: never an architect, the
controller, a session with no Legion environment, or a `task` subagent), `src/legion/phase-stall.ts`
tracks the phase: the daemon's assignment (a user message) opens it, the tool's successful
`handoff_complete` closes it. When a run is about to settle (`session_stop`) with the phase still
open, the extension returns one follow-up (`{continue: true, additionalContext}`), which the host sends
as the next turn of the same session: run `handoff_complete`, or reply with a WAITING line. A final
message holding a tool call written as text is told so. One follow-up per stall; a WAITING reply or a
sent follow-up stays quiet until the next Envoy delivery or assignment. The state is appended to the
transcript (`legion-phase-stall` entries) and restored at `session_start`, so a worker relaunched with
`--resume` keeps it. `extensions/legion-phase-stall-omp.test.ts` proves it on the pinned Oh My Pi
(`LEGION_TEST_OMP`).

## Native Dispatch tools

The twenty-one native Dispatch tools — `dispatch_issue`, `dispatch_issue_update`, `dispatch_claim`, `dispatch_ask`, `dispatch_edit_ask`, `dispatch_resolve_ask`, `dispatch_resolve_comment`,
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
`dispatch_issue_update` moves an issue's lifecycle `status`, retitles it, replaces `labels`, sets its coarse `priority` (`0` is P0, the highest, through `3`, P3, the lowest; `null` clears it — agents set priority by Sami's ruling of 2026-09-24 on `dispatch://LEGION/artifact/issue-status-conventions-md`), sets `route`, sets or clears its `parent` (a key in the same project; `""` clears, and the executor sends JSON `null`), attaches it to architecture `components` (`{mode: "inherit" | "explicit" | "none", ids?, reason?}`: `explicit` names bare component ids of the project's imported model, `none` needs a reason, `inherit` returns to the nearest ancestor's attachment; allowed on a closed issue; the result line reads `components -> explicit [a, b]` / `components -> none (reason)` / `components -> inherit`, and `400 COMPONENTS_INPUT` names an unknown, retired, or external id), or links URLs through `external_links` (merged into the existing links by URL, so linking the pull request just opened keeps earlier links); at least one field besides `issue` is required and `rank`, the board's own order, is not exposed. Its one-line result reads `KEY: status a -> b; priority -> P1; linked <url> (N links); parent -> KEY` (`priority cleared` / `parent cleared` on a clear), and a server refusal (`INVALID_STATUS`, `ISSUE_CLOSED`, `EXTERNAL_LINK_TAKEN`, `PARENT_INPUT`, `COMPONENTS_INPUT`) keeps its code at the head of the thrown message. `dispatch_read` of an issue renders a `Components:` line — `a, b (inherited from KEY)`, `none — <reason>`, or `unassigned`, plus `(retired: c)` for ids a re-import retired — and component nodes render like every other reference node at `dispatch://<PROJECT>/component/<id>`.
`dispatch_claim` claims the issue for this session before it starts implementing, and `{issue, release: true}` releases it. A live holder's claim answers `409 ISSUE_CLAIMED` naming that session, so the second agent talks to it instead of working the same issue; a human holder is named by login instead, with nothing said about a session running, since there is none to message. `409 CLAIM_CONTENDED` is the other refusal: the holder changed twice while the call ran, so nothing was applied and nobody's liveness was checked — read the issue and decide again. A claim whose session the Envoy listener no longer lists is taken automatically, and the session that lost it hears about the takeover on its own agent topic. The claim never moves the issue's status, so an agent that starts work claims the issue *and* moves it to `in_progress` with `dispatch_issue_update` — two explicit actions, because the status is also how humans track work. Closing an issue releases its claim.
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

`before_agent_start` injects nothing into the conversation; its open-asks query only arms the run-end nudge for a
turn carrying the user's own text, the snapshot's `as_of` becoming the period's first window. It runs that query
only for a session the stop could actually nudge — the host awaits this handler, so a session that is excluded
below would otherwise pay up to `OPEN_ASKS_TIMEOUT_MS` at the head of every turn for an answer nothing reads.
`agent_end` is the nudge's stop signal, and Dispatch state is only its cheap precondition: a session whose run
settles normally (`willContinue` unset and the last assistant reply ended `stopReason: "stop"` — an interrupt, a
provider error, a truncation, or a run with no reply of its own is never nudged, and asks Dispatch nothing) with
nothing open and nothing opened since the window it reads from then runs a **silent self-check**: one
`pi.askEphemeral` call — the host's one-shot, tool-free model call over a snapshot of the conversation, the same
channel a targeted Dispatch BTW uses — asking the agent for one word, WAITING or PROCEEDING. The prompt asks
nothing about Dispatch: the extension has just read from Dispatch that nothing is open, and the agent this
catches is one that asked the human in chat text, which such an agent can read as having asked. Only a reply
whose first word is WAITING, and which does not also name PROCEEDING (a model echoing the choice back rather
than making it), produces the one hidden `dispatch-ask-reminder` steer with `triggerTurn`, which tells the agent
it said it is waiting on a human with no open ask and to open one now with `dispatch_ask` (or
`dispatch_request_approval` for a document). The parse is case-sensitive and first-word-only because a false
WAITING is the expensive error — its steer asserts the agent said it is waiting, which sends it to page a human
with a question nobody had — while a false PROCEEDING is only the silence of the status quo. **Every other
outcome is silent**: PROCEEDING, an unparsable reply, an `askEphemeral` failure — a rejection or the synchronous
throw a host initialised without the capability installs, both caught and both spending the check — the timeout,
and a host with no `pi.askEphemeral` at all, that last one arming no period either, so it pays no round trip.
The timeout is the extension's own clock, not the host's: the call is given an `AbortSignal` so a host that
honours it stops paying for an answer nobody will read, but the handler races that call against
`ASK_SELF_CHECK_TIMEOUT_MS` (60 s, moved by `ENVOY_SELF_CHECK_TIMEOUT_MS`) and always settles there, because a
host that ignored the signal would otherwise hold the one-check-at-a-time latch — and every later check with it
— for as long as its call hung. A late answer is dropped, and the controller is reachable while the call is
open, so everything that invalidates a check aborts it rather than leaving a whole-context request running
beside the turn the user is waiting on: the user typing (at the start of `before_agent_start`, ahead of its
arming query, so a Dispatch that is slow or unreachable cannot hold the old check live into the new turn), a
session change, the agent opening the ask itself, and a staleness the post-race re-read finds. A failure is
logged once per session (`logger.warn`), never notified: a broken self-check must not put a warning in front of
the user for a reminder they were not going to get. An abort the extension made — a superseded check, or its
own timeout, which logs itself — is not a failure and is not logged, so the one warning stays for the failure
that matters. The cost of the common case is therefore one hidden model call per normally-settled turn that has
no open ask, and nothing on screen.

The check is owed and spent like the host's own todo reminder rather than once per period: the arming turn owes
one, running it spends it whatever came back, the agent opening the ask itself (`dispatch_ask`,
`dispatch_request_approval`) spends it too, and the agent's next real work — a successful `tool_result` whose
tool is not a `dispatch_*` one — owes another, so a run that keeps working keeps being checked without the user
typing again. Loop safety is two properties, not one: a settled turn that only replies calls no tool, so the
nudge's own continuation owes nothing and cannot nudge itself (that, plus the continuation re-entering no
`before_agent_start` at all, is what `extensions/legion-phase-stall-omp.test.ts` holds the pinned binary to);
and a continuation that ignores the steer and does work instead does re-arm, which is bounded by
`ASK_CHECKS_PER_PERIOD` (5), the cap on what one armed period pays for.

The window each stop reads Dispatch from moves. Every settle that resolves — silently because an ask is open or
was opened since, or by spending a check — carries `baseline_as_of` to that snapshot's `as_of`. A window pinned
to the arming turn never moves, and the server's `opened_since` is true for every ask the session authored after
`since` whatever state it is in now (`packages/envoy/internal/dispatch/api/asks.go`), so one ask would silence
the rest of the period however long the session lived — and a human's answer arrives through Envoy, which arms
no period, so nothing else would ever re-open it. Nothing latches an ask for the period: a genuinely open one
keeps settles silent through the live `count > 0` read, which every stop re-reads. One stop-time check runs at a
time: `agent_end` handlers are not awaited by the host, so a latch held across both the Dispatch round trip and
the self-check is what keeps a second stop inside either from checking or nudging again, and the staleness list
is re-read after each await — including the owed check itself, so an ask the agent opens while a check is in
flight drops that verdict instead of steering "you have no open ask" at a session that has just opened one.
Every run the host starts bumps a counter (`agent_start`), and a check compares it with the value it read at its
settle: a run that started meanwhile — an Envoy delivery waking the session, perhaps with the very reply the
agent was waiting for — means the verdict describes a run the session has moved past. Such a verdict is never
steered; the check still counts against the cap, but what the period owes stays owed, for the newer run's
settle. A run that starts while the Dispatch query is open pays for no check at all. A stop that arrives while a
check holds the latch is recorded, not dropped, and checked as soon as the open check ends if a check is still
owed — otherwise a woken run that settled inside the check's window, the usual case, would go unchecked until
some later run. Each such pass needs another real settle during the last, and every check that reaches the
model counts against the cap, so the re-check cannot loop. The latch, the period and its budget are in memory
only — a cold start or a session change begins at period 0, which
nudges nothing until the next genuine user turn arms one. The guard, the staleness list and the `tool_result`
edge compare only the host's **live** session id against the one the period was armed with, never the
module-level `sessionID` the registration heartbeat maintains: a fresh TUI mints its id after `session_start`, so
those two disagree for up to `ENVOY_HEARTBEAT_MS` and a brand-new terminal went unchecked, and unable to re-arm,
for that whole window. Only a change of session id resets the period and bumps the generation — a `/fork`,
`/handoff`, resume or switch. Re-establishing the *same* session does not: the heartbeat's drift heal runs for
every fresh TUI once the host mints its id, and the `session_start` NATS retry runs every
`NATS_RETRY_INTERVAL_MS` for the length of an Envoy outage, and clearing the period on those disabled the nudge
exactly where it was meant to work. A `task` subagent's instance arms no period at all, so nothing rests on the
module id.

What the arming rule excludes is as load-bearing as what it covers. An Envoy delivery wakes a session through the
same agent-initiated path the nudge itself uses, which emits no `before_agent_start`, so an event-woken turn arms
no period: a standing role-holder that works only when Envoy wakes it is never nudged, and is covered by the
skill norm alone. A run the host gave no UI context (`omp -p`, and any other headless launch) is never nudged
either: the host disposes the session when that one run ends, so the nudge only buys a provider turn nobody
reads. An RPC pane and an ACP session both have a UI context; on an ACP client that defers agent-initiated turns
the steer is queued as hidden next-turn context and consumed when the user next prompts, rather than running a
turn of its own. A `task` subagent is skipped outright. A Legion-driven session is never nudged, having the
design gate and its architect instead: `claimEnvoyRole` records it on the role-claim bridge and in the transcript
(`legion-managed-session`), which is what a fresh process reads — and neither record is matched against the
session id, so the exclusion survives a `/fork` or `/handoff` that mints a new one. An unavailable open-asks
query warns once per session until that session's subsequent query succeeds, and arms nothing — an unknown ask
state never nudges.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| OMP extension entries | `extensions/envoy.ts`, `extensions/legion.ts` | Both ship in the published npm package and load in every installed OMP session; `legion.ts` is inert without `LEGION_TREE`/`LEGION_ROLE`/`LEGION_CONTROLLER` in the environment |
| Legion lifecycle modules | `src/legion/` | Classification, the two daemon clients (`daemon-client.ts` for the TypeScript daemon, `go-daemon-client.ts` for the Go daemon) and the Go-only bootstrap (`go-bootstrap.ts`; see Daemon contract), grant file (`grant-file.ts`: the `tool_call` hook mints one grant per call that redeems one — every `bash` command, the `github` tool, and any tool whose `path`/`paths` names a `pr://` or `issue://` URL, which Oh My Pi serves by running `gh` (`needsGrant` in `extensions/legion.ts`) — writes it atomically to the pane's `LEGION_GRANT_FILE` as 0600, creating its directory 0700 when absent, and returns `undefined` — it never touches the tool's input; the static gh environment and the `LEGION_GRANT_FILE` pointer are the daemon's pane environment), jj attribution (`jj-attribution.ts`: the `JJ_CONFIG` overlay that adds the `Omp-Session` trailer; the commit identity itself is not the extension's — the daemon puts `JJ_USER`/`JJ_EMAIL` and the Git author/committer variables on the pane, and worker boot writes no jj config), control directives, tools |
| Controller session | `src/legion/controller-session.ts` | Owns controller identity, its resume transcript, claim and reclaim hooks, and recovery-less grant minting through a per-daemon adapter: under the TypeScript daemon its claim reports `ompSessionFile` on `/controller/ready` and grants are minted with the capability; under the Go daemon (`goControllerDaemon`) it registers on `claims/register` and mints with the registration's secret. The event router writes each returned grant through `grant-file.ts` to `LEGION_GRANT_FILE`. |
| Extension unit tests | `extensions/envoy.test.ts`, `extensions/legion.test.ts` | Mocked Pi and NATS surface; `beforeEach` points `ENVOY_URL` at an unroutable host and stubs `fetch` with the registration echo, so a test that forgets its own stub never registers a `ses_*` fixture on the devbox's real listener |
| Shared HTTP/tool behavior | `../envoy-client/src/` | Do not duplicate it here |
| Event subjects | `../contracts/src/subject.ts` | Canonical subject construction |
| Dispatch tools | `extensions/envoy.ts` (the `registerTool` block), `@legion/contracts` (`dispatchToolSpecs`, `dispatchToolSchema`, `zodSchemaApi`), `@legion/envoy-client/dispatch-execute` (`executeDispatchTool`) | Registers the twenty-one native tools only when `resolveDispatchConfig` resolves URL and token. Build each tool schema with `dispatchToolSchema(spec, zodSchemaApi(pi.zod))` — deliberately NOT strict: on installed OMP hosts a strict host schema makes the coercion pass delete an unknown key beside valid required fields and hand the executor silently narrowed args, while non-strict preserves unknown root fields so `executeDispatchTool`'s own always-strict parse names the invented field (legion #1242 review; the xd:// half is can1357/oh-my-pi#12871) — register it (and every Envoy tool) with `lenientArgValidation: true` so the host hands raw arguments through and `executeDispatchTool` / `parseEnvoyToolArguments` is the one refusal (a `ToolInputError` naming every problem), pass the live session id/title and host AbortSignal to `executeDispatchTool`, and never subscribe from a tool result: the `tool_result` hook only announces `details.follows` once per ask. |
| Role session prompts | `roles/` | Phase workers compose `core/<role>.md`, `mechanics/headless.md`, and the per-role residue; merger composes headless plus its residue. Root architect, controller, and sub-architect prompts remain single-file. The daemon concatenates the parts into the first portion of its one `--append-system-prompt` value (OMP's flag is last-wins), followed by the addressing fragment (roots and phase workers) and, when the deployment's `legion.yaml` sets `instructions`, `<state_dir>/deployment-instructions.md` as the last part |
| Real end-to-end delivery smoke | `smoke-delivery.sh`, `smoke-btw.sh`, `scripts/README.md` | Manual installed-plugin smokes against live Envoy; `smoke-btw.sh` creates a targeted Dispatch BTW or Steer attempt and verifies its correlated reply |

## Critical conventions

- Register every schema through the injected `pi.zod`. Every field counts, not just the outer object: OMP's converter reads internals (`.ir`) only its own Zod produces, and a field from another Zod instance fails the whole extension load (`undefined is not an object (evaluating 'e.ir.desc')`). The shared Dispatch contract exposes field shapes and cross-field validation through `dispatchToolSchema`; pass it `zodSchemaApi(pi.zod)`. `envoy.test.ts` proves every registered field came from the injected instance. |
- Keep direct NATS subscription lifecycle and Pi delivery adapter-local. Register `["aside", "btw", "steer"]` when `pi.askEphemeral` exists and `["aside", "steer"]` otherwise — both built from the contracts' `DELIVERY_CAPABILITIES`, never spelled here; an advertised `btw` frame never falls back to steering. Deliver targeted **Aside** / **Steer** with `triggerTurn: true`; reject an unparsed targeted frame without primary-turn injection, log it, and post its error to Dispatch whenever it has a reply address.
- Render every inbound envelope through `renderInbound`. Keep its bounded 50-item `envoy_inbox` metadata-only; use the shared `envoy_role_get` transport operation for current role holders.
- The registration heartbeat (`ensureHeartbeat`, `ENVOY_HEARTBEAT_MS`, default 120 s) re-asserts the session's held role after every successful re-registration: it reads `GET /v1/roles/<role>` and issues a soft `POST /v1/roles/set` only when the listener does not name this session as the live holder — a healthy tick writes nothing and appends no `envoy-role-claim` transcript entry. A 409 (a different live holder) drops the local claim, warns once, and ends re-assertion for that role; the newer holder is correct. A regain — or the first healthy tick after a failed registration, when a surviving claim may still have been unresolvable — fires `onEnvoyRoleRegained` detached from the heartbeat chain (a slow daemon never blocks the next re-registration), which re-runs the controller's `/controller/ready` and a root architect's `/process/ready` with bounded retries; a phase worker needs nothing, the daemon's own no-holder recovery prompts its catch-up. `legion.ts` registers that listener on the `LEGION_ROLE_CLAIM_BRIDGE` slot only once it holds a Legion identity (`claimController`, `bootstrapRoot`), since a `task` subagent's re-bound instance shares the process and would otherwise replace it.
- A `task` subagent's session shares its parent's identity in both extensions (`isSubagentSession` in `src/subagent-session.ts`): in a Legion process it claims no role, calls no daemon route, installs no tool gate, and never exits (`extensions/legion.ts`), and in every process `extensions/envoy.ts` skips its `session_start` and switch events entirely — no listener registration, no agent-subject subscription, no heartbeat — so `envoy ps` lists only top-level sessions and a finished subagent leaves no row heartbeating for the life of the parent process. envoy.ts additionally asks the host's own roster (`isRegisteredSubagent`: `AgentRegistry.global()` from `@oh-my-pi/pi-coding-agent`, where `createAgentSession` registers every session as `main`, `sub`, or `advisor` before `session_start` fires), which needs no transcript and so also covers a subagent under `OMP_SESSION_STORAGE=sql` or of a `--no-session` parent, and stays per session rather than per process (an ACP host runs several top-level sessions in one process). The transcript-based check is recognised by either of two signals: the process-local one — `bootstrapRoot`, `bootstrapWorker`, and the controller's `session_start` record the bootstrapped session's transcript path on `globalThis` under `Symbol.for("legion.pi-envoy.bootstrapped-session")`, and any later `session_start` in the same process with a different transcript path is a subagent — or OMP's on-disk layout for file storage (the parent's `.jsonl` sits beside the subagent's transcript directory). The process-local signal is what holds when the transcript is a SQL row rather than a file (LEGION-80: `OMP_SESSION_STORAGE=sql`); the on-disk check stays as the fallback for a process that has not bootstrapped anything.
- A daemon refusal of the boot registration itself — `/process/started` for a root, `/worker/started` for a phase worker or sub-architect — ends the process (`exitOnRegistrationRefusal` in `extensions/legion.ts`: one log line naming the route, the status, and the daemon's message, then `exitProcess(1)`) for every 4xx: a 400 or 404 (a request or a route the daemon does not have), a 403 (the boot token is stale, consumed, or unknown), and every 409 those routes answer — the same-agent rule (`Worker respawn must resume the same agent session`: this session is not the one the resumed claim recorded; under a database session store that is Oh My Pi having started a fresh session at a path whose row is gone), a stale generation (`Stale process generation` / `Stale worker generation`: the daemon already owns a newer launch of this role), and a tree being closed (`TreeClosingError`). None changes on retry, and a process that stayed up unregistered would sit alive under the daemon's boot watchdog with nothing ever retiring it; exiting hands the outcome to the daemon, which already holds the decision each 409 names (count the launch failure and decide the respawn, keep the newer generation, finish the close). Every other error there — a 5xx, a transport failure — propagates out of `session_start` without exiting, exactly as before (LEGION-81; `extensions/legion.test.ts` pins both routes for 403, 409, and the 500 negative control, and `/worker/started` for 400). The Go daemon's `/legion/v1/claims/register` has the same rule (`exitOnGoRegistrationRefusal`; see Daemon contract).
- A daemon `403 Invalid session secret` is recovered once per forgotten secret, shared by every request in flight: `src/legion/daemon-client.ts` keeps, per session id, the newest recovered secret and the recovery in flight — a refused request retries with a newer secret already known, else awaits the in-flight recovery, else starts the one `/legion/v1/worker-session` recovery, one retry per request and a second refusal returned to the caller — and `roleDaemon()` in `extensions/legion.ts` hands every caller the same client so that record is shared (LEGION-73).
- `spawnWorker` in `src/legion/daemon-client.ts` carries the caller's `requestId` (minted once per `legion` `spawn_worker` call in `src/legion/tools.ts`) and retries only a `fetch` that rejected — never a `LegionDaemonApiError`, whatever its status, and never a response-shape error — up to `SPAWN_WORKER_ATTEMPTS` (3) with `SPAWN_WORKER_RETRY_DELAYS_MS` between attempts, the same id every time so the daemon's ledger dedupes it; the last rejected fetch is a `LegionDaemonTransportError` naming the cause, attempts, and id. A response whose headers arrived but body cannot be read is not retried because `fetch` fulfilled; it is a `LegionDaemonResponseReadError` with the same request id and `legion state` guidance. The 403 recovery above composes with both paths unchanged (LEGION-102).
- `envoy_list` must report the union of locally live and registry-persisted topics, with each topic marked `live`, `registry`, or `both`.
- Do not alter `~/.omp` from this package. The README documents the local developer symlink.
- `smoke-delivery.sh` and `smoke-btw.sh` are manual, real end-to-end smokes against the installed plugin; never wire either into CI without live Envoy/NATS, Dispatch, and a configured model provider.
