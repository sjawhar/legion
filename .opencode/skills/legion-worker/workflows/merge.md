# Merge Workflow

Merge the PR into main, then clean up the workspace. Three-layer cleanup: merge worker (immediate),
daemon auto-cleanup (on next state collect), controller backup sweep (Step 6).

## Workflow

### 1. Check PR State

Before doing anything, verify the PR is open and mergeable:

```bash
gh pr view "$LEGION_ISSUE_ID" --json state,merged,mergeable
```

- If **already merged**: verify changes are on main (`jj log --revisions main`), then notify the controller and exit cleanly:
  - Remove `worker-active`:
    - **GitHub:** `gh issue edit $ISSUE_NUMBER --remove-label "worker-active" -R $OWNER/$REPO`
    - **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current without "worker-active"])`
  - Notify controller (best-effort):
    ```
    envoy_publish(topic="notifications.role.legion-controller", message="Worker done: $ISSUE_NUMBER merge skipped — PR already merged")
    ```
    If `envoy_publish` fails, continue — the label is the source of truth.
  - Exit
- If **closed without merge**: something unexpected happened.
  - Post comment:
    - **GitHub:** `gh issue comment $ISSUE_NUMBER --body "PR was closed without merging. Unexpected state — needs investigation." -R $OWNER/$REPO`
    - **Linear:** `linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="PR was closed without merging. Unexpected state.")`
  - Add `user-input-needed`:
    - **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "user-input-needed" --remove-label "worker-active" -R $OWNER/$REPO`
    - **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current without "worker-active" plus "user-input-needed"])`
  - Notify controller (best-effort):
    ```
    envoy_publish(topic="notifications.role.legion-controller", message="Worker failed: $ISSUE_NUMBER merge failed — PR closed without merge")
    ```
    If `envoy_publish` fails, continue — the label is the source of truth.
  - Exit
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
- Notify controller (best-effort):
  ```
  envoy_publish(topic="notifications.role.legion-controller", message="Worker failed: $ISSUE_NUMBER merge failed — unresolvable rebase conflict")
  ```
  If `envoy_publish` fails, continue — the label is the source of truth.
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

**If CI fails with a fundamental issue** (infrastructure failures, impossible conflicts, external service errors — NOT normal code issues):
- Post comment explaining the fundamental failure:
  - **GitHub:** `gh issue comment $ISSUE_NUMBER --body "Merge blocked: CI has a fundamental failure that cannot be fixed by the merge worker. [describe the issue]" -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="Merge blocked: fundamental CI failure.")`
- Add `user-input-needed` label:
  - **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "user-input-needed" --remove-label "worker-active" -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current without "worker-active" plus "user-input-needed"])`
- Notify controller (best-effort):
  ```
  envoy_publish(topic="notifications.role.legion-controller", message="Worker failed: $ISSUE_NUMBER merge blocked — fundamental CI failure")
  ```
  If `envoy_publish` fails, continue — the label is the source of truth.
- Exit

### 6. Merge (with conflict retry)

Attempt to merge. If the merge fails due to a race condition (main moved since rebase), retry.

```bash
gh pr merge "$LEGION_ISSUE_ID" --squash --delete-branch
```

**If merge succeeds:** Proceed to step 7.

**If merge fails — classify the failure** by checking PR state:

```bash
gh pr view "$LEGION_ISSUE_ID" --json state,mergeable,mergeStateStatus -R $OWNER/$REPO
```

**If `state` is `MERGED`:** The PR was merged by another process (manual merge, auto-merge). Proceed to step 7 — this is a success, not a failure.

**If `mergeStateStatus` is `BEHIND` or `DIRTY` (race condition — main moved):**

Retry up to **2 times** (track your retry count):

1. Rebase onto latest main:
   ```bash
   jj git fetch
   jj rebase -d main
   ```
2. Verify clean rebase — `jj status` must show no conflicts. If conflicts exist, resolve them (same as step 3). If conflicts are unresolvable, exit via the unresolvable-conflict path in step 3.
3. Push: `jj git push`
4. Wait for CI: `gh pr checks "$LEGION_ISSUE_ID" --watch` — fix any CI failures (same as step 5)
5. Retry merge: `gh pr merge "$LEGION_ISSUE_ID" --squash --delete-branch`
6. If merge succeeds, proceed to step 7. If it fails again, go back to substep 1 (unless retries exhausted).

**If merge still fails after 2 retries:**
- Post comment:
  - **GitHub:** `gh issue comment $ISSUE_NUMBER --body "Merge failed after 2 conflict retries. The PR keeps falling behind main despite rebasing. Manual intervention needed." -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="Merge failed after 2 conflict retries. Manual intervention needed.")`
- Add `user-input-needed` label:
  - **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "user-input-needed" --remove-label "worker-active" -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current without "worker-active" plus "user-input-needed"])`
- Notify controller (best-effort):
  ```
  envoy_publish(topic="notifications.role.legion-controller", message="Worker failed: $ISSUE_NUMBER merge failed after 2 conflict retries")
  ```
  If `envoy_publish` fails, continue — the label is the source of truth.
- Exit

**If merge fails for any other reason** (permission error, unknown error — inspect the error output to determine the cause):
- Post a comment describing the failure and what you observed:
  - **GitHub:** `gh issue comment $ISSUE_NUMBER --body "Merge failed: [describe the error — permission denied, unexpected API error, etc.]. Manual intervention needed." -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="Merge failed: [describe error].")`
- Add `user-input-needed` label:
  - **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "user-input-needed" --remove-label "worker-active" -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current without "worker-active" plus "user-input-needed"])`
- Notify controller (best-effort):
  ```
  envoy_publish(topic="notifications.role.legion-controller", message="Worker failed: $ISSUE_NUMBER merge failed — [brief error description]")
  ```
  If `envoy_publish` fails, continue — the label is the source of truth.
- Exit

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
