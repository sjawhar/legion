---
title: "Worker smoke testing must use real builds and runtime verification, not code review"
category: testing
tags:
  - testing
  - smoke-testing
  - quality-gates
  - worker-lifecycle
date: 2026-03-19
status: active
related_issues:
  - "3515"
  - "3983"
  - "3506"
---

# Worker Smoke Testing Must Use Real Builds and Runtime Verification, Not Code Review

Source session: `ses_2fe5a7e21ffeZicAZhxr9NXQxu` (2026-03-18 15:55 UTC – 2026-03-19 16:29 UTC). 10 testers dispatched across multiple environments.

## Problem: False Passes from Code Review

All 10 testers initially passed by doing code review — reading the code and reasoning about correctness — rather than actually building and running the environments. The controller caught this only because it re-inspected the results:

> "Let me resume all 10 with explicit instructions to do real testing" — 04:08 UTC

The testers produced `test-passed` labels without any build evidence, endpoint verification, or runtime output. The controller had to explicitly reprompt:

> "Code review is NOT testing." — 03:55 UTC

This is the core failure: **testers treated code review as a substitute for runtime verification**, and the testing framework did not prevent this.

### Evidence Pattern

The first round of testers completed with variations of "code looks correct, PASS" without:
- Building the application
- Starting the environment
- Making HTTP requests to verify endpoints
- Checking that the actual bug (e.g., 500 error, missing field) was resolved

One tester that actually ran the environment found real evidence:

> "Solid evidence — HTTP 200, build passes, job submitted, no 500s. This passes." — 04:15 UTC

This was the exception, not the norm. Most testers never reached the build step.

## Problem: No Acceptance Criteria in Issues or Plans

Issues and plans did not define what "tested" means. Testers had no runbook, no acceptance criteria checklist, and no defined smoke test procedure:

> "Each issue (or the plan) should declare acceptance criteria that are machine-executable. 'Profile page returns 200' not 'profile page works'." — 15:20 UTC

One tester illustrates the gap: it reported "PASS — 3/3 acceptance criteria verified" but was then found stuck for 14 hours trying to build:

> "The test already had 'PASS — 3/3 acceptance criteria verified' from the first testing round, but..."

The "acceptance criteria" were self-invented by the tester (code review based), not defined by the issue or plan.

## Problem: No Standardized Smoke Test Infrastructure

Each tester had to improvise the build and verification process from scratch. There was no standardized command to build an environment and run smoke tests.

Without a standard tool, testers encountered environment-specific build issues:
- `npm install` failures from ENOSPC
- Build space exhaustion across environments
- Missing environment variables and configuration

A standardized smoke test command would have failed fast with a clear error instead of letting workers loop for hours.

## Problem: Too Many Small PRs Create Testing/Merge Burden

Each environment got its own issue and PR, resulting in 10+ PRs that each needed independent testing, review, rebasing, and merging. After each merge, remaining PRs went stale:

> "#4529 merged. #4550 has a merge conflict — needs rebase" — 04:08 UTC

The testers consumed 5 serve slots simultaneously, and the merge cascade consumed hours of CI.

### Consolidation Principle

For batch fixes (same root cause across multiple targets), fewer larger PRs reduce the testing/rebase/merge burden. One PR touching 5 targets needs one test cycle and one merge, versus 5 PRs needing 5 test cycles and a 5-step merge cascade.

## Required Changes

### 1. Tester Workflow: Mandatory Runtime Evidence

The tester workflow must require structured evidence in the PR comment:

```markdown
## Smoke Test Evidence
- **Environment:** [name]
- **Build:** [pass/fail + build time]
- **Health check:** [endpoint → HTTP status]
- **Bug verification:** [original bug scenario → observed result]
- **Acceptance criteria:** [each criterion → pass/fail with evidence]
```

The controller should not advance on `test-passed` alone — it must verify the evidence block exists in the latest tester comment.

### 2. Issue Templates: Machine-Executable Acceptance Criteria

Each issue must include acceptance criteria that a tester can mechanically verify:

```markdown
## Acceptance Criteria
- [ ] Build succeeds
- [ ] Health check endpoint returns 200
- [ ] Target endpoint returns expected payload (no 500)
- [ ] Original bug scenario produces correct behavior
```

### 3. Infrastructure: Standardized Smoke Test Command

Build a standardized smoke test command that:
- Builds the application
- Starts the environment
- Runs health checks and verifies endpoints
- Exits with 0/1 and structured output

This eliminates tester improvisation and provides a fail-fast path for infrastructure issues.

### 4. Consolidation Policy

For batch fixes with the same root cause:
- Group into 1–2 PRs maximum
- Run a single combined test pass
- Merge once, avoid the N² rebase cascade

This is a controller-level policy decision during triage: when the architect identifies the same fix pattern across N targets, instruct the planner to consolidate.
