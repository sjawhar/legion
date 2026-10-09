---
title: "A proof quotes the runner's summary line verbatim and carries no total across rounds"
category: legion
tags:
  - proof
  - pr-body
  - handoff
  - test-counts
  - review-rounds
date: 2026-10-09
status: active
module: skills/legion-worker, .legion handoffs
related_issues:
  - "LEGION-634"
  - "sjawhar/legion#1865"
---

# A proof quotes the runner's summary line verbatim and carries no total across rounds

## The rules

- **Paste the runner's own summary line into the proof's `observed` and the PR body's `E2E` line.**
  `192 pass / 0 fail / 691 expect() calls / Ran 192 tests across 14 files` is a quotation; a number
  typed from memory after adding a case is a claim the tester will re-run and find false.
- **Never recompute a total.** Adding one test to a suite that printed `192 pass` does not make
  the proof `193 pass`: the run that printed 192 already held the case, or the next run prints the
  new number itself. Re-run and quote.
- **Every round re-quotes at its own head.** A docs-only round whose code is byte-identical still
  runs the suite once and names the head it ran at; the tester compares the quoted line with its
  own run and with CI's job log at that head.
- **Correct a wrong number everywhere it was written, and say so.** The handoff's `proof`, the PR
  body's `Proven by` and `E2E (implementer)` lines, and a `deviations` entry naming the miscount;
  the tester's own line stays as the tester wrote it.

## Evidence

- LEGION-634, round 1: the implementer's full-suite run printed `192 pass, 0 fail, 691 expect()
  calls, Ran 192 tests across 14 files`, yet `implement.json` proof 2 and the pull request's
  `Proven by` and `E2E (implementer)` lines said `193 pass, 697 expect() calls` — a total recomputed
  after the fifth phase-stall case was added, though that case was already in the run. The tester
  (`.legion/LEGION-634/test.json`, `implementerProof.how` and `documentationFeedback`) found 192/691
  in its own pod and in CI's `pi-legion` job log at the same head and listed it as a minor item.
- Round 2 re-ran the suite at `40d8393953f8` (192 pass / 691 expect(), as every run), quoted it in
  both places and recorded the miscount under `deviations`; the tester's round-2 handoff verified
  the corrected count.
