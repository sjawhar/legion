---
title: "External repo PRs don't auto-transition Linear issues"
category: integration-issues
tags:
  - linear
  - github
  - pull-requests
  - auto-transition
  - external-repos
  - worker-lifecycle
  - controller-dispatch
module: legion-worker
component: workflows/implement
symptoms:
  - controller keeps re-dispatching implement workers for same issue
  - issue stuck in "In Progress" after PR is created
  - auto-transition not firing after PR creation
  - worker-active label not being removed
  - multiple implement sessions for same completed issue
date_solved: 2026-02-15
---

# External Repo PRs Don't Auto-Transition Linear Issues

## Problem

The implement workflow assumes that creating a PR auto-transitions the Linear issue:

> "Exit without adding labels. Opening PR auto-transitions issue in Linear."

This works when the PR is on a repo linked to the Linear team's GitHub integration. But when the fix is in an **external/vendor repo** (like `obra/streamlinear` for a `LEG-*` issue), Linear has no integration with that repo and the issue state never changes.

The result is a re-dispatch loop:
1. Worker implements fix, creates PR on external repo, exits
2. Issue stays "In Progress" with no worker labels
3. Controller sees "In Progress" + no active worker → dispatches new implement worker
4. New worker finds work already done, exits
5. Repeat

In this case (LEG-126), the controller dispatched **3 implement sessions** before the issue was manually transitioned.

## Compounding Factor: Chicken-and-Egg Bug

LEG-126 was itself a fix for the Linear MCP's label update functionality. The worker couldn't properly clean up `worker-active` using the standard MCP `update` action because **that was the very bug being fixed**. The running MCP process still had the broken code. The worker had to use the raw GraphQL workaround documented in the issue itself.

## Solution

When the PR target repo is not integrated with Linear, the implement worker must **manually transition the issue state** before exiting, rather than relying on auto-transition.

### Detection

The worker can detect this situation by checking whether the PR repo matches the expected team repo. If the PR is on a fork or external repo, manual transition is needed.

### Manual Transition

```
linear_linear(action="update", id="$LINEAR_ISSUE_ID", state="In Review")
```

This should be done after creating the PR, before exiting.

## Key Takeaway

The implement workflow's "exit without adding labels" instruction assumes a specific integration topology. When that assumption breaks (external repos, forks, vendor dependencies), the worker must fall back to explicit state management.

## When This Applies

- Fixing bugs in vendor dependencies (MCP tools, external libraries)
- Creating PRs on upstream repos via fork
- Any PR target repo not linked to the Linear team's GitHub integration
- Repos where the team doesn't have direct push access (push to fork instead)

## Prevention Strategies

### For Worker Workflows
- Add a post-PR check: verify the issue state changed within a few seconds
- If state didn't change, manually transition as fallback
- Document which repos are linked to Linear and which aren't

### For Controller
- Add a timeout: if issue stays "In Progress" with no `worker-active` label for N minutes after a PR exists, flag for manual intervention rather than re-dispatching

### For Self-Referential Bugs
- When fixing tooling that the worker itself uses, identify the chicken-and-egg risk upfront
- Document known workarounds (e.g., raw GraphQL) in the plan
- Consider whether the fix should be deployed before the worker exits

## Related

- [Using PR draft status for review signaling](./github-graphql-pr-draft-status.md)
- LEG-126: Linear MCP label updates silently fail
