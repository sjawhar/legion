# Plan: Circuit Breaker for Repetitive Tool Use (Issue #272)

**Date:** 2026-04-13  
**Issue:** https://github.com/sjawhar/legion/issues/272  
**Commit message:** `feat(legion): add repetitive tool use circuit breaker`

## Overview

Add a `tool.execute.before` hook that detects when an agent calls the same tool with identical
arguments repeatedly (indicating a stuck loop), and either warns or aborts the call when a
configurable threshold is reached.

## Assumptions (documented)

1. **"Window" = count window** — tracks cumulative identical calls per session (not time-based).
   Matches AC: "5 identical calls triggers", "4 does NOT trigger".
2. **"Abort" = throw Error** — rejects the tool call by throwing, consistent with
   `subagentQuestionBlockerHook` pattern. Does NOT abort the session.
3. **"Identical args" = sort-key-normalized JSON** — `JSON.stringify` with recursively sorted keys
   so `{path:"/foo",limit:100}` and `{limit:100,path:"/foo"}` are treated as identical.
4. **5th call triggers** — when count reaches threshold (default 5), the 5th call triggers.
5. **Package** = `packages/opencode-plugin/src/hooks/`
6. **Args source** = `output.args` in `tool.execute.before` (parsed tool input lives in output
   object, as seen in `subagentQuestionBlockerHook` signature `_output: { args: Record<string, unknown> }`).
7. **Utilities** = `isRecord` and `resolveSessionID` already exist in
   `packages/opencode-plugin/src/hooks/utils.ts` — import from `"./utils"`.

## File Structure

| File | Action | Purpose |
|------|--------|---------|
| `packages/opencode-plugin/src/hooks/circuit-breaker.ts` | **Create** | Hook implementation |
| `packages/opencode-plugin/src/hooks/__tests__/circuit-breaker.test.ts` | **Create** | Tests (TDD-first) |
| `packages/opencode-plugin/src/config/index.ts` | **Modify** | Add `CircuitBreakerConfig` interface + `circuitBreaker` field + one line in `mergeConfig` |
| `packages/opencode-plugin/src/index.ts` | **Modify** | Import, instantiate, wire into `tool.execute.before` and `event` |

## Parallelism

- **Task 1** (config types + tests + hook impl) is one self-contained unit
- **Task 2** (wire into index.ts) **depends on** Task 1

---

## Task 1: Config types + failing tests + hook implementation [INDEPENDENT]

**Files:**
- Modify: `packages/opencode-plugin/src/config/index.ts`
- Create: `packages/opencode-plugin/src/hooks/__tests__/circuit-breaker.test.ts`
- Create: `packages/opencode-plugin/src/hooks/circuit-breaker.ts`

### Step 1.1: Add CircuitBreakerConfig to config/index.ts

In `packages/opencode-plugin/src/config/index.ts`:

**Add the interface** — insert after the `OutputCompressionConfig` interface (after line containing `maxIndexSizeMB?: number;`):

```typescript
export interface CircuitBreakerConfig {
  enabled?: boolean;
  threshold?: number;
  action?: "warn" | "abort";
}
```

**Add field to PluginConfig** — the current end of `PluginConfig` is:
```typescript
  outputCompression?: OutputCompressionConfig;
}
```
Change it to:
```typescript
  outputCompression?: OutputCompressionConfig;
  circuitBreaker?: CircuitBreakerConfig;
}
```

**Update mergeConfig** — the current `mergeConfig` return ends with:
```typescript
    outputCompression: mergeOutputCompression(base.outputCompression, override.outputCompression),
  };
}
```
Change it to:
```typescript
    outputCompression: mergeOutputCompression(base.outputCompression, override.outputCompression),
    circuitBreaker:
      base.circuitBreaker || override.circuitBreaker
        ? { ...base.circuitBreaker, ...override.circuitBreaker }
        : undefined,
  };
}
```

### Step 1.2: Type-check config change

```bash
cd packages/opencode-plugin && bunx tsc --noEmit
```

Expected: exit code 0, no errors

### Step 1.3: Write the failing test file

Create `packages/opencode-plugin/src/hooks/__tests__/circuit-breaker.test.ts`:

