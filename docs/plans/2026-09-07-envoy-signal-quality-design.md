# Envoy signal quality: what agents receive, what they can trust, and what they can find

**Status:** draft, ready for review. Written while the owner was offline; every decision made in his
absence is listed in §9 as an assumption to confirm or overturn. Nothing here is implemented.

## 1. Provenance

The owner asked for the next round of Envoy improvements, named one concrete defect (GitHub events
arrive with `summary` as a JSON string instead of structured TOON), and asked that the other agents on
the machine contribute. 21 live top-level sessions were asked once for concrete friction; 11 replied
within 20 minutes. Their reports are paraphrased in §2 with identifiers removed; the raw replies are in
the coordinating session's transcript, not in this public repository.

Every item below carries how many sessions raised it independently. Items raised by one session are
included only when the coordinator reproduced them in code.

## 2. Evidence

### 2.1 Rendering (what the agent reads)

| # | Finding | Sessions | Verified in code |
|---|---|---|---|
| R1 | GitHub events render `summary` as a JSON string that duplicates `message`. | 6 + owner | `normalize.go` `githubSummary` returns `summaryJSON(data)`; `githubPayload` returns the same fields untruncated. All three renderers emit both. Measured on a real issue-comment delivery: 1589 chars with both, 825 with `message` only (−48%). |
| R2 | For agent-to-agent messages `summary` *is* the body: the name is wrong, multi-paragraph text arrives as one escaped line, and long messages are read twice. | 3 | `/v1/messages/send` sets `PayloadSummary = body.Message`, `Payload` unset. TOON encodes a string field as one quoted line. |
| R3 | Envelope schema changes make older renderers dump the raw NATS bytes. | 1 + coordinator | `envoy.ts` `catch { content = encode({ envoy: { topic, message: raw } }) }`. Triggered live when `source: human` shipped: sessions on the previous plugin render the whole envelope JSON as `message`. |
| R4 | Sender and target identity are prose, not fields. `topic: notifications.agent.<id>` reads like an address and was mistaken for the sender; one session sent three replies to a stale id pasted in a message body while `reply_to` held the right one. | 4 | Renderer emits `topic`, `from`, `reply_with`. Nothing labels the topic as the reader's own inbox. No check that a session id in the body differs from the sender. |
| R5 | `issued_at` is never rendered; agents reconstruct send times by hand and cannot tell a 40-minute-old state claim from a fresh one. | 3 | Field exists in the envelope; no renderer prints it. |
| R6 | Deliveries look like a user turn. Two sessions could not distinguish a peer message from the owner typing except by the `envoy:` prefix. | 2 | pi-envoy uses `pi.sendMessage({customType: "envoy-message"})`; how OMP presents custom messages to the model needs checking (§9). |

### 2.2 GitHub event content (what the event carries)

| # | Finding | Sessions | Verified in code |
|---|---|---|---|
| G1 | No merge signal. Sessions learned of merges from refused pushes, 404s, or the owner typing it. Several ran `gh` pollers for "is it merged yet". | 4 | `pull_request` events publish to `pr.<n>` with `action: closed` and no `merged`/`merge_commit_sha`/`merged_by`. |
| G2 | No CI settlement signal. Per-check `pr.<n>.check` is too noisy on a 40-check PR; `ci_summary` snapshots replay for superseded heads (25 stale snapshots over 45 min for one PR); `cancelled` is bucketed as `failed`. Five sessions wrote their own `gh pr checks` pollers, one of which died silently. | 5 | `cistore/render.go` buckets: failed/running/passed/queued/skipped. `ci_summary` is keyed by (pr, sha) and has no notion of the PR's current head. |
| G3 | `push` carries `{kind, ref, repo}` only: no `after` sha, pusher, head commit subject, or merged PR number. Every push cost a `gh api` round-trip. `pr.<n>` `synchronize` carries no `head_sha`. | 1 (heavy user) | `githubPayload` has no `push` case with those fields; `pull_request` case omits `head.sha`. |
| G4 | `edited` comment events deliver the full body on every edit (a sticky bot comment produced 5–6 edits per job, one body was ~9 KB of plan JSON). Bodies are truncated at 500 in `summary` but untruncated in `payload`. | 1 | `githubPayload` `issue_comment` case: untruncated `body`, no `action` filter. |
| G5 | `pull_request_review` events arrive with `body: ""` because bots put substance in inline comments; each cost 2–3 API calls to read. | 2 | Webhook payload for `submitted` does not include the review's comments. Enrichment needs an API call (§9). |
| G6 | `.pr.<n>.check` was not discoverable: one session polled all day believing no CI topic existed. | 1 | The tool description lists `pr.<n>` and `issue.<n>.comment` as examples only. |

