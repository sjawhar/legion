# Envoy Package

Go-based cross-machine event transport and delivery subsystem.

## Overview

Envoy owns transport, routing, delivery, and the native Dispatch event path:

- ingests Slack, GitHub, Ghost Wispr, and agent events
- publishes ordinary notifications through JetStream and role lanes through core NATS
- resolves target OpenCode sessions
- delivers by hot `prompt_async` (ordinary events stay in JetStream for retry if a session is unavailable)
- runs the Dispatch server, which persists native issues, documents, and events in Postgres and
  publishes retained `notifications.dispatch.issue.<KEY>.<type>` and
  `notifications.dispatch.document.<PROJECT>.<slug>.<type>` envelopes

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
document.

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
An answered block carries `state`, `answered_by`, `answered_at`, `selected`, and `answer` in
canonical markdown.

## Critical conventions

- `packages/contracts` is the source of truth for event contract shape; regenerate Go output from there.
- Issues carry a server-generated fractional `rank`: `PATCH /api/v1/issues/{key}` accepts neighboring issue keys as `rank.before` and/or `rank.after`, validates they share the project, and serializes rank allocation per project before it rewrites only that issue's order key. Issue lists sort by lifecycle status then rank.

- Open asks accept `PATCH /api/v1/asks/{id}` from their asking session or any human. Each edit carries the full current ask, prior mutable fields, and its editor in an `ask.edited` event; `edited_at` is nullable until the first edit. Ask anchors are set on creation and are not editable through this route. `GET /api/v1/asks/{id}` returns `edits`, every rewording read back from those events oldest first (`{previous, edited_by, at}`).
- `POST /api/v1/issues/{key}/asks` and `POST /api/v1/artifacts/{id}/asks` create ordinary questions by default or an action with `kind: "action"`. Action asks always have `Done` / `Can't` options and no multiple selection; `Can't` requires text, `Done` answers normally, and an asker may edit its wording but not its fixed options. `kind: "approval"` is server-created by the document-approval route only.
- Document approval is a human review pinned to a version, the way a pull-request review is pinned to a commit. `POST /api/v1/artifacts/{id}/approval-requests` (any actor) opens - or returns the open - ask of `kind: "approval"` with the fixed options `Approve` / `Request changes`, naming the document and its latest settled version in `ask.approval`; its wording cannot be edited. Answering it (humans only; `Request changes` requires text) writes an `artifact_reviews` row pinned to the document's latest settled version at answer time and appends `artifact.approved` or `artifact.changes_requested` (`{artifact_id, name, version, actor, reason, ask_id}`) on the document's owner alongside `ask.answered`. `POST /api/v1/artifacts/{id}/reviews` `{state, reason?}` (humans only) writes the same review from the document header and answers the open approval ask if there is one (`ask_id` null otherwise). Every document read carries `approval` (`draft | awaiting | approved | stale | changes_requested`, with `latest_version`, the latest review's `version/by/at/reason/ask_id`, and `requested_by` while awaiting); `stale` is derived from versions, so a new version emits nothing approval-specific. Legion's design gate is the consumer; it is the exception path, not an every-issue step.
- `POST /api/v1/issues` and `PATCH /api/v1/issues/{key}` accept up to 20 labels. Dispatch trims labels, preserves case, removes case-insensitive duplicates, and returns `400 LABELS_INPUT` for blank or over-40-character labels; every label update emits `issue.updated` with its labels. `GET /api/v1/issues?label=<label>` is repeatable, normalizes filter labels identically, and case-insensitively matches every supplied label.
- `GET /api/v1/inbox` rows carry `last_reply` (`{author, created_at}` of the newest comment with that `ask_id`, or null). The client uses the same open-ask response to surface every row whose last reply is not a human's under `Waiting on you`, oldest first, and its `Blocked on you` count; human-last rows remain `Waiting on agents`. A `comment.created` payload that replies to an ask carries `ask_state` (the ask's state at posting time) beside `ask_question`, so an agent can tell a clarification request on its open ask from discussion after the answer.
- `GET /api/v1/issues/{key}/subscribers` and `GET /api/v1/artifacts/{id}/subscribers` (human-only) list the sessions whose persisted Envoy interests match that issue's or unlinked document's topic family, merging `GET /v1/interests/` with `GET /v1/sessions` for live status and title. `DELETE .../subscribers/{session_id}` (human-only) removes the matching topics via `POST /v1/interests/unsubscribe` and appends a `subscription.removed` event, which the outbox routes directly to the unsubscribed session's own `notifications.agent.<session_id>` topic in addition to the issue's own topic — the only way that session learns it was unsubscribed even though it no longer receives the issue's events.
- Keep Envoy API-level with OpenCode. Do not add DB introspection or OpenCode-specific hidden coupling unless there is no API path.
- Listener message APIs preserve the human `message` as a one-line `payload_summary` of at most 160 characters; when it differs, the complete message is `payload`. A supplied `payload` for `/v1/messages/publish` wins over the derived value.
- Ghost Wispr only publishes `session_started`, `session_ended`, and `summary_ready`; other verified events should return 200, log the skip, and not publish.
- `ENVOY_GHOSTWISPR_SIGNING_SECRET` is optional for trusted Ghost Wispr deployments; when unset, skip signature verification explicitly rather than half-verifying missing headers.
- GitHub mention routing is additive: matching comments publish to both `.comment` and `.mention` topics.
- Slack topics must use the real Slack `team_id`, not a workspace slug.
- NATS peer storage uses named Docker volumes, not repo-path bind mounts.
- Role lanes use core NATS, not JetStream: the listener queue subscriber resolves the live holder at delivery time, then makes a receipt-backed request to that holder's agent subject. The agent pump returns an empty receipt after accepting the envelope; no receipt within two seconds is `delivery_failed` and emits an exception. Do not add durable role consumers or retry transit for role messages.
- Role ownership is durable in the `envoy_roles` JetStream KV bucket. Each role key records `holder_session_id`, `claimed_at`, and `previous_session_id`; listener restart restores the claim from that record, but routes only while the holder is present in the `envoy_sessions` registry. Reaping stale interests never releases a role; a restored absent holder gets one registry TTL to re-register, then loses its claim atomically on the role reaper or next resolution, while the first core role delivery still emits its normal delivery exception.
- A failed control delivery emits `notifications.envoy.exceptions.<original-topic>`. Its payload preserves `original_topic`, `event_id`, `reason`, `payload_summary`, the original machine `payload`, `dedupe_key`, `source`, and `source_session`; the exception lane is not recursively exceptional. An API publish to an unheld role is rejected synchronously with 404 instead.
- **Source-specific vs generic ingestion**: Envoy has two ingestion paths: listener-hosted webhook handlers behind `readinessGate` (`internal/webhook/{github,slack,ghostwispr}.go`) and the generic MCP bridge (`cmd/mcp/`). The MCP bridge connects to any MCP server that publishes resources, so it's the low-maintenance default for new sources. Building source-specific webhook logic adds maintenance burden — consider whether the cost justifies the benefit over the generic MCP bridge before adding custom source-specific logic to Envoy. When using the MCP bridge, Envoy should stay naive about the message content — the MCP server owns the domain logic.

