---
title: "An exemption stored on a record belongs to the run the issue arrived with"
category: daemon
tags:
  - admission
  - readmit
  - exemption
  - migration-backfill
date: 2026-10-26
status: active
module: packages/daemon
related_issues:
  - "INFRA-225"
  - "sjawhar/legion#2188"
---

# An exemption stored on a record belongs to the run the issue arrived with

Extends docs/solutions/daemon/re-send-chains-are-keyed-by-the-message-and-a-late-receipt-is-receipt-timeout-not-delivery-failed.md.

When a rule exempts an issue for something it did ("work already under way is never re-checked"),
and the judgement happens later than the event that earned it:

- Store the exemption as a field; it cannot be derived once re-admission resets the run state.
- Write it where the run earns it (a phase of that run begins). Clear it on **every** path that
  opens a new run: a child's re-entry, a child handed over to a root of its own without its run,
  a root reached by another tree's work. Keep it only where the issue's own tree comes back.
- Write the invariant at each of those sites: no issue is exempt unless the run it is exempt for
  is the run it arrived with.
- Backfill it only from evidence tied to the record's present tree. Phase rows outlive a
  hand-over, so count one only where its claim's tree is the issue's tree.
- Prove it with a negative control that reduces the exemption to the narrower test (here
  `CarriesRun` alone): the exempt case must fail while the checked cases keep passing, so
  "check everything" cannot pass.

## Evidence

The `TakenBack` mark (`issues.taken_back`, migration 0052) took three tester rounds:

1. `readmit` wrote it for every returning root, so a waiting root moved to backlog and back was
   admitted past its unfinished blocker.
2. Made sticky in `Engine.transition`, it let a child that ran in an earlier run of its tree be
   admitted at hand-over while still blocked.
3. Final: `transition` writes it, `readmit` clears it when another tree's work arrives,
   `enterChild` starts each new run without it. The driven proof's step 9 is the control.
