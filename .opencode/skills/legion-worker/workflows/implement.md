# Implement Workflow

Execute implementation using TDD.

## Mode Detection

The controller passes explicit mode in the dispatch prompt:
- `"implement"` or `"fresh"` → Fresh Implementation
- `"address comments"` or `"changes"` → Address Comments

Trust the controller's explicit mode parameter.

> **Note:** The daemon API mode is always `implement`. The sub-mode (fresh vs address-comments) is conveyed in the controller's prompt text, not the API call.

---

## All Modes: Rebase First

**jj:**
```bash
jj git fetch
jj rebase -d main
```

**git:**
```bash
git fetch origin
git pull --rebase origin main
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

Execute plan steps sequentially. For independent tasks, work through them one at a time.

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

### 5. Self-Review

After all checks pass, review your own work before creating the PR:

1. Review the diff against the original plan/requirements:
   - **jj:** `jj diff`
   - **git:** `git diff origin/main`

2. Evaluate:
   - **Spec compliance:** Does the implementation match the plan requirements?
   - **Code quality:** Is the code clean, tested, and maintainable?
   - **Missing pieces:** Are there requirements from the plan that weren't implemented?
   - **Over-engineering:** Was anything built that wasn't requested?

3. If you find issues, fix them and re-run Pre-Ship Verification (step 4).

### 6. Ship

Before creating the PR, verify branch ancestry is clean:

**jj:**
```bash
jj log -r 'ancestors(@, 5)'  # Should show only your issue's commits on top of main
jj diff --stat --from main    # File count should match expectations — no unrelated files
```

**git:**
```bash
git log --oneline origin/main..HEAD  # Should show only your issue's commits
git diff --stat origin/main           # File count should match expectations
```

If unrelated commits are in the ancestry, rebase to isolate your changes before creating the PR.

**jj:**
```bash
jj describe -m "$LEGION_ISSUE_ID: [description]"
jj git push --named "$LEGION_ISSUE_ID"=@
```

**git:**
```bash
git add -A
git commit -m "$LEGION_ISSUE_ID: [description]"
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

Use TDD:
- `/superpowers/test-driven-development`

### 3. Verify

Before pushing, run all checks:

```bash
bun test
bunx tsc --noEmit
bunx biome check
```

Fix any failures before pushing.

### 4. Push

**jj:** `jj git push`

**git:** `git add -A && git commit -m "$LEGION_ISSUE_ID: address review comments" && git push`

### 5. Reply to Comments

Reply in PR comment threads acknowledging fixes. Reference specific changes made.

### 6. Exit

Exit without adding labels. Issue stays in Needs Review; controller will dispatch reviewer.
