---
title: "OpenCode Plugin Hook Authoring Patterns"
category: architecture-patterns
tags:
  - opencode-plugin
  - hooks
  - tool-execute-before
  - config
  - factory-pattern
  - session-cleanup
date: 2026-04-13
status: active
module: opencode-plugin
related_issues:
  - "272"
symptoms:
  - "hook sees empty args"
  - "output.args vs input.args"
  - "how to add a new hook"
  - "config not taking effect"
  - "session cleanup memory leak"
---

# OpenCode Plugin Hook Authoring Patterns

Patterns for adding new hooks and configurable features to `packages/opencode-plugin`.
Extracted from implementing the circuit breaker hook (#272).

## 1. `tool.execute.before` Arg Source

**Critical: `output.args` contains the parsed tool arguments, not `input.args`.**

```typescript
"tool.execute.before": (input, output) => {
  // input = { tool: string, sessionID: string, callID: string, args?: unknown }
  // output = { args: Record<string, unknown> }  ← PARSED tool args live here

  const args = output.args;  // ✅ correct — parsed/validated args
  // NOT input.args           // ❌ wrong — raw/unprocessed args
}
```

`input` carries metadata (tool name, session ID, call ID) plus raw args.
`output.args` carries the parsed tool input that the tool will actually receive.
Using `input.args` causes the hook to see different (possibly empty) values from
what the tool processes. This is the single most common mistake in hook authoring.

## 2. The 5+3 Touch Point Pattern for New Configurable Hooks

Adding a new configurable hook requires exactly 8 touch points:

### Config (5 in `config/index.ts`)

| # | What | Example |
|---|------|---------|
| 1 | Zod schema | `const CircuitBreakerConfigSchema = z.object({...}).strict()` |
| 2 | TypeScript interface | `export interface CircuitBreakerConfig { ... }` |
| 3 | PluginConfig field | `circuitBreaker?: CircuitBreakerConfig` in `interface PluginConfig` |
| 4 | PluginConfigSchema field | `circuitBreaker: CircuitBreakerConfigSchema.optional()` in `PluginConfigSchema` |
| 5 | mergeConfig entry | `mergeCircuitBreaker` function + call in `mergeConfig()` |

**Merge function pattern for flat configs:**

```typescript
function mergeCircuitBreaker(
  base?: CircuitBreakerConfig,
  override?: CircuitBreakerConfig
): CircuitBreakerConfig | undefined {
  if (!base) return override;
  if (!override) return base;
  return { ...base, ...override };
}
```

Only configs with special merge semantics (e.g., `outputCompression.excludeTools`
which unions arrays) need custom logic beyond spread.

**Single source of truth:** Define the TypeScript interface in `config/index.ts`
and import it in the hook file. Do not duplicate the interface.

### Wiring (3 in `index.ts`)

| # | What | Example |
|---|------|---------|
| 6 | Import | `import { createCircuitBreakerHook } from "./hooks/circuit-breaker"` |
| 7 | Instantiation | `const hook = createCircuitBreakerHook(pluginConfig.circuitBreaker ?? {})` |
| 8 | Hook point registration | Add call in the appropriate handler body |

The `?? {}` fallback ensures the factory always receives a valid config even when
the user didn't configure the feature.

## 3. Factory Function vs Plain Function

**Factory function** — for hooks that need state, config, or cleanup:

```typescript
export function createCircuitBreakerHook(config: CircuitBreakerConfig = {}) {
  const state = new Map<string, Map<string, number>>();

  return {
    "tool.execute.before": (input, output) => { /* uses state */ },
    event: async ({ event }) => { /* cleanup state */ },
  };
}
```

**Plain function** — for stateless, deterministic hooks:

```typescript
export function subagentQuestionBlockerHook(input, output) { /* no state */ }
```

Use a factory when:
- Hook tracks per-session state
- Hook reads from config at instantiation time
- Hook needs cleanup on `session.deleted`

Return an object with keys matching hook point names (`tool.execute.before`, `event`, etc.).

## 4. Session Cleanup via `session.deleted`

Stateful hooks must clean up when sessions are deleted. Wire in `index.ts`:

```typescript
if (event.type === "session.deleted") {
  // ... existing cleanup ...
  await circuitBreakerHook.event({ event });
}
```

Inside the hook, use `resolveSessionID` from `hooks/utils.ts`:

```typescript
import { isRecord, resolveSessionID } from "./utils";

const event = async ({ event }) => {
  if (event.type === "session.deleted") {
    const props = isRecord(event.properties) ? event.properties : undefined;
    const sessionID = resolveSessionID(props);
    if (sessionID) {
      sessionCounts.delete(sessionID);
    }
  }
};
```

`resolveSessionID` handles two property shapes from the SDK:
- `{ sessionID: "s-1" }` — direct
- `{ info: { id: "s-1" } }` — nested alternative

**Always test both shapes** in your test suite.

## 5. Stable JSON Hashing for Arg Comparison

When comparing tool arguments for equality, use key-sorted JSON stringification:

```typescript
function sortedStringify(value: unknown): string {
  return JSON.stringify(value, (_key, v: unknown) => {
    if (v !== null && typeof v === "object" && !Array.isArray(v)) {
      const r = v as Record<string, unknown>;
      const s: Record<string, unknown> = {};
      for (const k of Object.keys(r).sort()) s[k] = r[k];
      return s;
    }
    return v;
  });
}
```

This ensures `{b:1, a:2}` and `{a:2, b:1}` produce identical strings.
The replacer recursively sorts nested objects too. No external hash
library needed — the string itself is the comparison key.
