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

All `/v1/*` endpoints answer 503 until NATS is connected and the interest and session caches are warm; they do not wait for the durable consumer.
Every `/v1` request with a body must set `Content-Type: application/json` (parameters such as
`charset=utf-8` are accepted). The listener returns a JSON 415 error otherwise. Webhook routes
retain their signature and content handling.
A listener with no bearer configured serves `/v1` only to a `Host` that names it: loopback, or a
name in `ENVOY_ALLOWED_HOSTS` (port stripped, case folded, one trailing dot dropped). Any other
`Host` gets a JSON 421, so a DNS-rebinding page cannot reach the bearer-free API. A listener that
requires a bearer checks no `Host`.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/sessions` | GET | Lists all sessions across all machines — joins `envoy_interests` (topics, dir) with `envoy_sessions` (port, self-subscribed delivery mode). A cache view, and a list cannot be made authoritative key by key, so a caller holding one session id asks `/v1/registry/{session_id}` instead |
| `/v1/interests/subscribe` | POST | Persist a session's subscribed topics; `self_subscribed: true` permits a portless session that consumes its own NATS subscription. A topic that reaches a private stream subject other than the session's own agent subject (another session's `notifications.agent.<other>`, a failed-delivery lane `notifications.envoy.exceptions.notifications.agent.<id>` including its own, a Legion daemon notice under `notifications.legion.>` such as the controller lane `notifications.legion.<project>.controller`, or a wildcard such as `notifications.agent.>`, `notifications.envoy.>` or `notifications.>`) is not stored and is named in `warnings`, so neither the fan-out nor `/v1/replay` hands a session another session's inbox, a failed delivery's payload or the controller's notices |
| `/v1/interests/unsubscribe` | POST | Remove persisted topics for a session |
| `/v1/interests/` | GET | List persisted interests |
| `/v1/interests/{session_id}` | GET/DELETE | Get or delete a session's persisted interests |
| `/v1/roles/set` | POST | Claim a role for a live session and register its role topic |
| `/v1/registry/{session_id}` | GET | The authoritative answer for one session — reads through the cache to the session bucket, so a session the cache has not refilled is still found, and returns the same body a `/v1/sessions` row carries. It refuses with 413 or 400 for an id NATS will not take as a key, `404 {"error":"no live session <id>"}` only for a key the bucket does not hold, and 503 with `Retry-After: 1` for a read that did not answer |
| `/v1/messages/send` | POST | Send a direct agent-to-agent message |
| `/v1/messages/publish` | POST | Publish an event. Required `message` becomes the human `payload_summary`; optional `payload` is distinct machine data in `payload`. A topic the stream captures is answered once JetStream stored it, bounded to five seconds. A role topic is forwarded to its live holder, and an ad-hoc topic (any `notifications.` topic the stream does not capture, such as `notifications.team.deploy-squad`) is published over core NATS and forwarded to each self-subscribed session registered on it; both are answered once each recipient confirmed the message, or with a 504 naming each that did not after four seconds (see "Role delivery and exceptions"). |
| `/healthz` | GET | Health check (always available, even during startup) |

## Subscription visibility

An OMP extension keeps its direct NATS subscriptions locally and persists them through
`/v1/interests/subscribe`. `envoy_list()` reports their union: each topic is marked
`live` (local only), `registry` (persisted only), or `both`. This exposes live delivery
state even while an interest registration or cleanup request is delayed or unavailable.

## Role delivery and exceptions

`notifications.role.<role>` is a live, exactly-one-holder lane. A role message sent through
`/v1/messages/publish` is delivered by the route itself: it resolves the current live role holder,
sends a receipt-backed request with the original role topic to `notifications.agent.<session_id>`,
and the extension's agent pump replies after accepting that envelope. Without a reply inside two
seconds the route resolves the holder again and forwards the same envelope (one `event_id`)
100 ms later, until a reply arrives or four seconds from the request's arrival have passed. It
answers `200` with the `holder` once the holder replied; `404` with `reason` `unclaimed` or
`holder_lapsed` when no live session holds the role; `403` with `reason: "capability"` when the
holder does not take the payload's delivery mode; and `504` with `reason`, `event_id`,
`dedupe_key`, `holder` and `attempts` once the four seconds passed. The reason is
`receipt_timeout` when the message was forwarded and flushed to the server and the registered,
live holder sent no receipt (`bus.ErrReceiptTimeout`, which the client returns from the receipt
wait alone — never from the flush, whose own timeout is the same `nats.ErrTimeout` value), and
`delivery_failed` when the message is not known to have reached the holder (no live holder to
forward to, a holder lookup that failed or ran out of time, or a publish or flush to the server
that failed or timed out). The route publishes no exception: its sender has the answer.

An ad-hoc topic sent through the route is published over core NATS, then forwarded the same way
to each session registered on it, on any machine, that holds its own NATS connection
(`self_subscribed`), other than the sender. It answers `200` once each confirmed, and otherwise
`504` with `reason: "not_confirmed"`, `delivered` and `undelivered` (each `{session_id, reason}`).
A session that stopped without unregistering stays registered for up to five minutes and is
named in `undelivered` in that time; it is not a failed delivery to the others. A session served
on a port is not confirmed. Every forward carries a `Nats-Msg-Id`, so the stream keeps one copy
per message and recipient, and listeners acknowledge that stored copy without delivering it.

The role lane, a core-NATS queue subscriber in each listener, carries a role topic a client
publishes straight onto NATS rather than through the route. It resolves the holder at delivery
time and forwards once, and its outcome is one of three exception reasons: `no_holder` when no
session claims the role, `delivery_failed` or `receipt_timeout` as above. A role claimant does not
subscribe directly to its role subject, and a role message is not retained for a future claimant.

When a control delivery has no holder, cannot be forwarded, or draws no receipt, Envoy emits an
envelope on `notifications.envoy.exceptions.<original-topic>`. The exception payload preserves
the original delivery fields:

| Field | Meaning |
| --- | --- |
| `original_topic` | Original envelope topic |
| `event_id` | Original envelope event ID |
| `reason` | `no_holder`, `delivery_failed`, or `receipt_timeout` (defined above) |
| `payload_summary`, `payload` | Original human summary and distinct machine payload |
| `dedupe_key` | Original envelope dedupe key |
| `source`, `source_session` | Original envelope source fields |

Role exceptions use core NATS with the role lane. Other control exceptions retain ordinary
JetStream transport. Exception topics do not produce further exception events.
