# Contracts Package

Shared event contract surface for Legion/Envoy.

## Overview

This package is the language-neutral source of truth for Envoy event shapes.

Current scope:

- envelope schema/type and subject helpers
- model-facing Dispatch tool specifications built over an injected Zod surface
- Go output in `packages/envoy/internal/contracts/generated.go`

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| Envelope and subject contracts | `src/envelope.ts`, `src/subject.ts` | canonical TypeScript surface |
| Dispatch tool specifications | `src/dispatch-tools.ts`, `src/tool-schema.ts` | builders over a host-injected Zod |
| Go generation | `scripts/gen-go.ts` | writes the Envelope Go contract |
| Generated Go contract | `packages/envoy/internal/contracts/generated.go` | generated; do not hand-edit |
| Contract tests | `src/*.test.ts` | validation and schema drift coverage |

## Critical conventions

- Keep Dispatch tool shapes as builders over the exported `SchemaApi`; each host provides its own Zod.
- Do not hand-edit `packages/envoy/internal/contracts/generated.go`.
- If the envelope or subject shape changes, update its schema and regenerate the applicable Go output.
- Prefer backward-compatible additions when extending the envelope.
- Keep examples synchronized with the real receiver output (Slack team IDs, GitHub owner/repo segments, etc.).
