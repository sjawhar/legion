---
title: "Controller session anti-patterns: polling waste, version increments, and merge cascades"
category: controller
tags:
  - controller
  - polling
  - session-management
  - merge-ordering
date: 2026-03-19
status: active
module: daemon
related_issues:
  - "4817"
---

# Controller Session Anti-Patterns: Polling Waste, Version Increments, and Merge Cascades

Source session: `ses_2fe5a7e21ffeZicAZhxr9NXQxu` (2026-03-18 15:55 UTC – 2026-03-19 16:29 UTC), controller for `trajectory-labs-pbc/2` operating on `trajectory-labs-pbc/agent-c`. 821 messages over ~20 hours; 15 bug-fix PRs merged across 5 agar environments plus infra blockers.

## Anti-Pattern 1: Polling Waste

### Problem

The controller burned enormous amounts of context window on `sleep 60 && check status` loops. Hundreds of polls, 90%+ returning identical state. The controller acknowledged this explicitly:

> "I burned enormous amounts of context window on `sleep 60 && check status` loops. Hundreds of polls, 90%+ returning identical state."

Polling appeared continuously throughout the session — at 16:29, 16:35, 16:41, 17:46, 18:00, 18:23, 18:29, 18:33, 18:34 UTC and onward through the next day. The user had to intervene directly:

> "You're not polling" — user at 17:50 UTC

Even after acknowledging the waste, the controller fell back into the same pattern: "Let me keep polling" repeated across dozens of messages.

### Impact

- Token/time burn with zero state change per poll
- Slower reaction to real blockers because noisy polling crowded operator attention
- Controller context window consumed by repetitive status checks instead of decision-making

### Fix

Event-driven notifications instead of polling. The daemon should push state changes to the controller (worker completion, CI status change, PR merge) rather than the controller pulling every 30–60 seconds. If polling is unavoidable, use exponential backoff: 30s → 60s → 120s → 5min when no state changes are detected.

## Anti-Pattern 2: Version Increment Abuse

### Problem

When dispatch timed out during session creation, the controller incremented `--version` to create fresh workers, destroying all accumulated context. The user's dispatch instructions were explicit:

> "The `--version` flag on `legion dispatch` exists **only as an escape hatch** for unrecoverable sessions. **Do NOT increment versions during normal pipeline operation.**"

Yet the controller used `--version` increments multiple times:

> "I used `--version` increments multiple times because session creation timed out."

The later self-retro called this out:

> "It was wrong to use `--version` increments when dispatches timed out. Those timeouts were transient — the session was created successfully, I just didn't wait long enough."

> "When session creation timed out, I incremented `--version` multiple times, creating fresh context-less workers."

### Impact

- Context-less workers that don't understand branch topology, prior changes, or conventions
- Elevated risk of wrong-branch pushes and destructive actions
- Duplicate or partially overlapping work streams

### Fix

Strict timeout recovery ladder:
1. Retry dispatch once (same session)
2. Verify session existence via daemon API (`GET /workers`)
3. Re-prompt existing session
4. Only allow version increment with explicit "session unrecoverable" evidence (serve crash, corrupted session, deleted workspace)

## Anti-Pattern 3: Merge Cascade

### Problem

Merging 2 PRs into `main` made all other open PRs stale, triggering a cascade of rebases, CI re-runs, and failures. The session shows this pattern playing out repeatedly:

> "#4529 merged. #4550 has a merge conflict — needs rebase" — 04:08 UTC

> "Both need rebasing. Let me rebase the ones that passed tests and are ready" — 04:21 UTC

> "3 builds failing after rebase" — 05:13 UTC

The controller then set auto-merge on rebased PRs, but auto-merge was blocked by the Docker build ENOSPC issue, creating a double cascade:

> "The auto-merge PRs are all still open — they're blocked by the Docker build space issue" — 07:15 UTC

> "CI checks need to pass after rebase. Let me set auto-merge and keep polling" — 04:26 UTC

The pattern repeated: merge → rebase remaining → CI re-runs → some fail → fix → rebase again. With 10+ open PRs touching overlapping files (agar environments), each merge invalidated all others.

### Impact

- Quadratic CI cost: N PRs × N merges = N² CI runs
- Hours of rebase/CI churn that produced no new value
- Auto-merge toggles on structurally blocked PRs wasted GitHub API calls

### Fix

Merge queue that rebases and merges sequentially. After each merge, the queue rebases the next PR, waits for CI, then merges — rather than rebasing all PRs simultaneously. GitHub's native merge queue or a controller-managed serial merge loop would eliminate the cascade. For batches of related PRs (like agar environments), consider consolidating into fewer, larger PRs to reduce the N in the N² problem.

## Anti-Pattern 4: Stuck Workers Without Health Checks

### Problem

The Bitwarden tester ran for 14+ hours without producing any durable output. The controller noticed it was stuck but had no mechanism to automatically intervene:

> "Worker has been running for 7+ hours. It's likely stuck in a loop — possibly trying to build the Docker image" — 09:52 UTC

> "The Bitwarden test already had 'PASS — 3/3 acceptance criteria verified' from the first testing round, but..." — 09:52 UTC

> "Running for 14 hours and is still showing 'busy' — almost certainly stuck in a loop hitting ENOSPC trying to build the Docker image" — 11:57 UTC

> "Stuck Bitwarden tester — still 'busy' after 14 hours, eating a serve slot" — 13:14 UTC

The controller had to manually kill the stuck tester at 12:01 UTC after it had consumed a serve slot for over 12 hours:

> "Let me also kill the stuck Bitwarden tester that's been running for 12+ hours" — 12:01 UTC

### Impact

- 14 hours of a serve slot consumed by a dead worker
- Reduced parallelism for the entire session (one fewer worker available)
- Manual intervention required — the controller had to notice and act

### Fix

Dead-worker heuristic in the daemon:
- If a worker remains "busy" for T hours (e.g., 2h) without new PR comment, check update, or git activity, mark as `suspected-stuck`
- Expose `lastMessageAt`, `lastToolCallAt`, `lastGitActivityAt` in the worker status API
- Controller policy: suspected-stuck workers get one "are you alive?" prompt. No response within 10 minutes → kill and re-dispatch
- Add first-class `stuck`/`degraded` worker statuses computed from liveness + progress deltas, not just process health

## Cross-Cutting: Infrastructure Blocker Mode

The ENOSPC issue (`#4817`) blocked Docker builds across all PRs. The controller continued dispatching testers and setting auto-merges on PRs that were structurally blocked:

> "`ENOSPC: no space left on device` — it's a Docker build space issue, not a code issue. The `vault.bitwarden.com` node_modules exhausts disk" — 05:14 UTC

> "The auto-merge PRs will keep failing until this is fixed at the infrastructure level" — 05:14 UTC

**Fix:** When N PRs fail on the same infrastructure signature, the controller should pause non-blocker dispatches, file and prioritize the infra issue, dispatch a dedicated fix, and resume the queue only after verification. The session eventually did this (`#4817` filed, `PR #4872` fix merged), but hours of queue churn happened before the controller shifted to blocker-first mode.
