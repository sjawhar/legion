---
title: "A twin reader in another language shares one corpus whose refused rows name the reason"
category: testing
tags:
  - contracts
  - go
  - typescript
  - shared-corpus
  - parity
date: 2026-10-30
status: active
module: packages/contracts
---

# A twin reader in another language shares one corpus whose refused rows name the reason

## The rule

- When a TypeScript reader must decide exactly as a Go one does, give both one table of values.
  Write it in TypeScript, generate its JSON copy for Go from it, and have the TypeScript test
  assert that the JSON equals the table. Never edit the copy by hand.
- A row records whether the value is accepted. A refused row also carries a `problem`: a part of
  the reason both readers must give. Both tests assert it.
- A twin that refuses for a different reason than the original reads a different grammar, even
  when both refuse. Restate the original's grammar shape by shape, and add a row for each shape
  where they parted.

## Evidence

- In sjawhar/legion#2394, `DISPATCH_BLOCK_POINTER_VALUES` (`packages/contracts/src/dispatch-block-ref.ts`)
  feeds the TypeScript test, and its copy `pmdoc/testdata/block-pointer-values.json` feeds Go's
  `TestABlockPointerAttributeDecidesTheSharedCorpus`.
- The TypeScript reader of a comment's or message's block took an issue key in the
  `artifact/<slug>/comment/<id>` form, and any item id. Go reads that form only for a project
  key, and refuses an id that does not decode (`text/refs.go`'s `parseDispatch` and
  `itemSegment`).
  - So `dispatch://CORE-1/comment/%ZZ#b-x` was refused in both languages, but TypeScript said "is a
    block of a comment" where Go said "is not a block reference as Dispatch writes one".
  - The accept/refuse column could not see that. Review round 12 found it by reading the code.
  - The fix narrowed the TypeScript grammar and added two rows that carry Go's wording as their
    `problem`. Before the fix, the reader gave the wrong reason for both rows.
- Disabling the comment-and-message refusal in either language fails the five rows that carry
  its `problem` in both tests.
