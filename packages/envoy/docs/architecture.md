# Architecture

## Transport

```text
GitHub webhook  --\
Slack events    ---+--> NATS ── JetStream ──> listener ──> session wake / cold resume
Agent messages  --/       |
                            +── core NATS role lane ──> listener arbitration ──> current role holder
```

## Core pieces

- `packages/contracts`
  - envelope schema
  - env parsing
  - subject helpers
  - signing / verification helpers where envoy owns them
- `cmd/listener`
  - machine-scoped consumer
  - routing and dedupe
  - hot wake vs cold resume
- `cmd/github`
  - webhook verification
  - event normalization
  - NATS publish
- `cmd/slack`
  - request verification
  - challenge handling
  - event normalization
  - NATS publish

## Runtime shape

- one listener container per machine
- one GitHub receiver container on the public EC2 host
- one Slack receiver container on the public EC2 host
- NATS cluster across all machines via Tailscale mesh, with JetStream for durable notifications and core NATS for live role routing
- JetStream stream `ENVOY_NOTIFICATIONS` with 72h retention for non-role notification replay on listener restart; per-machine durable consumers carry a 7d inactive threshold so the server garbage-collects consumers of decommissioned listeners
- Role lanes and delivery-exception lanes for role topics use core NATS; they have no durable consumer, retention, or replay. Stream reconciliation excludes role subjects and purges any role message captured while that configuration is updated.
- JetStream KV bucket `envoy_interests` with 3 replicas for session subscriptions
- JetStream KV bucket `envoy_sessions` with 5m TTL for session port/host data

## Listener API

All `/v1/*` endpoints return 503 until NATS initialization completes.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/sessions` | GET | Lists all sessions across all machines — joins `envoy_interests` (topics, dir) with `envoy_sessions` (port, self-subscribed delivery mode) |
| `/v1/interests/subscribe` | POST | Persist a session's subscribed topics; `self_subscribed: true` permits a portless session that consumes its own NATS subscription |
| `/v1/interests/unsubscribe` | POST | Remove persisted topics for a session |
| `/v1/interests/` | GET | List persisted interests |
| `/v1/interests/{session_id}` | GET/DELETE | Get or delete a session's persisted interests |
| `/v1/roles/set` | POST | Claim a role for a live session and register its role topic |
| `/v1/registry/{session_id}` | GET | Get a session's registry entry (port, machine) |
| `/v1/messages/send` | POST | Send a direct agent-to-agent message |
| `/v1/messages/publish` | POST | Publish an event. Required `message` becomes the human `payload_summary`; optional `payload` is distinct machine data in `payload`. Role topics use core NATS, while JetStream acknowledgement for every other topic is bounded to five seconds. |
| `/healthz` | GET | Health check (always available, even during startup) |

## Subscription visibility

An OMP extension keeps its direct NATS subscriptions locally and persists them through
`/v1/interests/subscribe`. `envoy_list()` reports their union: each topic is marked
`live` (local only), `registry` (persisted only), or `both`. This exposes live delivery
state even while an interest registration or cleanup request is delayed or unavailable.

## Role delivery and exceptions

`notifications.role.<role>` is a live, exactly-one-holder lane. The listener's core-NATS
queue subscriber resolves the current live role holder at delivery time, then sends a
receipt-backed request with the original role topic to `notifications.agent.<session_id>`.
The extension's agent pump replies after accepting that envelope. If no receipt arrives within
two seconds, delivery fails and Envoy emits its `delivery_failed` exception. A role claimant
does not subscribe directly to its role subject, and a role message is not retained for a future
claimant.

When a control delivery has no live holder or fails, Envoy emits an envelope on
`notifications.envoy.exceptions.<original-topic>`. The exception payload preserves the
original delivery fields:

| Field | Meaning |
| --- | --- |
| `original_topic` | Original envelope topic |
| `event_id` | Original envelope event ID |
| `reason` | `no_holder` or `delivery_failed` |
| `payload_summary`, `payload` | Original human summary and distinct machine payload |
| `dedupe_key` | Original envelope dedupe key |
| `source`, `source_session` | Original envelope source fields |

Role exceptions use core NATS with the role lane. Other control exceptions retain ordinary
JetStream transport. Exception topics do not produce further exception events.
