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
| `/v1/sessions` | GET | Lists all sessions across all machines — joins `envoy_interests` (topics, dir) with `envoy_sessions` (port, self-subscribed delivery mode). A cache view, and a list cannot be made authoritative key by key, so a caller holding one session id asks `/v1/registry/{session_id}` instead. Interest records the cache misses are read from the bucket within one 3.5 s budget; only a read that did not answer (a cap or the budget ran out, or the connection could not carry the request) is a 503 with `Retry-After: 1`, never a row with no topics. Any other error on one session's interest read lists that session with no topics and roles, logged at WARN naming it: a record this build cannot decode (`store.ErrMalformedInterest`, logged by the registry), an id the interest bucket refuses as a key (`bus.ErrRefused`: an earlier listener's id past its get bound that `envoy_sessions` still lists, or one outside nats.go's key alphabet), or anything else |
| `/v1/interests/subscribe` | POST | Persist a session's subscribed topics; `self_subscribed: true` permits a portless session that consumes its own NATS subscription. A stored record this build cannot decode (`store.ErrMalformedInterest`) is replaced, not merged, logged at WARN by the registry; the session's role claims are kept |
| `/v1/interests/unsubscribe` | POST | Remove persisted topics for a session. A stored record this build cannot decode is left as it is (it routes none of its topics), logged at WARN; the role claims the topics name are released |
| `/v1/interests/` | GET | List persisted interests |
| `/v1/interests/{session_id}` | GET/DELETE | Get or delete a session's persisted interests. A GET reads a cache miss from the bucket within 3.5 s: 404 only for a key the bucket does not hold, `500 {"error":"envoy: the interest record of session <id> is unreadable: <reason>"}` with no `Retry-After` for a record this build cannot decode (`store.ErrMalformedInterest`), 503 with `Retry-After: 1` for a read that did not answer. A DELETE removes a record this build cannot decode too, releasing the session's role claims, with 204 |
| `/v1/roles/set` | POST | Claim a role for a live session and register its role topic |
| `/v1/registry/{session_id}` | GET | The authoritative answer for one session — reads through the cache to the session bucket, so a session the cache has not refilled is still found, and returns the same body a `/v1/sessions` row carries. It refuses with 413 or 400 for an id NATS will not take as a key, `404 {"error":"no live session <id>"}` only for a key the bucket does not hold, the unreadable-interest-record 500 `/v1/interests/{session_id}` gives, and 503 with `Retry-After: 1` for a read that did not answer, the session's or, within a 3.5 s budget, its interest record's |
| `/v1/messages/send` | POST | Send a direct agent-to-agent message. The target is read from the session store itself, so a 404 means the store says that session's key is gone; a store read or a publish the bus did not answer is a 503 with `Retry-After: 1` (see “Send and publish answers” below). |
| `/v1/messages/publish` | POST | Publish an event. Required `message` becomes the human `payload_summary`; optional `payload` is distinct machine data in `payload`. Role topics and ad-hoc topics (any `notifications.` topic the stream does not capture, such as `notifications.team.deploy-squad`) use core NATS and reach the sessions subscribed at that moment; JetStream acknowledgement for every topic the stream captures is bounded to five seconds. A publish the bus did not serve, and a role-holder read that did not answer, are both the 503 below rather than a 500. |
| `/healthz` | GET | Health check (always available, even during startup) |

## Send and publish answers

`POST /v1/messages/send` resolves its target from the session store, not from this listener's
cache of it: the cache answers a hit, and a miss is read through to the `envoy_sessions` bucket
within one second (`liveSessionEntry`, `cmd/listener/role_holder.go`), the same direct get the role
paths already make. That read and the publish share one 3.5 s budget taken from the caller's own
request context, so a store or a bus this listener cannot reach is answered inside the caller's
attempt:

