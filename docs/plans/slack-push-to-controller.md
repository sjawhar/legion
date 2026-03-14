# Slack Push to Legion Controller Session

**Date:** 2026-03-15
**Status:** Draft

## Problem

The controller currently polls Slack every 30-60 seconds for `#engineering` updates and `@sami`
mentions. Polling adds latency, wastes tokens, and burns controller cycles even when no relevant
messages exist.

We need a push architecture where Slack events are delivered to the controller session in near
real time, while keeping the deployment private on EC2 (no public inbound endpoint required).

## Constraints and Existing System

- Legion daemon runs on `127.0.0.1:13370` (`packages/daemon/src/daemon/server.ts`).
- Shared OpenCode serve runs on `127.0.0.1:13381` and hosts worker + controller sessions.
- Controller session is long-lived (`ses_30ea49044ffepa7XoYJBuzJ9CX`).
- Daemon currently exposes worker prompting via `POST /workers/:id/prompt` and calls
  `client.session.promptAsync(...)` through `OpenCodeAdapter.sendPrompt()`.
- Daemon session creation uses `POST /session` on OpenCode serve with `x-opencode-directory`
  (see `packages/daemon/src/daemon/serve-manager.ts`).
- EC2 is private (no public domain/IP for Slack webhooks), so Socket Mode is preferred.

## Delivery Modes Considered

### Option A (Recommended): Slack Socket Mode

Use Slack Socket Mode (WebSocket) with an internal bridge process.

Why this fits:
- No public inbound HTTP endpoint required.
- Works behind NAT/firewall on private EC2.
- Lower latency than polling.
- Bidirectional channel available for event acks and response workflows.

Tradeoffs:
- Stateful long-lived connection; must handle reconnects and lifecycle.
- Slightly harder horizontal scaling (not a blocker here; single EC2 host).
- Slack limits concurrent Socket Mode connections (up to 10 per app).

### Option B (Alternative): Events API over HTTP

Expose a public HTTPS endpoint and receive Slack webhook events.

Pros:
- Slack-recommended production model for broadly distributed apps.
- Stateless request handling.

Cons for this deployment:
- Requires public ingress, TLS, DNS, and signature verification endpoint.
- Conflicts with private-EC2/no-public-endpoint constraint.

## Recommended Architecture

### Components

1. **Slack Bridge Service** (new, local process)
   - Runs on same EC2 host as daemon/controller.
   - Maintains Socket Mode connection to Slack.
   - Filters and normalizes incoming events.
   - Forwards approved events to controller session via local API.

2. **Legion Daemon Control Ingress** (recommended addition)
   - New localhost endpoints on daemon (`:13370`) for controller session delivery:
     - `POST /controller/message` (maps to OpenCode serve `POST /session/{id}/message`)
     - `POST /controller/prompt` (maps to OpenCode serve `POST /session/{id}/prompt_async`)
   - Daemon resolves controller session ID from existing controller state and performs forwarding.

3. **OpenCode Serve Session API** (existing)
   - Session-targeted delivery into the controller session:
     - `POST /session/{sessionID}/message`
     - `POST /session/{sessionID}/prompt_async`

4. **Slack Web API Outbound Client** (bridge-owned)
   - Sends controller responses back to Slack (`chat.postMessage`, `chat.update`, thread replies).

### Why route through daemon instead of directly to serve

- Centralizes session identity and future auth/rate limit controls in one place.
- Keeps bridge blind to internals of worker/session topology.
- Matches existing architecture where daemon is the control plane for session interactions.

Direct-to-serve can exist as a fallback path if daemon endpointing is temporarily unavailable.

## Message Flows

### A) Slack -> Controller (notification ingestion)

1. Slack emits event to Socket Mode connection.
2. Bridge validates envelope freshness/idempotency and immediately acks envelope.
3. Bridge applies filters (channel/user/event type/mention target/thread policy).
4. Bridge normalizes payload into a compact internal event shape.
5. Bridge forwards to daemon:
   - informational context: `POST /controller/message`
   - actionable instruction: `POST /controller/prompt`
6. Daemon forwards to controller session on OpenCode serve (`/session/{id}/message` or
   `/session/{id}/prompt_async`).
7. Controller session processes event and decides whether to act.

### B) Controller -> Slack (responses)

1. Controller emits structured response intent (for example, JSON block in its output stream or
   explicit command message contract).
2. Bridge parses intent from controller session output stream or receives callback payload from
   daemon-side hook.
3. Bridge posts to Slack via Web API using bot token:
   - thread reply for existing thread context
   - channel message for new notifications
   - DM when event policy requires direct escalation
4. Bridge records Slack API result (`channel`, `ts`) for dedupe/retry correlation.

## Event Subscriptions and Filtering Policy

### Subscribe to

- `app_mention` (primary trigger in channels)
- `message.channels` (for `#engineering` scoped monitoring)
- `message.groups` (if private engineering channels are needed)
- `message.im` (DMs to bot)
- `message.mpim` (optional, if group DMs matter)
- `message_replied` (thread follow-ups)
- `app_rate_limited` (operational telemetry)

### Filter rules (default deny, explicit allow)

- **Workspace allowlist:** only `trajectorylabs` workspace/team ID.
- **Channel allowlist:** explicit list including `#engineering` channel ID(s).
- **User allowlist/priority:** `@sami` and optional trusted operators.
- **Bot/self suppression:** drop events from bot users including self.
- **Subtype exclusions:** ignore edits/deletes/join messages unless explicitly needed.
- **Thread binding:** only accept thread replies for threads created by bridge/controller, unless
  user is in allowlist.

## Security Model

### Secrets and tokens

