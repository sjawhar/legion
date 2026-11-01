# Worker Failure Notifications & Merge Retry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Execute tasks sequentially — do not parallelize.

**Goal:** Add failure notifications to all worker workflow exit paths and bounded retry logic to the merge workflow for race-condition conflicts.

**Architecture:** Two fixes applied to markdown workflow instruction files:
1. Add `envoy_publish` failure notifications at every non-success exit point across worker workflows and the shared escalation protocol in SKILL.md
2. Add bounded retry logic (max 2 retries) to merge.md step 6 for handling conflicts when `main` moves between rebase and merge

Notification pattern: best-effort, fire-and-forget, labels remain source of truth. Follow the existing success-notification pattern already in each workflow.

**Tech Stack:** Markdown workflow files (skill instructions for LLM agents), `gh` CLI, `jj`, Envoy `envoy_publish`

**Assumptions:**
- Only terminal exit notifications — no "retrying" progress signals during the retry loop
- After a merge failure, use `gh pr view --json state,mergeable,mergeStateStatus` to detect race conditions (BEHIND/DIRTY). For other failures (permission, unknown), the agent reads the error output and uses judgment — the API cannot distinguish these.
- Post-rebase `jj status` check required before every retry push
- `test.md` already has pass/fail notifications; its escalation exit delegates to SKILL.md
- `implement.md` and `review.md` don't have explicit inline escalation exits — they delegate to SKILL.md's shared "Blocking on User Input" protocol

**Relevant learnings:**
1. `skill-patterns/cross-cutting-workflow-concerns.md`: Pattern for adding cross-cutting steps — patch inline at each exit point rather than creating shared abstractions
2. `skill-patterns/worker-skill-failure-modes.md`: Real merge failure modes (already-merged, permission errors) that need coverage
3. `daemon/envoy-auto-subscription-patterns.md`: `envoy_publish` is fire-and-forget; labels are source of truth

---

### Task 1: Merge retry logic + failure notifications in merge.md

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/merge.md`

This task modifies merge.md to: (a) add bounded conflict-retry logic to step 6, and (b) add failure notifications to every non-success exit path (steps 1, 3, 5, and 6).

- [ ] **Step 1: Add notification to step 1 — already merged exit**

In step 1 (Check PR State), find the bullet starting with `- If **already merged**` (currently line 16). Replace the single-line bullet with:

```markdown
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
```

- [ ] **Step 2: Add notification to step 1 — closed without merge exit**

In step 1, find the bullet starting with `- If **closed without merge**` (currently line 17). Replace the single-line bullet with:

```markdown
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
```

- [ ] **Step 3: Add notification to step 3 — unresolvable conflict exit**

In step 3 (Resolve Conflicts), find the `**If conflicts are unresolvable:**` block (currently starting around line 35). Before the final `- Exit` line in that block, insert:

```markdown
- Notify controller (best-effort):
  ```
  envoy_publish(topic="notifications.role.legion-controller", message="Worker failed: $ISSUE_NUMBER merge failed — unresolvable rebase conflict")
  ```
  If `envoy_publish` fails, continue — the label is the source of truth.
```

- [ ] **Step 4: Add explicit escalation with notification to step 5 — fundamental CI failure**

In step 5 (Wait for CI), after the paragraph ending with "Normal code issues like type errors should be fixed, not escalated." (currently line 65), append:

```markdown

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
```

- [ ] **Step 5: Replace step 6 with retry-aware version**

Replace the entire step 6 content — from `### 6. Merge` (currently line 67) through the permission-error exit block ending with `- Exit — the user needs to merge manually or grant access` (currently line 80) — with:

```markdown
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
```

- [ ] **Step 6: Verify merge.md changes**

Run: `grep -c "envoy_publish" .opencode/skills/legion-worker/workflows/merge.md`
Expected: at least `7` (1 success in step 7.5 + 6 failure: already-merged, closed-without-merge, unresolvable-conflict, fundamental-CI, retry-exhausted, other-error)

Run: `grep "mergeStateStatus" .opencode/skills/legion-worker/workflows/merge.md`
Expected: at least 2 lines (the `gh pr view` command and the BEHIND/DIRTY condition)

Run: `grep "2 retries\|2 times" .opencode/skills/legion-worker/workflows/merge.md`
Expected: at least 2 matches

---

### Task 2: Failure notifications for SKILL.md, architect.md, and plan.md

