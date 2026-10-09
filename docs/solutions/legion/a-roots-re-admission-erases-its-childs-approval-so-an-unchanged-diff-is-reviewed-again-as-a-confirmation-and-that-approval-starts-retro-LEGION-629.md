---
title: "A root's re-admission erases its child's approval, so an unchanged diff is reviewed again as a confirmation, and that approval starts retro"
category: legion
tags:
  - re-admission
  - generation
  - unchanged-diff-fingerprint
  - confirmation-round
  - retro
  - pr-body
date: 2026-10-09
status: active
module: packages/daemon/internal/admit
applies_when:
  - A child issue of a Legion tree sits in merging (or past review) when the tree's root is re-admitted at a new generation
  - A pull request whose unchanged-diff fingerprint did not move is sent back to the tester and reviewer anyway
  - You are writing a round bullet into a pull request body several roles share
related_issues:
  - "LEGION-629"
  - "sjawhar/legion#1848"
  - "LEGION-578"
---

# A root's re-admission erases its child's approval, so an unchanged diff is reviewed again as a confirmation, and that approval starts retro

Extends docs/solutions/legion/a-conflict-round-that-changes-the-fingerprint-is-a-counted-review-round-and-its-approval-starts-a-second-retro-LEGION-578.md.

- That note's other case — "a rebase that changes nothing the reviewer re-decides, where the
  approval stands and the tree goes to the merger" — holds only while the daemon still holds the
  approval record. A root's re-admission bumps the tree's generation and clears the tree's handoff
  state with it — pull request, gate, handoffs and pending READY for every issue of the tree
  (`ClearTreeGeneration`, `packages/daemon/internal/record/store.go`, called from
  `admit.go`'s readmit): a child resumed in merging has no approved head (`approvedHead`,
  `packages/daemon/internal/workflow/effects.go`), the merger refuses for want of one, and the daemon
  runs the tester and the reviewer again even when the child's diff is byte-identical
  (`packages/daemon/internal/admit/admit_test.go`, `TestAResumedMergerIsToldNoApprovedHeadAcrossGenerations`).
- Run that round as a confirmation: the tester re-checks the bare gates and moves its `E2E` head
  with a `rebase re-check` record; the reviewer computes the fingerprint at its own last approved
  head and at the new head, and when they are equal re-states the approval by SHA with no
  thermonuclear pass and no thread pass. Every approval starts retro (`table.go`: `Reviewing` +
  `TriggerReviewApproved` → `Retro`), so a confirmation's approval starts the implementer on retro
  once more; that retro covers only the confirmation round and edits none of the earlier retros'
  files.
- A long pull request accumulates three round counters — the implementer's implementing rounds, the
  tester's test rounds and the reviewer's review rounds — in one body. Label every round bullet with
  the role whose counter it uses and the head it stands at (`Round 5 (reviewer, at 4dcf272f)`),
  never a bare `Round N`: the same number names different heads on different counters.

## Evidence

sjawhar/legion#1848 (child LEGION-629 of root LEGION-578). The reviewer approved the round-4 head
17150b84 (review 5464668416) and the first retro's docs landed at afcdd171 above it. The root was
then re-admitted at generation 2; the merger's task named no approved head, and at the same time
`main` had moved (#1857, #1859, #1835 and the v8.4.x releases above merge-base f1dcadfe), so GitHub
showed the head `CONFLICTING`. The architect's direction (envoy 06aa4fbd05a789c32752f043a49f1524):
forward-merge `main@origin`, push, and let the tester and reviewer run again. The merge 66c26bbc
changed no line of the branch's diff — fingerprint `ce20b82d…` at afcdd171 and at 66c26bbc, computed
by the implementer, independently by the tester (`test.json` `proof[0]`, with the earlier approved head
5137c274 → `bba40d23…` as the negative control) and by the reviewer (`review.json` `fingerprint`). The
tester's handoff: `"Round 7 is a rebase re-check, not a round"`; the reviewer's: `"kind":
"confirmation after a conflict-forced merge (conflict round 2): fingerprint unchanged, so round 4's
approval is re-stated by SHA — no thermonuclear pass, no thread pass"`, approved at bc9dbf23
(handoff 4dcf272f). The daemon then started retro a third time; this note is its output. The body's
`Not proven / risk` list at that point read two bullets both opening `Round 5` — the implementer's
(merge head 24f2532c, fix head 7d98d70e) and the reviewer's (at 4dcf272f) — with `Round 4 (reviewer, at
17150b84)` between them, while the tester called the same work round 7.
