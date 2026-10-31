---
title: "A tolerance is measured by setting it to zero, and ships with its separation stated"
category: testing
tags:
  - measurement
  - thresholds
  - playwright
  - dispatch-e2e
date: 2026-10-29
status: active
module: packages/dispatch/e2e
related_issues:
  - "sjawhar/legion#2502"
---

# A tolerance is measured by setting it to zero, and ships with its separation stated

Extends docs/solutions/testing/a-visual-check-measures-each-element-against-its-own-surroundings-never-two-shots-against-each-other-INFRA-527.md.
That document says to pick a threshold from logged values on both sides and set it well inside the
gap. The delta is how to get the legitimate side's value when the quantity is not a pixel share,
and what to do when the gap is not there.

- **Set the bound to zero and run the row N times at the head.** Every legitimate sample then
  fails it, and the failures report the largest value the machine produces. That is cheaper and
  harder to fool than a print statement, and it is one line to undo.
- **Measure in every project the row runs in**, not only the one you develop in: a phone
  emulation's layout gives a different scale.
- **Take the fault's own value from the control**, and state both numbers and the factor between
  them in the constant's comment, with the sample count and the engines.
- **When the two are not separated, report it rather than raise the bound until the row passes.**
  A number chosen to make a row green is a number that will let the fault through.
- **A bound stays measured only while the quantity does.** When a later round changes how the
  samples are read, set it to zero and run again.

## Evidence

The past-version row of sjawhar/legion#2502 fails when the page's scroll falls more than
`MAX_SCROLL_FALL_PX` below the highest offset it reached
(`packages/dispatch/e2e/deep-links.e2e.ts`). With the bound at 0, the row passed 20 of 20 in each
of chromium, iphone, webkit and firefox: the landing scroll only goes down, so the largest
legitimate fall is 0 px, at landing peaks of 1980 px on desktop and 2395 px on iPhone. The
injected collapse falls 990 px. The shipped bound is 64 px — above every measured adjustment with
room for an engine's anchor nudge, and more than fifteen times below the fault — and its comment
carries all of those numbers.

The plan had required a separation of at least five times before any number could ship, and an
explicit report to the architect otherwise.

A later round removed a `slice` from the loop that reads the samples, which changed nothing in
principle. The tester set the bound back to 0 and re-ran 20 of 20 in chromium rather than carrying
the earlier measurement forward.
