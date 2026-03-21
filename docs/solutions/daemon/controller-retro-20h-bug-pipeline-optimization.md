---
title: "Controller Retro: 20h Bug Pipeline Optimization"
category: daemon
tags:
  - controller
  - throughput
  - session-management
  - testing
  - workflow-design
date: 2026-03-19
status: active
module: daemon
related_issues:
  - "4817"
symptoms:
  - "dispatch timed out while creating worker sessions"
  - "workers stayed busy for hours with no new PR evidence"
  - "test-passed labels appeared without reproducible smoke evidence"
  - "stale workers accumulated across controller restarts"
---

# Controller Retro: 20h Bug Pipeline Optimization

## Context and Scope

This retro analyzes controller session `ses_2fe5a7e21ffeZicAZhxr9NXQxu` (2026-03-18 15:55 UTC to 2026-03-19 16:29 UTC) for `trajectory-labs-pbc/2` with focus on bug triage and fix flow in `trajectory-labs-pbc/agent-c`.

Observed outputs and board state during/after the session:

- Session scale: 821 messages over ~24h.
- Repository merge throughput in-session: 46 merged PRs in the same time window (mixed bugfix + unrelated work).
- User-stated target subset: 15 bug PRs across 5 agar environments plus 3 infra blockers.
- Board anchor issues at end of analysis: `#4450` Done, `#4524` Done, `#4522/#4523` still In progress.
- Open `agar-environment-issue` backlog remains substantial (18 open issues).

## What Went Well

- The controller kept a high issue movement rate despite infrastructure friction.
- Blocking infra issue `#4817` was eventually isolated, fixed (`PR #4872`), and unblocked multiple waiting PRs.
- Multi-environment workstreams (LinkedIn, Bitwarden, Email, Greenhouse, Google) were advanced in parallel rather than serially.
- The pipeline generally respected stage semantics (architect/plan/implement/test/review), even when manual nudging was required.

## Waste and Failure Patterns

### 1) Polling-heavy control loop burned context without adding state

Symptoms:

- Repeated "keep polling" cycles with low information gain.
- User had to explicitly prompt "You're not polling" after idle windows.
- Long stretches where workers were "busy" but produced no new durable artifact.

Impact:

- High token/time burn in the controller session.
- Slower reaction to true blockers because noisy polling crowded operator attention.

### 2) Session creation timeouts triggered unsafe recovery behavior

Symptoms:

- Multiple dispatch/session creation timeouts.
- Timeout handling sometimes escalated to `--version` increments, spawning fresh contextless sessions.
- Later self-retro explicitly called this out as harmful.

Impact:

- Lost worker continuity.
- Duplicate or partially overlapping work.
- Elevated risk of wrong-branch pushes and repeated onboarding prompts.

### 3) Worker liveness and work completion were inferred from weak signals

Symptoms:

- "busy" status treated as progress.
- Testers sometimes completed with no comment/label evidence, or labels were used without strong reproducibility proof.
- A Bitwarden tester remained effectively stuck for 12+ hours.

Impact:

- False confidence in test quality.
- Slow detection of deadlocked workers.
- Rework when missing evidence had to be reconstructed.

### 4) Infrastructure blocker triage came later than optimal

Symptoms:

- ENOSPC (`#4817`) blocked merges while environment PRs kept cycling through rebase/CI loops.
- Auto-merge was toggled repeatedly on PRs that were structurally blocked by infra state.

Impact:

- Queue churn and repeated CI runs.
- Longer cycle times for otherwise-ready fixes.

## Quality Findings

The main quality gap was not whether tests existed, but whether they produced auditable, environment-level acceptance evidence.

Observed failure mode:

- "test-passed" was sometimes treated as sufficient even when tester comments lacked hard evidence (exact command, endpoint, expected/actual outcome).

Required standard for this pipeline:

- A tester pass must include reproducible proof on the PR (or linked issue comment):
  - exact smoke command(s),
  - environment and scenario target,
  - observed result payload/status,
  - explicit pass/fail against acceptance criteria.

Without this artifact, the controller should treat the phase as unresolved.

## Prioritization Assessment

The controller correctly recognized infra blockers as important, but execution order lagged:

- Early parallelization increased throughput initially.
- Once ENOSPC became the dominant merge bottleneck, priority should have collapsed to "fix infra first, then resume queue".

Net: prioritization intent was good, but policy needed a stronger automatic "global blocker" mode.

## Session Management Assessment

Good:

- Sessions were often reused as designed.

Gaps:

- Timeout paths were not deterministic enough; retries vs reattach vs recreate were handled ad hoc.
- Stale worker accumulation (5 then 33 stale workers observed in-session) added noise and made status interpretation harder.
- No automatic stale-worker quarantine/escalation policy prevented very long-running stuck sessions.

## Concrete Changes to Go 2x Faster with Half the Waste

### Controller skill changes (`.opencode/skills/legion-controller/SKILL.md`)

1. Add a strict timeout recovery ladder:
   - retry dispatch once,
   - verify session existence,
   - re-prompt existing session,
   - only allow version increment with explicit "session unrecoverable" evidence.

2. Add evidence-gated tester completion:
   - do not advance on `test-passed` label alone,
   - require proof template in latest tester comment.

3. Add blocker mode policy:
   - when N PRs fail on same infra signature (e.g., ENOSPC), pause non-blocker dispatches,
   - dispatch dedicated infra implementer/tester pair,
   - resume queued PR flow only after blocker verification.

4. Add dead-worker heuristic:
   - if worker remains busy for T without new PR comment/check/run change, mark suspected-stuck and escalate.

### Daemon/state-machine enhancements (`packages/daemon/src/*`)

1. Expose richer progress signals per worker:
   - `lastMessageAt`, `lastToolCallAt`, `lastGitActivityAt`, `workspaceExists`.

2. Add first-class `stuck`/`degraded` worker statuses:
   - computed from liveness + progress deltas, not just process health.

3. Add dispatch idempotency key and timeout-safe return path:
   - if dispatch times out after creation, return existing worker/session metadata instead of inviting duplicate retries.

4. Add global blocker detector:
   - aggregate failing check signatures across active PRs and emit controller hint (`prioritize_blocker_issue`).

### Worker workflow changes (`.opencode/skills/legion-worker/workflows/*`)

1. Tester workflow must publish structured evidence block (machine-parseable section).
2. Implementer workflow should detect infra-class CI failures and annotate "blocked by global infra issue" once, then stop retry loops.
3. Retro workflow should auto-capture stuck-session incidents and timeout recovery paths for continual policy tuning.

## Suggested Metrics to Track Next Session

- Median issue cycle time per mode transition (architect->plan->implement->test->review->merge).
- Time in "busy with no artifact" per worker.
- Dispatch timeout count and successful reattach rate.
- Percentage of tester passes with full evidence template.
- Merge queue delay attributable to shared blocker signatures.

If these changes are implemented, the largest expected gains are from reducing blind polling, eliminating timeout-driven context resets, and enforcing evidence-based test transitions.
