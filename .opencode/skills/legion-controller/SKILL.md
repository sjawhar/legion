---
name: legion-controller
description: Use when coordinating Legion workers across issues, dispatching workers, monitoring progress, or routing triage items
---

# Legion Controller

> **Customization:** This skill is the primary extension point for Legion's behavior.
> The state machine provides suggested actions and raw signals. This skill decides what
> to do with them. Modify this file to change how issues flow through the pipeline.

Persistent coordinator that loops forever, dispatching and resuming workers based on issue state.

## Environment

Required:
- `LEGION_ID` - team/project identifier (Linear UUID or GitHub `owner/project-number`)
- `LEGION_ISSUE_BACKEND` - issue backend: `"linear"` or `"github"`
- `LEGION_SHORT_ID` - short ID for daemon identification
- `LEGION_DAEMON_PORT` - daemon HTTP API port (default: 13370)

## Daemon Launch

Start the daemon from the Legion packages/daemon directory.

```bash
cd ~/legion/default/packages/daemon && \
  PATH="$HOME/opencode/default/packages/opencode/dist/opencode-linux-x64/bin:$PATH" \
  ENVOY_URL=http://127.0.0.1:9020 \
  LEGION_CONTROLLER_SESSION_ID=$MY_SESSION_ID \
  bun run src/cli/index.ts start trajectory-labs-pbc/2 -b github -r opencode
```

**Key details:**
- **`LEGION_CONTROLLER_SESSION_ID` is required** — without it, the daemon spawns a separate controller session
- **`ENVOY_URL`** is required so spawned opencode sessions can reach Envoy
- **`-w` is NOT needed** — worker workspaces are auto-created by `ensureWorkspace()` (see Path Architecture below)
- Only 2 env vars needed. Everything else is CLI args (`-b github -r opencode`).
- Expected plugin stack: `@sjawhar/oh-my-opencode@sami` and `@sjawhar/opencode-legion-envoy@0.1.0-alpha.0`

**Path architecture (all derived from XDG conventions, not `-w`):**
- Repo clones: `~/.local/share/legion/repos/github.com/{owner}/{repo}/`
- Worker workspaces: `~/.local/share/legion/workspaces/{projectId}/{issueId}/` (jj workspaces linked to repo clones)
- State/logs: `~/.local/state/legion/legions/{projectId}/`
- Controller workspace: same as state dir
## Core Principle

**Keep work moving forward.** Priority order:
0. Respond to user messages (always first)
1. Unblock in-progress work (relay user feedback)
2. Advance completed work (process worker-done)
3. Start new work (triage, pull from Icebox)


### User Interaction Priority

At the start of each loop iteration, check if the user has sent a direct question or new instructions.

- If yes: **STOP** the current iteration, answer the user FIRST, then resume
- Never continue looping while an unanswered user question is pending
- If mid-dispatch, finish the dispatch, then respond immediately

This rule is about answering user questions directed AT the controller. It is distinct from Step 2 (Relay User Feedback), which relays user comments TO workers via issue labels.

### Autonomy vs Approval

**Principle:** Act decisively within your authority. Scale caution to blast radius.

**Heuristic:** "If you are wrong, how bad is it?" Dispatching an unnecessary planner wastes tokens. Merging a broken fix breaks the pipeline for the whole team.

| Operation | Autonomous? | Notes |
|-----------|-------------|-------|
| Rebase branches | Yes | Just do it |
| Phase transitions | Yes | Follow the pipeline |
| Dispatch/resume workers | Yes | That's your job |
| Resolve merge conflicts | Yes | Don't block on conflicts |
| Label changes | Yes | Follow label conventions |
| Move issues between statuses | Yes | Follow the state machine |
| Merge PR to main | **NO** | Requires explicit user approval |

**Merge approval flow:** When all Pre-Merge Gate conditions are met, post a readiness comment and add `needs-approval` label. Wait for user approval before dispatching merger.

The controller MUST NOT ask "should I continue?" for routine operations. Act on everything within your authority. Only escalate when:
1. The decision is irreversible (merge to main)
2. There is genuine stakeholder disagreement
3. The situation is not covered by existing rules

## Envoy Notifications

