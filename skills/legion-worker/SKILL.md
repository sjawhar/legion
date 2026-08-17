---
name: legion-worker
description: Use when dispatched by Legion controller to work on an issue in a jj workspace
---

# Legion Worker

Router skill for Legion issue work. Dispatched by controller with a mode parameter.

## Context from Prompt

The controller dispatches you with a prompt that includes your **issue ID**, **mode**, and **backend**:

- **GitHub:** `/legion-worker implement mode for acme-widgets-42 (github backend, repo: acme/widgets)`
- **Linear:** `/legion-worker plan mode for ENG-21 (linear backend)`

Extract these values from the prompt. For GitHub issues, also derive the **owner**, **repo**,
and **issue number** from the issue ID (format: `owner-repo-number`).

Throughout this skill and its workflows, `$LEGION_ISSUE_ID`, `$ISSUE_NUMBER`, `$OWNER`, and
`$REPO` are **placeholders** — substitute the values you extracted from your prompt context.
Use the **backend** from your prompt to choose GitHub CLI or Linear MCP commands.

## Essential Rules

1. **Read issue first**
   - **GitHub:** `gh issue view $ISSUE_NUMBER --json title,body,labels,comments,state -R $OWNER/$REPO`
   - **Linear:** `linear_linear(action="get", id="$LEGION_ISSUE_ID")`
2. **Use jj, not git** - changes auto-tracked (see jj safety rules below)
3. **Only the implementer creates branches** - the implement workflow creates the branch and
   opens the PR. Reviewers, retro, and closers push to the existing branch. Never create new
   branches or bookmarks in review, retro, or merge workflows.
   **Exception:** The retro workflow has a recovery fallback for when the tracked branch is
   lost — it may re-create the bookmark in that narrow case. See the retro SKILL.md for details.
4. **Signal completion (MOST IMPORTANT)** — before you stop for ANY reason, you MUST: push your work, unsubscribe from explicit Envoy topics (see Exiting), add `worker-done` label, remove `worker-active` label. If you skip this, the issue silently stalls. Create a todo for this at session start (see Required Startup Todos below).
4.5. **Write handoff data before signaling.** Each workflow has a handoff write step — you MUST complete it and confirm `.legion/$MODE.json` exists before adding `worker-done`. If the file is missing, stop, diagnose the failure (for example: `legion` missing from `PATH` or wrong `--workspace`), and do not signal completion until the failure is fixed or explicitly documented in your exit comment.
5. **Clean up on exit** - remove `worker-active` label when exiting (done or blocked)
6. **Notify the controller via Envoy** — after adding `worker-done`, send exactly one completion notification so the controller doesn't have to wait for its next polling cycle. Use `envoy_publish(topic="notifications.role.legion-controller", message="Worker done: <issue> <mode> <outcome>")`. If `envoy_publish` fails, that's fine — the label is the source of truth.
7. **Never stall on subagents** — when spawning background tasks (cross-family review, Oracle, spec compliance, etc.), always set `timeout_seconds` (typically 180s). If the subagent times out or fails, skip that step and continue with the workflow. Your primary obligation is to complete the workflow and signal `worker-done`. Subagent steps are quality improvements, not hard prerequisites — the downstream human/bot review is the authoritative quality gate.
8. **Never block bash with long-running commands** — any command expected to take >60 seconds (builds, deploys, serves, eval runs, smoke tests) MUST run in a tmux session. Workers that block their bash session get killed by tool timeouts.
   ```bash
   # Run long command in background tmux session
   tmux new-session -d -s <name> '<command>'
   # Monitor progress
   tmux capture-pane -t <name> -p | tail -20
   # Check if still running
   tmux has-session -t <name> 2>/dev/null && echo "running" || echo "done"
   ```
   Never run `bun run serve`, `pulumi up`, long test suites, or similar commands directly in bash.
9. **Circuit breaker on repetitive tool calls** — if you've called the same tool more than 10 times consecutively with similar input, STOP and reassess your approach. You are likely in a loop. Steps: (a) describe what you're trying to achieve, (b) identify why the repeated calls aren't working, (c) try a fundamentally different approach. Transcript analysis found 3 catastrophic loop incidents (4,490 wasted tool calls) where workers repeated the same failing action hundreds of times.

## Skill Discipline

You are executing work with an approved plan. Do NOT invoke the brainstorming or writing-plans skills — your workflow has already been designed. Follow your assigned workflow file. The individual skills referenced in your workflow (TDD, subagent-driven-development, etc.) are appropriate to load and use.

