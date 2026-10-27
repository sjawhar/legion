---
title: "Answer a fact from the record that owns it, not from a copy kept in step with it"
category: envoy
tags:
  - role-claims
  - derived-state
  - nats-kv
  - consistency
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-141"
  - "sjawhar/legion#2167"
---

# Answer a fact from the record that owns it, not from a copy kept in step with it

Extends docs/solutions/envoy/unsubscribe-resurrection.md.

That note gives the write order of a role claim, and that order still holds. Its last sentence
about a failed cleanup ("return the error") no longer does: since INFRA-141, a claim that landed is
a success, and its cleanups are best-effort.

- **When two records hold the same fact, find the one that owns it**: the one written with a
  compare-and-set, the one that decides a conflict. Answer every read of the fact from that record.
- **Treat the other record as a copy that may be stale.** Its writes are best-effort tidying. A
  copy that a cleanup leaves behind must give nobody anything.
- **Do not add a reconciler, a reaper or a compensation chain to keep a copy in step.** Each of
  them races with the owner's writes, and each race gives a wrong answer that needs more
  machinery. If a review finds a second defect in the code that keeps the copy in step, stop
  fixing it and move the reads to the owner.
- **When the reads move, delete the code that kept the copy in step**, and grep that its symbols
  are gone.

## Evidence

- A role claim (`envoy_roles`, a compare-and-set entry) names the session that holds a role. Each
  holder's interest record also carried the role's topic. Role delivery read the claim, but
  `GET /v1/sessions`, `GET /v1/registry/<session_id>` and a sent message's `sender.roles` read the
  topics. Review 5655062345 (at cc183a6f, thread `PRRT_kwDORFy7ds6wV7Ar`) found that the claim's
  rollback and the old holder's cleanup were never sent once the request's context had ended, so a
  topic stayed behind and its session looked like a holder.
- 74318aa8 kept the topics in step: it ran the claim's cleanups detached from the request, and
  added a role-topic reaper that removed a topic only when two passes saw its record at the same
  revision. Review 5656109351 (at 0c227469) reopened two threads and opened five more against that
  machinery: the reaper never removed the topic of a live session, because each client heartbeat
  rewrites its record; the reaper's test asserted that defect as a pass; the rollback could strip
  the topic of the session the claim finally named; a detached cleanup logged a false failure;
  and the text described the reaper wrongly.
- 757f5eff moved every read to the claims (`store.Registry.RolesHeldBy`, read by
  `sessionInfoFor` and `senderStamp`) and deleted the reaper, its guard and the detached cleanups:
  a `git grep` for their symbols matched 21 lines of `internal/store/kv.go` at 0c227469 and none
  after. The remaining race can still strip a topic, but no answer depends on the topic.
  `TestARoleIsHeldByTheSessionItsClaimNamesWhateverTheRecordsTopics`
  (`cmd/listener/roles_from_claims_test.go`) fails when roles are read from topics again.
- Reading the owner brought its own condition: the claims view must be complete before it can say
  that a session holds no role (`kvwatch.Watcher.Complete`, `store.ErrRoleViewNotReady`). That
  condition is in the code and its comments; it is the cost of the move, and it is smaller than
  the reconciler it replaced.
