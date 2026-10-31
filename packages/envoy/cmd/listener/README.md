# envoy-listener

The listener takes webhooks in, serves the `/v1` API, and delivers the events of one machine id
(`ENVOY_MACHINE_ID`) to the sessions it owns. Any number of listener processes may run for one
machine id, in Kubernetes as pods of one Deployment: one of them, the active pod, delivers what the
machine's durable consumer `listener-<machine id>` carries; every other is a standby that serves
everything else and takes over delivery when the active pod stops or is lost.
`packages/envoy/AGENTS.md` holds the listener's wider rules; this file holds how its pods share one
machine id.

## Run model: an active pod and standbys

Every pod of a machine id, active or standby:

- serves `/v1`, `/healthz`, `/metrics` and the webhook routes;
- is a member of the role lane's core-NATS queue group `envoy-listener-<machine id>`, and of the
  live topics lane's `envoy-listener-live-<machine id>` when `ENVOY_SUBSCRIBE_ALL_TOPICS=1`; NATS
  hands each message of a queue group to one member, so no role or live message is delivered by
  two pods;
- runs the self-health monitor over its caches.

Only the pod holding the machine's **delivery lease** receives the durable consumer, writes its
policy, and runs the loops one deliverer runs: the interest reaper, the role-claim reaper, the
interest marker collector and, on a listener with the GitHub webhook route, the CI summary loop. A
standby runs none of them, never creates or updates the durable, and registers no JetStream
subscription, so a NATS restart leaves it nothing to re-subscribe.

### The delivery lease