## jj Safety Rules

- **Always `jj new` to create isolated commits.** Never `jj edit @-` to go back to a parent — this changes what `@` points to and makes `jj abandon` dangerous.
- **Never `jj abandon` without first running `jj log`** to verify what `@` is. Abandoning the wrong commit destroys all changes on it.
- **If you accidentally abandon the wrong commit:** `jj op restore` recovers the last operation.
- **Before pushing, check ancestry:** `jj log -r 'ancestors(@, 5)'` — verify only your issue's commits are in the chain, not unrelated work.

## Session Lifecycle

### Starting

Sync with main and create a fresh commit on your branch:

```bash
jj git fetch
jj rebase -d main
jj new  # Fresh commit for this session
```

Orient yourself in the workspace:
```bash
jj-agent-status  # Shows branches, bookmark state, other agents, needs-attention items
```

Load repo-specific config from workspace root (if present):

```bash
if [ -f .legion/config.yml ]; then cat .legion/config.yml; fi
```

Then:
- Recognize and apply keys documented in @references/config.md
- Echo recognized keys + effective values before workflow-specific work
- If file is missing or malformed, proceed with defaults (no errors)
- Ignore unknown keys

Fetch per-worker environment variables from the daemon (non-blocking):

```bash
# Fetch and export per-worker env vars (requires LEGION_DAEMON_PORT)
# ISSUE_ID and MODE are extracted from your dispatch prompt (see "Context from Prompt" above).
# Example: dispatched with "implement mode for sjawhar-legion-106" → ISSUE_ID=sjawhar-legion-106, MODE=implement
if [ -n "$LEGION_DAEMON_PORT" ]; then
  _WORKER_ID="$(echo "${ISSUE_ID}-${MODE}" | tr '[:upper:]' '[:lower:]')"
  _ENV_FILE=$(mktemp) && \
    curl -fsS "http://127.0.0.1:$LEGION_DAEMON_PORT/workers/$_WORKER_ID/env" 2>/dev/null \
    | jq -r '.env // {} | to_entries[] | "export " + .key + "=" + (.value | @sh)' \
    > "$_ENV_FILE" && \
    . "$_ENV_FILE"; \
    rm -f "$_ENV_FILE"
fi
```

The daemon stores per-worker env vars passed via `legion dispatch --env '{"KEY":"VALUE"}'`.
This step retrieves them so tools like `gh` and `jj` see role-specific credentials. If the
endpoint is unavailable or returns empty, the worker proceeds with the shared process environment.

Optionally read prior handoff data (advisory, non-blocking):

```bash
legion handoff read --workspace . 2>/dev/null || echo '{}'
```

Prior phase data (from architect, plan, implement, etc.) is available in `.legion/` on this branch. Reading it is optional — individual workflow files handle phase-specific handoff reads. This note is a reminder that this data exists. Never block on missing handoff data.

If you're resuming after user feedback, also read the issue comments for the answer.
If you previously created a PR, re-subscribe to PR topics: `envoy_subscribe(["notifications.github.$OWNER.$REPO.pr.$PR_NUMBER.>"])` (the daemon re-subscribes you to issue topics automatically on resume).

### Required Startup Todos

**Before starting any workflow work**, create these todos (adapt the signal todo to your mode):

1. Your workflow-specific work items (from the workflow file)
2. A **write handoff data** todo:
   - `Write handoff data: complete the workflow handoff write and verify .legion/$MODE.json exists before signaling completion`
   - Keep this todo `pending` until the handoff file exists on disk
3. A **signal completion** todo as the LAST item:
   - `Signal completion: push changes, unsubscribe from Envoy topics, add worker-done label, remove worker-active label, notify controller via Envoy`
   - Keep this todo `pending` until you have actually run the label commands and verified they succeeded
   - **Do not mark this complete early** — it is your contract with the controller

The signal completion todo ensures you never finish a session without updating labels.
If you are about to stop or exit for any reason, check whether this todo is still pending — if so, do it now.
### When Stuck — Try Oracle FIRST

**Before escalating to humans**, always invoke `/legion-oracle [your question]` to search institutional knowledge (`docs/solutions/`, codebase patterns, past issue resolutions). Oracle answers 62% of questions without human help.

Example: `/legion-oracle How does the controller handle cross-mode cleanup for envoyTopics?`

Only proceed to the escalation flow below if oracle cannot answer or the answer is insufficient.

