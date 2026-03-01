---
name: legion-retro
description: Capture learnings from completed work via dual-perspective retrospective. Invoked by resuming an implement worker session — the implementer has full context, and a fresh subagent provides an outside view.
---

# Legion Retro

Capture learnings from completed work via parallel compounding.

## When This Runs

The controller resumes the **implement worker's existing session** after PR approval, so you (the implementer) have full context of what was built and why. This is intentional — your perspective as the person who did the work is valuable.

A fresh subagent provides the outside perspective (see step 2).

## Important

- **NO rebasing** - unlike other workflows, do not rebase before starting
- **Two perspectives** - fresh subagent (context-free) + you (full context)
- **Skip if nothing learned** - small mechanical changes (find-and-replace, formatting fixes, dependency bumps) with no real learnings don't need docs. Post a brief "no significant learnings" comment on the issue and signal completion.

## Workflow

### 1. Assess Whether a Retro Doc is Warranted

Not every PR needs documentation. Ask:
- Did anything surprising happen during implementation?
- Were there decisions that aren't obvious from the code?
- Did patterns emerge that would help future work?
- Were there gotchas that someone else would hit?

If the answer to all of these is no, skip to step 6 (post a brief summary comment) and step 7 (signal completion).

**If skipping (no significant learnings), use this brief comment template for step 6:**
```bash
gh issue comment $ISSUE_NUMBER --body "## Retro Complete

No significant learnings — mechanical change (find-and-replace / formatting / dependency bump)." -R $OWNER/$REPO
```
For Linear, use `linear_linear(action="comment", ...)` with the same body.

### 2. Get PR URL and Launch Background Subagent

```bash
PR_URL=$(gh pr view "$LEGION_ISSUE_ID" --json url --jq '.url')
```

Use `background_task` tool to spawn a fresh subagent:

- **Category:** `unspecified-low`
- **Description:** "Retro analysis for $LEGION_ISSUE_ID"
- **Prompt:**

> You are analyzing a completed PR to capture learnings.
>
> Issue: $LEGION_ISSUE_ID
> PR: $PR_URL
>
> 1. Fetch the PR diff and description via gh pr view and gh pr diff
> 2. Analyze: what patterns emerged, what was hard, what would help future implementations
> 3. Return your analysis as structured output (don't write files)
>
> Focus on patterns that would help future implementations.

### 3. Do Your Own Analysis (In Parallel)

While the subagent runs, capture your own perspective:
- What was hard
- What you would do differently
- What patterns emerged
- Decisions that weren't obvious from the code

### 4. Integrate Both Perspectives

When the subagent completes, review its suggestions alongside your own analysis.

**You are the integrator.** The subagent provides an outside view, but you have the
implementation context. Push back on suggestions that miss context, and incorporate
the ones that add genuine value.

Write the integrated learnings to `docs/solutions/`. Optimize for **discoverability**:
- Organize by topic, not by PR — a future agent should find these via YAML frontmatter
- If there are learnings about different parts of the system (e.g., one about Docker and
  one about Python testing), write separate docs so each can be found independently
- If all learnings are about one topic, write one doc
- Don't write multiple docs just because there are multiple bullet points

### 4.5. Update Learnings Index

After writing learning files to `docs/solutions/`, update the learnings index so future plans can proactively find them:

1. Get the PR's changed file paths:

   **GitHub:**
   ```bash
   gh pr view "$LEGION_ISSUE_ID" --json files --jq '[.files[].path]' -R $OWNER/$REPO
   ```

   **Linear:**
   ```bash
   # Use the PR URL from step 2
   gh pr view "$PR_URL" --json files --jq '[.files[].path]'
   ```

2. Read the current index:
   ```bash
   cat docs/solutions/index.json
   ```

3. For each learning file you just wrote:
   - Extract **directory-level path prefixes** from the PR's changed files. Group to the 3rd or 4th path segment (e.g., `packages/daemon/src/state/decision.ts` → `packages/daemon/src/state`, `.opencode/skills/legion-worker/workflows/plan.md` → `.opencode/skills/legion-worker`).
   - For each unique prefix, add the learning file path (relative to `docs/solutions/`) to that prefix's array in `index.json`. Create the key if it doesn't exist.
   - Deduplicate: don't add a learning that's already listed under a key.

4. Write the updated `docs/solutions/index.json`. Preserve all existing entries — only add new mappings.

**Example:** If the PR changed `packages/daemon/src/state/decision.ts` and `.opencode/skills/legion-worker/workflows/plan.md`, and you wrote `docs/solutions/daemon/my-new-learning.md`:

Add `"daemon/my-new-learning.md"` to these index keys (creating them if missing):
- `"packages/daemon/src/state"`
- `".opencode/skills/legion-worker"`

**If `docs/solutions/index.json` doesn't exist or is invalid JSON:** Skip this step and note it in the issue comment. The index is advisory — its absence should never block the retro workflow.

### 5. Commit and Push Learnings

Push to the **existing PR branch** — do NOT create a new branch or bookmark.
The implementer already created the branch when opening the PR.

**Critical: Create a new change first** — do NOT describe the current change, which
contains the implementer's code. The retro docs go in a separate commit.

```bash
jj new  # Create fresh change for retro docs (preserves implementer's commit)
# ... write docs/solutions/ files ...
jj describe -m "$LEGION_ISSUE_ID: retro learnings"
```

Then ensure the bookmark includes this new change and push:

```bash
# Find the bookmark name (usually matches the issue ID or branch name)
jj bookmark list
# Move bookmark forward to include the retro change
jj bookmark set "$BOOKMARK_NAME" -r @
jj git push
```

**If jj says there's no tracked branch:** The implementer should have created this branch.
Verify whether the bookmark exists:
```bash
jj bookmark list  # Should see a bookmark matching the issue ID or branch name
```
- **If the bookmark exists but isn't at @:** move it forward and push:
  ```bash
  jj bookmark set "$BOOKMARK_NAME" -r @
  jj git push
  ```
- **If the bookmark does NOT exist:** something went wrong — the implementer should have
  created it. Do not create a new branch. Instead, post a comment on the issue noting the
  missing branch and add `user-input-needed`, then exit.

### 6. Post Summary to Issue

Post a brief summary so learnings are discoverable without checking the repo:

**GitHub:**

```bash
gh issue comment $ISSUE_NUMBER --body "## Retro Complete

**Learnings documented in:**
- [list docs/solutions/ files written]

**Key takeaways:**
- [1-3 bullet summary of the most important learnings]" -R $OWNER/$REPO
```

**Linear:**

```
linear_linear(action="comment", id=$LEGION_ISSUE_ID, body="## Retro Complete

**Learnings documented in:**
- [list docs/solutions/ files written]

**Key takeaways:**
- [1-3 bullet summary of the most important learnings]")
```

### 7. Signal Completion

Add `worker-done` label to the issue, then exit:

- **GitHub:** `gh issue edit $ISSUE_NUMBER --add-label "worker-done" -R $OWNER/$REPO`
- **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current + "worker-done"])`

Then remove `worker-active`:
- **GitHub:** `gh issue edit $ISSUE_NUMBER --remove-label "worker-active" -R $OWNER/$REPO`
- **Linear:** `linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current labels without "worker-active"])`
