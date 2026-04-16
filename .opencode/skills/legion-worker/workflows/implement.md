# Implement Workflow

Execute implementation using TDD with subagent-driven development.

## Mode Detection

The controller passes explicit mode in the dispatch prompt:
- `"implement"` or `"fresh"` → Fresh Implementation
- `"address comments"` or `"changes"` → Address Comments

Trust the controller's explicit mode parameter.

> **Note:** The daemon API mode is always `implement`. The sub-mode (fresh vs address-comments) is conveyed in the controller's prompt text, not the API call.

## Tools Referenced

This workflow references environment-provided tools. These are available in the OpenCode runtime, not defined in this repo:

| Tool | Source | Purpose |
|------|--------|---------|
| `task_create` | OpenCode task system | Create a task with dependencies (`blockedBy`) |
| `task_claim_next` | OpenCode task system | Atomically claim the next ready task |
| `task_update` | OpenCode task system | Mark task completed/failed |
| `task_list` | OpenCode task system | List tasks and their status |
| `background_task` | OpenCode agent system | Spawn a background subagent |
| `/analyze` | `sjawhar/analyze` skill | Run code quality agents on recent changes |

The task system enables parallel execution with dependency ordering. If these tools are unavailable in your environment, execute tasks sequentially following the plan's dependency annotations.
---

## All Modes: Rebase First

```bash
jj git fetch
jj rebase -d main
```

Resolve any conflicts before proceeding.

### 1.2. Load Repo Config

Read repo config from workspace root:

```bash
if [ -f .legion/config.yml ]; then cat .legion/config.yml; fi
```

Apply @references/config.md semantics:
- Parse and apply recognized keys for `implement` mode only
- Merge `phases.implement.*` overrides on top of top-level values
- Echo recognized keys/effective values for auditability
- If missing or malformed, continue with defaults and note fallback in implement handoff

Implement-mode keys:
- `skills.required` (plus `phases.implement.skills.required` if present): load additively
- `notifications.ping_reporter_on_pr`: include `@<reporter>` in PR body summary
- `notifications.slack_channel`: best-effort status update via `slack-bot` skill if available

---

## ⚠️ When Stuck — Mandatory Oracle Consultation

**This is not optional.** When you hit any of these triggers, STOP what you're doing and consult the oracle subagent before spending another turn on the problem:

### Triggers

1. **3+ turns on the same problem** — if you've attempted the same fix or investigation 3 times without progress, you are looping. Stop.
2. **Complex debugging** — stack traces spanning multiple packages, race conditions, flaky tests with no obvious cause, or errors that don't reproduce consistently.
3. **Architectural uncertainty** — unsure whether to refactor vs. patch, which module owns a responsibility, or how a change will interact with other subsystems.
4. **Unfamiliar domain** — working in code you haven't seen before with non-obvious conventions or implicit contracts.

### How to Invoke

Spawn an oracle subagent via `background_task`:

```
background_task(
  description: "Oracle consultation: <1-line summary of what you're stuck on>",
  subagent_type: "oracle",
  prompt: """
  I'm implementing <issue description>.

  **What I'm stuck on:** <specific problem>
  **What I've tried:** <list of approaches and why they failed>
  **Key files:** <paths to relevant files>

  Help me debug this / evaluate these approaches / identify what I'm missing.
  """,
  timeout_seconds: 180
)
```

### Rules

- **Do not skip this.** "I'll try one more thing" after 3 failures is the exact pattern this rule prevents.
- **Do not wait for oracle to finish before doing other work** — it runs in background. Continue with other tasks if available, then integrate oracle's findings.
- **If oracle times out (>3 min):** Proceed with your best judgment. Note "Oracle consultation timed out" in the handoff. The oracle is a circuit-breaker, not a gate.
- **After oracle responds:** Evaluate its suggestions critically. Apply what makes sense, discard what doesn't. You own the final decision.

> **Why this matters:** Worker transcript analysis shows that stuck workers burn 10-15 turns on problems that oracle resolves in one consultation. The cost of invoking oracle is ~30 seconds. The cost of not invoking it is 10+ wasted turns and often a wrong fix.

