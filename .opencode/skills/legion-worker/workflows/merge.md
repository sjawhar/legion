# Merge Workflow

Prepare the PR and enable GitHub native auto-merge. GitHub merges when the required checks and
review are satisfied. If the PR merges immediately (the common case when branch protection is not
applied), the worker performs explicit issue-close verification and workspace cleanup. If the PR
remains open (auto-merge armed, waiting on required checks), the controller observes the merge and
handles post-merge transitions.

## Workflow

### 1. Check PR State

Before doing anything, verify the PR is open and mergeable:

```bash
gh pr view "$LEGION_ISSUE_ID" --json state,merged,mergeable
```

- If **already merged**: The PR is merged. Verify the issue is closed (GitHub auto-close may not fire if the issue is not linked to the PR). If not closed, close it explicitly. Clean up the workspace, then exit. The state machine already owns the downstream transition and retro dispatch.

  ```bash
  ISSUE_STATE=$(gh issue view "$LEGION_ISSUE_ID" --json state -R $OWNER/$REPO | jq -r '.state')
  if [ "$ISSUE_STATE" != "CLOSED" ]; then
    gh issue close "$LEGION_ISSUE_ID" -R $OWNER/$REPO
  fi
  cd /tmp && rm -rf "$LEGION_DIR"
  ```
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

**Handoff data cleanup:** The `.legion/` directory contains handoff data from pipeline phases. During rebase/conflict resolution, PRESERVE these files — they may be needed if merge fails and requires rework. Cleanup happens in step 3.5 (after conflicts are resolved, before push).

### 3.5. Remove Handoff Data

Remove `.legion/` files BEFORE pushing so they are excluded from the squash merge onto main.
Handoff data is preserved in branch history (jj/git) but must not land on main.

```bash
if [ -d .legion ]; then
  rm -rf .legion/
  # Changes are auto-tracked by jj — no need to commit separately
fi
```

### 4. Push

```bash
jj git push
```

### 5. Verify CI Status

Before enabling auto-merge, verify that required checks are passing or pending (not failed):

```bash
gh pr checks "$LEGION_ISSUE_ID" --watch
```

[Remove this step once branch protection requires the checks — GitHub will then enforce them at merge.]

If checks fail, fix the issues, push, and re-run this step. If checks are green or pending, proceed to step 5.1.

### 5.1. Enable GitHub Auto-Merge

Enable auto-merge after the branch is prepared and pushed. GitHub will merge when the required checks and review are satisfied.

```bash
gh pr merge "$LEGION_ISSUE_ID" --auto --squash --delete-branch
```

The `--auto` flag requests auto-merge enablement. On an immediately-mergeable PR (all required checks and reviews already satisfied), gh completes the merge in the same command. On a PR awaiting checks or review, gh arms auto-merge and returns; GitHub merges when requirements are met. The worker never uses `--admin` or bypasses required checks.
If auto-merge is enabled (or the PR merged immediately), proceed to step 5.2 to check the merge status.

### 5.2. Check PR Merge Status

After enabling auto-merge, check whether the PR merged immediately (the common case when branch protection is not applied):

```bash
gh pr view "$LEGION_ISSUE_ID" --json state,mergedAt -R $OWNER/$REPO
```

**If `state` is `MERGED`:** The PR merged immediately. Proceed to step 5.3 to verify issue close and clean up the workspace.

**If `state` is `OPEN`:** Auto-merge is armed and waiting on required checks. Exit; the controller observes the merge and handles post-merge transitions.
### 5.3. Verify Issue Close and Clean Up Workspace

The PR is merged. Verify the issue is closed (GitHub auto-close may not fire if the issue is not
linked to the PR). If not closed, close it explicitly:

```bash
ISSUE_STATE=$(gh issue view "$LEGION_ISSUE_ID" --json state -R $OWNER/$REPO | jq -r '.state')
if [ "$ISSUE_STATE" != "CLOSED" ]; then
  gh issue close "$LEGION_ISSUE_ID" -R $OWNER/$REPO
fi
```

Remove the workspace:

```bash
cd /tmp
rm -rf "$LEGION_DIR"
```

Exit. The state machine handles retro dispatch and final cleanup.

### 6. Handle Auto-Merge Enablement Failures
If auto-merge cannot be enabled, classify the PR state before taking action:

```bash
gh pr view "$LEGION_ISSUE_ID" --json state,mergeable,mergeStateStatus -R $OWNER/$REPO
```

**If `state` is `MERGED`:** no-op and exit. The state machine already owns the downstream transition and retro dispatch. Verify the issue is closed and clean up the workspace:

```bash
ISSUE_STATE=$(gh issue view "$LEGION_ISSUE_ID" --json state -R $OWNER/$REPO | jq -r '.state')
if [ "$ISSUE_STATE" != "CLOSED" ]; then
  gh issue close "$LEGION_ISSUE_ID" -R $OWNER/$REPO
fi
cd /tmp
rm -rf "$LEGION_DIR"
```

Then exit.

**If `mergeStateStatus` is `BEHIND` or `DIRTY` (base changed after preparation):**

Re-run steps 2 through 5.1 to rebase, resolve any conflicts, push, and enable auto-merge against the new base. Do not attempt a direct merge; always use `--auto`.

**If auto-merge is blocked** (for example, GitHub reports that auto-merge is disabled or unavailable for
the repository):

**NEVER use `gh pr merge --admin` or any admin override to bypass branch protection.** This is a security/governance rule — only Sami may authorize admin merges. Instead:
- Post a comment explaining the auto-merge enablement failure:
  - **GitHub:** `gh issue comment $ISSUE_NUMBER --body "Auto-merge blocked: [exact error]. Enable or permit GitHub auto-merge; do not bypass branch protection." -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="Auto-merge blocked: [exact error].")`
- Add `user-input-needed` label:
  - **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "user-input-needed" --remove-label "worker-active" -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current without "worker-active" plus "user-input-needed"])`
- Notify controller (best-effort):
  ```
  envoy_publish(topic="notifications.role.legion-controller", message="Worker blocked: $ISSUE_NUMBER auto-merge blocked")
  ```
  If `envoy_publish` fails, continue — the label is the source of truth.
- Exit

**If auto-merge enablement fails for any other reason** (permission error, unknown error — inspect the
error output to determine the cause):
- Post a comment describing the failure and what you observed:
  - **GitHub:** `gh issue comment $ISSUE_NUMBER --body "Auto-merge enablement failed: [describe the error — permission denied, unexpected API error, etc.]." -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="Auto-merge enablement failed: [describe error].")`
- Add `user-input-needed` label:
  - **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "user-input-needed" --remove-label "worker-active" -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current without "worker-active" plus "user-input-needed"])`
- Notify controller (best-effort):
  ```
  envoy_publish(topic="notifications.role.legion-controller", message="Worker failed: $ISSUE_NUMBER auto-merge enablement failed — [brief error description]")
  ```
  If `envoy_publish` fails, continue — the label is the source of truth.
- Exit