```typescript
import { describe, expect, it } from "bun:test";
import { createCircuitBreakerHook } from "../circuit-breaker";

function makeInput(tool: string, sessionID: string, args: unknown = {}) {
  return { tool, sessionID, callID: "c-1", args };
}

function makeOutput(args: unknown = {}) {
  return { args };
}

describe("createCircuitBreakerHook", () => {
  describe("threshold triggering", () => {
    it("does not trigger on 4 identical calls (below threshold)", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      for (let i = 0; i < 4; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }
    });

    it("triggers on the 5th identical call (reaches threshold)", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).toThrow(/repetitive tool use detected/i);
    });

    it("uses default threshold of 5 when not configured", () => {
      const hook = createCircuitBreakerHook();
      const args = { filePath: "/foo" };

      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("read", "s-1", args), makeOutput(args));
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("read", "s-1", args), makeOutput(args))
      ).toThrow();
    });

    it("respects custom threshold", () => {
      const hook = createCircuitBreakerHook({ threshold: 3 });
      const args = { command: "pwd" };

      for (let i = 0; i < 2; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).toThrow();
    });
  });

  describe("per-session isolation", () => {
    it("tracks sessions independently — 5 calls across 2 sessions does not trigger either", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // 3 calls in s-1, 2 calls in s-2 — neither reaches threshold
      for (let i = 0; i < 3; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }
      for (let i = 0; i < 2; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args));
      }

      // 4th call in s-1 — still below threshold
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).not.toThrow();
    });

    it("triggers independently per session without affecting the other", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // Fill s-1 to threshold
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).toThrow();

      // s-2 is unaffected — 4 calls should not trigger
      for (let i = 0; i < 4; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args))
        ).not.toThrow();
      }
    });
  });

  describe("args normalization", () => {
    it("two calls with same args in different key order count as the same call", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });

      // 4 calls with one key order
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](
          makeInput("read", "s-1", { path: "/foo", limit: 100 }),
          makeOutput({ path: "/foo", limit: 100 })
        );
      }

      // 5th call with different key order — should still trigger (same logical args)
      expect(() =>
        hook["tool.execute.before"](
          makeInput("read", "s-1", { limit: 100, path: "/foo" }),
          makeOutput({ limit: 100, path: "/foo" })
        )
      ).toThrow();
    });

    it("calls with different arg values are distinct and do not trigger", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });

      for (let i = 0; i < 4; i++) {
        const args = { command: `cmd-${i}` };
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }
    });

    it("different tools with same args are tracked independently", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { filePath: "/foo" };

      // 4 calls each for read and write — neither reaches threshold alone
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("read", "s-1", args), makeOutput(args));
        hook["tool.execute.before"](makeInput("write", "s-1", args), makeOutput(args));
      }

      // 5th read call triggers
      expect(() =>
        hook["tool.execute.before"](makeInput("read", "s-1", args), makeOutput(args))
      ).toThrow();

      // 5th write call also triggers
      expect(() =>
        hook["tool.execute.before"](makeInput("write", "s-1", args), makeOutput(args))
      ).toThrow();
    });
  });

  describe("action configuration", () => {
    it("throws on trigger when action is 'abort' (default)", () => {
      const hook = createCircuitBreakerHook({ threshold: 5, action: "abort" });
      const args = { command: "ls" };

      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).toThrow();
    });

    it("does not throw on trigger when action is 'warn'", () => {
      const hook = createCircuitBreakerHook({ threshold: 5, action: "warn" });
      const args = { command: "ls" };

      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).not.toThrow();
    });
  });

  describe("session cleanup", () => {
    it("clears tracking on session.deleted event", async () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // Fill to 4 calls
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      // Delete session
      await hook.event({
        event: { type: "session.deleted", properties: { sessionID: "s-1" } },
      });

      // Counter reset — 4 more calls should not trigger
      for (let i = 0; i < 4; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }
    });

    it("ignores session.deleted for unknown session without error", async () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });

      await expect(
        hook.event({ event: { type: "session.deleted", properties: { sessionID: "unknown" } } })
      ).resolves.toBeUndefined();
    });

    it("does not affect other sessions on session.deleted", async () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // Fill both sessions to 4 calls
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
        hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args));
      }

      // Delete s-1
      await hook.event({
        event: { type: "session.deleted", properties: { sessionID: "s-1" } },
      });

      // s-2 still at 4 calls — 5th should trigger
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args))
      ).toThrow();

      // s-1 was reset — should not trigger
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).not.toThrow();
    });
  });

  describe("disabled state", () => {
    it("never triggers when enabled is false, even after many identical calls", () => {
      const hook = createCircuitBreakerHook({ enabled: false, threshold: 5 });
      const args = { command: "ls" };

      for (let i = 0; i < 10; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }
    });
  });
});
```

### Step 1.4: Run tests to verify they fail

```bash
cd packages/opencode-plugin && bun test src/hooks/__tests__/circuit-breaker.test.ts
```