**Files:**
- Modify: `.opencode/skills/legion-worker/SKILL.md` (Blocking on User Input section)
- Modify: `.opencode/skills/legion-worker/workflows/architect.md` (step 3 "If unclear" exit)
- Modify: `.opencode/skills/legion-worker/workflows/plan.md` (steps 2 and 4 escalation exits)

This task adds failure notifications to: (a) the shared "Blocking on User Input" protocol in SKILL.md (covers implement, review, and test escalation exits), (b) architect.md's inline "unclear" exit, and (c) plan.md's two inline escalation exits.

- [ ] **Step 1: Add notification to SKILL.md shared escalation protocol**

In SKILL.md, find the "Blocking on User Input" section. The current numbered steps are:

```
1. Push your work: `jj git push`
2. Post a structured escalation comment to the issue: [...]
3. Update labels: add `user-input-needed`, remove `worker-active`
4. Exit immediately
```

Insert a new step 4 between "Update labels" (current step 3) and "Exit immediately" (current step 4), and renumber:

```markdown
3. Update labels: add `user-input-needed`, remove `worker-active`
4. Notify the controller via Envoy (best-effort):
   ```
   envoy_publish(topic="notifications.role.legion-controller", message="Worker blocked: $ISSUE_NUMBER [current mode] needs user input")
   ```
   If `envoy_publish` fails, continue — the label is the source of truth.
5. Exit immediately
```

Note: `[current mode]` is a placeholder the worker fills in with their actual mode (architect, plan, implement, test, review, merge). This matches how `$ISSUE_NUMBER` is already used as a placeholder throughout the workflows.

- [ ] **Step 2: Add notification to architect.md — unclear exit**

In architect.md step 3 (Act), find the line `**If unclear:** Add \`user-input-needed\` label, remove \`worker-active\` label, post comment with specific questions, exit.` (currently line 61). Replace it with:

```markdown
**If unclear:**
- Add `user-input-needed` label:
  - **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "user-input-needed" --remove-label "worker-active" -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current without "worker-active" plus "user-input-needed"])`
- Post comment with specific questions:
  - **GitHub:** `gh issue comment $ISSUE_NUMBER --body "..." -R $OWNER/$REPO`
  - **Linear:** `linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="...")`
- Notify controller (best-effort):
  ```
  envoy_publish(topic="notifications.role.legion-controller", message="Worker blocked: $ISSUE_NUMBER architect needs user input")
  ```
  If `envoy_publish` fails, continue — the label is the source of truth.
- Exit
```

- [ ] **Step 3: Add notification to plan.md step 2 — unclear requirements exit**

In plan.md step 2 (inside the `**If the skill determines requirements are fundamentally unclear**` block), the current escalation substeps end with:

```
3. Exit immediately - do NOT add `worker-done`
```

Insert a new substep 3 before Exit and renumber:

```markdown
3. Notify controller (best-effort):
   ```
   envoy_publish(topic="notifications.role.legion-controller", message="Worker blocked: $ISSUE_NUMBER plan needs user input — requirements unclear")
   ```
   If `envoy_publish` fails, continue — the label is the source of truth.
4. Exit immediately - do NOT add `worker-done`
```

- [ ] **Step 4: Add notification to plan.md step 4 — review max iterations exit**

In plan.md step 4 (inside the `**Max 3 iterations.** If still failing:` block), the current escalation substeps end with:

```
3. Exit without `worker-done`
```

Insert a new substep 3 before Exit and renumber:

```markdown
3. Notify controller (best-effort):
   ```
   envoy_publish(topic="notifications.role.legion-controller", message="Worker blocked: $ISSUE_NUMBER plan review failed after 3 iterations")
   ```
   If `envoy_publish` fails, continue — the label is the source of truth.
