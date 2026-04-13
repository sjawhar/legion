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
  - hook-ordering
  - json-repair
  - text-parsing
date: 2026-04-13
status: active
module: opencode-plugin
related_issues:
  - "272"
  - "274"
symptoms:
  - "hook sees empty args"
  - "output.args vs input.args"
  - "how to add a new hook"
  - "config not taking effect"
  - "session cleanup memory leak"
  - "hook execution order matters"
  - "regex fails on nested quotes"
  - "char-by-char vs regex for string transforms"
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

## 6. Hook Ordering in `tool.execute.before`

Hooks in the same hook point run sequentially in registration order (see `index.ts`).
**Order matters when hooks have semantic dependencies.**

Example from #274: `jsonErrorRecoveryHook` must run **before** `circuitBreakerHook`:

```typescript
"tool.execute.before": async (input, output) => {
  jsonErrorRecoveryHook(input, output);      // ← normalizes args first
  circuitBreakerHook["tool.execute.before"](input, output);  // ← hashes normalized args
  subagentQuestionBlockerHook(input, output);
  // ...
}
```

Why: circuit-breaker hashes `output.args` to detect repetitive tool calls.
If JSON recovery ran after circuit-breaker, the same logical call with
`{'key': 'val'}` (single-quoted) and `{"key": "val"}` (double-quoted) would
produce different hashes, defeating dedup. Recovery first ensures the
circuit-breaker always sees canonical JSON.

**General principle:** normalizers/canonicalizers go first, detectors/blockers
go second, transformers go last. When adding a new hook, ask: "does any
existing hook read `output.args` after my hook writes it?"

## 7. Guard-and-Skip Pattern for Arg Iteration

When a hook needs to inspect all tool arguments (not just a known key):

```typescript
for (const key of Object.keys(output.args)) {
  const value = output.args[key];
  if (typeof value !== "string") continue;    // type guard
  if (!looksLikeJson(value)) continue;         // cheap heuristic guard
  // expensive operation only on candidates
}
```

This avoids assuming which arg contains the data you care about — agents
may pass JSON in any argument. The heuristic guard (`looksLikeJson` checks
for leading `{` or `[`) prevents expensive parsing on non-candidates.

**Test the multi-key case:** if the hook iterates all args, verify it handles
multiple matching args in one call (e.g., `{ a: "{'k':'v'}", b: "{x: 1,}" }`).
This gap was caught in code review for #274.

## 8. Char-by-Char Parsing vs Regex for String Transforms

**Use regex** for structural transforms outside strings (trailing commas,
quoting bareword keys):

```typescript
// Trailing commas: safe — regex operates on structural tokens
repaired = repaired.replace(/,\s*([}\]])/g, "$1");

// Unquoted keys: safe — anchored after { or , (structural positions)
repaired = repaired.replace(/([{,]\s*)([a-zA-Z_$][a-zA-Z0-9_$]*)(\s*:)/g, '$1"$2"$3');
```

**Use a character-by-character state machine** when the transform must
understand string boundaries (anything involving quotes, escaping, nesting):

```typescript
// Single→double quote conversion: MUST use char-by-char
// because regex can't reliably handle:
//   - escaped quotes within strings
//   - double quotes inside single-quoted strings needing escaping
//   - mixed quote types: {'key': "it's a value"}
function replaceSingleQuotes(input: string): string {
  const chars: string[] = [];
  let i = 0;
  while (i < input.length) {
    if (input[i] === '"') { /* pass through double-quoted string */ }
    else if (input[i] === "'") { /* convert to double-quoted, escape inner " */ }
    else { chars.push(input[i]); i++; }
  }
  return chars.join("");
}
```

**When multiple transforms compose**, order matters: single quotes → unquoted
keys → trailing commas. Single quotes first because the unquoted-key regex
assumes strings are already double-quoted. Trailing commas last because
earlier transforms may leave trailing commas as artifacts.

## 9. Pure Function + Thin Hook Wrapper

Export the core logic as a pure function alongside the hook:

```typescript
// Pure, independently testable
export function repairJson(input: string): string | null { ... }

// Thin wrapper: walks args, calls pure function, mutates output
export function jsonErrorRecoveryHook(input, output): void {
  for (const key of Object.keys(output.args)) {
    const repaired = repairJson(output.args[key]);
    if (repaired !== null && repaired !== value) output.args[key] = repaired;
  }
}
```

This gives test suites two levels:
- **Unit tests** on the pure function: exercise edge cases without constructing
  hook input/output shapes
- **Integration tests** on the hook: verify arg iteration, passthrough, multi-key behavior

The pure function export also enables reuse outside the hook context.