---

## Mode 1: Fresh Implementation

### 1. Load Plan

> **CRITICAL: The plan is in the issue COMMENTS, not the issue body.** The issue body contains the
> original spec/request. The plan was posted as a comment by the plan worker. When fetching the
> issue, read ALL comments to find the plan — do not rely solely on the body.

Fetch issue and comments:

- **GitHub:** `gh issue view $ISSUE_NUMBER --json title,body,labels,comments,state -R $OWNER/$REPO`
- **Linear:** `linear_linear(action="get", id=$LEGION_ISSUE_ID)`

### 1.5. Check for Project-Specific Skills

Before implementing, check if the repo has skills relevant to this work:

1. List available skills in the repo beyond standard Legion workflows
2. Match skills to the issue domain — if the plan or issue references a specific workflow, there may be a skill for it
3. Invoke any relevant skills before or during implementation
4. Check AGENTS.md and CLAUDE.md for project-specific conventions (coding standards, testing requirements, domain-specific processes)

### 1.6. Read Prior Handoffs (Advisory)

Read any prior handoffs from architect or plan phases (non-blocking):

```bash
legion handoff read --workspace . 2>/dev/null || echo '{}'
```

If architect or plan handoffs are present, note any concerns, routing hints, or learnings used. This is informational only — proceed regardless of whether these files exist.

**WARNING: Handoff data may be stale** — `.legion/` files persist across workspace reuse and may be from a different issue that previously used this workspace. Verify the handoff content references YOUR issue before trusting it. If it references a different issue, IGNORE it and rely on the issue comments and PR.

**Skill loading from plan handoff:** If the plan handoff includes a `requiredSkills.implement` array, invoke each listed skill before proceeding to step 2. This front-loads skills the planner identified as relevant, and replaces the independent skill discovery in step 1.5 for this run.

    # Example: if plan handoff contains requiredSkills.implement: ["reskin-environment", "task-workflow"]
    # Invoke each skill:
    # /reskin-environment
    # /task-workflow

If `requiredSkills` is absent or the plan handoff is missing, proceed with step 1.5's independent skill discovery as the fallback (current behavior, no regression).

**Config-required skills are additive:** if config provides `skills.required`, invoke those skills in addition to plan handoff `requiredSkills.implement` and independently discovered skills.

### 1.7. Inject Relevant Learnings

Follow the injection algorithm in @references/knowledge-injection.md using these keyword sources:

| Keyword Source | Fallback |
|---------------|----------|
| Issue title | — (always available) |
| Plan handoff `concerns[]` | Issue title only |
| Plan handoff `requiredSkills.implement[]` | Issue title only |
| File paths mentioned in plan (from issue body or plan context) | Issue title only |

Extract keywords from all available sources above. Match against `docs/solutions/index.json` to surface patterns, pitfalls, and implementation guidance relevant to this work.

Output the injected learnings visibly in the session before proceeding to coding. If no relevant learnings are found, output "No relevant learnings found." and continue.

**Graceful degradation:** If `docs/solutions/index.json` is missing, invalid, or handoff data is unavailable, skip silently and proceed to step 2.

### 1.8. Implementation Principles

**Fail fast, fail loud.** When writing code:
- Do NOT swallow errors with `2>/dev/null || true`, empty catch blocks, or silent fallbacks
- Do NOT use `try { ... } catch(e) {}` patterns that hide failures
- Prefer crashing on unknown state over continuing in ambiguous state
- Programs should operate RELIABLY, which includes crashing when in an unknown state and recovering by restarting
- If an operation can fail, handle the failure explicitly (log it, propagate it, or retry with backoff) — never swallow it

**Test from the package, not the monorepo root:**
- Run `bun test` from the package directory (e.g., `packages/daemon`), not the monorepo root
- If tests fail, verify they pass on a clean checkout of main before investigating further — test failures in your workspace are your responsibility, not "pre-existing"

