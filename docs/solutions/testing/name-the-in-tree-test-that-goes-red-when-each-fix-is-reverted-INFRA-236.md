---
title: "Name the in-tree test that goes red when each fix is reverted, or say in the pull request that the fix is unpinned"
category: testing
tags:
  - test-coverage
  - review-process
  - regression
  - scratch-harness
  - go
date: 2026-10-25
status: active
module: packages/envoy/cmd/broker
related_issues:
  - "INFRA-236"
  - "sjawhar/legion#2162"
applies_when:
  - "a review finding is closed by a change whose proof was a scratch rig"
  - "a test measures headroom, or recomputes a production formula"
  - "a handoff or pull request body lists proofs for a set of fixes"
---

# Name the in-tree test that goes red when each fix is reverted

- **For each fix, name the test that fails if it is reverted.** A proof that a fix works today and
  a test that keeps it working are different artifacts, and a pull request that lists only the
  first leaves the most subtle invariant in the change unprotected. Do this per finding, in the
  handoff, by test name.
- **A scratch rig is a proof, not a pin.** When the drive that proved a fix lives outside the
  tree — a FIFO-fed two-process run, a shell harness that signals a real binary 300 times — either
  commit it where the suite that can run it already exists, or say plainly in the pull request
  that the behaviour is unpinned and why. The next reviewer cannot re-run what is not there.
- **Know which of your tests pin wiring and which only measure.** A row that asserts a margin
  ("comfortably inside the bound") stays green if the production timeout is deleted. A row that
  recomputes a production formula from production inputs passes for any value of the input. Both
  are worth having, and both must say in their own comment that they pin nothing, so a reader does
  not count them as protection.
- **A test whose fixture derives a parameter from the fixture cannot see the parameter change.**
  A fleet row that computes `replicas := len(fleet)` proves "fits at the size this fixture runs",
  never "fits at three".
- **Say which claim each figure supports.** Report the count that exercised the path, not the
  count that ran: when the kernel merges the signals a harness sends, most runs prove nothing, and
  only the exercised subset is evidence.

## Evidence

INFRA-236 / sjawhar/legion#2162 closed eight behavioural findings across five rounds and left
this gap distribution, found by an independent read at retro:

- The boot catch-up load — the subtlest invariant in the change, which makes a booting pod hold a
  retag a peer served while it was loading — is proven by a FIFO-fed two-process scratch rig
  (10 of 10 against the pre-change binary's 10 of 10 the other way) and by **no test in the tree**.
  Reverting that one call would leave CI green.
- The watchdog's decision is pinned 500 times as an extracted function, which is the right shape,
  but the wiring it depends on — the state channel closing before the server starts — is pinned by
  nothing.
- The 300-run real-binary signal harnesses that produced the strongest numbers in the pull request
  live only in the tester's scratch space, so the `SIGTERM`-then-`SIGINT` method cannot be
  reproduced from the tree.
- One limits row recomputes `perAddressFactor × overall.Every` from production inputs; its
  sibling's comment says exactly why that passes for any factor. The sibling — a floor written
  without reference to the factor — is the one that catches a regression.
- The fleet-traffic row computes its replica count from the fixture's own process list, so it
  proves the sizing fits at two pods and cannot detect a third.
