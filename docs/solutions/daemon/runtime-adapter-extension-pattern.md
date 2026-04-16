---
title: "Runtime Adapter Extension Pattern: Adding Optional Features Across Runtimes"
category: daemon
tags:
  - runtime-adapter
  - interface-extension
  - type-safety
  - cross-runtime
  - opencode
  - claude-code
date: 2026-04-16
status: active
module: daemon
related_issues:
  - "541"
symptoms:
  - "How to add a new parameter to RuntimeAdapter.sendPrompt"
  - "Type assertion error with heterogeneous message parts"
  - "AgentPartInput not assignable to TextPartInput array"
---

# Runtime Adapter Extension Pattern: Adding Optional Features Across Runtimes

Learnings from adding `agentName` support to `RuntimeAdapter.sendPrompt()` (#541), which maps
worker modes to agent types so different phases (architect, implement, review, etc.) can use
different agent configurations.

## Pattern: Optional Parameters for Cross-Runtime Features

When a feature is supported by one runtime but not another, extend the interface with an
optional parameter rather than creating a new method:

```typescript
// types.ts — RuntimeAdapter interface
sendPrompt(sessionId: string, text: string, agentName?: string): Promise<void>;
```

**Why optional parameter over new method:**
- Backwards compatible — existing callers don't need changes
- Single dispatch point in `server.ts` — no conditional method selection
- Each adapter decides how to handle the parameter

**Adapter implementations:**

```typescript
// OpenCode — uses the feature: prepends AgentPartInput to message parts
async sendPrompt(sessionId: string, text: string, agentName?: string): Promise<void> {
  const parts: Array<{ type: string; [key: string]: unknown }> = [];
  if (agentName) {
    parts.push({ type: "agent", name: agentName });
  }
  parts.push({ type: "text", text });
  await client.session.promptAsync({ sessionID: sessionId, parts });
}

// Claude Code — ignores the feature (no agent selection API)
async sendPrompt(sessionId: string, text: string, _agentName?: string): Promise<void> {
  // _agentName unused — Claude Code doesn't support agent selection
  await this.sdk.sendMessage(text, { sessionId });
}
```

**Server dispatch — lookup at call site:**

```typescript
const agentName = opts.modeAgents?.[mode];
await workerAdapter.sendPrompt(actualSessionId, prompt, agentName);
```

## Gotcha: Type Assertions with Heterogeneous Arrays

When prepending an `AgentPartInput` to an array of `TextPartInput`, the array becomes
heterogeneous. **Do not** cast it back to a homogeneous type:

```typescript
// BAD — bypasses type safety, lies about array contents
const parts = [{ type: "agent", name: agentName }, { type: "text", text }];
await client.session.promptAsync({
  sessionID: sessionId,
  parts: parts as Array<{ type: "text"; text: string }>,  // ← type lie
});

// GOOD — use a union type or the SDK's own part type
const parts: Array<TextPartInput | AgentPartInput> = [];
if (agentName) parts.push({ type: "agent", name: agentName });
parts.push({ type: "text", text });
```

**Why this matters:** The type assertion compiles but the runtime array contains objects
that don't match the asserted type. Any downstream code that trusts the type (e.g.,
accessing `.text` on every element) will fail silently or throw.

## Config Wiring Checklist

When adding a new mode-to-value mapping to daemon config:

1. **Interface** — add field to `DaemonConfig` (use `Partial<Record<string, string>>` for
   optional mappings)
2. **Schema** — add to `CONFIG_SCHEMA` with `{ [CONFIG_ANY_KEY]: null }` for dynamic keys
   (see `config-wildcard-schema-pattern.md`)
3. **Parser** — validate keys against known modes, warn (don't error) on unknowns for
   forward compatibility
4. **Resolution** — default to `{}` in `resolveDaemonConfig` so consumers can use
   optional chaining (`opts.modeAgents?.[mode]`)
5. **Wiring** — pass through `startDaemon` → `ServerOptions` → dispatch site
6. **Tests** — cover: valid mapping, unknown mode warning, missing mapping (default behavior),
   and the adapter's handling of the new parameter