**Don't build binaries in the package directory** — use `go build -o /tmp/<name>` to avoid accidentally committing binaries.

### 2. Invoke Skills (in order)

1. `/superpowers/executing-plans` - Load and structure the plan
2. `/superpowers/test-driven-development` - RED-GREEN-REFACTOR cycle
3. `/superpowers/subagent-driven-development` - Parallel execution for independent tasks

#### Parallel Execution with Task System

When the plan contains independent tasks (annotated with parallelism information):

1. **Create task graph:** For each task in the plan, use `task_create` with appropriate `blockedBy` edges based on the plan's dependency annotations.

2. **Spawn worker sessions:** Create N subagent sessions (one per independent task group). Each session loops:
   - `task_claim_next` — atomically claim the next ready task
   - Execute the claimed task
   - `task_update(status="completed")` — mark done
   - Repeat until no ready tasks remain

3. **Monitor progress:** Use `task_list` to track overall progress. The task system handles:
   - **Dependency ordering:** Tasks only become "ready" when all `blockedBy` dependencies are completed/cancelled
   - **Lock prevention:** `task_claim_next` atomically claims to prevent double-work
   - **Lease recovery:** If a session crashes, expired leases are automatically reclaimed
   - **Retry cap:** Tasks that fail 3 times are flagged for escalation

4. **Convergence:** When `task_list` shows all tasks completed or cancelled, proceed to the next step (Analyze).

**When to use parallel execution:**
- Plan has 3+ independent tasks
- Tasks don't share mutable state (different files/modules)
- Each task is self-contained enough for an independent session

**When to use sequential execution:**
- Plan has mostly sequential dependencies
- Tasks are small enough that parallelism overhead isn't worth it
- Tasks share the same files (merge conflict risk)

#### Wave-Based Parallelism

When a plan has both independent AND dependent tasks, group them into **waves**:

```
Wave 1 (parallel): T1 (jj.py), T2 (task_state.py), T3 (source_detection.py)
  ↓ all complete
Wave 2 (parallel): T4 (task_commands.py), T5 (merge_utils.py)
  ↓ all complete  
Wave 3 (parallel): T6 (tests), T7 (integration)
```

**The critical rule: never dispatch multiple subagents that edit the same file.**
Concurrent edits to the same file cause silent overwrites — the last writer wins and
earlier agents' work is lost. If two tasks both need to modify `task_commands.py`,
they must be in the same wave (sequential) or one must `blockedBy` the other.

**Grouping into waves:**
1. List all tasks and which files they create or modify
2. Tasks that touch disjoint files can run in parallel (same wave)
3. Tasks that share a file must be sequential (different waves, with dependency edges)
4. Within a wave, all tasks must complete before the next wave starts

**Wave failure handling:** If a task in a wave fails, it follows the existing retry
policy (3 attempts before escalation). Other tasks in the wave continue independently.
The next wave does NOT start until all tasks in the current wave are completed or cancelled.

**Wave timeout:** If a wave has not converged (all tasks completed/cancelled) within **5 minutes**, cancel all remaining running tasks in the wave and proceed. Execute uncompleted tasks sequentially in the main session as a fallback. Note "Wave N timed out — executed remaining tasks sequentially" in the handoff. **Do NOT stall indefinitely waiting for subagent convergence.**

### 3. Analyze

Invoke `/analyze` to run cleanup agents.

**If `/analyze` fails or times out (>3 min):** Skip and proceed to Pre-Ship Verification. The pre-ship checks will catch anything critical. Note "Analyze skipped (timeout)" in the handoff.

### 3.5. Long-Running Commands

Any command expected to take >60 seconds (builds, deploys, eval pipelines, serve commands) MUST run in a tmux session. Never block your bash session with long-running commands — tool timeouts will kill them.

```bash
# Run in background tmux session
tmux new-session -d -s build '<command>'
# Monitor progress
tmux capture-pane -t build -p | tail -20
# Check if still running
tmux has-session -t build 2>/dev/null && echo "running" || echo "done"
```

