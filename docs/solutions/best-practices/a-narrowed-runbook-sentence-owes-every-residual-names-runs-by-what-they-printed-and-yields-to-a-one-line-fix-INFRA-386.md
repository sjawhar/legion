---
title: "A narrowed runbook sentence owes every residual, names runs by what they printed, and yields to a one-line fix"
category: best-practices
tags:
  - documentation-drift
  - proof
  - review-rounds
date: 2026-10-25
status: active
module: docs/kubernetes.md
related_issues:
  - "INFRA-386"
  - "sjawhar/legion#2196"
---

# A narrowed runbook sentence owes every residual, names runs by what they printed, and yields to a one-line fix

Extends docs/solutions/best-practices/a-residual-review-records-narrows-the-published-sentence-INFRA-232.md.

- **A residual recorded in the pull request body is a sentence the page still owes.** That
  includes a residual about the remedy your narrowing adds ("no wait was measured before the
  re-list"). Before approval, map every `Not proven / risk` line to the page line that carries it.
- **Test a recovery instruction with its end-state question.** Give a fresh reader the
  recovery's own clean-looking output (an empty re-list) and ask whether the role is done. Asking
  "what should the role run next?" passes text that leaves the end state open.
- **A caveat reworded into a list of exceptions makes the list exhaustive.** Before you write
  "it can leave the workload: on X; on Y", enumerate the members from the code: every failure path
  the code prints, every signal it does not handle, every exit that runs no trap.
- **Classify a run only by what it printed.** "A stop that landed inside the create" was a class
  assigned because the label delete found a Job, which is the outcome being claimed. That is an
  inference that looks like a measurement. Say what the run printed ("took the label path and found
  a Job") and what its output cannot show.
- **When the finding is a defect in a published command, fix the command.** If one command line
  removes the hazard, ask the architect to lift any freeze on the block in that round. Do not write
  a warning around the hazard: each warning describes a narrower case than the truth.
- **A plan that fences edits to line ranges must cover every passage stating the same fact.** Grep
  the claim's key phrase over the whole page before approval, and again when you retract the claim.
  An evidence bullet outside the ranges otherwise contradicts the new sentence.

## Evidence

sjawhar/legion#2196 took nine implementing rounds on one section of `docs/kubernetes.md`.

- Round 1 was approved with "an empty re-list is not proven to rule such a Job out" in the body
  only. The first retro's reader, given an empty re-list, answered "Yes, but only by inference" to
  "is the role done?". Round 2 put the sentence on the page, and the same reader then answered
  "No".
- Round 1 reworded "However it ends, the trap removes the workload, short of a SIGKILL" into a list
  of exceptions. That list missed a failed clean-up step (round 3) and a stop during the trap
  (round 5).
- Round 3 wrote that nothing the 0.33 s and 0.35 s runs printed placed their stop. The evidence
  bullet, outside the plan's ranges, still said "two that landed inside the create", and the tester
  failed round 3 on it.
- Rounds 5 and 6 warned "send one stop", then "send a stop only while the work runs". Round 7 added
  the trap's signal line and deleted both warnings.
