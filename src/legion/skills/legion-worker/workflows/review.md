# Review Workflow

Deep PR review with line-level comments. Code is already local in the workspace.

## Constraint

**Cannot approve/request changes via GitHub API** - same user as PR author.
Signal outcome via GitHub PR labels instead.

## Workflow

### 1. Gather Context

Fetch the Linear issue:
```
mcp__linear__get_issue with id: $LINEAR_ISSUE_ID
```

Extract:
- Original requirements from description
- Implementation plan (from comments)
- Acceptance criteria

Fetch the PR metadata:
```bash
gh pr view "$LINEAR_ISSUE_ID" --json title,body,headRefName
```

**The code is already in the workspace** - review locally, no need to fetch diff remotely.

### 2. Run Review

Invoke `/compound-engineering:workflows:review` with the branch name.

Pass the context gathered in step 1. The review skill will:
- Dispatch multiple reviewer agents in parallel
- Analyze the code in the workspace
- Check against requirements
- Identify issues by severity (CRITICAL/P1, IMPORTANT/P2, MINOR/P3)

### 3. Post Summary Comment

Post a top-level PR comment with the review summary:

```bash
gh pr comment "$LINEAR_ISSUE_ID" --body "## Review Summary

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
  --field commit_id="$(gh pr view $LINEAR_ISSUE_ID --json headRefOid -q .headRefOid)" \
  --field path="[file_path]" \
  --field line=[line_number]
```

Group related issues when they affect the same area.

### 5. Add GitHub PR Label

**Order matters:** Add PR label BEFORE `worker-done` to avoid race condition with controller.

```bash
# If any CRITICAL/P1 issues found:
gh pr edit "$LINEAR_ISSUE_ID" --add-label "worker-changes-requested"

# If no CRITICAL issues:
gh pr edit "$LINEAR_ISSUE_ID" --add-label "worker-approved"
```

### 6. Signal Completion

Add `worker-done` to the Linear issue (see @references/linear-labels.md), then exit.
