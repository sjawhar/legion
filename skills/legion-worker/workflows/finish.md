# Finish Workflow

Merge the PR into main and clean up the workspace.

## Workflow

### 1. Rebase onto Main

```bash
cd "$WORKSPACE_DIR"
jj git fetch --repository "$WORKSPACE_DIR"
jj rebase -d main --repository "$WORKSPACE_DIR"
```

### 2. Resolve Conflicts

If rebase produces conflicts:

1. Check status: `jj status --repository "$WORKSPACE_DIR"`
2. Open each conflicted file and resolve markers
3. Verify no conflicts remain: `jj status` shows clean state

**If conflicts are unresolvable:**
- Add `user-input-needed` label to Linear issue
- Post comment describing the conflict
- Exit

### 3. Push

```bash
jj git push --repository "$WORKSPACE_DIR"
```

### 4. Wait for CI

```bash
gh pr checks "$LINEAR_ISSUE_ID" --watch
```

**If CI fails:**

1. Read the failure logs: `gh pr checks "$LINEAR_ISSUE_ID"`
2. Fix the issues (type errors, lint, test failures, build errors)
3. Push and re-check: `gh pr checks "$LINEAR_ISSUE_ID" --watch`
4. Repeat until CI passes

Only escalate with `user-input-needed` if something has gone fundamentally wrong (e.g., infrastructure issues, impossible conflicts, external service failures). Normal code issues like type errors should be fixed, not escalated.

### 5. Merge

```bash
gh pr merge "$LINEAR_ISSUE_ID" --squash --delete-branch
```

### 6. Clean Up Workspace

Fetch to sync the merge, then remove the workspace:

```bash
jj git fetch --repository "$LEGION_DIR"
jj workspace forget "$LINEAR_ISSUE_ID" --force --cleanup --repository "$LEGION_DIR"
```

The `--cleanup` flag deletes the workspace directory. No manual `rm -rf` needed.

### 7. Exit

Exit without adding a label. The Linear issue auto-closes when the PR merges.