## Security

Dispatch treats an agent endpoint and bearer token as one trust-bound configuration: a repository `dispatch.serverUrl` can use only the token in that same repository file, while explicit environment configuration supplies both. The deployed server's `DISPATCH_SERVER_URL` is separate: it overrides the merged `dispatch.serverUrl`, must be an absolute `http` or `https` URL with no path, and is the exact GitHub OAuth callback origin. `NATS_URLS` likewise overrides merged `natsUrls` for the server. Browser sessions carry a server-side generation that logout advances, and unsafe cookie-authenticated requests must prove the configured same origin; bearer automation remains separate. JSON decoding is limited to 1 MiB, multipart uploads retain their explicit 26 MiB limit, and the GitHub proxy has the same bounded request buffer. The event outbox retries each required issue, route, and author destination with exponential backoff, so a failed or poison delivery cannot be marked complete or starve later notifications.

## Operational notes

- Health endpoints reflect dependency health, not just process liveness. `/healthz` returns `degraded` for transient JetStream/KV probe failures and `unhealthy` for NATS loss, a stopped session or CI KV watcher, or a missing durable consumer.
- NATS reconnects indefinitely with backoff. Every reconnect recreates the session and CI KV watchers; the self-health monitor also rebuilds those watchers and a missing durable consumer while NATS is connected.
- Only a terminal failure that remains after three consecutive recovery intervals self-terminates the listener. Shutdown stops HTTP first, bounds the NATS drain to ten seconds, logs completion, and exits non-zero so Docker's restart policy can restore it.
- If a session is not live in the registry, delivery fails and the message is NAK'd for retry (up to MaxDeliver attempts over the stream's MaxAge window).
- Cross-machine route correctness depends on valid session registry entries with non-null ports.

