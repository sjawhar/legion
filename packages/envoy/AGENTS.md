# Envoy Package

Go-based cross-machine event transport and delivery subsystem.

## Overview

Envoy owns transport, routing, delivery, and the native Dispatch event path:

- ingests Slack, GitHub, Ghost Wispr, and agent events
- publishes ordinary notifications through JetStream and role lanes through core NATS
- resolves target OpenCode sessions
- delivers by hot `prompt_async` (ordinary events stay in JetStream for retry if a session is unavailable)
  publishes retained `notifications.dispatch.issue.<KEY>.<type>`,
  `notifications.dispatch.document.<PROJECT>.<slug>.<type>`, and
  `notifications.dispatch.project.<PROJECT>.<type>` envelopes

It does not own Legion workflow policy. The daemon/controller decides what to do; Envoy moves
events to the right session.

## Where to look

| Task                   | Location                                  | Notes                                              |
| ---------------------- | ----------------------------------------- | -------------------------------------------------- |
| Webhook handlers       | `internal/webhook/{github,slack,ghostwispr}.go` | HTTP ingress, signature verification, publish path |
| Webhook config         | `internal/webhook/config.go`                     | ENVOY_WEBHOOKS parsing, startup validation         |
| Listener behavior      | `cmd/listener/main.go`                    | subscribe/match/deliver flow                       |
| NATS client            | `internal/bus/nats.go`                    | reconnect/self-heal logic                          |
| Session delivery       | `internal/session/session.go`             | hot delivery via prompt_async                      |
| Interest storage       | `internal/store/kv.go`                    | JetStream KV subscriptions                         |
| Topic matching         | `internal/routing/match.go`               | wildcard matching                                  |
| Envelope normalization | `internal/contracts/*.go`                 | generated contract + source-specific normalization |
| Native Dispatch workspace | `cmd/dispatch/`, `internal/dispatch/` | HTTP API, Postgres store, documents, and event outbox |
| Document tree (Proof schema) | `internal/dispatch/pmdoc/` | render/parse/diff of Proof documents; fixtures from the fork's headless engine |
| Deploy/runtime         | `deploy/`                                 | compose, rollout scripts, NATS peer setup          |

Every non-inline Proof node has a stable `blockId`. `pmdoc.Parse` mints IDs in document order,
and `EnsureBlockIDs` repairs legacy or duplicate IDs before agent updates are written. Document
settlement is two-phase: it first applies `EnsureBlockIDs` in one Yjs transaction and persists that
captured update in the same Postgres transaction as any resulting version and event, then renders
and compares canonical markdown. `envoy-dispatch backfill-block-ids` runs that closure across every
document. Every write path that changes a document queues that closer once its transaction commits: a live edit (`POST /api/v1/artifacts/{id}/edits`), an uploaded document version (`POST /api/v1/issues/{key}/artifacts`, `POST /api/v1/projects/{key}/artifacts`), and a spec seeded at issue creation - so ask blocks written by any of them become asks without waiting for a later live change. The closer attributes the asks it indexes to the room's most recent mutating actor (`roomState.lastActor`, set by every edit, replacement and seed) when no pending author remains - an edit's own version write has already consumed `pending` by the time settlement runs. A free-text ask block (no bullet list) carries `options: []` on the wire, never JSON null.

Quote-anchored asks and comments retain their inline mark and quote cache, plus the stable `block_id`
of the lowest block containing the complete quote. A quote that spans top-level siblings stays
unpinned. `GET /api/v1/artifacts/{id}/blocks` returns each block's canonical markdown range and
`{comments, asks}` reference counts. The server resolves the block when it creates a quote or
browser-mark anchor; `envoy-dispatch backfill-anchor-blocks` fills legacy anchors only when their
cached quote has one current match.

Document edits (`POST /api/v1/artifacts/{id}/edits`, `docs/edits.go` `applyOperation`) are
`replace`, `delete`, `insert`, `retype`, and `move`. `replace` is inline: `with` parses through
`pmdoc.ParseInline` (paragraph-only block grammar), so a leading list or heading marker is text and a
multi-paragraph `with` is `INVALID_OP`. `delete` takes `find` or `block`; a `find` covering a
textblock's whole text removes that block (`pmdoc.DeleteTextblock`: it also drops a list, list item,
or blockquote it empties, hoists a nested list into the place of a bullet whose text goes, and
refuses a bullet with other content with `ErrListItemContent` naming `delete {block:"<item id>"}`;
`pmdoc.DeleteBlock` serves `block` and reports any emptied container's content rule as
`INVALID_OP`). `move` relocates the block with `block` to the document-level
boundary of an insert anchor (`pmdoc.MoveBlock`); insert and move anchors are a quote, `start`,
`end`, `heading:<title>`, or `block:<id>`. Block ids stay with moved and retyped nodes, so the ask
reconciliation keeps a moved ask; a removed block retracts its ask only while the ask is open (an
answered or resolved ask is already closed, and `asks_answer_state_check` forbids a resolved row
that still carries an answer). `ApplyOps` stamps `EnsureBlockIDs` on the live tree before
resolving operations so every block is addressable.