### Blocking on User Input

When you need human input that the oracle can't answer:

1. Push your work: `jj git push`
2. Post a structured escalation comment to the issue:

**GitHub:**
```
gh issue comment $ISSUE_NUMBER --body "## Escalation

**Phase:** [current mode - architect/plan/implement/review]
**Completed:** [what work has been done so far]

### Blocker
[Specific question or decision needed — be precise]

### Options Considered
1. [Option A] — [trade-offs]
2. [Option B] — [trade-offs]
3. [Option C if applicable]

### Context
- **Remaining estimate:** [rough scope of remaining work after unblock]
- **Expertise needed:** [domain knowledge required to answer, e.g. 'product decision', 'API design', 'infrastructure']
- **Branch:** [current branch name if applicable]" -R $OWNER/$REPO
```

**Linear:**
```
linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="## Escalation

**Phase:** [current mode - architect/plan/implement/review]
**Completed:** [what work has been done so far]

### Blocker
[Specific question or decision needed — be precise]

### Options Considered
1. [Option A] — [trade-offs]
2. [Option B] — [trade-offs]
3. [Option C if applicable]

### Context
- **Remaining estimate:** [rough scope of remaining work after unblock]
- **Expertise needed:** [domain knowledge required to answer, e.g. 'product decision', 'API design', 'infrastructure']
- **Branch:** [current branch name if applicable]")
```

3. Update labels: add `user-input-needed`, remove `worker-active`
4. Notify the controller via Envoy (best-effort):
   ```
   envoy_publish(topic="notifications.role.legion-controller", message="Worker blocked: $ISSUE_NUMBER [current mode] needs user input")
   ```
   If `envoy_publish` fails, continue — the label is the source of truth.
5. Exit immediately

The controller will resume your session when the user responds.

### Exiting

Before pushing, verify the required handoff file exists for modes that write handoff data:

```bash
if printf '%s\n' architect plan implement test review | grep -qx "$MODE"; then
  if [ ! -f ".legion/${MODE}.json" ]; then
    echo "FATAL: Missing required handoff file .legion/${MODE}.json"
    echo "STOP: Do NOT push or signal worker-done. Return to the workflow handoff step and diagnose the failure."
    echo "If the write cannot be fixed, document the failure in your exit comment before signaling completion."
  fi
fi
```

Always push before exiting:

```bash
jj git push
```

Then unsubscribe from explicit issue and PR topics (best-effort, non-blocking).
**IMPORTANT:** Use explicit topic list, NOT empty-array unsubscribe — empty array would
also remove `notifications.agent.{sessionId}` and create a delivery gap before daemon cleanup.

```
# Unsubscribe from issue and PR topics (substitute your actual values)
envoy_unsubscribe([
  "notifications.github.$OWNER.$REPO.issue.$ISSUE_NUMBER.>",
  "notifications.github.$OWNER.$REPO.pr.$PR_NUMBER.>"    # only if a PR was created
])
```
If `envoy_unsubscribe` fails, continue — the daemon's `DELETE /workers` is the authoritative cleanup.

Then update labels:
- Add `worker-done` if your mode requires it (see routing table)
- Remove `worker-active` (the controller added this when dispatching you)

Then notify the controller via Envoy (best-effort, non-blocking):
```
envoy_publish(topic="notifications.role.legion-controller", message="Worker done: <issue-id> <mode> completed")
```
If `envoy_publish` fails, continue — the label is the source of truth.

## Mode Routing

| Mode | Workflow | Adds `worker-done` |
|------|----------|-------------------|
| `architect` | @workflows/architect.md | Yes (or on children) |
| `plan` | @workflows/plan.md | Yes |
| `implement` | @workflows/implement.md | Yes |
| `test` | @workflows/test.md | Yes |
| `review` | @workflows/review.md | Yes |
| `merge` | @workflows/merge.md | No |

**Lifecycle order:** architect → plan → implement → test → review → (implement → test if changes requested) → retro → merge

**Retro** is not a mode — the controller resumes the implement worker's session with `/legion-retro`, preserving full implementation context. See the `legion-retro` skill.

## Review Mode Signaling

Review signals outcome via native GitHub review API BEFORE `worker-done`:
- **Approved** (`gh pr review --approve`) — no blocking issues
- **Changes requested** (`gh pr review --request-changes`) — blocking issues found

## Research Before Escalating

## Reference

Label conventions: @references/linear-labels.md (Linear), @references/github-labels.md (GitHub)
