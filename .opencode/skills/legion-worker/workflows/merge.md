# Merge Workflow

Merge the PR into main. The controller handles workspace cleanup after completion.

## Workflow

### 0.5. Load Repo Config

Read repo config from workspace root:

```bash
cat .legion/config.yml 2>/dev/null || true
```

Apply @references/config.md semantics for `merge` mode:
- Parse recognized keys
- Merge `phases.merge.*` overrides on top of top-level values
- Echo recognized keys/effective values
- Missing/malformed config falls back to defaults

Merge-mode keys:
- `merge.require_smoke_test`
- `merge.require_reporter_approval`
- `merge.auto_merge_allowed`

### 1. Check PR State

Before doing anything, verify the PR is open and mergeable:

```bash
gh pr view "$LEGION_ISSUE_ID" --json state,merged,mergeable
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
- Add `user-input-needed` label to the issue
  - **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "user-input-needed" --remove-label "worker-active" -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current without "worker-active" plus "user-input-needed"])`
- Post comment describing the conflict
  - **GitHub:** `gh issue comment $ISSUE_NUMBER --body "..." -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="...")`
- Exit

**Protected files:** The `.legion/` directory contains handoff data between pipeline phases. These files are intentional and must be preserved during merge — do not remove them or add `.legion/` to `.gitignore`.

### 4. Push

```bash
jj git push
```

### 5. Wait for CI

```bash
gh pr checks "$LEGION_ISSUE_ID" --watch
```

**If CI fails:**

1. Read the failure logs: `gh pr checks "$LEGION_ISSUE_ID"`
2. Fix the issues (type errors, lint, test failures, build errors)
3. Push and re-check: `gh pr checks "$LEGION_ISSUE_ID" --watch`
4. Repeat until CI passes

Only escalate with `user-input-needed` if something has gone fundamentally wrong (e.g., infrastructure issues, impossible conflicts, external service failures). Normal code issues like type errors should be fixed, not escalated.

### 6. Merge

Before running merge command, enforce config gates:

1. If `merge.auto_merge_allowed=false`: add `user-input-needed`, post a comment requesting human approval, remove `worker-active`, and exit.
2. If `merge.require_reporter_approval=true`: verify reporter sign-off exists in issue/PR comments. If missing, add `user-input-needed`, post comment asking reporter approval, remove `worker-active`, and exit.
3. If `merge.require_smoke_test=true`: read test handoff (`legion handoff read --phase test --workspace .`) and verify smoke/behavioral evidence exists. If missing, post blocking comment and exit without merge.

```bash
gh pr merge "$LEGION_ISSUE_ID" --squash --delete-branch
```

**If merge fails with a permission error** (e.g., external repo you don't own):
- Post a comment explaining the permission issue
  - **GitHub:** `gh issue comment $ISSUE_NUMBER --body "..." -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="...")`
- Add `user-input-needed` label
  - **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "user-input-needed" --remove-label "worker-active" -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current without "worker-active" plus "user-input-needed"])`
- Exit — the user needs to merge manually or grant access

### 7. Close Issue

After successful merge, explicitly close the issue to transition to Done:

**GitHub:**
```bash
gh issue close $ISSUE_NUMBER -R $OWNER/$REPO --comment "Closed via PR merge."
```

**Linear:**
```
linear_linear(action="update", id=$LEGION_ISSUE_ID, state="Done")
```

Then remove `worker-active` if present:
- **GitHub:** `gh issue edit $ISSUE_NUMBER --remove-label "worker-active" -R $OWNER/$REPO`
- **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current labels without "worker-active"])`

> **Note:** GitHub auto-close may also fire when the PR merges, which is fine — closing an already-closed issue is a no-op. This explicit close is a safety net for cases where auto-close doesn't trigger (e.g., issue not linked to PR, Linear backend).