This applies throughout implementation — builds, deploys, `pulumi up`, long test suites, `bun run serve`, etc. Short commands (<60s) like `bun test`, `bunx tsc --noEmit`, and `bunx biome check` can run directly.

### 4. Pre-Ship Verification

All checks must pass before creating PR:

```bash
bun test          # All tests must pass
bunx tsc --noEmit # No type errors
bunx biome check  # No lint/format issues
```

**For Python code**, also run the project's Python checks:
```bash
cd meta/trajectory_labs  # or relevant Python package
uv run ruff check --fix src/ tests/
uv run ruff format src/ tests/
uv run pytest
```

If any check fails:
1. Fix the issues
2. Re-run all checks
3. Only proceed to Ship when all pass

Do NOT create a PR if any check fails — fix first.

### 4.5. Documentation

For any user-facing behavior change, update relevant documentation before creating the PR:

- **README** — if the feature changes setup, usage, or configuration
- **Usage guides** — if the feature adds new user-facing functionality
- **API docs** — if the feature changes or adds API endpoints
- **Inline help** — if the feature adds CLI commands or options

Documentation should explain **how to use** the feature, not just what changed in the code. A user reading only the docs should be able to understand and use the new functionality.

Skip this step if the change is purely internal (refactoring, bug fix with no behavior change, test-only changes).
### 5. Cross-Family Review

After all checks pass, spawn a cross-family review session before creating the PR.

1. Spawn a review session using `background_task`:
   - Category: `review-implementation`
   - Model: Specify an explicit model from a different provider (e.g., `google/gemini-3-pro` or `openai/gpt-5.2-codex`)
   - **timeout_seconds: 180** (3 minutes — auto-cancels if the subagent stalls)
   - Prompt: Include:
    - The original plan/requirements from the issue
     - A summary of what was implemented
     - The diff (`jj diff`)

2. The reviewer evaluates:
   - **Spec compliance:** Does the implementation match the plan requirements?
   - **Code quality:** Is the code clean, tested, and maintainable?
   - **Missing pieces:** Are there requirements from the plan that weren't implemented?
   - **Over-engineering:** Was anything built that wasn't requested?

3. If the reviewer finds issues:
   - Address each finding
   - Re-run Pre-Ship Verification (step 4) after fixes
   - You do NOT need to re-review — one cross-family pass is sufficient

4. Only after addressing review findings, proceed to Ship.

**If cross-family review fails or times out (>3 min):** Skip the review step and proceed directly to Ship. Note "Cross-family review skipped (subagent timeout)" in the PR body summary. The downstream human/bot review will catch anything the cross-family reviewer would have found. **Do NOT stall waiting for the subagent — your primary obligation is to ship and signal completion.**

### 6. Ship

Before creating the PR, verify branch ancestry is clean:
```bash
jj log -r 'ancestors(@, 5)'  # Should show only your issue's commits on top of main
jj diff --stat --from main    # File count should match expectations — no unrelated files
```

If unrelated commits are in the ancestry, rebase to isolate your changes before creating the PR.

**Note:** `.legion/` handoff files (written in step 7.5) are expected in the diff — they are part of the PR deliverable, not build artifacts. Do not remove them or add `.legion/` to `.gitignore`.

**CRITICAL: The PR body MUST include closing keywords for every issue addressed by the PR.**
At minimum include `Closes #$ISSUE_NUMBER` (the dispatched issue). If the plan or issue context
indicates additional issues are fixed by the same PR, add one `Closes #<number>` line for each.
Missing keywords leave orphaned issues and stall the pipeline.

Before creating the PR, build the required closing-keyword list:

```bash
# Always include the dispatched issue
REQUIRED_CLOSES="$ISSUE_NUMBER"

# Add additional issue numbers from plan/issue context when this PR closes more issues.
# Example:
# REQUIRED_CLOSES="$REQUIRED_CLOSES 101 102"

CLOSES_BLOCK=""
for n in $REQUIRED_CLOSES; do
  CLOSES_BLOCK="${CLOSES_BLOCK}Closes #${n}\n"
done
```

