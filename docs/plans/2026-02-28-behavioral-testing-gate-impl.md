# Behavioral Testing Gate Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a mandatory behavioral testing phase (`test` mode) to the Legion pipeline between implement and review.

**Architecture:** New `Testing` issue status + `test` worker mode + `dispatch_tester` action. Tester is a fresh agent that boots the app and verifies acceptance criteria. Upstream workflows (architect, planner, implementer) gain testing-related sections. Controller routes new actions.

**Tech Stack:** TypeScript/Bun (daemon), Markdown (skills), Bun test (testing)

**Design doc:** `docs/plans/2026-02-28-behavioral-testing-gate-design.md`

---

### Task 1: Add new types — Independent

**Files:**
- Modify: `packages/daemon/src/state/types.ts`

**Step 1: Add "Testing" status**

In `IssueStatusLiteral` (line 22), add `"Testing"` between `"In Progress"` and `"Needs Review"`:

```typescript
export type IssueStatusLiteral =
  | "Triage"
  | "Icebox"
  | "Backlog"
  | "Todo"
  | "In Progress"
  | "Testing"
  | "Needs Review"
  | "Retro"
  | "Done";
```

In `IssueStatus` constant (line 63), add after `IN_PROGRESS`:

```typescript
TESTING: "Testing" as IssueStatusLiteral,
```

In `_lowercaseCanonicalMap` (line 123), add after `["in progress", "In Progress"]`:

```typescript
["testing", "Testing"],
```

**Step 2: Add "test" worker mode**

In `WorkerModeLiteral` (line 35):

```typescript
export type WorkerModeLiteral = "architect" | "plan" | "implement" | "test" | "review" | "merge";
```

In `WorkerMode` constant (line 141), add after `IMPLEMENT`:

```typescript
TEST: "test" as WorkerModeLiteral,
```

**Step 3: Add new action types**

In `ActionType` (line 40), add after `"dispatch_implementer_for_retro"`:

```typescript
  | "dispatch_tester"
  | "transition_to_testing"
  | "resume_implementer_for_test_failure"
```

**Step 4: Add test label computed properties to ParsedIssue**

In `createParsedIssue` (line 242), add getters after `hasHumanApproved`:

```typescript
    get hasTestPassed() {
      return this.labels.includes("test-passed");
    },

    get hasTestFailed() {
      return this.labels.includes("test-failed");
    },
```

Update the `ParsedIssue` interface (line 221) to include these:

```typescript
  readonly hasTestPassed: boolean;
  readonly hasTestFailed: boolean;
```

**Step 5: Add test fields to FetchedIssueData**

In `FetchedIssueData` interface (line 297), add after `hasHumanApproved`:

```typescript
  hasTestPassed: boolean;
  hasTestFailed: boolean;
```

**Step 6: Run type check**

