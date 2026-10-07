# Pi Envoy Extension

Tracked Oh My Pi extension package `@sjawhar/pi-envoy`: the Envoy messaging and Dispatch tools
every session loads, in one entry, `extensions/envoy.ts`, with the four skills every session with
Dispatch reads (`dispatch`, `dispatch-first`, `dispatch-brainstorming`, `envoy`) staged into
`dist/skills` by `scripts/pi-plugin-prepack.sh` at the repository root. The Legion entry, its
modules, agents and skills are `@sjawhar/pi-legion` (`packages/pi-legion`, its `AGENTS.md`), which a
Legion pane loads beside this plugin and which reaches it through the interface in
`@legion/pi-shared` (`packages/pi-shared/AGENTS.md`); this package never imports that one.

## Overview

This package owns Pi-specific tool registration, direct NATS subscriptions, targeted Dispatch
delivery, and self-subscription registration for every session. HTTP transport, tool metadata, and
subject construction come from the Envoy core packages. Its NATS connection (the agent subjects in
`extensions/envoy.ts`) connects as the nkey user
`@legion/envoy-client/nats-auth` reads from `NATS_NKEY_SEED_FILE` or `NATS_NKEY_SEED`, and without a
credential when neither is set; an unusable seed fails the connect naming the variable, and a Legion
daemon strips both from every pane it launches, handing it instead the `legion-pane` seed file it
holds as `NATS_NKEY_SEED_FILE` when it has one. `@legion/envoy-client/delivery` is the sole inbound
renderer: it produces a tolerant TOON block and never exposes raw envelope bytes. A targeted
Dispatch **BTW** frame runs the host's side turn (`ctx.runEphemeralTurn` on Oh My Pi 18.3,
`pi.askEphemeral` on the earlier fork releases Legion pins) and posts its body or error to the
correlated delivery attempt; **Aside** and **Steer** call `pi.sendMessage` with their respective
delivery mode, except a person's direct Send or Aside (below). On a host with
`ctx.runEphemeralTurn` (the fork's 18.3 releases included, since the context's call wins there), a
BTW side turn still running when the handler that subscribed the agent subject (`session_start`, a
session switch, or a Legion handler re-establishing through the claim bridge) reaches the host's
30 s handler budget is aborted, and Dispatch gets the abort as the reply's error.
`pi.askEphemeral` does not inherit the handler's signal, so the pin is unaffected.

A person's direct Send or Aside from Dispatch's Agents page (Send is the dashboard's name for a
steer) becomes the user's own turn (`src/dispatch-user-turn.ts`). A frame is only a candidate
(`isUserTurnCandidate`: Dispatch's `message.created` on no issue, a person as actor, aside or
steer, naming no broadcast), since the listener takes a frame's source from whoever sends it; any
other frame keeps its card with no call to Dispatch. For a candidate the extension asks Dispatch,
with its own Dispatch bearer, to accept that attempt
(`POST /api/v1/messages/{id}/deliveries/{attempt}/accept`; its conditions are that route's row in
`packages/envoy/cmd/dispatch/AGENTS.md`, in short a person's own fresh Send or Aside to this
session), and only on that 200 sends the stored body the accept answers with
`pi.sendUserMessage` (`deliverAs: "aside"` for an Aside, nothing for a Send, as the accepted
attempt says; `turnFromAccept`), never the frame's text. Only the accept's success makes a turn;
the extension's own checks can only keep a card. Besides the candidate filter, it never accepts an
attempt it already delivered, as a card or as a turn: before either goes out it writes a
transcript entry (`envoy-dispatch-handled-attempt`, `{message_id, attempt}`), rebuilt from every
entry of the session file (`sessionManager.getEntries()`) on each restore, and a frame naming a
recorded attempt is a card with no accept call. So a replay, a frame forged inside the minute for
a Send that arrived as a card, and one forged after a restart are each a card, while a person's
retry, a new attempt, can still be their turn. Every refusal, error and timeout (10 s), a Dispatch
configuration that no longer resolves included, keeps the card and posts nothing. The stream tags
the injected user message with the message id (`dispatchMessageId`, passed to
`AgentStreamPublisher.record` and kept on the ring entry) so the dashboard shows it once; which user
message it is comes from one process-wide record keyed by session (`matchInjectedUserTurn`: the
first user message with the sent text, remembered under its host timestamp, forgotten at the run's
`agent_end`; a turn the record misses shows twice, and the phase-stall section of
`packages/pi-legion/AGENTS.md` says which turns those are and what a miss costs a phase worker).

