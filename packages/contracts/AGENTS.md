# Contracts Package

Shared event contract surface for Legion/Envoy.

## Overview

This package is the language-neutral source of truth for Envoy event shapes.

Current scope:

- envelope schema/type
- subject helpers
- JSON Schema source
- Go contract generation into `packages/envoy/internal/contracts/generated.go`

## Where to look

| Task              | Location                            | Notes                             |
| ----------------- | ----------------------------------- | --------------------------------- |
| TS contract types | `src/envelope.ts`, `src/subject.ts` | current canonical TS surface      |
| Package exports   | `src/index.ts`                      | public package API                |
| Schema source     | `schemas/envelope.schema.json`      | canonical JSON Schema             |
| Go generation     | `scripts/gen-go.ts`                 | writes generated Go contract file |
| Contract tests    | `src/envelope.test.ts`              | basic shape validation            |

## Critical conventions

- If the envelope/subject shape changes, update the schema and regenerate Go output.
- Do not hand-edit `packages/envoy/internal/contracts/generated.go`.
- Prefer backward-compatible additions when extending the envelope.
- Keep examples synchronized with the real receiver output (Slack team IDs, GitHub owner/repo segments, etc.).
