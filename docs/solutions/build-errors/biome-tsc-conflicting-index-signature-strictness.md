---
title: Biome's useLiteralKeys and TypeScript's noPropertyAccessFromIndexSignature disagree on the same source
category: build-errors
tags:
  - typescript
  - biome
  - lint
  - monorepo
  - strictness
date: 2026-09-09
status: active
module: envoy-client
problem_type: build_error
component: tooling
symptoms:
  - "CI fails Biome lint on packages/envoy-client/src/dispatch-execute.ts for bracket access on a literal key (useLiteralKeys wants args.urgency)"
  - "CI fails claude-envoy-bridge's bunx tsc --noEmit on the same unmodified file for dot access on the same key (noPropertyAccessFromIndexSignature wants args['urgency'])"
  - "Neither package's own checks catch the conflict in isolation — only running both together does"
root_cause: config_error
resolution_type: code_fix
related_components:
  - claude-envoy-bridge
severity: medium
---

# Biome's useLiteralKeys and TypeScript's noPropertyAccessFromIndexSignature Disagree on the Same Source

## Problem

`packages/claude-envoy-bridge` resolves `@legion/envoy-client`'s `bun` export condition
straight to source (`packages/envoy-client/src/dispatch-execute.ts`, not a compiled
`dist/*.js`), so the bridge's own `bunx tsc --noEmit` — which sets
`noPropertyAccessFromIndexSignature: true` (`packages/claude-envoy-bridge/tsconfig.json:19`)
— type-checks envoy-client's raw file. envoy-client's own `tsconfig.json` has no such flag.

Before it was fixed in `sjawhar/legion#826`, `dispatch-execute.ts` typed its arguments and environment as bare
index signatures (`args: Record<string, unknown>`, `env: Record<string, string | undefined>`)
and read them with bracket access throughout (`args["urgency"]`, `env["LEGION_ISSUE"]`, …).
Bracket access on an index-signature type is exactly what
`noPropertyAccessFromIndexSignature` requires, so claude-envoy-bridge's `tsc` was fine with
it. But Biome's `useLiteralKeys` rule is syntax-only — it does not resolve TypeScript's types
— and flags any bracket access with a literal string key as "prefer `args.urgency`", entirely
unaware that the type it is looking at only allows brackets. envoy-client's own Biome run
(root `biome.json`'s `"recommended": true` ruleset, no local override) therefore failed lint
on the identical lines that satisfied the bridge's `tsc`. Switching to dot notation to please
Biome would have flipped the failure to `tsc`'s `TS4111` in the bridge.

## What Didn't Work

- Silencing either rule with a package-local override does not help: envoy-client's Biome
  config has no override (bracket access must stay flagged for `Record<string, unknown>`
  values that really are index-signature-only), and disabling
  `noPropertyAccessFromIndexSignature` in claude-envoy-bridge would remove a real safety net
  for every other index-signature type it checks (`process.env`, `EnvoyEnvironment`, …) —
  compare `packages/claude-envoy-bridge/biome.json:24`, which turns `useLiteralKeys` off
  *locally* for the bridge's own files precisely because the bridge needs bracket access
  elsewhere; that override does not reach envoy-client's source.

## Solution

Give the value a declared shape for the specific keys the module reads, intersected with the
pass-through index signature, instead of leaving it a bare `Record<string, T>`
(`packages/envoy-client/src/dispatch-execute.ts:33-46`):

```typescript
type ToolArguments = {
  readonly urgency?: unknown;
  readonly ref?: unknown;
  readonly issue?: unknown;
  readonly artifact?: unknown;
  readonly version?: unknown;
  readonly anchor?: unknown;
  readonly options?: unknown;
  readonly ops?: unknown;
} & Record<string, unknown>;

type ExecutorEnvironment = {
  readonly LEGION_ISSUE?: string;
} & Record<string, string | undefined>;
```

Every read the module does (`args.urgency`, `args.ref`, `env.LEGION_ISSUE`, …) then resolves
through the named property, not the index signature, so dot access is both a real,
type-checked field access (`noPropertyAccessFromIndexSignature` has nothing to flag) and a
literal-key access (`useLiteralKeys` is satisfied). Unnamed keys — forwarded to the API
unread — still fall through `& Record<string, unknown>` for callers that need them.

## Why This Works

`noPropertyAccessFromIndexSignature` only restricts a property when the *only* way TypeScript
knows about it is the index signature. Declaring the key as its own optional member gives
TypeScript a named field to resolve `obj.key` against, so the restriction no longer applies to
that key — while every other, undeclared key keeps requiring brackets. Biome's rule was never
the problem; it was correctly asking for the literal-key form the language allows once the
type stops hiding the key behind a signature.

## Prevention

- When a package's export resolves to another package's raw `.ts` source (a `bun`/`workspace`
  export condition, not a built `.d.ts`), that consuming package's `tsconfig.json` strictness
  applies to the source package's files verbatim. Check the strictest tsconfig among a
  source's *consumers*, not just its own, before assuming a `Record<string, T>` parameter type
  is safe.
- Prefer a declared shape over a bare index signature for any object whose keys a module reads
  by name; reserve `Record<string, T>` for values that are genuinely open-ended.

## Related Issues

- Fixed in `sjawhar/legion#826`.
