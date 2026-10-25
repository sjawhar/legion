---
title: "An advisory read on a write's transaction: savepoint, bound, reset, and a field that is absent rather than false"
category: database-issues
tags:
  - savepoint
  - statement-timeout
  - set-local
  - postgres
  - transactions
  - advisory-query
  - omitempty
  - recursive-cte
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "INFRA-184"
  - "sjawhar/legion#2157"
---
# An advisory read on a write's transaction: savepoint, bound, reset, and a field that is absent rather than false

A hint computed for a response, inside the transaction of a write that must succeed without it.

- Run it in a savepoint of that transaction, and answer `nil` on every error; never return the
  error to the caller. A failed statement aborts the transaction it ran in, so a hint run
  directly on the write's transaction turns the write's `COMMIT` into a rollback.
- Bound it with `set local statement_timeout` inside that savepoint. An advisory read with no
  bound holds the write's row locks and a pool connection for as long as the plan takes.
- `SET LOCAL` survives `RELEASE SAVEPOINT` and is undone only by `ROLLBACK TO SAVEPOINT`. So
  reset the bound **before** the release, not after, or the rest of the write silently inherits
  it. A savepoint that only reads can be closed with `ROLLBACK TO SAVEPOINT` instead, which
  reverts the bound for free and removes the reset statement and its error branch.
- `set local statement_timeout = default` restores the role, database or server default — not
  whatever the enclosing transaction had set. Where an outer transaction may set its own bound,
  save and restore the value instead of writing `default`.
- A bound set in an outer savepoint covers statements in a savepoint nested under it, so a nested
  advisory read needs no bound of its own.
- Pin the reset with a test that reads `current_setting('statement_timeout')` after the helper
  returns, or forces a later statement to run longer than the bound. Without one, deleting the
  reset leaves the suite green and only slow production writes fail, with `SQLSTATE 57014` on a
  statement that never asked for a bound.
- Make the field three-state on the wire — a pointer with `omitempty`, absent on every failure —
  and have every reader compare `=== true` (or `!== true`). A plain `bool` serialises `false` and
  states something false about the subject. Name the cases where it is absent and pick the safe
  direction for absent, because a reader gets one bit and the server's logs are the only place
  the cases are distinguishable.
- Assert absence in Go with a raw decode (`map[string]json.RawMessage`) or a pointer field. A
  typed `bool` decode cannot tell an omitted field from a stored `false`, so the test that was
  meant to pin the omission passes either way.

Detaching the advisory read's context is not what protects the write.

- `context.WithoutCancel` on one statement inside a transaction whose other statements still use
  the request context buys nothing: pgx refuses every statement on a context that is already
  done, so the write's own `INSERT` and `COMMIT` fail anyway once the caller hangs up.
- What makes a cancellation harmless is ordering — run the advisory read **before** the write, so
  a cancellation inside it means nothing was written to lose. Where the write genuinely must
  survive a disconnect, detach the whole decision, as
  `docs/solutions/daemon/a-handler-side-hook-that-writes-the-store-takes-the-decision-context-not-the-request-context-LEGION-663.md`
  does; this document extends that one for the single-statement case, which is not the same
  thing and does not substitute for it.

A recursive parent-chain walk added beside others:

- End it with the depth predicate, not `UNION`. Each row carries its own `depth`, so a cycle
  yields distinct rows and `UNION` de-duplicates nothing; drop the predicate and keep `UNION` and
  the walk does not terminate.
- Choose the over-cap policy deliberately and say which you chose. Sibling walks in one package
  disagree: a repository walk fails closed (marks the chain too deep and refuses the read), while
  an advisory walk fails open (answers false, so the ordinary text prints). A copy-pasted walk
  inherits whichever it was copied from.
- A walk inside a transaction that locks only the subject's own row reads the ancestors as they
  happen to stand. That is right for a hint and wrong for anything that enforces: enforcement
  needs the lock every reparent takes.
- Test the cap's boundary in the language's own suite, not only in a harness script. An
  off-by-one in `depth < cap` is invisible to tests two links deep.

## Evidence

INFRA-184 made three `dispatch` texts true for the agent that reads them. Two needed the same
predicate — does this issue, or any ancestor, carry the `legion` label — inside two different
writes: a claim and a write's advice.

The claim path first ran the walk between `appendEvent` and `COMMIT`; review round 2 moved it
before `writeIssueClaim` (`packages/envoy/internal/dispatch/api/issue_claim.go`), which removed
the cancellation window rather than making it safe. The reviewer had required a test that
cancels inside the walk and asserts the claim committed; after the move that assertion is not
constructible, because no claim has been written at that point, and the review accepted the
absence on that ground, recorded in the pull request's risk list. The fresh-eyes read of the
retro then showed the remaining `context.WithoutCancel` is not what saves the claim: pgx refuses
statements on a done context (`pgconn.go` in v5.11.0), so the claim's own writes fail regardless.

The advice path's walk is nested inside the advice savepoint and relies on the outer 500 ms
bound. That reliance is proven rather than asserted: the test's injected failure sleeps 2 s and
can only time out because the outer `SET LOCAL` reaches inside the nested savepoint
(`write_advice_test.go`).

Both paths reset the bound before releasing their savepoint, and nothing in
`internal/dispatch/api` reads `current_setting('statement_timeout')`, so deleting either reset
would keep the suite green — the gap this document exists to close for the next author.

`legion_managed_status` is absent in four distinct cases: a write past triage, a non-session
actor, a failed advice walk, and a failed or timed-out claim walk. Every client therefore reads
it `=== true`, and the Go tests use raw-JSON helpers (`rawClaimFields`, `rawAdviceFields`) so
that an omitted field cannot pass as `false`.
