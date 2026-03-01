---
title: "Session Isolation in OpenCode Plugin Hooks"
date: 2026-03-01
category: architecture-patterns
tags: [plugin, session-isolation, opencode-plugin, hooks, multi-tenant]
related-issues: [67]
related-prs: [75]
---

# Session Isolation in OpenCode Plugin Hooks

## Problem

Any stateful component created at plugin initialization is **process-scoped**, not session-scoped. A single `opencode serve` process hosts multiple concurrent agent sessions. If a hook creates state (database, cache, map) during init, all sessions share it.

This caused two P1 bugs in the output compression feature (#67):

1. **Cross-session data leakage** — `context_search` returned content indexed from other sessions
2. **Silent cross-session data destruction** — source key collisions across sessions caused `deleteSource()` to destroy another session's indexed content

## Root Cause: Lifetime vs Access Scoping

The spec said "session-scoped DB" and the implementation used a PID-based filename (`/tmp/legion-context-<pid>.db`). This confused **lifetime scoping** (when is the resource created/destroyed) with **access scoping** (who can read/write the resource).

| Scoping Type | PID-based filename | Session-namespaced rows |
|---|---|---|
| Lifetime | Per-process ✓ | Per-process ✓ |
| Access | All sessions share ✗ | Per-session isolation ✓ |

The PID-based filename only prevents cross-process collisions. Within a single process, all sessions still share the same DB.

## Solution Pattern: Session-Namespaced Shared Store

Two options were evaluated:

1. **`Map<sessionID, Store>`** — true isolation, one store instance per session
2. **Session column + default filtering** — single shared store, rows tagged with sessionID

Option 2 was chosen: simpler, one DB file, no lifecycle management for per-session instances.

### Implementation

```typescript
// FTS5 tables include session as UNINDEXED column (stored but not tokenized)
"CREATE VIRTUAL TABLE IF NOT EXISTS porter_index USING fts5(
  source UNINDEXED, session UNINDEXED, title, content,
  tokenize='porter unicode61'
);"

// Index includes session
store.index({ content, source: `${sessionID}:${tool}:${callID}`, session: sessionID });

// Search defaults to caller's session
store.search({ queries, session: ctx.sessionID });
```

### Source Key Construction

Any key that must be globally unique in a shared store needs **all scoping dimensions**:

```typescript
// WRONG: callID is only unique within a session
const source = `${tool}:${callID}`;

// RIGHT: sessionID provides the global namespace
const source = `${sessionID}:${tool}:${callID}`;
```

Without the sessionID prefix, `ContentStore.index()` calling `deleteSource(source)` before inserting would silently destroy another session's data on callID collision.

## Decision Checklist for Plugin State

When adding any stateful component to an OpenCode plugin, answer:

1. **Who shares this process?** Multiple agent sessions run in one `opencode serve` process.
2. **Does this state need session isolation?** If it holds session-specific data (tool outputs, conversation state, user context), yes.
3. **Lifetime or access scoping?** PID-based filenames give lifetime scoping. Session-namespaced rows or per-session instances give access scoping.
4. **Are composite keys session-qualified?** Any key used for lookup/deletion must include sessionID if the store is shared.
5. **Does the tool filter by session automatically?** The tool accessing the store should read `ctx.sessionID` and apply it as a default filter, not rely on agents to pass it.

## Testing Session Isolation

The initial test suite only tested single-session behavior, which missed both P1s. Multi-session tests should be standard for any shared-state plugin component:

```typescript
// Test session isolation at the store level
store.index({ content: "Session A data", source: "s-a:bash:1", session: "session-a" });
store.index({ content: "Session B data", source: "s-b:bash:2", session: "session-b" });

const results = store.search({ queries: ["data"], session: "session-a" });
expect(results.every(r => r.source.startsWith("s-a:"))).toBe(true);

// Test at the tool level (verify ctx.sessionID is passed through)
const tool = createContextSearchTool(store);
await tool.execute({ queries: ["data"] }, { ...ctx, sessionID: "session-a" });
// Verify store.search received session: "session-a"
```

## Hook Chain Ordering

A secondary learning: when multiple hooks transform/react to the same output, **size-reducing transforms run before size-sensitive triggers**.

```typescript
"tool.execute.after": async (input, output) => {
  await outputCompressionHook["tool.execute.after"]?.(input, output);   // reduces size
  await preemptiveCompactionHook["tool.execute.after"]?.(input, output); // reacts to size
},
```

If the order were reversed, the compaction hook would see the full raw output (6KB), potentially triggering unnecessarily, and then compression would run on an already-compacted context.
