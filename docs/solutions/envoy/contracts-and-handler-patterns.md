---
title: "Contracts generation pipeline and listener handler testability"
category: envoy
tags:
  - contracts
  - code-generation
  - testing
  - handler-extraction
  - listener
date: 2026-04-05
status: active
module: envoy
related_issues:
  - "#234"
symptoms:
  - "Do not hand-edit generated.go"
  - "cannot test handler without NATS"
  - "AgentTopicPrefix not found"
---

# Contracts Generation Pipeline and Listener Handler Testability

Two patterns that arise when modifying the Envoy listener's HTTP handlers and the shared contracts package.

## Contracts: Adding Constants and Helpers to Go

The Go file `packages/envoy/internal/contracts/generated.go` is **generated** — never hand-edit it. Changes go through:

1. **TS source** (`packages/contracts/src/subject.ts`): Add the constant or function here
2. **Go generator** (`packages/contracts/scripts/gen-go.ts`): Add to the `keep` template string, which contains hand-maintained Go source (constants, helper functions) appended after the generated struct and `Validate()` method
3. **Regenerate**: `bun run packages/contracts/scripts/gen-go.ts`

The `keep` string is the Go contracts surface for anything that isn't a struct field or type definition. It must use literal tab indentation to produce valid `gofmt` output.

### Example: Adding `AgentTopicPrefix`

```typescript
// In gen-go.ts, the `keep` block:
const keep = `const AgentTopicPrefix = "notifications.agent."

func AgentSubject(session string) string {
	return AgentTopicPrefix + session
}
// ...
`;
```

When adding a new subject helper or topic prefix, always extract the prefix as a named constant in the same commit. Don't wait for string duplication to force it.

## Listener: Extracting Handlers for Testability

Handlers in `cmd/listener/main.go` that are defined as inline closures inside `main()` cannot be unit-tested. The established pattern is:

1. **Extract** the handler as a named function taking the `*listenerDeps` it serves over
2. **Return** `http.HandlerFunc`
3. **Wire** it in `registerV1Routes`, which main calls once every store is open, via `v1.HandleFunc("/path", myHandler(d))`

Examples: `publishHandler`, `sendHandler`. `healthzHandler` is the exception: it answers during
startup, so it takes the `*atomic.Pointer[listenerDeps]` that stays nil until then.

### Validation Before Deps Access

Place all input validation (JSON decode, required fields, topic checks) before the handler touches a store. This means tests can pass `&listenerDeps{}` with nil inner fields and still exercise the validation/rejection path without NATS.

```go
// Tests use minimal deps — no NATS needed for validation path
handler := publishHandler(&listenerDeps{}) // nil client/registry/sessions
// POST with bad topic → 400, never reaches a store
```

### Table-Driven Tests

The listener test file uses `cases := []struct{...}` table-driven tests. New handler tests should follow this pattern.
