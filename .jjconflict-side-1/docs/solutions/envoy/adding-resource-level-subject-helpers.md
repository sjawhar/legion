---
title: "Adding resource-level subject helpers to the contracts pipeline"
category: envoy
tags:
  - contracts
  - code-generation
  - subject-helpers
  - topic-filtering
date: 2026-04-05
status: active
module: envoy
related_issues:
  - "#185"
symptoms:
  - "how to add a new subject helper"
  - "githubResourceSubject not found"
  - "gofmt rejects generated.go"
  - "topic wildcard not matching expected events"
---

# Adding Resource-Level Subject Helpers to the Contracts Pipeline

Extends the contracts generation pipeline (see `contracts-and-handler-patterns.md`) with
a concrete checklist for adding per-resource topic helpers like `githubResourceSubject`.

## Checklist

1. **TS function** in `packages/contracts/src/subject.ts` — accepts `number | string` for
   resource identifiers (callers pass PR numbers as integers, the function coerces via
   template literal)
2. **Go function** in the `keep` block of `packages/contracts/scripts/gen-go.ts` — accepts
   only `string` parameters (Go callers must convert numbers with `strconv.Itoa` or
   `fmt.Sprintf`). Must use **literal tab characters** for indentation.
3. **Regenerate**: `bun run packages/contracts/scripts/gen-go.ts`
4. **Verify formatting**: `gofmt -l packages/envoy/internal/contracts/generated.go` — if it
   prints the filename, the indentation is wrong (spaces instead of tabs)
5. **TS tests** in `packages/contracts/src/envelope.test.ts`
6. **Go tests** in `packages/envoy/internal/contracts/normalize_test.go`
7. **Filtering proof** in `packages/envoy/internal/routing/match_test.go` — prove the
   wildcard subscription pattern works as intended

## The Tab Indentation Trap

The `keep` block in `gen-go.ts` is a TypeScript template literal containing raw Go source.
Its indentation is written verbatim to `generated.go`. Most editors auto-insert spaces in
`.ts` files, but Go requires tabs.

**Before committing:** Inspect the diff for the `keep` block. If you see spaces instead of
tabs in the Go function body, the CI `gofmt` check will fail.

**Biome side effect:** Running `bunx biome check --write` on `gen-go.ts` may reformat
surrounding TypeScript code (collapsing multi-line signatures, etc.). This is harmless but
adds noise to the diff. Run Biome early to isolate formatting changes from logic changes.

## Filtering Proof Tests

When introducing a new topic shape (e.g., per-PR topics), add a dedicated test in
`match_test.go` that proves the intended subscription pattern:

- **Matches** the target resource and its subtopics (`pr.7706`, `pr.7706.comment`,
  `pr.7706.review`)
- **Rejects** adjacent resources (`pr.7707`, `pr.7707.comment`)
- **Rejects** different resource types (`issue.7706`)

This documents the filtering contract for consumers and catches routing regressions.
Table-driven format matches the existing `TestMatch` pattern.

## TS vs Go Type Signatures

| Language | Resource number param | Why |
|----------|----------------------|-----|
| TypeScript | `number \| string` | Callers pass PR/issue numbers as integers; template literal coerces |
| Go | `string` | Go string concatenation requires explicit conversion; callers own the conversion |

This asymmetry is intentional — TS is lenient at the caller site, Go is strict.