| Outcome of the attempt | Answer |
| --- | --- |
| The store says the key is gone (`nats.ErrKeyNotFound`, which also covers a session id no session can register under) | `404 {"error":"no live session <id>"}` |
| The store read did not answer (its second ran out, or the read failed any other way) | `503 {"error":"envoy: the message bus did not answer: <reason>"}` with `Retry-After: 1` |
| The target is live but does not advertise the frame's `delivery.mode`, or that mode cannot be read unambiguously | `403` naming the session, and the mode when it could be read |
| The publish is refused as too large (`bus.ErrTooLarge`) | `413` naming the size against the server's max payload |
| The publish is refused for its subject or key (`bus.ErrRefused`) | `400` naming the refusal, with no `Retry-After` |
| Any other publish failure | `503` with the same prefix and `Retry-After: 1`, carrying the bus's own reason |

`POST /v1/messages/publish` answers a publish failure, and a role-holder read that did not answer,
exactly the same way; its role-holder resolution (the claim read, the holder's session read and the
release of a lapsed claim) runs inside the same 3.5 s budget, and one the budget runs out on is that
503 too, naming the call that did not answer. Its 413, its 400s and its role 404s (`unclaimed`,
`holder_lapsed`) are unchanged. Two refusals are its own. A publish the server denies under this
listener's own grant (`bus.ErrPublishDenied`) is
`403 {"error":"envoy: the message bus denied the publish under this listener's own permissions: <reason>"}`
with no `Retry-After`, recorded `publish_denied`. Only a topic the stream does not capture — a
role lane or an ad-hoc topic, which go over core NATS — can be denied this way, since that error is
made only where a core publish is confirmed (`confirmPublished`, `internal/bus/confirm.go`); a send
publishes `notifications.agent.<id>`, which the stream captures, so it goes through JetStream and
never meets it. The other is a publish that lost the race for the role's claim — every one
of the two resolution attempts spent on a supersession (`errRoleHolderChanged`) — keeps the 503
and the `Retry-After: 1`, since a fresh read wins that race as soon as the replacement claim
settles, but answers its own body,
`{"error":"envoy: the holder of role <role> changed while this publish was resolving it"}`,
recorded `role_holder_changed`: every bus read answered, so no bus was unavailable.
`GET /v1/roles/<role>` resolves its holder the same way, within the same budget, and answers that
race with the same 503 and `Retry-After: 1`, worded
`{"error":"envoy: the holder of role <role> changed while this lookup was resolving it"}`; a read
that did not answer, within its cap or that budget, is the bus-unavailable 503 above with
`Retry-After: 1`, naming the call, and a key NATS refuses keeps its 413 or 400. On both routes a
claim record no read can decode (`store.ErrMalformedRoleClaim`: one naming no holder, or JSON that
is not a claim) is `500 {"error":"envoy: the claim record of role <role> is unreadable: <reason>"}`
with no `Retry-After` (`writeRoleClaimUnreadable`), recorded `role_claim_unreadable` on a publish:
the bus answered, and a retry reads the same record. `store.Registry` bounds the claim read and
the release by the caller's context itself (`RoleClaim`, `ReleaseExpiredRoleClaim`), since nats.go's
KV calls take none.