The record's limit: it keys on the attempt a frame names, so a forger who reads `message.created`
(every authenticated caller's event stream carries it, and Dispatch publishes it before its own
frame goes out) and names the attempt first spends it. If the accept answers 200, the person's
stored body is the turn, and Dispatch's frame then arrives as a duplicate card when the forger used
a key of its own, or is dropped by the session's dedupe when it used Dispatch's own idempotency key
(`<message>:<mode>`). If it answers 404 because the attempt row is not committed yet, the person's
Send is a card and never a turn, and under Dispatch's key it is not shown at all. Naming attempts
not written yet spends each later retry of the message the same way. A forged frame's own text is
never a turn, so this is the class of the accept route's self-claimed actor, which already lets
any bearer spend a session's acceptance. Why the record is kept even when the accept answers 404
is `acceptedUserTurn`'s comment (`extensions/envoy.ts`).

Role claims are routed by the listener: this extension receives a receipt-backed request on its
direct agent subject instead of subscribing to a role subject itself. The agent pump replies the
moment the envelope is decoded — before the inbox update, any Dispatch call, or the session
injection — so the listener's two-second receipt window measures decoding, not the host's turn: a
claimed-but-deaf holder still becomes a `delivery_failed` exception, and a busy one does not
(LEGION-101). The receipt goes only to a role-lane frame — one whose envelope `topic` is not the
direct subject, the shape the listener forwards to the holder. Every other frame carrying a reply
inbox on the direct subject (a Dispatch author route, a peer `envoy_send`) is a JetStream publish
whose inbox belongs to the server's PubAck; an empty receipt there fails the publisher with
`nats: invalid jetstream publish response`. A frame that cannot be decoded is still never
acknowledged, and a receipt that fails to publish is logged while delivery continues.

## Legion

