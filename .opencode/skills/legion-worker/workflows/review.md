# Review Workflow

Deep PR review with line-level comments. Code is already local in the workspace.

## Constraint

**Cannot approve/request changes via GitHub API** - same user as PR author.
Signal outcome via PR draft status instead (draft = changes requested, ready = approved).

## Workflow

### 1. Gather Context

Fetch the issue:

**GitHub:**

```
gh issue view $ISSUE_NUMBER --json title,body,labels,comments,state -R $OWNER/$REPO
```

**Linear:**

```
linear_linear(action="get", id=$LEGION_ISSUE_ID)
```

Extract:
- Original requirements from description
- Implementation plan (from comments)
- Acceptance criteria

Fetch the PR metadata:
```bash
gh pr view "$LEGION_ISSUE_ID" --json title,body,headRefName
```

**The code is already in the workspace** - review locally, no need to fetch diff remotely.

### 2. Run Review

Invoke `/compound-engineering/workflows/review` with the branch name.

Pass the context gathered in step 1. The review skill will:
- Dispatch multiple reviewer agents in parallel
- Analyze the code in the workspace
- Check against requirements
- Identify issues by severity (CRITICAL/P1, IMPORTANT/P2, MINOR/P3)

### 3. Post Summary Comment

Post a top-level PR comment with the review summary:

```bash
gh pr comment "$LEGION_ISSUE_ID" --body "## Review Summary

**CRITICAL (P1):** N issues
**IMPORTANT (P2):** N issues
**MINOR (P3):** N suggestions

[Brief verdict: approved to merge / needs changes]

---
[Detailed summary of key findings]"
```

### 4. Post Line-Level Comments

For each finding, post a line-level comment:

```bash
gh api repos/{owner}/{repo}/pulls/{pr_number}/comments \
  --method POST \
  --field body="**[SEVERITY]:** [description]" \
  --field commit_id="$(gh pr view $LEGION_ISSUE_ID --json headRefOid -q .headRefOid)" \
  --field path="[file_path]" \
  --field line=[line_number]
```

Group related issues when they affect the same area.

### 5. Set PR Draft Status

**Order matters:** Set draft status BEFORE `worker-done` to avoid race condition with controller.

Every review MUST set the PR draft status based on findings. This is how the controller knows the review outcome.

```bash
# If any CRITICAL/P1 issues found — convert to draft (changes requested):
gh pr ready "$LEGION_ISSUE_ID" --undo

# If no CRITICAL issues — mark ready for merge (approved):
gh pr ready "$LEGION_ISSUE_ID"
```

### 6. Signal Completion

Add `worker-done` to the issue, then exit:

- **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "worker-done" -R $OWNER/$REPO`
- **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current + "worker-done"])`