## Listener API

| Endpoint | Method | Contract |
| --- | --- | --- |
| `/v1/messages/send` | POST | Sends to a live `target_session`. Dispatch uses `source: "dispatch"`, an idempotency key, and a `payload` JSON string whose frame is `{event, delivery}`; the response is the envelope plus `recipient` with the full target session ID. |
| `/v1/messages/publish` | POST | Publishes a non-agent topic. A `notifications.role.<role>` topic requires a live holder and returns that session ID in `holder`. |
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
`expects_reply`, and `expires_at`; empty optional fields are omitted. `urgency` is `low`,
`med`, `high`, or `blocking`; `expects_reply` is `none`, `optional`, or `required`. Every
`/v1` 4xx/5xx response, including the startup readiness gate, is JSON:
`{"error":"<message>","expected":["field"]}`. `expected` appears when the caller must
provide a field.

## Targeted Dispatch messages

Humans can create an issue message with `target: "session:<id>"` or `target: "role:<name>"` and
`delivery: "btw" | "aside" | "steer"`. Dispatch resolves a role holder and checks the selected
session's capabilities for every attempt, then makes the synchronous listener send; `POST
/api/v1/messages/{id}/deliveries` creates an explicit retry attempt. The targeted session alone
uses `POST /api/v1/messages/{id}/reply` for the attempt's automatic BTW response; an ordinary
agent reply uses `dispatch_message({ in_reply_to })` on the same open issue.

The issue stream retains the targeted `message.created`, `message.delivery`, and
`message.answered` events for the Conversation card. The outbox always publishes the base issue
topic envelope, but targeted `message.created` skips bound-route and author fan-out because the
synchronous listener call records the sent or failed attempt instead of blind republishing.

When `ENVOY_API_TOKEN` is set, `/v1` requires its matching bearer token, and a non-loopback listener refuses to start without it unless `ENVOY_API_ALLOW_UNAUTHENTICATED=1` is the temporary Fargate transition flag.

## Topic shapes

- GitHub repository topics start with `notifications.github.<owner>.<repo>`.
  Pull requests use `pr.<n>` for lifecycle (a closed event carries
  `merged`, `merge_commit_sha`, `merged_by`, and `head_sha`), plus
  `pr.<n>.comment`, `pr.<n>.review`, `pr.<n>.mention`, and
  `pr.<n>.checks` when the head's checks settle. Check settlement is at-least-once: a settlement can be followed by a `superseded_settlement: "true"` payload. Every settlement carries its attempt set `check_runs` — the latest GitHub check-run id per check name, sorted by name — plus the listener's `generation` (the record's state version) and `snapshot` (the record's hash). Consumers order same-head settlements by the attempt set, compared per shared name: no id lower and some id higher (or a new name — a new name counts as higher) is newer; every shared id equal and no new name is the same set; no id higher, no new name, and some id lower is older; anything else (a higher or new alongside a lower) is a mixed view and is dropped as a conflict (names only in the stored set are ignored — a check can vanish from GitHub's view, and a record recreated after the seven-day KV TTL starts sparse). Within one producer record per-name ids never decrease, and a consumer's fence is the per-name maximum over every view it has accepted — an accepted set merges into the fence, nothing is pruned — so the fence never decreases either: a newer attempt is newer whatever its completion time, no timestamps take part in ordering, and a name an incomplete view omitted cannot later reappear as new. At the same set the listener's `generation` orders its own settlements: lower is stale; equal is a duplicate when the `snapshot` matches and otherwise a conflict (an equal pair with a different snapshot cannot occur within one record's lifetime; a recreated record may reuse one and is dropped). A live settlement is a possibly incomplete view of the head (a missed webhook, a record recreated after the KV TTL): it decides the outcome of every name it reports — at any id the ordering accepted, including the same run observed in place — and says nothing about the rest: a known failure among them stands (the consumer keeps failure names, not a per-name status map), and the head is red while any failure remains. A consumer that reconciles a verdict from GitHub's rollup compares the rollup's attempt set the same way, but GitHub's read is complete: its failing check runs and failing commit statuses replace the stored ones wholesale. Statuses have no check run and the listener never sees them, so a consumer keeps them apart from check-run failures: a check run that shares a status's name cannot retire it — only GitHub does (likewise a deleted check's failure). A newer rollup set merges into the fence and takes the identity (no listener generation); the same set applies GitHub's verdict and keeps the listener identity for duplicate detection; an older, mixed, or empty-over-fenced set is ignored. A terminal read (green or red) then holds the tie at that set: a live settlement at the same set is accepted only if its effective outcome — the check-run failures it reports plus the stored ones it omits and the stored commit-status failures — agrees with the reconciled verdict, refreshing the listener identity without releasing GitHub's authority; a disagreeing one is stale whatever its generation until the set advances; a pending or cancelled-only read uncertifies a green head, leaves a red one untouched, and holds nothing — it releases any authority held at that set — so the terminal live settlement that follows applies at once, subject to the ordinary generation and duplicate rules (a replay or a lower generation still does not apply). Pending is therefore not a commutative join: a pending read after a live green uncertifies it until the next terminal view. Two remainders. An in-place conclusion change on an existing run id: GitHub's view stands and the listener's is recovered by the next successful, non-skipped read at that set — the dropped delivery is not replayed. A check whose highest run is deleted on GitHub: the fence keeps that id, so a rollup reporting a lower run under the same name is older until a newer run appears. A consumer that orders head changes by the PR's `updated_at` (GitHub's second resolution) accepts a read of a different head at an equal clock — a stale read returning the previous head within the same second as its replacement rewinds that consumer until its next accurate, non-skipped read. A head publishes only when at least one check has a positive run id; legacy checks without one remain in the status groups and failing names but not in `check_runs`. A legacy in-progress check whose completion is never observed holds the head unsettled until it reruns; rerun the affected check to release it. `workflow.<file>.<action>` carries only runs without an associated
  pull request.
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
  applies to a comment or ask anchored on the issue's own artifact). A notifying ask, comment-thread,
  or message-reply event additionally publishes an author route straight to each involved session's
  `notifications.agent.<session_id>`, regardless of the issue's route: the ask's author; a comment
  reply's thread-root author and its direct parent author when different; a message reply's direct
  parent author; and, for a bare resolve or reopen, the resolved comment's own root author. An
  author route is skipped for a human author and for a session replying to its own thread.
- Dispatch document events use `notifications.dispatch.document.<PROJECT>.<slug>.<type>` and
  are retained in JetStream. Consumers subscribe or tail that document topic directly; a document
  has no delivery route, so the same author routes above are the only way an involved session
  learns about a human reply, resolution, reopening, or edit on its ask or comment thread.
- Direct agent topics use `notifications.agent.<session_id>`.
- Role topics use `notifications.role.<role>` and are normally published to,
  rather than subscribed to by role holders.
- A failed control delivery publishes
  `notifications.envoy.exceptions.<original-topic>`.
