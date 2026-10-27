---
title: Event outbox
parent: dispatch-server
depends_on: [contracts, dispatch-docs, dispatch-events, dispatch-model, dispatch-store, dispatch-text]
paths: [packages/envoy/internal/dispatch/outbox]
---
The publisher that turns each appended event into deliveries: its owner's topic, its route and its author and follower routes. One process publishes at a time, under a hold it renews. It sends each owner's events to each destination in order, retries a failed destination with exponential backoff, and records what a batch came to in one statement. It is the one package of the server that imports the generated Go contracts, because the envelope it publishes is the contract.

## One publisher, and take-over

Every envoy-dispatch process runs an outbox (`outbox.New`, `Run`). Only the process holding the publisher hold reads and sends events: `dispatch-outbox`, a `duty.Hold` row in `duty_holds` (migration `0121`). The hold lasts 6 s on the database's clock after each renewal, is renewed every 2 s, and reports itself lost 2 s before it lapses on the process's monotonic clock. The hold is taken and renewed on the outbox's own pool (`store.Pool.Outbox`, two connections), so a renewal never waits behind a batch statement or behind the API's requests on the shared pool. The others stand by.

- **Killed holder.** Its row lapses 6 s after its last renewal. A standby arms one timer to that moment (`duty.Expiry` reads what is left on the database's clock), takes the hold when the timer fires, and scans at once.
- **Stopping holder.** It takes no further batch, finishes and records the one in flight, and releases the hold. The release deletes the row and sends `pg_notify('dispatch_duty_holds', 'dispatch-outbox')` in one statement, which wakes a standby at once.
- **Holder without NATS.** It releases the hold when its client has been disconnected for 3 s without a break. It also releases the hold when a batch's sends fail for the connection (`bus.ErrUnavailable`, or `bus.ErrNotAcknowledged` on a connection still current) and a 2 s flush probe of that window's connection fails too. That probe is what finds a silent cut, a connection that still reports itself connected. A client that has already reconnected since the batch began keeps the hold and resends on the new connection.
- **Standby.** It tries the hold only while its own NATS client is connected. A process that gave the hold up for NATS loss also first needs an answered probe of its connection before it tries again, so it does not take back the hold it gave up for that same silent connection ahead of a standby whose NATS works. If both processes lose NATS, neither holds until one reconnects.

`/healthz` reports `outbox_publisher`: `true` while this process holds the hold and its loss deadline has not passed, and `false` on a standby, after a loss, and with NATS disabled. It never decides `ok`.

## Fencing

A process publishes only while it holds the hold and its loss deadline has not passed. Every send of a batch checks the deadline first (`fencedSender`), and so does the record. So a process frozen past its deadline (SIGSTOP, a long pause) sends nothing and writes nothing when it resumes, and its batch is left unrecorded for the next holder. The deadline is Margin ahead of the row's lapse, so the row is still this holder's whenever the check passes. The record writes what is true whoever holds the hold, regardless of the hold: destinations reached, and `published_at` once every destination is reached. It writes back-offs, hold-backs and releases only while the hold row still names this holder and has not lapsed, checked in the same statement. A record that finds the hold another's drops the hold without releasing it. A copy an old holder sent before it stopped lands as a duplicate, never ahead of a later event of its destination, because the new holder sends that event again first.

## Wake

Nothing scans on a period. One one-slot wake channel is signalled by:

- a LISTEN connection (`pglisten`, dialled through `store.Pool.Listener`) on `dispatch_events`, which migration `0107` notifies once per statement that inserts events, and on `dispatch_duty_holds` for this hold's release. Every new connection signals too, to catch up on what it missed while it had none;
- the in-process events broker, so this process's own commits are seen while the LISTEN connection redials;
- every change of the NATS connection (`bus.Client.ConnectionChanged`): a holder resends what a lost connection released, and a standby tries the hold once connected;
- the holder's one retry timer, armed after each scan to the earliest due `next_attempt_at`, read as a duration on the database's clock. After a scan that failed it is armed to `retry.BaseDelay`.

The holder scans on every wake. A standby only tries the hold: on a wake, on its timer to the current hold's lapse, or a second later when the database or its NATS probe did not answer. A scan takes one batch after another while the last one published anything or was full.

## The batch read

`readBatch` writes no row: with one publisher, no row needs a claim. In one statement it chooses up to 100 owners that have a due event: the lowest-id events never tried (`events_outbox_fresh`) and the earliest back-offs that have fallen due (`events_outbox_retry`). It then reads each chosen owner's unpublished events in id order, due or not, up to 100 per owner (`events_outbox_owner`, on `coalesce(issue_key, artifact_id::text, project_key)`). The batch keeps at most 100 due events. An owner is an issue, a document or a project. All three indexes are partial (migration `0122`) and exclude published rows and pending subscription commands. So a read never walks past backed-off rows of owners the batch does not choose, or past commands that are never published. An event with no owner (session-addressed, seq 0) has no destination and is marked published.

The followers and authors a batch routes to are read once per batch: one set-based statement per kind (ask followers, comment authors, message authors, document authors), so at most 4 lookup statements whatever the batch's size (`loadLookups`).

## Order

Order is kept per (owner, destination key). A destination key is the subject family a session receives on: the owner's topic family under `notifications.dispatch.` is one, and each `notifications.agent.<session>` subject is one, whichever route reaches it. A role route's key is its holder's agent subject. `planChains` builds one chain per owner and key, in id order. `sendChains` sends every chain through one window, and sends a chain's next envelope only once the answer to the one before it came back as reached. So no two sends of one (owner, destination key) are ever in flight, and different owners and keys proceed side by side.

A destination that stalls (a send that failed, or an event waiting out a back-off) holds back that destination's later events of that owner and nothing else. Such a held-back event is recorded `blocked`: it falls due when the earliest of its blockers does, with no attempt counted, and is read again beside its blocker. A blocker of the same batch that was released releases the event it held back too. An event that cannot be read at all (a scan or decode failure) is backed off and holds nothing back. It is the only event that stops blocking without being published.

Residual: a session that subscribes to an issue's topic family and is also routed to by its events receives on two keys. If event N's topic publish fails while N+1's agent publish succeeds, that session sees N+1's first copy before N's. Both are JetStream publishes on the same stream, so this takes a stream failing one subject and not the other.

## Role routes

A `role:<id>` route is not published to `notifications.role.<id>`: nothing of Dispatch's crosses `notifications.role.*` any more. The outbox resolves the role's holder itself with `roles.ResolveHolder`, the resolution the listener's role lane also uses, so the two cannot drift. It resolves once per role per batch, reading the role and session buckets directly (`roles.Buckets` on the outbox's bus client). The outcomes:

- no claim: the destination counts as reached, nothing sent;
- a claim whose holder's session is gone: the claim is released (`ReleaseExpiredRoleClaim`, a compare and set) and resolved again, at most twice. Once released, or found already gone, the destination is reached with nothing sent. A claim the store kept, or one that churned past that bound, stays pending;
- a read that failed, or a holder that has not heartbeated within `session.ClaimStaleAfter`: pending, backed off with an attempt counted and retried;
- the holder is the event's own actor session (the listener's echo skip): reached, nothing sent;
- otherwise a forward to `notifications.agent.<holder>` with a window `Request`. It carries the envelope a listener forward carries: topic `notifications.role.<id>`, dedupe key `envoy.role.forward.` plus the event's key (`contracts.RoleForwardDedupePrefix`), event id unchanged. It is sent under a `Nats-Msg-Id` of that key and subject (`bus.ForwardMsgID`), so the stream stores one copy however often it is sent.

The forward's receipt decides it, within 5 s. A self-subscribed holder answers with an empty receipt. A holder with a serving port never answers the request itself, so the stream's acknowledgement of the stored copy counts as its receipt. That holds once dispatch://INFRA-594 makes the listener deliver to such holders from the stream. No receipt leaves the destination pending and retried. The host plugins apply their receipt and dedupe to the forward unchanged.

## Failure classes

Once every answer of a batch is in:

- **released**, with no attempt counted and due again at once: a send that met a lost or replaced connection (`bus.ErrUnavailable`), the hold's deadline or the process stopping. Released events are sent again on the reconnect's wake. When the batch's connection was replaced since it began, or its probe failed (NATS lost, above), the not-acknowledged sends are released too;
- **backed off**, with the attempt counted and the error recorded in `events.publish_error`: a send NATS denied (`bus.ErrPublishDenied`) or did not acknowledge on a connection still current (`bus.ErrNotAcknowledged`: no acknowledgement in time, no responders, no receipt from a role's holder), a topic the stream does not capture (`bus.ErrNotCaptured`), a role route left pending, or an event whose destinations could not all be resolved. The delay is `retry.Delay` of the attempt count, capped at 5 min, added to the database's clock (`now() + make_interval(...)`);
- **reached**: a send NATS refuses the same way however often it is sent (`bus.ErrRefused`: past the max payload, or a subject NATS would not take). It is logged once (`dispatch outbox: destination refused`) and the event goes on to its other destinations.

## The record

`recordBatch` writes a batch's outcomes in one statement, so all of it or none, and an event row is written once per attempt. It appends each reached destination to `published_destinations`. It sets `published_at` and clears `publish_error` once every destination is reached. Behind the hold check of "Fencing" above, it writes back-off (`next_attempt_at`, `attempt_count`, `publish_error`), hold-back (the earlier of its blocker's delay and due time) and release (`next_attempt_at = null`). Every row is guarded on `published_at is null`. A record that fails leaves the batch as it was read, as a process that dies between NATS's acknowledgements and the record does. The next scan or the next holder sends it again: JetStream drops a repeat by MsgId inside its 72-hour duplicate window, and a role's holder drops a repeat forward by its dedupe key. Delivery is therefore at least once, and each receiver takes each event once.
