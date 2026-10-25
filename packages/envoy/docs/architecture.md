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
| `/v1/sessions` | GET | Lists all sessions across all machines — joins `envoy_interests` (topics, dir) with `envoy_sessions` (port, self-subscribed delivery mode) |
| `/v1/interests/subscribe` | POST | Persist a session's subscribed topics; `self_subscribed: true` permits a portless session that consumes its own NATS subscription |
| `/v1/interests/unsubscribe` | POST | Remove persisted topics for a session |
| `/v1/interests/` | GET | List persisted interests |
| `/v1/interests/{session_id}` | GET/DELETE | Get or delete a session's persisted interests |
| `/v1/roles/set` | POST | Claim a role for a live session and register its role topic |
| `/v1/registry/{session_id}` | GET | Get a session's registry entry (port, machine) |
| `/v1/messages/send` | POST | Send a direct agent-to-agent message. The target is read from the session store itself, so a 404 means the store says that session's key is gone; a store read or a publish the bus did not answer is a 503 with `Retry-After: 1` (see “Send and publish answers” below). |
| `/v1/messages/publish` | POST | Publish an event. Required `message` becomes the human `payload_summary`; optional `payload` is distinct machine data in `payload`. Role topics and ad-hoc topics (any `notifications.` topic the stream does not capture, such as `notifications.team.deploy-squad`) use core NATS and reach the sessions subscribed at that moment; JetStream acknowledgement for every topic the stream captures is bounded to five seconds. A publish the bus did not serve, and a role-holder read that did not answer, are both the 503 below rather than a 500. |
| `/healthz` | GET | Health check (always available, even during startup) |

## Send and publish answers

`POST /v1/messages/send` resolves its target from the session store, not from this listener's
cache of it: the cache answers a hit, and a miss is read through to the `envoy_sessions` bucket
within one second (`liveSessionEntry`, `cmd/listener/api.go`), the same direct get the role paths
already make. That read and the publish share one 3.5 s budget taken from the caller's own request
context, so a store or a bus this listener cannot reach is answered inside the caller's attempt:

| Outcome of the attempt | Answer |
| --- | --- |
| The store says the key is gone (`nats.ErrKeyNotFound`, which also covers a session id no session can register under) | `404 {"error":"no live session <id>"}` |
| The store read did not answer (its second ran out, or the read failed any other way) | `503 {"error":"envoy: the message bus did not answer: <reason>"}` with `Retry-After: 1` |
| The target is live but does not advertise the frame's `delivery.mode`, or that mode cannot be read unambiguously | `403` naming the session, and the mode when it could be read |
| The publish is refused as too large (`bus.ErrTooLarge`) | `413` naming the size against the server's max payload |
| The publish is refused for its subject or key (`bus.ErrRefused`) | `400` naming the refusal, with no `Retry-After` |
| Any other publish failure | `503` with the same prefix and `Retry-After: 1`, carrying the bus's own reason |

`POST /v1/messages/publish` answers a publish failure, and a role-holder read that did not answer,
exactly the same way; its 413, its 400s and its role 404s (`unclaimed`, `holder_lapsed`) are
unchanged.

Each inbound send and publish is recorded in one line, INFO when accepted and WARN when refused:
`listener send` (`target_session`) and `listener publish` (`topic`), both carrying `event_id`,
`source`, `source_session`, `payload_summary`, `outcome` and `status`, and `holder` as well when a
role topic resolved one. `outcome` is one of `accepted`, `invalid_request`, `too_large`,
`bus_unavailable`, plus `no_live_session` and `capability_refused` for send and `role_unclaimed`
and `role_lapsed` for publish. Two refusals carry no such line, because neither reaches a handler:
the 401 the API auth wrapper writes, and a request the starting gate turns away while the routes
are still opening, which logs `request refused while starting` itself.

`envoy_messages_delivered_total` counts one delivery attempt per recipient, labelled
`delivery_status`:

| `delivery_status` | Attempt it counts |
| --- | --- |
| `delivered` | Pushed to the session, or forwarded to a role holder that receipted it |
| `skipped` | Nothing to push: the session consumes its own subscription, or the envelope would echo back to the session that sent it |
| `dedupe` | A repeat the dedupe cache recognised as already delivered, or the attempt cache as already attempted |
| `failed` | The push, the role forward, or the delivery-mode guard before either, failed |
| `receipt_timeout` | A role forward reached the server and its live holder sent no receipt within two seconds |
| `unplaceable_no_record` | An agent delivery given up: no session record this listener could read |
| `unplaceable_no_push_target` | An agent delivery given up: a record that names nothing to push to, or one this listener resolved and could not reach |

`envoy_delivery_duration_seconds` carries the same label for the attempts it times.

## Subscription visibility

An OMP extension keeps its direct NATS subscriptions locally and persists them through
`/v1/interests/subscribe`. `envoy_list()` reports their union: each topic is marked
`live` (local only), `registry` (persisted only), or `both`. This exposes live delivery
state even while an interest registration or cleanup request is delayed or unavailable.

## Role delivery and exceptions

`notifications.role.<role>` is a live, exactly-one-holder lane. The listener's core-NATS
queue subscriber resolves the current live role holder at delivery time, then sends a
receipt-backed request with the original role topic to `notifications.agent.<session_id>`.
The extension's agent pump replies after accepting that envelope. The outcome is one of three
exception reasons: `no_holder` when no session claims the role; `delivery_failed` when a claim
exists but the message is not known to have reached the holder (the holder lookup failed, the
holder's registration is stale, or the publish or the flush to the server failed or timed out);
`receipt_timeout` when the message was forwarded and flushed to the server and the registered,
live holder sent no receipt inside the two-second window (`bus.ErrReceiptTimeout`, which the
client returns from the receipt wait alone — never from the flush, whose own timeout is the same
`nats.ErrTimeout` value). A role claimant does not subscribe directly to its role subject, and a
role message is not retained for a future claimant.

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

An agent subject's `no_holder` is published once per message, at give-up rather than on the first
attempt that failed to place it. A delivery this listener cannot place — no session record it
could read, or a record naming nothing to push to — is NAKed for redelivery every five seconds
while the message's JetStream age is inside `ENVOY_UNPLACEABLE_RETRY_WINDOW` (default 60 s) and
its delivery count is below 18, logging `listener agent delivery deferred` each time; only the
give-up logs `listener agent delivery given up`, publishes the exception, and counts
`unplaceable_no_record` or `unplaceable_no_push_target`. A role lane publishes its exception on
the attempt itself, since nothing redelivers a core message.

Role exceptions use core NATS with the role lane. Other control exceptions retain ordinary
JetStream transport. Exception topics do not produce further exception events.
