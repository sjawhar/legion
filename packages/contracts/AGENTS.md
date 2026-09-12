# Contracts Package

Shared event contract surface for Legion/Envoy.

## Overview

This package is the language-neutral source of truth for Envoy event shapes and
the native Dispatch tool suite:

- envelope schema/type and subject helpers
- model-facing `dispatchToolSpecs` built over an injected Zod surface and
  `zodSchemaApi`
- Go output in `packages/envoy/internal/contracts/generated.go`
- Dispatch `Actor` session values may include an `owner` GitHub login when a personal agent token authenticated the request; consumers render it as `(for <owner>)` and persist it wherever they preserve actor JSON.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| Envelope and subject contracts | `src/envelope.ts`, `src/subject.ts` | canonical TypeScript surface |
| Dispatch tool specifications | `src/dispatch-tools.ts`, `src/tool-schema.ts` | builders over `zodSchemaApi(hostZod)` |
| Go generation | `scripts/gen-go.ts` | writes the Envelope Go contract |
| Generated Go contract | `packages/envoy/internal/contracts/generated.go` | generated; do not hand-edit |
| Contract tests | `src/*.test.ts` | validation and schema drift coverage |

## Critical conventions

- `src/dispatch-tools.ts` is the source of the thirteen native Dispatch tools:
  `dispatch_issue`, `dispatch_ask`, `dispatch_edit_ask`, `dispatch_resolve_ask`, `dispatch_comment`,
  `dispatch_suggest`, `dispatch_message`, `dispatch_doc_edit`, `dispatch_doc_read`, `dispatch_request_approval`,
  `dispatch_artifact`, `dispatch_read`, and `dispatch_search`. It defines their names, descriptions,
  and field shapes; host adapters consume `dispatchToolSpecs` directly.
- `dispatch_issue` accepts optional initial labels (at most 20 labels, each at most 40 characters); project-document arguments accept the document's artifact id, slug, or filename.
- Build field shapes through `zodSchemaApi(hostZod)` so option bags apply to the
  host's Zod. Use `dispatchToolSchema(spec, zodSchemaApi(hostZod))` when the
  host validates a call so tool-level cross-field validation also applies.
- Do not hand-edit `packages/envoy/internal/contracts/generated.go`.
- If the envelope or subject shape changes, update its schema and regenerate the
  applicable Go output.
- Prefer backward-compatible additions when extending the envelope.
- Keep examples synchronized with the real receiver output (Slack team IDs,
  GitHub owner/repo segments, and native Dispatch keys).
