---
title: "Reuse work across one write's calls with a per-call object that prepares after its checks, not a version-keyed cache"
category: envoy
tags:
  - performance
  - pmdoc
  - caching
  - error-order
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2447"
---

# Reuse work across one write's calls with a per-call object that prepares after its checks, not a version-keyed cache

When one write calls the same expensive function many times over one input, for example one quote
search or one mark per open anchor:

- Wrap the function as `NewX(input).Do(...)` and make the old one-shot function exactly that call.
  The two cannot then disagree.
- Prepare lazily, at the first `Do`, and only after the checks the one-shot function runs before it
  reads anything. Keep the preparation's error and return it from every later `Do`. Errors and
  their order then match the one-shot function. A caller that never reaches the first `Do` reads
  nothing.
- Do not use a cache keyed on a document head or version. Every write moves the key, so back-to-back
  writes never hit it, and the cache is memory the server keeps between requests.
- When a caller throws a miss away, give it a variant that returns the plain sentinel error. Do not
  build the miss's diagnostic text, which can cost a whole extra walk.
- Prove the reuse with two copies of the input written with one fixed client id, one per-call and
  one through the object. Check `bytes.Equal` on the encoded result, and check after each call that
  any table the object cached still equals a fresh one.

## Evidence

- `pmdoc/find.go`, `type QuoteFinder` (`prepared`, `err`, `NearestFirst`, whose miss is
  `ErrTargetNotFound` itself, ranking no nearest blocks) and `MarkWriter.Mark` (which reads
  `yTextRanges` only after `MarkRange`'s range, type and nesting checks).
  `FindQuote` and `MarkRange` are their one-shot calls.
- `ReplaceText` (`docs/mutation.go`) builds one finder and one writer for each upload. A dropped
  miss no longer ranks the nearest blocks, which costs 66–1,006 ms per miss on LINUX specs.
- The plan rejected the spec's suggested cache of flattened text per document head: it would be
  resident memory, and it would never hit, because each anchored comment writes a mark and so moves
  the head.
- `TestReanchorReuseMatchesPerCall` (`pmdoc/reanchor_test.go:249`) checks `bytes.Equal` states.
  A writer that reads its table before `Update`, or drops one `Format`, fails it. On a 150 KB
  document the upload's N80/N10 time ratio went from 5.37 at base to 0.88–1.07 at head.
