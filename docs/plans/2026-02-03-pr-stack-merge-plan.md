# PR Stack Merge Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to execute this plan. Launch one subagent per PR, working sequentially (PR #13 must merge before PR #18).

**Goal:** Safely merge the PR stack (PR #13 → PR #18) into main by fixing CI failures, running code review, and merging in order.

**Architecture:** Two stacked PRs where PR #18 depends on PR #13. We process bottom-up: fix and merge #13 first, then rebase #18, fix its issues, and merge it.

**Tech Stack:** jj (Jujutsu), GitHub CLI (`gh`), Python (ruff, basedpyright, pytest)

---

## PR Stack Overview

| PR | Branch | Base | Status |
|----|--------|------|--------|
| #13 | `feat-backlog-management` | `main` | lint/typecheck failing |
| #18 | `feat-daemon-worker-monitoring` | `feat-backlog-management` | lint/typecheck failing |

## Current CI Failures

**Lint errors (ruff):**
- `src/legion/daemon.py:14-17` - E402: Module imports not at top of file (after logging setup)
- `tests/test_daemon.py:839` - F841: Unused variable `mock_new`

**Type errors (basedpyright):**
- Import cycles in `src/legion/state/__init__.py`
- Unnecessary isinstance calls (warnings)
- Unused parameters and variables in tests
- Missing type annotations in tests

---

## Task 1: Fix and Merge PR #13 (feat-backlog-management)

**Subagent Task:** Fix CI failures on PR #13, run code review, address major findings, and merge to main.

### Step 1: Checkout the branch

```bash
jj edit xrouqwkn  # feat-backlog-management commit
```

### Step 2: Fix ruff lint errors

**File:** `src/legion/daemon.py`

Move imports to top of file, before the logging setup. The E402 errors are because imports appear after `logger = logging.getLogger(__name__)`.

Restructure the imports section:
```python
import logging
import os
from typing import TYPE_CHECKING

from legion import short_id as short_id_mod
from legion import tmux
from legion.state import types
from legion.state.types import WorkerModeLiteral
# ... rest of imports

logger = logging.getLogger(__name__)
```

**File:** `tests/test_daemon.py:839`

The variable `mock_new` is assigned but never used. Either:
- Remove the assignment if it's not needed
- Use `_` prefix: `_mock_new = mocker.patch(...)`

### Step 3: Run lint check locally

```bash
uv run ruff check .
```

Expected: No errors (warnings OK)

### Step 4: Fix basedpyright errors

Focus on actual errors, not warnings:

**File:** `src/legion/state/__init__.py`

Fix import cycles. Check what's importing from where and break the cycle by using `TYPE_CHECKING` imports or reorganizing.

**File:** `tests/test_session.py:28`

Fix the read-only attribute assignment for `Session.session_id`.

### Step 5: Run typecheck locally

```bash
uv run basedpyright .
```

Expected: No errors (warnings OK)

### Step 6: Run tests

```bash
uv run pytest
```

Expected: All tests pass

### Step 7: Push and verify CI

```bash
jj git push
```

Wait for CI to complete. Expected: All checks pass.

### Step 8: Run code review

Use the `compound-engineering:review:code-simplicity-reviewer` agent or `superpowers:requesting-code-review` skill to review the PR.

**Triage findings using the Deferred Findings Protocol (see below):**
- Fix Critical/Major findings immediately
- Log Minor/Suggestions to `scratchpad/deferred-findings.md`

First, initialize the deferred findings file if it doesn't exist:

```bash
mkdir -p /tmp/claude-1000/-home-sami-legion-default/837ac4b3-3f59-4a63-b111-7a4ca0d1531b/scratchpad
echo "# Deferred Code Review Findings\n\nFindings from PR stack merge (2026-02-03) that were not addressed.\n" > /tmp/claude-1000/-home-sami-legion-default/837ac4b3-3f59-4a63-b111-7a4ca0d1531b/scratchpad/deferred-findings.md
```

### Step 9: Push fixes if any

```bash
jj git push
```

Wait for CI. Expected: All checks pass.

### Step 10: Merge PR #13

```bash
gh pr merge 13 --squash --delete-branch
```

---

## Task 2: Fix and Merge PR #18 (feat-daemon-worker-monitoring)

**Subagent Task:** Rebase PR #18 onto updated main, fix CI failures, run code review, address major findings, and merge.

### Step 1: Fetch and rebase

```bash
jj git fetch
jj edit sorkzqws  # feat-daemon-worker-monitoring commit
jj rebase -d main
```

### Step 2: Resolve any conflicts

If conflicts arise from the merge of #13:
1. Check `jj log` for divergent commits
2. Edit conflicted files to resolve
3. Don't lose any functionality from either side

### Step 3: Fix any remaining lint errors

```bash
uv run ruff check .
```

If #13's fixes didn't cover everything, apply the same patterns:
- Move imports to top of file
- Remove or prefix unused variables

### Step 4: Fix any remaining type errors

```bash
uv run basedpyright .
```

Focus on errors, not warnings. The same patterns from Task 1 apply.

### Step 5: Run tests

```bash
uv run pytest
```

Expected: All tests pass

### Step 6: Push and verify CI

```bash
jj git push
```

Wait for CI. Note: The base branch changed from `feat-backlog-management` to `main` since #13 was merged. GitHub should auto-update, but verify.

### Step 7: Update PR base branch if needed

If GitHub didn't auto-update the base:

```bash
gh pr edit 18 --base main
```

### Step 8: Run code review

Use the `compound-engineering:review:code-simplicity-reviewer` agent or `superpowers:requesting-code-review` skill.

**Triage findings using the Deferred Findings Protocol:**
- Fix Critical/Major findings immediately
- Log Minor/Suggestions to `scratchpad/deferred-findings.md` (append, don't overwrite)

This PR builds on #13, so focus on the delta.

### Step 9: Push fixes if any

```bash
jj git push
```

Wait for CI. Expected: All checks pass.

### Step 10: Merge PR #18

```bash
gh pr merge 18 --squash --delete-branch
```

---

---

## Task 3: Create Cleanup Issue for Deferred Findings

**Subagent Task:** Collect all deferred code review findings and create a GitHub issue to track them.

### Step 1: Review the deferred findings log

During Tasks 1 and 2, any code review findings that were NOT addressed should have been logged to:

```
/tmp/claude-1000/-home-sami-legion-default/837ac4b3-3f59-4a63-b111-7a4ca0d1531b/scratchpad/deferred-findings.md
```

### Step 2: Create GitHub issue if there are deferred findings

If the deferred findings file has content:

```bash
gh issue create \
  --title "Code review cleanup: deferred findings from PR stack merge" \
  --body "$(cat /tmp/claude-1000/-home-sami-legion-default/837ac4b3-3f59-4a63-b111-7a4ca0d1531b/scratchpad/deferred-findings.md)"
```

If no findings were deferred, skip this step.

### Step 3: Report the issue URL

Output the created issue URL so it can be tracked.

---

## Deferred Findings Protocol

**During code review in Tasks 1 and 2:**

When a code review agent raises a finding, categorize it:

| Category | Action |
|----------|--------|
| **Critical** (security, data loss, crashes) | Fix immediately |
| **Major** (bugs, architectural issues) | Fix immediately |
| **Minor** (style, naming, minor refactors) | Log to deferred findings |
| **Suggestions** (nice-to-haves, optimizations) | Log to deferred findings |

**Logging format** (append to `scratchpad/deferred-findings.md`):

```markdown
## [PR #N] Finding Title

**Source:** [reviewer agent name]
**Severity:** Minor / Suggestion
**File:** `path/to/file.py:123`

**Description:**
[What the reviewer found]

**Suggested fix:**
[What they recommended]

---
```

---

## Success Criteria

- [ ] PR #13 merged to main with passing CI
- [ ] PR #18 rebased onto main and merged with passing CI
- [ ] No lint errors (E402, F841)
- [ ] No type errors (import cycles, attribute access)
- [ ] All tests passing
- [ ] Major code review findings addressed
- [ ] Deferred findings logged to cleanup issue (if any)

## Notes for Subagents

1. **Work sequentially** - PR #13 must merge before starting on #18
2. **Use jj, not git** - This repo uses Jujutsu for version control
3. **Don't over-fix** - Address CI failures and major review findings only
4. **Verify locally before pushing** - Run `ruff check .`, `basedpyright .`, `pytest` before each push
5. **Check CI after pushing** - Wait for GitHub Actions to complete
6. **Squash merge** - Use `gh pr merge --squash --delete-branch`
7. **Log deferred findings** - Append minor/suggestion-level findings to the scratchpad file; don't fix them
8. **Task 3 runs last** - Only create the cleanup issue after both PRs are merged
