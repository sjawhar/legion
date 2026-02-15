# Merge Workflow

Merge the PR into main. The controller handles workspace cleanup after completion.

## Workflow

### 1. Check PR State

Before doing anything, verify the PR is open and mergeable:

```bash
gh pr view "$LINEAR_ISSUE_ID" --json state,merged,mergeable
```

- If **already merged**: verify changes are on main (`jj log --revisions main`), then exit cleanly.
- If **closed without merge**: escalate with `user-input-needed` — something unexpected happened.
- If **open**: proceed to step 2.

### 2. Rebase onto Main

```bash
jj git fetch
jj rebase -d main
```

### 3. Resolve Conflicts

If rebase produces conflicts:

1. Check status: `jj status`
2. Open each conflicted file and resolve markers
3. Verify no conflicts remain: `jj status` shows clean state

**If conflicts are unresolvable:**
- Add `user-input-needed` label to Linear issue
- Post comment describing the conflict
- Exit

### 4. Push

```bash
jj git push
```

### 5. Wait for CI

```bash
gh pr checks "$LINEAR_ISSUE_ID" --watch
```

**If CI fails:**

1. Read the failure logs: `gh pr checks "$LINEAR_ISSUE_ID"`
2. Fix the issues (type errors, lint, test failures, build errors)
3. Push and re-check: `gh pr checks "$LINEAR_ISSUE_ID" --watch`
4. Repeat until CI passes

Only escalate with `user-input-needed` if something has gone fundamentally wrong (e.g., infrastructure issues, impossible conflicts, external service failures). Normal code issues like type errors should be fixed, not escalated.

### 6. Merge

```bash
gh pr merge "$LINEAR_ISSUE_ID" --squash --delete-branch
```

**If merge fails with a permission error** (e.g., external repo you don't own):
- Post a comment to Linear explaining the permission issue
- Add `user-input-needed` label
- Exit — the user needs to merge manually or grant access

### 7. Exit

Exit without adding a label. The Linear issue auto-closes when the PR merges. The controller will clean up the workspace.