### 2.3 Delivery semantics

| # | Finding | Sessions | Verified in code |
|---|---|---|---|
| D1 | Each delivery arrives as a steer; when it lands during a blocking `hub wait` the wait returns "Skipped due to pending system advisory" and the agent re-issues it. One session estimated ~200 wasted tool calls in a night. Two sessions want mid-turn delivery kept for blockers. | 3 + coordinator | `envoy.ts` `deliverAs: "steer", triggerTurn: true`. The skip is OMP behaviour on a queued steer. |
| D2 | Silent non-delivery. A session subscribed to three PR topics for a repository the GitHub App is not installed on; `envoy_subscribe` said "Subscribed", nothing ever arrived, and the merge was learned from the owner. | 1 | Subscribe validates topic shape only; the listener has no record of which repositories have ever produced an event. |
| D3 | `envoy_publish` to a role topic returns "Published" whether or not a holder exists; a delivery-failed exception is emitted asynchronously where the sender never sees it. No `envoy_role_get`; roles are not in `envoy_sessions`. | 3 | Role delivery emits `notifications.envoy.exceptions.<topic>` on no holder; the publish HTTP response is 200. |
| D4 | Unsubscribing a wildcard then re-subscribing narrower topics still delivered the wildcard's backlog for ~45 min. | 1 | Hypothesis, not reproduced: the registry-persisted interest may outlive the live one (`envoy_list` distinguishes `live`/`registry`/`both`). Needs a reproduction before design (§9). |
| D5 | One transient `nats: invalid jetstream publish response` 500 on `envoy_send` with no retry. | 1 | No retry in `transport.ts` or the listener. |
| D6 | Send returns broker acceptance, not delivery; a sender correcting an earlier message could not tell whether the recipient had acted on the wrong one. | 1 | By design today; noted, not designed (§10). |

### 2.4 Trust and correlation

| # | Finding | Sessions | Verified in code |
|---|---|---|---|
| T1 | Relayed human authority is indistinguishable from an agent's own claim. One message ("overnight approver on duty, owner's directive, reply to a *different* session id") had the exact shape of the phishing the team's own tasks train against and cost an incident cycle to verify; a near-identical genuine one arrived days later. Sessions must choose between ignoring the owner and obeying an impersonation. | 4 | Envelope has `source` and `source_session` only. Nothing carries "this quotes a user message" or where to verify it. |
| T2 | No correlation: broadcasts hit sessions as several restated messages; a later notice "supersedes one detail" of an earlier one had to be hand-merged; senders chased each recipient for "did you see this". | 3 | `event_id` exists but there is no `in_reply_to` or `supersedes`, and `envoy_send` cannot reference a prior message. |
| T3 | Deadlines, urgency, and "reply expected" are prose ("URGENT", "within ~15 min", "no ack"). | 6 | No field. Dispatch threads already have `urgency: low|med|high|blocking`; the envelope schema already has an unused `expires_at`. |
| T4 | Bulk data has no by-reference path: a sandboxed subagent pasted a 22 KB findings JSON into a message; a coordinator moved ~20 files by absolute path in prose. | 2 | `payload_ref` exists in the schema and is unused outside the MCP bridge. |
| T5 | Subagent completion arrives twice (harness task-result and an Envoy summary) with no shared id. | 2 | Harness-side; Envoy can carry a correlation id but cannot suppress the harness result (§10). |

