---
title: "A JetStream answer is not the outcome: a sequence-0 duplicate stored nothing, and a timed-out update may have applied"
category: envoy
tags:
  - nats
  - jetstream
  - deduplication
  - publish
  - stream-update
date: 2026-10-28
status: active
module: packages/envoy/internal/bus
---

# A JetStream answer is not the outcome: a sequence-0 duplicate stored nothing, and a timed-out update may have applied

- **A publish acknowledged `Duplicate` with `Sequence == 0` stored nothing.** Treat it as an error,
  never as a success, and send the same envelope once more **without** its `Nats-Msg-Id`. If that
  send fails too, return an error that wraps both, so the sender retries. A real duplicate of a
  stored message is acknowledged with its real sequence, so the zero is the tell.
- **A copy sent without its id is not deduplicated by the stream.** Every consumer of the stream
  must drop a repeat itself, by the envelope's event id or dedupe key. Check each consumer before
  you rely on the resend, and name in the pull request body which key each one drops by.
- **A stream or bucket update that timed out may still have been applied.** On a timeout, read the
  stream again and count the update as done only when the stream now holds what the update asked
  for; otherwise return the update's own error. Use one helper for this, keyed on the update's own
  stream name, so the stream and its buckets share the rule.
- **A permission violation on a JetStream request arrives as a timeout, not a refusal.** The
  read-back above does not hold for it, so the timeout is returned; name the resource in the error
  (`raise KV_<bucket>: …`), because the server's answer names nothing.

## Evidence

- nats-server v2.10.29, clustered, records a publish's `Nats-Msg-Id` at sequence 0 on the stream
  leader before the store runs (`jetstream_cluster.go:7995`). When the store then refuses the
  message (a full `DiscardNew` stream, error 10077 `maximum bytes exceeded`), nothing removes that
  record, and it stays for the whole duplicate window (72 hours here). Every retry under the same
  id is answered `{"seq":0,"duplicate":true}` with no error. A purge does not clear it; only a
  stream leader step-down does. A single server and nats-server v2.15.1 do not stage the id.
- `packages/envoy/internal/bus/nats.go` (`ErrNotStored`, `notStored`, the resend in
  `publishJetStream`) and `async.go` (`sendJetStream`, `acknowledged`) carry the rule;
  `publish_not_stored_test.go` refuses GitHub deliveries on a full three-server 2.10.29 cluster,
  purges, and requires both paths to store them. It fails with either resend removed
  (sjawhar/legion#2362, commits e572c5be3cba and ae6ca339075c).
- The consumers of `ENVOY_NOTIFICATIONS` drop a repeat differently: sessions by event id or dedupe
  key, Dispatch's delivery intake by upserting its rows, the Legion daemon by event id only. A
  GitHub redelivery gets a new event id at the webhook, so only the first two drop it.
- Two concurrent replica raises on nats-server v2.10.29 can leave one unanswered
  (`TestTwoStartsRaisingOneStreamAtOnceBothSucceed`); `updateStream` in
  `packages/envoy/internal/bus/stream.go` reads the stream again and succeeds when it holds the
  change (commit fcd91e242074).
