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
- JetStream stream `ENVOY_NOTIFICATIONS` with 72h retention for non-role notification replay on listener restart, and an optional byte limit (`ENVOY_STREAM_MAX_BYTES`) past which it refuses new publishes (`DiscardNew`; the sender gets the retryable `bus.ErrStreamFull`, which the webhook answers 503) and keeps every stored message for its 72h, so the limit is sized above the stream's 72-hour volume; per-machine durable consumers carry a 7d inactive threshold so the server garbage-collects consumers of decommissioned listeners
- Role lanes and delivery-exception lanes for role topics use core NATS; they have no durable consumer, retention, or replay. Stream reconciliation excludes role subjects and purges any role message captured while that configuration is updated. The listener forwards a role message to its holder's agent subject, which the stream stores, so a holder on consumer delivery gets the forward through its session consumer also after a restart.
- Session consumers: a session that asks at registration (`delivery`) gets a durable pull consumer `s-<session id>` on the stream, filtered on its agent subject and its stored followed topics, 72h inactive threshold; the listener that owns the session relays it on `notifications.envoy.deliver.<session id>` (core NATS, not stored) and acks a message only on the session's `done` (`packages/envoy/AGENTS.md`, "Session consumers").
- The stream and every KV bucket hold the deployment's replica count, `ENVOY_NATS_REPLICAS` (1 to 5, default 1; more than 1 needs a clustered NATS), and the stream at least the byte limit `ENVOY_STREAM_MAX_BYTES` when set: a start creates them at that count and limit, raises one found with fewer copies or a smaller limit (or none), and never lowers either, and every program that owns the stream (each listener, the Dispatch server) runs a keeper that every 30 seconds raises the stream's copies and byte limit back to its settings, puts back the discard policy and `envoy_stream_id`, and raises its own buckets' copies. An operator's raise of the limit stands, and programs with different settings do not fight: the largest stands. JetStream's stream update has no compare-and-swap, so a start with other or no settings can put back what it read until the next keeper pass; a clustered deployment sets the same `ENVOY_NATS_REPLICAS` and `ENVOY_STREAM_MAX_BYTES` in every program
- JetStream KV bucket `envoy_interests` for session subscriptions
- JetStream KV bucket `envoy_sessions` with 5m TTL for session port/host data, plus the `plugin` and `plugin_version` each registration names (the npm package the session registers through and that plugin's build; absent for a session whose plugin predates them)

## Listener API

All `/v1/*` endpoints answer 503 until NATS is connected and the interest and session caches are warm; they do not wait for the durable consumer.
A listener's caches follow their buckets through watchers (`internal/kvwatch`): the interest and
session buckets, and the CI bucket on a listener that mounts the GitHub webhook route. A start
reads each bucket in full. A rewatch, after a NATS reconnect or a self-health rebuild, resumes
after the last revision its cache applied, so it delivers only what was written since; it reads in
full again when it cannot resume (a recreated bucket, or a scan that did not complete), and when
its resuming watcher cannot be created, which it logs at WARN. JetStream
delivers no event when the session bucket's 5m `MaxAge` expires a key, so the session cache gives
each entry a deadline one TTL after its write: a read reports an entry past it as absent and
changes nothing, and a timer removes the entry at that deadline.
Every `/v1` request with a body must set `Content-Type: application/json` (parameters such as
`charset=utf-8` are accepted). The listener returns a JSON 415 error otherwise. Webhook routes
retain their signature and content handling.
A listener with no bearer configured serves `/v1` only to a `Host` that names it: loopback, or a
name in `ENVOY_ALLOWED_HOSTS` (port stripped, case folded, one trailing dot dropped). Any other
`Host` gets a JSON 421, so a DNS-rebinding page cannot reach the bearer-free API. A listener that
requires a bearer checks no `Host`.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/sessions` | GET | Lists all sessions across all machines — joins `envoy_interests` (topics, dir) with `envoy_sessions` (port, self-subscribed delivery mode, `plugin`, `plugin_version`), and reports as each session's `roles` those whose claims in `envoy_roles` name it, from the listener's watched view of that bucket, whatever role topics its record carries. A cache view, and a list cannot be made authoritative key by key, so a caller holding one session id asks `/v1/registry/{session_id}` instead. Interest records the cache misses are read from the bucket within one 3.5 s budget. A read counts as answered only for its named answers: no key (the session has no topics), a record this build cannot decode (`store.ErrMalformedInterest`, logged by the registry) or an id the interest bucket refuses as a key (`bus.ErrRefused`: an earlier listener's id past its get bound that `envoy_sessions` still lists, or one outside nats.go's key alphabet, logged at WARN naming it); the last two list that session with no topics. Any other error, a cap or the budget running out, a nats.go connection error, a raw socket error or a claims view that is not complete among them, is a read that did not answer: a 503 with `Retry-After: 1`, never a row with no topics or roles. The claims view answers only while it is complete (`store.Registry.RolesHeldBy`): the role claim watcher's first scan delivered every claim, and after a recreated bucket's reset only once the refill's scan has; until then it is `store.ErrRoleViewNotReady`, never a holder without its role. A first scan nats.go's idle timer ended is scanned again a second later until one completes (`kvwatch.WithRescan`) |
| `/v1/interests/subscribe` | POST | Persist a session's subscribed topics; `self_subscribed: true` marks a portless session, one the listener relays its consumer to rather than pushes to a port (`packages/envoy/AGENTS.md`, "Session consumers", states how a session's messages reach it). A stored record this build cannot decode (`store.ErrMalformedInterest`) is replaced, not merged, logged at WARN by the registry; the session's role claims are kept. A write route (see “Write route answers” below). A `notifications.` topic no stream subject captures, and that is no role lane, refuses the whole registration `400`, since nothing would ever be delivered on it; follow a `notifications.team.<name>` topic instead. A topic that reaches a private stream subject other than the session's own agent subject (another session's `notifications.agent.<other>`, a failed-delivery lane `notifications.envoy.exceptions.notifications.agent.<id>` including its own, a Legion daemon notice under `notifications.legion.>` such as the controller lane `notifications.legion.<project>.controller`, or a wildcard such as `notifications.agent.>`, `notifications.envoy.>` or `notifications.>`) is not stored and is named in `warnings`, so neither the fan-out nor a session's own consumer hands a session another session's inbox, a failed delivery's payload or the controller's notices. An optional `delivery` object (`consumer`, `epoch`, `resume`) asks for a session consumer, created or refiltered before the call answers; the answer's `delivery` says `consumer` (with the relay `subject` and `stream_id`), and a registration the listener can give no consumer is refused by name — `503 no_grant` or `consumer_error`, `409 not_requested` or `invalid_session_id` — with nothing written, a serving-port registration excepted |
| `/v1/interests/unsubscribe` | POST | Remove persisted topics for a session; a consumer-delivery session's consumer stops filtering on them. A stored record this build cannot decode is left as it is (it routes none of its topics), logged at WARN; the role claims the topics name are released. A write route |
| `/v1/interests/` | GET | List persisted interests |
| `/v1/interests/{session_id}` | GET/DELETE | Get or delete a session's persisted interests. A GET reads a cache miss from the bucket within 3.5 s: 404 only for a key the bucket does not hold, 413 or 400 for an id NATS will not take as a key, `500 {"error":"envoy: the interest record of session <id> is unreadable: <reason>"}` with no `Retry-After` for a record this build cannot decode (`store.ErrMalformedInterest`), 503 with `Retry-After: 1` for a read that did not answer. A DELETE removes a record this build cannot decode too, releasing the session's role claims, with 204; it is a write route |
| `/v1/roles/set` | POST | Claim a role for a live session; the claim makes it the holder, and the route also adds the role's topic to its record. A write route |
| `/v1/registry/{session_id}` | GET | The authoritative answer for one session — reads through the cache to the session bucket, so a session the cache has not refilled is still found, and returns the same body a `/v1/sessions` row carries, roles from the claims included. A live session whose id only the interest bucket refuses as a key is a 200 with no topics, as the list shows it, logged at WARN naming it: Dispatch's envoy client reads a 413 or 400 here as a session that cannot exist. It refuses with 413 or 400 for an id the session bucket will not take as a key, `404 {"error":"no live session <id>"}` only for a key the bucket does not hold, the unreadable-interest-record 500 `/v1/interests/{session_id}` gives, and 503 with `Retry-After: 1` for a read that did not answer, the session's, its interest record's or the claims view's, all within a 3.5 s budget |
| `/v1/sessions/{session_id}` | DELETE | Remove a live session registration. A write route |
| `/v1/messages/send` | POST | Send a direct agent-to-agent message. The target is read from the session store itself, so a 404 means the store says that session's key is gone; a store read or a publish the bus did not answer is a 503 with `Retry-After: 1` (see “Send and publish answers” below). |
| `/v1/messages/publish` | POST | Publish an event. Required `message` becomes the human `payload_summary`; optional `payload` is distinct machine data in `payload`. A topic the stream captures is answered once JetStream stored it, bounded to five seconds. A role topic is forwarded to its live holder, and an ad-hoc topic (any `notifications.` topic the stream does not capture, such as `notifications.team.deploy-squad`) is published over core NATS and forwarded to each self-subscribed session registered on it; both are answered once each recipient confirmed the message, or with a 504 naming each that did not after four seconds (see "Role delivery and exceptions"). A publish of a captured topic the bus did not serve, and a role-holder read at a role send's first resolution that did not answer, are both the 503 below rather than a 500. |
| `/healthz` | GET | Health check (always available, even during startup); its bus checks answer within 3 s, and a rebuild of the KV watches that has not succeeded for a minute after a reconnect is `200 degraded` naming the `KV watch rebuild` - not a refusal, which would take the probes' only listener out and replace it with one that rebuilds into the same failure on empty caches, so the state pages through the `<N>-reconnect-hooks-stalled` alarm instead - while a rebuild that fails and is run again under that minute leaves the answer it had (see “Webhook and health check bounds” below) |

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
| The stream refuses the publish because it holds its byte limit (`bus.ErrStreamFull`), a republish the full stream refused (`bus.ErrNotStored` wrapping it) among them | `503 {"error":"envoy: the message bus refused the message because its stream is full: <reason>"}` with `Retry-After: 1`, recorded `stream_full` |
| The server denies the publish under this listener's own grant (`bus.ErrPublishDenied`) | `403 {"error":"envoy: the message bus denied the publish under this listener's own permissions: <reason>"}` with no `Retry-After`, recorded `publish_denied` (below) |
| Any other publish failure | `503` with the same prefix and `Retry-After: 1`, carrying the bus's own reason |

`POST /v1/messages/publish` answers the publish of a topic the stream captures that failed, and a
role send's first role-holder resolution that did not answer, exactly the same way; that
resolution (the claim read, the holder's session read and the release of a lapsed claim) runs
inside the same 3.5 s budget, and one the budget runs out on is that 503 too, naming the call that
did not answer. Its 413, its 400s, its 403 for a denied publish and its role 404s (`unclaimed`,
`holder_lapsed`) are unchanged; a role or ad-hoc topic's confirmed send then runs on within its
own 4 s and answers its 200, 403 `capability` or 504 as "Role delivery and exceptions" says.

A publish the server denies under this listener's own grant (`bus.ErrPublishDenied`) is the
table's `403` on either route. A publish on either transport can be denied this way: a core
publish reads the denial from the connection's last error across its flush (`confirmPublished`,
`internal/bus/confirm.go`), and a JetStream publish, which the stream acknowledges with nothing
when the server refuses it, reads the violation the server reported under its own subject
(`deniedPublish`, `internal/bus/denials.go`). So a topic the stream captures is answered that 403
too, and so is `POST /v1/messages/send`, which publishes `notifications.agent.<id>` over
JetStream. That is a change for a client: such a publish and such a send were the retried `503`
with `Retry-After: 1` before, and are now a `403` with none, which
`packages/envoy-client/src/transport.ts` does not retry (it retries every status at or past 500 and
none below it).

One refusal is the publish route's own: a publish that lost the race for the role's claim — every one
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
follows meets the same silent bus and answers the 503 above inside the caller's attempt. The
stamp's `roles` are the roles whose claims name the sender, from the claims view the session routes
read; a view that is not complete (`store.ErrRoleViewNotReady`) leaves them out, and the send goes
on.

A publish whose caller's context has already ended puts nothing on the wire; on these two routes it
is answered the 503 above, and an ad-hoc topic's confirmed send its 504 `publish_failed`. A core
publish (a role send's forward or an ad-hoc topic) checks its context
immediately before it publishes (`publishConfirmed`, `internal/bus/confirm.go`): one check sees
both a spent deadline and a cancel while the deadline is still ahead, so a cancel during a wait for
a reconnect sends nothing (a cancel after that check cannot be undone); the
JetStream path has no check of its own and relies on nats.go's, whose request returns the context's
error before it sends anything. It matters because a route's reads run before its publish under the
same deadline, and one that answers just as the deadline ends leaves the publish none: a core
subject has no duplicate window, and the client retries every 5xx
(`packages/envoy-client/src/transport.ts`), so a message emitted behind a failure answer would
reach its subscriber twice.

Each inbound send and publish is recorded in one line, INFO when accepted and WARN when refused:
`listener send` (`target_session`) and `listener publish` (`topic`), both carrying `event_id`,
`source`, `source_session`, `payload_summary`, `outcome` and `status`, and `holder` as well when a
role topic resolved one. `outcome` is one of `accepted`, `invalid_request`, `too_large`,
`bus_unavailable` and `stream_full`, plus `no_live_session` for send, `capability_refused` for send
and a role send, `publish_denied` for send and publish alike, since a publish on either transport
can be denied, and `role_unclaimed`, `role_lapsed`, `role_holder_changed`,
`role_claim_unreadable` and a confirmed send's 504 reason (`receipt_timeout`, `delivery_failed`,
`not_confirmed`, `publish_failed`) for publish. Two
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

`envoy_delivery_duration_seconds` carries the same label for the attempts it times.

## Write route answers

`POST /v1/interests/subscribe`, `POST /v1/interests/unsubscribe`, `POST /v1/roles/set`,
`DELETE /v1/interests/{session_id}` and `DELETE /v1/sessions/{session_id}` do every store read and
write inside the same 3.5 s budget taken from the request's own context, where a silent bus held
each for the 10 s JetStream wait: nats.go's KV reads, writes and deletes take no context, so each
bucket call waits through `bus.Bounded` (`internal/bus/kv.go`), which stops waiting when the
context ends. Store work that did not answer, whatever it names, is
`503 {"error":"envoy: the message bus did not answer: <reason>"}` with `Retry-After: 1`
(`writeStoreError`, `cmd/listener/api.go`). The store's own answers keep the answers they had: a
refused key its 413 or 400, a held role its 409, and no key, an undecodable record or a lost
compare-and-swap the status the route always gave it; an error a claim joins from several is one
only when every part of it is. A write the budget abandoned may still land
after the 503: a put rewrites the record the caller's retry writes again, and a delete or a claim
is conditional on the revision it read (`store.Registry.UpsertWithin`, `Remove`,
`SetRoleWithPrevious`, `session.SessionRegistry.PutWithin` and `Delete` say why each is safe).
A session holds a role exactly when the role's claim in `envoy_roles` names it, so a claim's
rollback and its old holder's topic cleanup are best-effort tidying of interest topics within the
same budget: a claim that landed answers 200 whatever the cleanup did, and a topic left behind
gives nobody the role.

## Webhook and health check bounds

The webhook routes and `/healthz` do their bus work within the request; the last row below is the
one check `/healthz` makes with no call of its own, from a stall the bus already recorded. The
webhook routes keep every answer and status they had, and `/healthz` every answer but a check's
that did not answer; what changed is how long a silent bus can hold them.

| Path | Bus work | Bound | A call that did not answer |
| --- | --- | --- | --- |
| `/webhook/github`, a check_suite or check_run | CI record writes (`cistore.Store.Record`, `RecordSuite`) | each record 4 s (`recordWait`: the wait for a write it queues behind and its own 2 s write budget), every KV call included; all of it ends when GitHub drops the delivery or at the route's write deadline | `503 service unavailable`, as a failed record |
| `/webhook/github`, any other event | publishes | the request, the route's write deadline, and the bus's 5 s a publish | `503 service unavailable` |
| `/webhook/slack` | publishes | 2.5 s from the handler's start (`slackAnswerBudget`), inside the 3 s Slack waits | `503 service unavailable` |
| `/webhook/ghostwispr` | publish | the request, and the bus's 5 s | `503 service unavailable` |
| `/healthz` | flush, each cache's KV ping, the durable consumer's info read | 3 s from the request's start, shared (`healthzBudget`) | `503 unhealthy` naming the dependency and `context deadline exceeded`, so the container health check fails, as it did when the 10 s wait outran it; a check that answered with a failure keeps its answer (`200 degraded` or `503 unhealthy`) |
| `/healthz`, the KV watch rebuild | none: the stall the bus client recorded for its reconnect hooks (`bus.Client.HookStall`) | a minute of hooks due with no run that succeeded (`bus.RepairGrace`) | `200 degraded`, `KV watch rebuild unavailable: <last rewatch error> (no rebuild has succeeded for 1m0s)`, the stall alone when no run has failed behind it; deliberately not a refusal, since the ECS container health check and GKE's readiness probe both read `/healthz` on a single task, so refusing would replace or unroute the only listener and its replacement would rebuild into the same failing rewatch with caches that warm up empty, losing the fanout messages the running task's stale caches still place, and the state pages through the `<N>-reconnect-hooks-stalled` alarm on the bus's ERROR `envoy nats reconnect hooks have not succeeded` instead; a shorter stall keeps the answer the checks above gave, since the bus runs the hooks again within seconds of a failed run |

nats.go's KV calls take no context and wait out the JetStream MaxWait (10 s), so a CI write's KV
calls and `/healthz`'s pings are waited for through a context (`kvCall`, `bus.Bounded`) that
stops waiting when the bound ends. A CI write given up on may still land after the 503: it is a
compare-and-swap at the revision it read (a create when it read none), so it never overwrites a
write since, and a redelivery of its observations writes nothing. A ping given up on runs on to its
own end and changes nothing a later ping would not.

## Subscription visibility

An OMP extension keeps its direct NATS subscriptions locally and persists them through
`/v1/interests/subscribe`. `envoy_list()` reports their union: each topic is marked
`live` (local only), `registry` (persisted only), or `both`. This exposes live delivery
state even while an interest registration or cleanup request is delayed or unavailable.
A session's messages do not come from those subscriptions: they come from its own durable
consumer, which the listener relays (`packages/envoy/AGENTS.md`, "Session consumers").

## Role delivery and exceptions

`notifications.role.<role>` is a live, exactly-one-holder lane. A role message sent through
`/v1/messages/publish` is delivered by the route itself: it resolves the current live role holder,
sends a receipt-backed request with the original role topic to `notifications.agent.<session_id>`,
and the extension's agent pump replies after accepting that envelope. Without a reply inside two
seconds the route resolves the holder again and forwards the same envelope (one `event_id`)
100 ms later, until a reply arrives or four seconds from the request's arrival have passed. A
holder served on a port answers no receipt, so its forward counts as delivered once the stream
acknowledges storing it, and the stored copy is what reaches its port. It answers `200` with the
`holder` once the holder replied; `404` with `reason` `unclaimed` or
`holder_lapsed` when no live session holds the role; the 503 or 500 `GET /v1/roles/<role>` gives
when its first resolution of the holder, within 3.5 s of the request's arrival, fails (see "Send
and publish answers"); `403` with `reason: "capability"` when the holder does not take the
payload's delivery mode; and `504` with `reason`, `event_id`, `dedupe_key`, `holder` and
`attempts` once the four seconds passed. The reason is
`receipt_timeout` when the message was forwarded and flushed to the server and the registered,
live holder sent no receipt (`bus.ErrReceiptTimeout`, which the client returns from the receipt
wait alone — never from the flush, whose own timeout is the same `nats.ErrTimeout` value), and
`delivery_failed` when the message is not known to have reached the holder (no live holder to
forward to, a holder lookup that failed or ran out of time, a publish or flush to the server
that failed or timed out, or the stream did not acknowledge storing the forward,
`bus.ErrForwardNotStored`). The route publishes no exception: its sender has the answer.

An ad-hoc topic sent through the route is published over core NATS, then forwarded the same way
to each session registered on it, on any machine, that holds its own NATS connection
(`self_subscribed`), other than the sender. It answers `200` once the topic was published and
each recipient confirmed, there being none included; `504` with `reason: "publish_failed"` and no
recipient list when the topic's own publish did not reach NATS, whether the four seconds ended
first or the publish failed at once for any reason but a refusal, since nothing was forwarded;
and otherwise `504` with `reason: "not_confirmed"`, `delivered` and `undelivered` (each
`{session_id, reason}`). Only a receipt that arrived through the route counts as a confirmation:
the route keeps its own record of who confirmed each dedupe key, apart from the delivery handlers'
dedupe cache, where the live topics lane records a self-subscribed session whose push it skipped.
A session that stopped without unregistering stays registered for up to five minutes and is
named in `undelivered` in that time; it is not a failed delivery to the others. A session served
on a port is not confirmed. Every forward carries a `Nats-Msg-Id`, so the stream keeps one copy
per message and recipient; a listener acknowledges that stored copy without delivering it, except
for a serving-port session of its own, which has no subscriber and takes the stored copy as its
delivery.

The role lane, a core-NATS queue subscriber in each listener, carries a role topic a client
publishes straight onto NATS rather than through the route. It resolves the holder at delivery
time and forwards once, and its outcome is one of three exception reasons: `no_holder` when no
session claims the role, `delivery_failed` or `receipt_timeout` as above. A role claimant does not
subscribe directly to its role subject, and a role message is not retained for a future claimant.
The forward is stored on the holder's agent subject (one copy per message and holder, by its
`Nats-Msg-Id`), so a holder on consumer delivery that was away gets it from its session consumer
when it is back; a serving-port holder counts the stream's acknowledgement alone, since its relay,
or the owning listener's durable, pushes the stored copy to its port.

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
that message's cause is allowed; only the give-up counts
`unplaceable_no_record` or `unplaceable_no_push_target`, logs WARN
`listener agent delivery given up`, and then publishes the exception. A notice that cannot be
published — it embeds the original payload, so an envelope near the server's max payload has its
notice refused as too large on every attempt — logs ERROR `listener exception publish failed`
beside the give-up line and changes nothing else: the message is not NAKed, it keeps the
attempt-cache entry that turns a redelivery a lost ACK brings into `listener attempt-dedupe skip`,
and it is ACKed. No other path NAKs on a notice it could not publish: the generic fanout, the one
that still could, owes no `no_holder` notice at all, since the handler routes every role and agent
topic elsewhere and a topic reaching the fanout is never a control topic.
A role lane publishes its exception on the attempt itself, since nothing redelivers a core message.
On the durable path a push to a serving-port session this listener owns goes to that session's
serving-port worker, which pushes it again (1 s doubling to 30 s) until the port answers 204, so
its later messages keep their stream order; the one-retry bound on a push that failed applies to a
push the handler makes inline. The `no_holder` exception the durable path stores carries
`Nats-Msg-Id` `exception.no_holder.<origin>` (the message's own `Nats-Msg-Id`, else its stream
sequence), so the stream keeps one copy however many listeners give the message up.

Role exceptions use core NATS with the role lane. Other control exceptions retain ordinary
JetStream transport. Exception topics do not produce further exception events.