## 3. Principles

1. **The envelope is the contract; renderers only format it.** Every fix lands in the producer (Go
   normalizer, listener API) or the schema, never as renderer heuristics that reverse-engineer prose.
2. **Additive fields only.** Existing consumers keep working. Renderers must degrade by dropping
   unknown fields and values, never by dumping the raw envelope (R3).
3. **No silent success.** Subscribe to a repository Envoy has never heard from, publish to a role
   nobody holds, send that the broker could not store: each returns a visible answer (D2, D3, D5).
4. **Envoy asserts what it can verify and points at what it cannot.** It can attest which session
   sent a message and when. It cannot attest what a human said; it can carry a reference the recipient
   verifies itself (T1).
5. **Webhook payload first.** GitHub enrichment uses fields already in the webhook body. Enrichment
   that needs an API call is a separate, owner-approved step because it needs a credential on the
   listener (G5).

## 4. Approaches considered

**A. Renderer-only.** Fix `githubSummary`, move agent bodies into `payload`, make renderers tolerant.
Fixes R1–R5 in one PR, no contract change. Leaves every other row of §2 in place: agents keep writing
`gh` pollers and keep guessing at provenance.

**B. Envelope additions plus webhook-only GitHub content (recommended).** A on top of: additive
envelope fields for sender identity, correlation, urgency, and relay provenance; merge and
CI-settlement events derived from data the webhooks already carry; visible failures for unwired
repositories and unheld roles. Four independent PRs. No new credentials, no new storage.

**C. B plus a platform layer.** By-reference payloads in a NATS object store, API-enriched review
events, delivery receipts, read-state on broadcasts. Needs a GitHub credential on the listener and an
attachment store with retention. The by-reference payload piece is designed below as Phase 4 because
two sessions hit it hard and `payload_ref` already exists; the rest is deferred with reasons (§10).

Recommendation: **B now, Phase 4 of C when the owner agrees to the storage question in §9.**

## 5. Design

Phases are independent PRs in dependency order. Each names its files.

### Phase 1: render the envelope honestly

**Producer changes** (`packages/envoy/internal/contracts/normalize.go`, `packages/envoy/cmd/listener/api.go`)

- `githubSummary` returns one line of prose per event, never JSON:
  `comment on <owner>/<repo>#<n> by <author>: <first 120 chars of body>`,
  `review (<state>) on <owner>/<repo>#<n> by <author>`,
  `pr <action>: <owner>/<repo>#<n> <title>`, `issue <action>: …`,
  `workflow <name> <branch> run <run_id> <conclusion> (<started>→<updated>)`,
  `push to <ref>: <head subject> (<sha7>) by <pusher>`.
  `payload` keeps the structured object.
- `/v1/messages/send` and `/publish`: `payload_summary` becomes the first non-empty line of the
  message (≤ 160 chars); `payload` carries the full message text when it differs from the summary.
  Agent bodies therefore render as a TOON multi-line block, preserved, once. The send path applies no
  transformation today, so the fused-whitespace and truncated-sha paraphrases one session received
  were authored by the sender; verbatim `message` delivery makes that visible (R2).
- `issue_comment`/`pull_request_review_comment` with `action: edited`: `payload.body` is omitted
  and `payload.body_changed: true` is set; the URL remains. Bodies over 2 KB in any event are
  truncated at 2 KB with `body_truncated: true` and the URL (G4).

**Renderer changes** (`packages/envoy-client/src/delivery.ts` becomes the single TS renderer;
`packages/pi-envoy/extensions/envoy.ts` and `packages/claude-envoy-bridge/src/envoy-monitor.ts`
call it; `packages/envoy/internal/session/session.go` `Deliverer.Text` mirrors it)

Rendered shape, TOON:

```
envoy:
  to: you (01a0…)                  # only when topic is the reader's own agent subject
  from: 01a0…  |  human  |  github
  at: 2026-09-07T04:41:12Z         # issued_at
  reply_with: envoy_send(session_id="01a0…", message="...")   # agent senders only
  reply_role: envoy_publish(topic="notifications.role.<role>", message="...")  # Phase 3, when the sender holds a role
  urgency: high                    # Phase 3, only when set
  expects_reply: required          # Phase 3, only when set
  summary: <one line>
  message: <structured payload, or the multi-line body>
```

- `summary` is omitted when `message` is a string equal to it.
- Parse failure or an unknown `source` value renders the fields the renderer does recognise plus
  `unrecognised: <field list>`; the raw envelope is never emitted (R3). The zod `source` enum becomes
  `z.string()` with the known values documented; Go keeps the validation on ingress.
- If the message body contains a session id (regex on the `01a0…` shape) that is not
  `source_session`, the renderer appends `note: body names session <id>; the sender is <source_session>`
  (R4, the misrouting incident).
- The tool description for `envoy_subscribe` lists every GitHub topic shape: `pr.<n>`, `pr.<n>.check`,
  `pr.<n>.comment`, `pr.<n>.review`, `pr.<n>.mention`, `issue.<n>`, `issue.<n>.comment`,
  `push.branch.<name>`, `workflow.<file>.<action>`, and the Phase 2 additions. Four sessions polled
  `gh` for events that topics already carried because the examples did not name them (G6).

**Tests:** table-driven summary tests per event in `normalize_test.go`; renderer tests in
`envoy-client/src/__tests__/delivery.test.ts` for the shape above, for unknown `source`, for the
foreign-session-id note, and for the equal-summary omission; Go `session_test.go` mirrors the TS cases.

### Phase 2: GitHub events an agent can act on

All fields come from the webhook body. (`normalize.go`, `cistore/`)

- **`push`**: add `after` (sha), `before`, `pusher`, `head_subject` (first line of `head_commit.message`),
  `commit_count`, `compare_url` (G3).
- **`pull_request`**: add `head_sha`, `base_ref`, `merged`, `merge_commit_sha`, `merged_by`.
  When `action == closed && merged == true`, publish an additional envelope to
  `notifications.github.<owner>.<repo>.pr.<n>.merged` with summary
  `merged <owner>/<repo>#<n> by <login> → <merge_sha7>`; `action == closed && !merged` publishes to
  `pr.<n>.closed` (G1). Base `pr.<n>` still receives the `closed` event unchanged.
- **CI settlement** (`cistore`): the store records each PR's current head from `pull_request`
  `opened`/`synchronize`/`reopened`. `ci_summary` gains `is_head: bool` and a `cancelled` bucket.
  A new topic `pr.<n>.checks.settled` fires once per head sha when no check is queued or running
  and the sha is the current head, carrying `{sha, passed, failed, cancelled, skipped, failing_checks:
  [name, url]}`. Summaries for a non-head sha are not published (G2). Per-check `pr.<n>.check` is
  unchanged for sessions that want it.
- **Comment `edited`** handled in Phase 1.
- **`workflow_run`**: add `run_id`, `run_attempt`, `head_sha`, `pr_numbers` (from
  `workflow_run.pull_requests[]`), `run_started_at`, `updated_at`. Publish additively to
  `workflow.<file>.<action>.branch.<name>` beside the existing `workflow.<file>.<action>`, the same
  most-specific-copy pattern `push` uses, so a watcher of `main` stops receiving ~40 cancelled runs a
  day from feature branches without losing the topic it has.
- **CI attempts**: `cistore` keys a check by `(name, run_attempt)` and reports only the latest attempt
  per name; after `gh run rerun --failed` one session was shown four failures from attempt 2 while
  attempt 3 was green and running.

**Tests:** `cistore` state-machine tests: head moves → old sha's pending summary suppressed; settled
fires once; cancelled classified. Normalizer tests for each new field and both new topics.

### Phase 3: identity, correlation, urgency, relay provenance

