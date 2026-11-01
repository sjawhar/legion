---
title: "Self-Fetch Endpoint Composition: Reusing Server Handlers via HTTP Loopback"
category: daemon
tags:
  - server
  - endpoint-composition
  - http
  - architecture
  - technical-debt
date: 2026-04-13
status: active
module: daemon
related_issues:
  - "sjawhar-legion-494"
symptoms:
  - "new endpoint needs to dispatch workers but handler is a closure"
  - "POST /workers handler too large to extract as a function"
  - "duplicate logic between advance and dispatch endpoints"
---

# Self-Fetch Endpoint Composition: Reusing Server Handlers via HTTP Loopback

Learnings from implementing `POST /state/advance` and `POST /state/auto-advance` endpoints that needed to reuse the existing `POST /workers` dispatch logic (#494).

## The Situation

The `POST /workers` handler is a ~330-line inline closure inside the `Bun.serve` fetch handler. It cannot be called as a function from other endpoints. The advance endpoint needs to dispatch workers based on action recommendations.

## Pattern: Internal Self-Fetch

Rather than refactoring the monolithic handler (high-risk scope creep), the advance endpoint reuses it by making HTTP requests to itself:

```typescript
// POST /state/advance calls POST /workers via loopback
const response = await fetch(`http://127.0.0.1:${server.port}/workers`, {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ issueId, stage: workerMode, prompt }),
});
```

`POST /state/auto-advance` similarly calls `POST /state/advance` for each issue.

## Trade-offs

**Advantages:**
- Zero refactoring risk — the existing handler stays untouched
- Full request pipeline exercised (JSON parsing, validation, state lookups)
- Easy to test — integration tests use real HTTP just like production

**Disadvantages:**
- Adds latency per dispatch (loopback HTTP round-trip)
- Error handling is indirect — HTTP status codes instead of exceptions
- Requires the server to be listening (can't use in unit tests without server)
- **This is technical debt** — any future endpoint that needs dispatch will face the same choice

## Action-to-Prompt Mapping

The semantic heart of the advance endpoint is the `switch` statement mapping `ActionType` to worker prompts. This encodes what each action means:

```typescript
switch (action) {
  case "dispatch_planner":
    workerMode = "plan"; prompt = "Plan implementation..."; break;
  case "resume_implementer_for_changes":
    workerMode = "implement"; prompt = "Address review feedback..."; break;
  case "relay_user_feedback":
    workerMode = "implement"; prompt = "User feedback: ..."; break;
  // ...
}
```

**Gotcha:** Every new `ActionType` added to the state machine must be evaluated for inclusion in:
1. The action-to-prompt mapping (what mode and prompt to use)
2. The `AUTO_ADVANCE_ACTIONS` set (whether auto-advance should handle it)

Missing either causes silent no-ops — the action is suggested but never executed.

## Fire-and-Forget for Non-Critical Operations

Auto-advance after state refresh uses `.then().catch()` instead of `await`:

```typescript
// Don't block the state refresh response on auto-advance
autoAdvanceAll().then(() => { /* logged */ }).catch((e) => console.error(e));
```

This is appropriate when: the caller doesn't need the result, failure is logged but non-fatal, and blocking would add latency to the critical path.

## Future Direction

Extract `POST /workers` handler into a callable `createAndDispatchWorker(opts)` function that both endpoints can invoke directly. This was identified as high-risk during planning and deliberately deferred — but it's the right eventual architecture.

## Pipe Deadlock in Process Spawning

When spawning `gh` CLI commands with `Bun.spawn`, awaiting `proc.exited` before reading stdout can deadlock if the process produces enough output to fill the pipe buffer. The fix: read streams concurrently with `Promise.all`:

```typescript
// WRONG — can deadlock
await proc.exited;
const stdout = await new Response(proc.stdout).text();

// RIGHT — read and wait concurrently
const [stdout, stderr, exitCode] = await Promise.all([
  new Response(proc.stdout).text(),
  new Response(proc.stderr).text(),
  proc.exited,
]);
```

This applies to any subprocess invocation where the output could be large (GitHub GraphQL responses, for example).
