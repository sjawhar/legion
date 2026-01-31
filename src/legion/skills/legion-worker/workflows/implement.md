# Implement Workflow

Execute implementation using TDD with subagent-driven development.

## Mode Detection

The controller passes explicit mode in the dispatch prompt:
- `"implement"` or `"fresh"` → Fresh Implementation
- `"address comments"` or `"changes"` → Address Comments

Trust the controller's explicit mode parameter.

---

## All Modes: Rebase First

```bash
cd "$WORKSPACE_DIR"
jj git fetch --repository "$WORKSPACE_DIR"
jj rebase -d main --repository "$WORKSPACE_DIR"
```

Resolve any conflicts before proceeding.

---

## Mode 1: Fresh Implementation

### 1. Load Plan

Fetch issue and comments with `mcp__linear__get_issue`. The plan is in comments.

### 2. Invoke Skills (in order)

1. `/superpowers:executing-plans` - Load and structure the plan
2. `/superpowers:test-driven-development` - RED-GREEN-REFACTOR cycle
3. `/superpowers:subagent-driven-development` - Parallel execution for independent tasks

### 3. Analyze

Invoke `/analyze` to run cleanup agents.

### 4. Ship

```bash
jj describe -m "$LINEAR_ISSUE_ID: [description]" --repository "$WORKSPACE_DIR"
jj git push --named "$LINEAR_ISSUE_ID"=@ --repository "$WORKSPACE_DIR"

gh pr create \
  --title "$LINEAR_ISSUE_ID: [title]" \
  --body "Implements $LINEAR_ISSUE_ID

[summary]" \
  --head "$LINEAR_ISSUE_ID" \
  --repo "$(cd "$LEGION_DIR" && gh repo view --json nameWithOwner -q .nameWithOwner)"
```

Linear auto-associates the PR via the issue ID in the branch/title.

### 5. Exit

Exit without adding labels. Opening PR auto-transitions issue in Linear.

---

## Mode 2: Address Comments

### 1. Process Review Feedback

Invoke `/superpowers:receiving-code-review` to evaluate and prioritize feedback.

Key behaviors:
- Verify suggestions against codebase before implementing
- Push back with technical reasoning if wrong
- Clarify unclear items before implementing

### 2. Fix Issues

Use TDD and subagent-driven development:
- `/superpowers:test-driven-development`
- `/superpowers:subagent-driven-development`

### 3. Push

```bash
jj git push --repository "$WORKSPACE_DIR"
```

### 4. Reply to Comments

Reply in PR comment threads acknowledging fixes. Reference specific changes made.

### 5. Exit

Exit without adding labels. Issue stays in Needs Review; controller will dispatch reviewer.
