# Plan: Skill Audit — Top 10 Recurring User Corrections

**Issue:** #120
**Status:** Ready for review
**Scope:** 8 actionable items → 11 discrete changes across 4 files (× 2 directories)
**Risk:** Low — skill-file-only changes, no code modifications

## Overview

Audited 2,049 controller session messages from March 23–26. Extracted 10 recurring user corrections. 2 already fixed (#9 version flag, #10 workspace management). The remaining 8 map to 11 file changes across 4 skill files.

**Key insight:** Issues #1 and #6 share a root cause (sequential polling blocks the main thread). The fix is a single "Polling Architecture" section that addresses both.

## IMPORTANT: Dual Directory Mirroring

The skill files exist in **two identical directories** that must be kept in sync:
- `.opencode/skills/` — used by OpenCode
- `.claude/skills/` — used by Claude Code

**Every change must be applied to both directories.** After editing `.opencode/skills/...`, copy the same change to `.claude/skills/...`. Verify with `diff` before committing.

## Files Changed

| File (relative to skills root) | Changes | Lines (current) |
|------|---------|-----------------|
| `legion-controller/SKILL.md` | 6 modifications | 569 |
| `legion-worker/workflows/test.md` | 2 modifications | 310 |
| `legion-worker/workflows/review.md` | 2 modifications | 188 |
| `legion-worker/SKILL.md` | 1 modification | 176 |

---

## Changes to `legion-controller/SKILL.md`

### Change 1: User Interaction Priority Rule (Issue #8)

**Location:** Insert after "Core Principle" priority list (~line 26), before "## Algorithm".

**Rationale:** User messages directed at the controller are higher priority than any loop step. The existing "Relay User Feedback" (Step 2) is about relaying user comments TO workers — this is different. This rule is about answering user questions directed AT the controller itself.

**Content — new subsection `### User Interaction Priority`:**

- At the start of each loop iteration, check if the user has sent a direct question or new instructions
- If yes: STOP the current iteration, answer the user FIRST, then resume
- Never continue looping while an unanswered user question is pending
- If mid-dispatch when a user message arrives, finish the dispatch, then respond immediately
- Update Core Principle's priority list to add item 0: "Respond to user messages (always first)"

**Acceptance criteria:**
- [ ] New subsection exists between Core Principle and Algorithm
- [ ] Core Principle priority list has user interaction as #0
- [ ] Clearly distinguished from Step 2 (relay TO workers) — this is about answering the user directly
- [ ] Rule is unambiguous: check → answer → resume

---

### Change 2: Autonomy vs Approval Table (Issue #5)

**Location:** Insert after User Interaction Priority, before "## Algorithm".

**Rationale:** The controller keeps asking permission for routine operations. An explicit table eliminates ambiguity.

**Content — new subsection `### Autonomy vs Approval`:**

| Operation | Autonomous? | Notes |
|-----------|-------------|-------|
| Rebase branches | Yes | Just do it |
| Phase transitions | Yes | Follow the pipeline |
| Dispatch/resume workers | Yes | That's your job |
| Resolve merge conflicts | Yes | Don't block on conflicts |
| Label changes | Yes | Follow label conventions |
| Move issues between statuses | Yes | Follow the state machine |
| Merge PR to main | **NO** | Requires explicit user approval |

**Merge approval flow:** When all Pre-Merge Gate conditions (Change 4) are satisfied, post a comment summarizing readiness and add the `needs-approval` label. Do NOT dispatch the merge worker until the user approves (via comment, `human-approved` label, or direct instruction). This mirrors the architect approval flow.

**Acceptance criteria:**
- [ ] Table with at least the 7 operations listed
- [ ] Merge clearly requires approval with named signal (`needs-approval` label)
- [ ] Describes the approval flow (post readiness comment → add label → wait → dispatch on approval)
- [ ] All other pipeline operations are autonomous

---

### Change 3: Polling Architecture (Issues #1, #6)

**Location:** Insert after "## Algorithm" diagram and "Do not exit" line (~line 48), before "### 1. Fetch Issues".

**Rationale:** #1 most repeated correction (6+ times). Current Step 9 uses synchronous `sleep 30`, blocking the controller. When anything interrupts the loop, polling dies silently.

**Content — new subsection `### Polling Architecture`:**

The 9-step loop describes WHAT the controller does. The execution model uses **background polling** via Claude Code's `task(run_in_background=true)`:

1. **Main thread** — handles user messages, makes routing decisions, acts on poller reports. MUST never call `sleep` or block.
2. **Background poller** — a persistent background task that:
   - Fetches issues from tracker
   - Posts to `/state/collect` for analysis
   - Reports state changes (worker-done, user-feedback-given, triage items) back to main thread
   - Runs on ~60-second intervals
3. **Lifecycle:**
   - Launch the poller as a background task at session start
   - If the poller stops or is interrupted, re-launch immediately
   - Check for poller health periodically (at the start of each main-thread action)
   - The poller is disposable — no state to preserve, just re-launch

**Rules:**
- The main thread MUST never call `sleep`. If you find yourself writing `sleep 30`, you're doing it wrong.
- All polling happens via background tasks. The main thread stays free to receive user instructions.
- When the poller reports a state change, the main thread acts on it synchronously (dispatch, transition, relay), then returns to idle.

**Fallback:** If background tasks are unavailable, process all 9 steps as fast as possible without any `sleep`, then end your turn. The external runtime will re-invoke you on the next cycle.

**Also modify Step 9 (~line 261-268).** Replace:
```
### 9. Sleep and Loop
sleep 30
Then return to step 1.
```
With:
```
### 9. Continue
Return to step 1. The background poller handles timing — do not `sleep` here.
If the background poller has stopped, re-launch it before continuing.
```

**Acceptance criteria:**
- [ ] "Polling Architecture" subsection exists between Algorithm and Step 1
- [ ] References `task(run_in_background=true)` explicitly (Claude Code execution model)
- [ ] States main thread MUST never call `sleep`
- [ ] Describes poller lifecycle (launch, health check, re-launch)
- [ ] Includes fallback behavior
- [ ] Step 9 no longer contains `sleep 30`
- [ ] Step 9 references polling architecture

---

### Change 4: Pre-Merge Gate (Issue #3)

**Location:** Insert after existing "Quality Gate (Controller Policy)" section (~line 195), before "### Post-Merge Monitoring".

**Rationale:** Controller merged PRs that hadn't passed CI or testing. The existing Quality Gate covers pre-tester checks only.

**Content — new subsection `### Pre-Merge Gate`:**

Before requesting merge approval (see Autonomy vs Approval), verify ALL conditions:

- [ ] CI checks are green (not pending, not failed) — `gh pr checks "$LEGION_ISSUE_ID"`
- [ ] PR is NOT in draft state — `gh pr view "$LEGION_ISSUE_ID" --json isDraft -q .isDraft` returns `false`
- [ ] `test-passed` label is present (tester verified behavior)
- [ ] Issue has been through retro (or retro was explicitly skipped via routing hints)
- [ ] No `user-input-needed` label present

If ANY condition is not met, do NOT request merge approval. Log which condition failed and re-evaluate next iteration.

Include bash snippet for the full verification sequence.

**Note on merge.md:** The merge workflow handles rebase-induced CI failures (fixing type errors after rebase). This is a different scenario from the controller's pre-merge gate, which ensures the implementation itself has been properly tested and reviewed. merge.md does not need changes for this issue.

**Acceptance criteria:**
- [ ] Pre-Merge Gate subsection exists after Quality Gate, before Post-Merge Monitoring
- [ ] All 5 conditions listed with verification commands
- [ ] States "do NOT request merge approval" (not "do not dispatch merger")
- [ ] Connects to the Autonomy vs Approval merge flow (Change 2)

---

### Change 5: Phase-Skip Guards (Issue #2)

**Location:** Two insertion points:
1. **Normative rule** — insert after the "Implement → Testing → Review Handoff" section (~line 157-180), as a new subsection `### Pipeline Integrity`
2. **Red Flag entries** — add to existing Red Flags table (~line 500)

**Rationale:** Red Flags alone are advisory. Phase-skip guards need normative MUST/MUST NOT rules that the controller is required to follow, with Red Flags as reinforcement.

**Content — new subsection `### Pipeline Integrity`:**

The pipeline phases MUST run in order: architect → plan → implement → test → review → retro → merge.

**Rules:**
- The testing phase MUST NOT be skipped. The tester ALWAYS runs, even for "trivial" changes.
- The review phase MUST NOT be skipped after testing passes.
- Retro may be skipped ONLY when ALL conditions are met: `skipRetro=true` in routing hints AND no tricky parts AND no deviations. When in doubt, run retro.
- No phase may be skipped because the change "seems simple." Simple issues go through faster, but they go through every phase.

**Red Flag entries (3 new rows):**

| Thought | What to do instead |
|---------|--------------------|
| "Let me skip planning, the issue is simple enough" | STOP. Every phase runs. No exceptions. |
| "Testing isn't needed, it's a trivial change" | STOP. The tester ALWAYS runs. |
| "Let me skip retro, the PR is clean" | Check routing hints. Only skip when ALL skip conditions are explicitly met. |

**Acceptance criteria:**
- [ ] Pipeline Integrity subsection exists with MUST/MUST NOT rules
- [ ] Testing explicitly stated as unskippable
- [ ] Retro skip conditions are precise (all 3 conditions must be met)
- [ ] 3 Red Flag entries added as reinforcement
- [ ] Rules use "MUST NOT" language, not just "should" or "don't"

---

### Change 6: Role Boundary Guards (Issue #4)

**Location:** Two insertion points:
1. **Normative rule** — insert after Pipeline Integrity (Change 5), as subsection `### Role Boundary`
2. **Red Flag entries** — add to existing Red Flags table
3. **Common Mistakes entries** — add to existing Common Mistakes table

**Content — new subsection `### Role Boundary`:**

The controller MUST NOT perform worker actions. Specifically:
- MUST NOT run `jj` commands (rebase, push, edit)
- MUST NOT edit files or write code
- MUST NOT run `gh pr merge` directly
- MUST NOT run tests

If work needs doing, dispatch the appropriate worker. The controller's job is to dispatch, monitor, and route — never to execute.

**Red Flag entries (4 new rows):**

| Thought | What to do instead |
|---------|--------------------|
| "Let me just merge this PR directly" | STOP. Dispatch a merge worker. |
| "I'll rebase and push this fix" | STOP. Dispatch an implementer. |
| "I'll run the tests myself" | STOP. Dispatch a tester. |
| "Let me quickly edit this file" | STOP. You're about to do worker work. Dispatch the appropriate worker. |

**Common Mistakes (2 new rows):**

| Mistake | Correction |
|---------|------------|
| Running `jj`, `gh pr merge`, or editing files | Controllers dispatch workers. Never touch code, branches, or PRs directly. |
| Skipping phases because "it's simple" | Every phase runs. Simple issues just go through faster. |

**Acceptance criteria:**
- [ ] Role Boundary subsection with MUST NOT rules
- [ ] Explicitly names forbidden commands (`jj`, `gh pr merge`, file edits)
- [ ] 4 Red Flag entries with "STOP" in each
- [ ] 2 Common Mistakes entries

---

## Changes to `legion-worker/workflows/test.md`

### Change 7: Evidence Gate (Issue #7)

**Location:** Section 5 "Execute Acceptance Criteria" (~line 158-180) and Section 6 "Post Results to PR" (~line 212-240).

**Rationale:** test.md already has evidence guidance but testers still omit artifacts. Elevate from guidance to a hard gate.

**Changes:**
1. **Section 5** — add bold callout after the evidence types list:

   > **EVIDENCE GATE:** Every acceptance criterion in your PR comment MUST include at least one concrete artifact: screenshot, command output, log excerpt, or — for non-behavioral criteria verifiable only by reading code — "Verified by code inspection: [file:line]". A test result without evidence is not a valid test. One artifact may cover multiple related criteria if explicitly noted.

2. **Section 6 PR template** — change `[output/screenshot]` to `[REQUIRED: screenshot/output/log/code-ref]`

3. **Common Mistakes table** — add entry:

   | Mistake | Correction |
   |---------|------------|
   | Posting results without evidence artifacts | Every criterion needs at least one: screenshot, command output, log, or code reference. "It works" is not evidence. |

**Acceptance criteria:**
- [ ] Bold evidence gate callout in Section 5
- [ ] PR template marks evidence as REQUIRED
- [ ] Allows "Verified by code inspection: [file:line]" for non-behavioral criteria
- [ ] Allows one artifact for multiple related criteria
- [ ] New Common Mistakes entry

---

### Change 8: Test Handoff Enforcement (Issue #7)

**Location:** Section 6.5 "Write Handoff Data" (~line 242-250) and Section 7 "Signal Completion" (~line 252-280).

**Changes:**
1. **Section 6.5** — add: "You MUST attempt the handoff write before signaling completion. The `|| true` ensures CLI failures don't block you, but skipping this step entirely is not acceptable. If the write fails, note it in your PR comment."

2. **Section 7** — add pre-signal checklist: "Before adding labels, verify: (1) PR results posted (step 6), (2) handoff write attempted (step 6.5). If either was skipped, go back and do it."

**Acceptance criteria:**
- [ ] Section 6.5 uses "MUST attempt" language
- [ ] Section 7 has pre-signal checklist referencing steps 6 and 6.5
- [ ] `|| true` preserved (CLI failures non-blocking)
- [ ] Distinction clear: attempt is mandatory, success is non-blocking

---

## Changes to `legion-worker/workflows/review.md`

### Change 9: Evidence in Review Comments (Issue #7)

**Location:** Section 3 "Post Summary Comment" (~line 72-85) and Common Mistakes (~line 162-188).

**Changes:**
1. **Section 3 template** — add Evidence section:
   ```
   ### Evidence
   - CI status: [paste `gh pr checks` output]
   - [For each P1/P2: code snippet showing the issue, or test output demonstrating the problem]
   ```

2. **Common Mistakes** — add:

   | Mistake | Correction |
   |---------|------------|
   | Posting findings without evidence | Include code snippets, CI output, or reproduction steps for every P1/P2 finding. |

**Acceptance criteria:**
- [ ] Summary comment template includes Evidence section
- [ ] CI status output explicitly required
- [ ] P1/P2 findings require supporting evidence
- [ ] New Common Mistakes entry

---

### Change 10: Review Handoff Enforcement (Issue #7)

**Location:** Section 4.5 "Write Handoff Data" (~line 97-115) and Section 6 "Signal Completion" (~line 140-160).

Same pattern as Change 8.

**Changes:**
1. **Section 4.5** — add "MUST attempt" language before signaling
2. **Section 6** — add pre-signal checklist: "(1) summary comment posted (step 3), (2) line-level comments posted (step 4), (3) handoff write attempted (step 4.5), (4) PR draft status set (step 5)"

**Acceptance criteria:**
- [ ] Section 4.5 uses "MUST attempt"
- [ ] Section 6 has 4-item pre-signal checklist
- [ ] `|| true` preserved

---

## Changes to `legion-worker/SKILL.md`

### Change 11: Universal Handoff Enforcement (Issue #7)

**Location:** Add to "Essential Rules" section (~line 24-35) or "Exiting" section (~line 107-115).

**Rationale:** Changes 8 and 10 enforce handoff in test.md and review.md specifically. A universal rule in the worker SKILL.md catches ALL workflow phases (including implement, plan, architect).

**Content — add to Essential Rules as rule 4.5 (between "Signal completion" and "Clean up on exit"):**

> **Write handoff data before signaling.** Each workflow has a handoff write step — you MUST attempt it before adding `worker-done`. The `|| true` means CLI failures don't block you, but skipping the attempt is not acceptable. If the write fails, note it in your exit comment.

**Acceptance criteria:**
- [ ] New essential rule between rules 4 and 5
- [ ] Uses "MUST attempt" language consistently with Changes 8 and 10
- [ ] References `|| true` non-blocking pattern
- [ ] Applies universally to all workflow phases

---

## Non-Changes (Already Fixed)

| # | Issue | Status |
|---|-------|--------|
| 9 | Version flag misuse | Fixed in Session Versioning section |
| 10 | Workspace mismanagement | Fixed via `legion dispatch` + "One PR per issue" |

## Out of Scope

**merge.md** — The merge workflow handles rebase-induced CI failures (step 5: fix type errors after rebase). This is a legitimate merger responsibility for issues introduced by the rebase itself, not a bypass of the testing phase. The Pre-Merge Gate (Change 4) ensures the implementation passed CI before the merger was dispatched. No merge.md changes needed for this issue.

---

## Testing Infrastructure

Markdown-only changes. Verification:

1. **Content presence** — each acceptance criterion checkable by reading the file
2. **Consistency** — no contradictions with existing sections (reviewer verifies)
3. **Dual directory sync** — `diff .opencode/skills/... .claude/skills/...` returns clean
4. **Lint** — `bunx biome check` (if Biome handles .md)
5. **No code impact** — `bun test` and `bunx tsc --noEmit` unaffected

---

## Open Questions for Review

1. **Merge approval signal:** Plan proposes `needs-approval` label (matching architect flow). Is this the right mechanism, or should merges use a different signal?

2. **Polling architecture specificity:** The plan describes `task(run_in_background=true)` as the polling mechanism. Should it include a concrete code template for the poller task, or is the conceptual description sufficient for the skill file?

3. **Evidence gate strictness:** The plan allows "Verified by code inspection: [file:line]" as evidence for non-behavioral criteria. Is this lenient enough, or should there be other exception types?

---

## Concerns

1. **Polling architecture depends on runtime.** The skill describes the ideal (background tasks) and a fallback (no-sleep fast iteration). If the agent runtime can't maintain persistent background tasks, the fallback is the practical behavior.

2. **Handoff enforcement language precision.** "Mandatory attempt, non-blocking outcome" is the formulation. Workers MUST try the handoff write. If it fails (CLI error), they continue but note the failure. This preserves the `|| true` pattern while making the attempt non-optional.

3. **Dual directory maintenance.** Long-term, `.opencode` and `.claude` should be symlinked or auto-synced. For this PR, manual duplication with diff verification is acceptable.

---

## Implementation Order

All changes are independent. Suggested order for a single implementer:

1. Controller Red Flags + Common Mistakes (Changes 5, 6) — additive rows, lowest risk
2. Controller User Interaction Priority + Autonomy (Changes 1, 2) — new sections
3. Controller Pre-Merge Gate (Change 4) — new section
4. Controller Polling Architecture + Step 9 (Change 3) — most complex
5. Worker SKILL.md handoff rule (Change 11) — one line
6. Test workflow (Changes 7, 8) — independent file
7. Review workflow (Changes 9, 10) — independent file
8. Mirror all changes to `.claude/skills/` — final step, verify with diff

## Relevant Skills for Downstream Workers

- **legion-worker** — standard worker dispatch
- **using-jj** — version control operations for commit/push
- No external libraries involved
