# Session Adoption Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow standalone OpenCode sessions to be adopted as Legion workers via `POST /workers` with an optional `sessionId` field and a new `legion adopt` CLI command.

**Architecture:** Add an optional `sessionId` field to the existing `POST /workers` endpoint. When provided, the server skips deterministic ID computation and uses the provided ID directly, with format validation and a duplicate-session guard. A new `legion adopt` CLI command scans the OC registry for workspace/PID info, registers the session via the daemon API, and sends SIGHUP to clean up the original process.

**Tech Stack:** TypeScript on Bun, citty CLI, Bun.serve HTTP, Biome lint, jj VCS

**Relevant learnings from prior work:**
1. `docs/solutions/deterministic-session-ids.md`: [Deterministic Session ID Generation | tags: session-management, deterministic-systems, parameter-threading] Session IDs use UUIDv5 from (legionId, issueId, mode, version). Format: `ses_` + 12 hex + 14 Base62. Defense-in-depth validation at every layer boundary.
2. `docs/solutions/daemon/server-side-dispatch-validation.md`: [Server-side dispatch validation | tags: dispatch-validation, pure-functions, gated-modes, escape-hatch] POST /workers handler is ~200 lines. `force === true` strict equality for bypass. Gotcha: `normalizedIssueId` is declared downstream — check for variable conflicts.
3. `docs/solutions/daemon/prompt-delivery-bootstrap-delay.md`: [Session bootstrap delay | tags: dispatch, session-lifecycle, prompt-delivery, retry-pattern] `prompt` field in POST /workers body triggers server-side delay + retry. `SESSION_READY_DELAY_MS` (2s) + 3 retries with exponential backoff. Adopted sessions will hit 409 DuplicateIDError in createSession — this is treated as idempotent success.

**Metis pre-analysis:** Metis timed out (>3 min). Proceeding without pre-analysis. Key risks identified from architect handoff instead — see Concerns section in each relevant task.

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `packages/daemon/src/state/types.ts` | Modify | Export `SESSION_ID_PATTERN` regex constant |
| `packages/daemon/src/state/__tests__/types.test.ts` | Modify | Tests for `SESSION_ID_PATTERN` |
| `packages/daemon/src/daemon/server.ts` | Modify | Accept optional `sessionId` in POST /workers, validate format, duplicate guard, skip computeSessionId when provided |
| `packages/daemon/src/daemon/__tests__/server.test.ts` | Modify | Tests for sessionId parameter, format validation, duplicate-session guard |
| `packages/daemon/src/cli/index.ts` | Modify | Add exported `cmdAdopt()` function, `scanOcRegistry()` helper, `adoptCommand` definition, register in mainCommand |
| `packages/daemon/src/cli/__tests__/index.test.ts` | Modify | Tests for adopt command structure, cmdAdopt behavior (POST body, 409 handling, validation), and OC registry scanning |

---

## Task 1: Export SESSION_ID_PATTERN from types.ts — Independent

**Files:**
- Modify: `packages/daemon/src/state/types.ts:564` (before `computeSessionId`)
- Modify: `packages/daemon/src/state/__tests__/types.test.ts`

- [ ] **Step 1: Write failing test for SESSION_ID_PATTERN**

Add to `packages/daemon/src/state/__tests__/types.test.ts`, after the existing `computeSessionId` tests:

```typescript
import { SESSION_ID_PATTERN } from "../types";

describe("SESSION_ID_PATTERN", () => {
  it("matches valid session IDs from computeSessionId", () => {
    const id = computeSessionId("sjawhar/5", "gh-42", "implement");
    expect(SESSION_ID_PATTERN.test(id)).toBe(true);
  });

  it("matches known valid session IDs", () => {
    expect(SESSION_ID_PATTERN.test("ses_31617365bffeUEa4wPBVIL2LBI")).toBe(true);
    expect(SESSION_ID_PATTERN.test("ses_5f6e229e023c20L4w2B1RNa3WZ")).toBe(true);
  });

  it("rejects strings that are too short", () => {
    expect(SESSION_ID_PATTERN.test("ses_abc")).toBe(false);
  });

  it("rejects strings without ses_ prefix", () => {
    expect(SESSION_ID_PATTERN.test("31617365bffeUEa4wPBVIL2LBI")).toBe(false);
  });

  it("rejects empty string", () => {
    expect(SESSION_ID_PATTERN.test("")).toBe(false);
  });

  it("rejects uppercase hex portion", () => {
    // Hex portion (first 12 after ses_) must be lowercase
    expect(SESSION_ID_PATTERN.test("ses_31617365BFFEUEa4wPBVIL2LBI")).toBe(false);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/state/__tests__/types.test.ts --filter "SESSION_ID_PATTERN"`