A send and a publish stamp the envelope's `sender` from the sender's interest record (`senderStamp`,
`cmd/listener/api.go`), which a cache miss reads through to the bucket within one second of the
same budget (`senderStampReadTimeout`; `store.Registry.Get` bounds the bucket read alone, so a
cache hit answers in the caller's goroutine). The stamp is only enrichment: a read that does not
answer leaves it the session id and the title this listener's cache holds, and the publish that
follows meets the same silent bus and answers the 503 above inside the caller's attempt.

A publish whose caller's context has already ended puts nothing on the wire; on these two routes it
is answered the 503 above. A core publish (a role lane or an ad-hoc topic) checks its context
immediately before it publishes (`publishConfirmed`, `internal/bus/confirm.go`): one check sees
both a spent deadline and a cancel while the deadline is still ahead, so a cancel during a wait for
a reconnect sends nothing (a cancel after that check cannot be undone); the
JetStream path has no check of its own and relies on nats.go's, whose request returns the context's
error before it sends anything. It matters because the publish route's role-holder resolution runs
before the publish under the same budget, and one that answers just as the budget ends leaves the
publish none: a core subject has no duplicate window, and the client retries every 5xx
(`packages/envoy-client/src/transport.ts`), so a message emitted behind a failure answer would
reach its subscriber twice.

Each inbound send and publish is recorded in one line, INFO when accepted and WARN when refused:
`listener send` (`target_session`) and `listener publish` (`topic`), both carrying `event_id`,
`source`, `source_session`, `payload_summary`, `outcome` and `status`, and `holder` as well when a
role topic resolved one. `outcome` is one of `accepted`, `invalid_request`, `too_large` and
`bus_unavailable`, plus `no_live_session` and `capability_refused` for send and `publish_denied`,
`role_unclaimed`, `role_lapsed`, `role_holder_changed` and `role_claim_unreadable` for publish. Two
refusals carry no such line, because neither reaches a handler: the 401 the API auth wrapper
writes, and a request the starting gate turns away while the routes are still opening, which logs
`request refused while starting` itself.

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
| `unplaceable_no_push_target` | An agent delivery given up: a record that names nothing to push to |
| `unplaceable_push_failed` | An agent delivery given up: a record named a serve port and the push did not land on it — an unreachable peer, a session process restarting, a connection refused, or a server that answered with anything but 204 |

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
attempt that failed to place it, and only after the give-up itself is recorded. A delivery this
listener cannot place — no session record it could read, a record naming nothing to push to, or a
push that failed — is NAKed for redelivery every five seconds, counted from the message's first
delivery rather than from its age in the stream: `ENVOY_UNPLACEABLE_RETRY_WINDOW` (default 60 s) is
spent as whole five-second deferrals, so it buys twelve of them at that default, and the delivery
count stops the retries at 18 whatever the window says. The first two causes push nothing, so
retrying them repeats nothing, and they keep the window's whole allowance. A push that failed is
deferred once, however long the window, and the give-up after it is terminal whether or not its
notice can be published, so it is pushed at most twice: it may have landed — the session server
refusing the prompt, a connection that never reached it and an answer lost after the server took
the prompt all come back as the same failure — and a deferral clears the attempt-cache entry that
otherwise keeps a redelivery from pushing again, so each retry can repeat an instruction the
session already has. One retry covers a peer restarting or a connection refused for a moment;
twelve would let the same
instruction arrive thirteen times. At most 64 agent deliveries are held deferred at once — a
quarter of the durable's 256 ack-pending slots, one of which a deferred message holds for its whole
window — and a message that arrives while that cap is reached is given up at once. The 64 is one
budget for the whole listener: a single set, made once at start and shared by every delivery
handler built from it, the one a self-health rebind builds after a NATS flap included, so a rebind
opens no second budget beside the first. The set is keyed on each message's JetStream stream
sequence, so only that message's next delivery gives its slot back; a redelivery this listener
never deferred (another branch's NAK, an expired ack wait) frees nothing. A slot whose message
never comes back here (the stream dropped or purged it, or its redelivery reached another
subscriber) expires 125 s after the deferral that took it, the 5 s delay plus two 60 s ack waits,
the longest a deferred message can legitimately stay pending, so such slots cannot hold the budget
for longer than that. Each deferral logs
`listener agent delivery deferred` with `reason`, `attempt` and `window_deferrals`, the deferrals
that message's cause is allowed (1 for `push_failed`); only the give-up counts
`unplaceable_no_record`, `unplaceable_no_push_target` or `unplaceable_push_failed`, logs WARN
`listener agent delivery given up`, and then publishes the exception. A notice that cannot be
published — it embeds the original payload, so an envelope near the server's max payload has its
notice refused as too large on every attempt — logs ERROR `listener exception publish failed`
beside the give-up line and changes nothing else: the message is not NAKed, it keeps the
attempt-cache entry that turns a redelivery a lost ACK brings into `listener attempt-dedupe skip`,
and it is ACKed. No other path NAKs on a notice it could not publish: the generic fanout, the one
that still could, owes no `no_holder` notice at all, since the handler routes every role and agent
topic elsewhere and a topic reaching the fanout is never a control topic.
A role lane publishes its exception on the attempt itself, since nothing redelivers a core message.

Role exceptions use core NATS with the role lane. Other control exceptions retain ordinary
JetStream transport. Exception topics do not produce further exception events.
