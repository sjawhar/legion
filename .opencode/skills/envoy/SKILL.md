---
name: envoy
description: Use when subscribing sessions to Envoy topics, sending agent-to-agent messages, or reasoning about topic formats for Slack/GitHub/agent routing.
---

# Envoy

Envoy is Legion's event-routing subsystem. It delivers Slack, GitHub, and agent-to-agent events to OpenCode sessions.

## What the tools do

- `envoy_subscribe(topics)` — make the current session RECEIVE future events on those topics
- `envoy_unsubscribe(topics?)` — stop receiving some or all topics
- `envoy_list()` — show the session's current subscriptions
- `envoy_send(target_session, message)` — SEND a message directly to another session

## Topic formats

### Agent-to-agent

- Direct session route:
  - `notifications.agent.<session_id>`

Example:

- `notifications.agent.ses_2e6ca3034ffejVikSZ8mDwk0mR`

### Role-based routing

- Role topic:
  - `notifications.role.<role>`

Role topics route to exactly one session — whoever last called `envoy_role_set(role="<role>")`. Claiming a role transfers it from the previous holder using ordered last-writer-wins semantics.

Examples:

- `notifications.role.legion-controller` (routes to whoever holds the controller role)
- `notifications.role.opencode-dev` (routes to the active dev session)
- `notifications.role.legion-po` (routes to the product owner session)

### GitHub

GitHub topics are **resource-scoped** — every event includes the resource type and number (or, for push/workflow events, the ref or workflow filename).

**Topic structure:** `notifications.github.<owner>.<repo>.<resource_type>.<number>.<event_kind>`

- PR opened/closed/merged/ready:
  - `notifications.github.<owner>.<repo>.pr.<number>`
- Issue opened/closed/labeled:
  - `notifications.github.<owner>.<repo>.issue.<number>`
- Comment on a PR:
  - `notifications.github.<owner>.<repo>.pr.<number>.comment`
- Comment on an issue:
  - `notifications.github.<owner>.<repo>.issue.<number>.comment`
- PR review submitted:
  - `notifications.github.<owner>.<repo>.pr.<number>.review`
- CI/check summary (per-PR, per-commit, debounced):
  - `notifications.github.<owner>.<repo>.pr.<number>.ci`
  - **Not** a per-check-event firehose. `check_run` webhooks attached to a PR are aggregated ingest-side into per-commit state (JetStream KV bucket `envoy_ci_state`) and re-emitted as **one debounced JSON summary** once the commit's checks have been quiet for the debounce window (`ENVOY_CI_DEBOUNCE`, default `5s`). A subscriber sees the checks "build up" instead of every transition. A new summary is emitted only when the check set actually changes (new push → fresh tally; a re-run flips a check back to running and re-emits).
  - Payload: `PayloadSummary` is a compact JSON object (`Payload` unused): `{"kind":"ci_summary","repo":"<o>/<r>","number":"<n>","sha":"<sha>","failed":{"count":N,"checks":[...]},"running":{...},"passed":{...},"queued":{...},"skipped":{...}}`. Each status is `{count, checks}` with the full sorted name list (nothing collapsed); every status is always present (`{"count":0,"checks":[]}` when empty).
  - `check_suite` is ignored (per-app rollup, no per-check name). Checks not tied to a PR are dropped — there is no repo-wide CI topic. For non-PR visibility, use the `workflow.<filename>.<action>` family instead. Individual GitHub Actions *jobs* (`workflow_job` webhooks) are not routed — only whole workflow runs.
- Mention events (per-resource):
  - `notifications.github.<owner>.<repo>.pr.<number>.mention`
  - `notifications.github.<owner>.<repo>.issue.<number>.mention`
- Mention events (repo-wide — catches all mentions):
  - `notifications.github.<owner>.<repo>.mention`
- Push events (per branch/tag):
  - `notifications.github.<owner>.<repo>.push.branch.<branch>`
  - `notifications.github.<owner>.<repo>.push.tag.<tag>`
  Branch and tag names with dots are sanitized to underscores (`v1.0.0` → `v1_0_0`).
  Push events for refs other than `refs/heads/...` and `refs/tags/...` are not routed.
- Workflow run events (per workflow file):
  - `notifications.github.<owner>.<repo>.workflow.<filename>.<action>`
  Filename is the basename of `workflow_run.path` with dots sanitized (`ci.yml` → `ci_yml`).
  `action` is one of `requested`, `in_progress`, `completed`.