Expected: FAIL — `SESSION_ID_PATTERN` is not exported from types.ts

- [ ] **Step 3: Export SESSION_ID_PATTERN from types.ts**

In `packages/daemon/src/state/types.ts`, insert before the `computeSessionId` function (before the JSDoc comment at line 553):

```typescript
/**
 * Session ID format regex.
 * Matches OpenCode format: ses_ + 12 lowercase hex chars + 14 Base62 chars.
 */
export const SESSION_ID_PATTERN = /^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/;
```

- [ ] **Step 4: Run test to verify it passes**

Run: `bun test packages/daemon/src/state/__tests__/types.test.ts --filter "SESSION_ID_PATTERN"`
Expected: All 6 tests PASS

- [ ] **Step 5: Describe and advance**

```bash
jj describe -m "feat(types): export SESSION_ID_PATTERN regex for session ID validation"
jj new
```

---

## Task 2: Add sessionId format validation to POST /workers — Depends on: Task 1

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts:9,688-711`
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`

- [ ] **Step 1: Write failing test for 422 on invalid sessionId**

Add to `packages/daemon/src/daemon/__tests__/server.test.ts`, inside the existing `POST /workers` describe block:

```typescript
it("returns 422 for invalid sessionId format", async () => {
  await startTestServer();
  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-42",
      mode: "implement",
      workspace: "/tmp/work",
      sessionId: "invalid_format",
    }),
  });
  expect(response.status).toBe(422);
  const body = (await response.json()) as Record<string, unknown>;
  expect(body.error).toBe("invalid_session_id");
});

it("returns 422 for sessionId with wrong type", async () => {
  await startTestServer();
  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-42",
      mode: "implement",
      workspace: "/tmp/work",
      sessionId: 12345,
    }),
  });
  expect(response.status).toBe(422);
  const body = (await response.json()) as Record<string, unknown>;
  expect(body.error).toBe("invalid_session_id");
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "422 for"`
Expected: FAIL — server returns 200 (sessionId is currently ignored)

- [ ] **Step 3: Add sessionId extraction and validation to server.ts**

In `packages/daemon/src/daemon/server.ts`:

1. Add `SESSION_ID_PATTERN` to the import from `../../state/types` (line 9):
```typescript
import {
  // ... existing imports
  computeSessionId,
  SESSION_ID_PATTERN,
  // ... rest
} from "../../state/types";
```

2. After extracting `prompt` at line 690, add extraction of `sessionId`:
```typescript
            const prompt = payload.prompt;
            const providedSessionId = payload.sessionId;
```

3. After mode validation (after line 711), add sessionId format validation:
```typescript
            if (
              providedSessionId !== undefined &&
              (typeof providedSessionId !== "string" ||
                !SESSION_ID_PATTERN.test(providedSessionId))
            ) {
              return jsonResponse(
                {
                  error: "invalid_session_id",
                  message:
                    "sessionId must match format: ses_ + 12 hex + 14 Base62",
                },
                422
              );
            }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "422 for"`
Expected: Both tests PASS

- [ ] **Step 5: Describe and advance**

```bash
jj describe -m "feat(server): validate sessionId format in POST /workers (422 on invalid)"
jj new
```

---

## Task 3: Add duplicate session guard to POST /workers — Depends on: Task 2

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts` (after sessionId validation, before phase prerequisite check)
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`

- [ ] **Step 1: Write failing test for duplicate session adoption**

Add to `packages/daemon/src/daemon/__tests__/server.test.ts`:

