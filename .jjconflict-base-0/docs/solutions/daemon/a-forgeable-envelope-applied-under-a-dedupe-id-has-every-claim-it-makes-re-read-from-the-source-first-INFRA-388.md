---
title: "A forgeable envelope applied under a dedupe id has every claim it makes re-read from the source first"
category: daemon
tags:
  - envoy
  - intake
  - dedupe
  - authz
  - production-check
date: 2026-10-25
status: active
module: packages/daemon
related_issues:
  - "INFRA-388"
  - "sjawhar/legion#2203"
---

# A forgeable envelope applied under a dedupe id has every claim it makes re-read from the source first

Extends docs/solutions/daemon/work-after-the-close-is-fenced-on-its-own-record-woken-by-events-bounded-twice-and-gated-on-in-workflow-not-on-the-phases-role-LEGION-781.md.

- That document holds a bus value to a validated shape. Shape is not enough when the fact is
  applied under a deterministic event id (`intake.ApplyFact` records each source and id once).
  A forged envelope that arrives first spends the id, and the genuine delivery is then a
  duplicate that does nothing.
- So, before `ApplyFact`, read from the authoritative source **every condition the envelope
  claims**, not only its identity. For a GitHub run that means the workflow file, branch, head
  repository and event, and also its `status`.
- When a plan changes when a fact fires (here, at the run attempt's `completed` webhook rather
  than at the step's own finish), the new trigger's field becomes a precondition. Add it to the
  re-read in the same change.
- A row whose source says "not yet" finishes without a retry when the genuine event queues a row
  of its own. Name the remaining risk, a REST read that lags the webhook, and measure it, rather
  than adding a retry loop.

## Evidence

- Review round 1 on #2203, thread PRRT_kwDORFy7ds6wDyya (Security[authz], Important), quoted:
  the departure makes "the run has completed" a precondition of the wake, and the executor does
  not read it from GitHub. Any Envoy bearer holder can publish a `source: github` envelope.
- The fix is in 833909e7: `checkDeployRun` wakes nothing unless the attempt reads `completed`.
  The wake event id is `check-wake:<issue>:<check>:deploy run:<run>:<attempt>:<job>`. The lock,
  `TestACheckCompareOfADeployRunStillRunningWakesNothingUntilItCompletes`, answers
  `in_progress` (no jobs read, no fact, no `processed_events` row), then `completed` (one wake).
- The tester forged a `completed` envelope for real running run 40220571121, and it was refused
  on status alone. The run's genuine completion then opened look 2. REST read `completed` for 8
  of 8 production Envoy deliveries, within about 0.7 s of each.