**Using wildcards to subscribe broadly:**
- All events in a repo: `notifications.github.<owner>.<repo>.>`
- All PR events in a repo: `notifications.github.<owner>.<repo>.pr.>`
- All events for a specific PR: `notifications.github.<owner>.<repo>.pr.<number>.>`
- All issue events in a repo: `notifications.github.<owner>.<repo>.issue.>`
- All events for a specific issue: `notifications.github.<owner>.<repo>.issue.<number>.>`
- All push events in a repo: `notifications.github.<owner>.<repo>.push.>`
- Pushes to main only: `notifications.github.<owner>.<repo>.push.branch.main`
- All branch pushes: `notifications.github.<owner>.<repo>.push.branch.>`
- All tag pushes: `notifications.github.<owner>.<repo>.push.tag.>`
- All workflow events: `notifications.github.<owner>.<repo>.workflow.>`
- All events for a specific workflow: `notifications.github.<owner>.<repo>.workflow.ci_yml.>`
- Every workflow completion: `notifications.github.<owner>.<repo>.workflow.*.completed`

Examples:

- `notifications.github.trajectory-labs-pbc.agent-c.pr.9880` (PR #9880 state changes)
- `notifications.github.trajectory-labs-pbc.agent-c.pr.9880.comment` (comments on PR #9880)
- `notifications.github.trajectory-labs-pbc.agent-c.issue.9909.>` (all events on issue #9909)
- `notifications.github.sjawhar.legion.pr.>` (all PR events across all PRs)
- `notifications.github.sjawhar.legion.mention` (all @mentions repo-wide)
- `notifications.github.sjawhar.legion.push.branch.main` (pushes to main)
- `notifications.github.sjawhar.legion.workflow.ci_yml.in_progress` (CI workflow starts)

### Slack

- Channel message events:
  - `notifications.slack.<team_id>.<channel_id>.message`
- App mention events:
  - `notifications.slack.<team_id>.<channel_id>.mention`
- Thread message events:
  - `notifications.slack.<team_id>.<channel_id>.thread.<normalized_ts>.message`
- Thread mention events:
  - `notifications.slack.<team_id>.<channel_id>.thread.<normalized_ts>.mention`

Thread timestamps are normalized: `1234567890.123456` → `1234567890_123456`
(dots replaced with underscores to make the thread ID a single NATS segment).

Examples:

- `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.message`
- `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.mention`
- `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.thread.1234567890_123456.message`
- `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.thread.1234567890_123456.mention`

### Ghost Wispr

- Session started events:
  - `notifications.ghostwispr.<session_id>.session.started`
- Session ended events:
  - `notifications.ghostwispr.<session_id>.session.ended`
- Summary ready events:
  - `notifications.ghostwispr.<session_id>.summary.ready`

**Parameters:**
- `<session_id>`: Ghost Wispr session timestamp string (e.g., `20260326041405`). Alphanumeric only, safe for NATS topic segments.
- Supported kinds: `session.started`, `session.ended`, `summary.ready`

Examples:

- `notifications.ghostwispr.20260326041405.session.ended` (session ended)
- `notifications.ghostwispr.20260326041629.summary.ready` (summary ready)

### WhatsApp

- Chat message events:
  - `notifications.whatsapp.<phone>.<jid>.message`
- Status/receipt events:
  - `notifications.whatsapp.<phone>.<jid>.status`

**Parameters:**
- `<phone>`: Connected WhatsApp account phone number in E.164 digits-only format (no `+` prefix). Example: `15551234567`. This identifies **which WhatsApp account** the events belong to — not the remote contact.
- `<jid>`: Remote chat's WhatsApp JID. Individual: `PHONE@s.whatsapp.net`. Group: `ID@g.us`.
- Supported kinds: `message`, `status`

> **⚠️ JID dot expansion:** JID dots (`.`) become additional NATS subject tokens. For example, `5551234567@s.whatsapp.net` splits into tokens `5551234567@s`, `whatsapp`, `net`. This means individual chat topics produce 7 tokens and group chat topics produce 6 tokens. **Always use `>` (multi-level wildcard), never `*` (single-token wildcard)**, when subscribing to a chat or phone number.

Examples:

- `notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message` (individual chat messages — note this expands to 7 NATS tokens)
- `notifications.whatsapp.15551234567.120363XXX@g.us.message` (group chat messages — 6 NATS tokens)

## When to use what

### To receive future Slack/GitHub/WhatsApp events

1. Decide the exact topic(s)
2. Call `envoy_subscribe([...])`
3. Optionally call `envoy_list()` to confirm

### To talk directly to another agent/session

1. Get the target session ID
2. Call `envoy_send(target_session, message)`

You do NOT need to subscribe in order to send or publish.

### To wait for CI, PR checks, or other async work

**Don't `sleep`-poll. Don't "check back in N minutes."** Subscribe to the event and continue with productive work — the system will wake the session when the event arrives.

1. Identify the relevant topic (e.g., `notifications.github.<owner>.<repo>.pr.<num>.>` for all PR events; `.pr.<num>.ci` for per-PR check/CI status updates; `.workflow.<filename>.completed` for a workflow run finishing)
2. Call `envoy_subscribe([...])`
3. Move on to other work, or end the response and let the watcher wake you
4. The next response is triggered by the event, with the payload available in your context

If you have nothing else to do, end the response. The user is not your alarm clock; do not loop with `sleep`.

### Tools

- `envoy_subscribe(topics)` — receive future events on those topics
- `envoy_unsubscribe(topics?)` — stop receiving some or all topics
- `envoy_list()` — show current subscriptions
- `envoy_send(target_session, message)` — send directly to a specific session (point-to-point)
- `envoy_publish(topic, message)` — broadcast to any topic (all matching subscribers receive it)
- `envoy_role_set(role)` — claim a named role for the current session (exactly-one-holder)

## Patterns

### Subscribe controller to a specific Slack channel mentions

```text
envoy_subscribe([
  "notifications.slack.T09FRELLTS8.C0A0DHVU8HE.mention"
])
```

### Subscribe to all events in a specific Slack thread

```text
envoy_subscribe([
  "notifications.slack.T09FRELLTS8.C0A0DHVU8HE.thread.1234567890_123456.>"
])
```

### Subscribe to only messages in a Slack thread (not mentions)

```text
envoy_subscribe([
  "notifications.slack.T09FRELLTS8.C0A0DHVU8HE.thread.1234567890_123456.message"
])
```

### Subscribe to all threads in a Slack channel

```text
envoy_subscribe([
  "notifications.slack.T09FRELLTS8.C0A0DHVU8HE.thread.>"
])
```

### Subscribe to all PR events for agent-c

```text
envoy_subscribe([
  "notifications.github.trajectory-labs-pbc.agent-c.pr.>"
])
```

### Subscribe controller to GitHub @mentions for agent-c

```text
envoy_subscribe([
  "notifications.github.trajectory-labs-pbc.agent-c.mention"
])
```

### Message another session directly

```text
envoy_send(
  target_session="ses_2e6ca3034ffejVikSZ8mDwk0mR",
  message="Please continue the smoke test"
)
```

### Subscribe to a specific WhatsApp contact (1:1 chat)

```text
envoy_subscribe([
  "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.>"
])
```

Use `>` (not `*`) to catch all event kinds despite JID dot expansion into multiple NATS tokens.

### Subscribe to a WhatsApp group

```text
envoy_subscribe([
  "notifications.whatsapp.15551234567.120363XXX@g.us.>"
])
```

### Subscribe to all WhatsApp events for an account

```text
envoy_subscribe([
  "notifications.whatsapp.15551234567.>"
])
```

Catches all conversations and event kinds for the specified phone number.

**When to use which:**
- **1:1 chat** — when monitoring a specific contact conversation (e.g., a bot handling customer queries)
- **Group chat** — when monitoring a specific group for commands or events
- **All chats for a phone** — when building a general WhatsApp event handler or dashboard for an account

## Important notes

- Sessions choose their own Slack/GitHub subscriptions
- Different sessions can subscribe to different channels/repos
- Agent-to-agent delivery uses exact session IDs
- If you are unsure what a session is currently subscribed to, call `envoy_list()` first
- For Slack, use the real `team_id` in topics (for example `T09FRELLTS8`), not a workspace slug like `trajectorylabs`
- GitHub mention routing is body-based because GitHub has no dedicated app mention webhook event

## Synthetic Smoke Test (WhatsApp — NATS Routing Only)

> **Important:** This procedure validates Envoy's NATS → listener → session delivery path using `envoy_publish`. It does **not** test real WhatsApp message ingestion. The generic MCP bridge (`packages/envoy/cmd/mcp/`) can bridge real WhatsApp events, but requires production configuration. See "Current Limitations" below.
>
> **Two sessions required:** `envoy_publish` sets `source_session` to the publishing session's ID. The listener skips delivering broadcasts back to the sender (`packages/envoy/cmd/listener/main.go`). You must subscribe in one session and publish from a different session.

### Step 1: Subscribe to a WhatsApp topic (Session A)

```text
envoy_subscribe([
  "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.>"
])
```

### Step 2: Verify subscription is active (Session A)

```text
envoy_list()
```

Confirm `notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.>` appears in the subscription list.

### Step 3: Publish a synthetic test envelope (Session B — a different session)

```text
envoy_publish(
  topic="notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message",
  message="Synthetic WhatsApp smoke test: hello from envoy_publish"
)
```

### Step 4: Verify delivery (Session A)

Session A should receive a notification containing the text "Synthetic WhatsApp smoke test: hello from envoy_publish". This confirms:
- The topic pattern matches the subscription
- NATS routes the message to the listener
- The listener delivers to the subscribed session (Session A ≠ the publishing session)

**If the notification does not arrive:** Check `envoy_list()` in Session A to confirm the subscription is still active. Verify the topic in `envoy_publish` matches the subscription pattern. Ensure you are publishing from a **different** session than the one subscribed.

### Reference: Real WhatsApp Envelope Shape

When the MCP bridge (`packages/envoy/internal/mcpbridge/envelope.go`) publishes a real WhatsApp event, the Envoy envelope has this structure:

```json
{
  "event_id": "<generated unique ID>",
  "source": "whatsapp",
  "source_event_id": "whatsapp://messages/15551234567/5551234567@s.whatsapp.net",
  "topic": "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message",
  "dedupe_key": "whatsapp.<event_id value>",
  "issued_at": 1712345678000,
  "payload_summary": "Hello from WhatsApp",
  "payload_ref": "whatsapp://messages/15551234567/5551234567@s.whatsapp.net",
  "trace_id": "<generated unique ID>"
}
```

**Field notes:**
- `source` is `"whatsapp"` — in contrast, `envoy_publish` sets `source: "agent"` for synthetic messages
- `issued_at` is in **milliseconds** (Unix epoch ms), not seconds
- `dedupe_key` is `source + "." + event_id` (e.g., `"whatsapp.cuid_abc123"`)
- `payload_summary` is the actual message text from the MCP resource read (truncated to 200 chars), or fallback `"whatsapp event from <uri>"` if no text content
- `payload_ref` and `source_event_id` are both the MCP resource notification URI
- `source_session` is **omitted** (empty) — the MCP bridge is not an OpenCode session, so no echo-skip occurs
- `expires_at` is **omitted** — the bridge does not set message expiry

## Current Limitations (WhatsApp)

- **No production WhatsApp event ingestion configured.** The repo contains a generic MCP→NATS bridge (`packages/envoy/cmd/mcp/` + `packages/envoy/internal/mcpbridge/`) that already supports WhatsApp topic patterns (tested in `packages/envoy/internal/integration/delivery_test.go`). However, it is not yet configured/deployed to connect to the `@sjawhar/whatsapp-mcp` server in production.
- **Synthetic testing only.** The smoke test above uses `envoy_publish` to inject test messages into NATS. It validates Envoy delivery mechanics (NATS → listener → session), not true WhatsApp end-to-end delivery.
- **Production wiring needed.** To receive real WhatsApp events, the MCP bridge needs to be configured with the `@sjawhar/whatsapp-mcp` server connection details (similar to how `packages/envoy/cmd/github/` and `packages/envoy/cmd/slack/` are configured for their respective platforms). The bridge would then subscribe to WhatsApp MCP resource notifications and publish Envoy envelopes to NATS automatically.
- **Subscription + routing + delivery path is ready.** The contracts layer (`whatsappSubject` helper), NATS topic format, generic listener routing, and MCP bridge infrastructure all work. Only the production configuration connecting the bridge to the WhatsApp MCP server is missing.