```typescript
it("returns 409 session_already_adopted when sessionId is tracked by live worker", async () => {
  await startTestServer();
  // Create a normal worker first
  const first = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-42",
      mode: "implement",
      workspace: "/tmp/work",
    }),
  });
  expect(first.status).toBe(200);
  const firstBody = (await first.json()) as { sessionId: string };

  // Try to adopt using the same sessionId under different issue+mode
  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-99",
      mode: "plan",
      workspace: "/tmp/work",
      sessionId: firstBody.sessionId,
    }),
  });
  expect(response.status).toBe(409);
  const body = (await response.json()) as Record<string, unknown>;
  expect(body.error).toBe("session_already_adopted");
  expect(body.id).toBe("eng-42-implement");
});

it("allows adoption when existing worker with same sessionId is dead", async () => {
  await startTestServer();
  // Create a worker, then kill it
  const first = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-42",
      mode: "implement",
      workspace: "/tmp/work",
    }),
  });
  expect(first.status).toBe(200);
  const firstBody = (await first.json()) as { id: string; sessionId: string };

  // Mark it dead
  await requestJson(`/workers/${firstBody.id}`, {
    method: "PATCH",
    body: JSON.stringify({ status: "dead" }),
  });

  // Now adopt with the same sessionId under different issue+mode
  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-99",
      mode: "plan",
      workspace: "/tmp/work",
      sessionId: firstBody.sessionId,
    }),
  });
  expect(response.status).toBe(200);
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "session_already_adopted|allows adoption when"`
Expected: First test FAIL (server returns 200 instead of 409), second test may pass already

- [ ] **Step 3: Add duplicate session guard to server.ts**

In `packages/daemon/src/daemon/server.ts`, after the sessionId format validation (from Task 2) and before the `normalizedIssueId` declaration at line 714, add:

```typescript
            // Duplicate session guard: prevent same session from being tracked by multiple workers
            if (typeof providedSessionId === "string") {
              for (const [, existingEntry] of workers) {
                if (
                  existingEntry.status !== "dead" &&
                  existingEntry.sessionId === providedSessionId
                ) {
                  return jsonResponse(
                    {
                      error: "session_already_adopted",
                      id: existingEntry.id,
                      sessionId: providedSessionId,
                    },
                    409
                  );
                }
              }
            }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "session_already_adopted|allows adoption when"`
Expected: Both tests PASS

- [ ] **Step 5: Describe and advance**

```bash
jj describe -m "feat(server): add duplicate session guard to prevent double-adoption"
jj new
```

---

## Task 4: Add sessionId override to skip computeSessionId — Depends on: Task 2

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts:842-847`
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`

- [ ] **Step 1: Write failing test for sessionId override**

Add to `packages/daemon/src/daemon/__tests__/server.test.ts`:

```typescript
it("uses provided sessionId instead of computing one", async () => {
  await startTestServer();
  const customSessionId = "ses_31617365bffeUEa4wPBVIL2LBI";
  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-42",
      mode: "implement",
      workspace: "/tmp/work",
      sessionId: customSessionId,
    }),
  });
  expect(response.status).toBe(200);
  const body = (await response.json()) as { id: string; sessionId: string };
  expect(body.sessionId).toBe(customSessionId);
  expect(createSessionCalls[0].sessionId).toBe(customSessionId);
});

it("computes sessionId normally when sessionId not provided", async () => {
  await startTestServer();
  const response = await requestJson("/workers", {
    method: "POST",
    body: JSON.stringify({
      issueId: "ENG-42",
      mode: "implement",
      workspace: "/tmp/work",
    }),
  });
  expect(response.status).toBe(200);
  const body = (await response.json()) as { sessionId: string };
  expect(body.sessionId).toBe(computeSessionId(legionId, "eng-42", "implement"));
});
```

- [ ] **Step 2: Run tests to verify the override test fails**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "uses provided sessionId"`
Expected: FAIL — sessionId is computed, not the provided one

- [ ] **Step 3: Replace computeSessionId call with conditional override**

In `packages/daemon/src/daemon/server.ts`, replace lines 842-847:

**Before:**
```typescript
            const sessionId = computeSessionId(
              opts.legionId,
              issueId,
              mode as WorkerModeLiteral,
              version
            );
```

**After:**
```typescript
            const sessionId =
              typeof providedSessionId === "string"
                ? providedSessionId
                : computeSessionId(
                    opts.legionId,
                    issueId,
                    mode as WorkerModeLiteral,
                    version
                  );
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "uses provided sessionId|computes sessionId normally"`
Expected: Both tests PASS

- [ ] **Step 5: Run full server test suite to verify no regressions**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`
Expected: All existing tests PASS (unchanged behavior when sessionId is absent)