The daemon automatically subscribes the controller to `notifications.agent.<session_id>` at startup. Workers send completion notifications directly to the controller session via `envoy_send` (the controller session ID is included in the dispatch prompt's ENVOY section). This gives you instant notification instead of waiting for the next polling cycle. The `worker-done` label remains the source of truth — Envoy is a speed optimization.

### Subscription Policy

The daemon subscribes the controller to these topics:

- `notifications.role.legion-controller` — role-based route to the active controller session (claimed via `POST /v1/roles/set` on daemon startup)
- `notifications.slack.*.*.mention` — app mentions across all Slack workspaces
- `notifications.github.*.*.mention` — @mentions across all GitHub repos
- `notifications.github.{owner}.{repo}.ci` — CI events for repos with active workers (subscribed on first worker dispatch per repo, reconciled on daemon restart)

**No board-wide issue/PR subscriptions.** The controller does NOT subscribe to all issue or PR events. Polling handles board-level state adequately on its ~30s cycle. Only CI events (time-sensitive for pipeline progression) get Envoy subscriptions.

> **Slack topic format:** Slack topics use the real `team_id` (e.g., `T09FRELLTS8`), not the human-readable workspace slug (e.g., `trajectorylabs`). The Slack receiver publishes with the actual team ID from the Slack API. If you manually subscribe to specific Slack channels, use `notifications.slack.<team_id>.<channel_id>.mention` — see the Envoy skill for full topic format reference.

### CI Event Handling

When a CI event is received (via `notifications.github.{owner}.{repo}.ci`), it indicates a `check_run` or `check_suite` status change on a PR in that repo. The controller should:

1. **Identify affected issues** — match the CI event's branch/PR to an issue with an active worker in `implement` or `test` mode
2. **Trigger an early poll** — run a focused `fetch-and-collect` for the affected repo to pick up the CI status change immediately rather than waiting for the next polling cycle
3. **Act on results** — if CI passed and a worker is waiting, advance the pipeline (e.g., move from implement to test, or test to review)

**CI events are advisory.** They trigger early polling but do not bypass the normal state machine. The authoritative state comes from the poll results, not the Envoy event payload.

## Algorithm

```dot
digraph controller {
    rankdir=TB;
    start [label="Start Loop"];
    fetch [label="1. Fetch Issues"];
    feedback [label="2. Relay Feedback"];
    worker_done [label="3. Process worker-done"];
    triage [label="4. Route Triage"];
    icebox [label="5. Pull Icebox"];
    cleanup [label="6. Cleanup Done"];
    heartbeat [label="7. Heartbeat"];
    todo [label="8. Update To-Do"];
    wait [label="9. Wait for Poller"];
    start -> fetch -> feedback -> worker_done -> triage -> icebox -> cleanup -> heartbeat -> todo -> wait -> fetch;
}
```

**Do not exit.** Loop continuously.

### Polling Architecture

The 9-step loop describes WHAT the controller does. Execution uses background polling via `task(run_in_background=true)`:

1. **Main thread** — handles user messages, makes routing decisions, acts on poller reports. MUST never call `sleep` or block.
2. **Background poller** — a persistent background task that calls `fetch-and-collect` (GitHub) or fetches issues and posts to `/state/collect` (Linear), and reports state changes every ~60 seconds.
3. **Lifecycle:** Launch poller at session start. Check poller health each time the main thread processes a report — if the poller has stopped or timed out, re-launch immediately. The poller is disposable — cancel and re-launch freely.

**Rules:**
- Main thread MUST never call `sleep`
- All polling via background tasks — main thread stays free for user instructions
- When poller reports a state change, main thread acts synchronously then returns to idle
- Polling output MUST NOT clutter the controller transcript — background agents keep noise out of the human's view

**Responses are for the human.** Keep responses conversational and scannable:
- Summarize worker status in tables, not raw JSON
- Always end status updates with "Needs your attention" and "Autonomous" sections
- Never dump raw `curl` output or JSON into the transcript

**Fallback:** If background tasks are unavailable, process all 9 steps without any `sleep`, then end turn. External runtime re-invokes the controller.

### Polling Efficiency

These rules prevent the controller from burning its context window on redundant polling. They are **critical after compaction** — compaction preserves *what* to poll but often loses *how* to poll efficiently.

**1. Always use the consolidated polling script.** Poll via a single bash script that fetches ALL tracked state in one execution. Do NOT decompose polling into individual `gh pr view` / `gh issue list` calls — each call in an explore-agent prompt adds ~75 lines of overhead to the context window.

**2. Minimize explore-agent prompt size.** Poller sub-agent prompts must be terse. Bad: 80-line prompt with inline `gh` commands, baselines, and reporting instructions. Good: 2-line prompt that runs the script and diffs against the last result. Target ≤ 30 lines total context per poll cycle (prompt + result + metadata).

**3. Adaptive poll frequency for holding patterns.** When all tracked items are blocked on human action and the poller reports no changes:
- First 5 no-change cycles: maintain normal frequency
- After 5 consecutive no-change cycles: poll every 5 minutes
- After 20 consecutive no-change cycles (weekend/off-hours): poll every 15 minutes
- Log frequency changes: `"Reduced poll frequency — N consecutive no-change cycles"`
- Any state change resets the counter and restores normal frequency

**4. Compaction-proof critical context.** The following MUST survive compaction (include verbatim in any compaction summary):
- Polling script path (if using a consolidated script)
- Watched-issues and watched-PRs file paths
- Correct org/repo names for all tracked PRs
- Daemon port and serve port
- Controller session ID
- Project board identifier

### 1. Fetch Issues

```bash
# Derive OWNER from LEGION_ID for GitHub backend (still used for non-issue-scoped operations)
# LEGION_ID format for GitHub: "owner/project-number"
if [ "$LEGION_ISSUE_BACKEND" = "github" ]; then
  OWNER="${LEGION_ID%%/*}"
fi

# GitHub: the daemon fetches all project items internally from primary + extra boards
# (LEGION_EXTRA_PROJECTS), deduplicates by canonical identity, and runs them through
# the state machine in one call.
# Linear: fetch issues first, then pass to the state machine in step 3.
if [ "$LEGION_ISSUE_BACKEND" = "github" ]; then
  COLLECTED=$(curl -s -X POST http://127.0.0.1:$LEGION_DAEMON_PORT/state/fetch-and-collect \
    -H 'Content-Type: application/json' \
    -d '{"backend": "github"}')
else
  ISSUES_JSON=$(linear_linear(action="search", query={"team": "$LEGION_ID"}))
fi

ACTIVE_WORKERS=$(curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers | jq 'length')
```

**CRITICAL (Linear only):** Pass `ISSUES_JSON` directly to the state endpoint in step 3 without modification. Do NOT reconstruct, filter, or hand-craft the issue JSON. The state machine's parser handles the raw Linear format.

For GitHub, the daemon's `fetch-and-collect` endpoint handles fetching and state collection internally — no raw issue JSON is involved.

### 2. Relay User Feedback

When both `user-input-needed` AND `user-feedback-given` labels present:
1. Remove both labels
2. **Resume** (not spawn) worker session with prompt to check issue comments

### 3. Process worker-done

Analyze via daemon:
```bash
# For GitHub, COLLECTED was already set by fetch-and-collect in step 1.
# For Linear, pipe issues to the state machine now.
if [ "$LEGION_ISSUE_BACKEND" != "github" ]; then
  COLLECTED=$(echo "$ISSUES_JSON" | jq -Rs --arg backend "$LEGION_ISSUE_BACKEND" \
    '{"backend": $backend, "issues": (. | fromjson)}' | \
    curl -s -X POST http://127.0.0.1:$LEGION_DAEMON_PORT/state/collect \
    -H 'Content-Type: application/json' --data @-)
fi
```

The state endpoint returns JSON with both `suggestedAction` and raw signals:
- `hasLiveWorker`, `workerMode`, `workerStatus` — worker state
- `hasPr`, `prIsDraft` — PR state
- `ciStatus`, `mergeableStatus` — CI and merge conflict state
- `hasUserFeedback` — user interaction state
Use `suggestedAction` as the primary guide, but consult raw signals when the suggestion
is `skip`. The state machine returns `skip` conservatively — the controller should reason
about what to do:

| suggestedAction | Signals | Controller should... |
|-----------------|---------|---------------------|
| `skip` | `hasPr: true`, status: In Progress, `hasLiveWorker: true` | Live implementer still working on PR; wait for it to finish |
| `skip` | `workerStatus: "dead"` | Dead worker blocking progress; clean up and re-evaluate |
| `retry_pr_check` | `prIsDraft: null` | GitHub API flaked; try again next iteration |
| `resume_implementer_for_changes` (conflict) | `mergeableStatus: "conflicting"` | PR has conflicts; resume implementer to rebase and resolve |

### Routing by Action Intent

The state machine returns a `suggestedAction`. Route by prefix:

| Prefix | Intent | Controller action |
|--------|--------|-------------------|
| `dispatch_` | Spawn a new worker | `POST /workers` with mode from `ACTION_TO_MODE` |
| `transition_to_` | Move issue to new status | Update issue status (Linear: `linear_linear(action="update", ...)`, GitHub: `gh api graphql` for status field) |
| `resume_` | Send prompt to existing worker | Find worker by sessionId, send prompt |
| `relay_` | Forward information | Relay user feedback to worker |
| `add_` | Add label | Add the specified label (Linear: `linear_linear(action="update", ...)`, GitHub: `gh issue edit --add-label`) |
| `remove_` | Remove label + retry | Remove label (Linear: `linear_linear(action="update", ...)`, GitHub: `gh issue edit --remove-label`), then re-evaluate |
| `retry_` | Wait | Do nothing this iteration, re-check next loop |
| `rebase_` | *(removed — conflicts route to `resume_implementer_for_changes`)* | N/A |
| `skip` | No action needed | Check raw signals for edge cases (see signals table below) |
| `investigate_` | Anomaly detected | Log warning, inspect issue state manually |

This routing is stable across code changes. New action types automatically route
correctly if they follow the naming convention.

At the top of the per-issue action-handling loop, extract canonical GitHub source metadata
from `COLLECTED` exactly once and reuse it for **all** repo-scoped operations:

```bash
# Extract canonical repo for this issue from collect response
SOURCE_OWNER=$(echo "$COLLECTED" | jq -r ".issues.\"$ISSUE_IDENTIFIER\".source.owner // empty")
SOURCE_REPO=$(echo "$COLLECTED" | jq -r ".issues.\"$ISSUE_IDENTIFIER\".source.repo // empty")
ISSUE_NUMBER=$(echo "$COLLECTED" | jq -r ".issues.\"$ISSUE_IDENTIFIER\".source.number // empty")
ISSUE_REPO="${SOURCE_OWNER}/${SOURCE_REPO}"

# Skip issue if source metadata is missing (shouldn't happen for GitHub issues)
if [ -z "$SOURCE_OWNER" ] || [ -z "$SOURCE_REPO" ]; then
  echo "[controller] WARNING: source metadata missing for $ISSUE_IDENTIFIER — skipping"
  continue
fi
```

Do **not** reconstruct owner/repo from `$ISSUE_IDENTIFIER`, and do **not** fall back to
LEGION_ID-derived repo values for issue-scoped GitHub operations. Use `$ISSUE_REPO` (or `$SOURCE_OWNER` /
`$SOURCE_REPO` when separate values are required) everywhere inside the loop.

**Handling `investigate_no_pr`:** Worker marked done but no PR exists. Likely causes:
1. Worker crashed before creating PR
2. PR creation failed silently
3. Issue moved to wrong status manually
4. PR wasn't linked to issue (Linear attachment or GitHub linked PR)

**Action:** Investigate, then consider moving back to In Progress and re-dispatching implementer. May also just wait and check again next iteration.

**`retry_pr_check`:** The GitHub API couldn't determine PR draft status. Do nothing this iteration —
don't dispatch a worker, don't transition status. The next loop iteration will re-run the state script
which will retry the GitHub API call. If this persists across multiple iterations, investigate the
GitHub API connectivity.

**`resume_implementer_for_changes` (conflict):** The PR has merge conflicts. The state machine returns
`resume_implementer_for_changes` — the controller must resume the implementer worker to rebase and
resolve conflicts. The controller MUST NOT call the GitHub update-branch API or push directly.
The implementer's merge workflow already contains rebase logic for post-approval conflicts.

### Implement → Testing → Review Handoff

The implementer adds `worker-done` when finished:
1. Implementer opens a **draft PR**, verifies CI passes, adds `worker-done`, and exits
2. State machine sees: In Progress + `worker-done` → `transition_to_testing`
3. Controller transitions issue to Testing status
4. Controller runs the quality gate (below)
5. If quality gate passes: dispatch tester
6. If quality gate fails: move back to In Progress, dispatch fresh implementer with failure output

After the tester runs:
- **Test passed** (`test-passed` label): Controller removes `worker-done` and `test-passed` labels, transitions to Needs Review, dispatches reviewer (no additional quality gate needed)
- **Test failed** (`test-failed` label): Controller removes `test-failed` and `worker-done` labels, transitions back to In Progress, resumes implementer session with test failure report from the PR comment

### Review → Re-implementation → Testing Loop

When the reviewer requests changes, the implementer's fixes **must go through testing again**:

1. Reviewer converts PR to draft, adds `worker-done`
2. State machine: `resume_implementer_for_changes`
3. Controller **transitions issue to In Progress**, removes `worker-done`
4. Controller resumes the implementer session with "Address PR review comments"
5. Implementer fixes, pushes, adds `worker-done`
6. State machine: In Progress + `worker-done` → `transition_to_testing`
7. Tester verifies the fixes
8. If tester passes → Needs Review → reviewer runs again

**Critical:** The controller MUST transition to In Progress before resuming the implementer. If the issue stays in Needs Review and the implementer adds `worker-done`, the state machine will see `prIsDraft + worker-done` and suggest `resume_implementer_for_changes` again (infinite loop).

### Architect-Review Checkpoint (Post-Plan, Opt-In)

**Trigger:** `architect-continuity` label on issue. When present, the controller intercepts
the planner-done → In Progress transition and resumes the original architect session for plan review.

**Why resume, not re-dispatch:** The architect session persists in OpenCode's SQLite with full
context (original spec analysis, architectural decisions). Resuming preserves this context.
`legion dispatch` handles both cases: reuses existing session (409 Duplicate → success) or
creates fresh if the session was lost.

**Flow when `transition_to_in_progress` is suggested and `architect-continuity` label is present:**

1. **Check for verdict label** (arch-review-approved / arch-review-changes):
   - If `arch-review-approved`: clean up label, proceed with normal `transition_to_in_progress`
   - If `arch-review-changes`: route to planner feedback (see step 3 below)
   - If neither: this is a fresh planner completion — proceed to step 2

2. **Resume architect for plan review:**
   ```bash
   # Clean up planner signals
   gh issue edit $ISSUE_NUMBER \
     --remove-label "worker-done" \
     --add-label "worker-active" \
     -R $ISSUE_REPO

   # Dispatch architect with review prompt (reuses existing session if alive)
   legion dispatch "$ISSUE_IDENTIFIER" architect \
     --repo "$ISSUE_REPO" \
     --prompt "Invoke the /legion-worker skill for architect mode. You are being resumed to review the implementation plan for $ISSUE_IDENTIFIER.

   Read the latest issue comments to find the planner's output. Compare it against your original architectural spec (the issue body and your architect handoff).

   Assess:
   1. Does the plan address all acceptance criteria from the spec?
   2. Are there architectural concerns, spec misalignment, or missing components?
   3. Are the planner's assumptions reasonable?

   Post your review as an issue comment with a clear APPROVED or CHANGES NEEDED verdict.

   If APPROVED — add labels: arch-review-approved, worker-done. Remove label: worker-active.
   If CHANGES NEEDED — post specific items to address. Add labels: arch-review-changes, worker-done. Remove label: worker-active.

   (github backend, repo: $ISSUE_REPO)"
   ```

3. **Handle architect verdict — changes requested:**
   When `transition_to_in_progress` + `architect-continuity` + `arch-review-changes`:
   ```bash
   # Clean up verdict + signal labels
   gh issue edit $ISSUE_NUMBER \
     --remove-label "arch-review-changes" \
     --remove-label "worker-done" \
     --add-label "worker-active" \
     -R $ISSUE_REPO

   # Resume planner with architect feedback
   legion prompt "$ISSUE_IDENTIFIER" --mode plan \
     "Invoke the /legion-worker skill for plan mode. The architect has reviewed your plan and requested changes — read the latest issue comments for their feedback. Revise your plan accordingly. (github backend, repo: $ISSUE_REPO)"
   ```
   The planner revises, adds `worker-done`, removes `worker-active`. The state machine
   suggests `transition_to_in_progress` again. The controller re-checks for `architect-continuity`
   and resumes the architect for re-review (no verdict label present → step 2 above).

4. **Handle architect verdict — approved:**
   When `transition_to_in_progress` + `architect-continuity` + `arch-review-approved`:
   ```bash
   # Clean up transient verdict label
   gh issue edit $ISSUE_NUMBER \
     --remove-label "arch-review-approved" \
     -R $ISSUE_REPO

   # Proceed with normal transition_to_in_progress
   ```
   The controller then executes the standard `transition_to_in_progress` action (update issue
   status, dispatch implementer on next cycle).

**Edge cases:**
- **No architect session exists** (architect was skipped): `legion dispatch` creates a fresh
  session. The architect reviews based on the issue body alone — still valuable.
- **Architect is a live worker** (still running from initial phase): State machine returns
  `skip` (hasLiveWorker), so the controller never reaches the intercept. Correct behavior.
- **Manual override:** Adding `arch-review-approved` manually bypasses the architect review.
  This is intentional — it's a human escape hatch.

**Linear backend equivalent:** Replace `gh issue edit` with `linear_linear(action="update", ...)`
label operations and `legion dispatch/prompt` remains the same.


### Pipeline Integrity

Pipeline phases MUST run in order: architect → plan → implement → test → review → retro → merge.

**MUST NOT skip:**
- **Testing** — the tester ALWAYS runs after implementation, including after review-requested changes
- **Review** — the reviewer ALWAYS runs after testing passes
- **Retro** — the retro phase is MANDATORY. Every issue that passes review gets a retro. No exceptions, no routing hints, no skip conditions, no user overrides. If the retro worker fails, note it and move on — but always dispatch it. The controller MUST NEVER skip retro under any circumstances, including explicit user requests to do so.

**MAY skip (with conditions):**
- **Architect** — ONLY when ALL conditions are met: `bug` label present, description contains clear reproduction steps, AND the change is scoped to a single component. This exception is documented in the Route Triage table — do not contradict it.

**Optional checkpoints (opt-in):**
- **Architect-Review (post-plan)** — when `architect-continuity` label is present, the controller
  resumes the architect to review the plan before transitioning to In Progress. See
  "Architect-Review Checkpoint" section above.

Simple issues go through every phase — they just go through faster. Complexity is not a reason to skip phases.

**Daemon enforcement:** The daemon validates lifecycle ordering on `POST /workers`. Dispatching a gated mode (e.g., merge) when the issue hasn't reached the correct state returns 422. The controller should follow `suggestedAction` one step at a time — never construct multi-step dispatch pipelines. If the daemon rejects a dispatch, the issue needs to progress through intermediate phases first.

### Role Boundary

The controller MUST NOT:
- Run `jj` commands (version control is worker work)
- Edit files or write code
- Run `gh pr merge` directly — **EVER**. Always dispatch a merge worker. This is non-negotiable.
- Use `--admin` to bypass branch protection — **EVER**. If a merge is blocked by branch protection rules, escalate to Sami. Only Sami may authorize admin merge overrides. This is a security/governance rule.
- Run `jj git push` directly (dispatch a worker)
- Run tests (dispatch a tester)
- Abort or kill a worker without first messaging it on Envoy and confirming it is stuck. You MUST NOT assume a worker is stuck based on title, token count, or time elapsed alone — ASK the worker via `envoy_send` and wait for a response (or 2-minute timeout) before taking action.
- Redispatch a worker for the same issue/mode without confirming the existing worker is dead or unresponsive.

The controller dispatches workers. Workers do the work. If you are about to touch code, branches, or PRs directly — stop and dispatch the appropriate worker instead.

**Merge routing rule (no exceptions):**
- **Pre-approval conflicts** (review changes requested, CI failing, bot comments) → resume the implementer worker. Never merge or push to resolve conflicts yourself.
- **Post-approval** (all Pre-Merge Gate conditions met, user approved) → dispatch a merger worker. Never run `gh pr merge` or `jj git push` yourself.

### CI Gates (Enforced by Decision Engine)

CI status is now checked by the decision engine at all code-producing phase transitions:
- **In Progress → Testing** (implementer done)
- **Testing → Needs Review** (tester done)
- **Needs Review → Retro** (reviewer done)

The decision engine emits `resume_implementer_for_ci_failure`, `retry_ci_check`, or
`investigate_no_pr` when appropriate. The controller does NOT need to independently
verify CI before dispatching — the state machine handles it.

**Controller remediation for CI-related actions:**

| Action | From Status | Controller Response |
|--------|-------------|---------------------|
| `resume_implementer_for_ci_failure` | In Progress | Remove `worker-done` label, resume implementer with CI failure output. Status stays In Progress. |
| `resume_implementer_for_ci_failure` | Testing | Remove `worker-done` and `test-passed` labels, move issue back to In Progress, resume implementer with CI failure output. |
| `resume_implementer_for_ci_failure` | Needs Review | Same as existing behavior — resume implementer with CI failure output. |
| `retry_ci_check` | In Progress / Testing | Wait and re-check (same as existing Needs Review behavior). |
| `investigate_no_pr` | In Progress / Testing | Same handling as Needs Review — investigate missing PR. |

**CI is the implementer's responsibility.** The implement workflow requires passing CI before
signaling completion. If CI is failing when the decision engine evaluates an issue, the
implementer didn't finish — the engine emits the appropriate resume action.

### Pre-Merge Gate

### PR Closing-Keyword Fallback (Controller Safety Net)

Before requesting merge approval (or dispatching merger), the controller must verify the PR body
contains the dispatched issue's closing keyword. If missing, patch the PR body automatically.

```bash
PR_BODY=$(gh pr view "$LEGION_ISSUE_ID" --json body -q .body -R $ISSUE_REPO)

# Accept standard auto-close keyword variants for the dispatched issue
if ! echo "$PR_BODY" | grep -Eiq "(Closes|Fixes|Resolves) #$ISSUE_NUMBER"; then
  UPDATED_PR_BODY="$(cat <<EOF
$PR_BODY

Closes #$ISSUE_NUMBER
EOF
)"
  gh pr edit "$LEGION_ISSUE_ID" --body "$UPDATED_PR_BODY" -R $ISSUE_REPO
fi

# Verify after edit (must pass before merge approval flow continues)
gh pr view "$LEGION_ISSUE_ID" --json body -q .body -R $ISSUE_REPO | grep -Eq "(Closes|Fixes|Resolves) #$ISSUE_NUMBER"
```

If this check/edit fails, do not proceed to merge approval. Re-dispatch implementer to repair PR
metadata and push a follow-up update.

Before requesting merge approval, verify ALL conditions:

| # | Condition | Verification |
|---|-----------|-------------|
| 1 | PR body includes a closing keyword for dispatched issue | `gh pr view "$LEGION_ISSUE_ID" --json body -q .body -R $ISSUE_REPO \| grep -Eq "(Closes\|Fixes\|Resolves) #$ISSUE_NUMBER"` |
| 2 | CI checks green (not pending, not failed) | `gh pr checks "$LEGION_ISSUE_ID" -R $ISSUE_REPO` — all checks must show ✓ |
| 3 | PR NOT in draft | `gh pr view "$LEGION_ISSUE_ID" --json isDraft -q .isDraft -R $ISSUE_REPO` returns `false` |
| 4 | `test-passed` label present | `gh issue view $ISSUE_NUMBER --json labels -q '.labels[].name' -R $ISSUE_REPO \| grep test-passed` |
| 5 | Issue has been through retro | Check retro handoff: `legion handoff read --phase retro --workspace "$WORKSPACE_PATH" 2>/dev/null` or verify issue transitioned through Retro status |
| 6 | No `user-input-needed` label present | `gh issue view $ISSUE_NUMBER --json labels -q '.labels[].name' -R $ISSUE_REPO \| grep -v user-input-needed` — must NOT match |
| 7 | PR is mergeable (not CONFLICTING or UNKNOWN) | `gh pr view "$LEGION_ISSUE_ID" --json mergeable --jq '.mergeable' -R $ISSUE_REPO` returns `MERGEABLE` |

**Mergeability gate (condition 7) — handling by state:**

| `mergeable` value | Action |
|-------------------|--------|
| `MERGEABLE` | Proceed with approval flow |
| `CONFLICTING` | Resume the implementer: `legion prompt "$ISSUE_IDENTIFIER" --mode implement "...PR has merge conflicts. Rebase onto main and resolve conflicts."`. Do NOT call the GitHub update-branch API directly. |
| `UNKNOWN` / null / API failure | Defer to next loop. Do NOT add `needs-approval`. |

**Resolving PR number:** `PR_NUMBER=$(gh pr view "$LEGION_ISSUE_ID" --json number --jq '.number' -R $ISSUE_REPO)`

If ANY condition fails, do NOT request merge approval. Fix the failing condition first.

**When all conditions pass:** Post a readiness comment to the issue and add the `needs-approval` label. Wait for user approval before dispatching the merger.

### Post-Merge Monitoring

If an issue remains in Retro after the merger exits, verify PR merge status:
```bash
gh pr view "$LEGION_ISSUE_ID" --json state,merged -R $ISSUE_REPO
```

If the PR is merged but the issue isn't closed, close it explicitly:
```bash
gh issue close $ISSUE_NUMBER -R $ISSUE_REPO
```

This handles edge cases where the merge workflow's explicit close failed or where GitHub auto-close didn't trigger. The `transition_to_done` action type exists in the state machine for this purpose.

### 4. Route Triage

**Be ambitious. Prioritize user value. Keep work moving.**

No issue is "too big" for Legion — that's what the architect phase is for. Large or complex
issues go to Backlog where the architect breaks them down. Only route to Icebox if the issue
is genuinely unclear (missing context, ambiguous requirements, needs user clarification).

Controller routes Triage issues directly (no worker needed):

| Assessment | Route To |
|------------|----------|
| Urgent AND clear requirements | Todo (dispatch planner) |
| Bug label + clear reproduction steps + no architectural uncertainty | Todo (skip architect — dispatch planner directly) |
| Clear requirements, any size | Backlog (architect breaks down if large) |
| Ambiguous or missing context | Icebox (needs clarification) |

The skip-architect rule only applies when ALL conditions are met: `bug` label present,
description contains clear reproduction steps, and the change is scoped to a single
component. When in doubt, route to Backlog.

### 5. Pull from Icebox

**If active workers < 10:**
1. Check for Icebox items that have been clarified: look for `user-feedback-given` label OR new comments added since the issue was moved to Icebox
2. If no clarified items exist, skip — leave Icebox items until users respond
3. Move the oldest clarified item to Backlog
4. Dispatch architect

### 6. Cleanup Done (MUST run every iteration)

**This step MUST execute on every loop iteration.** Iterate over ALL Done issues from the collected
state that still have worker entries in the daemon. The daemon also auto-cleans Done issues during
state collection (Layer 1), and the merge worker cleans up at merge time (Layer 2). This controller
sweep is the **backup safety net** that catches stragglers — issues where the merge worker or daemon
auto-cleanup didn't fire (e.g., issue was closed manually, daemon restarted between collect cycles).

For each Done issue that has worker entries (check the `/workers` response):

1. Remove workspace:
```bash
# Use ANY worker ID for the issue — all modes share the same workspace
curl -s -X DELETE "http://127.0.0.1:$LEGION_DAEMON_PORT/workers/$WORKER_ID/workspace" \
  -H 'Content-Type: application/json' \
  -d '{"repo": "'"$ISSUE_REPO"'"}'
```

2. Prune all worker entries and crash history for the issue:
```bash
curl -s -X POST "http://127.0.0.1:$LEGION_DAEMON_PORT/workers/prune" \
  -H 'Content-Type: application/json' \
  -d '{"issueIds": ["'"$ISSUE_ID"'"]}'
```

Both calls are idempotent. The prune call removes all worker modes for the issue, their crash
history, and the dispatch validation cache. If the daemon auto-cleanup already handled this issue,
both calls are no-ops.

**Do NOT skip this step** even if the daemon auto-cleanup is active — the three layers are
defense-in-depth against disk exhaustion.

### 7. Write Heartbeat

```bash
The daemon handles heartbeat writing automatically. No manual heartbeat step needed.
```

### 8. Update To-Do List

Maintain in context:
```markdown
## Controller State
**Active workers:** [count] / 10 max
### Priority Queue
- [ENG-XX] description
### In Progress
- [ENG-YY] mode - worker running
### Blocked
- [ENG-ZZ] user-input-needed
```

### 9. Wait for Poller

The background poller handles timing. The main thread does not sleep — it processes poller reports as they arrive and returns to idle between reports. See **Polling Architecture** above.

If operating in fallback mode (no background tasks), end turn here. The external runtime re-invokes the controller for the next iteration.

## Dispatch vs Resume

## Repo-Specific Configuration (`.legion/config.yml`)

Repos can define Legion behavior in `.legion/config.yml` (committed in repo root). Worker workflows read this file at startup and apply recognized keys.

Controller behavior with repo config:
- Do not duplicate repo-specific rules in every dispatch prompt
- Assume workers will self-configure from `.legion/config.yml`
- If needed for diagnostics, query config from the worker workspace (via worker context) and compare against observed behavior

Supported schema and key precedence are documented at:
- @../legion-worker/references/config.md

### Backend in Prompts

Workers must know which backend they're on. The controller always includes the backend
in dispatch and resume prompts so workers don't need to check environment variables.

Build the backend suffix from `LEGION_ISSUE_BACKEND` and (for GitHub) the per-issue source
metadata extracted from `COLLECTED` in the action loop:

- **GitHub:** `(github backend, repo: $ISSUE_REPO)` — use the canonical `IssueState.source`
  owner/repo for the current issue; do not parse repo identity from `$ISSUE_IDENTIFIER`
- **Linear:** `(linear backend)`

### GitHub App Credentials (Worker Identity)

When GitHub App credentials are configured on the daemon, each worker mode maps to a
specific role identity. The daemon **automatically injects** role-specific credentials
into each worker's env during `POST /workers` — the controller does not need to fetch
or pass credentials manually.

**Mode-to-role mapping (handled by daemon):**

| Mode | Role | App Name |
|------|------|----------|
| `implement`, `merge` | `impl` | `legion-impl` |
| `review`, `test`, `architect`, `plan` | `review` | `legion-review` |

**How it works:**
1. Controller dispatches worker normally (no credential handling needed)
2. Daemon's `POST /workers` automatically:
   - Maps mode → role
   - Generates a fresh installation token for that role
   - Stores `GH_TOKEN`, `GIT_AUTHOR_*`, `GIT_COMMITTER_*` in worker's env
3. Worker startup sequence fetches env from `GET /workers/{id}/env`
4. Worker uses role-specific identity for all `gh` and `jj` operations

**Important constraints:**
- Never call `gh auth login`, `git config --global`, or write to `~/.config/gh/` or `~/.gitconfig`
- Credential injection is automatic — no `--env` flag needed for credentials
- If credentials are not configured for a role, workers use ambient credentials
- Tokens expire after ~1 hour; the daemon refreshes automatically
- Workers on separate role serves cannot access other roles' credentials (process isolation)
- Private key paths are scrubbed from all serve environments

### Dispatch (New Worker)

**Always use skill invocation (`/skill-name`), not file paths.** Workers load skills via the
skill system. Pointing them at file paths bypasses skill loading and risks the worker not
getting the full skill content.

```bash
# GitHub example:
legion dispatch "$ISSUE_IDENTIFIER" "$MODE" \
  --repo "$ISSUE_REPO" \
  --prompt "Invoke the /legion-worker skill for $MODE mode for $ISSUE_IDENTIFIER (github backend, repo: $ISSUE_REPO). Before starting, check for project-specific skills that may be relevant to this work."

# Linear example:
legion dispatch "$ISSUE_IDENTIFIER" "$MODE" \
  --prompt "Invoke the /legion-worker skill for $MODE mode for $ISSUE_IDENTIFIER (linear backend). Before starting, check for project-specific skills that may be relevant to this work."
```

The `dispatch` command handles: workspace creation (jj workspace add), daemon API call (POST /workers), initial prompt, and prints worker info.

For custom prompts, still include the backend suffix:
```bash
legion dispatch "$ISSUE_IDENTIFIER" "$MODE" \
  --repo "$ISSUE_REPO" \
  --prompt "Custom instructions here (github backend, repo: $ISSUE_REPO)"
```

**Note:** The daemon also supports auto-resolving repo from `issueStateCache` when `--repo` is
omitted. This is a **fallback for manual CLI callers** — the controller MUST always pass `--repo`
explicitly because it has the freshest data from the collect response. Do not rely on daemon
auto-resolve for controller dispatches.

### Resume (Prompt Existing Worker)

```bash
# User feedback relay (GitHub):
legion prompt "$ISSUE_IDENTIFIER" \
  "Invoke the /legion-worker skill. Check issue comments for user feedback. (github backend, repo: $ISSUE_REPO)"

# PR changes requested — tell them to invoke the skill, not give step-by-step fix instructions:
legion prompt "$ISSUE_IDENTIFIER" --mode implement \
  "Invoke the /legion-worker skill for implement mode. CI is failing on your PR — check the failures and fix. (github backend, repo: $ISSUE_REPO)"
```

If multiple workers exist for the same issue (different modes), specify mode with `--mode`.

Use resume for: user feedback relay, PR changes requested, retro after review approval.

### Advance (Preferred for Normal Flow)

`legion advance` is a high-level command that reads `suggestedAction` from the daemon's
state cache and executes it — dispatching workers, transitioning issue status, or resuming
workers as appropriate. It replaces manual dispatch + transition sequences for normal flow:

```bash
# Advance an issue to its next stage (reads suggestedAction from daemon cache)
legion advance "$ISSUE_IDENTIFIER"

# Force a specific stage (equivalent to dispatch --force)
legion advance "$ISSUE_IDENTIFIER" --stage implement

# See what would happen without doing it
legion advance "$ISSUE_IDENTIFIER" --dry-run
```

**`advance` handles:**
- `dispatch_*` actions → dispatches worker with correct mode and prompt
- `transition_to_*` actions → updates issue status in tracker + removes `worker-done` label
- `resume_*` actions → dispatches worker with appropriate resume prompt
- `skip`/`investigate`/`retry` → returns actionable message without executing

**When to use `advance` vs manual `dispatch`:**
- Use `advance` for normal lifecycle progression (it handles the full action mapping)
- Use `dispatch` directly when you need custom prompts, env vars, or workspace overrides

#### Auto-Progression

When `LEGION_AUTO_ADVANCE=true` is set (in `legion.yaml` as `auto_advance: true`):
- The daemon automatically dispatches the next worker when the current one finishes
- After each state collection (60s health tick), ready issues are auto-advanced
- The controller's role shifts to exception handling, triage, and priority management
- Check `LEGION_AUTO_ADVANCE` env var before manual dispatch — skip if auto-advance handles it

```bash
# In the loop body, check auto-advance before manual dispatch:
if [ "$LEGION_AUTO_ADVANCE" = "true" ]; then
  # Only handle exceptions — auto-advance handles normal flow
else
  # Use legion advance for each actionable issue
  legion advance "$ISSUE_IDENTIFIER"
fi
```

### Retro

**The retro phase is MANDATORY. Every issue that passes review gets a retro. No exceptions.**

Retro is triggered by resuming the **implement worker's existing session** — this preserves the implementer's full context. The retro skill handles spawning a fresh subagent for an outside perspective.

Always dispatch retro after review approval. Do not check routing hints, `skipRetro` flags, or any other conditions — retro runs unconditionally.

**Use skill invocation for retro too:**

```bash
# GitHub:
legion prompt "$ISSUE_IDENTIFIER" --mode implement \
  "Invoke the /legion-retro skill. (github backend, repo: $ISSUE_REPO)"

# Linear:
legion prompt "$ISSUE_IDENTIFIER" --mode implement \
  "Invoke the /legion-retro skill. (linear backend)"
```

**If the implement worker died** (action `dispatch_implementer_for_retro`), a fresh worker is dispatched in `implement` mode. This loses the implementer's perspective — both retro analyses will be from a fresh viewpoint.

**ADAPTIVE ROUTING GUARDRAILS — MUST NEVER BE SKIPPED:**
- Testing phase CANNOT be skipped — tester ALWAYS runs
- Retro phase CANNOT be skipped — retro ALWAYS runs after review approval
- Routing hints are ADVISORY — labels, PR state, daemon state remain authoritative
- When hints are missing/corrupt: fall back to full pipeline
- When multiple hints conflict: fall back to full pipeline

## Worker Inspection

The daemon is the controller's interface to workers. Use the daemon API, not direct port access.

```bash
# List all workers
curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers | jq '.[] | {id, status, port, sessionId}'

# Check worker status (busy/idle)
curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers/$WORKER_ID/status | jq '.'
```

The state machine reports `hasLiveWorker`, `workerMode`, and `workerStatus` for each issue.

### Session ID Source of Truth

**`GET /workers` is the ONLY reliable source for session IDs.** Never parse session IDs
from dispatch output, truncated logs, or cached values. The dispatch response may be
truncated or ambiguous — always verify against the daemon's worker list.

```bash
# CORRECT: Get exact session ID from daemon
SESSION_ID=$(curl -s http://127.0.0.1:$LEGION_DAEMON_PORT/workers | jq -r '.[] | select(.id == "'$WORKER_ID'") | .sessionId')

# WRONG: Parse from dispatch output (may be truncated)
# SESSION_ID=$(echo "$DISPATCH_OUTPUT" | grep -o 'ses_[a-zA-Z0-9]*')
```

**After every dispatch, verify the session ID from `GET /workers` before monitoring.**
Monitoring the wrong session ID leads to false "dead worker" reports — the worker may
be alive and productive while you abort and redispatch based on a non-existent session.

### Worker Health Assessment

To determine if a worker is healthy, stuck, or dead:

1. **Get exact session ID** from `GET /workers` (not from dispatch output)
2. **Read the transcript** — check the last assistant message content, not just message count
3. **Check last assistant output type**:
   - Has text or tool calls → alive (may be slow)
   - EMPTY (0-length response) → dead session (serve bug). Abort and redispatch.
   - NO_ASST (no assistant messages at all) → session never started. Abort and redispatch.
4. **"Flat message count" is ambiguous** — it can mean:
   - Worker is DONE and idle (check transcript for "idle" / "complete" text)
   - Worker is in a long tool call (check last tool state)
   - Worker is dead (check for EMPTY last response)
   - You are monitoring the WRONG session ID

**Never assume a worker is stuck based on message count alone.** Always read the transcript.

### Prompt Delivery to Busy Sessions

`prompt_async` to a busy session is **silently dropped** (OpenCode Runner bug). The prompt
is queued as a user message but the model loop never processes it. This means:

- **Never nudge busy workers** — the nudge won't be seen
- **Wait for idle, then prompt** — or abort and redispatch
- **+1 message after nudge does NOT mean the worker responded** — it's just your queued prompt
Use these signals — don't independently verify worker liveness.

### Session Versioning (Escape Hatch Only)

Session IDs are **deterministic** — `computeSessionId(teamId, issueId, mode)` uses UUID v5.
Same inputs always produce the same session ID. If the serve still has that session in
memory, re-dispatching with the same issue ID and mode re-attaches to the existing session
(the serve returns 409 DuplicateIDError, which the daemon treats as "reuse").

**This is by design — session reuse preserves the worker's full context.** A worker that has
been reading code, making changes, and iterating on review feedback carries all that context
in its session. Re-dispatching without a version increment reconnects to that session, so
the worker continues where it left off.

The `--version` flag on `legion dispatch` exists **only as an escape hatch** for unrecoverable
sessions — e.g., the session is corrupted, the serve crashed and lost the session, or the
workspace was deleted and recreated. **Do NOT increment versions during normal pipeline
operation.** Each version increment creates a completely fresh session that has zero context
about the issue, the codebase changes, or prior work.

**NEVER do this:**
```bash
# WRONG: Incrementing version on every dispatch throws away all worker context
legion dispatch issue-123 implement --version 1 --prompt "Fix review feedback"
# Later...
legion dispatch issue-123 implement --version 2 --prompt "Fix CI"
# Later...
legion dispatch issue-123 implement --version 3 --prompt "Fix more things"
```

**Do this instead:**
```bash
# CORRECT: Same version (or no version) = worker resumes with full context
legion dispatch issue-123 implement --prompt "Fix review feedback"
# Later...
legion dispatch issue-123 implement --prompt "Fix CI"  # Same session, worker remembers everything
```

A context-less worker is dangerous — it doesn't understand the branch topology, prior
changes, or conventions established during earlier work. This can lead to destructive
actions like force-pushing to the wrong branch.
### Don't Delete a Workspace While Workers May Resume

**A worker whose workspace has been deleted cannot respond to prompts.** Every tool call
fails because the working directory no longer exists. The session appears "busy" but
produces no output — it looks like the worker is stalled, but the root cause is the
missing workspace.

This applies to both active workers AND idle workers you might want to resume later
(e.g., for retro). Only delete workspaces during Cleanup Done (step 6) — after the
issue is fully complete and no further prompts will be sent.

## Observability Rules

### 1. Trust the state machine

The state machine checks worker liveness, PR status, labels, and draft state. Use
`fetch-and-collect` (GitHub) or POST issue data to `/state/collect` (Linear) and route
by `suggestedAction`. Don't independently check PRs, ports, or process status — that's
the state machine's job.

For the full observability architecture and failure case studies, see `docs/solutions/daemon/controller-observability.md`.

### 2. Never reconstruct state machine input

For GitHub, call `fetch-and-collect` — the daemon fetches project items internally via
paginated GraphQL. For Linear, pass tracker output directly to `/state/collect`. Never
hand-craft JSON, filter issues, or inject your own assumptions about labels or status.

### 3. Fresh data every loop iteration

Fetch issues from the tracker at the start of every loop. Don't carry labels, statuses, or
worker state between iterations — they go stale.

### 4. One PR per issue

Each issue gets its own workspace, its own branch (named after the issue ID), and its
own PR. Do not accumulate changes from multiple issues into a single PR — this makes
it impossible to track what's merged.

## Labels

| Label | Meaning |
|-------|---------|
| `worker-done` | Worker finished phase, controller acts |
| `worker-active` | Worker dispatched and running |
| `user-input-needed` | Blocked on human, controller skips |
| `user-feedback-given` | Human responded, controller resumes |
| `needs-approval` | Waiting for human approval (architect output or merge readiness) |
| `human-approved` | Human approved, controller advances to planner |
| `test-passed` | Tester verified behavior, controller advances to Needs Review |
| `test-failed` | Tester found issues, controller returns to implementer |

### Label Batching Pattern

Combine multiple label changes on the same issue into a single `gh issue edit` call to reduce GitHub API calls:

**Instead of multiple calls:**
```bash
gh issue edit $ISSUE_NUMBER --remove-label "worker-done" -R $ISSUE_REPO
gh issue edit $ISSUE_NUMBER --remove-label "test-passed" -R $ISSUE_REPO
```

**Use a single batched call:**
```bash
gh issue edit $ISSUE_NUMBER \
  --remove-label "worker-done" \
  --remove-label "test-passed" \
  -R $ISSUE_REPO
```

**Combined add + remove:**
```bash
# Remove worker-done and test-passed, add nothing (clean up after test pass → needs review)
gh issue edit $ISSUE_NUMBER \
  --remove-label "worker-done" \
  --remove-label "test-passed" \
  -R $ISSUE_REPO

# Remove worker-done and worker-active, add nothing (after processing)
gh issue edit $ISSUE_NUMBER \
  --remove-label "worker-done" \
  --remove-label "worker-active" \
  -R $ISSUE_REPO
```

**When dispatching a worker, combine worker-active:**
```bash
# Add worker-active in same call as status update where possible
gh issue edit $ISSUE_NUMBER --add-label "worker-active" -R $ISSUE_REPO
```

This reduces GitHub API calls from N individual calls to 1 batched call per label group.

## Red Flags — STOP and Verify

If you catch yourself thinking any of these, STOP. You're about to make a mistake.

| Thought | What to do instead |
|---------|--------------------|
| "Let me construct the JSON for the state machine" | Use `fetch-and-collect` (GitHub) or POST tracker output to `/state/collect` (Linear) — no hand-crafting |
| "I know the label/status from last iteration" | Fetch fresh from the tracker. State goes stale between iterations. |
| "The changes are lost" | Check open PRs (`gh pr list`), worker workspaces (daemon API), and issue comments before concluding anything is lost. Do NOT run `jj` — dispatch a worker to check version control. |
| "I'll give the worker specific instructions" | State the mode, issue ID, and backend. Invoke the skill. Let the workflow guide the worker. |
| "Let me check the worker's port directly" | Use the daemon API (`/workers`, `/workers/:id/status`). The state machine reports liveness. |
| "I'll accumulate these changes into the existing PR" | One issue = one workspace = one branch = one PR. |
| "This issue is too big/complex" | No issue is too big. That's what the architect phase is for. Route to Backlog. |
| "The worker is busy, I'll wait" | Check the transcript. If the worker received prompts but produced no response, the workspace may have been deleted. A worker cannot function without its workspace. |
| "CI can be fixed later" | CI is the implementer's responsibility. If CI is failing, the implementer isn't done — re-dispatch. |
| "This worker keeps failing, let me increment the version" | **NEVER increment `--version` during normal operation.** Version increments destroy all worker context. Re-dispatch without version — the worker resumes with full context of prior work. Version is an escape hatch for unrecoverable sessions only. |
| "Let me skip planning, the issue is simple enough" | STOP. Every phase runs. No exceptions. |
| "Testing isn't needed, it's a trivial change" | STOP. The tester ALWAYS runs. |
| "Let me skip retro, the PR is clean" | STOP. Retro is MANDATORY. Always dispatch retro after review approval. |
| "Let me just merge this PR directly" | STOP. Dispatch a merge worker. |
| "I'll rebase and push this fix" | STOP. Dispatch an implementer. |
| "I'll run the tests myself" | STOP. Dispatch a tester. |
| "Let me quickly edit this file" | STOP. You're doing worker work. Dispatch the appropriate worker. |
| "This worker is dead — 0 messages" | STOP. Verify session ID from `GET /workers` first. You may be checking the wrong session. |
| "Worker is stuck — flat message count" | Read the transcript. Flat can mean done, slow, or wrong session ID. Check last assistant message content. |
| "I'll nudge the busy worker" | STOP. `prompt_async` to busy sessions is silently dropped. Wait for idle or abort/redispatch. |
| "This worker looks stuck, I'll abort and redispatch" | STOP. Message the worker on Envoy first. Check message count. Only abort after confirmed stuck or timeout. |
| "Let me run tl run / Docker / tests myself" | STOP. You're doing worker work. Dispatch the appropriate worker. |

## Common Mistakes

| Mistake | Correction |
|---------|------------|
| Spawn new worker for user feedback | **Resume** existing session via HTTP API |
| Skip Icebox when capacity exists | Pull oldest Icebox item if workers < 10 |
| Plan Triage items directly | Route first (to Icebox/Backlog/Todo), then workers act |
| Exit after processing all issues | **Never exit** - loop continuously via background polling (see Polling Architecture) |
| Process issue with live worker | Skip it - worker is already handling |
| Give workers step-by-step fix instructions | Invoke the skill. State the mode, issue ID, and backend only. |
| Forget to remove `worker-done` after processing | Always remove `worker-done` label after acting on it. |
| Classify issues as "too big" | Route to Backlog for architect breakdown. No issue is too big for Legion. |
| Advance pipeline with CI failing | Re-dispatch implementer with CI failure output. Don't dispatch reviewer until CI passes. |
| Delete a workspace to "reset" a worker | **Never delete a workspace while the worker might be resumed.** A deleted workspace silently kills the worker — prompts arrive but every tool call fails. Only delete during Cleanup Done. |
| Increment `--version` on every dispatch | **Never increment version during normal pipeline operation.** Each increment creates a fresh session with zero context. A context-less worker is dangerous — it can push to wrong branches, overwrite work, or break the repo. Only use `--version` when a session is truly unrecoverable (serve crash, corrupted session). |
| Running `jj`, `gh pr merge`, or editing files | Controllers dispatch workers. Never touch code/branches/PRs directly. |
| Skipping phases because "it's simple" | Every phase runs. Simple issues just go through faster. |
| Checking wrong session ID (truncated from output) | **Always get session IDs from `GET /workers`**, never from dispatch output. Truncated IDs cause false "dead worker" reports. |
| Nudging busy workers via `prompt_async` | Prompts to busy sessions are silently dropped. Wait for idle or abort/redispatch. |
| Aborting workers without reading transcript | Read the last assistant message first. "Flat" can mean done, slow, or wrong session ID — not just stuck. |
| Running sandboxes, Docker, tests, or `tl run` | Controller dispatches workers. Never run evaluation tools directly. |

## Status Flow

```
Triage ─┬─► Icebox ─► Backlog ─► Todo ─► In Progress ─► Testing ─► Needs Review ─► Retro ─► Done
        ├─► Backlog ──────────────┘           │                        │
        └─► Todo ─────────────────────────────┴────────────────────────┘
```
