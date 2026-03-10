# Implement Workflow

Execute implementation using TDD with subagent-driven development.

## Mode Detection

The controller passes explicit mode in the dispatch prompt:
- `"implement"` or `"fresh"` → Fresh Implementation
- `"address comments"` or `"changes"` → Address Comments

Trust the controller's explicit mode parameter.

> **Note:** The daemon API mode is always `implement`. The sub-mode (fresh vs address-comments) is conveyed in the controller's prompt text, not the API call.

## Tools Referenced

This workflow references environment-provided tools. These are available in the OpenCode runtime, not defined in this repo:

| Tool | Source | Purpose |
|------|--------|---------|
| `task_create` | OpenCode task system | Create a task with dependencies (`blockedBy`) |
| `task_claim_next` | OpenCode task system | Atomically claim the next ready task |
| `task_update` | OpenCode task system | Mark task completed/failed |
| `task_list` | OpenCode task system | List tasks and their status |
| `background_task` | OpenCode agent system | Spawn a background subagent |
| `/analyze` | `sjawhar/analyze` skill | Run code quality agents on recent changes |

The task system enables parallel execution with dependency ordering. If these tools are unavailable in your environment, execute tasks sequentially following the plan's dependency annotations.
---

## All Modes: Rebase First

```bash
jj git fetch
jj rebase -d main
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

#### Wave-Based Parallelism

When a plan has both independent AND dependent tasks, group them into **waves**:

```
Wave 1 (parallel): T1 (jj.py), T2 (task_state.py), T3 (source_detection.py)
  ↓ all complete
Wave 2 (parallel): T4 (task_commands.py), T5 (merge_utils.py)
  ↓ all complete  
Wave 3 (parallel): T6 (tests), T7 (integration)
```

**The critical rule: never dispatch multiple subagents that edit the same file.**
Concurrent edits to the same file cause silent overwrites — the last writer wins and
earlier agents' work is lost. If two tasks both need to modify `task_commands.py`,
they must be in the same wave (sequential) or one must `blockedBy` the other.

**Grouping into waves:**
1. List all tasks and which files they create or modify
2. Tasks that touch disjoint files can run in parallel (same wave)
3. Tasks that share a file must be sequential (different waves, with dependency edges)
4. Within a wave, all tasks must complete before the next wave starts

**Wave failure handling:** If a task in a wave fails, it follows the existing retry
policy (3 attempts before escalation). Other tasks in the wave continue independently.
The next wave does NOT start until all tasks in the current wave are completed or cancelled.

### 3. Analyze

Invoke `/analyze` to run cleanup agents.

### 4. Pre-Ship Verification

All checks must pass before creating PR:

```bash
bun test          # All tests must pass
bunx tsc --noEmit # No type errors
bunx biome check  # No lint/format issues
```

**For Python code**, also run the project's Python checks:
```bash
cd meta/trajectory_labs  # or relevant Python package
uv run ruff check --fix src/ tests/
uv run ruff format src/ tests/
uv run pytest
```

If any check fails:
1. Fix the issues
2. Re-run all checks
3. Only proceed to Ship when all pass

Do NOT create a PR if any check fails — fix first.

### 4.5. Documentation

For any user-facing behavior change, update relevant documentation before creating the PR:

- **README** — if the feature changes setup, usage, or configuration
- **Usage guides** — if the feature adds new user-facing functionality
- **API docs** — if the feature changes or adds API endpoints
- **Inline help** — if the feature adds CLI commands or options

Documentation should explain **how to use** the feature, not just what changed in the code. A user reading only the docs should be able to understand and use the new functionality.

Skip this step if the change is purely internal (refactoring, bug fix with no behavior change, test-only changes).
### 5. Cross-Family Review

After all checks pass, spawn a cross-family review session before creating the PR.

1. Spawn a review session using `background_task`:
   - Category: `review-implementation`
   - Model: Specify an explicit model from a different provider (e.g., `google/gemini-3-pro` or `openai/gpt-5.2-codex`)
   - Prompt: Include:
    - The original plan/requirements from the issue
     - A summary of what was implemented
     - The diff (`jj diff`)

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
```bash
jj log -r 'ancestors(@, 5)'  # Should show only your issue's commits on top of main
jj diff --stat --from main    # File count should match expectations — no unrelated files
```

If unrelated commits are in the ancestry, rebase to isolate your changes before creating the PR.

```bash
jj describe -m "$LEGION_ISSUE_ID: [description]"
jj git push --named "$LEGION_ISSUE_ID"=@

