# Fix Deterministic Session IDs via Version Bump Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Resolve issue #84 by standardizing on `--version` for re-dispatch so operators can intentionally generate a new deterministic session ID.

**Architecture:** Keep the existing deterministic seed model (`computeSessionId(teamId, issueId, mode, version)`) and avoid any session deletion semantics. The fix is documentation, operator workflow clarity, and test coverage around versioned dispatch behavior already introduced in PR #83.

**Tech Stack:** TypeScript, Bun runtime, citty CLI, Bun test runner

---

## Scope Decision (Blocking Feedback Applied)

- In scope:
  - Validate and document version-based re-dispatch (`--version N`).
  - Add/adjust tests proving version is threaded end-to-end and changes session ID deterministically.
  - Update issue narrative to describe version bump workflow.
- Explicitly out of scope:
  - No session deletion APIs.
  - No `--fresh` flag.
  - No workspace mismatch detection.
  - No crash-history/workspace tracking additions.

---

## Desired Behavior

1. First dispatch defaults to version `0` (or omitted) and uses base deterministic session ID.
2. Re-dispatch for same issue/mode can pass `--version 1`, `--version 2`, etc.
3. Different version values produce different deterministic session IDs.
4. Same version value remains deterministic across retries.
5. Operator guidance is clear: bump `--version` when a clean session context is needed.

---

### Task 1: Confirm and lock current version threading behavior

**Files:**
- Validate: `packages/daemon/src/cli/index.ts`
- Validate: `packages/daemon/src/daemon/server.ts`
- Validate: `packages/daemon/src/state/types.ts`
- Validate: `packages/daemon/src/state/__tests__/types.test.ts`

**Step 1: Verify CLI argument semantics**

- Confirm `dispatch` accepts `--version`.
- Confirm CLI validates non-negative integer and forwards `version` to `POST /workers`.

**Step 2: Verify daemon threading**

- Confirm `POST /workers` parses `version` and passes it into `computeSessionId(...)`.

**Step 3: Verify deterministic contract tests exist**

- Confirm tests in `state/__tests__/types.test.ts` cover:
  - version `0` equivalence,
  - version differentiation,
  - deterministic same-version output.

---

### Task 2: Add missing tests for version behavior at CLI and daemon boundaries

**Files:**
- Modify: `packages/daemon/src/cli/__tests__/index.test.ts`
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`
- Modify: `packages/daemon/src/daemon/__tests__/session-id-contract.test.ts` (if needed)

**Step 1: CLI dispatch payload tests**

- Add/adjust tests to assert:
  - `--version 2` sends `version: 2` in body.
  - omitted `--version` does not send invalid values.
  - invalid version input is rejected with clear error.

**Step 2: Server session ID version tests**

- Add tests in `server.test.ts` to assert:
  - same `issueId+mode` with different `version` values yields different `sessionId`.
  - same `version` yields same deterministic `sessionId`.

**Step 3: Contract-level coverage (optional if already sufficient)**

- If current contract tests are thin, add one end-to-end contract case from `/workers` request with explicit `version` to expected `computeSessionId(..., version)`.

---

### Task 3: Minimal operator documentation for re-dispatch

**Files:**
- Modify: `packages/daemon/src/cli/index.ts` (arg description text only, if needed)
- Modify: `docs/plans/2026-03-10-fix-deterministic-session-ids.md` (this plan may be final reference)
- Optional docs location (if available in repo): worker/runbook docs referencing dispatch retries

**Step 1: Clarify command help text**

- Ensure `--version` description explains practical use: bump for fresh session context on re-dispatch.

**Step 2: Add concise operator workflow snippet**

- Example:
  - initial: `legion dispatch ENG-42 plan`
  - retry clean context: `legion dispatch ENG-42 plan --version 1`
  - second retry: `legion dispatch ENG-42 plan --version 2`

---

### Task 4: Optional small enhancement - daemon auto-increment fallback (only if trivial)

**Decision gate:** implement only if the code change is small and testable without introducing session deletion logic.

**Files (if implemented):**
- Modify: `packages/daemon/src/daemon/server.ts`
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`

**Behavior candidate:**

- If `version` is omitted and session create returns duplicate conflict path, try `version+1` up to a tiny cap (e.g., +3) and use first successful deterministic ID.
- Keep explicit operator-provided version authoritative (no auto-bump when user specified `version`).

**If not trivial:** skip this task and keep explicit operator bump only.

---

### Task 5: Update issue communication

**Files/Systems:**
- GitHub issue #84 comments/description in `sjawhar/legion`

**Step 1: Post concise resolution strategy comment**

- State that the accepted fix is versioned session IDs (already in PR #83), with tests and operator guidance.
- Explicitly state no session deletion/fresh flag/workspace tracking changes are planned.

**Step 2: Link verification evidence**

- Reference relevant tests and command examples.

---

## Testing Plan

### Targeted

- `bun test packages/daemon/src/state/__tests__/types.test.ts`
- `bun test packages/daemon/src/cli/__tests__/index.test.ts`
- `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
- `bun test packages/daemon/src/daemon/__tests__/session-id-contract.test.ts`

### Quality Gate

- `bunx tsc --noEmit`
- `bun test`

---

## Acceptance Criteria

- Version-based re-dispatch behavior is covered by tests at state, CLI, and daemon boundaries.
- Operator-facing guidance for `--version` is explicit and accurate.
- No session deletion semantics are introduced.
- No workspace mismatch tracking or auto-fresh logic is introduced.
