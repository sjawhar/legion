# Contracts Package

Shared event contract surface for Legion/Envoy.

## Overview

This package is the language-neutral source of truth for Envoy event shapes.

Current scope:

- envelope schema/type and subject helpers
- dispatch question Zod contract in `src/dispatch-question.ts`
- checked-in dispatch question JSON Schema in `schemas/dispatch-question.schema.json`
- Go outputs in `packages/envoy/internal/contracts/generated.go` and
  `packages/envoy/internal/dispatch/core/generated.go`

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| Envelope and subject contracts | `src/envelope.ts`, `src/subject.ts` | canonical TypeScript surface |
| Dispatch question contract | `src/dispatch-question.ts` | canonical Zod source |
| Dispatch question schema | `schemas/dispatch-question.schema.json` | generated and checked in |
| Dispatch schema emitter | `scripts/dispatch-question-schema.ts` | emits the checked-in schema |
| Go generation | `scripts/gen-go.ts` | writes both Go outputs |
| Generated Go contracts | `packages/envoy/internal/contracts/generated.go`, `packages/envoy/internal/dispatch/core/generated.go` | generated; do not hand-edit |
| Contract tests | `src/*.test.ts` | validation and schema drift coverage |

## Critical conventions

- Change the dispatch question shape in `src/dispatch-question.ts`, emit
  `schemas/dispatch-question.schema.json` with `scripts/dispatch-question-schema.ts`, then run
  `scripts/gen-go.ts` to regenerate both Go outputs.
- Do not hand-edit `schemas/dispatch-question.schema.json`,
  `packages/envoy/internal/contracts/generated.go`, or
  `packages/envoy/internal/dispatch/core/generated.go`.
- If the envelope or subject shape changes, update its schema and regenerate the applicable Go output.
- Prefer backward-compatible additions when extending the envelope.
- Keep examples synchronized with the real receiver output (Slack team IDs, GitHub owner/repo segments, etc.).
