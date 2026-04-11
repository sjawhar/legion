---
title: "Worker notification conventions and two-tier escalation architecture"
category: skill-patterns
tags:
  - envoy
  - notifications
  - worker-lifecycle
  - escalation
  - conventions
date: 2026-04-11
status: active
module: legion-worker
related_issues:
  - "411"
---

# Worker Notification Conventions and Two-Tier Escalation Architecture

## Three-Tier Message Format

Every `envoy_publish` notification from a worker to the controller uses exactly one of three prefixes:

| Prefix | Meaning | When to use |
|--------|---------|-------------|
| `Worker done:` | Success exit | Work completed, PR ready for next phase |
| `Worker failed:` | Terminal failure | Unrecoverable error, needs investigation |
| `Worker blocked:` | Needs user input | `user-input-needed` label added, waiting for human |

**Message body format:** `"Worker [done|failed|blocked]: $ISSUE_NUMBER [workflow] [brief reason]"`

Examples:
- `Worker done: 411 merge completed. Issue closed.`
- `Worker failed: 411 merge failed — unresolvable rebase conflict`
- `Worker blocked: 411 plan needs user input — requirements unclear`

**Why the prefix matters:** The controller subscribes to `notifications.role.legion-controller` and can pattern-match on the prefix without parsing the full message. This enables future routing logic without changing the message format.

**Edge case — `already-merged`:** Uses `Worker done:` not `Worker failed:` because the PR was merged (by another process). The merge worker's job is to get the code merged, and that happened — it's a success.

## Two-Tier Escalation Architecture

Worker workflows use two tiers of escalation, and knowing which tier a workflow uses determines where to add notifications:

### Tier 1: Inline Escalation (workflow-specific exit paths)

**Workflows:** architect.md, plan.md, merge.md

These have specific, workflow-unique exit conditions with custom messages. Each exit path includes its own label updates, issue comments, and `envoy_publish` calls inline.

Example: merge.md has 6 distinct failure paths (already-merged, closed-without-merge, unresolvable-conflict, fundamental-CI, retry-exhausted, other-error), each with a unique notification message.

### Tier 2: Delegated Escalation (shared SKILL.md protocol)

**Workflows:** implement.md, review.md, test.md

These delegate all `user-input-needed` escalation to SKILL.md's "Blocking on User Input" section. They don't have inline escalation blocks — instead, their text says "follow the escalation pattern from SKILL.md."

**Consequence:** Adding a notification to SKILL.md's shared protocol automatically covers implement, review, and test escalation paths without touching those workflow files.

### Before adding notifications to a workflow:

1. Check whether the workflow has inline escalation blocks or delegates to SKILL.md
2. If it delegates, the SKILL.md change covers it — no per-workflow change needed
3. If it has inline exits, patch each exit point individually (per the cross-cutting-workflow-concerns pattern)

## Notification Pattern

Every notification block follows this structure:

```markdown
- Notify controller (best-effort):
  ```
  envoy_publish(topic="notifications.role.legion-controller", message="Worker [prefix]: $ISSUE_NUMBER [details]")
  ```
  If `envoy_publish` fails, continue — the label is the source of truth.
```

**Key constraint:** Labels are always set BEFORE the notification. The notification is an optimization (faster signal), not the authoritative state. See `daemon/envoy-auto-subscription-patterns.md` for details.

## When NOT to notify

Retry substeps within a workflow (e.g., the merge retry loop) should NOT contain `envoy_publish` calls. Only terminal exits notify. This prevents notification spam during normal retry behavior.