The lease is the key `lease.<durable>` (`lease.listener-<machine id>`) in the KV bucket
`envoy_listener` (`bus.ListenerStateBucket`), which the listener creates at `ENVOY_NATS_REPLICAS`
replicas, file storage, one value per key and a 20 minute `MaxAge`
(`listenerStateMaxAge`), raising a bucket an earlier build created with a shorter one on every
open. The age is sized by the `seen.` records beside the lease, not by the lease: every pod
answers a lapsed session's `last_seen` out of that record for the session TTL and the interest
reaper's allowance (20 minutes with the default 5 minute TTL), so the record must outlive the
answer. Its value is JSON: `holder`
(the holding pod's instance), `boot` (random per process start), `deliver_subject`,
`service_since`, `acquired_at`, `released`, and `stale_binding`, `unsettled`, `ack_wait_owed` and
`receiving` ("Take-over" below); a field this build does not know is kept across every rewrite.

Beside the lease, a pod that stopped gracefully writes one key per hand-over it saw through:
`handback.<durable>.<revision>` (`bus.ListenerHandBackPrefix`), the clean-hand-back mark, named
for the revision of the lease record its own release wrote. Its value is JSON: `instance`, `boot`
and `revision`. The pod that takes that very revision reads it inside its settle hold and ends
the hold at the hand-back delay instead of an ack wait ("Take-over" below). Nothing deletes a
mark: the bucket's 20 minute `MaxAge` drops it, no pod reads a mark for a revision it did not
replace, and revisions never repeat, so an old mark is never read as a newer hand-over's.

| Constant (`lease.go`) | Value | What it does |
| --- | --- | --- |
| `leaseRenewEvery` | 1 s | The holder renews the record with a compare-and-set at its last revision. |
| `leaseTTL` | 5 s | A standby takes an unreleased record that has not changed for this long, measured on its own monotonic clock from when it first saw that revision. |
| `leaseMargin` | 1 s | The holder counts itself the holder until the start of its last successful renewal plus `leaseTTL` minus this, so its term ends before any standby can take the record. |
| `leaseStandbyPoll` | 500 ms | How often a standby reads the record. |
| `leaseReceiveLost` | 3 s | How long this pod's receive connection may be down before the holder stops renewing and releases the record ("Take-over" below). |

A standby takes an absent record with a create, a released one at once, and an unreleased one
only once the TTL has passed; no wall clocks are compared. A holder whose renewal finds its record
taken (a compare-and-set conflict) steps down and unsubscribes. The holder receives with a core
subscription on the durable's deliver subject, never `nats.Bind`, and handles a message only while
it holds the lease: one received while it does not (the moment before an unsubscribe lands, or a
subscription nats.go sent again after a reconnect in place) is dropped with no ack and no NAK, and
the durable redelivers it to the holder after its ack wait.

Renewing is not the whole of holding. The lease renews on a NATS connection of its own
(`envoy-listener-lease <instance>`), while the durable is received on the pod's main client
(`envoy-listener <instance>`), so the one can be up while the other is down. The receive loop
gives the lease one signal for that (`durableReceiver.receiveUp`, `lease.SetReceiveSignal`),
the same state that makes `/healthz` 503 on the active pod: the main client's connection is
connected, and a term that is receiving holds a subscription on that connection. A
pod still preparing - reading the durable, waiting out a binding, settling before it has
subscribed - counts as receiving, as `/healthz` counts it a standby rather than an inactive
subscription. The lease judges that signal once a round, for the holder's release and the
standby's take alike ("Take-over" below).

The signal loads two atomics and reads nothing else: the client's own connected flag
(`bus.Client.Connected`, which its connection's disconnect, reconnect and close callbacks keep)
and the connection the loop recorded for the term's subscription when it made it
(`receiveSubscription`). It must, because the connection it judges is the one that is down:
nats.go holds a connection's own lock across each reconnect dial and its connect init, for the
connect timeout at a time when the route to the server drops packets rather than refusing them,
so a signal that read `nats.Conn.Status()`, or took the receive loop's lock, which that loop
holds across its own subscribes, would stall the lease's renewals along with the release it owes.
A subscription nats.go ends while its connection stays up is not read there; the self-health
monitor's durable probe finds that one and resubscribes (`durableReceiver.probe`).

The durable's ack wait is 6 s (`consumerAckWait`). The holder sends `InProgress` every 2 s for
every message it has received and not settled, queued or in hand, so a message waits out the ack
wait only once the pod holding it has died or lost the lease.

### Take-over

- **Graceful stop** (SIGTERM on the active pod): the stopping pod stops handing messages to the
  handler, gives the message in hand its grace, and hands back everything it has not handled: it
  NAKs those messages with a 2 s delay (`handBackDelay`), as one batch in stream order, and
  releases the lease marked `unsettled`. A standby takes the released lease at its next poll and
  settles (below): it subscribes at once and holds what it receives until every outstanding
  message is in its hands, or until the stopping pod's clean-hand-back mark is there and the
  hand-back delay and a second (3 s) have passed since it subscribed, then delivers in stream
  order, so per-session order is kept. The stopping pod keeps its
  subscription meanwhile, since nats-server loses a message whose first delivery is still in its
  send queue when the deliver subject's last subscriber goes: it NAKs with the same delay each
  message that reaches it after the stop, a repeat of one it already handed back included (three
  NAKs of one message at most, `handBackNaksMax`, then it drops it), and reads the lease record
  every 250 ms (`lingerPoll`) until it names another holder with `receiving` true (which the holder
  writes once its subscription is up), or for 3 s (`lingerMax`) after the batch at most. When no
  other pod has taken the record, there is no standby to wait for and it stops waiting; no end but
  a receiving holder comes before 1 s (`lingerUntaken`, two standby polls) and one poll have
  passed since the release, however far its 3 s cap has run, so a standby taking the record has
  the deliver subject held for it while it subscribes. Then it drains the subscription.

  Once that is done the stopping pod writes the mark, `handback.<durable>.<the revision its own
  release wrote>`, and only when every part of the hand-over went through: its own release wrote
  that record (a release its lease loop outlasted writes none), the term had no lost node's push
  binding open beside it (`stale_binding` false on the record it released), the message in hand's
  handler returned rather than ignoring its cancel, its linger ended by reading another holder
  `receiving`, and the server confirmed its drain rather than the 2 s drain wait running out. So
  everything it handed back is back within the standby's 3 s hold. A
  stop that falls short of any of that - no standby took the record, none was receiving when the
  linger ended, the successor's subscribe never reached the server, the release timed out and a
  later one wrote the record, a lost node's binding stood beside the term, or a handler was still
  running after its cancel, holding a message nothing keeps alive once the term stops - writes no
  mark, and
  what the durable pushed meanwhile may have gone to no subscriber, or to that binding alone, so
  the next holder settles for an ack wait instead (below). A failed write of the mark costs the
  same and nothing more. The stop's own line says which of those it was (`no_mark`). The standby
  delivers about 2 to 3 s after the stop (the hand-back delay and a settle read), about 3 to 4 s
  after it when a message waits out a retry's delay and holds the hold to the mark, plus the
  message in hand's grace when it takes it. The tests bound it at 10 s.
- **Hard stop** (SIGKILL, node or network loss): a standby takes the expired lease about 5 to 6 s
  after the last renewal (the TTL and one poll). Its term must settle: it subscribes at once,
  beside the dead pod's push binding when the server still counts a lost node's connection, and
  holds every message it receives, keeping it alive with `InProgress`, until every message the
  durable has outstanding is in its hands. It reads the durable every 250 ms (`settlePoll`) and
  ends the hold once `NumAckPending` is no more than the messages it holds, or at the cap of one ack
  wait and a second (`settleMargin`), 7 s after it subscribed; then it delivers the held batch in
  stream order and goes on live. The messages the dead pod had in hand come back one ack wait
  after its last `InProgress`, and those the server pushed to a lost node's connection alone before
  the standby subscribed one ack wait after that push, so all are back within the cap: they land in
  the held batch and a session keeps the order of its events. With nothing outstanding the hold
  ends at its first read and messages reach their sessions about 5 to 6 s after the last renewal;
  otherwise once the dead pod's messages are back, and at the cap, about 12 to 13 s after it, when
  a message waits out a retry's delay (a failed prompt's 30 s, an unplaceable delivery's 5 s): that
  message stays outstanding until its delay ends and arrives after the hold, as it would have on
  the dead pod. The tests bound it at 15 s.
- **Graceful stop after a hard one.** The lease record carries three flags for the next holder.
  `stale_binding` is set by a holder that subscribed beside a push binding it did not wait out (a
  lost node's lingering connection, or a stopping pod's subscription it keeps until this one
  receives), carried by every
  renewal, release and take-over, and cleared by a holder that finds the durable not push-bound,
  or, for a holder that took a hand-back candidate, by that hand-back's mark: the mark says the
  binding it subscribed beside was the stopping pod's own linger.
  `unsettled` is written from the acquiring write of a term that must settle until its hold ends,
  and a release keeps it, so a pod stopped mid-hold hands the hold on; a graceful stop that hands
  messages back always releases it set, and so does a stop taken while the term's renewals have
  lapsed, whatever its own flags stood at: the lapse dropped what it held without an ack.
  `ack_wait_owed` is the ack wait the releasing term still
  owed its successor: every term that must settle owes one from its acquiring write until its own
  settle hold ends, one taken from a hand-back included, so every release -
  a stop, a lapse, a give-up, a stop before the term ever subscribed - writes the debt as the term
  stands, a release during a lapse writing it set, and every take-over writes it false. A release
  the lease's loop outlasts (`handBackReleaseWait`, 2 s) writes no record and records the debt
  instead, so the release that lands after the linger has let go of the deliver subject carries
  it, and that release is vouched for by no mark. A pod that
  takes a released record carrying
  `stale_binding` or `unsettled` settles too, subscribing at once beside any binding rather than
  waiting it out as a pre-lease build's. Its hold ends at the hand-back delay and a second only
  when the record carries `unsettled` without `ack_wait_owed` and without `stale_binding` - which
  makes the term a hand-back candidate - and the mark for the revision it replaced is there,
  read every `settlePoll` inside the hold. Otherwise the cap is one
  ack wait and a second: the messages the stopping pod handed back come back after the
  hand-back delay to the next pod and to a lingering binding alike, but one that went to such a
  binding alone, or to no subscriber at all once the linger ended, or that a dead pod before the
  stopping one still held, comes back one ack wait later. A mark that cannot be read leaves the
  ack wait's cap standing, so a bucket that does not answer costs 4 s of a hand-over and no
  message.
- **A holder that cannot use the durable.** A holder whose reads, creations, policy corrections or
  subscribes of the durable fail retries every second (`durableBindPoll`) and times the failures
  from the start of the first since its term began or an attempt last succeeded. Once they have
  lasted 135 s (`durableTakeoverAlarmAfter`) it logs `lease holder cannot deliver from the durable`
  at ERROR, with the failure as `reason` and `waited_ms`, which the AWS `subscribe-exhausted` alarm
  matches, and gives the lease up: it releases the record, keeping `unsettled` and `ack_wait_owed`
  as its term left them, so the next holder settles as this one would have, and a standby takes
  the record at its next poll. The pod goes back to standby and does not take back the record it
  released for two standby polls (1 s), so every other standby has its chance first; a pod that
  runs alone takes it back after that and tries again, alarming again at the end of each stuck
  term. A listener without a lease has no one to give it to: it keeps retrying and logs the line
  again every 5 min
  (`durableTakeoverAlarmEvery`).
- **A holder whose receive connection is lost.** The lease and the durable ride two connections,
  so a pod can renew a lease it receives nothing on: the main client's link freezes or its hub
  stops answering, nats.go declares the connection stale two to three seconds later
  (`bus.WithSilentCutDetection`) and reconnects about a second after that, while the lease's own
  connection renews throughout. A holder whose receive signal has been down for `leaseReceiveLost`
  (3 s) stops renewing and releases the record with the WARN `listener lease released: the receive
  connection is down`, naming `down_ms`; a connection back inside those 3 s costs it nothing, not
  even a new term. The release carries `unsettled`, `ack_wait_owed` and `stale_binding`, whatever
  the term's own flags stood at: what the durable pushed to the lost connection comes back only at
  its ack wait, and that connection's subscription can still be bound at the server. So the
  standby takes the released record at its next poll, subscribes at once beside that binding and
  holds its full 7 s cap, and no clean-hand-back mark vouches for a release no hand-over went
  through. The receive loop ends the term as it ends any lost lease - it drops the subscription
  and leaves what it queued to come back at its ack wait - since nothing can be NAKed over a
  connection that is down. A route that drops the pod's packets rather than refusing them leaves
  nats.go's reconnect dials hanging, each holding the connection's own lock for the connect
  timeout, and costs the hand-over nothing: the signal the lease reads touches neither that
  connection nor the receive loop's lock (above). A message published as the connection was cut
  reaches its session
  about 13 s later (3 s detection, the 3 s bound, a standby poll and the 7 s hold), inside the
  15 s a hard take-over is bounded at. No pod takes the lease while its own receive connection is
  down - not an absent record, not a released one, not an expired one, and a holder that has
  lapsed does not resume - so the cut pod does not take back what it just released; it stands by
  again, and may take the lease, once the connection is up. A release the bucket does not take
  keeps the hold: nothing was handed over, so the pod goes on as a holder whose bucket calls fail,
  its renewals lapse, a standby takes its record once it expires, and it tries the release again
  at its next turn.

The lines to read are `listener lease acquired` (with `from`: `absent`, `released` or `expired`,
and `settle`, `stale_binding`, `hand_back` - the term is a hand-back candidate - and `replaced`,
the revision its mark would be keyed by), `listener lease term must settle; receiving at
once and holding what arrives until the durable's outstanding messages are in hand` (with `cap`
and `hand_back`),
`durable still push-bound by an earlier lease holder; receiving beside it`, `settle hold ended;
delivering the held messages in stream order` (with `outcome` `settled`, `cap` or `handed back`,
`held_ms`, `messages`, `ack_pending` and `hand_back_marked`), `settle hold: hand-back mark read
failed; holding on` (with `error`), `durable receiving` (with `lease`, `waited_ms` and `settle`),
`handed the durable's messages back to the next holder` (with `handed_back`, `lingered_ms`,
`linger_end` `receiving holder seen`, `not taken` or `timeout`, `renaked_repeats`,
`dropped_repeats`, `hand_back_marked` and, when that is false, `no_mark`: why no mark was
written), `cancelling the message in hand after the shutdown grace` (with `grace`), `the message
in hand was still being handled after its cancel`, `clean hand-back not marked; the next holder
settles for an ack wait` (with `error`),
`durable consumer read failed; retrying` and `durable subscribe failed;
retrying` (with `error`), `lease holder cannot deliver from the durable` (ERROR, with `reason` and
`waited_ms`), `giving the listener lease up: this pod cannot deliver from the durable`, `listener
lease lost: another listener holds it`, `listener lease released: the receive connection is down`
(WARN, with `down_ms` and the `revision` it released) and `listener lease released`.

### Graceful stop order

On SIGTERM or SIGINT, once `/v1` serves (`shutdownListener`):

1. the self-health monitor stops;
2. receiving stops, inside a 10 s budget the HTTP stop shares: the holder-only loops are told to
   stop, the handler takes no more messages, the message in hand gets 3 s (`inflightGrace`)
   before its prompt is cancelled (a cancelled prompt is a retryable failure: its ledger attempt
   is cleared and the message NAKed) and a second more (`inflightCancelWait`) for its handler to
   return, every message not handled (queued, or held by a settle hold)
   is NAKed with the 2 s hand-back delay in stream order, the lease is marked released and
   `unsettled`, the subscription stays until the next holder receives, 3 s after the NAKs at
   most and 1.25 s after the release at least (the floor that holds the deliver subject while a
   standby subscribes), then
   drains (2 s at most) and, when all of that went through, the clean-hand-back mark is written
   (one bucket call, 2 s at most): 11 s in all, and 12.25 s when the flush (1 s) and the release
   (2 s) both
   run to their bounds and leave that floor past the cap. The budget bounds none of those waits:
   it is how long the shutdown waits for the receive stop. Past it the shutdown goes on to the
   HTTP stop with no time left, then to the NATS drains and `os.Exit(1)`, while the hand-back may
   still be running. The listener's own shutdown, not the runtime's kill, cuts that hand-back off:
   `client.Drain` ends the subscription of a hand-back still lingering or draining, and `os.Exit(1)`
   ends only a mark write still in flight on the lease's own connection. So a longer grace period in
   the runtime keeps no mark, and the next holder then settles at its 7 s cap, as it does after any
   stop that wrote none. A
   listener without a lease drains its subscription and NAKs what it has not handled at once, in
   stream order;
3. the HTTP server shuts down within what is left of the budget;
4. the live topics lane drains (3 s), the cache watchers stop, and the NATS client drains (13 s).

The process exits 1. A runtime must allow about 26 s after SIGTERM; give it 30 s. A lease left
unreleased (the budget ran out) is taken by a standby once it expires, as after a hard stop.

## Readiness: `/healthz`

`/healthz` takes no credential and answers within 5 s (`healthzDeadline`): a check that has not
answered by then makes the answer `503 {"status":"unhealthy"}`.

| Answer | When |
| --- | --- |
| `503 {"status":"starting"}` | Until the pod can serve: NATS connected, the webhook and `/v1` gates open over warm caches, the receive loop started and, with an OIDC issuer configured, its discovery succeeded. A NATS or issuer that does not answer keeps the pod here, up and retrying; it never exits for that. |
| `200 {"status":"healthy","role":"active",…}` | The pod receives the durable. The body carries the durable's lag, `num_pending` and `num_ack_pending`. |
| `200 {"status":"healthy","role":"standby",…}` | The pod is ready, does not deliver, and another pod of the machine holds the lease: `active_instance` names it. |
| `200 {"status":"degraded",…}` | A transient KV failure the monitor and the NATS reconnect retry, or, on the active pod, a durable whose lag could not be read. Also a pod that is the machine's deliverer and does not receive yet - it holds the lease, or runs without one, or has read no holder at all, or the record still names this pod while it no longer holds it (its renewals lapsed, and no other pod has taken the record yet): nothing of the durable reaches a session until it receives, so no such answer is healthy. `role` is `standby` and `receive` says what it waits for (`preparing the durable`, or `waiting for another subscriber's push binding to end`); when its reads, creations, corrections or subscribes of the durable fail, `error` is `durable consumer unavailable: <failure>` and `receive_failing_ms` how long it has failed. It stays `200`, as a standby does, so a readiness probe keeps it in service. |
| `503 {"status":"unhealthy",…}` | A NATS flush failed (`nats unavailable`), the role lane's subscription is not valid (`role subscription inactive`), the live topics lane is disconnected or refused (`live subscription inactive`), a cache watcher stopped; on the active pod also its subscription is not valid, the durable is gone, or the durable delivers to another subject than its subscription's (`not bound to this listener's subscription`, which a durable deleted and created again under it leaves). An active pod answers `subscription inactive` for `leaseReceiveLost` (3 s) at most: the lease reads the same signal and releases the record, after which the pod answers as a standby ("Take-over" above). Also the failing durable above once it has failed for 135 s: a lease holder gives the lease up then and answers as a standby, so only a listener without a lease, which keeps retrying, stays unready. |

Every healthy answer also carries the body fields it always did (`live_topics`, `ci_cache`, the
session registry's fields). A standby is ready: route traffic to it. A pod answers `503 starting`
only while it cannot serve, so a readiness probe that sends traffic on `200` sends none too early.

A probe contract that fits these answers:

- readiness: an exec of `curl -fsS http://127.0.0.1:<port>/healthz`, `timeoutSeconds` 6 or more,
  above the handler's 5 s deadline;
- liveness: the same request without `-f`, with `--max-time 8`, so a `503` (starting, or a
  dependency down) is no liveness failure and only a process that does not answer is restarted;
- `terminationGracePeriodSeconds` 30 or more, for the graceful stop above.

## State the pods share

Every pod of a machine id reads and writes the same records, so a pod that takes over, or a
standby answering `/v1`, answers as the active pod would:

- **Delivery ledger** (`internal/dedupe`, `delivery.<durable>.<32 hex of sha256(dedupe key)>.<session>`
  in `envoy_listener`): before it prompts a session or forwards a role-lane message, a pod records
  the pair attempted, and once it lands, delivered. A pair another pod delivered is not prompted
  again. A pod skips a pair as `listener attempt-dedupe skip` only for an attempt it holds itself:
  one whose prompt may still be running (less than 35 s old and not settled), or one a delivery kept
  (`dedupe.Ledger.Keep`: a give-up, a fanout refusal) for 5 minutes. An attempted record of its own
  boot that it holds no such attempt for (a clear that could not be written, a write whose reply
  was lost) is taken over and the session prompted. An attempt by a boot that held this machine's
  lease before the current holder (the lease record's `former_boots`: a holder that was killed, lost
  the lease, or released it) is taken over at once, so the settle hold delivers that message in
  stream order. An attempt by a pod that still holds or renews a lease, or by a live pod's role or
  live lane, less than 35 s old (the 30 s prompt limit and 5 s) is retried later on the durable
  lane; an older one is taken over, since its writer died. A repeat with the same event id is
  possible after a hard take-over. A ledger that cannot be read NAKs the durable message for a
  retry; the role and live lanes then deliver without it and log `delivery ledger unavailable`.
- **Delivery exceptions** carry an id derived from the event id, the recipient session and the
  reason, so a redelivery that fails the same way publishes an exception with the same id and
  dedupe key, which the stream stores once.
- **Clean-hand-back marks** (`handback.<durable>.<revision>` in `envoy_listener`): a pod that
  stopped gracefully vouches for the lease record it released, and the pod that took that record
  reads the mark inside its settle hold ("Take-over" above). A mark nobody reads leaves the
  bucket with its 20 minute `MaxAge`.
- **Last seen** (`seen.<session>` in `envoy_listener`): every session registration write records
  when the session was last seen, and every pod answers a lapsed session's `last_seen` out of that
  record alone, so two pods never answer one session differently - a pod that watched the session
  keeps no answer of its own. The answer lasts the session TTL and the interest reaper's allowance
  past the last write (20 minutes with the default 5 minute TTL), which the bucket's `MaxAge` is
  sized to outlive; with no record in the bucket - it was never written, or it could not be read
  within the request's budget - no pod answers one.
- **Role-claim grace** counts from the machine's service start, `service_since` in the lease
  record: the server's time of the write that took the lease when no pod of the machine was
  serving (no record, or one last written more than two TTLs before). A claim stored before
  `service_since` keeps its grace for one session TTL after it; every pod answers a claim alike,
  whenever it started.
- **Registry writes** (session registrations, interests, role claims) are compare-and-set at the
  revision the pod read, merged again over the bucket's value on a conflict, at most 5 attempts,
  then `503` with `Retry-After: 1`. A pod whose cache has not seen a registration reads the bucket
  before `/v1/messages/send` or `/v1/roles/set` answers that the session is unknown.

One record is each pod's own, deliberately: the **publish route's confirmation record**
(`listenerDeps.confirmed`, `internal/dedupe`'s in-process cache), which holds each (dedupe key,
session) whose receipt arrived through `/v1/messages/send` or `/v1/messages/publish` on this pod.
Only a receipt the route itself took counts, and the delivery ledger above records what the
machine delivered rather than what a recipient confirmed, so the two are not one record. A send
retried against another pod of the machine forwards again, and the holder's receipt answers it.

## Two builds of the listener

During a roll two builds run for one machine id, and each must deliver what the other expects:

- **The durable takes every stored message.** Its filter is `>` (`consumerFilterAll`), not a
  build's own list of stream subjects, so a build whose stream carries a subject the other does
  not know still has it delivered. It is `>` and not empty because nats-server 2.10 accepts an
  update that clears a push durable's filter and then delivers nothing published after it. The
  holder writes the policy only once nothing but it receives.
- **The live lane splits by the deployed stream.** It leaves to the durable every subject the
  stream's current subject list captures, read from the server at start, every 5 s and after each
  reconnect, not the list this build compiles in. Before its first read succeeds it uses the
  compiled-in list. A stale list can only lack a subject added since, and the ledger keeps the
  session from a second prompt for it.
- **Fields a build does not know are kept.** Session registrations, interests, role claims and
  the lease record keep every JSON field they carry across every read-modify-write.

A build from before INFRA-557 binds the durable with `nats.Bind` and knows no lease. Run one beside
this build only during a roll from the old build to this one: this build's lease holder, finding
the durable push-bound when it took an absent record or a released one carrying neither
`stale_binding` nor `unsettled`, waits (`/healthz` 200 standby, `receive` `waiting for another
subscriber's push binding to end`) and subscribes within a second of that binding's end. Past
135 s it logs `lease holder cannot deliver from the durable` at ERROR, and again every 5 min, which
the AWS `subscribe-exhausted` alarm matches; it never exits for it.

An unreleased record stays in the bucket for the bucket's `MaxAge`, 20 minutes, where an earlier
build of this release kept it 10: after every pod of this build died hard, a pod starting within
20 minutes finds that record, takes it as an expired one and counts a push binding it finds as
the dead holder's rather than a pre-lease build's - the window in which that reading holds is now
twice as long. A pod starting later finds no record and waits such a binding out as a pre-lease
build's, as it always has.

Rolling back to a build before this release works: that build corrects the durable's filter to its
own list and binds once this build's pods have stopped. But a hard stop of this build's lease
holder while a build from before the release is up and polling to bind can deliver messages twice:
the old build binds as soon as the dead holder's connection is gone, and the standby that takes
the expired lease counts that binding as the dead holder's and receives beside it. A rollback that
stops every pod of this build before starting the old one carries no such risk.

## Without the grant: no-lease mode

The listener's NATS user needs the `envoy_listener` bucket in its grant (`infra/core`
`ListenerPermissions`, `DispatchPermissions` and `HostPermissions` carry it from this release).
That grant writes only the machine's own keys, `lease.listener-<machine id>`,
`handback.listener-<machine id>.>` and
`delivery.listener-<machine id>.>`, and the `seen.` records. It reads each of its buckets by key, a
direct get on the key's own subject, and refuses a get by revision, a direct get on the stream's
bare subject. So the listener reads back each write it must stamp, the lease's acquiring write and
a session registration in `envoy_sessions`, by key, and counts it as its own only at the revision
it wrote; another revision there means a newer write landed first.
A listener whose grant refuses the bucket does not exit: it logs one ERROR, `listener lease
unavailable: the NATS user lacks the grant for KV bucket envoy_listener; delivering without a
lease`, and delivers as the machine's one deliverer, receiving from the durable only while no other
subscriber holds it push-bound and waiting for as long as one does. `/healthz` says so in `lease`:
`"lease":"unavailable: missing NATS grant for KV bucket envoy_listener"`. The shared state above
falls back to the pod's own: the ledger is in process, last-seen is in process, and the role-claim
grace counts from the pod's own start. Run one pod per machine id in this mode: it has no fencing
against a second deliverer after a reconnect in place.

Deploy prerequisite: render and apply the NATS grant carrying `envoy_listener` (for a
`gcp:NatsHub`, its users; for a host, its NATS authorization) before running a second pod of a
machine id.

## Port-session delivery addresses

Every pod of a machine computes a session's address the same way (`session.Deliverer.hostFor`,
from the registration and `ENVOY_HOST_BRIDGE`), and every answer reaches the session from any pod
but two, which are local to the pod that dials them: a loopback bridge (`ENVOY_HOST_BRIDGE` such as
`127.0.0.1`) and the `host.docker.internal` fallback. The subscribe handler stamps a port
registration with `accepted_host`, the accepting pod's host name. A pod that would dial a
pod-local address for a session another pod accepted sends nothing: it reports the delivery as
`delivery_failed` (the log names `address not reachable from this listener pod`) and does not
retry. A registration without `accepted_host` (written by an earlier build) is delivered as
before, and pods that share a host name (host networking) deliver normally. A session on a serve
port that needs every pod to reach it registers a host every pod reaches.

## Environment

The settings every listener reads are listed on the docs site's Envoy page ("Running the
listener"). These concern the pods of one machine id:

| Variable | What it is |
| --- | --- |
| `ENVOY_MACHINE_ID` | The machine id. Every pod with the same id shares one durable, one lease and one queue group; two machines whose ids collide silently become each other's standby. |
| `ENVOY_INSTANCE_ID` | The pod's instance name: the lease record's `holder`, `/healthz`'s `active_instance`, the `instance` field of every JSON log line, and the NATS client names (`envoy-listener <instance>`, `envoy-live-topics <instance>`, `envoy-listener-lease <instance>`). Default: the host name, the pod's name in Kubernetes. Go's text log lines carry no instance prefix, since the deployed text metric filters match by position. |
| `ENVOY_POD_HOSTNAME` | The host name stamped as `accepted_host` and compared against it. Default: the host name. Two processes on one host set it to stand for two pods (the tests). |
| `ENVOY_NATS_REPLICAS` | Replicas of every bucket the listener creates, `envoy_listener` included. |

## Recreating the durable

Recreating `listener-<machine id>` (to drop a setting NATS cannot change in place, as `subscribe
refused, shutting down` names) keeps its cursor only while no pod of the machine runs: stop every
pod of the machine id first, active and standby. A lease holder that finds the durable missing
creates it at deliver policy `all`, which replays every retained message, and a standby takes the
lease and does the same when the active pod stops. Then recreate it as `packages/envoy/AGENTS.md`
(Operational notes) shows and start the pods; the lease holder stamps the rest of the policy onto
it and subscribes.

## Tests

The two-process and two-build tests start real listener binaries against one in-process NATS
server, each behind a relay that can cut, freeze or delay its link. They carry the build tag
`multiprocess`, and CI runs them in the `listener-multiprocess` job of
`.github/workflows/ci.yaml`, which runs only when a file its `listener_multiprocess` filter there
names changes (a change that makes these tests compile a new package or read a new file adds it to
that filter, which `.github/scripts/check-ci-filters.sh` holds them to):

```bash
cd packages/envoy
go test -tags multiprocess -count=1 -timeout 20m -v -run '^Test(TwoPods|TwoBuilds)' ./cmd/listener
```

`TestTwoBuildsSkipOrDoubleNoMessage` also runs the first-roll direction against a build from before
the lease when `ENVOY_LISTENER_OLD_BINARY` names one:

```bash
ENVOY_LISTENER_OLD_BINARY=<path to that build> \
  go test -tags multiprocess -count=1 -timeout 20m -v -run '^TestTwoBuilds' ./cmd/listener
```

The lease, receive and ledger rows that need no second process (`lease_test.go` and the
`lease_*_test.go` files beside it, `receive_test.go` and the `receive_*_test.go` files beside it,
`ledger_test.go`) run with the package's other tests.