References form one graph. Mentions (`dispatch://` refs and same-origin dashboard URLs in a
document version, ask question, comment body, or issue message) are derived on every write into
`refs` by `refs.Replace`, which reconciles rather than rewrites: an edge that survives keeps its
`created_at` and `source_seq` (the `events.id` that introduced it, stamped by `refs.Stamp` right
after the source's event is appended, in the same transaction). Structural relations stay in the
columns that own them and the `graph_edges` view (migration 0032) unions both into one typed edge
relation: `mentions`, `child_of` (`issues.parent_key`), `attached_to` (`artifacts.issue_key`),
`anchored_to` (ask/comment anchors), `owned_by` (project-document asks/comments), `replies_to`
(comment and message threads), `followed_by` (`ask_followers`). Artifact targets are addressed by
`ref_key`, artifact sources by uuid; each arm has the index its `to`/`from` predicate needs.
`GET /api/v1/references?to=|from=` reads the view; `envoy-dispatch rebuild-refs` reparses every
source and reconciles the index (the text is the truth), deleting edges whose source no longer
exists, and refuses to run without `dispatch.server_url`.

Typed document blocks are declared only in `internal/dispatch/pmdoc/schema/blocks.json`. The
embedded file is the server-owned schema, `GET /api/v1/schema/blocks` returns its exact JSON, and
the fixture generator reads that checked-in file. A typed block is CommonMark generic-directive
syntax: `:::name{#block-id key="value"}` followed by block children and a matching `:::`. There is
no whitespace between `name` and `{`; Pandoc fenced divs, leaf directives, and text directives are
invalid outside code blocks. An unclosed typed block at document level is rejected, while one nested
inside another block runs to that parent’s end.

A typed block renders its `blockId`, defaulted attributes, and every explicitly set optional
attribute. Parsing mints an omitted id, while live document reads and writes validate each node
against its schema content rule. Schema changes are additive: add a type, add a defaulted
attribute, add an enum choice, or widen a content rule. Tightening content, removing or renaming a
type or attribute, or requiring a new attribute requires a document migration and version bump.

`ask` blocks are indexed at settlement: their body and client-owned attributes update the ask row,
the row restores server-owned answer state into the block, and removal retracts the indexed ask.
An invalid browser-edited ask retains its indexed ask, carries the server-owned `invalid` parse-error
attribute, and emits `block.invalid`; repairing its body clears `invalid` before updating the ask row.
An answered block carries `state`, `answered_by`, `answered_at`, `selected`, and `answer` in
canonical markdown.

## Critical conventions

- `packages/contracts` defines the TypeScript event schemas consumed by Dispatch clients. The Go Dispatch server maintains its emitted event names and wire payloads separately; `bun run gen:go` generates Envoy envelope validation only.
- Issues carry a nullable coarse priority (`P0` highest through `P3` lowest) alongside their server-generated fractional `rank`. `POST /api/v1/issues` and `PATCH /api/v1/issues/{key}` accept `priority` as `0` through `3` or null; each priority write emits `issue.updated`. Issue and pinned lists sort by lifecycle status, then rank, then creation time; priority is a badge and a filter, never a sort key, so the List and the Board (whose columns keep the list's order) show the same order. `PATCH /api/v1/issues/{key}` accepts neighboring issue keys as `rank.before` and/or `rank.after`, validates they share the project, and serializes rank allocation per project before it rewrites only that issue's order key.
- Every issue has a nullable `assignee`: the **lowercase** GitHub login of the human who answers its asks (`issues.assignee`, migration `0034`, indexed on open issues). `parseAllowedLogins` lowercases `DISPATCH_ALLOWED_LOGINS` and the identity implementations compare lowercased, so `api/issue_assignee.go`'s `canonicalLogin` (`strings.ToLower(strings.TrimSpace(login))`) is the stored form and plain `=` compares it; `/auth/whoami` still echoes GitHub's display casing (`Xodarap`), so the SPA lowercases the viewer once and compares exact. `POST /api/v1/issues` and `PATCH /api/v1/issues/{key}` accept `assignee` (the PATCH's `json.RawMessage` tri-state: absent leaves it, `null` clears, a string is canonicalised and must be an allowlist key — else `400 ASSIGNEE_NOT_ALLOWED` naming the login; a non-string is `400 INVALID_ISSUE`). Any authenticated actor may set it; the event's actor is who assigned it, and the whole issue rides in `issue.created` / `issue.updated`, so there is no `assigned_by`. Absent on create, the default is the first match of: a human actor's login; a personal token's `Owner`; the parent's assignee (which may be null); null — so the shared token's parentless issues are unassigned and a daemon status PATCH (no `assignee` key) never touches it. `GET /api/v1/users` (human-only) returns the allowlist keys sorted as `{users: [{login}]}` — the picker's options, a pure config read; `GET /api/v1/whoami` (any auth) returns `{kind: "user", login}` for a cookie/header caller or `{kind: "agent", owner}` for a bearer (`owner` is the personal token's lowercase login, null under the shared token) — what `dispatch_whoami` reports. Issue reads (`GET /issues/{key}`, summaries, pinned) carry `assignee`, and so does the `issue` on every inbox row.

- Open asks accept `PATCH /api/v1/asks/{id}` from their asking session or any human. Each edit carries the full current ask, prior mutable fields, and its editor in an `ask.edited` event; `edited_at` is nullable until the first edit. Ask anchors are set on creation and are not editable through this route. `GET /api/v1/asks/{id}` returns `edits`, every rewording read back from those events oldest first (`{previous, edited_by, at}`). A human answer must carry the `edited_at` revision it reviewed; a mismatch returns `409 ASK_EDITED` without closing the ask.
- `POST /api/v1/issues/{key}/asks` and `POST /api/v1/artifacts/{id}/asks` create ordinary questions by default or an action with `kind: "action"`. Action asks always have `Done` / `Can't` options and no multiple selection; `Can't` requires text, `Done` answers normally, and an asker may edit its wording but not its fixed options. `kind: "approval"` is server-created by the document-approval route only.
- Document approval is a human review pinned to a version, the way a pull-request review is pinned to a commit. `POST /api/v1/artifacts/{id}/approval-requests` (any actor) opens - or returns the open - ask of `kind: "approval"` with the fixed options `Approve` / `Request changes`, naming the document and its latest settled version in `ask.approval`; its wording cannot be edited. Answering it (humans only; `Request changes` requires text) writes an `artifact_reviews` row pinned to the document's latest settled version at answer time and appends `artifact.approved` or `artifact.changes_requested` (`{artifact_id, name, version, actor, reason, ask_id}`) on the document's owner alongside `ask.answered`. `POST /api/v1/artifacts/{id}/reviews` `{state, reason?}` (humans only) writes the same review from the document header and answers the open approval ask if there is one (`ask_id` null otherwise). Every document read carries `approval` (`draft | awaiting | approved | stale | changes_requested`, with `latest_version`, the latest review's `version/by/at/reason/ask_id`, and `requested_by` while awaiting); `stale` is derived from versions, so a new version emits nothing approval-specific. Legion's design gate is the consumer; it is the exception path, not an every-issue step.
- `POST /api/v1/issues` and `PATCH /api/v1/issues/{key}` accept up to 20 labels. Dispatch trims labels, preserves case, removes case-insensitive duplicates, and returns `400 LABELS_INPUT` for blank or over-40-character labels; every label update emits `issue.updated` with its labels. `GET /api/v1/issues?label=<label>` is repeatable, normalizes filter labels identically, and case-insensitively matches every supplied label.
- A reply to an open ask (`ask_id` set on `POST /api/v1/issues/{key}/comments` or `POST /api/v1/artifacts/{id}/comments`) records `turn` (`comments.turn`, migration 0028): who holds the turn after it. A human author's reply always stores `agent` whatever the request says; a session author's stores `human` unless the request says `turn: "agent"` (a progress note - the agent still owes the next move). A reply under an answered or resolved ask records no turn (nothing is waiting; a requested `turn` is ignored there, as a human's is). `turn` on a comment that is not an ask reply is `400 TURN_REQUIRES_ASK`; any value but `human`/`agent` is `400 INVALID_COMMENT`. Comment objects carry `turn` (null under a closed ask and off ask replies). The column's only constraint is `turn requires ask_id`, so a pre-0028 server still draining during a deploy inserts its ask replies with a null turn; every reader coalesces a null newest-reply turn to `human`.
- Every ask the API serves is read through one row shape: `docs.AskColumns` + `docs.ScanAsk` decode the ask row (the document settler reads its indexed ask blocks through the same pair), `api/ask_rows.go` extends it with the block ask's document (`askRowColumns`, a `left join artifacts` - the shape `ask.*` event payloads are built from via `loadAskForUpdate`) and, for reads, with the newest comment in the ask's thread (`askReadColumns`, a `lateral ... limit 1` join). `opened_event_id` is attached afterwards by `attachOpenedEventIDs`, one `events` query per read served by the partial index `events_ask_payload_id` (`store/migrations/0030_events_ask_payload_id.up.sql`; its `type in (...)` list mirrors that query and must change with it). `asks.options` is always a JSON array: `0031_ask_options_array.up.sql` rewrote stored nulls in rows and `ask.*` payloads and added the `asks_options_array` check, so no reader normalises it.
- Every open ask read carries `waiting_on` (`human` | `agent`): the `turn` of the newest comment with that `ask_id`, `human` when nobody has replied (`scanAskRead`, `api/ask_rows.go`; `askReadFrom` and `GET /api/v1/asks/open` both read the newest reply through the one `lastReplyJoin` lateral join). It is on `GET /api/v1/inbox` rows, `GET /api/v1/asks/{id}`, `GET /api/v1/issues/{key}/asks`, `GET /api/v1/artifacts/{id}/asks`, and the issue detail's `open_asks`; closed asks and the `ask.*` event payloads (built from `loadAskForUpdate`) do not carry it. A `comment.created` payload that replies to an open ask carries the resulting `ask_waiting_on` so a stream consumer moves the ask without a refetch.
- `GET /api/v1/inbox` rows carry the owning issue's nullable `priority` and `last_reply` (`{author, created_at}` of the newest comment with that `ask_id`, or null) beside `waiting_on`. It groups rows by `waiting_on` (human first), then sorts each group by priority ascending with unset priorities last before recency. The client uses the same open-ask response to surface every `waiting_on: human` row under `Waiting on you` and its `Blocked on you` count; `waiting_on: agent` rows are `Waiting on agents`, whoever replied last. `GET /api/v1/issues/{key}` carries the same `last_reply` and `waiting_on` on every `open_asks` row (`issueOpenAsk`, `issue_queries.go`), so the issue header applies the inbox's whose-turn rule without the human-only inbox. A `comment.created` payload that replies to an ask carries `ask_state` (the ask's state at posting time) beside `ask_question`, so an agent can tell a clarification request on its open ask from discussion after the answer.
- `GET /api/v1/inbox?assignee=me|unassigned|<login>` (one value, combining with `project`) narrows the human-only inbox by the owning issue's assignee (`inboxAssigneeFilter`, `api/inbox.go`): `me` is the caller's lowercase login; a `<login>` is canonicalised (`strings.ToLower(strings.TrimSpace(...))`) before the allowlist check, so a shared `?assignee=Alice` link never 400s, while an unlisted login is `400 ASSIGNEE_NOT_ALLOWED`; `me` / `<login>` keep issue-owned asks with `i.assignee = $login`, `unassigned` keeps asks on unassigned issues **and every project-document ask** (a document has no assignee, so it is unassigned by definition); no value returns everything. This is the contract for shareable links and humans' scripts: the SPA fetches the unfiltered inbox once (the one `["inbox"]` query every surface shares) and partitions it client-side into **Mine** (rows whose `issue.assignee` equals the lowercased `/auth/whoami` login, plus an **Unassigned** band with an `Assign to me` PATCH on issue rows) and **Everyone**, defaulting to Mine and remembering the choice per login (`inbox.view`); a `?view=mine|everyone` URL wins over the remembered choice.
- `GET /api/v1/asks/open?author_session=<session-id>[&since=<RFC3339>]` lists every active open ask authored by that session across open issues and unlinked project documents. It requires a nonblank session ID, returns exact counts split by whose reply is next plus the full oldest-first inventory, and reports `opened_since` over all asks so a just-closed ask still disarms an automatic reminder.
- Event IDs are the SSE resume cursor. `events.Broker.Append` takes one global transaction-scoped advisory lock after it locks the event owner and before inserting, so IDs are allocated in commit order. A transaction that appends to multiple owners must call `LockOwners` with every owner before its first append; it locks them deterministically before any transaction can hold the global lock. This deliberately serializes concurrent event-producing transactions across every owner; do not bypass the broker with direct event inserts.
- Project creation and repository mappings own project-scoped events (`project.created`, `project.updated`, `settings.repo_project.updated`); per-user issue-state writes emit project-owned `user_state.updated` and do not advance an issue unread sequence. These events are retained on `notifications.dispatch.project.<PROJECT>.<type>` but never wake agents or take a direct agent/role route.
- Per-user agent state is its own table and route pair, not a key in `GET /api/v1/me/state` (that map is keyed by issue). `PUT /api/v1/me/agents/{session_id}/state` (human-only) takes `{cleared_before: <RFC3339>}` and upserts `user_agent_state(login, session_id, cleared_before)` (`store/migrations/0033_user_agent_state.up.sql`); a malformed value, a missing one, or one more than a minute ahead of the server clock (`clearedBeforeSkew`, the allowance for a fast browser clock) is `400 INVALID_STATE`. `GET /api/v1/me/agents/state` returns `{[session_id]: {cleared_before}}` for the caller. Neither write touches `messages` nor appends an event: the cutoff is a view preference the Agents page applies client-side (an exchange is hidden when its newest message is at or before the cutoff), so a reply that lands after the Clear still surfaces; other devices pick it up on their next state read.
- `GET /api/v1/issues/{key}/subscribers` and `GET /api/v1/artifacts/{id}/subscribers` (human-only) list the sessions whose persisted Envoy interests match that issue's or unlinked document's topic family, merging `GET /v1/interests/` with `GET /v1/sessions` for live status and title. `DELETE .../subscribers/{session_id}` removes matching topics with `POST /v1/interests/unsubscribe`; it first commits `subscription.remove_requested` with `pending: true`, which the outbox does not publish. After idempotent listener removal succeeds, Dispatch appends `subscription.removed` with the `request_event_id` it settles and routes that notice directly to the unsubscribed session's `notifications.agent.<session_id>` topic in addition to the owner topic.
- The Dispatch API describes itself. `internal/dispatch/api/routes_table.go` is the one list of `/api/v1` routes (`apiRoute{Method, Pattern, Auth, Description, Handler}`, `Auth` one of `public`, `any`, `human`, `bearer`); `Register` mounts that table and public `GET /api/v1` serves it as `{routes: [{method, path, auth, description}], docs: "skills/dispatch/SKILL.md"}` sorted by path then method. A new route is a new row (and a bumped pin in `routes_table_test.go`), never a `mux.HandleFunc` line. An unknown path under `/api`, `/v1`, `/auth`, `/ws`, or `/healthz` is `404 {"code":"NOT_FOUND","error":"no route for GET /v1/issues","hint":"GET /api/v1 lists every route"}` from `routes/router.go`, decided before any dashboard lookup, so an API caller never receives the SPA shell. `GET /api/v1/agents` is readable by any authenticated caller (a session picks a message target by the `capabilities` it advertises); the issue-less `POST /api/v1/agents/{session_id}/messages` and `GET /api/v1/agents/{session_id}/messages` stay human-only.
- Every session that writes to an ask follows it: `asks.FollowAuthor` runs at all three ask insert sites (`POST .../asks`, approval requests, ask blocks indexed from documents) and on every ask reply (`POST .../comments` with `ask_id`), so no insert path can miss it; `store/migrations/0029_ask_followers.up.sql` backfills existing asks and replies. `GET /api/v1/asks/{id}` returns `followers` (`{session_id, since}`, oldest first); `GET /api/v1/asks/{id}/followers` (any authenticated actor) returns the same list. `PUT` / `DELETE /api/v1/asks/{id}/followers/{session_id}` add or remove one follower and append `ask.follower_added` / `ask.follower_removed` (`{ask_id, session_id, by}`): a bearer sends `{ "actor": { "kind": "session", "id": "<own id>" } }` in the body on both verbs and may act only when the path session equals it (`403 FOLLOWER_FORBIDDEN`); a human (cookie identity) sends no body and may add or remove any session. A non-UUID ask id is `400 ASK_ID_INPUT`; removing a session that does not follow is `404 FOLLOWER_NOT_FOUND`; a repeated `PUT` is a `204` no-op with no event.
- Keep Envoy API-level with OpenCode. Do not add DB introspection or OpenCode-specific hidden coupling unless there is no API path.
- Dispatch caps (`CAP_EXCEEDED` 400) read `<field> is N characters over the M-character limit (L/M)` (`capExceededError`, UTF-16 units) or `<field> is N over the M-item limit (C/M)` for counts; `GET /asks/{id}`, `/comments/{id}`, and `/issues/{key}/messages/{id}` answer a non-uuid id with 400 `<KIND>_ID_INPUT` (`requireUUIDPath`); a document-edit quote miss (`TARGET_NOT_FOUND`) names the three nearest blocks and a `# Title` quote selects a heading. The outbox `payload_summary` of `ask.answered` is the answer rendering (`<selected> - <text>`), not the question.
- Listener message APIs preserve the human `message` as a one-line `payload_summary` of at most 160 characters; when it differs, the complete message is `payload`. A supplied `payload` for `/v1/messages/publish` wins over the derived value.
- Ghost Wispr only publishes `session_started`, `session_ended`, and `summary_ready`; other verified events should return 200, log the skip, and not publish.
- `ENVOY_GHOSTWISPR_SIGNING_SECRET` is optional for trusted Ghost Wispr deployments; when unset, skip signature verification explicitly rather than half-verifying missing headers.
- GitHub mention routing is additive: matching comments publish to both `.comment` and `.mention` topics.
- Slack topics must use the real Slack `team_id`, not a workspace slug.
- NATS peer storage uses named Docker volumes, not repo-path bind mounts.
- Role lanes use core NATS, not JetStream: the listener queue subscriber resolves the live holder at delivery time, then makes a receipt-backed request to that holder's agent subject (`bus.Client.RequestCoreTo`). The agent pump returns an empty receipt after accepting the envelope. No receipt within two seconds from a registered, live holder is `receipt_timeout` (the message was forwarded and not acknowledged; the Legion daemon treats it as delivered to a live process) — keyed on `bus.ErrReceiptTimeout`, which `RequestCoreTo` returns only after the publish and the flush both succeeded and the receipt wait ran out; the flush is bounded by the same two-second window, and a flush that fails or times out (a reconnecting or stalled connection still buffering the forward) is the client's own error, so it is `delivery_failed`, never `receipt_timeout`. `delivery_failed` is a claim whose message is not known to have reached the holder (holder lookup failed, holder stale, the publish or flush failed); `no_holder` is no claim at all. Every reason emits an exception; the attempt cache holds an entry only while a forward is in flight and both forward failures roll it back, while the dedupe cache records a forward only when its receipt arrived — so a publish that re-uses a `dedupe_key` after a `receipt_timeout` is forwarded again, while one after a delivered forward is skipped. Do not add durable role consumers or retry transit for role messages.
- Role ownership is durable in the `envoy_roles` JetStream KV bucket. Each role key records `holder_session_id`, `claimed_at`, and `previous_session_id`; listener restart restores the claim from that record, but routes only while the holder is present in the `envoy_sessions` registry. Reaping stale interests never releases a role; a restored absent holder gets one registry TTL to re-register, then loses its claim atomically on the role reaper or next resolution, while the first core role delivery still emits its normal delivery exception.
- A failed control delivery emits `notifications.envoy.exceptions.<original-topic>`. Its payload preserves `original_topic`, `event_id`, `reason` (one of `no_holder`, `delivery_failed`, `receipt_timeout`), `payload_summary`, the original machine `payload`, `dedupe_key`, `source`, and `source_session`; the exception lane is not recursively exceptional. An API publish to an unheld role is rejected synchronously with 404 instead.
- **Source-specific vs generic ingestion**: Envoy has two ingestion paths: listener-hosted webhook handlers behind `readinessGate` (`internal/webhook/{github,slack,ghostwispr}.go`) and the generic MCP bridge (`cmd/mcp/`). The MCP bridge connects to any MCP server that publishes resources, so it's the low-maintenance default for new sources. Building source-specific webhook logic adds maintenance burden — consider whether the cost justifies the benefit over the generic MCP bridge before adding custom source-specific logic to Envoy. When using the MCP bridge, Envoy should stay naive about the message content — the MCP server owns the domain logic.

## Security

Dispatch treats an agent endpoint and bearer token as one trust-bound configuration: a repository `dispatch.serverUrl` can use only the token in that same repository file, while explicit environment configuration supplies both. The deployed server's `DISPATCH_SERVER_URL` is separate: it overrides the merged `dispatch.serverUrl`, must be an absolute `http` or `https` URL with no path, and is the exact GitHub OAuth callback origin. `NATS_URLS` likewise overrides merged `natsUrls` for the server. A GitHub login the OAuth callback exchanges but `DISPATCH_ALLOWED_LOGINS` does not list is logged (`dispatch: login not allowed login=<login>`) and answered with a 403 HTML page naming that login and linking back to `/auth/start`, so an operator can find who to add; the JSON `LOGIN_NOT_ALLOWED` stays on the API paths. Browser sessions carry a server-side generation that logout advances, and unsafe cookie-authenticated requests must prove the configured same origin; bearer automation remains separate. JSON decoding is limited to 1 MiB, multipart uploads retain their explicit 26 MiB limit, and the GitHub proxy has the same bounded request buffer. The event outbox retries each required issue, route, and author destination with exponential backoff, so a failed or poison delivery cannot be marked complete or starve later notifications.

## Operational notes

- Health endpoints reflect dependency health, not just process liveness. `/healthz` returns `degraded` for transient JetStream/KV probe failures and `unhealthy` for NATS loss, a stopped session or CI KV watcher, or a missing durable consumer.
- NATS reconnects indefinitely with backoff. Every reconnect recreates the session and CI KV watchers; the self-health monitor also rebuilds those watchers and a missing durable consumer while NATS is connected.
- Only a terminal failure that remains after three consecutive recovery intervals self-terminates the listener. Shutdown stops HTTP first, bounds the NATS drain to ten seconds, logs completion, and exits non-zero so Docker's restart policy can restore it.
- If a session is not live in the registry, delivery fails and the message is NAK'd for retry (up to MaxDeliver attempts over the stream's MaxAge window).
- The `ENVOY_NOTIFICATIONS` duplicate window is 72 hours, matching the retained notification lifetime. Startup reconciles that setting with `UpdateStream`, so a Dispatch outbox retry after a post-publish crash cannot create another retained message while the original remains available.
- Cross-machine route correctness depends on valid session registry entries with non-null ports.

## Listener API

| Endpoint | Method | Contract |
| --- | --- | --- |
| `/v1/messages/send` | POST | Sends to a live `target_session`. Dispatch uses `source: "dispatch"`, an idempotency key, and a `payload` JSON string whose frame is `{event, delivery}`; the response is the envelope plus `recipient` with the full target session ID. |
| `/v1/messages/publish` | POST | Publishes a non-agent topic. An optional `dedupe_key` is used verbatim as the envelope's dedupe key (how a re-sent copy stays recognisable to the receiver's own dedupe); it is mutually exclusive with `idempotency_key` — both present is a 400 whose `expected` names both fields — it may not begin with the reserved forward mark `envoy.role.forward.` (a 400 naming `dedupe_key`; the role arbiter drops such an envelope on sight, so accepting it would be a 200 for a message that vanishes), and an empty string is absent; without either key the key is minted. A `notifications.role.<role>` topic requires a live holder and returns that session ID in `holder`. |
| `/v1/roles/<role>` | GET | Returns the live role holder, including its capabilities and `last_seen`, or 404 when no holder is live. |
| `/v1/roles/set` | POST | Claims a role for a live session and registers its role topic. Last-claim-wins by default. With `"soft": true` the claim lands only if the role is unheld, already this session's, held by a session that is no longer live, or held by the declared `previous_session_id` (the id a fork/branch continues); any other live holder answers `409 {error, role, holder}` and nothing changes. |
| `/v1/interests/subscribe` | POST | Persists session topics, route metadata, and optional delivery `capabilities`; registrations without capabilities persist `[]`. The response can include `warnings` when a GitHub repository has no retained events. |
| `/v1/interests/unsubscribe` | POST | Removes the supplied topics and returns them in `removed`. |
| `/v1/sessions` | GET | Lists live sessions; optional case-sensitive substring filters are `dir` and `title`. Rows include `roles`, `capabilities`, and `last_seen`. |
| `/v1/interests/` | GET | Lists persisted interests. |
| `/v1/interests/<session_id>` | GET, DELETE | Gets or removes a session's persisted interests. |
| `/v1/registry/<session_id>` | GET | Gets a live session registry entry. |
| `/v1/sessions/<session_id>` | DELETE | Removes a live session registration. |

`send` and `publish` accept optional `in_reply_to`, `supersedes`, `urgency`,
`expects_reply`, and `expires_at`, and `publish` additionally `dedupe_key`; empty optional
fields are omitted. `urgency` is `low`, `med`, `high`, or `blocking`; `expects_reply` is
`none`, `optional`, or `required`. Every `/v1` 4xx/5xx response, including the startup
readiness gate, is JSON: `{"error":"<message>","expected":["field"]}`. `expected` appears
when the caller must provide a field.

## Targeted Dispatch messages

Any authenticated caller — a browser session or a bearer naming its session in `actor` — can
create an issue message with `target: "session:<id>"` or `target: "role:<name>"` and
`delivery: "btw" | "aside" | "steer"`; a bearer-authored targeted message is authored by that
session, never by a human. A human may also create an issue-less, session-targeted message with
`POST /api/v1/agents/{session_id}/messages` `{body, delivery, in_reply_to?}`, and
`GET /api/v1/agents/{session_id}/messages` returns that session's issue-less and issue-anchored
targeted roots newest first with their deliveries and reply chains (a reply in the chain carries
its own deliveries). Dispatch resolves a role holder
and checks the selected session's capabilities for every attempt, then makes the synchronous
listener send; `POST /api/v1/messages/{id}/deliveries` creates an explicit retry attempt (same
callers, same `actor` rule for bearers). The targeted session alone uses
`POST /api/v1/messages/{id}/reply` for the attempt's automatic BTW response; an ordinary agent
reply uses `dispatch_message({ in_reply_to })` on the same open issue.

Messages thread: `in_reply_to` names a message in the same conversation - a message of the same
issue, or, for `POST /api/v1/agents/{session_id}/messages`, an issue-less message whose thread
root targets that same session (400 `MESSAGE_INPUT` otherwise) - and the reply's event and
delivery frame carry `reply_body`, the parent's first 160 characters. A reply is recorded as
`message.answered`; the broker's `Notify` treats it exactly like `message.created` (quiet when
the payload has a `target`, else the user-actor rule), so a human's reply on a plain agent message
wakes the issue's route and the parent message's author (`notifications.agent.<session>`,
correlated `re:` the parent with `reply_body`) instead of being recorded silently. A human's reply that names no `target` inherits the thread's:
when the root of the reply's ancestry was targeted, the reply is delivered to that target in the
mode of the thread's most recent delivery attempt, exactly like a fresh targeted message (its own
`message_deliveries` row and `message.delivery` event), so a human's follow-up on an agent's
answer reaches that agent and the agent answers it through `POST /api/v1/messages/{reply id}/reply`.
A session's own reply never inherits (the target would be itself), and an explicit `target` wins.
`POST /api/v1/messages/{id}/reply` with a `body` is accepted on a `failed` attempt as well as a
`sent` one — the session answering is proof the message reached it, whatever the receipt said
(a stale plugin, `nats: invalid jetstream publish response`, an error the session itself reported
earlier) — and records the attempt as `sent` with no error and the reply's id; an `error` on an
already-failed attempt returns the stored attempt unchanged, and a second `body` on an answered
attempt returns the stored reply (200).

The issue stream retains the targeted `message.created`, `message.delivery`, and
`message.answered` events for the Conversation card. Issue-less targeted-message events have no
issue owner, use sequence `0`, and reach their recipient through the synchronous listener send;
the outbox marks them published without republishing an agent-topic envelope, while the browser
receives them through the server's SSE stream. They are excluded from global `/search` and issue
reference closures. Targeted issue `message.created` skips bound-route and author fan-out because
the synchronous listener call records the sent or failed attempt instead of blind republishing.

When `ENVOY_API_TOKEN` is set, `/v1` requires its matching bearer token, and a non-loopback listener refuses to start without it unless `ENVOY_API_ALLOW_UNAUTHENTICATED=1` is the temporary Fargate transition flag.

## Topic shapes

- GitHub repository topics start with `notifications.github.<owner>.<repo>`.
  Pull requests use `pr.<n>` for lifecycle (a closed event carries
  `merged`, `merge_commit_sha`, `merged_by`, and `head_sha`), plus
  `pr.<n>.comment`, `pr.<n>.review`, `pr.<n>.mention`, and
  `pr.<n>.checks` when the head's checks settle. Check settlement is at-least-once: a settlement can be followed by a `superseded_settlement: "true"` payload. Every settlement carries its attempt set `check_runs` — the latest GitHub check-run id per check name, sorted by name — plus the listener's `generation` (the record's state version) and `snapshot` (the record's hash). Consumers order same-head settlements by the attempt set, compared per shared name: no id lower and some id higher (or a new name — a new name counts as higher) is newer; every shared id equal and no new name is the same set; no id higher, no new name, and some id lower is older; anything else (a higher or new alongside a lower) is a mixed view and is dropped as a conflict (names only in the stored set are ignored — a check can vanish from GitHub's view, and a record recreated after the seven-day KV TTL starts sparse). Within one producer record per-name ids never decrease, and a consumer's fence is the per-name maximum over every view it has accepted — an accepted set merges into the fence, nothing is pruned — so the fence never decreases either: a newer attempt is newer whatever its completion time, no timestamps take part in ordering, and a name an incomplete view omitted cannot later reappear as new. At the same set the listener's `generation` orders its own settlements: lower is stale; equal is a duplicate when the `snapshot` matches and otherwise a conflict (an equal pair with a different snapshot cannot occur within one record's lifetime; a recreated record may reuse one and is dropped). A live settlement is a possibly incomplete view of the head (a missed webhook, a record recreated after the KV TTL): it decides the outcome of every name it reports — at any id the ordering accepted, including the same run observed in place — and says nothing about the rest: a known failure among them stands (the consumer keeps failure names, not a per-name status map), and the head is red while any failure remains. A consumer that reconciles a verdict from GitHub's rollup compares the rollup's attempt set the same way, but GitHub's read is complete: its failing check runs and failing commit statuses replace the stored ones wholesale. Statuses have no check run and the listener never sees them, so a consumer keeps them apart from check-run failures: a check run that shares a status's name cannot retire it — only GitHub does (likewise a deleted check's failure). A newer rollup set merges into the fence and takes the identity (no listener generation); the same set applies GitHub's verdict and keeps the listener identity for duplicate detection; an older, mixed, or empty-over-fenced set is ignored. A terminal read (green or red) then holds the tie at that set: a live settlement at the same set is accepted only if its effective outcome — the check-run failures it reports plus the stored ones it omits and the stored commit-status failures — agrees with the reconciled verdict, refreshing the listener identity without releasing GitHub's authority; a disagreeing one is stale whatever its generation until the set advances; a pending or cancelled-only read uncertifies a green head, leaves a red one untouched, and holds nothing — it releases any authority held at that set — so the terminal live settlement that follows applies at once, subject to the ordinary generation and duplicate rules (a replay or a lower generation still does not apply). Pending is therefore not a commutative join: a pending read after a live green uncertifies it until the next terminal view. Two remainders. An in-place conclusion change on an existing run id: GitHub's view stands and the listener's is recovered by the next successful, non-skipped read at that set — the dropped delivery is not replayed. A check whose highest run is deleted on GitHub: the fence keeps that id, so a rollup reporting a lower run under the same name is older until a newer run appears. A consumer that orders head changes by the PR's `updated_at` (GitHub's second resolution) accepts a read of a different head at an equal clock — a stale read returning the previous head within the same second as its replacement rewinds that consumer until its next accurate, non-skipped read. A head publishes only when at least one check has a positive run id; legacy checks without one remain in the status groups and failing names but not in `check_runs`. A legacy in-progress check whose completion is never observed holds the head unsettled until it reruns; rerun the affected check to release it. `workflow.<file>.<action>` carries only runs without an associated
  pull request.
- A branch or tag push publishes `push.branch.<ref>` / `push.tag.<ref>` with the payload fields
  `kind: "push"`, `repo`, `ref`, `after`, `before`, `pusher`, `head_subject`, `commit_count`,
  `compare_url`, `changed_paths` (the unique paths across every pushed commit's `added`,
  `removed`, and `modified` lists, in first-seen order, newline-separated, at most 100; omitted
  when no commit is listed, since the payload drops empty strings), and `changed_paths_truncated`
  (`"true"` when more than 100 unique paths were seen, else `"false"` — present on every push, so
  its absence alone tells a consumer the listener predates the field). Envoy forwards what GitHub
  sent; what counts as a handoff-only push is the Legion daemon's rule, not the listener's.
- A `pull_request_review` payload carries the review's own `commit_id` and the PR's current
  `head_sha` so consumers can tell whether the review is at head; `pull_request_review_comment`
  carries `head_sha` too, and all three comment/review events set `legion_footer: "true"` when the
  uncapped body contains a `<!-- legion:` worker footer.
- NATS `>` matches one or more trailing tokens, not its base subject. A subscription to a concrete
  `<subject>.>` is registered as the pair `<subject>` and `<subject>.>`, so the recommended
  per-PR default receives lifecycle plus child events.
- Dispatch issue events use `notifications.dispatch.issue.<KEY>.<type>`, carry every event, and
  are retained in JetStream; `notify` only controls agent wake and routing the same envelope to
  `notifications.role.<role>` or `notifications.agent.<session_id>` (the issue's route, which also
  applies to a comment or ask anchored on the issue's own artifact). An ask-scoped event -
  `ask.answered`, `ask.edited`, `ask.resolved`, and every `comment.*` carrying an `ask_id` - is
  additionally published straight to each follower's `notifications.agent.<session_id>` regardless
  of `notify` and of the issue's route, so a session-authored reply on an ask reaches its followers
  even though it wakes nobody through the issue topic; the acting session is skipped and a session
  already reached by the issue's route is not sent the envelope twice. A notifying comment-thread
  or message-reply event without an `ask_id` keeps the author routes: a comment reply's
  thread-root author and its direct parent author when different; a message reply's direct parent
  author; and, for a bare resolve or reopen, the resolved comment's own root author. An author
  route is skipped for a human author and for a session replying to its own thread.
  `ask.follower_added` and `ask.follower_removed` also route directly to the session they name,
  since a newly added follower is not subscribed to the issue topic by default.
- Dispatch document events use `notifications.dispatch.document.<PROJECT>.<slug>.<type>` and
  are retained in JetStream. Consumers subscribe or tail that document topic directly; a document
  has no delivery route, so the same author routes above are the only way an involved session
  learns about a human reply, resolution, reopening, or edit on its ask or comment thread.
- Dispatch project events use `notifications.dispatch.project.<PROJECT>.<type>` and are retained in JetStream. They have no issue route; project, repository-setting, and user-interface state consumers refresh from the event stream.
- Direct agent topics use `notifications.agent.<session_id>`.
- Role topics use `notifications.role.<role>` and are normally published to,
  rather than subscribed to by role holders.
- A failed control delivery publishes
  `notifications.envoy.exceptions.<original-topic>`.
