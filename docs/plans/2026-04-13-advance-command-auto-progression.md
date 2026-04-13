# Architecture: `legion advance` + Auto-Progression (#494)

**Issue:** feat: simplify controller lifecycle primitives — advance command + automatic progression  
**Date:** 2026-04-13  
**Phase:** Architect

---

## Problem Statement

The controller's job is too complex because lifecycle primitives are too low-level:

1. The controller must manually route every transition — it reads `suggestedAction`, decides what to do, and dispatches workers one at a time.
2. Stages get skipped (retro, merge) because the controller has to explicitly handle each one.
3. The controller writes ad-hoc scripts instead of using structured primitives.

The fix: make the daemon enforce the lifecycle structurally, not just suggest it.

---

## Design Overview

Two complementary features:

| Feature | What it does | Who uses it |
|---------|-------------|-------------|
| `legion advance <issueId>` | CLI command: moves an issue to its next lifecycle stage | Controller, humans |
| Auto-progression | Daemon auto-dispatches next worker when current one finishes | Daemon (no controller needed) |

These are independent but complementary. Auto-progression is the long-term goal; `advance` is the escape hatch and the building block.

---

## Feature 1: `legion advance <issueId> [--stage <stage>]`

### Semantics

`advance` is a **high-level lifecycle command** that:
1. Reads the current `suggestedAction` from the daemon's issue state cache
2. Executes that action (dispatch worker, transition status, or both)
3. Returns the result

It is **not** a thin wrapper around `dispatch`. It understands the full action space including `transition_to_*` actions that don't dispatch workers.

### CLI Interface

```
legion advance <issueId> [options]

Arguments:
  issueId           Issue identifier (e.g., sjawhar-legion-494)

Options:
  --stage <stage>   Force advance to a specific stage (architect|plan|implement|test|review|merge)
  --repo <owner/repo>  Repository (required if not in state cache)
  --dry-run         Print what would happen without doing it
  --daemon-port <n> Override daemon port
```

### Behavior

**Default (no `--stage`):**
1. Fetch `suggestedAction` from `GET /workers/:id/status` or the state cache via a new `GET /state/issue/:issueId` endpoint
2. Execute the action:
   - `dispatch_*` → POST /workers with the appropriate mode
   - `transition_to_*` → POST /state/advance (new endpoint, see below)
   - `resume_*` → POST /workers with mode + resume prompt
   - `skip` → print "Issue is not ready to advance: <reason>" and exit 0
   - `investigate_no_pr` → print actionable message and exit 1
   - `retry_ci_check` → print "CI is pending, retry later" and exit 0

**With `--stage`:**
- Bypasses `suggestedAction` and forces dispatch of the specified mode
- Equivalent to `legion dispatch <issueId> <stage> --force` but with better UX
- Daemon still enforces hard gates (e.g., merge requires retro)

### New HTTP Endpoint: `POST /state/advance`

```typescript
// Request
{
  issueId: string;
  repo?: string;  // owner/repo, required if not in cache
}

// Response (200)
{
  action: ActionType;
  executed: "dispatched" | "transitioned" | "skipped" | "error";
  workerId?: string;       // if dispatched
  sessionId?: string;      // if dispatched
  port?: number;           // if dispatched
  newStatus?: string;      // if transitioned
  reason?: string;         // if skipped or error
}

// Response (409) — worker already running
{
  error: "worker_already_running";
  workerId: string;
  sessionId: string;
}

// Response (412) — issue not in state cache
{
  error: "issue_not_in_cache";
  message: "Run 'legion collect-state' first or provide --repo";
}
```

### What `POST /state/advance` does server-side

```
1. Look up issueId in issueStateCache
2. If not found → 412
3. Compute suggestedAction from cached state
4. Switch on action:
   a. dispatch_* → call existing POST /workers logic (reuse createAndRegisterWorker)
   b. transition_to_* → call issue tracker to move status + remove worker-done label
   c. resume_* → call existing POST /workers logic with resume prompt
   d. skip / retry_* → return {executed: "skipped", reason: ...}
   e. investigate_no_pr → return {executed: "error", reason: ...}
5. Return result
```

**Key insight:** The server already has all the dispatch logic in `POST /workers`. `advance` reuses it by computing the mode from `ACTION_TO_MODE[suggestedAction]` and calling the same internal function.

### Issue Tracker Transitions

Currently the daemon has no method to move an issue's status in the tracker. This needs to be added to the `IssueTracker` interface:

```typescript
// packages/daemon/src/state/backends/issue-tracker.ts
export interface IssueTracker {
  parseIssues(raw: unknown): ParsedIssue[];
  
  // NEW: Move issue to a new status in the tracker
  // Returns the new canonical status string
  transitionIssue(issueId: string, newStatus: IssueStatusLiteral): Promise<void>;
  
  // NEW: Remove a label from an issue
  removeLabel(issueId: string, label: string): Promise<void>;
}
```

