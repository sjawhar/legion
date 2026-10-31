---
title: "Drop forged attributes after the schema judges the block, and fail closed when the type lookup misses"
category: dispatch-web
tags:
  - untrusted-input
  - typed-blocks
  - proof-editor
  - remark
date: 2026-10-28
status: active
module: packages/proof-editor
related_issues:
  - "INFRA-612"
  - "sjawhar/legion#2405"
---

# Drop forged attributes after the schema judges the block, and fail closed when the type lookup misses

Extends docs/solutions/dispatch-web/a-text-body-draws-typed-blocks-through-renderblock-and-reads-no-server-owned-attribute-INFRA-526.md.

- Run the schema's accept-or-refuse check on the attributes as written. Drop the server-owned
  attributes only after that check accepts the block. If you drop them first, a forged value the
  schema must refuse (`:::ask{state="bogus"}`) disappears, and the block is accepted as valid.
- Drop them once, in the parse transform that decides each block. Do not let the parse write them
  and then reset them in a later pass over the built tree.
- If the helper that drops them cannot find the block's type, throw. Never return the attributes
  unchanged: that skips the one step that removes forged values, and nothing reports it.
- Test both orders with a mutation: skip the drop (a forged `state="answered"` must read as open),
  and drop before the refusal (`state="bogus"` must stay fence text).

## Evidence

- `packages/proof-editor/src/read-only-directives.ts:60-64` runs main's `remarkDecideTypedBlocks`
  first, then `dropServerAttributes` (`:32-37`). Every container directive that is left after the
  decision is one that the decision accepted.
- `clientAttributes` throws at `:16` when the type is missing. The first version returned
  `node.attributes` unchanged there. The reviewer flagged it in round 1 and it was fixed in
  `fa623f9a`.
- `packages/proof-editor/tests/read-only-directives.test.ts` (its last two cases): with the drop
  skipped, Expected `"open"`, Received `"answered"`. With the refusal checked on the stripped
  attributes, the `state="bogus"` case fails.
- The engine-side reset (`withoutServerAttributes` in `packages/dispatch/web/src/features/refs/markdown-engine.ts`)
  was deleted in `3510c56b`.
