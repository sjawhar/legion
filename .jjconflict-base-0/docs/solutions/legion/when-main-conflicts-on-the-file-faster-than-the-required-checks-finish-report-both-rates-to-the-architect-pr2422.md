---
title: "When main conflicts on the file faster than the required checks finish, report both rates to the architect"
category: legion
tags:
  - conflicts
  - ready
  - ci
  - forward-merge
date: 2026-10-28
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2422"
---
# When main conflicts on the file faster than the required checks finish, report both rates to the architect

Extends docs/solutions/legion/when-runners-queue-longer-than-a-review-round-make-one-code-push-per-round-pr2422.md.

- Approval does not end the race with main. READY is withdrawn when the head conflicts with its
  base. Each forward merge is a code push that starts the required checks again.
- After each forward merge forced by a conflict, compare two numbers: how often main lands a
  conflicting edit on the file, and how long the required checks take to queue and finish. Name
  the conflicting commits and the queue time you observed.
- When conflicts come faster than the checks finish, send both numbers to the architect in your
  report. A hold on the other pull requests that edit the file is the architect's or the
  controller's decision, not yours. Do not start more regeneration rounds than the conflicts
  require.

## Evidence

- sjawhar/legion#2422 was approved, and READY was then refused twice for a conflict with main in
  `packages/envoy/AGENTS.md`:
  - at 45760c26, against main 20ba65a6 (#2366), fixed by code push 29a28301;
  - at 9ffaba81, against 300ff910 (#2453), fixed by code push a809764e.
- At each of those heads the required checks were still queued, about an hour behind.
- The pull request body records 31 conflicting merges of main on its two files in one week.