```bash
jj describe -m "$LEGION_ISSUE_ID: [description]"
jj git push --named "$LEGION_ISSUE_ID"=@

gh pr create --draft \
  --title "$LEGION_ISSUE_ID: [title]" \
  --body "${CLOSES_BLOCK}

## Summary
[summary]

[if notifications.ping_reporter_on_pr=true, add: @<issue-reporter>]" \
  --head "$LEGION_ISSUE_ID" \
  -R $OWNER/$REPO
```

**Verify the PR body contains all required closing keywords** after creation:
```bash
PR_BODY=$(gh pr view $LEGION_ISSUE_ID --json body --jq '.body' -R $OWNER/$REPO)
for n in $REQUIRED_CLOSES; do
  echo "$PR_BODY" | grep -q "Closes #$n" || {
    echo "Missing closing keyword: Closes #$n"
    exit 1
  }
done
```

#### 6.1. Subscribe to PR Topics (Envoy)

After PR creation, subscribe to PR-specific Envoy topics so the implementer receives real-time notifications for PR comments, reviews, and state changes:

```bash
# Extract PR number from the created PR
PR_NUMBER=$(gh pr view "$LEGION_ISSUE_ID" --json number --jq '.number' -R $OWNER/$REPO)
```

```
envoy_subscribe(["notifications.github.$OWNER.$REPO.pr.$PR_NUMBER.>"])
```

This catches: PR state changes (opened/closed/merged/draft→ready), comments, reviews, mentions, and CI events on the PR.

**If `envoy_subscribe` fails:** Log and continue — this is a speed optimization, not a requirement. The existing controller polling cycle is the authoritative fallback for PR state changes.

The issue ID in the branch/title preserves traceability for the controller.

If `notifications.slack_channel` is configured and `slack-bot` skill is available, post a best-effort implementation status update (PR URL, issue ID, current CI state). If unavailable, note this in implement handoff and continue.

### 7. Wait for CI

After pushing, wait for CI to complete:
```bash
gh pr checks "$LEGION_ISSUE_ID" --watch
```

**If CI fails:** Read the failure logs, fix the issues, push again, and re-check.
Do NOT exit with failing CI — it's your job to get CI green before the reviewer sees the PR.

**Note:** Some repositories suppress CI on draft PRs. If `gh pr checks --watch` hangs
with no checks reported, convert the PR to ready (`gh pr ready`), wait for CI, then
convert back to draft (`gh pr ready --undo`) if needed.

### 7.1. Address Automated Review Comments

After CI runs, automated code reviewers (Claude, GitHub Copilot, Codex, etc.) may post
review comments on the PR. Check for and address these before proceeding:

```bash
PR_NUMBER=$(gh pr view "$LEGION_ISSUE_ID" --json number --jq '.number' -R $OWNER/$REPO)
BOT_COMMENTS=$(gh api repos/$OWNER/$REPO/pulls/$PR_NUMBER/comments --jq '[.[] | select(.user.type == "Bot")] | length')
echo "Bot review comments: $BOT_COMMENTS"
```

If bot comments exist:

1. **Read the comments:**
   ```bash
   gh api repos/$OWNER/$REPO/pulls/$PR_NUMBER/comments \
     --jq '.[] | select(.user.type == "Bot") | {author: .user.login, file: .path, line: .line, body: .body}'
   ```
2. **Evaluate each comment** — fix legitimate issues (bugs, security, correctness). Dismiss
   style suggestions that conflict with project conventions. Bot suggestions are advisory,
   not authoritative.
3. **If fixes were made**, re-run Pre-Ship Verification (step 4), push, and wait for CI again.
4. **Reply to addressed comments** acknowledging the fix or explaining why dismissed.

If no bot comments exist, skip to step 7.2.

### 7.2. Pre-Ship Anti-Pattern Check

Before writing handoff, verify you haven't fallen into these documented anti-patterns (each was observed 3+ times across worker sessions in transcript analysis):

