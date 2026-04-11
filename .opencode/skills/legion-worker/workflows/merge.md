# Merge Workflow

Merge the PR into main, then clean up the workspace. Three-layer cleanup: merge worker (immediate),
daemon auto-cleanup (on next state collect), controller backup sweep (Step 6).

## Workflow

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

### 7.5. Cleanup Workspace

After the issue is closed and Done, clean up the workspace and worker entries to prevent disk exhaustion.
Best-effort — do not fail the merge workflow if cleanup errors occur, but only prune worker state
after workspace deletion succeeds (preserves retry handle for the daemon/controller backup layers).

**1. Remove the filesystem workspace:**
```bash
CLEANUP_OK=false
if curl -sf -X DELETE "http://127.0.0.1:$LEGION_DAEMON_PORT/workers/$LEGION_ISSUE_ID-merge/workspace" \
  -H 'Content-Type: application/json' \
  -d '{"repo": "'"$OWNER/$REPO"'"}'; then
  CLEANUP_OK=true
else
  echo 'Workspace cleanup failed (non-fatal) — daemon/controller will retry'
fi
```

**2. Prune worker entries only if workspace was removed:**
```bash
if [ "$CLEANUP_OK" = "true" ]; then
  curl -sf -X POST "http://127.0.0.1:$LEGION_DAEMON_PORT/workers/prune" \
    -H 'Content-Type: application/json' \
    -d '{"issueIds": ["'"$LEGION_ISSUE_ID"'"]}' || echo 'Worker prune failed (non-fatal)'
fi
```

**Why here?** The daemon also auto-cleans Done issues on its next state collection cycle (Layer 1 safety net),
and the controller has a backup sweep (Step 6). This merge-time cleanup is the fastest path — it reclaims
disk space immediately when the issue completes, without waiting for the next poll cycle.

Then notify the controller via Envoy (best-effort, exactly one notification):
```
envoy_publish(topic="notifications.role.legion-controller", message="Worker done: $ISSUE_NUMBER merge completed. Issue closed.")
```
If `envoy_publish` fails, continue — the label is the source of truth.

> **Note:** GitHub auto-close may also fire when the PR merges, which is fine — closing an already-closed issue is a no-op. This explicit close is a safety net for cases where auto-close doesn't trigger (e.g., issue not linked to PR, Linear backend).