- [ ] **Step 6: Describe and advance**

```bash
jj describe -m "feat(server): skip computeSessionId when sessionId provided in POST /workers"
jj new
```

---

## Task 5: Add `legion adopt` CLI command — Depends on: Task 3, Task 4

**Files:**
- Modify: `packages/daemon/src/cli/index.ts`
- Modify: `packages/daemon/src/cli/__tests__/index.test.ts`

### Part A: OC registry scanner helper

- [ ] **Step 1: Write failing test for scanOcRegistry**

Add to `packages/daemon/src/cli/__tests__/index.test.ts`:

```typescript
import { scanOcRegistry } from "../index";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";

describe("scanOcRegistry", () => {
  let tempDir: string;

  beforeEach(async () => {
    tempDir = await mkdtemp(join(tmpdir(), "oc-registry-"));
  });

  afterEach(async () => {
    await rm(tempDir, { recursive: true, force: true });
  });

  it("finds matching session entry", async () => {
    const entry = {
      pid: 12345,
      port: 41089,
      dir: "/home/ubuntu/agent-c/google",
      started: "2026-03-14T20:43:57+00:00",
      session: { id: "ses_316beec6dffevTRQ4mUzpuleS6", title: "Test" },
    };
    await writeFile(join(tempDir, "test.json"), JSON.stringify(entry));

    const result = await scanOcRegistry("ses_316beec6dffevTRQ4mUzpuleS6", tempDir);
    expect(result).toEqual({ pid: 12345, dir: "/home/ubuntu/agent-c/google" });
  });

  it("returns null when no match found", async () => {
    const entry = {
      pid: 12345,
      dir: "/tmp",
      session: { id: "ses_other000000000000000000" },
    };
    await writeFile(join(tempDir, "test.json"), JSON.stringify(entry));

    const result = await scanOcRegistry("ses_316beec6dffevTRQ4mUzpuleS6", tempDir);
    expect(result).toBeNull();
  });

  it("returns null when directory does not exist", async () => {
    const result = await scanOcRegistry("ses_316beec6dffevTRQ4mUzpuleS6", "/nonexistent");
    expect(result).toBeNull();
  });

  it("skips malformed JSON files", async () => {
    await writeFile(join(tempDir, "bad.json"), "not json");
    const entry = {
      pid: 99,
      dir: "/good",
      session: { id: "ses_316beec6dffevTRQ4mUzpuleS6" },
    };
    await writeFile(join(tempDir, "good.json"), JSON.stringify(entry));

    const result = await scanOcRegistry("ses_316beec6dffevTRQ4mUzpuleS6", tempDir);
    expect(result).toEqual({ pid: 99, dir: "/good" });
  });

  it("skips entries with missing pid or dir", async () => {
    const entry = {
      session: { id: "ses_316beec6dffevTRQ4mUzpuleS6" },
      // missing pid and dir
    };
    await writeFile(join(tempDir, "test.json"), JSON.stringify(entry));

    const result = await scanOcRegistry("ses_316beec6dffevTRQ4mUzpuleS6", tempDir);
    expect(result).toBeNull();
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `bun test packages/daemon/src/cli/__tests__/index.test.ts --filter "scanOcRegistry"`
Expected: FAIL — `scanOcRegistry` is not exported

- [ ] **Step 3: Implement scanOcRegistry in cli/index.ts**

Add these imports at the top of `packages/daemon/src/cli/index.ts`:

```typescript
import { readdir, readFile } from "node:fs/promises";
```

Note: `join` from `node:path` should already be imported. If not, add it.

Add the following **exported** function (before the command definitions section, e.g., after `cmdDispatch`):

```typescript
interface OcRegistryEntry {
  pid: number;
  dir: string;
}

/**
 * Scan the OC registry for a session entry.
 * @param sessionId - session ID to search for
 * @param registryDir - override for testing (default: /run/user/$UID/opencode-$UID)
 */
