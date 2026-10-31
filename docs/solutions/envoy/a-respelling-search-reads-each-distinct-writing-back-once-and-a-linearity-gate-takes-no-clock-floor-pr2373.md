---
title: "A respelling search reads each distinct writing back once, and a linearity gate takes no clock floor"
category: envoy
tags:
  - pmdoc
  - read-back
  - performance
  - linear-time
  - testing
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/pmdoc
---

# A respelling search reads each distinct writing back once, and a linearity gate takes no clock floor

Extends docs/solutions/envoy/a-writer-that-checks-its-output-by-reading-it-back-keeps-stored-bytes-only-when-the-new-reader-joins-every-step-of-the-old-ladder-INFRA-534.md.

- **Each new respelling flag multiplies the search; pay for it once per distinct writing.** The
  spellings `inlineWithEscapes` tries are a product of independent flags, and most combinations
  write the same bytes for a given run. Compute what the run needs once (`runNeeds`), make a
  spelling only where its flag can change a byte of this run, read each distinct writing back once
  (key it by the digest of its bytes, with how many readers it passed), and let the later ladder
  steps reuse those counts. Keep the list's order, so the spelling kept is the one the full search
  would keep.
- **Test the cost, not only the choice.** Add a paragraph that needs every flag and reads back in
  no spelling to the growth test, and a test that fails when one writing is read twice under the
  same reader (`readBackObserver`).
- **Never floor a timing in `growthVerdict`.** A coarse clock (gVisor's 10 ms step) is solved by
  `cputest.Measure`, which runs an operation enough times to fill its window. A floor on top of it
  reads a 1 ms sample as 10 ms and lets a quadratic regression through with up to ten times its
  bound. If a linear-time test fails only in a pod, look at how it measures, not at its verdict.

## Evidence

- Pull request #2373, commit e0b8910b: a paragraph failing every spelling made 574 read-backs of
  128 writings under one reader (`TestRenderReadsEachWritingBackUnderAReaderOnce`, red before) and
  cost about nine times main to write at 64 KiB and 256 KiB; after the change the worst case is the
  parse of the 128 distinct writings. `readsByBytes` and `runSpellings` are in `render_inline.go`.
- Commit c523ad7e floored the smaller sample at the clock's resolution so the growth test passed in
  a pod; main's #2396 then fixed the cause with `cputest.Measure`, and together they passed a
  synthetic `size²` timing that main fails. Commit 7dda41c7 removed the floor.