For GitHub backend, `transitionIssue` maps to `gh project item-edit --field-id Status --single-select-option-id <id>` (or the GraphQL equivalent). For Linear, it maps to `linear_linear(action="update", status=...)`.

**Transition map** (what `transition_to_*` actions do):

| Action | Status change | Label change |
|--------|--------------|-------------|
| `transition_to_todo` | → Todo | remove `worker-done` |
| `transition_to_in_progress` | → In Progress | remove `worker-done` |
| `transition_to_testing` | → Testing | remove `worker-done` |
| `transition_to_needs_review` | → Needs Review | remove `worker-done` |
| `transition_to_retro` | → Retro | remove `worker-done` |
| `transition_to_done` | → Done | remove `worker-done` |

### CLI Implementation

```typescript
// packages/daemon/src/cli/index.ts

export async function cmdAdvance(
  issue: string,
  opts: AdvanceOptions
): Promise<void> {
  // If --stage provided, delegate to cmdDispatch with --force
  if (opts.stage) {
    return cmdDispatch(issue, opts.stage, { ...opts, force: true });
  }
  
  // Otherwise, call POST /state/advance
  const response = await fetch(`${baseUrl}/state/advance`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ issueId: issue, repo: opts.repo }),
  });
  
  // Handle responses and print human-readable output
  // ...
}

export const advanceCommand = defineCommand({
  meta: { name: "advance", description: "Advance an issue to its next lifecycle stage" },
  args: {
    issue: { type: "positional", description: "Issue identifier", required: true },
    stage: { type: "string", description: "Force advance to specific stage" },
    repo: { type: "string", alias: "r", description: "Repository (owner/repo)" },
    "dry-run": { type: "boolean", description: "Print action without executing", default: false },
    "daemon-port": { type: "string", description: "Override daemon port" },
  },
  async run({ args }) { ... }
});
```

---

## Feature 2: Auto-Progression

### Semantics

When a worker signals completion (adds `worker-done` label), the daemon automatically dispatches the next worker — without waiting for the controller to poll and act.

The controller's role shifts from **routing every transition** to **handling exceptions** (blocked issues, quality concerns, priority changes).

### Trigger Mechanism

Workers signal completion by:
1. Adding `worker-done` label to the issue (GitHub)
2. Removing `worker-active` label
3. Publishing an Envoy message: `notifications.role.legion-controller`

The daemon already receives Envoy events via the envoy-plugin. The auto-progression hook listens for the `worker-done` signal.

**Two trigger paths:**

**Path A: Envoy event (fast path)**
- Worker publishes `notifications.role.legion-controller` with `"Worker done: <issueId> <mode> completed"`
- Daemon's Envoy subscriber receives this
- Daemon calls `POST /state/advance` internally for that issueId

**Path B: Periodic poll (fallback)**
- Existing health tick loop (60s interval) already calls `POST /state/collect`
- After collection, daemon checks for issues with `suggestedAction` that are actionable
- Dispatches workers for those issues automatically

Path A is the fast path (seconds). Path B is the safety net (up to 60s delay).

### Auto-Progression Logic

```typescript
// In server.ts, after state collection:
async function autoAdvanceReadyIssues(state: CollectedState): Promise<void> {
  const AUTO_ADVANCE_ACTIONS: ReadonlySet<ActionType> = new Set([
    "dispatch_architect",
    "dispatch_planner", 
    "dispatch_implementer",
    "dispatch_implementer_for_retro",
    "dispatch_tester",
    "dispatch_reviewer",
    "dispatch_merger",
    "transition_to_todo",
    "transition_to_in_progress",
    "transition_to_testing",
    "transition_to_needs_review",
    "transition_to_retro",
  ]);
  
  for (const [issueId, issueState] of Object.entries(state.issues)) {
    if (!AUTO_ADVANCE_ACTIONS.has(issueState.suggestedAction)) continue;
    if (issueState.hasLiveWorker) continue;  // already running
    
    // Fire-and-forget advance (log errors, don't throw)
    advanceIssue(issueId, issueState).catch(err => {
      console.error(`[auto-advance] ${issueId}: ${err.message}`);
    });
  }
}
```

### Configuration: Opt-In vs Opt-Out

Auto-progression is **opt-in** initially, controlled by a daemon config flag:

```typescript
// DaemonConfig
autoAdvance?: boolean;  // default: false (opt-in)
```

Environment variable: `LEGION_AUTO_ADVANCE=true`

This lets teams adopt it incrementally. The controller skill can be updated to stop manually routing when `autoAdvance` is enabled.

### Interaction with Controller

