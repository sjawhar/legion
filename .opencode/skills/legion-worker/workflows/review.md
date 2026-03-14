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

Also read all prior handoffs for full context chain:

```bash
legion handoff read 2>/dev/null || echo '{}'
```

If present, implementer's `trickyParts` and `deviations` can highlight areas to review more carefully. This is advisory.

### 2. Run Review

Invoke `/ce:review` with the branch name.

Pass the context gathered in step 1. The review skill will:
- Dispatch multiple reviewer agents in parallel
- Analyze the code in the workspace
- Check against requirements
- Identify issues by severity (CRITICAL/P1, IMPORTANT/P2, MINOR/P3)

### 2.5. Check CI Status

Check whether CI is passing on the PR:
```bash
gh pr checks "$LEGION_ISSUE_ID"
```

Include the CI status in your review summary (step 3). If CI is failing, note which
checks are failing and treat it as a P1 issue — the implementer should have fixed this
before opening the PR.

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

### 4.5. Write Handoff Data

Write handoff data (non-blocking) — BEFORE setting PR draft status:

```bash
legion handoff write --phase review --data '{
  "critical": 0,
  "important": 2,
  "minor": 3,
  "verdict": "approved",
  "keyFindings": [
    {"severity": "P2", "file": "src/auth.ts", "description": "Missing null check on line 45"},
    {"severity": "P2", "file": "src/session.ts", "description": "Session TTL should be configurable"}
  ]
}' 2>/dev/null || true
```

Replace the example counts and findings with actual review results:
- `critical`: count of CRITICAL/P1 issues found
- `important`: count of IMPORTANT/P2 issues found
- `minor`: count of MINOR/P3 suggestions found
- `verdict`: "approved" if no CRITICAL issues, "changes_requested" if any CRITICAL issues found
- `keyFindings`: list of `{"severity": "P1"|"P2"|"P3", "file": "path", "description": "..."}`


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

**CRITICAL: The `worker-done` label is how the controller knows you finished.** If you skip this,
the issue silently stalls. This is the MOST IMPORTANT step.

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
