---
title: "A new optional field on a shared contract type breaks packages that never import your code"
category: build-errors
tags:
  - typescript
  - contract-census
  - exactOptionalPropertyTypes
  - go
  - struct-equality
date: 2026-10-31
status: active
module: packages/contracts
related_issues:
  - "sjawhar/legion#2546"
---

# A new optional field on a shared contract type breaks packages that never import your code

Extends docs/solutions/legion/a-type-census-also-searches-the-types-that-contain-it-their-re-exports-and-indexed-access-types-INFRA-367.md.

That note censuses the types that *hold* yours. A field added as optional needs three more rows,
and each one fails in a package whose source you never opened.

- **Census the forms that wrap the type**, not only the ones that contain it:
  `Required<T>`, `Partial<T>`, `Omit<T>`, `Pick<T>`. Under `Required<T>` a new optional member
  becomes required, so a literal that was exhaustive yesterday is an assignability error today.
  Repair it by narrowing the wrapper to the members the writer actually owns
  (`Required<Omit<T, "new_field">>`), never by filling the new member in: the writer does not know
  it, and filling it publishes a value the server would refuse.
- **Census the packages that compile your source under stricter compiler options than your own
  package sets.** A workspace dependency is type-checked as raw TypeScript by its dependents, each
  with its own `tsconfig.json`, so a spread that writes an absent member as `undefined` passes in
  your package and fails under `exactOptionalPropertyTypes`. Omit the key with a rest destructure
  instead of assigning `undefined`.
- **Census every value equality over the struct.** A member a read fills and a mutation's response
  leaves empty turns `==`, `!=` and `slices.Equal` over the whole value into a comparison across
  the read/write boundary. Replace each with a comparison over the fields the assertion is about,
  through one named helper that clears the new member, so the next such field needs one edit.
- **Run every package's typecheck before the first push**, not only the packages the diff touches.
  A failure in a package the diff never names reads as unrelated red and is believed last.

## Evidence

sjawhar/legion#2546 added three optional members to one contract: `AskOption.ID` and
`AskAnswer.ID` (`packages/envoy/internal/dispatch/model/model.go:1097-1112`,
`packages/contracts/src/dispatch-api.ts:1152,1162`) and `Ask.Revision`
(`model.go:872`, `dispatch-api.ts:860`). They reached three packages three ways.

- `packages/pi-legion/scripts/skill-scenarios/seed.ts` declared its turn state's options as
  `Required<AskOption>[]`, so the new optional `id` became required: TS2322 in `seed.ts` and
  TS2769 in `seed.test.ts`, in a package the change never otherwise touched. `1b769383daf4`
  narrowed it to `Required<Omit<AskOption, "id">>[]` (`seed.ts:375`).
- `packages/claude-envoy` type-checks `packages/envoy-client`'s raw source under
  `exactOptionalPropertyTypes: true` (`packages/claude-envoy/tsconfig.json:19`), which
  `envoy-client`'s own tsconfig does not set — the same asymmetry as
  docs/solutions/build-errors/biome-tsc-conflicting-index-signature-strictness.md. The grouping
  `{ ...edge, block: undefined }` added in `6104c247a13c` and copied in `d1ea6c2e1226` is
  TS2375/TS2379 there; `1b769383daf4` replaced it with
  `const { block: _block, ...atPart } = edge` (`packages/envoy-client/src/dispatch-execute.ts:2454`).
- Three assertions of `main`'s own
  `packages/envoy/internal/dispatch/api/ask_block_edit_settlement_test.go` compared whole
  `model.AskOption` values across that boundary — one `!=` pair against struct literals and two
  `slices.Equal` — where a read carries the id and a mutation's response does not. The merge
  `1b191490e1ba` added `askOptionText` (the option with its id cleared, `:89`) and moved those
  three to it (`:1008-1009`, and `:1067` and `:1124` as `slices.EqualFunc`), leaving the two
  read-against-read comparisons as `slices.Equal` (`:1085`, `:1132`).

Both TypeScript failures were red in CI while the first review was being written, and the repair
cost a whole review round.
