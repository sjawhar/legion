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
- JetStream KV bucket `envoy_sessions` for session host data, plus the `plugin` and `plugin_version` each registration names (the npm package the session registers through and that plugin's build; absent for a session whose plugin predates them). `ENVOY_SESSION_MAX_AGE` sets the bucket's `MaxAge` at start; unset, the listener leaves the bucket as it is. That age bounds how long a record survives, not how long a session is alive: liveness is computed from each record's `UpdatedAt` (see “Session state”), never from the presence of a key
- Each listener serves both buckets from an in-memory copy its watcher keeps (`internal/kvwatch`). A watcher reads its bucket again on a reconnect, a recreated bucket or a failed watch without emptying the copy: every row stays until the new read completes, each delivered record replaces its row as it arrives, and only the completed read removes the rows it did not find. An incomplete read removes nothing, and the listener reads again itself.

## Listener API

All `/v1/*` endpoints answer 503 `service starting` until NATS is connected and the interest and session caches are warm; they do not wait for the durable consumer. The two list reads, `GET /v1/sessions` and `GET /v1/interests/`, answer that time with the refilling 503 the routes themselves give (below).
A listener's caches follow their buckets through watchers (`internal/kvwatch`): the interest and
session buckets, and the CI bucket on a listener that mounts the GitHub webhook route. A start
reads each bucket in full, and so does a rewatch after a NATS reconnect or a self-health rebuild
(`kvwatch.Watcher.Rewatch`), which resumes from no revision: the copy keeps every row while the
new read runs, each delivered record replaces its row as it arrives, and only a completed read
removes the rows it did not find, so an incomplete read removes nothing and the listener reads
again itself. A rewatch that cannot be created is logged at WARN. JetStream
delivers no event when the session bucket's `MaxAge` expires a key, so the session cache gives
each entry a deadline one `MaxAge` after its write, and a timer removes the entry at that
deadline. That deadline bounds the cache, not a session's life: state is computed from the
record's `UpdatedAt`, and an entry the cache no longer holds is read through to the bucket, where
a record the bucket does not hold is `gone` and nothing else.
A list read answers `503` with `Retry-After: 5` and `{"error":"envoy: the agent list is refilling; try again in a moment","code":"refilling"}` while a copy it reads cannot be served: before that copy's first complete read, and once it has not followed its bucket (a read under way, an ended watcher, a recreated bucket, a lost NATS connection) for longer than 2 minutes, or half the bucket's `MaxAge` when that is shorter. A list from such a copy could be missing a live agent that a caller would take for absent. The `envoy_active_sessions` and `envoy_active_interests` gauges have no sample then; `0` is only ever a complete empty copy.
Every `/v1` request with a body must set `Content-Type: application/json` (parameters such as
`charset=utf-8` are accepted). The listener returns a JSON 415 error otherwise. Webhook routes
retain their signature and content handling.
A listener with no bearer configured serves `/v1` only to a `Host` that names it: loopback, or a
name in `ENVOY_ALLOWED_HOSTS` (port stripped, case folded, one trailing dot dropped). Any other
`Host` gets a JSON 421, so a DNS-rebinding page cannot reach the bearer-free API. A listener that
requires a bearer checks no `Host`.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/sessions` | GET | Lists the `live` sessions across all machines, and the `unresponsive` ones too when asked with `include=unresponsive` — joins `envoy_interests` (topics, dir) with `envoy_sessions` (self-subscribed delivery mode, `plugin`, `plugin_version`), and reports as each session's `roles` those whose claims in `envoy_roles` name it, from the listener's watched view of that bucket, whatever role topics its record carries. Every row's `port` is 0, since a registration naming a port is refused and the field stays only because every released client parses it. Each row carries the session's `state` (`live` or `unresponsive`) and its `last_seen`, computed from its registration's `UpdatedAt`: the list is not itself the liveness answer, and a row's absence is not death. A cache view, and a list cannot be made authoritative key by key, so a caller holding one session id asks `/v1/registry/{session_id}` instead. Answers the refilling 503 while either copy cannot be served. Interest records the cache misses are read from the bucket within one 3.5 s budget. A read counts as answered only for its named answers: no key (the session has no topics), a record this build cannot decode (`store.ErrMalformedInterest`, logged by the registry) or an id the interest bucket refuses as a key (`bus.ErrRefused`: an earlier listener's id past its get bound that `envoy_sessions` still lists, or one outside nats.go's key alphabet, logged at WARN naming it); the last two list that session with no topics. Any other error, a cap or the budget running out, a nats.go connection error, a raw socket error or a claims view that is not complete among them, is a read that did not answer: a 503 with `Retry-After: 1`, never a row with no topics or roles. The claims view answers only while it is complete (`store.Registry.RolesHeldBy`): the role claim watcher's first scan delivered every claim, and after a recreated bucket's reset only once the refill's scan has; until then it is `store.ErrRoleViewNotReady`, never a holder without its role. A first scan nats.go's idle timer ended is scanned again a second later until one completes (`kvwatch.WithRescan`) |
| `/v1/interests/subscribe` | POST | Persist a session's subscribed topics; `self_subscribed: true` marks a session that takes its messages off a NATS subscription of its own (`packages/envoy/AGENTS.md`, "Session consumers", states how a session's messages reach it). A registration whose `port` is above 0 is refused `400 {"error":"port is not supported: …","expected":["port"]}` before anything is stored: the listener delivers to no port. A stored record this build cannot decode (`store.ErrMalformedInterest`) is replaced, not merged, logged at WARN by the registry; the session's role claims are kept. A write route (see “Write route answers” below). A topic that reaches a private stream subject other than the session's own agent subject (another session's `notifications.agent.<other>`, a failed-delivery lane `notifications.envoy.exceptions.notifications.agent.<id>` including its own, a Legion daemon notice under `notifications.legion.>` such as the controller lane `notifications.legion.<project>.controller`, or a wildcard such as `notifications.agent.>`, `notifications.envoy.>` or `notifications.>`) is not stored and is named in `warnings`, so neither the fan-out nor `/v1/replay` hands a session another session's inbox, a failed delivery's payload or the controller's notices. An optional `delivery` object (`consumer`, `epoch`, `resume`) asks for a session consumer, created or refiltered before the call answers; the answer's `delivery` says `consumer` (with the relay `subject` and `stream_id`) or `core` with a `reason` |
| `/v1/interests/unsubscribe` | POST | Remove persisted topics for a session and answer in `removed` exactly the topics that left the record, never an echo of the request; a consumer-delivery session's consumer stops filtering on them. A stored record this build cannot decode is left as it is (it routes none of its topics), logged at WARN. The request carries `client_instance_id`, the process handing the interests back, and that decides which role claims it frees: of the roles the topics name, only the claims naming that same process, so a second process that resumed this session's id frees none of the holder's. A release that states no process frees no claim that names one, and frees a claim that names none outright, the carve-out that keeps every claim standing at a deploy released by its holder's clean stop. A role lane topic whose claim this release did not free stays in the record and is left out of `removed`, because one record serves every process running under one session id and that topic is the interest half of a claim that still stands, written there by the claim itself; no delivery rides on it, since a role lane never enters a session's consumer filter and a role message goes to its holder's agent subject with the holder read off the claim. Such a release — stating no process, not forcing, and naming that role's own lane topic — is a hand-back, and it marks the claim it could not free (`handed_back`): no registration renews a marked claim, `GET /v1/roles/<role>` answers `handed_back` meanwhile, and the role claim reaper frees the role at its first pass past the live window, unless the holder, which reads a marked claim as a role it does not hold, claims it back at its next heartbeat. A release that loses its conditional delete to a record written since reads the claim again, and one still naming that same session and that same process is its own holder's renewal, so the delete goes out again at the revision that renewal left, three deletes in all; a claim renewed under every one of them is left standing and fails the route naming the role, rather than reporting a release it did not make. This route never forces a release; `DELETE /v1/interests/{session_id}` is the only caller that does. A write route |
| `/v1/interests/` | GET | List persisted interests. Answers the refilling 503 while the interest copy cannot be served |
| `/v1/interests/{session_id}` | GET/DELETE | Get or delete a session's persisted interests. A GET reads a cache miss from the bucket within 3.5 s: 404 only for a key the bucket does not hold, 413 or 400 for an id NATS will not take as a key, `500 {"error":"envoy: the interest record of session <id> is unreadable: <reason>"}` with no `Retry-After` for a record this build cannot decode (`store.ErrMalformedInterest`), 503 with `Retry-After: 1` for a read that did not answer. A DELETE removes a record this build cannot decode too, forcing the release of every role claim the session holds whichever process holds it, with 204; it is the one caller that forces a release, because an operator clearing a session's interests states no process and a release naming none frees nothing a process the claim names holds. It names no role's lane topic either, so it is no hand-back and marks nothing. It is a write route |
| `/v1/roles/set` | POST | Claim a role; the claim makes the session the holder, and the route also adds the role's topic to its record. The request carries `client_instance_id`, the process making the claim (the id every registration carries: one per Oh My Pi Envoy extension instance and one per Claude Code channel-server process, minted when that instance starts), and the claim record names it as the process holding the role. An explicit claim is last-claim-wins and reads no holder at all. A soft claim is granted when the role is unheld, when its holder is not `live` — `gone`, or `unresponsive` past the live window, which is how a relaunched Legion role recovers its own role — and when its holder is the session this one continues (`previous_session_id`) and that predecessor is this claimant's to take: the predecessor is not `live`, or its live entry carries the claimant's own `client_instance_id`, or either side carries no instance id. The last case is a client older than the field, and the listener logs that takeover with both session ids; the predecessor rule reads the claimant's instance from the request and falls back to its stored entry only where the request states none. A role already held under the claimant's **own** session id is decided on the standing claim and never on the session record, which names whichever process registered that id last: granted where that claim names this very process, granted where nobody has answered for the claim inside the listener's configured live window (`renewed_at`, which every registration of the holding process stamps) and the role claim view can answer, and otherwise refused the 409 naming that session, logged once with the holder's instance and the claimant's. That renewal arm is read only while the view answers (`store.Registry.RoleViewErr`), because nothing but a registration renews a claim and a registration renews through that very view: while the view cannot answer — the state a listener restart leaves until its first read of the bucket completes — every claim's renewal ages with nothing able to move it, so the arm is the bus-unavailable 503 with `Retry-After: 1` naming the view and claims nothing rather than hand a live holder's role to a second process. The arm that grants to the process the standing claim names is not gated: it rests on no renewal, and it is that holder re-claiming its own role after the restart. So a second process that resumed a live session's id cannot take the role that session holds, and reads `/v1/roles/{role}`'s `client_instance_id` to see which process does. A `live` holder refuses the claim atomically, a predecessor still live under another client instance among them, which gets the 409 any other live holder gets. The predecessor's entry is read from the `envoy_sessions` bucket itself, not from this listener's cache of it, and a predecessor read that did not answer is the bus-unavailable 503. The claimant's entry is read from the bucket too, falling back to the cached entry when that read does not answer. The answer names the previous holder's `holder_state`. A claim carrying `if_claim_revision` is conditional: it lands only while the role's claim record is still at that `envoy_roles` revision, the one `/v1/roles/{role}` reported as `claim_revision`, where `0` names a role with no claim record at all. Any other revision, a record where the claim names none, or none where it names one is `409 {error, role, holder, claim_revision}` carrying the revision the bucket does hold, and so is a claim that lost its conditional write to a claim that landed in between; no claim record is written, so the role keeps the holder the standing claim gave it. A hard claim with no `if_claim_revision` is weighed by the last-claim-wins rule alone, while a soft one with none carries a precondition the route supplies in the caller's place: the revision of the claim record the route decided on, `0` where no record stands, since the route decides on one read of that record and the write reads it again, and without the precondition two processes of one session id claiming one unheld role in the same moment were both granted it. A record replaced under such a claim is no answer for a caller that asked nothing about revisions: the route reads the claim again, decides the whole thing again on what the second read finds, and tries once more, a second mismatch answered with the same 409. A claim stating its own `if_claim_revision` is never decided again, that revision being the caller's own precondition. A write route |
| `/v1/registry/{session_id}` | GET | The authoritative answer for one session — reads through the cache to the session bucket, so a session the cache has not read yet is still found, and returns the same body a `/v1/sessions` row carries, roles from the claims included, with the session's `state` and `last_seen`. It takes the list's selector: a session whose state is `unresponsive` is a 200 asked with `include=unresponsive`, and without it the 404 below, which names that session's `state` and `last_seen` all the same, so a caller reads `state` rather than the status code to decide whether the session ended. A live session whose id only the interest bucket refuses as a key is a 200 with no topics, as the list shows it, logged at WARN naming it: Dispatch's envoy client reads a 413 or 400 here as a session that cannot exist. It refuses with 413 or 400 for an id the session bucket will not take as a key, `404 {"error":"no live session <id>", "state":…, "last_seen":…}` for a key the bucket does not hold, which is `gone`, and for an `unresponsive` session the caller did not ask for, the unreadable-interest-record 500 `/v1/interests/{session_id}` gives, and 503 with `Retry-After: 1` for a read that did not answer, the session's, its interest record's or the claims view's, all within a 3.5 s budget |
| `/v1/sessions/{session_id}` | DELETE | Remove a session's registration; the session is `gone` from that moment, with no live window to wait out. A caller that must not remove a registration another process wrote names a condition, and the registration is removed only while the stored entry meets it, answered `200 {"deleted":true}` for one that is gone and `200 {"deleted":false,"reason":…}` for one that stayed. With `client_instance_id` it is removed only while it carries that id or carries none, so a departing client instance never removes the registration a later instance made under the same session id; with `if_not_registered_after` (unix milliseconds, and a value that is not a positive whole number of them is a 400) only while it was last written at or before that moment, so the Legion daemon's clean stop of a dead role's session never removes the registration that claim's relaunch wrote under the same session id. A write route |
| `/v1/messages/send` | POST | Send a direct agent-to-agent message. The target is read from the session store itself, so a 404 means the store says that session's key is gone; a store read or a publish the bus did not answer is a 503 with `Retry-After: 1` (see “Send and publish answers” below). |
| `/v1/messages/publish` | POST | Publish an event. Required `message` becomes the human `payload_summary`; optional `payload` is distinct machine data in `payload`. A topic the stream captures is answered once JetStream stored it, bounded to five seconds. A role topic is forwarded to its live holder, and an ad-hoc topic (any `notifications.` topic the stream does not capture, such as `notifications.team.deploy-squad`) is published over core NATS and forwarded to each self-subscribed session registered on it; both are answered once each recipient confirmed the message, or with a 504 naming each that did not after four seconds (see "Role delivery and exceptions"). A publish of a captured topic the bus did not serve, and a role-holder read at a role send's first resolution that did not answer, are both the 503 below rather than a 500. |
| `/healthz` | GET | Health check (always available, even during startup); its bus checks answer within 3 s, and a rebuild of the KV watches that has not succeeded for a minute after a reconnect is `200 degraded` naming the `KV watch rebuild` - not a refusal, which would take the probes' only listener out and replace it with one that rebuilds into the same failure on empty caches, so the state pages through the `<N>-reconnect-hooks-stalled` alarm instead - while a rebuild that fails and is run again under that minute leaves the answer it had (see “Webhook and health check bounds” below) |

## Session state

A session is `live`, `unresponsive` or `gone`, computed from its registration record's `UpdatedAt`
and from nothing else — never from whether a cache, a list or the bucket's key set holds it:

| State | Rule |
| --- | --- |
| `live` | It registered within `ENVOY_SESSION_LIVE_WINDOW` (a Go duration, default 5m) |
| `unresponsive` | Its last registration is older than the live window and newer than `ENVOY_SESSION_GONE_HORIZON` (a Go duration, default 1h). It is not dead: it keeps its role claims and its issue claims, a message sent to it is stored and reaches it when it answers again, and nothing starts a replacement for it |
| `gone` | Its last registration is older than the gone horizon, or the registry holds no record of it |

Every list row, every one-session lookup, every send or publish refusal that names a session and
every role answer carries that session's `state` and `last_seen`. A role answer names the holder's
state as `holder_state` and says, with `claim_released`, whether that answer released the claim;
it is always `false`, because no answer releases one. `GET /v1/roles/<role>` also carries
`handed_back` where the holder's claim carries the hand-back mark, on the 200 and on a lapsed
holder's 404 alike, and leaves the key out where it carries none. Both clients read a marked
claim as a role they do not hold (`roleHeldBy` in `@legion/envoy-client/transport`), and the mark
is read nowhere in `POST /v1/roles/set`, which weighs a marked claim by the process it names and
then by its renewal exactly as it weighs any other; every grant writes a fresh record, so a grant
to the process the claim names clears the mark.

No lookup, publish, role-lane delivery attempt, Dispatch outbox forward or daemon probe releases a
role claim, whatever the holder's state. Only the role claim reaper does. A claim renewed inside
the live window is kept whatever the session bucket says of the id its holder registered under: a
holder renews its claim by registering (`renewed_at`), and that is what keeps the role of a
process whose session id a second process resumed and then deregistered, leaving the record gone
and the holder running. Past that window the rule is the one dispatch://INFRA-392 settled,
unchanged: an unmarked claim is kept while its holder's own record reads short of `gone` and
freed once that session is gone, for a claim that names a process and a claim that names none
alike, so an `unresponsive` holder keeps its role and the paths that need a live holder report it
instead. The one claim freed past a holder session that still answers is one a hand-back marked
(`handed_back`), and only once its own renewal has lapsed: the mark is the holder's own word that
it gave the role up and no registration renews a marked claim, so a role an older client handed
back comes free one live window plus up to one reap interval later — about ten minutes with the
shipped defaults — while that client is still registering. A registration renews every unmarked
claim its own process holds, whatever topics it carries, and one that states no process renews
the claims of its own session that name none and adopts none, so nothing standing at a deploy
ages out. The reaper runs on a period of its own,
`ENVOY_ROLE_CLAIM_REAP_INTERVAL` (a Go duration, default 5m), and not on the session reaper's:
while the session bucket's maximum age is below the gone horizon, as the 5m it ships with is, a
record the bucket expires reads `gone` too, so a period shorter than that age takes a merely
stalled holder's role that much sooner. Once the maximum age covers the horizon no record goes
early, and the period decides only how long past the horizon a gone holder keeps its role, so it
belongs well inside the horizon. A Legion role whose agent process exits is `gone` at once rather
than at the end of the live window, so its successor's soft claim takes the role without waiting
the window out; a soft claim replaces the holder, it does not release the claim.

Dispatch reads `ENVOY_SESSION_LIVE_WINDOW` and `ENVOY_SESSION_GONE_HORIZON` too and judges a role
route's holder by them (`roles.Windows`, `cmd/dispatch`'s `roleHolderWindows`), so a deployment
gives its listeners and its Dispatch server the same two values.

`ENVOY_SESSION_MAX_AGE` sets the `envoy_sessions` bucket's `MaxAge`; unset, the listener leaves the
bucket's own. A gone horizon longer than that age reads a record the bucket expired as `gone`
before the horizon, so the operator raises the bucket to cover the horizon. `/healthz` reports the
age in use as `session_max_age`, a Go duration string such as `"5m0s"`.

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
| The stream refuses the publish because it holds its byte limit (`bus.ErrStreamFull`) | `503 {"error":"envoy: the message bus refused the message because its stream is full: <reason>"}` with `Retry-After: 1`, recorded `stream_full` |
| The server denies the publish under this listener's own grant (`bus.ErrPublishDenied`) | `403 {"error":"envoy: the message bus denied the publish under this listener's own permissions: <reason>"}` with no `Retry-After`, recorded `publish_denied` (below) |
| Any other publish failure | `503` with the same prefix and `Retry-After: 1`, carrying the bus's own reason |

A target whose record the store holds is resolved whatever its state, so a send to an
`unresponsive` session is published to its inbox and waits there for it. The `404` is only a target
the store holds no record of; it reports that target's `state` as `gone`, and its `last_seen` while
the registry still remembers it (two hours past the last registration, `session.LastSeenMemory`),
so a caller never reads a refusal as the end of a session that is merely quiet.

`POST /v1/messages/publish` answers the publish of a topic the stream captures that failed, and a
role send's first role-holder resolution that did not answer, exactly the same way; that
resolution (the claim read and the holder's session read; it writes nothing to the role bucket, so
a holder it finds past the live window keeps its claim) runs
inside the same 3.5 s budget, and one the budget runs out on is that 503 too, naming the call that
did not answer. Its 413, its 400s and its 403 for a denied publish are unchanged. Its role 404s
(`unclaimed`, `holder_lapsed`) keep their bodies, and a role send gives one only when the listener
kept nothing: the request sent `keep: false`; the role's token is one `roles.Valid` refuses, which
no claim can ever name; the claim lapsed and the restart grace retained it, whose holder keeps the
role and writes no new claim for a handover to follow; or the keep itself failed. A role send whose
message
the listener kept answers `202` instead. A role or ad-hoc topic's confirmed send then runs on
within its own 4 s and answers its 200, 202, 403 `capability` or 504 as "Role delivery and
exceptions" says.

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

A role-holder read that did not answer, within its cap or the 3.5 s budget, is the bus-unavailable
503 above with `Retry-After: 1`, naming the call, on the publish route and on
`GET /v1/roles/<role>` alike, and a key NATS refuses keeps its 413 or 400. On both routes a
claim record no read can decode (`store.ErrMalformedRoleClaim`: one naming no holder, or JSON that
is not a claim) is `500 {"error":"envoy: the claim record of role <role> is unreadable: <reason>"}`
with no `Retry-After` (`writeRoleClaimUnreadable`), recorded `role_claim_unreadable` on a publish:
the bus answered, and a retry reads the same record. One resolution is one claim read: neither
route retries, because a holder that stopped answering keeps its claim and there is no release for
a second read to see past. `store.Registry` bounds that claim read by the caller's context itself
(`RoleClaim`), since nats.go's KV calls take none.

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
can be denied, and `role_unclaimed`, `role_lapsed`,
`role_claim_unreadable` and a confirmed send's 504 reason (`receipt_timeout`, `delivery_failed`,
`not_confirmed`, `publish_failed`) for publish. A role send whose message the listener kept is
`accepted`, because its 202 is an answer and not a refusal, so `role_unclaimed` and `role_lapsed`
name a send that kept nothing. Two
refusals carry no such line, because neither reaches a handler: the 401 the API auth wrapper
writes, and a request the starting gate turns away while the routes are still opening, which logs
`request refused while starting` itself.

`envoy_messages_delivered_total` counts one delivery attempt per recipient, labelled
`delivery_status`:

| `delivery_status` | Attempt it counts |
| --- | --- |
| `delivered` | Delivered to the session, or forwarded to a role holder that receipted it |
| `skipped` | Nothing to deliver: the session's own consumer holds the message, the session consumes its own subscription, or the envelope would echo back to the session that sent it |
| `dedupe` | A repeat the dedupe cache recognised as already delivered, or the attempt cache as already attempted |
| `kept` | A role message no live holder could take, kept for the session that claims the role next |
| `failed` | The delivery, the role forward, or the delivery-mode guard before either, failed |
| `receipt_timeout` | A role forward reached the server and its live holder sent no receipt within two seconds |
| `unplaceable_no_record` | An agent delivery given up: no session record this listener could read |

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
100 ms later, until a reply arrives or four seconds from the request's arrival have passed. No
attempt forwards to a holder that is not live, so no role send is counted on a stored copy. It
answers `200` with the
`holder` and its `holder_state` once the holder replied; `202` with `kept: true` and
`kept_reason: "unclaimed"` when no session holds the role at all and the listener kept the
message for the role's next holder; `404` with `reason` `unclaimed` or
`holder_lapsed` when no live session holds the role and nothing was kept — `unclaimed` when
nothing claims it and the keep was refused or failed, which a sender asks for with `keep: false`,
and `holder_lapsed` when the claim's holder is not live, carrying that holder's `holder_state`
(`unresponsive` inside the gone horizon, `gone` past it), its `last_seen` and
`claim_released: false`. A holder that is only `unresponsive` is refused this way too, and keeps
everything: the publish is not forwarded to it, its claim stands, nothing is kept for the role,
and the sender holds the message
and publishes it again rather than treating the holder as dead. Also the 503 or 500 `GET /v1/roles/<role>` gives
when its first resolution of the holder, within 3.5 s of the request's arrival, fails (see "Send
and publish answers"); `403` with `reason: "capability"` when the holder does not take the
payload's delivery mode; and `504` with `reason`, `event_id`, `dedupe_key`, `holder` and
`attempts` once the four seconds passed, which keeps nothing, since that forward is already stored
on the holder's agent subject. The reason is
`receipt_timeout` when the message was forwarded and flushed to the server and the registered,
live holder sent no receipt (`bus.ErrReceiptTimeout`, which the client returns from the receipt
wait alone — never from the flush, whose own timeout is the same `nats.ErrTimeout` value), and
`delivery_failed` when the message is not known to have reached the holder (no live holder to
forward to, a holder lookup that failed or ran out of time, a publish or flush to the server
that failed or timed out, or the stream did not acknowledge storing the forward,
`bus.ErrForwardNotStored`). The route publishes no exception: its sender has the answer.

The listener keeps a role message on a stored subject of its own,
`notifications.envoy.kept.role.<role>`. No session may register that subject, no replay hands it
out, and no consumer of the notification stream delivers it, so a kept copy never re-enters the
delivery path it was taken out of. `notifications.role.>` is still not a stream subject. The next
session that claims the role is handed the role's kept copies off its own claim request, and the
claim is answered before they are forwarded. One walk of a role's lane is bounded - twenty
forwards, two hundred copies read, sixty seconds - and the handover around it walks the lane again,
for the role's holder as it resolves it each time, until the lane is empty, the role has no live
holder, or three walks running make no progress at all; a walk that failed is waited out on a
widening cadence while the holder is live. The role's cursor,
`notifications.envoy.kept.cursor.<role>`, carries the lane's progress and one walk's claim of it,
so two listeners walking one lane do not forward a copy twice. A handover this process lost - a
restart, a rolling deploy - is picked up by a sweep every five minutes and at the listener's own
start, which hands over every lane that holds copies after its cursor while the role's holder is
live: nothing waits for a claim a live holder will not make. Each forwarded
copy carries `kept_at`, the Unix-millisecond time the listener kept it, so the holder can judge
the message's age. A copy the listener kept more than an hour before is dropped rather than
forwarded, and counted on `envoy_role_kept_expired_total`. `GET /v1/roles/<role>` still answers
404 for an unheld or lapsed role: only the publish route gained the kept answer. A sender that
keeps a queue of its own and redelivers from it asks not to be kept (`keep: false` in the publish
body) and gets today's 404; the Legion daemon's notice queue does, and a phase worker's stall
reminder, whose next run says the same thing. The opt-out is the route's alone: a message published
straight onto a role lane is always kept, Dispatch's role-routed events among them, which the
outbox sends once and reads no exception for.

An ad-hoc topic sent through the route is published over core NATS, then forwarded the same way
to each session registered on it, on any machine, that holds its own NATS connection
(`self_subscribed`), other than the sender. It answers `200` once the topic was published and
each recipient confirmed, there being none included; `504` with `reason: "publish_failed"` and no
recipient list when the topic's own publish did not reach NATS, whether the four seconds ended
first or the publish failed at once for any reason but a refusal, since nothing was forwarded;
and otherwise `504` with `reason: "not_confirmed"`, `delivered` and `undelivered` (each
`{session_id, reason}`). Only a receipt that arrived through the route counts as a confirmation:
the route keeps its own record of who confirmed each dedupe key, apart from the delivery handlers'
dedupe cache, where the durable path records a self-subscribed session whose delivery it skipped.
A session that stopped without unregistering stays a recipient until the reaper finds it
`gone`; one inside the gone horizon is forwarded to with no receipt wait and named under
`unresponsive`, and one that answers nothing is named in `undelivered`. Neither is a failed
delivery to the others. Every forward carries a `Nats-Msg-Id`, so the stream keeps one copy per
message and recipient, and a listener acknowledges that stored copy without delivering it.

The role lane, a core-NATS queue subscriber in each listener, carries a role topic a client
publishes straight onto NATS rather than through the route. It resolves the holder at delivery
time and forwards once. An unclaimed role is kept for the role's next holder first, as the
publish route keeps one, and a message that was kept publishes no exception and counts as `kept`.
A keep that failed leaves its branch as it was, and a lapsed holder keeps nothing, since its
claim stands, so the lane's outcome is then one of three exception reasons: `no_holder` when no
session claims the role, `delivery_failed` when the holder is not live (an `unresponsive` holder
among them, whose claim the lane leaves alone) or a read or forward failed, and `receipt_timeout`
as above. A role claimant does not subscribe directly to its role subject.
The forward is stored on the holder's agent subject (one copy per message and holder, by its
`Nats-Msg-Id`), so a holder on consumer delivery that was away gets it from its session consumer
when it is back.

Dispatch's outbox forwards its own role-routed events to the holder itself: it resolves the
holder through the one resolution the listener's routes and its lane use (`roles.ResolveHolder`,
`internal/roles`) and sends the envelope, its role topic kept, to the holder's agent subject, so
Dispatch's events no longer cross `notifications.role.*`. That resolution writes nothing to the
role bucket here either: a role with no claim, and one whose holder is `gone`, counts as reached
with nothing sent, and the `gone` holder's claim is left where it is for the role claim reaper;
an `unresponsive` holder's event stays pending and is retried, so it waits for that session to
answer rather than being published with nothing sent, as a read or a forward that failed does.
The outbox raises no exception.
The listener's role lane gives its forward a `Nats-Msg-Id` (`bus.ForwardMsgID`): the forward's
dedupe key and the agent subject when the original envelope's dedupe key, before the forward's
`envoy.role.forward.` prefix is added, names its event, and the envelope's `event_id` and the
agent subject otherwise, since two distinct envelopes can share a key their sender chose. So the
stream stores one copy of a message per recipient however often it is forwarded, and two messages
under one key as two.

When a control delivery has no holder, cannot be forwarded, or draws no receipt, Envoy emits an
envelope on `notifications.envoy.exceptions.<original-topic>`. So does a role-lane publish the API
refuses because no live session holds the role: that refusal is still answered `404`, and the
exception goes out behind the answer, on a goroutine of its own, so it never delays it. This adds
a bus signal only: no Legion consumer acts on a peer's refusal. Upgrade every Legion daemon before
the listener: an older daemon reads either refusal reason as malformed and logs
`ERROR role-lane exception not re-held` once for each refused attempt to publish its own notice. A
held notice row tries again after 1, 2, 4, 8, 16 and 32 s and then every 64 s, and only the oldest
held notice of each architect publishes, so that is about seven ERROR lines in the first minute and
then one every 64 s for each architect whose role is unheld while it has a notice to deliver. The
exception payload preserves the original delivery fields:

| Field | Meaning |
| --- | --- |
| `original_topic` | Original envelope topic |
| `event_id` | Original envelope event ID |
| `reason` | A delivery that failed: `no_holder`, `delivery_failed`, or `receipt_timeout` (defined above). A publish the API refused: `unclaimed` or `holder_lapsed`, the refusal's own state, told apart from `no_holder` because nothing was forwarded |
| `payload_summary`, `payload` | Original human summary and distinct machine payload. A refusal's exception carries the payload only up to 64 KiB (`refusalExceptionBound`) and leaves a larger one out, so a refusal never becomes a publish NATS refuses whole |
| `dedupe_key` | Original envelope dedupe key |
| `source`, `source_session` | Original envelope source fields |

An agent subject's `no_holder` is published once per message, at give-up rather than on the first
attempt that failed to place it, and only after the give-up itself is recorded. A delivery this
listener cannot place — no session record it could read — is NAKed for redelivery every five
seconds, counted from the message's first delivery rather than from its age in the stream:
`ENVOY_UNPLACEABLE_RETRY_WINDOW` (default 60 s) is
spent as whole five-second deferrals, so it buys twelve of them at that default, and the delivery
count stops the retries at 18 whatever the window says. That cause delivers nothing, so
retrying it repeats nothing, and it keeps the window's whole allowance.
At most 64 agent deliveries are held deferred at once — a
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
`unplaceable_no_record`, logs WARN
`listener agent delivery given up`, and then publishes the exception. A notice that cannot be
published — it embeds the original payload, so an envelope near the server's max payload has its
notice refused as too large on every attempt — logs ERROR `listener exception publish failed`
beside the give-up line and changes nothing else: the message is not NAKed, it keeps the
attempt-cache entry that turns a redelivery a lost ACK brings into `listener attempt-dedupe skip`,
and it is ACKed. No other path NAKs on a notice it could not publish: the generic fanout, the one
that still could, owes no `no_holder` notice at all, since the handler routes every role and agent
topic elsewhere and a topic reaching the fanout is never a control topic.
A role lane publishes its exception on the attempt itself, since nothing redelivers a core message.
The `no_holder` exception the durable path stores carries
`Nats-Msg-Id` `exception.no_holder.<origin>` (the message's own `Nats-Msg-Id`, else its stream
sequence), so the stream keeps one copy however many listeners give the message up.

Role exceptions use core NATS with the role lane. Other control exceptions retain ordinary
JetStream transport. Exception topics do not produce further exception events.
