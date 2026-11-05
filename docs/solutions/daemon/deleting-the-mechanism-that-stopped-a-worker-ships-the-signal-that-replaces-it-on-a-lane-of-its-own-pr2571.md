---
title: "Deleting the mechanism that stopped a worker ships the signal that replaces it, on a lane of its own, finishing when nobody is listening"
category: daemon
tags:
  - outbox
  - role-notices
  - lanes
  - mechanism-removal
date: 2026-10-31
status: active
module: packages/daemon/internal/record
related_issues:
  - "sjawhar/legion#2571"
---

# Deleting the mechanism that stopped a worker ships the signal that replaces it, on a lane of its own, finishing when nobody is listening

When you remove the thing that used to interrupt, suspend or abort a worker, something else has to
tell that worker its world changed — the interruption was the notification. Ship that message in
the same change, and decide three things explicitly:

- **Who is addressed.** The replacement goes to the worker whose work ended, not only to whoever
  supervises it. A notice on the supervisor's topic tells the wrong party.
- **Which lane.** Put it on a delivery lane of its own, not the lane the supervisor's notices
  already use. Lanes are ordered within themselves, so one undeliverable notice ahead of it would
  hold the news until it is no longer news.
- **What an undeliverable one does.** Separate "nobody holds this address" from "the transport
  failed". No holder is *finish the row and log it once*: the information has no value later, and
  the worker's own next task says the same thing. Every other failure keeps the ordinary retry.

State in the message that nothing of the worker's is stopped, and what it must stop doing anyway
(writing a shared workspace the next phase now owns), because the worker has no other way to learn
that the removal changed its obligations.

Prove the no-holder case as its own step of the live proof: an address no session holds, one
publish attempt, no retry over a settle window, and the row gone from the queue — with the
positive fact beside it, that the replacement path still acted.

## Evidence

- `packages/daemon/internal/record/outbox.go:38` adds `OutboxKindPhaseEnded`, and `:62`/`:70` put
  it alone on `LaneRoleMessage`, whose comment records the reason: the board lane's architect
  notices are ordered among themselves, and one of them waits as long as its architect holds no
  role, while a role told that its phase ended must hear it while that is news.
- `packages/daemon/internal/daemon/outbox.go:749` finishes the row on `notify.ErrNoHolder` with one
  log line; the other publish failures keep the retry.
- `packages/daemon/internal/daemon/ci_red_no_takeover_live_test.go:573`
  `noHolderFinishesTheRow` is the live proof's step 5: the reviewer's topic answers `404 unclaimed`,
  one publish attempt is made, no `phase_ended` row is left in the outbox, nothing is delivered,
  and the implementer is still started.
- What was removed: the CI-red takeover, its wait for the outgoing turn, the RPC `abort` frame,
  `runtime.Conn.Abort` and `supervise.Machine.Quiesce`. Before the removal the abort itself was how
  a tester, reviewer or merger learned its phase had ended.