Expected: exit code non-zero — `Cannot find module '../circuit-breaker'`

### Step 1.5: Implement the circuit-breaker hook

Create `packages/opencode-plugin/src/hooks/circuit-breaker.ts`:

```typescript
// isRecord and resolveSessionID are in packages/opencode-plugin/src/hooks/utils.ts
import { isRecord, resolveSessionID } from "./utils";

const DEFAULT_THRESHOLD = 5;
const DEFAULT_ACTION = "abort" as const;

export interface CircuitBreakerConfig {
  enabled?: boolean;
  threshold?: number;
  action?: "warn" | "abort";
}

/**
 * Recursively sorts object keys to produce a stable JSON string
 * regardless of key insertion order.
 * e.g. {b:1,a:2} and {a:2,b:1} both produce '{"a":2,"b":1}'
 */
function sortedStringify(value: unknown): string {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return JSON.stringify(value);
  }
  const record = value as Record<string, unknown>;
  const sorted: Record<string, unknown> = {};
  for (const key of Object.keys(record).sort()) {
    sorted[key] = record[key];
  }
  return JSON.stringify(sorted, (_key, v: unknown) => {
    if (v !== null && typeof v === "object" && !Array.isArray(v)) {
      const r = v as Record<string, unknown>;
      const s: Record<string, unknown> = {};
      for (const k of Object.keys(r).sort()) {
        s[k] = r[k];
      }
      return s;
    }
    return v;
  });
}

/**
 * Circuit breaker hook that detects repetitive identical tool calls per session.
 *
 * Tracking structure: Map<sessionID, Map<"toolName:argsHash", callCount>>
 *
 * Triggers when count reaches threshold (default 5).
 * action "abort": throws Error (rejects the tool call, does not abort session).
 * action "warn": logs to console.warn only.
 * Cleanup: clears session tracking on session.deleted event.
 */
export function createCircuitBreakerHook(config: CircuitBreakerConfig = {}) {
  const enabled = config.enabled !== false;
  const threshold = config.threshold ?? DEFAULT_THRESHOLD;
  const action = config.action ?? DEFAULT_ACTION;

  // Map<sessionID, Map<"toolName:argsHash", count>>
  const sessionCounts = new Map<string, Map<string, number>>();

  // Signature matches tool.execute.before: input has tool/sessionID/callID,
  // output has args (the parsed tool input).
  // See subagentQuestionBlockerHook for the same pattern.
  const toolExecuteBefore = (
    input: { tool: string; sessionID?: string; callID?: string; args?: unknown },
    output: { args?: unknown }
  ): void => {
    if (!enabled) return;

    const sessionID = typeof input.sessionID === "string" ? input.sessionID : undefined;
    if (!sessionID) return;

    const toolName = typeof input.tool === "string" ? input.tool : "";
    // Args live in output.args (parsed tool input) in tool.execute.before
    const args = isRecord(output) && "args" in output ? output.args : input.args;
    const argsHash = sortedStringify(args ?? {});
    const key = `${toolName}:${argsHash}`;

    let sessionMap = sessionCounts.get(sessionID);
    if (!sessionMap) {
      sessionMap = new Map<string, number>();
      sessionCounts.set(sessionID, sessionMap);
    }

    const count = (sessionMap.get(key) ?? 0) + 1;
    sessionMap.set(key, count);

    if (count >= threshold) {
      const message =
        `[circuit-breaker] Repetitive tool use detected: "${toolName}" called ${count} times ` +
        `with identical arguments in session "${sessionID}". ` +
        `This may indicate a stuck loop. Vary your approach or use a different tool.`;

      if (action === "abort") {
        throw new Error(message);
      } else {
        console.warn(message);
      }
    }
  };

  const event = async ({
    event,
  }: {
    event: { type: string; properties?: unknown };
  }): Promise<void> => {
    if (event.type === "session.deleted") {
      const props = isRecord(event.properties) ? event.properties : undefined;
      const sessionID = resolveSessionID(props);
      if (sessionID) {
        sessionCounts.delete(sessionID);
      }
    }
  };

  return {
    "tool.execute.before": toolExecuteBefore,
    event,
  };
}
```

### Step 1.6: Run tests to verify they pass

```bash
cd packages/opencode-plugin && bun test src/hooks/__tests__/circuit-breaker.test.ts
```

Expected: exit code 0, all tests pass

### Step 1.7: Run full test suite to check for regressions

```bash
cd packages/opencode-plugin && bun test
```

Expected: exit code 0, all existing tests still pass

