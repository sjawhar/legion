---
title: "When acceptance depends on which GitHub App acts, the phase running as that App must manufacture the case itself"
category: legion
tags:
  - legion
  - github-apps
  - review
  - acceptance
  - e2e
  - review-threads
date: 2026-09-13
status: active
module: legion
related_issues:
  - "LEGION-34"
  - "sjawhar/legion#1003"
symptoms:
  - "an acceptance criterion reads 'App X can act on App Y's artifact' and the tester, running as App X, cannot produce App Y's artifact"
  - "the tester's E2E proves the mechanism against artifacts the same App created, so the cross-App case reaches production unproven"
---

# When acceptance depends on which GitHub App acts, the phase running as that App must manufacture the case itself

## Context

Legion runs every role but the reviewer as the implement App (`legion-implementer[bot]`); only the
reviewer runs as the review App (`legion-reviewer[bot]`) — `appRoleForLegionRole` in
`packages/daemon/src/daemon/api/github.ts`. LEGION-34's acceptance 2 was "a thread the **reviewer**
opened and accepted ends resolved by the implementer's `legion threads resolve`". The implementer and
the tester both run as the implement App, so every thread either of them opens is an
implement-App-opened thread: their live runs (sjawhar/legion#1003, E2E steps 1–4) proved the
mechanism — read, accept-rule, one `resolveReviewThread` per thread, `isResolved` flipping on
GitHub — but on the wrong identity. The one case the feature exists for (the review App opens, the
implement App closes) cannot be produced by any phase but the review phase.

## The pattern

Plan the probe into the review round, not into the tester's E2E:

1. **The planner names it.** The plan's review step says: submit the first review with at least one
   inline `comments[]` entry — a real finding if there is one, otherwise a deliberate probe on the
   file under test whose body says what it is for (`Acceptance-2 probe: this thread is opened by the
   review App and must end resolved by the implementer's run of legion threads resolve.`). A probe
   is a `COMMENT`-class finding: it never turns a clean review into `REQUEST_CHANGES` on its own.
2. **The reviewer answers it in the machine-readable form** on re-review — here
   `Accepted: not a defect — acceptance-2 probe; …` — and nothing after it.
3. **The other App's phase runs the mechanism against it** at the point the process already
   requires (the implementer, before its next push) and pastes the output into the PR body. On
   #1003 that was `resolved https://github.com/sjawhar/legion/pull/1003#discussion_r3998955772`
   next to `left open …#discussion_r3998955769 — newest reply by legion-reviewer is not an
   acceptance` for the still-open blocking thread — positive and negative control in one run.
4. **The reviewer verifies on GitHub, not in the body** (`gh api graphql … reviewThreads { id
   isResolved }`, `PRRT_kwDORFy7ds6h3A8X` → `true`) and quotes that fragment in the approval. The
   quote is the acceptance's recorded check.

The probe costs one inline comment and one reply; it turns "the review round" into the only
production-like surface that exists for the cross-App case, and it produces the same `Threads`
disposition line as any other thread (`not a defect — acceptance-2 probe`), so the record stays
truthful.

## When to apply

Any criterion of the shape "App X can act on App Y's artifact", or "the App of role R is refused
X" — thread resolution, a push the review App must not be able to make, a permission boundary
between the two Apps. The tester's negative control (a deliberately not-accepted thread reported
`left open`) still belongs to the tester; what the tester cannot do is *be* the other App. If no
phase runs as the App the criterion names, the tester names that missing surface as the blocker
instead of passing the phase (the `legion-worker` skill's production-like-surface rule).

## Related

- [external-red-and-phase-ownership](../daemon/external-red-and-phase-ownership.md) §3 — why the
  review App can reply but neither resolve nor push, the reply grammar, and the GraphQL facts.
- [worker-pane-shell-gotchas](worker-pane-shell-gotchas.md) §13 — the pane's `legion` is the
  deployed build; a new subcommand is exercised live as `bun packages/daemon/src/cli/index.ts …`.
