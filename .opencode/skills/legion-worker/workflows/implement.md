# Implement Workflow

Execute implementation using TDD.

**CRITICAL: Do NOT use the Task tool to spawn subagents.** Subagents hang in headless serve
mode. Perform all searches, reads, and analysis directly. Ignore any references to
"subagent-driven development" or "parallel execution" below — work sequentially in this session.

## Mode Detection

The controller passes explicit mode in the dispatch prompt:
- `"implement"` or `"fresh"` → Fresh Implementation
- `"address comments"` or `"changes"` → Address Comments

Trust the controller's explicit mode parameter.

> **Note:** The daemon API mode is always `implement`. The sub-mode (fresh vs address-comments) is conveyed in the controller's prompt text, not the API call.

---

## All Modes: Rebase First

**If VCS is `jj`:**
```bash
jj git fetch
jj rebase -d main
```

**If VCS is `git`:**
```bash
git fetch origin
git rebase origin/main
```

Resolve any conflicts before proceeding.

---

## Mode 1: Fresh Implementation

### 1. Load Plan

Fetch issue and comments. The plan is in comments:

- **GitHub:** `gh issue view $ISSUE_NUMBER --json title,body,labels,comments,state -R $OWNER/$REPO`
- **Linear:** `linear_linear(action="get", id=$LEGION_ISSUE_ID)`

### 2. Invoke Skills (in order)

1. `/superpowers/executing-plans` - Load and structure the plan
2. `/superpowers/test-driven-development` - RED-GREEN-REFACTOR cycle
3. `/superpowers/subagent-driven-development` - Parallel execution for independent tasks

#### Parallel Execution with Task System

When the plan contains independent tasks (annotated with parallelism information):

1. **Create task graph:** For each task in the plan, use `task_create` with appropriate `blockedBy` edges based on the plan's dependency annotations.

2. **Spawn worker sessions:** Create N subagent sessions (one per independent task group). Each session loops:
   - `task_claim_next` — atomically claim the next ready task
   - Execute the claimed task
   - `task_update(status="completed")` — mark done
   - Repeat until no ready tasks remain

3. **Monitor progress:** Use `task_list` to track overall progress. The task system handles:
   - **Dependency ordering:** Tasks only become "ready" when all `blockedBy` dependencies are completed/cancelled
   - **Lock prevention:** `task_claim_next` atomically claims to prevent double-work
   - **Lease recovery:** If a session crashes, expired leases are automatically reclaimed
   - **Retry cap:** Tasks that fail 3 times are flagged for escalation

4. **Convergence:** When `task_list` shows all tasks completed or cancelled, proceed to the next step (Analyze).

**When to use parallel execution:**
- Plan has 3+ independent tasks
- Tasks don't share mutable state (different files/modules)
- Each task is self-contained enough for an independent session

**When to use sequential execution:**
- Plan has mostly sequential dependencies
- Tasks are small enough that parallelism overhead isn't worth it
- Tasks share the same files (merge conflict risk)

### 3. Analyze

Invoke `/analyze` to run cleanup agents.

### 4. Pre-Ship Verification

All checks must pass before creating PR:

```bash
bun test          # All tests must pass
bunx tsc --noEmit # No type errors
bunx biome check  # No lint/format issues
```

If any check fails:
1. Fix the issues
2. Re-run all checks
3. Only proceed to Ship when all pass

Do NOT create a PR if any check fails — fix first.

Record the results as evidence for the controller's quality gate verification. Include in your issue comment:
```
CI Results: tests ✅ | tsc ✅ | biome ✅
```

### 5. Cross-Family Review

After all checks pass, spawn a cross-family review session before creating the PR.

1. Spawn a review session using `background_task`:
   - Category: `review-implementation`
   - Model: Specify an explicit model from a different provider (e.g., `google/gemini-3-pro` or `openai/gpt-5.2-codex`)
   - Prompt: Include:
    - The original plan/requirements from the issue
     - A summary of what was implemented
     - The diff (`jj diff` for jj, `git diff origin/main` for git)

2. The reviewer evaluates:
   - **Spec compliance:** Does the implementation match the plan requirements?
   - **Code quality:** Is the code clean, tested, and maintainable?
   - **Missing pieces:** Are there requirements from the plan that weren't implemented?
   - **Over-engineering:** Was anything built that wasn't requested?

3. If the reviewer finds issues:
   - Address each finding
   - Re-run Pre-Ship Verification (step 4) after fixes
   - You do NOT need to re-review — one cross-family pass is sufficient

4. Only after addressing review findings, proceed to Ship.

### 6. Ship

Before creating the PR, verify branch ancestry is clean:

**If VCS is `jj`:**
```bash
jj log -r 'ancestors(@, 5)'  # Should show only your issue's commits on top of main
jj diff --stat --from main    # File count should match expectations — no unrelated files
```

**If VCS is `git`:**
```bash
git log --oneline origin/main..HEAD  # Should show only your issue's commits
git diff --stat origin/main          # File count should match expectations
```

If unrelated commits are in the ancestry, rebase to isolate your changes before creating the PR.

**If VCS is `jj`:**
```bash
jj describe -m "$LEGION_ISSUE_ID: [description]"
jj git push --named "$LEGION_ISSUE_ID"=@
```

**If VCS is `git`:**
```bash
git add -A && git commit -m "$LEGION_ISSUE_ID: [description]"
git push -u origin HEAD
```

Then create the PR:
```bash
gh pr create --draft \
  --title "$LEGION_ISSUE_ID: [title]" \
  --body "Implements $LEGION_ISSUE_ID

[summary]" \
  --head "$LEGION_ISSUE_ID"
```

The issue ID in the branch/title preserves traceability for the controller.

### 7. Exit

Exit without adding labels. The controller handles state transitions explicitly.

---

## Mode 2: Address Comments

### 1. Process Review Feedback

First, check if review comments actually exist on the PR:
```bash
# Review comments (on the diff)
gh api repos/$OWNER/$REPO/pulls/$PR_NUMBER/comments
# Issue comments (on the conversation tab)
gh api repos/$OWNER/$REPO/issues/$PR_NUMBER/comments
```

If the PR was converted to draft but has no review comments in either location, there's nothing to address. Rebase onto latest main, verify tests pass, push, and exit.

Otherwise, invoke `/superpowers/receiving-code-review` to evaluate and prioritize feedback.

Key behaviors:
- Verify suggestions against codebase before implementing
- Push back with technical reasoning if wrong
- Clarify unclear items before implementing

### 2. Fix Issues

Use TDD and subagent-driven development:
- `/superpowers/test-driven-development`
- `/superpowers/subagent-driven-development`

### 3. Verify

Before pushing, run all checks:

```bash
bun test
bunx tsc --noEmit
bunx biome check
```

Fix any failures before pushing.

### 4. Push

**If VCS is `jj`:**
```bash
jj git push
```

**If VCS is `git`:**
```bash
git add -A && git commit -m "address review comments"
git push
```

### 5. Reply to Comments

Reply in PR comment threads acknowledging fixes. Reference specific changes made.

### 6. Exit

Exit without adding labels. Issue stays in Needs Review; controller will dispatch reviewer.