The Legion lifecycle is not this package's. The daemon contract (`legion.daemonApiVersion`, the
claim and controller sessions, the pane's environment) is described under "Daemon contract" in
`packages/pi-legion/AGENTS.md`. The session titles a Legion session gives itself are described
under "Session titles" there. A phase worker's handoff actions and the phase-stall follow-up are
described under "Phase workers' handoff actions and the phase-stall follow-up" there. What this
plugin gives the Legion entry is the interface it publishes at factory time
(`publishEnvoyPluginInterface(import.meta.url)`, `@legion/pi-shared/interface`): the role-claim
bridge the Legion entry claims through, the record of the user turns this plugin sends in, and the
slot for the bootstrapped session's transcript; what the Legion entry does with it, and how it
refuses to run without it, is "What this plugin needs from pi-envoy" there.

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
`dispatch_issue_update` moves an issue's lifecycle `status` (closing it, `status: "done"`, requires a `reason`, which the executor posts as an issue message before the PATCH because a closed issue takes no messages, comments, or artifacts; `reason` goes only with `done`, a failed message post sends no PATCH, and a PATCH that fails after the message landed names that message in its error), retitles it, replaces `labels`, sets its coarse `priority` (`0` is P0, the highest, through `3`, P3, the lowest; `null` clears it — agents set priority, `dispatch://LEGION/artifact/issue-status-conventions-md`), sets `route`, sets or clears its `parent` (a key in the same project; `""` clears, and the executor sends JSON `null`), attaches it to architecture `components` (`{mode: "inherit" | "explicit" | "none", ids?, reason?}`: `explicit` names bare component ids of the project's imported model, `none` needs a reason, `inherit` returns to the nearest ancestor's attachment; allowed on a closed issue; the result line reads `components -> explicit [a, b]` / `components -> none (reason)` / `components -> inherit`, and `400 COMPONENTS_INPUT` names an unknown, retired, or external id), or links URLs through `external_links` (merged into the existing links by URL, so linking the pull request just opened keeps earlier links); at least one field besides `issue` is required and `rank`, the board's own order, is not exposed. Its one-line result reads `KEY: status a -> b; priority -> P1; linked <url> (N links); parent -> KEY` (`priority cleared` / `parent cleared` on a clear), and a server refusal (`INVALID_STATUS`, `ISSUE_CLOSED`, `EXTERNAL_LINK_TAKEN`, `PARENT_INPUT`, `COMPONENTS_INPUT`) keeps its code at the head of the thrown message. `dispatch_read` of an issue renders a `Components:` line — `a, b (inherited from KEY)`, `none — <reason>`, or `unassigned`, plus `(retired: c)` for ids a re-import retired — and component nodes render like every other reference node at `dispatch://<PROJECT>/component/<id>`.
`dispatch_claim` claims the issue for this session before it starts implementing, and `{issue, release: true}` releases it. A live holder's claim answers `409 ISSUE_CLAIMED` naming that session, so the second agent talks to it instead of working the same issue; a human holder is named by login instead, with nothing said about a session running, since there is none to message. `409 CLAIM_CONTENDED` is the other refusal: the holder changed twice while the call ran, so nothing was applied and nobody's liveness was checked — read the issue and decide again. A claim whose session the Envoy listener no longer lists is taken automatically, and the session that lost it hears about the takeover on its own agent topic. The claim never moves the issue's status, so an agent that starts work claims the issue *and* moves it to `in_progress` with `dispatch_issue_update` — two explicit actions, because the status is also how humans track work. Closing an issue releases its claim.
`dispatch_ask` takes no `kind`: every ask it opens is a question whose options the asker chooses; no label is special to the server (a human to-do is the to-do phrased as the question, with whatever options fit it). `approval` asks are opened only through `dispatch_request_approval`.
`dispatch_comment` accepts `turn: "agent" | "human"` only with `reply_to_ask` (the shared cross-field validation rejects it otherwise): `agent` is a progress note that keeps the ask waiting on the agent in the human's Inbox, `human` (the default) hands the turn to the human. The result text names the resulting state (`ask now waiting on agent` / `human`) and `details.ask_waiting_on` carries it.
Quote anchors returned from Dispatch include nullable `block_id`: new anchors are pinned to the
lowest block containing their complete quote, while top-level cross-block and legacy anchors remain
unpinned.
`GET /comments/{id}` and `GET /asks/{id}` add `anchor_block`, where that block stands (path; in a
table the row, column, header and cells), and `dispatch_read` prints it as `Position:` after the
quote. When Dispatch could not read the document, the read carries `anchor_block_error` instead
and `dispatch_read` prints `Position: unavailable (<code>)`, the code the API answers that error
with elsewhere (`DOC_SERVICE_UNAVAILABLE`, `DOC_SCHEMA` or `INTERNAL`); the read itself, and
`dispatch_follow`, which reads the ask first, still succeed.

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

With Dispatch configured, a `context` handler (`src/dispatch-first.ts`) puts the `dispatch-first`
skill into every provider request as a user message after any leading `compactionSummary`
messages: top-level sessions, Legion panes and `task` subagents alike. Oh My Pi keeps nothing a
`context` handler returns, so the insert runs on each request rather than once; its text is read
once at module load (a package without `dist/skills/dispatch-first/SKILL.md` fails to load naming
it) and it carries no id, so its bytes repeat on every request. Only the insertion position is
checked for a copy already there (a second copy of the extension inserts at the same place): a
tool result, delivered message or reply that quotes `DISPATCH_FIRST_MARKER` is conversation and
never switches the skill off. Oh My Pi marks the inserted message per-call, so on Anthropic its
15-turn decimation cache anchors are not placed behind it, and a session reads the same cache on
every request as a session without it, plus the skill's own tokens. `src/dispatch-first.test.ts`
holds the insertion rules, and `extensions/dispatch-first-omp.test.ts` checks turn 1, turn 2, after
a compaction, and without Dispatch on the real binary.

`before_agent_start` injects nothing into the conversation; its open-asks query arms the run-end
nudge only for a turn carrying the user's own text, its snapshot `as_of` becoming the period's first
window. It runs only for a session the stop could actually nudge — the host awaits this handler, so
an excluded session does not pay up to `OPEN_ASKS_TIMEOUT_MS` for an answer nothing reads.