**DO NOT:**
1. **Use `git` commands in a jj workspace** — corrupts working copy state. Use `jj` equivalents exclusively.
2. **Use prefix/startsWith matching for issue or change IDs** — causes false positives. Use exact ID matching.
3. **Use the `write` tool on existing files** — causes "file already exists" errors. Use `edit` for existing files, `write` only for new files.
4. **Assume a feature is missing without checking main** — run `jj diff --from trunk()` or read the code on main before reimplementing something that may already exist.
5. **Delay PR creation until after exhaustive verification** — you may run out of session. Create the PR early (even as draft), verify, then mark ready.
6. **Over-scope beyond the plan** — if the planner narrowed the scope, follow it. Don't rebuild features the plan said were already done.

### 7.5. Write Handoff Data

Write handoff data for downstream phases:

First, assess which injected learnings were helpful:

> Review the learnings injected at Step 1.5. For each, assess: did this learning materially influence your work, prevent a mistake, or provide useful context for this phase? List only those canonical paths in `learningsHelpful`. If none were helpful, use an empty array. If no learnings were injected, omit both fields from handoff.

```bash
legion handoff write --phase implement --workspace . <<'HANDOFF'
{
  "filesChanged": ["src/file1.ts", "src/file2.ts"],
  "trickyParts": ["Describe any difficult implementation decisions or gotchas"],
  "deviations": ["List any deviations from the plan with rationale"],
  "openQuestions": ["Unresolved questions for downstream phases"],
  "subPlanningNeeded": false,
  "learningsInjected": ["<canonical docs/solutions/ paths injected into this phase>"],
  "learningsHelpful": ["<subset that materially helped>"]
}
HANDOFF
```

Verify the handoff was written:

```bash
if [ ! -f .legion/implement.json ]; then
  echo "FATAL: Handoff write failed — .legion/implement.json not created"
  echo "STOP: Do NOT signal worker-done. Diagnose: Is 'legion' CLI in PATH? Is --workspace correct?"
  echo "If write cannot be fixed, note the failure in your exit comment."
fi
```
Key fields:
- `filesChanged`: List of files created or modified during implementation
- `trickyParts`: Notes about what was difficult or required special handling
- `deviations`: List of deviations from the plan with rationale for each
- `openQuestions`: Unresolved questions or concerns for downstream phases
- `subPlanningNeeded`: Boolean — set to `true` if discovered complexity was greater than planned
- `learningsInjected`: Canonical `docs/solutions/` file paths of learnings presented to the worker at the start of the phase (omit if none were injected)
- `learningsHelpful`: Subset of `learningsInjected` that materially helped this phase's output (empty array if none were helpful; omit if no learnings were injected)

### 8. Exit

**CRITICAL: The `worker-done` label is how the controller knows you finished.** If you skip this,
the issue silently stalls and no one advances it. This is the MOST IMPORTANT step.

First, verify the PR is still open (force-pushes can accidentally close PRs):

**GitHub:**
```bash
PR_STATE=$(gh pr view $ISSUE_NUMBER --json state --jq '.state' -R $OWNER/$REPO 2>/dev/null)
if [ "$PR_STATE" = "CLOSED" ]; then
  gh pr reopen $ISSUE_NUMBER -R $OWNER/$REPO
  echo "PR was accidentally closed by force-push, reopened"
fi
```

Then add the label and verify it was applied:

**GitHub:**
```bash
gh issue edit $ISSUE_NUMBER --add-label "worker-done" --remove-label "worker-active" -R $OWNER/$REPO
# Verify the label was actually applied
LABELS=$(gh issue view $ISSUE_NUMBER --json labels --jq '[.labels[].name] | join(",")' -R $OWNER/$REPO)
if ! echo "$LABELS" | grep -q "worker-done"; then
  echo "WARNING: worker-done label not applied, retrying"
  gh issue edit $ISSUE_NUMBER --add-label "worker-done" -R $OWNER/$REPO
fi
```

