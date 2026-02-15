---
name: plan-review
description: Have specialized agents review a plan in parallel
argument-hint: "[plan file path]"
---

Review the provided plan using parallel specialist agents. Spawn both as background tasks and wait for results.

## Reviewers

### 1. Momus — Plan Executability (cross-family)

Spawn via background_task:
- subagent_type: momus
- model: openai/gpt-5.2 (cross-family — default agents are Anthropic)
- description: "Plan executability review"
- prompt: Start with "MODE: PLAN_EXECUTABILITY" then include the plan content. Ask Momus to check: file paths real and specific, code examples complete, test commands runnable, dependency graph correct, acceptance criteria machine-verifiable.

### 2. Simplicity Reviewer — Complexity Challenge

Spawn via background_task:
- subagent_type: simplicity-reviewer
- description: "Plan simplicity review"
- prompt: Include the plan content. Ask the reviewer to challenge unnecessary tasks, over-engineered approaches, premature abstractions, and tasks that could be combined or removed.

## Mode Verification

After receiving Momus's response, verify it begins with `## Mode: Plan Executability Review`. If it echoes a different mode (e.g., "Critical Review"), the MODE header was not picked up — retry once with the MODE: line as the very first line of the prompt, before any other content.

## Aggregation

After both complete (use background_output for each), aggregate into:

- **Verdict:** [executable/needs-work/reject] — reject if either reviewer finds blocking issues
- **Issues:** Combined list, deduplicated, severity-ranked (blocking first, then non-blocking)
- **Fixes:** Specific fix for each issue

## Failure Handling

- If one reviewer fails or times out (>5 min), proceed with the other's results and note "partial review — [agent] unavailable"
- If both fail, report "review unavailable" and escalate to manual review