export async function scanOcRegistry(
  sessionId: string,
  registryDir?: string
): Promise<OcRegistryEntry | null> {
  const dir =
    registryDir ??
    (() => {
      const uid = process.getuid?.();
      if (uid === undefined) return null;
      return `/run/user/${uid}/opencode-${uid}`;
    })();
  if (!dir) return null;

  let files: string[];
  try {
    files = await readdir(dir);
  } catch {
    return null;
  }

  for (const file of files) {
    if (!file.endsWith(".json")) continue;
    try {
      const content = await readFile(join(dir, file), "utf-8");
      const entry = JSON.parse(content) as Record<string, unknown>;
      const session = entry.session as Record<string, unknown> | undefined;
      if (session?.id === sessionId) {
        const pid = typeof entry.pid === "number" ? entry.pid : undefined;
        const entryDir = typeof entry.dir === "string" ? entry.dir : undefined;
        if (pid !== undefined && entryDir !== undefined) {
          return { pid, dir: entryDir };
        }
      }
    } catch {
      continue;
    }
  }

  return null;
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `bun test packages/daemon/src/cli/__tests__/index.test.ts --filter "scanOcRegistry"`
Expected: All 5 tests PASS

- [ ] **Step 5: Describe and advance**

```bash
jj describe -m "feat(cli): add scanOcRegistry helper for OC process registry lookup"
jj new
```

### Part B: cmdAdopt function and adoptCommand definition

- [ ] **Step 6: Write failing tests for adoptCommand structure and cmdAdopt behavior**

Add to `packages/daemon/src/cli/__tests__/index.test.ts`:

```typescript
import { adoptCommand, cmdAdopt } from "../index";

describe("adoptCommand", () => {
  it("has correct meta", () => {
    expect(adoptCommand.meta?.name).toBe("adopt");
  });

  it("requires team and session as positional args", () => {
    const args = adoptCommand.args!;
    expect(args.team).toEqual(
      expect.objectContaining({ type: "positional", required: true })
    );
    expect(args.session).toEqual(
      expect.objectContaining({ type: "positional", required: true })
    );
  });

  it("requires --mode and --issue flags", () => {
    const args = adoptCommand.args!;
    expect(args.mode).toEqual(
      expect.objectContaining({ type: "string", required: true })
    );
    expect(args.issue).toEqual(
      expect.objectContaining({ type: "string", required: true })
    );
  });

  it("has optional --workspace flag", () => {
    const args = adoptCommand.args!;
    expect(args.workspace).toEqual(
      expect.objectContaining({ type: "string" })
    );
    expect(args.workspace!.required).toBeFalsy();
  });
});

describe("cmdAdopt behavior", () => {
  const originalFetch = globalThis.fetch;
  let capturedCalls: Array<{ url: string; body: Record<string, unknown> }>;

  beforeEach(() => {
    capturedCalls = [];
    const mockFn = async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/health")) {
        return new Response(JSON.stringify({ status: "ok" }), { status: 200 });
      }
      if (url.includes("/workers") && init?.method === "POST") {
        const body = JSON.parse(init.body as string);
        capturedCalls.push({ url, body });
        return new Response(
          JSON.stringify({
            id: `${body.issueId}-${body.mode}`,
            port: 13381,
            sessionId: body.sessionId,
            promptDelivered: true,
          }),
          { status: 200 }
        );
      }
      return originalFetch(input, init);
    };
    globalThis.fetch = Object.assign(mockFn, {
      preconnect: originalFetch.preconnect,
    });
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it("sends sessionId, force=true, and prompt in POST body", async () => {
    const testSession = "ses_31617365bffeUEa4wPBVIL2LBI";
    await cmdAdopt("sjawhar/5", testSession, {
      mode: "implement",
      issue: "eng-42",
      workspace: "/tmp/test-workspace",
      daemonPort: 13370,
    });

    expect(capturedCalls).toHaveLength(1);
    expect(capturedCalls[0].body).toEqual(
      expect.objectContaining({
        issueId: "eng-42",
        mode: "implement",
        sessionId: testSession,
        force: true,
        prompt: "/legion-worker implement mode for eng-42",
        workspace: "/tmp/test-workspace",
      })
    );
  });

  it("throws CliError for session_already_adopted 409", async () => {
    // Override mock to return 409
    const mock409 = async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/health")) {
        return new Response(JSON.stringify({ status: "ok" }), { status: 200 });
      }
      if (url.includes("/workers") && init?.method === "POST") {
        return new Response(
          JSON.stringify({ error: "session_already_adopted", id: "eng-42-implement" }),
          { status: 409 }
        );
      }
      return originalFetch(input, init);
    };
    globalThis.fetch = Object.assign(mock409, {
      preconnect: originalFetch.preconnect,
    });

    await expect(
      cmdAdopt("sjawhar/5", "ses_31617365bffeUEa4wPBVIL2LBI", {
        mode: "implement",
        issue: "eng-42",
        workspace: "/tmp/work",
        daemonPort: 13370,
      })
    ).rejects.toThrow("already tracked by worker");
  });

  it("throws CliError for invalid session ID format", async () => {
    await expect(
      cmdAdopt("sjawhar/5", "not-a-session-id", {
        mode: "implement",
        issue: "eng-42",
        workspace: "/tmp/work",
        daemonPort: 13370,
      })
    ).rejects.toThrow("Invalid session ID format");
  });
});
```

- [ ] **Step 7: Run tests to verify they fail**

Run: `bun test packages/daemon/src/cli/__tests__/index.test.ts --filter "adoptCommand|cmdAdopt"`
Expected: FAIL — `adoptCommand` and `cmdAdopt` are not exported

- [ ] **Step 8: Implement cmdAdopt and adoptCommand**

Add the `SESSION_ID_PATTERN` import at the top of `packages/daemon/src/cli/index.ts`:

```typescript
import { SESSION_ID_PATTERN } from "../state/types";
```

Add the **exported** `cmdAdopt` function (after `cmdDispatch`, before command definitions):

```typescript
interface AdoptOptions {
  mode: string;
  issue: string;
  workspace?: string;
  daemonPort?: number;
}

export async function cmdAdopt(
  team: string,
  session: string,
  opts: AdoptOptions
): Promise<void> {
  if (!SESSION_ID_PATTERN.test(session)) {
    throw new CliError(
      `Invalid session ID format: ${session}\nExpected: ses_ + 12 hex + 14 Base62`
    );
  }
  if (!SAFE_IDENTIFIER_RE.test(opts.issue)) {
    throw new CliError(
      `Invalid issue identifier: ${opts.issue} (must match [a-zA-Z0-9_-]+)`
    );
  }
  if (!SAFE_IDENTIFIER_RE.test(opts.mode)) {
    throw new CliError(
      `Invalid mode: ${opts.mode} (must match [a-zA-Z0-9_-]+)`
    );
  }

  const legionId = await resolveLegionId(team);
  const daemonPort = opts.daemonPort ?? (await getDaemonPort(legionId));
  const baseUrl = `http://127.0.0.1:${daemonPort}`;

  // Health check
  try {
    const healthResp = await fetch(`${baseUrl}/health`);
    if (!healthResp.ok) {
      throw new CliError("Daemon is not healthy. Is it running?");
    }
  } catch (error) {
    if (error instanceof CliError) throw error;
    throw new CliError(
      `Could not connect to daemon. Is it running?\nTried: ${baseUrl}/health`
    );
  }

  // Resolve workspace from OC registry if not provided
  let workspace = opts.workspace;
  let registryPid: number | undefined;
  if (!workspace) {
    const registryEntry = await scanOcRegistry(session);
    if (registryEntry) {
      workspace = registryEntry.dir;
      registryPid = registryEntry.pid;
      console.log(`Resolved workspace from OC registry: ${workspace}`);
    }
  }

  if (!workspace) {
    throw new CliError(
      "Could not resolve workspace. Provide --workspace or ensure the session is in the OC registry."
    );
  }

  // POST /workers with sessionId and force=true
  const body: Record<string, unknown> = {
    issueId: opts.issue,
    mode: opts.mode,
    workspace,
    sessionId: session,
    force: true,
    prompt: `/legion-worker ${opts.mode} mode for ${opts.issue}`,
  };

  let response: Response;
  try {
    response = await fetch(`${baseUrl}/workers`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch {
    throw new CliError(
      `Could not connect to daemon. Is it running?\nTried: ${baseUrl}/workers`
    );
  }

  let responseBody: Record<string, unknown>;
  try {
    responseBody = (await response.json()) as Record<string, unknown>;
  } catch {
    throw new CliError(
      `Daemon returned non-JSON response (status ${response.status})`
    );
  }

  if (response.status === 409) {
    if (responseBody.error === "session_already_adopted") {
      throw new CliError(
        `Session ${session} is already tracked by worker ${responseBody.id}`
      );
    }
    console.log(`Worker already running: ${responseBody.id}`);
    console.log(`  port: ${responseBody.port}`);
    console.log(`  session: ${responseBody.sessionId}`);
    return;
  }

  if (response.status === 422) {
    throw new CliError(`Invalid request: ${JSON.stringify(responseBody)}`);
  }

  if (!response.ok) {
    throw new CliError(`Failed to adopt: ${JSON.stringify(responseBody)}`);
  }

  const workerId = responseBody.id as string;
  const workerPort = responseBody.port as number;
  const adoptedSessionId = responseBody.sessionId as string;

  console.log(`Session adopted: ${workerId}`);
  console.log(`  port: ${workerPort}`);
  console.log(`  session: ${adoptedSessionId}`);

  if (responseBody.promptDelivered === true) {
    console.log(
      `Prompt sent: /legion-worker ${opts.mode} mode for ${opts.issue}`
    );
  } else if (responseBody.promptDelivered === false) {
    console.warn("Session adopted but prompt delivery failed. Send manually:");
    console.warn(
      `  legion prompt ${opts.issue} "/legion-worker ${opts.mode} mode for ${opts.issue}"`
    );
  }

  // SIGHUP to original process (best-effort, transient)
  if (registryPid) {
    try {
      process.kill(registryPid, "SIGHUP");
      console.log(`Sent SIGHUP to original process (PID ${registryPid})`);
    } catch (error) {
      console.warn(
        `Could not send SIGHUP to PID ${registryPid}: ${(error as Error).message}`
      );
    }
  }

  console.log(`\nTo attach: legion attach ${team} ${opts.issue}`);
}
```

Add the `adoptCommand` definition (after `attachCommand`, before `legionsCommand`):

```typescript
export const adoptCommand = defineCommand({
  meta: {
    name: "adopt",
    description: "Adopt an existing OpenCode session as a Legion worker",
  },
  args: {
    team: {
      type: "positional",
      description: "Legion key or ID (e.g., sjawhar/5)",
      required: true,
    },
    session: {
      type: "positional",
      description: "OpenCode session ID (e.g., ses_...)",
      required: true,
    },
    mode: {
      type: "string",
      alias: "m",
      description:
        "Worker mode (architect, plan, implement, test, review, merge)",
      required: true,
    },
    issue: {
      type: "string",
      alias: "i",
      description: "Issue identifier (e.g., eng-21, gh-42)",
      required: true,
    },
    workspace: {
      type: "string",
      alias: "w",
      description:
        "Override workspace path (default: resolved from OC registry)",
    },
  },
  async run({ args }) {
    try {
      await cmdAdopt(args.team, args.session, {
        mode: args.mode as string,
        issue: args.issue as string,
        workspace: args.workspace as string | undefined,
      });
    } catch (e) {
      if (e instanceof CliError) {
        console.error(e.message);
        process.exit(e.code);
      }
      throw e;
    }
  },
});
```

Register in `mainCommand.subCommands` (add alphabetically after `attach`):

```typescript
  subCommands: {
    start: startCommand,
    stop: stopCommand,
    status: statusCommand,
    attach: attachCommand,
    adopt: adoptCommand,
    dispatch: dispatchCommand,
    prompt: promptCommand,
    "reset-crashes": resetCrashesCommand,
    teams: legionsCommand,
    "collect-state": collectStateCommand,
    poll: pollCommand,
    handoff: handoffCommand,
  },
```

- [ ] **Step 9: Run tests to verify they pass**

Run: `bun test packages/daemon/src/cli/__tests__/index.test.ts --filter "adoptCommand|cmdAdopt|scanOcRegistry"`
Expected: All tests PASS (metadata + behavior + registry scanner)

- [ ] **Step 10: Describe and advance**

```bash
jj describe -m "feat(cli): add legion adopt command for session adoption"
jj new
```

---

## Task 6: Full verification — Depends on: Task 1, Task 2, Task 3, Task 4, Task 5

**Files:** All modified files from Tasks 1-5

- [ ] **Step 1: Run Biome lint check**

Run: `bunx biome check packages/daemon/src/`
Expected: No errors. If there are formatting issues, fix with: `bunx biome check --write packages/daemon/src/`

- [ ] **Step 2: Run TypeScript type check**

Run: `bunx tsc --noEmit`
Expected: No type errors

- [ ] **Step 3: Verify DuplicateIDError idempotent success (existing tests)**

Run: `bun test packages/daemon/src/daemon/__tests__/serve-manager.test.ts --filter "DuplicateIDError"`
Expected: All existing DuplicateIDError tests pass. This confirms that when `adapter.createSession()` returns 409 with `DuplicateIDError`, it is treated as idempotent success and returns the session ID. Adoption uses the same `createSession()` call path (serve-manager.ts lines 119-129), so this acceptance criterion is satisfied by existing code and tests.

- [ ] **Step 4: Run full test suite**

Run: `bun test`
Expected: All tests pass (existing + new). Note: pre-existing test failures (if any) should be documented but not fixed as part of this change.

- [ ] **Step 5: Describe and advance (only if lint/format fixes were needed)**

```bash
jj describe -m "chore: lint and type-check pass for session adoption feature"
jj new
```

---

## Testing Plan

### Setup
- `bun install` (if not already done)
- Tests run via `bun test` — no external infrastructure needed

### Health Check
- `bun test --help` returns help text (Bun test runner is available)

### Verification Steps

For each acceptance criterion:

1. **sessionId format validation**
   - Action: Run `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "422 for"`
   - Expected: Tests pass confirming 422 response for invalid formats
   - Tool: bun test

2. **sessionId override skips computeSessionId**
   - Action: Run `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "uses provided sessionId"`
   - Expected: Test confirms custom sessionId is used, not computed
   - Tool: bun test

3. **Duplicate session guard**
   - Action: Run `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "session_already_adopted"`
   - Expected: Test confirms 409 when sessionId already tracked by live worker
   - Tool: bun test

4. **Unchanged behavior when sessionId absent**
   - Action: Run `bun test packages/daemon/src/daemon/__tests__/server.test.ts --filter "computes sessionId normally"`
   - Expected: Existing tests pass, computed sessionId matches before
   - Tool: bun test

5. **SESSION_ID_PATTERN exported and works**
   - Action: Run `bun test packages/daemon/src/state/__tests__/types.test.ts --filter "SESSION_ID_PATTERN"`
   - Expected: All pattern tests pass
   - Tool: bun test

6. **OC registry scanner**
   - Action: Run `bun test packages/daemon/src/cli/__tests__/index.test.ts --filter "scanOcRegistry"`
   - Expected: All registry scanner tests pass including malformed/missing scenarios
   - Tool: bun test

7. **adopt command structure and behavior**
   - Action: Run `bun test packages/daemon/src/cli/__tests__/index.test.ts --filter "adoptCommand|cmdAdopt"`
   - Expected: Metadata tests and behavior tests pass (POST body includes sessionId/force/prompt, 409 session_already_adopted handling, invalid session ID format rejection)
   - Tool: bun test

8. **DuplicateIDError treated as idempotent success (existing coverage)**
   - Action: Run `bun test packages/daemon/src/daemon/__tests__/serve-manager.test.ts --filter "DuplicateIDError"`
   - Expected: All existing DuplicateIDError tests pass — confirms that `adapter.createSession()` returning 409 with DuplicateIDError is treated as idempotent success (returns the session ID). Adoption uses the same `createSession()` call, so this acceptance criterion is satisfied by existing code (serve-manager.ts lines 119-129) and existing tests.
   - Tool: bun test

9. **Full suite regression**
   - Action: Run `bun test`
   - Expected: All tests pass
   - Tool: bun test

### Tools Needed
- `bun test` for all automated testing
- `bunx biome check` for lint verification
- `bunx tsc --noEmit` for type checking

### Skills to Invoke
- `/test-driven-development` — TDD workflow for writing tests before implementation
- `/verification-before-completion` — verify all acceptance criteria before marking complete

### Manual Verification (not in CI)
- Prompt queue-order behavior (adopted session processes prompt after current task completes)
- Instance bootstrap on shared serve (readiness-wait pattern for adopted sessions)
- End-to-end adoption with real standalone process → SIGHUP → shared serve pickup

---

## Required Skills

The following project-specific skills should be loaded by downstream workers:

| Phase | Skills |
|-------|--------|
| Implement | `test-driven-development`, `envoy` |
| Test | `verification-before-completion` |
| Review | (none beyond standard) |

Workers: invoke these skills at the start of your workflow before beginning work.
If a skill is unavailable in your environment, proceed without it.
