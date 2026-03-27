# Controller SKILL.md Audit Implementation Plan (#125)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add 6 policy subsections (User Interaction Priority, Autonomy vs Approval, Polling Architecture, Pre-Merge Gate, Pipeline Integrity, Role Boundary) to the controller SKILL.md, plus clean up existing text that would contradict the new content. Mirror across `.opencode/` and `.claude/` directories.

**Architecture:** All changes are Markdown-only edits to `.opencode/skills/legion-controller/SKILL.md`. Apply edits top-to-bottom in one pass, then copy to `.claude/skills/legion-controller/SKILL.md` and verify parity with `diff`.

**Tech Stack:** Markdown, grep/diff (verification), jj (version control)

**Known concern:** The Pre-Merge Gate includes `test-passed` label as a condition (per issue spec). However, the current controller flow removes `test-passed` when transitioning to Needs Review (line 146). By merge time, this label may not be present. This is a label-handling issue that exists outside the scope of this Markdown-only change — the implementer should include the condition as specified and flag this for the reviewer.

---

## File Structure

- **Modify:** `.opencode/skills/legion-controller/SKILL.md` (primary — all edits here first)
- **Mirror:** `.claude/skills/legion-controller/SKILL.md` (exact copy after edits)

No new files. No code changes.

---

## Task 1: Apply All Edits to Controller SKILL.md — Independent

**File:** `.opencode/skills/legion-controller/SKILL.md`

Apply all edits below in file order (top to bottom). The implementer should read the file, then use the edit tool with multiple operations referencing original line positions (the tool applies them bottom-up automatically).

### Edit 1a: Update Core Principle priority list (~line 24)

Find lines 24-27:
```markdown
**Keep work moving forward.** Priority order:
1. Unblock in-progress work (relay user feedback)
2. Advance completed work (process worker-done)
3. Start new work (triage, pull from Icebox)
```

Replace with:
```markdown
**Keep work moving forward.** Priority order:
0. Respond to user messages (always first)
1. Unblock in-progress work (relay user feedback)
2. Advance completed work (process worker-done)
3. Start new work (triage, pull from Icebox)
```

### Edit 1b: Insert User Interaction Priority subsection (after priority list, before `## Algorithm`)

Insert AFTER the `3. Start new work` line, BEFORE the blank line preceding `## Algorithm` (line 29):

```markdown

### User Interaction Priority

At the start of each loop iteration, check if the user has sent a direct question or new instructions.

- If yes: **STOP** the current iteration, answer the user FIRST, then resume
- Never continue looping while an unanswered user question is pending
- If mid-dispatch, finish the dispatch, then respond immediately

This rule is about answering user questions directed AT the controller. It is distinct from Step 2 (Relay User Feedback), which relays user comments TO workers via issue labels.
```

### Edit 1c: Insert Autonomy vs Approval subsection (after User Interaction Priority, before `## Algorithm`)

Insert AFTER the User Interaction Priority subsection, BEFORE `## Algorithm`:

```markdown

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
```

### Edit 2a: Update Algorithm diagram (~lines 43-44)

In the ```dot block, find:
```
    sleep [label="9. Sleep 30s"];
    start -> fetch -> feedback -> worker_done -> triage -> icebox -> cleanup -> heartbeat -> todo -> sleep -> fetch;
```

Replace with:
```
    wait [label="9. Wait for Poller"];
    start -> fetch -> feedback -> worker_done -> triage -> icebox -> cleanup -> heartbeat -> todo -> wait -> fetch;
```

### Edit 2b: Insert Polling Architecture subsection (after `**Do not exit.**` line 48, before `### 1. Fetch Issues` line 50)

Insert AFTER `**Do not exit.** Loop continuously.`, BEFORE `### 1. Fetch Issues`:

```markdown

### Polling Architecture

The 9-step loop describes WHAT the controller does. Execution uses background polling via `task(run_in_background=true)`:

1. **Main thread** — handles user messages, makes routing decisions, acts on poller reports. MUST never call `sleep` or block.
2. **Background poller** — a persistent background task that fetches issues, posts to `/state/collect`, and reports state changes every ~60 seconds.
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
```

