# Proactive Memory Injection Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build a file-path-to-learnings index (`docs/solutions/index.json`) that enables the plan workflow to proactively inject relevant learnings into the implementer's context, shifting from reactive search to preloaded knowledge.

**Architecture:** Inverted JSON index mapping source path prefixes → learning file paths (relative to `docs/solutions/`). The retro workflow updates the index after writing new learnings (using PR diff paths). The plan workflow reads the index before invoking `/workflows:plan`, matching module/area references from the issue text against index keys via substring matching, and injects up to 3 relevant learning excerpts into the planner context.

**Tech Stack:** JSON (index file), Markdown (skill files), Bash (`gh` CLI for PR file paths, `jq` for validation)

**Design doc:** Architect assessment in [#32 comment](https://github.com/sjawhar/legion/issues/32#issuecomment-3979469548)

---

### Task 1: Create `docs/solutions/index.json` with bootstrapped content — Independent

**Files:**
- Create: `docs/solutions/index.json`

**Step 1: Create the index file**

Create `docs/solutions/index.json` with the inverted index. Keys are source path prefixes (matched via substring against issue/module keywords at plan time). Values are arrays of learning file paths relative to `docs/solutions/`.

```json
{
  "version": 1,
  "index": {
    "packages/daemon/src/state": [
      "daemon/controller-lifecycle-separation.md",
      "daemon/controller-observability.md",
      "architecture-patterns/shared-state-file-ownership.md",
      "integration-issues/github-graphql-pr-draft-status.md",
      "integration-issues/external-repo-pr-auto-transition-gap.md",
      "integration-issues/linear-mcp-label-resolution.md"
    ],
    "packages/daemon/src/daemon/serve-manager": [
      "daemon/opencode-serve-lifecycle.md",
      "daemon/shared-serve-refactor.md",
      "daemon/shared-serve-retro.md",
      "daemon/graceful-worker-shutdown.md"
    ],
    "packages/daemon/src/daemon/state-file": [
      "architecture-patterns/shared-state-file-ownership.md",
      "task-index-patterns.md"
    ],
    "packages/daemon/src/daemon": [
      "daemon/worker-directory-fix.md",
      "daemon/worker-directory-resolution.md",
      "daemon/workspace-validation-retro.md",
      "daemon/controller-observability.md",
      "delegation/hardening-patterns.md",
      "integration-patterns/controller-worker-protocol.md",
      "architecture-patterns/task-index-retro.md",
      "task-index-patterns.md"
    ],
    "packages/daemon/src/cli": [
      "daemon/controller-lifecycle-separation.md"
    ],
    "packages/daemon": [
      "architecture-patterns/plugin-migration-assessment.md",
      "architecture-patterns/sync-to-async-modular-refactoring.md",
      "integration-patterns/tmux-askuserquestion-navigation.md"
    ],
    ".opencode/skills/legion-worker": [
      "skill-patterns/worker-skill-failure-modes.md",
      "daemon/deferred-review-comments-pattern.md",
      "integration-patterns/controller-worker-protocol.md"
    ],
    ".opencode/skills/legion-controller": [
      "integration-patterns/controller-worker-protocol.md",
      "daemon/controller-lifecycle-separation.md",
      "daemon/controller-observability.md"
    ],
    ".opencode/skills/linear": [
      "integration-issues/linear-mcp-label-resolution.md"
    ],
    ".opencode/skills": [
      "skill-patterns/parallel-subagent-background-execution.md",
      "delegation/delegation-hardening-retro.md",
      "delegation/hardening-patterns.md"
    ],
    "tests": [
      "best-practices/prefer-autospec-over-asyncmock-pytest-20260202.md"
    ]
  }
}
```

**Bootstrap mapping rationale:**
- Each learning file is mapped to the code areas it documents (derived from frontmatter `module`/`component`/`tags` fields where available, directory prefix as fallback)
- A learning can appear under multiple keys (e.g., `controller-lifecycle-separation.md` appears under both `packages/daemon/src/state` and `.opencode/skills/legion-controller`)
- Keys are directory-level prefixes, not individual files — this enables substring matching against issue text keywords like "daemon", "state", "controller", "worker", "skills"
- All 25 existing learning files are indexed across 11 prefix keys
- Stale entries from file renames are harmless — the plan workflow gracefully handles missing files

**Step 2: Validate the index**

Run: `jq -e '.version == 1 and (.index | type) == "object"' docs/solutions/index.json`
Expected: `true` (exit code 0)

Run: `jq -r '.index | values | flatten | unique[]' docs/solutions/index.json | while read p; do test -f "docs/solutions/$p" || echo "MISSING: $p"; done`
Expected: No output (all paths exist)

Run: `jq -r '.index | values | flatten | unique | length' docs/solutions/index.json`
Expected: `25` (all learning files indexed at least once)

**Step 3: Describe and advance**

```bash
jj describe -m "feat(#32): create docs/solutions/index.json with bootstrapped learnings"
jj new
```

---

### Task 2: Modify retro SKILL.md — add index update step — Independent

**Files:**
- Modify: `.opencode/skills/legion-retro/SKILL.md` (insert after step 4, before step 5)

**Step 1: Locate the insertion point**

The new step goes between the existing step 4 "Integrate Both Perspectives" and step 5 "Commit and Push Learnings".

Specifically, insert **after** this block (the last paragraph of step 4):
```
- Don't write multiple docs just because there are multiple bullet points
```

And **before** this line:
```
### 5. Commit and Push Learnings
```

**Step 2: Insert the new step 4.5**

Insert the following markdown:

```markdown

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

```

**Step 3: Verify the modification**

Run: `grep -q "Update Learnings Index" .opencode/skills/legion-retro/SKILL.md`
Expected: exit code 0

Run: `grep -q "docs/solutions/index.json" .opencode/skills/legion-retro/SKILL.md`
Expected: exit code 0

**Step 4: Describe and advance**

```bash
jj describe -m "feat(#32): add index update step to retro workflow"
jj new
```

---

### Task 3: Modify plan.md — add learnings injection step — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/plan.md` (insert between step 1.5 and step 2)

**Step 1: Locate the insertion point**

The new step goes between the existing step 1.5 "Pre-Planning Analysis (Metis)" and step 2 "Invoke /workflows:plan (Autonomous)".

Specifically, insert **after** this block (the last code block of step 1.5):
```
Create the implementation plan accounting for these findings.
```
````

And **before** this line:
```
### 2. Invoke /workflows:plan (Autonomous)
```

**Step 2: Insert the new step 1.7**

Insert the following markdown:

```markdown

### 1.7. Inject Relevant Learnings

Before invoking `/workflows:plan`, check the learnings index for applicable prior knowledge:

1. **Read the index:**
   ```bash
   cat docs/solutions/index.json
   ```
   If the file doesn't exist or is invalid JSON, skip this step entirely — proceed to step 2.

2. **Extract module/area keywords** from the issue title, description, and Metis analysis output. Look for references to:
   - Source path segments (e.g., `packages/daemon/src/state/`, `serve-manager`)
   - Module names (e.g., "daemon", "controller", "worker", "state")
   - Component names (e.g., "serve-manager", "decision", "fetch")
   - Feature areas (e.g., "skills", "linear", "github", "review", "retro")
   - Integration concerns (e.g., "PR", "labels", "MCP")

3. **Match keywords against index keys.** For each key in `.index`, check if any extracted keyword appears as a substring of the key (case-insensitive). Collect all matched learning file paths.

4. **Deduplicate and rank** matched learnings:
   - Remove duplicates (same file matched via multiple keys)
   - Sort by number of distinct key matches (most matches first = most relevant)
   - **Cap at 3 learnings maximum**

5. **Read each matched learning file** (from `docs/solutions/<path>`). Extract the first meaningful paragraph after the title — skip YAML frontmatter (`---` blocks), skip headings, take the first paragraph of prose (typically the Problem or Overview section). Truncate excerpt to **300 characters**.

6. **Add to `/workflows:plan` context** in step 2. Append this section to the autonomous context template, between the Metis pre-analysis and the feature description:

   ```
   Relevant learnings from prior work (preloaded from docs/solutions/index.json):

   1. [docs/solutions/<path>]: <300-char excerpt>
   2. [docs/solutions/<path>]: <300-char excerpt>
   3. [docs/solutions/<path>]: <300-char excerpt>

   Review these learnings for patterns and pitfalls relevant to this implementation.
   ```

**If no matches found:** Skip — do not add an empty "Relevant learnings" section.

**If a matched file doesn't exist on disk:** Skip that entry silently (stale index entry from a file rename). Do not error.

```

**Step 3: Verify the modification**

Run: `grep -q "Inject Relevant Learnings" .opencode/skills/legion-worker/workflows/plan.md`
Expected: exit code 0

Run: `grep -q "Cap at 3 learnings maximum" .opencode/skills/legion-worker/workflows/plan.md`
Expected: exit code 0

**Step 4: Describe and advance**

```bash
jj describe -m "feat(#32): add learnings injection step to plan workflow"
jj new
```

---

### Task 4: Mirror skill changes to `.claude/skills/` — Depends on: Task 2, Task 3

**Files:**
- Mirror: `.claude/skills/legion-retro/SKILL.md` ← `.opencode/skills/legion-retro/SKILL.md`
- Mirror: `.claude/skills/legion-worker/workflows/plan.md` ← `.opencode/skills/legion-worker/workflows/plan.md`

**Step 1: Copy modified files to mirror location**

```bash
cp .opencode/skills/legion-retro/SKILL.md .claude/skills/legion-retro/SKILL.md
cp .opencode/skills/legion-worker/workflows/plan.md .claude/skills/legion-worker/workflows/plan.md
```

**Step 2: Verify mirrors are byte-for-byte identical**

Run: `diff .opencode/skills/legion-retro/SKILL.md .claude/skills/legion-retro/SKILL.md`
Expected: No output (exit code 0)

Run: `diff .opencode/skills/legion-worker/workflows/plan.md .claude/skills/legion-worker/workflows/plan.md`
Expected: No output (exit code 0)

**Step 3: Describe and advance**

```bash
jj describe -m "feat(#32): mirror skill changes to .claude/skills/"
jj new
```

---

### Task 5: Final verification — Depends on: Task 1, Task 2, Task 3, Task 4

**Step 1: Validate index schema**

Run: `jq -e '.version == 1 and (.index | type) == "object"' docs/solutions/index.json`
Expected: `true` (exit code 0)

**Step 2: Validate all indexed paths exist**

Run: `jq -r '.index | values | flatten | unique[]' docs/solutions/index.json | while read p; do test -f "docs/solutions/$p" || echo "MISSING: $p"; done`
Expected: No output (all paths exist)

**Step 3: Validate no duplicate entries per key**

Run: `jq -r '.index | to_entries[] | select((.value | unique | length) != (.value | length)) | .key' docs/solutions/index.json`
Expected: No output (no duplicates within any key)

**Step 4: Verify all learning files are indexed**

Run: `jq -r '.index | values | flatten | unique | length' docs/solutions/index.json`
Expected: `25`

Run: `find docs/solutions -name "*.md" -not -name "index.md" | wc -l`
Expected: `25`

**Step 5: Verify skill modifications**

Run: `grep -q "Update Learnings Index" .opencode/skills/legion-retro/SKILL.md && echo "retro: OK"`
Expected: `retro: OK`

Run: `grep -q "Inject Relevant Learnings" .opencode/skills/legion-worker/workflows/plan.md && echo "plan: OK"`
Expected: `plan: OK`

**Step 6: Verify mirrors**

Run: `diff .opencode/skills/legion-retro/SKILL.md .claude/skills/legion-retro/SKILL.md && echo "retro mirror: OK"`
Expected: `retro mirror: OK`

Run: `diff .opencode/skills/legion-worker/workflows/plan.md .claude/skills/legion-worker/workflows/plan.md && echo "plan mirror: OK"`
Expected: `plan mirror: OK`

---

## Testing Plan

### Setup
- No special setup needed — all changes are static JSON and Markdown files
- Ensure `jq` is available: `which jq`

### Health Check
- `jq -e . docs/solutions/index.json` returns valid JSON (exit code 0)
- Retry: N/A (static file check)

### Verification Steps

1. **Index exists with correct schema**
   - Action: `jq -e '.version == 1 and (.index | type) == "object" and (.index | keys | length) > 0' docs/solutions/index.json`
   - Expected: `true` (exit code 0)
   - Tool: jq

2. **All 25 learning files are indexed**
   - Action: `jq -r '.index | values | flatten | unique | length' docs/solutions/index.json`
   - Expected: Output is `25`
   - Tool: jq

3. **All indexed paths point to existing files**
   - Action: `jq -r '.index | values | flatten | unique[]' docs/solutions/index.json | while read p; do test -f "docs/solutions/$p" || echo "MISSING: $p"; done`
   - Expected: No output (exit code 0)
   - Tool: jq + bash

4. **Retro workflow contains index update instructions**
   - Action: `grep -c "Update Learnings Index" .opencode/skills/legion-retro/SKILL.md`
   - Expected: `1`
   - Tool: grep

5. **Plan workflow contains learnings injection instructions**
   - Action: `grep -c "Inject Relevant Learnings" .opencode/skills/legion-worker/workflows/plan.md`
   - Expected: `1`
   - Tool: grep

6. **Mirrors are byte-for-byte identical**
   - Action: `diff .opencode/skills/legion-retro/SKILL.md .claude/skills/legion-retro/SKILL.md; diff .opencode/skills/legion-worker/workflows/plan.md .claude/skills/legion-worker/workflows/plan.md`
   - Expected: No output for both (exit code 0)
   - Tool: diff

### Tools Needed
- `jq` for JSON validation and querying
- `diff` for mirror verification
- `grep` for content verification
- `find` for file counting