### Step 1.8: Type-check

```bash
cd packages/opencode-plugin && bunx tsc --noEmit
```

Expected: exit code 0, no errors

### Step 1.9: Commit

```bash
jj describe -m "feat(circuit-breaker): add config types, hook implementation, and tests"
jj new
```

---

## Task 2: Wire circuit breaker into plugin index.ts [DEPENDS ON Task 1]

**Files:**
- Modify: `packages/opencode-plugin/src/index.ts`

### Step 2.1: Add import

In `packages/opencode-plugin/src/index.ts`, add after the existing hook imports (keep alphabetical order among hook imports):

```typescript
import { createCircuitBreakerHook } from "./hooks/circuit-breaker";
```

### Step 2.2: Instantiate the hook

In the `OpenCodeLegion` plugin function body, after the line:
```typescript
const stopContinuationGuardHook = createStopContinuationGuardHook();
```
Add:
```typescript
const circuitBreakerHook = createCircuitBreakerHook(pluginConfig.circuitBreaker ?? {});
```

### Step 2.3: Wire into `tool.execute.before`

The current `tool.execute.before` handler begins:
```typescript
    "tool.execute.before": async (input, output) => {
      subagentQuestionBlockerHook(input, output);
```

Change it to:
```typescript
    "tool.execute.before": async (input, output) => {
      circuitBreakerHook["tool.execute.before"](input, output);
      subagentQuestionBlockerHook(input, output);
```

(Insert `circuitBreakerHook["tool.execute.before"](input, output);` as the new first line of the handler body, before `subagentQuestionBlockerHook`.)

### Step 2.4: Wire into `event` handler

The current `session.deleted` block in the `event` handler is:
```typescript
      if (event.type === "session.deleted") {
        const sessionProps = isRecord(event.properties) ? event.properties : undefined;
        const sessionID = resolveSessionID(sessionProps);
        if (sessionID) {
          await manager.cleanup(sessionID);
        }
      }
```

Change it to:
```typescript
      if (event.type === "session.deleted") {
        const sessionProps = isRecord(event.properties) ? event.properties : undefined;
        const sessionID = resolveSessionID(sessionProps);
        if (sessionID) {
          await manager.cleanup(sessionID);
        }
        await circuitBreakerHook.event({ event });
      }
```

(Add `await circuitBreakerHook.event({ event });` immediately after the closing `}` of the `if (sessionID)` block, still inside the `if (event.type === "session.deleted")` block.)

### Step 2.5: Type-check

```bash
cd packages/opencode-plugin && bunx tsc --noEmit
```

Expected: exit code 0, no errors

### Step 2.6: Run full test suite

```bash
cd packages/opencode-plugin && bun test
```

Expected: exit code 0, all tests pass

### Step 2.7: Run lint

```bash
cd packages/opencode-plugin && bunx biome check src/
```

Expected: exit code 0, no errors. If formatting issues: `cd packages/opencode-plugin && bunx biome check --write src/`

### Step 2.8: Commit

```bash
jj describe -m "feat(circuit-breaker): wire hook into plugin index"
jj new
```

---

## Testing Plan

### QA Evidence

Write to `.sisyphus/evidence/task-3-circuit-breaker.txt`:

```
## Circuit Breaker QA Evidence

### Test 1: 5 identical Read calls → trigger
Run: cd packages/opencode-plugin && bun test src/hooks/__tests__/circuit-breaker.test.ts --grep "triggers on the 5th"
Expected: exit code 0, 1 test passed

### Test 2: 4 identical calls → no trigger
Run: cd packages/opencode-plugin && bun test src/hooks/__tests__/circuit-breaker.test.ts --grep "does not trigger on 4"
Expected: exit code 0, 1 test passed

### Test 3: 5 calls across 2 sessions → no trigger
Run: cd packages/opencode-plugin && bun test src/hooks/__tests__/circuit-breaker.test.ts --grep "5 calls across 2 sessions"
Expected: exit code 0, 1 test passed

### Full suite
Run: cd packages/opencode-plugin && bun test
Expected: exit code 0, all tests pass
```

### Skills to Invoke

No project-specific skills required (issue states "Skills: None required").

---

## Summary

| Task | Files | Parallel? |
|------|-------|-----------|
| Task 1: Config + hook + tests | `config/index.ts`, `circuit-breaker.ts`, `circuit-breaker.test.ts` | Yes (independent) |
| Task 2: Wire into index | `index.ts` | After Task 1 |

**Total tasks:** 2 (1 independent, 1 sequential)  
**Estimated complexity:** Quick (1 implementer)