### Edit 2c: Rename Step 2 heading (line 72)

The existing heading is:
```markdown
### 2. Relay User Feedback (Highest Priority)
```

Replace with (remove parenthetical — user messages are now priority #0):
```markdown
### 2. Relay User Feedback
```

### Edit 3a: Insert Pipeline Integrity subsection (after Review → Re-implementation section, before Quality Gate)

Find the end of `### Review → Re-implementation → Testing Loop` — the last line is:
```
**Critical:** The controller MUST transition to In Progress before resuming the implementer. If the issue stays in Needs Review and the implementer adds `worker-done`, the state machine will see `prIsDraft + worker-done` and suggest `resume_implementer_for_changes` again (infinite loop).
```

Insert AFTER that line, BEFORE `### Quality Gate (Controller Policy)` (line 164):

```markdown

### Pipeline Integrity

Pipeline phases MUST run in order: architect → plan → implement → test → review → retro → merge.

**MUST NOT skip:**
- **Testing** — the tester ALWAYS runs after implementation, including after review-requested changes
- **Review** — the reviewer ALWAYS runs after testing passes

**MAY skip (with conditions):**
- **Architect** — ONLY when ALL conditions are met: `bug` label present, description contains clear reproduction steps, AND the change is scoped to a single component. This exception is documented in the Route Triage table — do not contradict it.
- **Retro** — ONLY when ALL skip conditions are met per the routing hints (see Retro section)

Simple issues go through every phase — they just go through faster. Complexity is not a reason to skip phases.
```

### Edit 3b: Insert Role Boundary subsection (after Pipeline Integrity, before Quality Gate)

Insert AFTER Pipeline Integrity, BEFORE `### Quality Gate (Controller Policy)`:

```markdown

### Role Boundary

The controller MUST NOT:
- Run `jj` commands (version control is worker work)
- Edit files or write code
- Run `gh pr merge` directly (dispatch a merge worker)
- Run tests (dispatch a tester)

The controller dispatches workers. Workers do the work. If you are about to touch code, branches, or PRs directly — stop and dispatch the appropriate worker instead.
```

### Edit 4a: Insert Pre-Merge Gate subsection (after Quality Gate, before Post-Merge Monitoring)

Find the end of `### Quality Gate (Controller Policy)` — the last line before `### Post-Merge Monitoring` (line 189) is:
```
CI status and include it in the review.
```

Insert AFTER that line, BEFORE `### Post-Merge Monitoring`:

```markdown

### Pre-Merge Gate

Before requesting merge approval, verify ALL conditions:

| # | Condition | Verification |
|---|-----------|-------------|
| 1 | CI checks green (not pending, not failed) | `gh pr checks "$LEGION_ISSUE_ID"` — all checks must show ✓ |
| 2 | PR NOT in draft | `gh pr view "$LEGION_ISSUE_ID" --json isDraft -q .isDraft` returns `false` |
| 3 | `test-passed` label present | `gh issue view $ISSUE_NUMBER --json labels -q '.labels[].name' -R $OWNER/$REPO \| grep test-passed` |
| 4 | Issue has been through retro (or skipped via routing hints) | Check retro handoff: `legion handoff read --phase retro --workspace "$WORKSPACE_PATH" 2>/dev/null` or verify issue transitioned through Retro status |
| 5 | No `user-input-needed` label present | `gh issue view $ISSUE_NUMBER --json labels -q '.labels[].name' -R $OWNER/$REPO \| grep -v user-input-needed` — must NOT match |

If ANY condition fails, do NOT request merge approval. Fix the failing condition first.

**When all conditions pass:** Post a readiness comment to the issue and add the `needs-approval` label. Wait for user approval before dispatching the merger.

**Human override:** If the user explicitly authorizes a merge despite unmet conditions (e.g., "merge it, skip retro"), proceed. Log which conditions were overridden in the merge dispatch prompt:

```bash
legion dispatch "$ISSUE_IDENTIFIER" merge \
  --repo "$OWNER/$REPO" \
  --prompt "Invoke the /legion-worker skill for merge mode. Override: user approved with unmet conditions: [list waived conditions]. ($BACKEND_SUFFIX)"
```
```

### Edit 5a: Rewrite Step 9 (~lines 261-267)

Find `### 9. Sleep and Loop` and replace the ENTIRE section (heading + code block + "Then return to step 1."):

Old:
```markdown
### 9. Sleep and Loop

```bash
sleep 30
```

Then return to step 1.
```

New:
```markdown
### 9. Wait for Poller

The background poller handles timing. The main thread does not sleep — it processes poller reports as they arrive and returns to idle between reports. See **Polling Architecture** above.

If operating in fallback mode (no background tasks), end turn here. The external runtime re-invokes the controller for the next iteration.
```

### Edit 5b: Update Labels table — clarify `needs-approval` (~line 484)

Find the row:
```markdown
| `needs-approval` | Architect done, waiting for human approval |
```

Replace with:
```markdown
| `needs-approval` | Waiting for human approval (architect output or merge readiness) |
```

### Edit 6a: Add 7 Red Flag rows (after last existing row, before `## Common Mistakes`)

Find the Red Flags table (`## Red Flags — STOP and Verify`). After the LAST existing row (the `--version` row), append:

```markdown
| "Let me skip planning, the issue is simple enough" | STOP. Every phase runs. No exceptions. |
| "Testing isn't needed, it's a trivial change" | STOP. The tester ALWAYS runs. |
| "Let me skip retro, the PR is clean" | Check routing hints. Only skip when ALL conditions met. |
| "Let me just merge this PR directly" | STOP. Dispatch a merge worker. |
| "I'll rebase and push this fix" | STOP. Dispatch an implementer. |
| "I'll run the tests myself" | STOP. Dispatch a tester. |
| "Let me quickly edit this file" | STOP. You're doing worker work. Dispatch the appropriate worker. |
```

### Edit 6b: Update existing Red Flag — remove `jj log` suggestion

Find the existing Red Flag row:
```markdown
| "The changes are lost" | Check local commits (`jj log`), open PRs (`gh pr list`), and worker workspaces before concluding anything is lost |
```

Replace with (remove `jj log` — controller MUST NOT run jj commands per Role Boundary):
```markdown
| "The changes are lost" | Check open PRs (`gh pr list`), worker workspaces (daemon API), and issue comments before concluding anything is lost. Do NOT run `jj` — dispatch a worker to check version control. |
```

### Edit 6c: Add 2 Common Mistakes rows (after last existing row, before `## Status Flow`)

Find the Common Mistakes table. After the LAST existing row (the `--version` row), append:

```markdown
| Running `jj`, `gh pr merge`, or editing files | Controllers dispatch workers. Never touch code/branches/PRs directly. |
| Skipping phases because "it's simple" | Every phase runs. Simple issues just go through faster. |
```

### Edit 6d: Update existing Common Mistake — remove sleep reference

Find the existing row:
```markdown
| Exit after processing all issues | **Never exit** - loop forever with 30s sleep |
```

Replace with:
```markdown
| Exit after processing all issues | **Never exit** - loop continuously via background polling (see Polling Architecture) |
```

- [ ] **Step 1: Read the file and apply all edits (1a through 6d) in a single editing session**

Read `.opencode/skills/legion-controller/SKILL.md`, identify each anchor by heading text (not line number), and apply all edits top-to-bottom. Use multiple edit operations in one tool call where possible.

- [ ] **Step 2: Verify all changes**

Run:
```bash
# Change 1: User Interaction Priority
grep -c "0\. Respond to user messages" .opencode/skills/legion-controller/SKILL.md
# Expected: 1
grep -c "### User Interaction Priority" .opencode/skills/legion-controller/SKILL.md
# Expected: 1

# Change 2: Autonomy vs Approval
grep -c "### Autonomy vs Approval" .opencode/skills/legion-controller/SKILL.md
# Expected: 1
grep -c "Scale caution to blast radius" .opencode/skills/legion-controller/SKILL.md
# Expected: 1
grep -c "If you are wrong, how bad is it" .opencode/skills/legion-controller/SKILL.md
# Expected: 1
grep 'MUST NOT ask "should I continue?"' .opencode/skills/legion-controller/SKILL.md
# Expected: 1 match

# Change 3: Polling Architecture + Step 9
grep -c "### Polling Architecture" .opencode/skills/legion-controller/SKILL.md
# Expected: 1
grep -c "task(run_in_background=true)" .opencode/skills/legion-controller/SKILL.md
# Expected: 1
grep -c "sleep 30" .opencode/skills/legion-controller/SKILL.md
# Expected: 0
grep -c "Sleep 30s" .opencode/skills/legion-controller/SKILL.md
# Expected: 0
grep -c "Sleep and Loop" .opencode/skills/legion-controller/SKILL.md
# Expected: 0
grep -c "Wait for Poller" .opencode/skills/legion-controller/SKILL.md
# Expected: 2 (diagram + heading)
grep "Needs your attention" .opencode/skills/legion-controller/SKILL.md
# Expected: 1 match

# Change 4: Pre-Merge Gate
grep -c "### Pre-Merge Gate" .opencode/skills/legion-controller/SKILL.md
# Expected: 1
grep -n "### Quality Gate\|### Pre-Merge Gate\|### Post-Merge" .opencode/skills/legion-controller/SKILL.md
# Expected: 3 lines, Pre-Merge between Quality Gate and Post-Merge
grep "Human override" .opencode/skills/legion-controller/SKILL.md
# Expected: 1 match
grep "conditions were overridden" .opencode/skills/legion-controller/SKILL.md
# Expected: 1 match

# Change 5: Pipeline Integrity
grep -c "### Pipeline Integrity" .opencode/skills/legion-controller/SKILL.md
# Expected: 1
grep -c "MUST NOT skip" .opencode/skills/legion-controller/SKILL.md
# Expected: 1
grep -c "tester ALWAYS runs" .opencode/skills/legion-controller/SKILL.md
# Expected: 1

# Change 6: Role Boundary
grep -c "### Role Boundary" .opencode/skills/legion-controller/SKILL.md
# Expected: 1
grep -c "controller MUST NOT" .opencode/skills/legion-controller/SKILL.md
# Expected: 1
grep -c "STOP\. Dispatch" .opencode/skills/legion-controller/SKILL.md
# Expected: at least 3
grep "Controllers dispatch workers" .opencode/skills/legion-controller/SKILL.md
# Expected: 1 match

# Cleanup: existing contradictions resolved
grep "(Highest Priority)" .opencode/skills/legion-controller/SKILL.md
# Expected: no match (removed from Step 2 heading)
grep "loop forever with 30s sleep" .opencode/skills/legion-controller/SKILL.md
# Expected: no match (updated)
grep 'Check local commits (`jj log`)' .opencode/skills/legion-controller/SKILL.md
# Expected: no match (updated to remove jj suggestion)

# Preservation: skip-architect exception in Route Triage untouched
grep -A2 "Bug label" .opencode/skills/legion-controller/SKILL.md | grep -i "skip architect\|dispatch planner"
# Expected: at least 1 match (Route Triage exception preserved)

# Labels table updated
grep "needs-approval.*merge readiness" .opencode/skills/legion-controller/SKILL.md
# Expected: 1 match
```

---

## Task 2: Mirror and Final Verification — Depends on: Task 1

- [ ] **Step 1: Copy to mirror**

```bash
cp .opencode/skills/legion-controller/SKILL.md .claude/skills/legion-controller/SKILL.md
```

- [ ] **Step 2: Verify parity**

```bash
diff .opencode/skills/legion-controller/SKILL.md .claude/skills/legion-controller/SKILL.md
# Expected: no output (files identical)
```

- [ ] **Step 3: No code impact check**

```bash
bunx tsc --noEmit  # Expected: exit 0
bun test  # Expected: all pass
```

- [ ] **Step 4: Describe commit**

```bash
jj describe -m "skill(controller): add polling architecture, autonomy table, pipeline integrity, role boundary (#125)"
```

---

## Parallelism Summary

```
Task 1: [All edits to SKILL.md]       — Independent (single file, sequential edits)
Task 2: [Mirror + Verify + Commit]     — Depends on: Task 1
```

**Recommended approach:** Read the file once, apply all edits from Task 1 in one or two edit tool calls (referencing original line positions — the tool applies bottom-up), then proceed to Task 2.

---

## Testing Plan

### Setup
No environment setup needed — Markdown-only changes.

### Health Check
```bash
test -f .opencode/skills/legion-controller/SKILL.md && echo "OK"
test -f .claude/skills/legion-controller/SKILL.md && echo "OK"
```

### Verification Steps

**1. User Interaction Priority (Change 1)**
- Action: `grep -c "0\. Respond to user messages" .opencode/skills/legion-controller/SKILL.md`
- Expected: `1`
- Action: `grep -c "### User Interaction Priority" .opencode/skills/legion-controller/SKILL.md`
- Expected: `1`
- Tool: grep

**2. Autonomy vs Approval (Change 2)**
- Action: `grep -c "### Autonomy vs Approval" .opencode/skills/legion-controller/SKILL.md`
- Expected: `1`
- Action: `grep "Scale caution to blast radius" .opencode/skills/legion-controller/SKILL.md`
- Expected: 1 match
- Action: `grep 'MUST NOT ask "should I continue?"' .opencode/skills/legion-controller/SKILL.md`
- Expected: 1 match
- Tool: grep

**3. Polling Architecture (Change 3)**
- Action: `grep -c "sleep 30" .opencode/skills/legion-controller/SKILL.md`
- Expected: `0`
- Action: `grep -c "Wait for Poller" .opencode/skills/legion-controller/SKILL.md`
- Expected: `2`
- Action: `grep "task(run_in_background=true)" .opencode/skills/legion-controller/SKILL.md`
- Expected: 1 match
- Tool: grep

**4. Pre-Merge Gate (Change 4)**
- Action: `grep -n "### Quality Gate\|### Pre-Merge Gate\|### Post-Merge" .opencode/skills/legion-controller/SKILL.md`
- Expected: 3 lines in correct order
- Action: `grep "Human override" .opencode/skills/legion-controller/SKILL.md`
- Expected: 1 match
- Tool: grep

**5. Pipeline Integrity (Change 5)**
- Action: `grep -c "MUST NOT skip" .opencode/skills/legion-controller/SKILL.md`
- Expected: `1`
- Action: Verify skip-architect preserved: `grep -i "Bug label" .opencode/skills/legion-controller/SKILL.md`
- Expected: at least 1 match in Route Triage (untouched)
- Tool: grep

**6. Role Boundary (Change 6)**
- Action: `grep -c "controller MUST NOT" .opencode/skills/legion-controller/SKILL.md`
- Expected: `1`
- Action: `grep -c "STOP\. Dispatch" .opencode/skills/legion-controller/SKILL.md`
- Expected: at least 3
- Action: `grep "Controllers dispatch workers" .opencode/skills/legion-controller/SKILL.md`
- Expected: 1 match
- Tool: grep

**7. Cleanup (no contradictions)**
- Action: `grep "(Highest Priority)" .opencode/skills/legion-controller/SKILL.md`
- Expected: no match
- Action: `grep "loop forever with 30s sleep" .opencode/skills/legion-controller/SKILL.md`
- Expected: no match
- Tool: grep

**8. File Parity**
- Action: `diff .opencode/skills/legion-controller/SKILL.md .claude/skills/legion-controller/SKILL.md`
- Expected: no output
- Tool: diff

**9. No Code Impact**
- Action: `bunx tsc --noEmit` — Expected: exit 0
- Action: `bun test` — Expected: all pass
- Tool: bun