`agent_end` is the nudge's stop signal. Every normally settled, UI-hosted session with an armed
period runs one **silent self-check**: one side turn — the host's one-shot, tool-free model call
over a snapshot of the conversation, the same channel a targeted Dispatch BTW uses. The check uses
the open-asks snapshot it reads to name the session's own open asks in the prompt: the first line of
at most five questions, each truncated to about 120 characters, followed by `+N more`; with no asks
it says so. Every Unicode line separator ends that displayed first line, and other C0/C1 controls
become spaces, so a listed ask cannot add prompt lines. The one prompt asks whether the agent is
waiting on a human for anything those asks do not cover, answered with exactly WAITING or
PROCEEDING. An open ask or `opened_since` never suppresses this check.

Only a reply whose first word is WAITING, and which does not also name PROCEEDING (a model echoing
the choice rather than making it), produces the one hidden `dispatch-ask-reminder` steer with
`triggerTurn`: it says the agent is waiting on a human for something no open ask covers, and tells
it to open one now: a decision block in the document the wait concerns (`dispatch_doc_edit` with an
ask block), or `dispatch_ask` for a to-do only a human can do, naming exactly what it needs and from
whom. It never offers `dispatch_request_approval`: an approval request is for a settled spec, not a
way to wait on a human. The parse is case-sensitive and first-word-only because a false
WAITING is the expensive error — its steer tells an agent to page a human with a question it does
not need — while a false PROCEEDING is only silence. PROCEEDING, an unparsable reply, a side-turn
failure, the timeout, and a host with no side turn at all are silent; the last also arms no period.

The check runs on the host's managed timer (`ctx.setTimeout`) once `agent_end` returns, not inside
the handler: on Oh My Pi 18.3 a side turn started while a handler is still running inherits that
handler's abort signal, which the host fires at its 30 s handler budget. When the timer fires, the
check first confirms its settle is still current against the run count, the user-turn generation and
the session id captured at the stop, and returns without reading Dispatch otherwise.

The timeout is the extension's own clock, not the host's: the call gets an `AbortSignal`, but the
check races it against `ASK_SELF_CHECK_TIMEOUT_MS` (60 s, moved by `ENVOY_SELF_CHECK_TIMEOUT_MS`)
and always settles there so a host that ignores the signal cannot hold the one-check-at-a-time
latch. A late answer is dropped. A user typing, a session change, the agent opening an ask itself,
and a post-race staleness re-check abort the in-flight call; an ask opened mid-check never produces
its verdict. A failure is logged once per session (`logger.warn`), never notified.

