---
title: "A visual check measures each element against its own surroundings, never two shots against each other"
category: testing
tags:
  - playwright
  - screenshots
  - pixels
  - device-pixel-ratio
  - negative-control
  - dispatch-web
date: 2026-10-27
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-527"
  - "sjawhar/legion#2322"
---

# A visual check measures each element against its own surroundings, never two shots against each other

Extends docs/solutions/testing/a-browser-race-with-no-in-page-signal-is-proven-by-a-held-read-a-margin-a-red-run-justifies-a-sampled-settle-and-one-shot-assertions-INFRA-303.md and docs/solutions/testing/a-point-probe-places-what-it-measures-and-asserts-its-margins-before-the-checks-that-need-them-INFRA-95.md.

Those documents say to settle by sampling and to prove a check with an N-of-N `--repeat-each` red
run. For a check that reads pixels:

- **Assert an absolute property of each element, not "these two look different".** For example:
  at least a quarter of the ticked box's middle is ink, and at most a twentieth of the unticked
  box's. A relational check (byte equality of two shots, or "N% of pixels differ") is met by
  antialiased edges and by two identical elements that sit at different fractional positions, so
  it can pass without the fix.
- **Measure each element against the background around it.** Shoot a clip of the element's
  `boundingBox()` padded by a few CSS pixels and widened to whole CSS pixels. Take the background
  as the per-channel median of the clip's outer device-pixel ring. Read only the element's middle
  (its rect inset by a quarter on each side), so a one-pixel shift never moves a border into it.
  Scale by `image width / clip width`, so the same code holds at DPR 1 and DPR 3.
- **Take the shot once two in a row match with the element's rect unchanged.** The page can move
  for a frame between reading the rect and the shot, and the shot then shows whatever passed under
  the element.
- **Pick thresholds from logged values on both sides.** Log the measured share with the fix and
  without it on every project and scheme, then set each threshold well inside the gap.
- **Decode in the page** (`createImageBitmap` on a `Blob`, then an `OffscreenCanvas`), so the test
  needs no PNG library.

`steadyShotOf`, `inkedShareOfMiddle` and `expectTickedFilledUntickedEmpty` in
`packages/dispatch/e2e/task-items.e2e.ts` are this check. Move them into a shared e2e module for
the next pixel check, rather than copying them.

## Evidence

INFRA-527 (sjawhar/legion#2322) needed a browser check that a ticked and an unticked task-item
checkbox look different, also under forced colors. It took three versions:

1. Byte equality of the two boxes' element screenshots. With the forced-colors fix removed it
   still passed: two boxes drawn alike differed in a few faint edge pixels.
2. At least 10% of pixels differing by 128 or more in a channel. It failed its first negative
   control, but review round 2 showed it depended on position. The tester saw it pass once
   without the fix in Chromium dark (both boxes empty outlines), then fail 3 of 3. A probe of two
   identical boxes at offsets of 0 to 0.9 px gave 0.102 to 0.122 strong pixels at DPR 1, which
   passes. At DPR 3 with a 0.9 px offset the two shots were 42x42 and 42x45, so the same-size
   assertion failed with the fix in place.
3. Each box against its own ring (commit 5e832b77). Measured shares: ticked 0.500 to 0.626 and
   unticked 0.000 with the fix, ticked 0.000 without it, identical across 5 repeats. Thresholds
   0.25 and 0.05. `--repeat-each=5`: 50 of 50 passed with the fix, and 20 of 20 forced-colors runs
   failed without it. Before the steady-shot wait, one run read the unticked box at 0.400: the
   22x22 shot had caught a line of text sliding under the box.