**Linear:**
```
issue = linear_linear(action="get", id=$LEGION_ISSUE_ID)
current_labels = [l.name for l in issue.labels if l.name != "worker-active"]
linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current_labels, "worker-done"])
```

Then notify the controller via Envoy (best-effort, exactly one notification):
```
envoy_publish(topic="notifications.role.legion-controller", message="Worker done: $ISSUE_NUMBER implement completed. PR ready for testing.")
```
If `envoy_publish` fails, continue — the label is the source of truth.

---

## Mode 2: Address Comments

### 1. Process Review Feedback

First, check if review comments actually exist on the PR:
```bash
# Review comments (on the diff)
gh api repos/$OWNER/$REPO/pulls/$PR_NUMBER/comments
# Issue comments (on the conversation tab)
gh api repos/$OWNER/$REPO/issues/$PR_NUMBER/comments
```

If the PR was converted to draft but has no review comments in either location, there's nothing to address. Rebase onto latest main, verify tests pass, push, and exit.

Otherwise, invoke `/superpowers/receiving-code-review` to evaluate and prioritize feedback.

**You MUST address ALL review comments, including bot comments.** The API calls above
return comments from all authors including bots (Claude, GitHub Copilot, Codex). Do NOT
dismiss or ignore bot comments just because they're automated. Bot reviewers catch real
issues — type errors, missing imports, security concerns, logic bugs. For each comment:
- Fix the issue if it's valid
- Explain why it doesn't apply if you disagree (with evidence, not assertion)
- Never silently skip a comment

### 1.5. Read Prior Handoffs (Advisory)

Read any prior handoffs from architect or plan phases (non-blocking):

```bash
legion handoff read --workspace . 2>/dev/null || echo '{}'
```

If architect or plan handoffs are present, note any concerns, routing hints, or learnings used. This is informational only — proceed regardless of whether these files exist.

### 1.6. Subscribe to Existing PR Events (Envoy)

When resuming with an existing PR, subscribe to PR-specific Envoy topics so you receive
real-time notifications for comments, reviews, and state changes:

```bash
PR_NUMBER=$(gh pr view "$LEGION_ISSUE_ID" --json number --jq '.number' -R $OWNER/$REPO 2>/dev/null)
```

If a PR exists:

```
envoy_subscribe(["notifications.github.$OWNER.$REPO.pr.$PR_NUMBER.>"])
```

**If no PR exists or `envoy_subscribe` fails:** Log and continue — this is a speed
optimization, not a requirement. The controller polling cycle is the authoritative fallback.

Key behaviors:
- Verify suggestions against codebase before implementing
- Push back with technical reasoning if wrong
- Clarify unclear items before implementing

### 2. Fix Issues

Use TDD and subagent-driven development:
- `/superpowers/test-driven-development`
- `/superpowers/subagent-driven-development`

### 3. Verify

Before pushing, run all checks:

```bash
bun test
bunx tsc --noEmit
bunx biome check
```

Fix any failures before pushing.

### 4. Push

```bash
jj git push
```

### 4.5. Wait for CI

```bash
gh pr checks "$LEGION_ISSUE_ID" --watch
```

**If CI fails:** Read the failure logs, fix the issues, push again, and re-check.
Do NOT reply to comments or exit with failing CI.

### 4.8. Pre-Ship Anti-Pattern Check

Before replying to comments or writing handoff, verify you haven't fallen into these documented anti-patterns (each was observed 3+ times across worker sessions in transcript analysis):

**DO NOT:**
1. **Use `git` commands in a jj workspace** — corrupts working copy state. Use `jj` equivalents exclusively.
2. **Use prefix/startsWith matching for issue or change IDs** — causes false positives. Use exact ID matching.
3. **Use the `write` tool on existing files** — causes "file already exists" errors. Use `edit` for existing files, `write` only for new files.
4. **Assume a feature is missing without checking main** — run `jj diff --from trunk()` or read the code on main before reimplementing something that may already exist.
5. **Delay PR creation until after exhaustive verification** — you may run out of session. Create the PR early (even as draft), verify, then mark ready.
6. **Over-scope beyond the plan** — if the planner narrowed the scope, follow it. Don't rebuild features the plan said were already done.

