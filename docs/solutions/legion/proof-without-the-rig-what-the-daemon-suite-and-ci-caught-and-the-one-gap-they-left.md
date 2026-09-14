---
title: "Proof without the smoke rig: what the daemon suite and the pull request's CI caught, what the CI-only real-pane suite proves, and the one sub-path only fakes cover"
category: legion
tags:
  - smoke-rig
  - standing-order
  - proof-surface
  - LEGION_E2E
  - real-prompt-delivery-e2e
  - ci-as-surface
  - e2e-line
date: 2026-09-14
status: active
module: packages/daemon/src/daemon/__tests__
problem_type: process
severity: medium
related_issues:
  - "LEGION-93"
  - "sjawhar/legion#1082"
  - "LEGION-60"
applies_when:
  - An acceptance line that named a smoke-rig or scratch-daemon proof is withdrawn (Sami, 2026-09-13 19:00Z, "Please shutdown the goddamn legion smoke")
  - You are writing the PR body's E2E line for a daemon change whose only allowed surfaces are the unit suite and CI
  - A test suite in the repository is gated on an environment variable you may not set locally
---

# Proof without the smoke rig

## What was withdrawn, and what stood in for it

LEGION-93's spec had a fifth acceptance line: a fake shim on the smoke rig drives one role through
retire → relaunch → `worker-died`, and the rig's Envoy listener records the notice on the architect
topic. Sami's standing order landed mid-tree and withdrew it; the plan's Task 4 (a `swallow-prompts`
stand-in for the rig's launch prefix) and Task 6 (the rig run) were dropped. The proof surface
became the daemon's own suite plus CI on the pull request — and the E2E line said exactly that,
citing the order, rather than dressing a unit run up as an end-to-end pass.

## What that surface caught that a rig run would not have

Two defects, both in CI, neither reachable from the rig:

1. **The `LEGION_E2E=1`-only suite had drifted.** `real-prompt-delivery-e2e.test.ts` (a real tmux
   pane, the real `legion worker-shim`, a stand-in OMP that never starts a turn) asserted the
   LEGION-60 accounting — `promptFailures` 3 at the third failure — and is skipped locally (10 skips
   in `bun test packages/daemon`). The first CI run failed it; the fix was test-only. Under the
   standing order that suite runs **only** in CI, so before pushing a change to a counter or a log
   line, grep the gated suites for it: `grep -n promptFailures packages/daemon/src/daemon/__tests__/*e2e*`.
   A green local run is not evidence the gated suite agrees.
2. **`main`'s harness change on the merge ref** (#1027; see
   `mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md`). A rig exercises the daemon
   binary, not the merge of two test files.

## What the CI-only real-pane suite proves for real, and the one gap

The gated suite's no-turn test now asserts the first cycle on a **real** pane: three acknowledged
prompts with no turn, the real pane killed, the locator cleared with the session file carried,
`promptRetires: 1`, the exact retire line, no `worker-died`. Its rig refuses workspace provisioning
by design, so the relaunch lands as a launch failure — the **second** cycle on a relaunched real pane
and the `worker-died` reaching a real listener are proven only with the in-memory fakes
(`swallowedRelaunchFixture` in `processes.test.ts`: `first`/`relaunched` fake clients keyed by
socket path, `relaunchingTmux`). That is precisely what the withdrawn rig proof would have added:
a relaunched real pane's second cycle and the role-lane delivery (`listener role forwarded` with
the notice's `event_id`). The fake-driven test asserts the same daemon call the real lane would
carry (`publishRole` on the architect topic with `{type:"worker-died", issue, role}`), and the
listener half of that path was proven end to end for `worker-queued`/`worker-started` on
LEGION-60's rig (`../daemon/role-notices-go-through-the-listener-a-bare-nats-publish-reaches-nobody.md`),
so the residual risk is small — but it is a sub-path with no real-surface proof, and the honest
E2E line says so.

Cheapest closure without a rig, if it is ever wanted: let the gated suite's stand-in relaunch (its
`run` fake refuses `jj` today) and drive the relaunched real pane through three more swallowed
prompts to `worker-died`; a CI-only change.

## The rule for the E2E line

When a rig proof is withdrawn, do not write "unit tests" as the surface and stop. Name, per
acceptance line, which remaining surface reaches it — the in-memory suite, the CI-only real-pane
suite, CI itself — and name the sub-path only fakes cover. That partition is what let the reviewer
approve the change and what tells the next issue where a real surface is still missing.

## Related

- `smoke-rig-modes-gate-checkpoints-and-the-pin-has-one-home.md` — the rig this order retired,
  for the record of what it could do.
- `../daemon/a-bounded-counter-on-a-relaunched-claim-audit-every-constructor-advance-after-the-stop-reset-in-one-place.md`
  — the change these surfaces proved.
- `../testing/tick-budgets-over-real-io-flake-await-the-event-the-production-path-emits.md` —
  LEGION-60's note on the same fake-client/stand-in split.
