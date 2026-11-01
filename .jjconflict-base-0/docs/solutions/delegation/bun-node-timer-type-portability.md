---
title: Timer Type Portability in Bun/Node TypeScript
date: 2026-04-12
status: active
tags:
  - typescript
  - bun
  - timer
  - portability
  - gotcha
---

# Timer Type Portability in Bun/Node TypeScript

## Problem

`setTimeout` returns different types across runtimes:
- **Browser**: `number`
- **Node.js**: `NodeJS.Timeout` (an object)
- **Bun**: `Timer` (a different object type)

Using `number` or `NodeJS.Timeout` as the timer type causes TypeScript errors in Bun,
and using `clearTimeout` without `globalThis.` can resolve to the wrong overload.

## Solution

Use `ReturnType<typeof setTimeout>` for the timer type and `globalThis.clearTimeout` for clearing:

```typescript
// ✅ Portable — works in Bun, Node, and browser
private timeoutTimers = new Map<string, ReturnType<typeof setTimeout>>();

private clearTimeout(taskId: string): void {
  const timer = this.timeoutTimers.get(taskId);
  if (timer) {
    globalThis.clearTimeout(timer);  // globalThis ensures correct overload resolution
    this.timeoutTimers.delete(taskId);
  }
}
```

```typescript
// ❌ Node-specific — fails in Bun
private timeoutTimers = new Map<string, NodeJS.Timeout>();

// ❌ Browser-specific — fails in Node/Bun
private timeoutTimers = new Map<string, number>();
```

## Why `globalThis.clearTimeout`

In TypeScript with `@types/node` installed, `clearTimeout` has multiple overloads
(one for `number`, one for `NodeJS.Timeout`). Without `globalThis.`, the compiler may
pick the wrong overload when the timer type is `ReturnType<typeof setTimeout>`.
`globalThis.clearTimeout` forces resolution to the global (browser-compatible) overload,
which accepts the return type of `setTimeout` in all runtimes.

## Where This Applies

Any TypeScript code in a Bun project that stores `setTimeout` return values for later
cancellation — background task managers, debounce utilities, polling loops, etc.

## Test Verification

The type is correct if `tsc --noEmit` passes without errors and `bun test` runs the
timer-based tests successfully. The runtime behavior (timer fires, timer clears) is
what matters — the type annotation is just to satisfy the compiler.