When auto-progression is enabled:
- Controller still runs (it handles exceptions, triage, priority)
- Controller no longer needs to call `legion dispatch` for normal flow
- Controller can still call `legion advance` to force-advance or intervene
- Controller's `suggestAction` loop becomes: "look for anomalies, not normal transitions"

The controller skill should be updated to check `LEGION_AUTO_ADVANCE` and skip normal dispatch routing when it's enabled.

---

## Architecture Diagram

```
Worker finishes
    │
    ├─► adds worker-done label
    ├─► removes worker-active label  
    └─► publishes Envoy: notifications.role.legion-controller
              │
              ▼
    Daemon Envoy subscriber
              │
              ▼
    autoAdvanceIssue(issueId)
              │
              ▼
    Read issueStateCache[issueId]
              │
              ▼
    suggestedAction?
    ├─ dispatch_* ──────────────► POST /workers (existing logic)
    ├─ transition_to_* ─────────► issueTracker.transitionIssue() + removeLabel()
    ├─ resume_* ────────────────► POST /workers with resume prompt
    └─ skip/retry/investigate ──► log + notify controller
```

---

## Files to Change

| File | Change |
|------|--------|
| `packages/daemon/src/state/backends/issue-tracker.ts` | Add `transitionIssue()` and `removeLabel()` to interface |
| `packages/daemon/src/state/backends/github.ts` | Implement `transitionIssue()` and `removeLabel()` |
| `packages/daemon/src/state/backends/linear.ts` | Implement `transitionIssue()` and `removeLabel()` |
| `packages/daemon/src/daemon/server.ts` | Add `POST /state/advance` endpoint; add `autoAdvanceReadyIssues()`; wire Envoy trigger |
| `packages/daemon/src/daemon/config.ts` | Add `autoAdvance?: boolean` to `DaemonConfig` |
| `packages/daemon/src/cli/index.ts` | Add `cmdAdvance()` and `advanceCommand`; register in `mainCommand` |
| `.Claude/skills/legion-controller/SKILL.md` | Update to use `legion advance`; document auto-progression behavior |

---

## Key Design Decisions

### 1. `advance` reuses existing dispatch logic

The server already has a well-tested `POST /workers` path. `POST /state/advance` is a thin orchestration layer on top — it computes the right mode and calls the same internal function. No duplication.

### 2. `transition_to_*` actions need tracker integration

Currently the daemon never moves issue status in the tracker — that's done by workers or the controller skill. For `advance` to work end-to-end, the daemon needs this capability. The `IssueTracker` interface extension is the right place.

### 3. Auto-progression is opt-in

Changing the default behavior of the controller loop is high-risk. Opt-in via `LEGION_AUTO_ADVANCE=true` lets teams test it without breaking existing workflows.

### 4. Envoy fast path + poll fallback

The Envoy fast path gives sub-second response to worker completion. The poll fallback ensures correctness even if Envoy delivery fails. Both paths call the same `autoAdvanceIssue()` function.

### 5. `--stage` is `--force` dispatch

`legion advance --stage merge` is equivalent to `legion dispatch <issue> merge --force`. The `advance` command is the user-facing primitive; `dispatch` remains the low-level escape hatch.

---

## Out of Scope (Future)

- **Per-issue workflow customization** (issue #494 mentions this as "future"): storing custom workflow in issue metadata. Not in this design.
- **Backward-skipping** (review → implement for changes requested): this already works via `resume_implementer_for_changes` in the existing state machine. `advance` will handle it via the `resume_*` action path.

---

## Open Questions

1. **GitHub status transition API**: The GitHub Projects V2 API requires knowing the field ID and option ID for status values. Does the daemon already have this? If not, we need a one-time fetch of project field metadata. The `github.ts` backend may need a `getProjectFields()` helper.

2. **Linear `transitionIssue`**: Linear uses workflow state IDs, not names. Same question — does the daemon have these cached?

3. **Envoy subscriber in daemon**: Does the daemon currently subscribe to Envoy topics? If not, we need to add subscription logic in `index.ts` or `server.ts`. The envoy-plugin handles this for workers, but the daemon itself may not be subscribed.

4. **Race condition**: If two workers finish simultaneously for the same issue (unlikely but possible), both Envoy events would trigger `autoAdvanceIssue`. The existing `hasLiveWorker` check + 409 from `POST /workers` handles this gracefully.

---

## Handoff to Planner

The planner should produce a step-by-step implementation plan covering:

1. `IssueTracker` interface extension + GitHub/Linear implementations
2. `POST /state/advance` server endpoint
3. `autoAdvanceReadyIssues()` + Envoy trigger wiring
4. `DaemonConfig.autoAdvance` flag
5. `cmdAdvance` + `advanceCommand` CLI
6. Controller skill update
7. Tests for each component

The open questions about GitHub/Linear status transition APIs should be resolved during planning by reading the existing `github.ts` and `linear.ts` implementations.
