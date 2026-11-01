---
title: "Work main adds to a pass your branch removed moves to the pass that replaced it"
category: architecture-patterns
tags:
  - forward-merge
  - conflict-resolution
  - background-loops
date: 2026-11-01
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2391"
---

# Work main adds to a pass your branch removed moves to the pass that replaced it

Extends docs/solutions/architecture-patterns/work-that-moves-to-another-loop-keeps-its-lease-name-and-its-once-per-period-pass-pr2391.md.

- When your branch replaced a function and main adds a step to the old one, a conflict resolution that takes your side drops main's step without a word. Read main's side of every conflict in code your branch replaced, and list what it added.
- Move each added step to the replacement whose shape fits the work, not to the one nearest in the file. Work that a commit announces belongs in a woken loop. Work that only the clock or a dead process makes due belongs on the leased once-per-period timer.
- Name the move in the diff check, so the reviewer sees main's step is still run, and where.

## Evidence

Pull request sjawhar/legion#2391, merge e5e0198d. Main's #2541 added a sweep of spent DPoP proofs to the old leased suggestion pass, which this branch had replaced with a woken suggestion sweep and a leased maintenance timer (`api.RunMaintenanceSweep`). The conflict in `api/suggestion_outcomes.go` showed main's lines only inside the removed function. A spent proof expires by the clock, as an Idempotency-Key does, so the sweep went onto `RunMaintenanceSweep` beside `sweepRequestKeys`, under the same lease, and main's comment on `store.DPoPProofs.Sweep` now names that caller.