gh pr create --draft \
  --title "$LEGION_ISSUE_ID: [title]" \
  --body "Closes #$ISSUE_NUMBER

## Summary
[summary]" \
  --head "$LEGION_ISSUE_ID" \
  -R $OWNER/$REPO
```

The issue ID in the branch/title preserves traceability for the controller.

### 7. Wait for CI

After pushing, wait for CI to complete:
```bash
gh pr checks "$LEGION_ISSUE_ID" --watch
```

**If CI fails:** Read the failure logs, fix the issues, push again, and re-check.
Do NOT exit with failing CI — it's your job to get CI green before the reviewer sees the PR.

**Note:** Some repositories suppress CI on draft PRs. If `gh pr checks --watch` hangs
with no checks reported, convert the PR to ready (`gh pr ready`), wait for CI, then
convert back to draft (`gh pr ready --undo`) if needed.

### 8. Exit

**CRITICAL: The `worker-done` label is how the controller knows you finished.** If you skip this,
the issue silently stalls and no one advances it. This is the MOST IMPORTANT step.

First, verify the PR is still open (force-pushes can accidentally close PRs):

**GitHub:**
```bash
PR_STATE=$(gh pr view $ISSUE_NUMBER --json state --jq '.state' -R $OWNER/$REPO 2>/dev/null)
if [ "$PR_STATE" = "CLOSED" ]; then
  gh pr reopen $ISSUE_NUMBER -R $OWNER/$REPO
  echo "PR was accidentally closed by force-push, reopened"
fi
```

Then add the label and verify it was applied:

**GitHub:**
```bash
gh issue edit $ISSUE_NUMBER --add-label "worker-done" --remove-label "worker-active" -R $OWNER/$REPO
# Verify the label was actually applied
LABELS=$(gh issue view $ISSUE_NUMBER --json labels --jq '[.labels[].name] | join(",")' -R $OWNER/$REPO)
if ! echo "$LABELS" | grep -q "worker-done"; then
  echo "WARNING: worker-done label not applied, retrying"
  gh issue edit $ISSUE_NUMBER --add-label "worker-done" -R $OWNER/$REPO
fi
```

**Linear:**
```
issue = linear_linear(action="get", id=$LEGION_ISSUE_ID)
current_labels = [l.name for l in issue.labels if l.name != "worker-active"]
linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current_labels, "worker-done"])
```

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

```bash
jj git push
```

### 4.5. Wait for CI

```bash
gh pr checks "$LEGION_ISSUE_ID" --watch
```

**If CI fails:** Read the failure logs, fix the issues, push again, and re-check.
Do NOT reply to comments or exit with failing CI.

### 5. Reply to Comments

Reply in PR comment threads acknowledging fixes. Reference specific changes made.

### 6. Exit

**CRITICAL: The `worker-done` label is how the controller knows you finished.** If you skip this,
the issue silently stalls and no one advances it. This is the MOST IMPORTANT step.

First, verify the PR is still open (force-pushes can accidentally close PRs):

**GitHub:**
```bash
PR_STATE=$(gh pr view $ISSUE_NUMBER --json state --jq '.state' -R $OWNER/$REPO 2>/dev/null)
if [ "$PR_STATE" = "CLOSED" ]; then
  gh pr reopen $ISSUE_NUMBER -R $OWNER/$REPO
  echo "PR was accidentally closed by force-push, reopened"
fi
```

Then add the label and verify it was applied:

**GitHub:**
```bash
gh issue edit $ISSUE_NUMBER --add-label "worker-done" --remove-label "worker-active" -R $OWNER/$REPO
# Verify the label was actually applied
LABELS=$(gh issue view $ISSUE_NUMBER --json labels --jq '[.labels[].name] | join(",")' -R $OWNER/$REPO)
if ! echo "$LABELS" | grep -q "worker-done"; then
  echo "WARNING: worker-done label not applied, retrying"
  gh issue edit $ISSUE_NUMBER --add-label "worker-done" -R $OWNER/$REPO
fi
```

**Linear:**
```
issue = linear_linear(action="get", id=$LEGION_ISSUE_ID)
current_labels = [l.name for l in issue.labels if l.name != "worker-active"]
linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current_labels, "worker-done"])
```