Additive envelope fields (`packages/contracts/schemas/envelope.schema.json`, regenerate Go with
`bun packages/contracts/scripts/gen-go.ts`; listener stamps, renderers print):

| Field | Set by | Meaning |
|---|---|---|
| `sender` | listener, from the registry row of `source_session` at publish time | `{session_id, machine, cwd, title, roles: []}`. Stamped, not self-asserted (T1, R4). |
| `in_reply_to` | `envoy_send`/`envoy_publish` argument | `event_id` of the message being answered. Renderer prints `re: <event_id>` (T2). |
| `supersedes` | argument | `event_id` this message replaces. Renderer prints `supersedes: <event_id>` (T2). |
| `urgency` | argument, `low|med|high|blocking` (dispatch's vocabulary) | Rendered in the header. Delivery mode is unchanged in this phase (see OMP dependency) (T3). |
| `expects_reply` | argument, `none|optional|required` | Rendered in the header so an FYI does not cost the recipient an ack turn (T3). |
| `expires_at` | argument (already in the schema, unused) | Deadline in ms; rendered as `by: <ISO time>` (T3). |
| `on_behalf_of` | argument, only accepted when the sending session has a user turn | `{session_id, message_id, issued_at}` pointing at a user message in the sender's own transcript. The renderer prints `quotes user message <id> in session <sid>; verify with read history://<sid>`. Envoy never asserts the content is authentic; it gives the recipient the exact place to check (T1, principle 4). |

`envoy_send` returns `{event_id, recipient: <full session id>}` so a sender can be answered and
superseded and sees exactly which session it reached (a prefix or unknown id already returns 404 since
sjawhar/legion#794). `envoy_sessions` includes `roles` and `last_seen` and accepts `dir` and `title`
substring filters; today one caller pages 25 KB of JSON to find a session by directory. When the
sender holds a role, renderers add `reply_role` beside `reply_with` so a successor session that lost
the sender's id after compaction can still reach the party.

**Inbox** (`pi-envoy`, `claude-envoy-bridge`): the extension keeps the last 50 delivered envelopes per
session and exposes `envoy_inbox` → `[{event_id, at, from, summary}]`. Three deliveries in one turn
leave only the last in front of the agent; the others scroll into history with nothing to drain.

**Roles** (`cmd/listener/api.go`, `envoy-client`): `POST /v1/messages/publish` to
`notifications.role.<role>` returns 404 `{"error":"no holder for role <role>"}` when the lane has no
live holder, and `{holder: <session_id>}` on success. New tool operation `envoy_role_get <role>` →
`{holder, last_seen}` or 404 (D3).

**Unwired repositories** (`cmd/listener`): the listener keeps a KV of `github_repos_seen:<owner>/<repo>
→ last event ms`, written on every GitHub ingress. `POST /v1/interests/subscribe` for a topic under
`notifications.github.<owner>.<repo>.` answers 200 with `warnings: ["no GitHub event ever received
for <owner>/<repo>; is the App installed there?"]` when the key is absent; the tool result prints the
warning. Not an error: a brand-new repository legitimately has no history (D2).

**Send retry and error bodies** (`envoy-client/src/transport.ts`, `cmd/listener/api.go`): one retry
after 250 ms on 5xx (including the listener's `503: service starting` during a rollout) or connection
reset, then the error. Every listener 400 carries `{"error": …, "expected": <field list>}`; a session
that hand-retried a failed send today got an empty 400 (D5).

**Tests:** schema round-trip (TS ↔ generated Go, byte-identical as in the dispatch question schema);
listener handler tests for the role 404, the subscribe warning, the `sender` stamp; renderer tests for
each new header line; `on_behalf_of` rejected when the session has no user turn.

### Phase 4: payloads by reference

`envoy_send` and `envoy_publish` accept `attachments: [{name, content | path}]` (≤ 4 MB each). The
listener stores bytes in a NATS JetStream Object Store bucket `envoy-attachments` (TTL 7 days) and
sets `payload_ref` on the envelope to `envoy://att/<sha256>`; the rendered message lists
`attachments: [{name, size, sha256, ref}]`. A new tool operation `envoy_fetch <ref>` (and
`read envoy://att/<sha256>` in OMP via the extension's resource handler) returns the bytes. A
sandboxed subagent that cannot write files can still hand a report to its coordinator (T4). Retention
and bucket size are the storage question in §9.

### OMP dependency (separate repository)

`hub wait` returning "Skipped due to pending system advisory" on every queued steer is the single most
expensive behaviour reported (D1). The fix belongs in Oh My Pi: a queued steer that arrives during
`hub wait` should complete the wait with the steer as its wake reason. Until then Envoy keeps
`deliverAs: "steer"`, because two sessions reported blockers that only reached them in time because of
it. `urgency` (Phase 3) is rendered only; wiring `urgency < high → followUp` is a one-line change held
back until the OMP fix lands, so the two are not confounded.

R6 (deliveries look like a user turn) is also an OMP presentation question: check how
`sendMessage({customType})` content reaches the model and, if it is a user-role block, ask OMP for a
tagged system-notice channel. Recorded in §9.

## 6. Error handling

- Normalizer: a missing webhook field yields an empty string in `payload`, never a dropped event
  (today's behaviour); `merged`/`checks.settled` publish only when their defining fields are present.
- Renderer: never throws into the harness; unknown values render as `unrecognised`.
- Listener KV/Object Store unavailable: subscribe proceeds without the warning and logs at WARN;
  attachment upload fails the send with 503 and the message is not published half-formed.
- `on_behalf_of` without a user turn: 400 from the tool before any network call.
- A tool contract change strands running sessions: MCP tool schemas load at session start, so after
  sjawhar/legion#777 one session could satisfy neither the old schema its harness held nor the new one
  the server enforced, and gave up on dispatch. Any future change to a tool's argument shape ships
  with a broadcast to live sessions naming the change and the fix (restart), and the server keeps
  accepting the previous shape for seven days. See question 4.

## 7. Testing

Unit tests per phase as listed. Live acceptance per phase, run from a devbox session before each
listener rollout:

1. Post a comment on a test issue; the delivery shows `summary` as prose and `message` once.
2. `envoy send` a three-paragraph message to a session; it arrives as a preserved block, once, with
   `to: you`, `from: human`, `at:`.
3. Merge a test PR; `pr.<n>.merged` arrives with the squash sha; `checks.settled` fired exactly once
   for the final head and never for the superseded one.
4. Subscribe to a repository the App does not cover; the warning is printed.
5. Publish to an unheld role; the tool errors.
6. Load a session on the previous plugin and send it a Phase 3 envelope; it renders fields, not raw bytes.

## 8. Rollout

Each phase: PR → listener image → `pulumi up --stack prod` for the three machines → `pi-legion-envoy`
and `opencode-legion-envoy` release → dotfiles pin bump. Renderer changes reach a session only when it
restarts; the design tolerates mixed versions (principle 2), which is exactly what R3 lacked. Each
listener rollout is followed by one broadcast to live sessions (`envoy_publish` to every agent subject
in the registry) naming the new fields, so long-lived sessions learn what changed without restarting.

## 9. Decisions made in the owner's absence, and open questions

Assumptions (overturn any of them and the phase changes, not the whole design):

1. **Delivery stays `steer`**; the wasted-wait cost is fixed in OMP, not by queuing deliveries to end
   of turn. Reasoning: three sessions asked for exactly the OMP behaviour ("wake with the message as
   the reason"); two reported blockers that mid-turn delivery saved.
2. **`urgency` reuses dispatch's `low|med|high|blocking`** rather than a new vocabulary.
3. **Relay provenance is a pointer, not a certificate.** `on_behalf_of` carries `history://` coordinates
   the recipient can read on the same machine; Envoy does not claim to know what the human said.
   Alternative considered and rejected: a listener-signed "human said this" flag, which would require
   the listener to read transcripts.
4. **Phase 2 is webhook-only.** Review inline comments (G5) need `pulls/<n>/reviews/<id>/comments`
   from the listener and therefore an installation token on Fargate. Not designed; see question 2.
5. **Per-check `pr.<n>.check` stays**; `checks.settled` is added beside it rather than replacing it.

Questions only the owner can answer:

1. **Storage for Phase 4:** a JetStream Object Store bucket on the existing NATS cluster, 7-day TTL,
   4 MB per attachment. Acceptable, or should attachments stay out of Envoy?
2. **A GitHub credential on the listener** for API enrichment (review comments, `is_head` for
   repositories without `synchronize` history). Recommendation: no; the webhook-only design covers
   the reported cases except G5.
3. **OMP-side changes:** `hub wait` wake reason (D1) and the presentation channel for custom messages
   (R6). These are Oh My Pi fork changes; should they be filed and done as part of this work?
4. **Tool-shape compatibility window.** Seven days of accepting the previous argument shape on a
   changed tool, or none (restart is the fix, and it is announced)? The config-key precedent from
   sjawhar/legion#777 was "reject loudly, no legacy names"; a running session that cannot restart
   mid-task is a different case.

To verify before Phase 2 (not blocking Phase 1):

- The D4 backlog replay, now reported by two sessions (`envoy_list` showed the topic gone; `.ci`,
  `.comment`, and `.review` deliveries continued for 30+ minutes). Reproduce by unsubscribing a wildcard
  on a busy PR and checking for a lingering `registry`-only interest and for queued JetStream messages
  whose interest was evaluated at enqueue time.
- Slack channel subscriptions delivering nothing while direct reads show bot posts. The Slack ingress
  has no bot or subtype filter in code, so this is routing or the Slack app's event subscription, not
  a normalizer rule; reproduce with one bot post to a subscribed channel.

## 10. Out of scope, with reasons

- **Delivery/read receipts (D6) and broadcast answered-state (T2 partial):** requires the recipient
  harness to report consumption; Envoy sees broker acceptance only. `in_reply_to` gives a controller
  enough to collect answers by hand.
- **Harness double-delivery of subagent results (T5):** the task-result path is OMP's; Envoy carrying
  a correlation id does not stop the second copy.
- **Presence registry with `open_prs` / task fields:** repository-specific; `sender.title` and `roles`
  cover the roster use.
- **Evidence field (command + output) on claims:** prose already can; a field would not make it true.
- **Per-comment-id or per-author exclusion on `.comment`:** the `edited` diet in Phase 1 removes the
  reported flood; revisit if a second source appears.
- **Mergeable-state change events (MERGEABLE → CONFLICTING):** GitHub computes mergeability lazily
  and sends no webhook for it; only an API poll sees it. Belongs with question 2 if that credential is
  ever granted.
- **Structured message kinds (`kind`, `verdict`, `asks[]`) on `envoy_send`:** one session asked for
  it after a 25-message negotiation; `in_reply_to` and `expects_reply` remove the restating that made
  the messages long. Revisit if a second controller pattern needs it.
- **Auto-subscribing a session to its own PR on `gh pr create`:** a harness or skill concern
  (`landing-a-pr`), not Envoy's; noted for the dotfiles side.
- **Rendering position of inbound messages (turn start vs mid-tool-output):** OMP presentation, with R6.
- **Suppressing a session's own GitHub actions on issue topics:** every session comments through the
  same App identity, so the actor login cannot be mapped to a session; dispatch markers are the one
  case that carries session provenance and are already suppressed. Commit trailers could carry it for
  pushes; comments cannot without a marker.
- **`refs: [pr, issue]` on GitHub events from the commit → PR → linked-issue graph:** needs the API;
  `pr_numbers` on `workflow_run` covers the reported case from the webhook alone.
- **Dropping `reply_with`:** one session called it redundant with `from`; five called it the reason
  replying was zero-thought. It stays.
