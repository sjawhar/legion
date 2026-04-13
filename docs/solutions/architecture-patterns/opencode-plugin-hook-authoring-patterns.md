---
title: "OpenCode Plugin Hook Authoring Patterns"
category: architecture-patterns
tags:
  - opencode-plugin
  - hooks
  - tool-execute-before
  - arg-hashing
  - plugin-config
date: 2026-04-13
status: active
module: opencode-plugin
related_issues:
  - "272"
symptoms:
  - "circuit breaker always triggers on same tool regardless of args"
  - "hook sees empty args object"
  - "JSON.stringify produces different hashes for same args"
  - "adding new plugin config section causes validation errors"
---

# OpenCode Plugin Hook Authoring Patterns

Patterns and gotchas for writing hooks in `packages/opencode-plugin/src/hooks/`.

## Pattern 1: `tool.execute.before` — Args Live in `output.args`, Not `input.args`

In the `tool.execute.before` hook, the **parsed tool input (args) lives in `output.args`**, not `input.args`.

```typescript
const toolExecuteBefore = (
  input: { tool: string; sessionID?: string; callID?: string; args?: unknown },
  output: { args?: unknown }
): void => {
  // CORRECT: args are in output
  const args = isRecord(output) && "args" in output ? output.args : input.args;

  // WRONG: input.args is often undefined or stale
  // const args = input.args;  // ← don't do this
};
```

**Why:** The `tool.execute.before` hook receives the raw input in `input` and the parsed/resolved tool input in `output`. The `output.args` object contains the actual arguments the tool will receive. Using `input.args` as a fallback is safe but `output.args` is the authoritative source.

**Reference:** `subagentQuestionBlockerHook` uses the same pattern — see `src/hooks/subagent-question-blocker.ts`.

## Pattern 2: Stable Arg Hashing Requires Recursive Key Sorting

`JSON.stringify` is **not stable** across key insertion orders. `{path:"/foo",limit:100}` and `{limit:100,path:"/foo"}` produce different strings.

For any hook that needs to compare or deduplicate tool args, use a recursive sorted-stringify:

```typescript
function sortedStringify(value: unknown): string {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    return JSON.stringify(value);
  }
  return JSON.stringify(value, (_key, v: unknown) => {
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
```

This ensures `{path:"/foo",limit:100}` and `{limit:100,path:"/foo"}` produce the same hash.

**When you need this:** Any hook tracking "have I seen this exact tool call before?" — circuit breakers, deduplication, caching.

## Pattern 3: Hook Factory Pattern

All hooks use a factory function returning a plain object with hook method keys:

```typescript
export function createMyHook(config: MyConfig = {}) {
  // private state here
  const sessionData = new Map<string, ...>();

  return {
    "tool.execute.before": (input, output) => { ... },
    "tool.execute.after": async (input, output) => { ... },
    event: async ({ event }) => { ... },
    // expose for testing:
    getStats: () => ({ ... }),
  };
}
```

- No class, no interface — just a factory + plain object
- Private state is closed over in the factory scope
- Expose test helpers (stats, store accessors) as additional properties on the returned object
- The hook is instantiated once in `src/index.ts` and wired into the plugin's handler methods

## Pattern 4: Adding a New Config Section — All Five Touch Points

Adding a new config section to `PluginConfig` requires **five** changes in `src/config/index.ts`:

1. **Zod schema** — `const MyConfigSchema = z.object({ ... }).strict();`
2. **TypeScript interface** — `export interface MyConfig { ... }`
3. **Field on `PluginConfig`** — `myConfig?: MyConfig;`
4. **Field in `PluginConfigSchema`** — `myConfig: MyConfigSchema.optional(),`
5. **Merge logic in `mergeConfig`** — add a merge entry (shallow spread is fine for simple configs)

Missing any one of these causes silent failures (config ignored) or type errors.

**Merge strategy:** For simple flat configs, shallow spread is sufficient:
```typescript
myConfig:
  base.myConfig || override.myConfig
    ? { ...base.myConfig, ...override.myConfig }
    : undefined,
```

For configs with array fields that should be unioned (like `excludeTools`), write a dedicated merge function (see `mergeOutputCompression`).

## Pattern 5: Session Cleanup via `session.deleted` Event

Stateful hooks that track per-session data must clean up on `session.deleted`:

```typescript
event: async ({ event }) => {
  if (event.type === "session.deleted") {
    const props = isRecord(event.properties) ? event.properties : undefined;
    const sessionID = resolveSessionID(props);
    if (sessionID) {
      sessionData.delete(sessionID);
    }
  }
},
```

Wire the hook's `event` handler inside the `session.deleted` block in `src/index.ts` (not at the top-level event handler) to ensure it runs for every session deletion.

**Reference:** `src/hooks/output-compression.ts` uses the same pattern for its SQLite store.
