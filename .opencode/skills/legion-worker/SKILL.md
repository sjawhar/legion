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
4.5. **Write handoff data before signaling.** Each workflow has a handoff write step — you MUST attempt it before adding `worker-done`. The CLI will fail loudly if the write encounters an error. If the write fails, diagnose the issue and note it in your exit comment.
5. **Clean up on exit** - remove `worker-active` label when exiting (done or blocked)
6. **Notify the controller via Envoy** — after adding `worker-done`, send exactly one completion notification so the controller doesn't have to wait for its next polling cycle. Use `envoy_publish(topic="notifications.role.legion-controller", message="Worker done: <issue> <mode> <outcome>")`. If `envoy_publish` fails, that's fine — the label is the source of truth.

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

Load repo-specific config from workspace root (if present):

```bash
cat .legion/config.yml 2>/dev/null || true
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
2. A **signal completion** todo as the LAST item:
   - `Signal completion: push changes, unsubscribe from Envoy topics, add worker-done label, remove worker-active label, notify controller via Envoy`
   - Keep this todo `pending` until you have actually run the label commands and verified they succeeded
   - **Do not mark this complete early** — it is your contract with the controller

The signal completion todo ensures you never finish a session without updating labels.
If you are about to stop or exit for any reason, check whether this todo is still pending — if so, do it now.
### Blocking on User Input

When you need human input that the legion-oracle can't answer:

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
4. Exit immediately

The controller will resume your session when the user responds.

### Exiting

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

Review signals outcome via PR draft status BEFORE `worker-done`:
- **PR ready** (not draft) - no blocking issues, approved
- **PR draft** - blocking issues found, changes requested

## Research Before Escalating

Before blocking on user input, workers should invoke `/legion-oracle [your question]` to search institutional knowledge (docs/solutions/, codebase patterns). Only escalate to the user (via issue comment + label) if the legion-oracle cannot answer.

## Reference

Label conventions: @references/linear-labels.md (Linear), @references/github-labels.md (GitHub)