4. Exit without `worker-done`
```

- [ ] **Step 5: Verify all non-merge changes**

Run: `grep -c "envoy_publish" .opencode/skills/legion-worker/SKILL.md`
Expected: at least `1`

Run: `grep -c "envoy_publish" .opencode/skills/legion-worker/workflows/architect.md`
Expected: at least `2` (1 existing success + 1 new failure)

Run: `grep -c "envoy_publish" .opencode/skills/legion-worker/workflows/plan.md`
Expected: at least `3` (1 existing success + 2 new failure)

---

### Task 3: Final verification and commit

**Files:** All modified files (read-only verification), then commit

- [ ] **Step 1: Read each modified file's exit paths and confirm notification coverage**

Read the changed sections of each file and verify:

1. **merge.md**: Every exit path (steps 1, 3, 5, 6, 7.5) has exactly one `envoy_publish` call. The retry substeps (1-6 inside step 6) do NOT contain any `envoy_publish` — only the terminal exits after retry exhaustion or other failure.

2. **SKILL.md**: The "Blocking on User Input" section includes an `envoy_publish` step between label update and exit.

3. **architect.md**: The "If unclear" exit in step 3 includes an `envoy_publish` call.

4. **plan.md**: Both escalation exits (step 2 unclear requirements, step 4 review max iterations) include `envoy_publish` calls. Step 1.5's escalation delegates to SKILL.md (covered by SKILL.md change).

5. **implement.md, review.md, test.md**: No changes needed. Verify these files do NOT have inline `user-input-needed` escalation exits with explicit label/comment instructions — their only escalation path is through SKILL.md's general "Blocking on User Input" protocol (now updated with a notification). Run: `grep -c "user-input-needed" .opencode/skills/legion-worker/workflows/implement.md .opencode/skills/legion-worker/workflows/review.md .opencode/skills/legion-worker/workflows/test.md` — any matches should be in descriptive text or test-specific pass/fail handling, not in inline escalation blocks that duplicate the SKILL.md protocol.

- [ ] **Step 2: Verify notification message format consistency**

Run: `grep -r "envoy_publish.*message=" .opencode/skills/legion-worker/workflows/ .opencode/skills/legion-worker/SKILL.md`

All notification messages should follow one of these patterns:
- `Worker done:` — success exits
- `Worker failed:` — terminal failure exits
- `Worker blocked:` — user-input-needed exits

- [ ] **Step 3: Commit all changes**

```bash
jj describe -m "fix(worker): add failure notifications to all worker workflows and merge retry logic

Adds envoy_publish failure notifications at every non-success exit path across
worker workflows (merge, architect, plan) and the shared escalation protocol
in SKILL.md. Adds bounded retry logic (max 2 retries) to the merge workflow
for handling race-condition conflicts when main moves between rebase and merge.

Closes #411"
```

---

## Testing Plan

### Setup
- No infrastructure needed — these are markdown file edits
- The workspace should be on the issue branch with all changes applied

### Health Check
- All modified files exist: `merge.md`, `SKILL.md`, `architect.md`, `plan.md`
- `jj diff --stat` shows only the expected files changed

### Verification Steps

1. **Merge retry logic is bounded and uses PR state inspection**
   - Action: `grep -c "mergeStateStatus" .opencode/skills/legion-worker/workflows/merge.md`
   - Expected: at least 2 (the `gh pr view` command and the BEHIND/DIRTY condition)
   - Action: `grep "2 retries\|2 times" .opencode/skills/legion-worker/workflows/merge.md`
   - Expected: at least 2 matches referencing the retry bound
   - Tool: grep

2. **Every terminal exit path in merge.md has exactly one notification**
   - Action: `grep -c "envoy_publish" .opencode/skills/legion-worker/workflows/merge.md`
   - Expected: at least 7 (1 success + 6 failure)
   - Action: Read the retry substeps and confirm NO `envoy_publish` inside the retry loop body — only after retry exhaustion
   - Tool: grep + Read

3. **Shared escalation protocol and non-merge workflows have failure notifications**
   - Action: `grep -c "envoy_publish" .opencode/skills/legion-worker/SKILL.md`
   - Expected: at least 1
   - Action: `grep -c "envoy_publish" .opencode/skills/legion-worker/workflows/architect.md`
   - Expected: at least 2
   - Action: `grep -c "envoy_publish" .opencode/skills/legion-worker/workflows/plan.md`
   - Expected: at least 3
   - Tool: grep

### Skills to Invoke
- `verification-before-completion` — verify all acceptance criteria are met before marking done

### Tools Needed
- grep (for content verification)
- Read tool (for exit path coverage review)
- jj (for commit verification)

## Required Skills

The following project-specific skills should be loaded by downstream workers:

| Phase | Skills |
|-------|--------|
| Implement | (none beyond standard — markdown file edits) |
| Test | `verification-before-completion` |
| Review | (none beyond standard) |

Workers: invoke these skills at the start of your workflow before beginning work.
If a skill is unavailable in your environment, proceed without it.