The check is owed and spent like the host's todo reminder rather than once per period: the arming
turn owes one, running it spends it whatever came back, the agent opening the ask itself spends it
too (`dispatch_ask`, `dispatch_request_approval`, a `dispatch_issue` or `dispatch_artifact` whose
result counts a decision block in the stored document (`advice.decision_blocks`), or a
`dispatch_doc_edit` whose result counts a decision block the edit added
(`advice.decision_blocks_added`): `opensAsk` in `src/opens-ask.ts`), and the agent's next real
work — a successful `tool_result` whose tool is not a `dispatch_*` one — owes another. Both counts
are the Dispatch server's reading of the document, so the extension parses no markdown: an opener
quoted in code counts nothing and one in a blockquote or a list item counts, as the server stores
them, and an edit result from a server that reports no count spends nothing. The server counts
answered blocks in a stored document too, so re-uploading a document whose blocks are all answered
spends the check with nothing new in the Inbox: that stop goes without a reminder. An edit that
writes an answered or person-retracted block's id back reports it added, though settlement leaves
that row closed, so it has the same gap. A tool-device
call (a `write` to `xd://<tool>`, named by its result's `details.xdev.tool`: `deviceTool`) never
opens an ask by its `write`, and counts as work by the tool it names: a write to a `dispatch_*`
device is no work, and a write to any other device is work, as before. A device backed by a
registered tool is reported twice, the tool Oh My Pi ran under its own name, input and details and
then the `write`, so the tool's own report is what opens an ask; Oh My Pi's own devices (`resolve`,
`reject`, `propose`, `report_issue`) report only the `write`. A help write (`?`, `help` or empty
content) runs nothing and has only the `write`. A settled turn that only replies calls no tool, so
the nudge's continuation cannot re-arm itself; work is bounded by `ASK_CHECKS_PER_PERIOD` (5).

Each completed check carries `baseline_as_of` to its snapshot's `as_of`, keeping the next open-asks
read current. One stop-time check runs at a time: `agent_end` handlers are not awaited by the host,
so a latch spans the Dispatch read and self-check; a settle recorded while it is held is re-checked
when the first check ends only if it is still current under the same test the timer applies. Every
run the host starts increments the run count that test reads, so an Envoy delivery or another newer
run makes an old verdict stale and unsteerable. The latch, period, and budget are in memory only; a
cold start or session change begins at period 0, which nudges nothing until the next genuine user
turn arms one. The normal-settle, UI-host, Legion-managed, task-subagent, generation, staleness, and
one-check-at-a-time guards remain load-bearing.

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
| OMP extension entry | `extensions/envoy.ts` | The one entry; ships as `dist/envoy.js` in `@sjawhar/pi-envoy` and loads in every installed OMP session. The Legion entry is `../pi-legion/extensions/legion.ts` (`packages/pi-legion/AGENTS.md`) |
| Legion lifecycle | `../pi-legion/` | The daemon client, the claim and controller sessions, the grant file, the `legion` tool and the phase stall are `@sjawhar/pi-legion`'s: see `packages/pi-legion/AGENTS.md` |
| Shared modules and the interface | `../pi-shared/` | `@legion/pi-shared`: the interface this entry publishes (`interface`), the role-claim bridge, the injected-user-turn record, the subagent check (`subagent-session`), the host types and `toolSuccess`/`toolFailure`; inlined into `dist/envoy.js` by `bun build`. See `packages/pi-shared/AGENTS.md` |
| Extension unit tests | `extensions/envoy.test.ts` | Mocked Pi and NATS surface; `beforeEach` points `ENVOY_URL` at an unroutable host and stubs `fetch` with the registration echo, so a test that forgets its own stub never registers a `ses_*` fixture on the devbox's real listener, and resets the process-wide interface so no Legion suite's record reaches it |
| Skills partition and its guard | `src/skills-guard.test.ts`, `scripts/prepack.test.ts`, `scripts/pi-plugin-prepack.sh` (repository root) | The four skills this package ships, staged as its prepack stages them, held to the size, name and link rules in `@legion/pi-shared/test/skills-guard` and the `dispatch-first` budget |
| No import of the sibling | `src/no-cross-import.test.ts` | Fails on a shipped source under `extensions/` or `src/` whose relative import resolves into `packages/pi-legion` |
| Shared HTTP/tool behavior | `../envoy-client/src/` | Do not duplicate it here |
| Event subjects | `../contracts/src/subject.ts` | Canonical subject construction |
| Dispatch tools | `extensions/envoy.ts` (the `registerTool` block), `@legion/contracts` (`dispatchToolSpecs`, `dispatchToolSchema`, `zodSchemaApi`), `@legion/envoy-client/dispatch-execute` (`executeDispatchTool`) | Registers the twenty-one native tools only when `resolveDispatchConfig` resolves URL and token. Build each tool schema with `dispatchToolSchema(spec, zodSchemaApi(pi.zod))` — deliberately NOT strict: on installed OMP hosts a strict host schema makes the coercion pass delete an unknown key beside valid required fields and hand the executor silently narrowed args, while non-strict preserves unknown root fields so `executeDispatchTool`'s own always-strict parse names the invented field (the xd:// half is can1357/oh-my-pi#12871) — register it (and every Envoy tool) with `lenientArgValidation: true` so the host hands raw arguments through and `executeDispatchTool` / `parseEnvoyToolArguments` is the one refusal (a `ToolInputError` naming every problem), pass the live session id/title and host AbortSignal to `executeDispatchTool`, and never subscribe from a tool result: the `tool_result` hook only announces `details.follows` once per ask. |
| Real end-to-end delivery smoke | `scripts/smoke-delivery.sh`, `scripts/smoke-btw.sh`, `scripts/README.md` | Manual installed-plugin smokes against live Envoy; `smoke-btw.sh` creates a targeted Dispatch BTW or Steer attempt and verifies its correlated reply |

## Critical conventions

- Register every schema through the injected `pi.zod`. Every field counts, not just the outer object: OMP's converter reads internals (`.ir`) only its own Zod produces, and a field from another Zod instance fails the whole extension load (`undefined is not an object (evaluating 'e.ir.desc')`). The shared Dispatch contract exposes field shapes and cross-field validation through `dispatchToolSchema`; pass it `zodSchemaApi(pi.zod)`. `envoy.test.ts` proves every registered field came from the injected instance. |
- Keep direct NATS subscription lifecycle and Pi delivery adapter-local. Register `["aside", "btw", "steer"]` when the host has a side turn (`ctx.runEphemeralTurn` on Oh My Pi 18.3, or `pi.askEphemeral` on an earlier fork release; the context's call wins when both exist; pi-envoy wraps the question in the /btw prompt itself, which the older call left to the host) and `["aside", "steer"]` otherwise — both built from the contracts' `DELIVERY_CAPABILITIES`, never spelled here; an advertised `btw` frame never falls back to steering. Deliver targeted **Aside** / **Steer** with `triggerTurn: true`; reject an unparsed targeted frame without primary-turn injection, log it, and post its error to Dispatch whenever it has a reply address.
- Render every inbound envelope through `renderInbound`. Keep its bounded 50-item `envoy_inbox` metadata-only; use the shared `envoy_role_get` transport operation for current role holders.
- The registration heartbeat (`ensureHeartbeat`, `ENVOY_HEARTBEAT_MS`, default 120 s) re-asserts the session's held role after every successful re-registration: it reads `GET /v1/roles/<role>` and issues a soft `POST /v1/roles/set` only when the listener does not name this session as the live holder — a healthy tick writes nothing and appends no `envoy-role-claim` transcript entry. A 409 (a different live holder) drops the local claim, warns once, and ends re-assertion for that role; the newer holder is correct. A regain — or the first healthy tick after a failed registration, when a surviving claim may still have been unresolvable — fires `onEnvoyRoleRegained` detached from the heartbeat chain (a slow daemon never blocks the next re-registration), which re-runs a claim's `claims/ready` with bounded retries; the controller re-runs nothing, since the daemon holds nothing for it. `packages/pi-legion/src/claim-session.ts` registers that listener on the shared interface's `roleClaim.regained` slot (`@legion/pi-shared/interface`, `packages/pi-shared/AGENTS.md`) only once it holds a Legion identity, since a `task` subagent's re-bound instance shares the process and would otherwise replace it.
- A `task` subagent's session shares its parent's identity in both plugins (`subagentSessionCheck` in `@legion/pi-shared/subagent-session`): in a Legion process it claims no role, calls no daemon route, installs no tool gate, and never exits (`packages/pi-legion/extensions/legion.ts`), and in every process `extensions/envoy.ts` skips its `session_start` and switch events entirely — no listener registration, no agent-subject subscription, no heartbeat — so `envoy ps` lists only top-level sessions and a finished subagent leaves no row heartbeating for the life of the parent process. Both entries ask the host's own roster first (`registeredSubagent`: `AgentRegistry.global()` from `@oh-my-pi/pi-coding-agent`, where `createAgentSession` registers every session as `main`, `sub`, or `advisor` before `session_start` fires), which needs no transcript and no write, so it also covers a subagent under `OMP_SESSION_STORAGE=sql` or of a `--no-session` parent, and stays per session rather than per process (an ACP host runs several top-level sessions in one process). The roster's answer outranks the transcript layout. Only where the roster gives no opinion (a host build without the export, or a session the global roster does not list) does the transcript decide, once `ensureOnDisk` has published it; that publish stays, since the transcript is then the only signal and has to be on disk to read as top-level. It fails while another writer holds the transcript's publish lock (Oh My Pi's `SessionLockError`); the check then answers that one call from the transcript as it is on disk, logs a warning, and asks again at the next call, so a lost race never fails a hook or the tool call it gates. Each hook calls the check once and passes the answer on, and each extension instance keeps only a settled answer, for its life. The transcript check is recognised by either of two signals: the process-local one — the claim session's boot and a launched controller's claim (`packages/pi-legion/src/claim-session.ts`, `packages/pi-legion/src/controller-session.ts`) record the bootstrapped session's transcript path on the shared interface (`bootstrappedSession` in `@legion/pi-shared/interface`), and any later `session_start` in the same process with a different transcript path is a subagent — or OMP's on-disk layout for file storage (the parent's `.jsonl` sits beside the subagent's transcript directory). The process-local signal is what holds when the transcript is a SQL row rather than a file (LEGION-80: `OMP_SESSION_STORAGE=sql`); the on-disk check stays as the fallback for a process that has not bootstrapped anything. The identity it shares is its reply address. Every top-level instance publishes its own session on `globalThis` under `Symbol.for("legion.pi-envoy.envoy-session")` (`src/envoy-session.ts`), keyed by its transcript path, at the one chokepoint that moves the module id — `restoreLocalSessionState`, which a start, a switch and the heartbeat's drift heal all run — and deletes its previous key when its id or transcript path changes, so a retired session leaves no entry to resolve. A session is recorded from its `session_start`, before the host has minted its id — under its transcript path, with the empty id it has — so a fresh TUI's subagent resolves that session and reports no address until the drift heal fills the id in, rather than falling through to another live session; and `session_shutdown` deletes the entry, since a deregistered session is not an address a reply reaches. It is a map, not one slot, because an ACP host runs several top-level sessions in one process: one slot would hand a subagent whichever of them last started. A subagent, whose module `sessionID` stays empty, resolves its own by walking OMP's layout up — `dirname(<own transcript>) + ".jsonl"`, repeated for a nested subagent — until a recorded session matches, and `envoy_whoami`, the `/whoami` command, and the `source_session` of `envoy_send` and `envoy_publish` all name that session. `envoy_whoami` reports it as `session_id` and the subagent's own host id under `subagent`. Where the walk matches nothing, one recorded session is used only when it is the only one in the process; otherwise there is no reply address at all — `session_id` is empty, the `subagent` note says so, and `packages/envoy-client/src/transport.ts` omits `source_session` rather than send an empty one, which the listener's `omitempty` erases on the way out, costing the recipient both the sender label and the reply hint. Naming an unrelated live session is never the answer. A subagent's `envoy_publish` never reaches its own parent: the listener delivers nothing to the session an envelope names as its source — `roleTopicDelivery` skips the resolved holder and `fanoutDelivery` skips any interest whose session is the source — so a publish to a role the parent holds, or to any topic it subscribes to, is accepted and delivered to nobody. The publish tool result says so for the role case, where the answer names the holder; the hop a subagent actually has to its parent is hub. A direct `envoy_send` is unaffected, the agent-subject lane having no such skip. The host's own `AgentRegistry` carries a `parentId`, but it is undocumented and a host upgrade could change it silently; the published record is this package's own.
- What a daemon refusal of the boot registration does to a Legion process (`exitOnRegistrationRefusal`) is the Legion entry's rule: see the critical conventions of `packages/pi-legion/AGENTS.md`.
- Nothing under `extensions/` or `src/` imports `packages/pi-legion`: what both plugins need lives in `@legion/pi-shared`, which each bundles (`src/no-cross-import.test.ts`).
- `envoy_list` must report the union of locally live and registry-persisted topics, with each topic marked `live`, `registry`, or `both`.
- Do not alter `~/.omp` from this package. The README documents the local developer symlink.
- `scripts/smoke-delivery.sh` and `scripts/smoke-btw.sh` are manual, real end-to-end smokes against the installed plugin; never wire either into CI without live Envoy/NATS, Dispatch, and a configured model provider. Each runs its session on its own tmux server, on a socket in its temp directory (`tmux() { command tmux -S "$tmux_socket" "$@"; }`), and prints the `tmux -S <socket> attach -t <session>` line that watches it, so its cleanup's `tmux kill-session` can end only the session it started: on the shared default server that kill could end anyone's session. Keep new tmux calls on that wrapper rather than the default server.
