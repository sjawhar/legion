---
title: "A type census also searches the types that contain it, their re-exports and indexed-access types"
category: legion
tags:
  - contract-census
  - typescript
  - contracts
date: 2026-10-27
status: active
module: packages/contracts
related_issues:
  - "INFRA-367"
  - "sjawhar/legion#2389"
---

# A type census also searches the types that contain it, their re-exports and indexed-access types

Extends docs/solutions/legion/a-strict-schemas-new-required-field-is-censused-across-every-parser-rigs-and-stand-ins-included-LEGION-578.md.

- When a field of a shared type changes, a grep for the type's own name finds only some of its
  readers. Also search:
  - every type that holds it, such as `GraphReferences`, `GraphEdge`, `BlockLinks` and
    `BlockLink` for `GraphNode`;
  - each re-export of those types, such as `packages/dispatch/web/src/api/types.ts`, which
    re-exports `@legion/contracts`;
  - indexed-access types that reach the field without its name, such as `X["node"]` and
    `X["node"]["kind"]`.
- Write that search into the census, for example
  `git grep -nE '\b(GraphReferences|GraphEdge|GraphNode)\b' -- 'packages/dispatch/web/src/*.ts' 'packages/dispatch/web/src/*.tsx'`,
  so the reviewer re-runs it.

## Evidence

sjawhar/legion#2389 widened `GraphNode.changed_at` to `string | null`. Its census at 69d138bb
searched `git grep -ln 'GraphNode\b'` and reported `packages/envoy-client/src/dispatch-execute.ts`
as the only TypeScript reader. Round 2 (at 27cab638) found three dashboard readers the search
missed. They reach the node through the `GraphReferences` and `GraphEdge` re-exports at
`packages/dispatch/web/src/api/types.ts:137-138`:
- `features/refs/ReferencedBy.tsx`, through `GraphEdge["node"]["kind"]`;
- `features/refs/RefPreview.tsx`;
- `features/refs/reference-target.ts`, through `blockText(node: GraphReferences["node"])`.

None of them read `changed_at`, so nothing broke, but the census was wrong until round 2.