- Store tokens in EC2 secret storage (SSM Parameter Store or Secrets Manager), never in repo:
  - `SLACK_BOT_TOKEN` (`xoxb-...`)
  - `SLACK_APP_TOKEN` (`xapp-...`) for Socket Mode
  - `SLACK_SIGNING_SECRET` only required for HTTP Events fallback
- Inject as environment variables at process start.
- Rotate tokens on a schedule and on incident.
- Restrict token scopes to minimum required (`app_mentions:read`, `channels:history`,
  `chat:write`, `im:history`, etc. only as needed).

### Validation and trust boundaries

- **Socket Mode path:** trust is anchored by authenticated WebSocket session; still validate
  `team_id`, `api_app_id`, and envelope shape.
- **HTTP fallback path:** validate Slack signature (`X-Slack-Signature`) + timestamp skew check
  (<= 5 min) using HMAC SHA-256 signing secret.
- Enforce localhost-only ingress from bridge to daemon (`127.0.0.1`), not public network.
- Add shared local auth token between bridge and daemon control endpoints to prevent accidental
  local injection from unrelated processes.
- Sanitize inbound text before prompt injection (strip control chars, cap length, prefix with
  provenance metadata).

### Prompt-injection mitigation

- Wrap Slack content in a strict envelope:
  - source metadata (`user`, `channel`, `ts`, `event_type`)
  - quoted message body (not merged into raw instructions)
  - explicit system prefix: "external untrusted user input"
- Apply max size and truncation policy with link back to full Slack context.

## Reliability, Rate Limiting, and Backpressure

### Slack-side constraints

- Events API requires quick ack behavior (3-second rule for HTTP; Socket Mode requires per-envelope
  ack).
- `app_rate_limited` events signal workspace-level event pressure.

### Bridge controls

- In-memory + durable queue (disk-backed) for transient daemon/controller outages.
- Idempotency key from `event_id` + `event_ts` to dedupe retries/reconnect duplicates.
- Token bucket rate limiting per channel/user to avoid flooding controller session.
- Circuit breaker when controller ingress fails repeatedly.
- Dead-letter queue for poisoned/unparseable events with operator alerting.

### Controller ingress controls

- Per-session prompt concurrency cap (serialize or small bounded parallelism).
- Coalescing policy for bursty events (e.g., merge thread reply bursts within 2-5 seconds).
- Distinguish `message` (context) vs `prompt_async` (action) to reduce unnecessary wakeups.

## Failure Modes and Handling

1. **Socket disconnect / refresh requested**
   - Auto-reconnect with exponential backoff + jitter.
   - Optionally keep secondary warm connection for seamless rollover.

2. **Daemon unavailable (`:13370` down)**
   - Queue events locally with TTL.
   - Health-check daemon and replay on recovery.

3. **OpenCode serve unavailable (`:13381` down)**
   - Daemon should return retriable failure; bridge retries through queue.

4. **Controller session missing/stale**
   - Daemon resolves current controller session from state file/runtime and rejects unknown session.
   - Bridge pauses actionable forwarding, emits ops alert.

5. **Slack API post failures (outbound)**
   - Retry with backoff for 429/5xx.
   - Respect `Retry-After` headers.
   - Dead-letter and alert after retry budget exhaustion.

6. **Duplicate deliveries**
   - Deduplicate by Slack `event_id` and normalized routing key.

7. **Injection/abuse attempt in Slack content**
   - Treat as untrusted data; never interpolate raw text as system instructions.

## Deployment on Existing EC2 Infrastructure

### Process placement

- Keep daemon (`13370`) and shared serve (`13381`) unchanged.
- Add `slack-bridge` process managed similarly to daemon (systemd/supervisor/tmux-managed service).
- Bridge binds no public port in Socket Mode.

### Local networking

- Bridge -> daemon over `127.0.0.1:13370` only.
- Daemon -> shared serve over `127.0.0.1:13381` only.
- Security group inbound can remain closed for Slack integration path.

### Observability

- Structured logs with correlation IDs (`event_id`, `session_id`, `slack_ts`).
- Metrics: events received, filtered, forwarded, dropped, retry count, queue depth, ack latency,
  post-to-Slack latency.
- Alerts: reconnect storm, queue growth, repeated daemon forward failures, token expiry.

## Suggested Contract Between Bridge and Daemon

### `POST /controller/message`

Body:

```json
{
  "source": "slack",
  "eventId": "Ev123",
  "teamId": "T123",
  "channelId": "C123",
  "threadTs": "1712345678.000100",
  "userId": "U123",
  "text": "raw message text",
  "metadata": {
    "type": "app_mention"
  }
}
```

Semantics: Add contextual message to controller session using OpenCode serve
`POST /session/{sessionID}/message`.

### `POST /controller/prompt`

Body:

```json
{
  "source": "slack",
  "eventId": "Ev123",
  "prompt": "You received a mention in #engineering. Evaluate and respond if needed. Context: ..."
}
```

Semantics: Trigger controller reasoning using OpenCode serve
`POST /session/{sessionID}/prompt_async`.

## Rollout Plan (Architecture-Level)

1. Build and run bridge in shadow mode (receive + filter + log, no forwarding).
2. Enable forwarding to `/controller/message` only (context ingestion, no prompting).
3. Enable controlled `prompt_async` for allowlisted events (`app_mention`, DM, `@sami` only).
4. Enable outbound controller-to-Slack responses in limited channels.
5. Decommission polling loops after equivalent coverage is verified.

## Decision

Adopt **Socket Mode bridge -> daemon controller ingress -> OpenCode session endpoints** as the
primary architecture. Keep Events API over HTTP as a documented fallback for future environments
with public ingress.
