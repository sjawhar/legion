---
title: "Taking a single-process service to several: the per-request cost model, the subscribe-before-snapshot order, and the consumers that read one answer as the fleet's"
category: architecture-patterns
tags:
  - multi-instance
  - fan-out
  - listen-notify
  - rate-limiting
  - cache-invalidation
  - secrets-broker
  - boot-order
date: 2026-10-25
status: active
module: packages/envoy/cmd/broker
related_issues:
  - "INFRA-236"
  - "sjawhar/legion#2162"
applies_when:
  - "a service that ran as one process is about to run as several against one database or store"
  - "a route is about to broadcast an invalidation to every instance"
  - "a cache in each instance is invalidated over LISTEN/NOTIFY, a KV watch, or a pub/sub topic"
---

# Taking a single-process service to several: the four audits

Running a second copy of a service changes three things no diff shows: what each request costs the
fleet, what an instance knows at the moment it becomes reachable, and what a caller's success
answer means. Do these four audits at plan time. On INFRA-236 the first two were missed by the
plan and found by review, each costing a round.

- **Write the per-request cost formula before sizing anything.** Fleet cost is
  `instances × per-instance rate × fan-out width`. A route that broadcasts to every instance has
  width `N`, so the fleet spends `N² × rate` — one accepted request is a store read on every pod,
  and every pod can accept. A plan that says "halve the buckets because there are two pods"
  divides by `N` and overspends by `N` at any size. Divide the **rate** by `N²`; do not divide the
  **burst**, which is a depth that admits one person's write sequence and has a hard floor, where
  the rate is a sustained cost against a provider quota. Sizing the hard floor by the soft cost is
  the error this corrects.
- **Make `N` an input, or log the `N` you assumed.** A constant derived from a replica count that
  only appears in prose silently overspends when someone scales out. If the count lives in another
  repository, log it at boot next to the other derived budgets, so a reader of the log can see the
  assumption the limits were built on.
- **Subscribe before you snapshot, and gate on a confirmed subscription.** The order is: subscribe,
  wait for the confirmation that the subscription itself exists (the hook that fires after the
  `LISTEN` statement returns, not the one on dial or connect), read the snapshot, then become
  reachable. A notification committed before the subscription is delivered to nobody, so a read
  taken before it can never be invalidated and the instance serves stale state until its periodic
  reload — on every boot, which is what a rolling deploy is. Where the snapshot necessarily comes
  first, take a second one after the subscription is confirmed: it is one extra read per boot and
  it restores the ordering.
- **A read-your-writes assumption does not survive this move.** A snapshot read is usually a
  listing, and listings lag: Secrets Manager documents five minutes, Secret Manager documents up to
  six hours for everything but reading a version back by number. The ordering audit above fixes
  *which read happens first*; it does not make either read fresh. Say which of the two you have.
- **Audit every consumer that reads one instance's answer as the fleet's.** A synchronous 200, an
  exit 0, a CLI that "confirms" a change — each was a statement about the whole service when there
  was one process and is now a statement about the one that answered. Grep for them before
  shipping. Where a procedure uses such an answer as permission for an irreversible act, the honest
  fix is usually to remove the need for it rather than to make it a better gate.
- **Invalidate, do not push.** A cross-instance message that carries only a key and a sender, with
  the receiver re-reading from the source of truth, costs at most a read for an untrusted payload
  and cannot carry a secret. Bound what the receiving side will do with one: a refresh that spends
  no token is unlimited work for anyone who can reach the channel.

## Evidence

INFRA-236 made the Envoy secrets broker safe to run as more than one pod
(`packages/envoy/cmd/broker`, sjawhar/legion#2162).

- The plan's sizing decision read "halve both buckets in both dimensions", the `N` model. The
  tester's first round measured the fleet at **7.64 store reads/s against a 3.82 budget** — the
  `N²` the plan had not written down. `internal/broker/api/limits.go` now carries the formula, and
  `TestTwoBrokers` row 7 measures the fleet's real traffic against it.
- Halving the shared bucket to a burst of two broke an end-to-end test that writes four secrets in
  sequence; the burst's floor is a human's write sequence, not a fraction of a quota. The burst is
  undivided at ten.
- The boot bound its socket after waiting for the listener's **dial**, and the `LISTEN` statement
  had not necessarily run. Fixed to a hook that fires after `LISTEN` returns. The next round found
  the deeper order: the first whole-namespace load ran *before* the subscription, so a retag a peer
  served during that load was lost until the five-minute reload. A FIFO-fed two-process drive
  showed the booting pod serving the old tags **10 times out of 10** before the fix and the new
  tags 10 of 10 after it.
- `agent-secrets secret retag` exits 0 on the answering pod's view, and the operator guide used
  that exit 0 as permission to store an App key or a Google login. A peer that reads stale tags
  *successfully* logs nothing, so no second exit 0 and no clean log can establish the fleet. The
  guide now leads with "create the secret with its kind from the start, under a name no agent holds
  a grant on", which removes the question instead of bounding it.
- The notification payload is a name and a sender; the receiver re-reads. A notification-driven
  reread deliberately spends no rate-limit token, so the fleet's arithmetic holds for honest
  senders only — recorded here because that is the trade, not an oversight.
- The listing lag the ordering fix cannot close is tracked at `dispatch://INFRA-375`.