Run: `bunx tsc --noEmit`
Expected: Errors in fetch.ts and decision.ts about missing fields (we'll fix those in Tasks 2-3)

**Step 7: Commit**

```bash
jj describe -m "feat: add Testing status, test mode, and tester action types"
jj new
```

---

### Task 2: Write failing tests for new decision logic — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/state/__tests__/decision.test.ts`

**Step 1: Add Testing tests to suggestAction describe block**

Add after the existing `done_always_skips` test (around line 131):

```typescript
  // Testing status
  it("in_progress_worker_done_transitions_to_testing", () => {
    const action = suggestAction(IssueStatus.IN_PROGRESS, true, false, null, false, false);
    expect(action).toBe("transition_to_testing");
  });

  it("testing_no_worker_done_no_live_worker_dispatches_tester", () => {
    const action = suggestAction(IssueStatus.TESTING, false, false, null, false, false);
    expect(action).toBe("dispatch_tester");
  });

  it("testing_worker_done_test_passed_transitions_to_needs_review", () => {
    const action = suggestAction(IssueStatus.TESTING, true, false, null, true, true);
    expect(action).toBe("transition_to_needs_review");
  });

  it("testing_worker_done_test_failed_resumes_implementer", () => {
    const action = suggestAction(IssueStatus.TESTING, true, false, null, true, false);
    expect(action).toBe("resume_implementer_for_test_failure");
  });

  it("testing_with_live_worker_skips", () => {
    const action = suggestAction(IssueStatus.TESTING, false, true, null, false, false);
    expect(action).toBe("skip");
  });
```

**Step 2: Update existing in_progress_worker_done test**

The test at line 28 expects `transition_to_needs_review` — update to `transition_to_testing`:

```typescript
  it("in_progress_worker_done transitions to testing", () => {
    const action = suggestAction(IssueStatus.IN_PROGRESS, true, false, null, false, false);
    expect(action).toBe("transition_to_testing");
  });
```

Also update the `feedback_without_input_needed_follows_normal_flow` test (line 224) which expects `transition_to_needs_review` for In Progress + worker-done — it should now expect `transition_to_testing`:

```typescript
    expect(state.suggestedAction).toBe("transition_to_testing");
```

And the `buildCollectedState` test `builds_state_for_multiple_issues` (line 556) which expects `transition_to_needs_review` for In Progress + worker-done:

```typescript
    expect(state.issues["ENG-22"].suggestedAction).toBe("transition_to_testing");
```

**Step 3: Add buildIssueState tests for Testing status**

Add to the `buildIssueState` describe block:

```typescript
  it("testing_worker_done_test_passed_transitions_to_needs_review", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "Testing",
      labels: ["worker-done", "test-passed"],
      hasPr: true,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: true,
      hasTestFailed: false,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("transition_to_needs_review");
  });

  it("testing_worker_done_test_failed_resumes_implementer", () => {
    const data: FetchedIssueData = {
      issueId: "ENG-21",
      status: "Testing",
      labels: ["worker-done", "test-failed"],
      hasPr: true,
      prIsDraft: null,
      hasLiveWorker: false,
      workerMode: null,
      workerStatus: null,
      hasUserFeedback: false,
      hasUserInputNeeded: false,
      hasNeedsApproval: false,
      hasHumanApproved: false,
      hasTestPassed: false,
      hasTestFailed: true,
      source: null,
    };

    const state = buildIssueState(data, "00000000-0000-0000-0000-000000000000");
    expect(state.suggestedAction).toBe("resume_implementer_for_test_failure");
  });
```

**Step 4: Update all existing FetchedIssueData literals**

Every `FetchedIssueData` object in the test file needs the two new fields. Add to each:

```typescript
      hasTestPassed: false,
      hasTestFailed: false,
```

There are approximately 20 FetchedIssueData literals in the file. Each one needs these fields.

**Step 5: Run tests to verify they fail**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: FAIL — `suggestAction` signature doesn't match yet, Testing case not implemented

**Step 6: Commit**

```bash
jj describe -m "test: add failing tests for Testing status and tester dispatch"
jj new
```

---

### Task 3: Implement decision logic — Depends on: Task 2

**Files:**
- Modify: `packages/daemon/src/state/decision.ts`

**Step 1: Update suggestAction signature and In Progress case**

Add `hasTestPassed` parameter and change In Progress transition:

```typescript
export function suggestAction(
  status: IssueStatusLiteral | string,
  hasWorkerDone: boolean,
  hasLiveWorker: boolean,
  prIsDraft: boolean | null,
  hasPr: boolean,
  hasTestPassed: boolean
): ActionType {
```

In the `IN_PROGRESS` case (line 53), change `transition_to_needs_review` to `transition_to_testing`:

```typescript
    case IssueStatus.IN_PROGRESS:
      if (hasWorkerDone) {
        return "transition_to_testing";
      }
```

**Step 2: Add Testing case**

Add after the `IN_PROGRESS` case, before `NEEDS_REVIEW`:

```typescript
    case IssueStatus.TESTING:
      if (hasWorkerDone) {
        if (hasTestPassed) {
          return "transition_to_needs_review";
        }
        return "resume_implementer_for_test_failure";
      }
      if (hasLiveWorker) {
        return "skip";
      }
      return "dispatch_tester";
```

**Step 3: Update ACTION_TO_MODE**

Add new action mappings (line 97):

```typescript
  dispatch_tester: WorkerMode.TEST,
  transition_to_testing: WorkerMode.TEST,
  resume_implementer_for_test_failure: WorkerMode.IMPLEMENT,
```

**Step 4: Update buildIssueState to pass hasTestPassed**

In `buildIssueState` (line 118), update the `suggestAction` call to pass the new parameter:

```typescript
    action = suggestAction(
      data.status,
      data.labels.includes("worker-done"),
      data.hasLiveWorker,
      data.prIsDraft,
      data.hasPr,
      data.hasTestPassed ?? false
    );
```

**Step 5: Run tests**

Run: `bun test packages/daemon/src/state/__tests__/decision.test.ts`
Expected: ALL PASS

**Step 6: Run full suite + type check**

Run: `bun test && bunx tsc --noEmit`
Expected: PASS (may have fetch.ts errors if FetchedIssueData fields not wired yet — fix in Task 4)

**Step 7: Commit**

```bash
jj describe -m "feat: implement Testing status decision logic and tester dispatch"
jj new
```

---

### Task 4: Wire up new fields in enrichment — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/state/fetch.ts`

**Step 1: Add test label fields to enrichParsedIssues return**

In `enrichParsedIssues` (line 290), add to the return object (after `hasHumanApproved`, around line 337):

```typescript
      hasTestPassed: issue.hasTestPassed,
      hasTestFailed: issue.hasTestFailed,
```

**Step 2: Run type check and tests**

Run: `bunx tsc --noEmit && bun test`
Expected: ALL PASS

**Step 3: Commit**

```bash
jj describe -m "feat: wire test-passed/test-failed labels in enrichment"
jj new
```

---

### Task 5: Create tester workflow — Independent

**Files:**
- Create: `.opencode/skills/legion-worker/workflows/test.md`

**Step 1: Write the tester workflow**

Create `.opencode/skills/legion-worker/workflows/test.md` with the following content. Follow the existing workflow file patterns (architect.md, implement.md) for structure and conventions.

The workflow should contain:

```markdown
# Test Workflow

Behavioral verification of implemented features against running infrastructure.

## Context

You are a fresh agent with no prior context about the implementation.
Your job is to verify that the feature works by exercising it against real
running infrastructure — not by reading code or running unit tests.

## Inputs

You receive:
- The issue (with acceptance criteria from architect)
- The testing plan (from planner, posted as an issue comment)
- The PR (code changes and documentation)

## Workflow

### 1. Fetch Context

[GitHub/Linear fetch commands — same pattern as other workflows]

Extract:
- Acceptance criteria from the issue
- Testing plan from issue comments (look for "Testing Plan" section)
- PR metadata: `gh pr view $PR_NUMBER --json title,body,files -R $OWNER/$REPO`

### 2. Read the Documentation

Before doing anything else, try to understand the feature from the repo's
documentation alone. Read the README, usage guides, and any docs the
implementer updated in the PR.

This is intentional — your first experience mirrors a real user's experience.
Note any gaps, confusion, or missing information.

### 3. Boot the Environment

Follow the testing plan's setup instructions:

1. Run the setup commands from the testing plan
2. Run the health check to verify the environment is ready
3. If the environment fails to boot, that is a test failure — skip to
   step 6 with the boot error as evidence

### 4. Execute Acceptance Criteria

Work through each criterion from the testing plan. Use appropriate tools:

- **Playwright / agent-browser** for web UIs (navigate, click, fill forms,
  verify results)
- **curl / HTTP requests** for APIs (hit endpoints, verify responses)
- **CLI commands** for command-line tools (run commands, verify output)
- **Subprocess execution** for scripts, build tools

For each criterion, capture concrete evidence:
- Screenshots (for UI tests)
- Command output (for CLI/API tests)
- Log excerpts (for backend behavior)

Do NOT accept "it looks like it works" — capture actual artifacts.

### 5. Assess Documentation Quality

Based on your experience in steps 2-4:
- Was it easy to understand what the feature does from the docs?
- Were setup instructions accurate and complete?
- Were there steps you had to figure out that should have been documented?
- Would a new user be able to use this feature from the docs alone?

### 6. Post Results to PR

Post a structured comment on the PR:

[Template with pass/fail per criterion, evidence, doc feedback, observations]

### 7. Signal Completion

Add labels based on outcome:

**If all criteria pass:**
- Add `worker-done` label
- Add `test-passed` label

**If any criterion fails:**
- Add `worker-done` label
- Add `test-failed` label

Then remove `worker-active` and exit.
```

Flesh out each section with the full GitHub/Linear command patterns matching the other workflow files. Include the PR comment template, label update commands, and error handling patterns.

**Step 2: Commit**

```bash
jj describe -m "feat: create tester workflow (test.md)"
jj new
```

---

### Task 6: Update worker SKILL.md — Depends on: Task 5

**Files:**
- Modify: `.opencode/skills/legion-worker/SKILL.md`

**Step 1: Add test to mode routing table**

In the Mode Routing table (line 126), add after `implement`:

```markdown
| `test` | @workflows/test.md | Yes |
```

**Step 2: Update lifecycle order**

Update line 134:

```markdown
**Lifecycle order:** architect → plan → implement → test → review → (implement if changes requested) → retro → merge
```

**Step 3: Commit**

```bash
jj describe -m "feat: add test mode to worker routing table"
jj new
```

---

### Task 7: Update architect workflow — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/architect.md`

**Step 1: Add Testing Infrastructure Assessment**

After the "What Makes Good Acceptance Criteria" section (line 88), add a new section:

```markdown
## Testing Infrastructure Assessment

After defining acceptance criteria, assess whether they can be verified
against running infrastructure. Add a "Testing Infrastructure" section
to your output:

**For each acceptance criterion, evaluate:**
- Can this be verified against a running application?
- What infrastructure is needed? (local server, browser, database, seed data)
- What's missing? (no docker-compose, no seed script, README doesn't explain
  how to run locally)

**Example output:**

```
### Testing Infrastructure

**Available:**
- Local dev server via `bun run dev`
- Seed data script at `scripts/seed.sh`

**Gaps:**
- No browser test harness (Playwright not configured)
- README missing instructions for local database setup
- No health check endpoint to verify server is ready
```

Flag gaps clearly so the user can address them before planning begins.
If the project has no way to run locally at all, note this prominently.
```

**Step 2: Update acceptance criteria guidance**

In the "What Makes Good Acceptance Criteria" section, add a note about behavioral verifiability:

```markdown
Each criterion should also be **behaviorally verifiable** — a tester should
be able to verify it by interacting with the running application, not just
by reading code or running unit tests.
```

**Step 3: Commit**

```bash
jj describe -m "feat: add testing infrastructure assessment to architect workflow"
jj new
```

---

### Task 8: Update planner workflow — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/plan.md`

**Step 1: Add Testing Plan section**

After the "Parallelism Annotation" section (line 146), add:

```markdown
#### Testing Plan

After creating executable tasks, add a **Testing Plan** section to the plan.
The tester agent will use this to verify the implementation works end-to-end.

**Required sections:**

```
## Testing Plan

### Setup
- [Concrete commands to boot the environment]
- [e.g., `bun install && bun run dev`, `docker-compose up -d`]

### Health Check
- [How to verify the environment is ready]
- [e.g., `curl -s http://localhost:3000/health` returns 200]

### Verification Steps
For each acceptance criterion:
1. **[Criterion name]**
   - Action: [What to do — navigate to URL, run command, etc.]
   - Expected: [What should happen — page shows X, API returns Y]
   - Tool: [Playwright / curl / CLI]

### Tools Needed
- [List of tools the tester should use]
- [e.g., Playwright for browser, curl for API, CLI for commands]
```

**Guidelines:**
- Setup commands must be copy-pasteable — no placeholders the tester would
  need to figure out
- Health checks should have a timeout (e.g., "retry for 30s")
- Verification steps should be specific enough that a fresh agent with no
  implementation context can follow them
- If infrastructure doesn't exist yet (architect flagged gaps), note what
  the implementer needs to create
```

**Step 2: Commit**

```bash
jj describe -m "feat: add testing plan section to planner workflow"
jj new
```

---

### Task 9: Update implementer workflow — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/implement.md`

**Step 1: Add documentation requirement to Pre-Ship Verification**

After the Pre-Ship Verification section (line 77), before Cross-Family Review, add:

```markdown
### 4.5. Documentation

For any user-facing behavior change, update relevant documentation before
creating the PR:

- **README** — if the feature changes setup, usage, or configuration
- **Usage guides** — if the feature adds new user-facing functionality
- **API docs** — if the feature changes or adds API endpoints
- **Inline help** — if the feature adds CLI commands or options

Documentation should explain **how to use** the feature, not just what changed
in the code. A user reading only the docs should be able to understand and
use the new functionality.

Skip this step if the change is purely internal (refactoring, bug fix with
no behavior change, test-only changes).
```

**Step 2: Change exit behavior to add worker-done**

In "7. Exit" (line 148), change from:

```markdown
Exit without adding labels. The controller handles state transitions explicitly.
```

To:

```markdown
Add `worker-done` label, then remove `worker-active` label. The controller
uses `worker-done` to trigger the transition to the Testing phase.

**GitHub:**
```
gh issue edit $ISSUE_NUMBER --add-label "worker-done" --remove-label "worker-active" -R $OWNER/$REPO
```

**Linear:**
```
issue = linear_linear(action="get", id=$LEGION_ISSUE_ID)
current_labels = [l.name for l in issue.labels if l.name != "worker-active"]
linear_linear(action="update", id=$LEGION_ISSUE_ID, labels=[...current_labels, "worker-done"])
```
```

**Step 3: Update the Mode Routing table reference**

In the worker SKILL.md, the implement mode says "Adds `worker-done`: No". This was updated in Task 6, but double-check that the routing table now says "Yes" for implement mode.

**Step 4: Commit**

```bash
jj describe -m "feat: add docs requirement and worker-done signal to implementer"
jj new
```

---

### Task 10: Update controller skill — Depends on: Task 1

**Files:**
- Modify: `.opencode/skills/legion-controller/SKILL.md`

**Step 1: Update Implement → Review Handoff section**

Replace the "Implement → Review Handoff" section (line 136) with:

```markdown
### Implement → Testing → Review Handoff

The implementer now adds `worker-done` when finished:
1. Implementer opens a **draft PR**, adds `worker-done`, and exits
2. State machine sees: In Progress + `worker-done` → `transition_to_testing`
3. Controller transitions issue to Testing status
4. Controller runs the quality gate (below)
5. If quality gate passes: dispatch tester
6. If quality gate fails: move back to In Progress, dispatch fresh implementer

After the tester runs:
- **Test passed** (`test-passed` label): Controller transitions to Needs Review,
  dispatches reviewer (no additional quality gate needed — already verified)
- **Test failed** (`test-failed` label): Controller removes test labels and
  `worker-done`, transitions back to In Progress, resumes implementer session
  with the test failure report from the PR comment
```

**Step 2: Update Quality Gate section**

Update "When to run" (line 148):

```markdown
**When to run:** Whenever about to execute a `dispatch_tester` action.
```

**Step 3: Add test-specific labels**

Add to the Labels table (line 340):

```markdown
| `test-passed` | Tester verified behavior, controller advances |
| `test-failed` | Tester found issues, controller returns to implementer |
```

**Step 4: Update Status Flow diagram**

Replace the status flow (line 378):

```markdown
```
Triage ─┬─► Icebox ─► Backlog ─► Todo ─► In Progress ─► Testing ─► Needs Review ─► Retro ─► Done
        ├─► Backlog ──────────────┘           │                         │
        └─► Todo ─────────────────────────────┴─────────────────────────┘
```
```

**Step 5: Update routing table**

The existing prefix-based routing table (line 108) automatically handles new action types. Verify that `dispatch_tester` matches `dispatch_` prefix and `transition_to_testing` matches `transition_to_` prefix. No changes needed if the naming convention is followed.

Add a note after the routing table:

```markdown
**Test failure handling:** When `resume_implementer_for_test_failure` is suggested:
1. Remove `worker-done`, `test-failed` labels
2. Read the tester's PR comment for the failure report
3. Resume the implementer session with the failure details
```

**Step 6: Commit**

```bash
jj describe -m "feat: update controller for Testing status routing"
jj new
```

---

### Task 11: Update AGENTS.md and documentation — Depends on: Task 5, Task 6

**Files:**
- Modify: `.opencode/skills/AGENTS.md`
- Modify: `AGENTS.md` (root)

**Step 1: Update skills AGENTS.md**

In the Structure section, add `test.md` to the workflows list:

```markdown
    │   ├── test.md        # Behavioral testing against running infrastructure
```

Update the Worker Lifecycle section to include the test phase.

**Step 2: Update root AGENTS.md**

Update the Issue Lifecycle diagram:

```markdown
Triage ──┬──► Icebox ──► Backlog ──► Todo ──► In Progress ──► Testing ──► Needs Review ──► Retro ──► Done
```

Update "Worker modes" to include: `architect → plan → implement → test → review → merge`

Update the Labels table to include `test-passed` and `test-failed`.

**Step 3: Commit**

```bash
jj describe -m "docs: update AGENTS.md for behavioral testing gate"
jj new
```

---

### Task 12: Final verification — Depends on: Task 1, Task 2, Task 3, Task 4

**Step 1: Run full test suite**

Run: `bun test`
Expected: ALL PASS

**Step 2: Type check**

Run: `bunx tsc --noEmit`
Expected: NO ERRORS

**Step 3: Lint**

Run: `bunx biome check packages/daemon/src/`
Expected: NO ERRORS

**Step 4: Verify test count increased**

Run: `bun test 2>&1 | tail -5`
Expected: Test count should be higher than baseline (was 172)

**Step 5: Commit (if any fixes needed)**

```bash
jj describe -m "fix: address lint/type issues from testing gate implementation"
jj new
```