### 5. Reply to Comments

Reply in PR comment threads acknowledging fixes. Reference specific changes made.

### 5.5. Write Handoff Data

Write handoff data for downstream phases:

First, assess which injected learnings were helpful:

> Review the learnings injected at Step 1.5. For each, assess: did this learning materially influence your work, prevent a mistake, or provide useful context for this phase? List only those canonical paths in `learningsHelpful`. If none were helpful, use an empty array. If no learnings were injected, omit both fields from handoff.

```bash
legion handoff write --phase implement --workspace . <<'HANDOFF'
{
  "filesChanged": ["src/file1.ts", "src/file2.ts"],
  "trickyParts": ["Describe any difficult implementation decisions or gotchas"],
  "deviations": ["List any deviations from the plan with rationale"],
  "openQuestions": ["Unresolved questions for downstream phases"],
  "subPlanningNeeded": false,
  "learningsInjected": ["<canonical docs/solutions/ paths injected into this phase>"],
  "learningsHelpful": ["<subset that materially helped>"]
}
HANDOFF
```

Verify the handoff was written:

```bash
if [ ! -f .legion/implement.json ]; then
  echo "FATAL: Handoff write failed — .legion/implement.json not created"
  echo "STOP: Do NOT signal worker-done. Diagnose: Is 'legion' CLI in PATH? Is --workspace correct?"
  echo "If write cannot be fixed, note the failure in your exit comment."
fi
```

Key fields:
- `filesChanged`: List of files created or modified during implementation
- `trickyParts`: Notes about what was difficult or required special handling
- `deviations`: List of deviations from the plan with rationale for each
- `openQuestions`: Unresolved questions or concerns for downstream phases
- `subPlanningNeeded`: Boolean — set to `true` if discovered complexity was greater than planned
- `learningsInjected`: Canonical `docs/solutions/` file paths of learnings presented to the worker at the start of the phase (omit if none were injected)
- `learningsHelpful`: Subset of `learningsInjected` that materially helped this phase's output (empty array if none were helpful; omit if no learnings were injected)

### 6. Exit

**CRITICAL: The `worker-done` label is how the controller knows you finished.** If you skip this,
the issue silently stalls and no one advances it. This is the MOST IMPORTANT step.

First, verify the PR is still open (force-pushes can accidentally close PRs):

**GitHub:**
```bash
PR_STATE=$(gh pr view $ISSUE_NUMBER --json state --jq '.state' -R $OWNER/$REPO 2>/dev/null)
if [ "$PR_STATE" = "CLOSED" ]; then
  gh pr reopen $ISSUE_NUMBER -R $OWNER/$REPO
  echo "PR was accidentally closed by force-push, reopened"
fi
```

Then add the label and verify it was applied:

**GitHub:**
```bash
gh issue edit $ISSUE_NUMBER --add-label "worker-done" --remove-label "worker-active" -R $OWNER/$REPO
# Verify the label was actually applied
LABELS=$(gh issue view $ISSUE_NUMBER --json labels --jq '[.labels[].name] | join(",")' -R $OWNER/$REPO)
if ! echo "$LABELS" | grep -q "worker-done"; then
  echo "WARNING: worker-done label not applied, retrying"
  gh issue edit $ISSUE_NUMBER --add-label "worker-done" -R $OWNER/$REPO
fi
```

**Linear:**
```
issue = linear_linear(action="get", id=$LEGION_ISSUE_ID)
current_labels = [l.name for l in issue.labels if l.name != "worker-active"]
linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current_labels, "worker-done"])
```

Then notify the controller via Envoy (best-effort, exactly one notification):
```
envoy_publish(topic="notifications.role.legion-controller", message="Worker done: $ISSUE_NUMBER implement completed after changes. PR ready for re-testing.")
```
If `envoy_publish` fails, continue — the label is the source of truth.
