---
title: "A restart of a phase entry reuses the token that entry issued; only a start that hands new work mints one"
category: daemon
tags:
  - at-most-once-delivery
  - correlation-token
  - retry
  - workflow
date: 2026-10-31
status: active
module: packages/daemon/internal/workflow
related_issues:
  - "sjawhar/legion#2571"
---

# A restart of a phase entry reuses the token that entry issued; only a start that hands new work mints one

When a per-attempt token (an assignment id, an attempt id, a lease id) is minted by the code that
hands a worker its task, and a completion is held to it, split that code in two before you write
the second caller:

- **Mint** only where the worker is given work it was not already on.
- **Reuse** the value the row already holds wherever the worker is still on the same work: a retry
  of a held phase, a resume, a promotion, a re-look, a boot repair.

The test that tells them apart is not "is this a start?" but **"will the queue deliver this worker
a second copy of the task?"** Under at-most-once delivery it will not — the queue skips a claim
that still holds an unconfirmed task — so a freshly minted token reaches the row and never reaches
the worker, and from then on every completion that worker sends is refused against a value nothing
ever delivered. The refusal is permanent and silent: the worker is working, the row looks right,
and only the comparison between them is wrong.

Write the two entry points with names that carry the rule (`start` mints, `restart` reads the row),
and put the list of the callers that must not mint in the doc comment of the minting function, so
the next caller added reads it.

Prove it at the route, not at the mint: drive the held phase's retry and assert the start carries
the id the phase already handed out. The negative control is the one-word edit that makes the
restart call the mint, which must fail naming both ids.

## Evidence

- `packages/daemon/internal/workflow/effects.go:246` `start` mints through `assign`;
  `:266` `restart` reads the role's `phases` row and mints nothing; `:279` `started` is the enqueue
  both share. `engine.go:743`'s `retryOrEscalate` calls `restart`.
- Before that split, the architect's or operator's retry of a held phase minted a new assignment
  while the daemon's outbox returned early for the failed claim, which still held the phase's
  unconfirmed task (`packages/daemon/internal/daemon/outbox.go`, "not delivered again"). The
  relaunched worker went on serving the old assignment, and every completion it sent was refused
  `HANDOFF_STALE_ASSIGNMENT` for good. A reviewer found it by reading the two paths together; no
  test drove the retry.
- `packages/daemon/internal/api/handoff_assignment_test.go`'s
  `TestAHeldPhasesRetryKeepsTheAssignmentItsWorkerStillServes` drives it through the daemon's own
  completion route over a real PostgreSQL. Negative control, run at the head: `retryOrEscalate`
  made to call `start` →
  `the retry's start carries assignment "NIPJS3JMWZ7XNCJM7WAQ7OD5XG", want the "NVYQV7THNZ57VS562YWJYAS2GK" the held phase handed the tester`.
