# Contracts Package

Shared event contract surface for Legion/Envoy.

## Overview

This package is the language-neutral source of truth for Envoy event shapes and
the native Dispatch tool suite:

- envelope schema/type and subject helpers
- model-facing `dispatchToolSpecs` built over an injected Zod surface and
  `zodSchemaApi`
- Go output in `packages/envoy/internal/contracts/generated.go`

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| Envelope and subject contracts | `src/envelope.ts`, `src/subject.ts` | canonical TypeScript surface |
| Dispatch tool specifications | `src/dispatch-tools.ts`, `src/tool-schema.ts` | builders over `zodSchemaApi(hostZod)` |
| Go generation | `scripts/gen-go.ts` | writes the Envelope Go contract |
| Generated Go contract | `packages/envoy/internal/contracts/generated.go` | generated; do not hand-edit |
| Contract tests | `src/*.test.ts` | validation and schema drift coverage |

## Critical conventions

- `src/dispatch-tools.ts` is the source of the nine native Dispatch tools:
  `dispatch_issue`, `dispatch_ask`, `dispatch_comment`, `dispatch_suggest`,
  `dispatch_message`, `dispatch_doc_edit`, `dispatch_doc_read`,
  `dispatch_artifact`, and `dispatch_read`. It defines their names, descriptions,
  and field shapes; host adapters consume `dispatchToolSpecs` directly.
- Build field shapes through `zodSchemaApi(hostZod)` so option bags apply to the
  host's Zod. Use `dispatchToolSchema(spec, zodSchemaApi(hostZod))` when the
  host validates a call so tool-level cross-field validation also applies.
- Do not hand-edit `packages/envoy/internal/contracts/generated.go`.
- If the envelope or subject shape changes, update its schema and regenerate the
  applicable Go output.
- Prefer backward-compatible additions when extending the envelope.
- Keep examples synchronized with the real receiver output (Slack team IDs,
  GitHub owner/repo segments, and native Dispatch keys).
