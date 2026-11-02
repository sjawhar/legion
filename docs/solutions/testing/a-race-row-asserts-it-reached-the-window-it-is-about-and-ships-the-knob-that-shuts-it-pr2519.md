---
title: "A race row asserts it reached the window it is about, and ships the knob that shuts it"
category: testing
tags:
  - playwright
  - race
  - negative-control
  - annotations
date: 2026-11-01
status: active
module: packages/dispatch/e2e
related_issues:
  - "sjawhar/legion#2519"
---

# A race row asserts it reached the window it is about, and ships the knob that shuts it

A test for a timing defect has two ways to pass: the defect is gone, or the row never reached the
moment the defect lives in. Separate them in the row itself.

- Assert the defect at zero, and assert each precondition of the window above zero, as its own
  expectation with its own message. Name in that message the shape parameters the row ran at, so
  a failure says "raise the count", not "the change is broken".
- Push every count onto `test.info().annotations` before asserting, pass or fail, so a green run
  carries the evidence that it measured something.
- Count only what the defect itself cannot explain. A signal that every run produces before the
  first paint proves the page loaded and nothing more.
- Ship an environment override whose only purpose is to shut the window, show the guard going red
  under it, and record that red. Make the override refuse a malformed value, naming the variable
  and the value, rather than falling back to the shipped shape under a title that claims another.
- Record in a comment the shape that was green at both heads, so the next person does not shrink
  the row back to it.

## Evidence

`packages/dispatch/e2e/heavy-streamed-turn.e2e.ts` (sjawhar/legion#2519), the row "a reader who
opens the live view while a long turn streams is never shown a frame's text in front of the
body".

Three expectations end it. `inFront.length` is the defect, held at zero, its message carrying the
widest sample's characters and blocks (`:299-304`). `partBuilt.length` must exceed zero — "no
paint of this turn ran across tasks, so this row measured none of the window it is about"
(`:316-321`). `framesInside.length` must exceed zero — "no frame was published while a paint was
part-way through" (`:322-328`). Both window messages end with "at ${streamedParagraphs}
paragraphs and ${streamedFrameGapMs} ms a frame", and the comment above them says what a failure
with no override set means: "the shape as outgrown by the node, not the change as broken: raise
the paragraph count" (`:312-313`).

All three counts go onto the result first: `test.info().annotations.push({ … type: "recorded
while the turn streamed" })` (`:284-290`), "kept on the result even when it passes: a runner too
fast or too slow to reach the window is read here rather than as a silently empty green"
(`:281-283`).

What the row deliberately does not count is the frame's own text landing in the element: "before
the first paint, every mount writes it, whatever the node's speed and whatever this row is
measuring, so counting it proved only that the page had loaded" (`:272-277`).

`probed(variable, shipped)` reads the two overrides and throws
`${variable} must be a positive whole number of its own unit, not ${JSON.stringify(given)}` for
anything else (`:50-62`); its docblock says they exist "for one thing - driving this row with the
window it is about deliberately shut", and that "nothing in the suite, the harness or CI sets
either variable" (`:35-48`). The red it records: with `DISPATCH_STREAM_PROBE_PARAGRAPHS=2` the
row fails the first window guard, `no batch landed in a body already holding blocks in 63
recorded samples, at 2 paragraphs and 80 ms a frame` (pull request body, round 7).

The shipped shape is 4,000 paragraphs and 80 ms a frame (`:73`, `:76`), and the comment above it
records both the red at the code before the fix — "36 of 142 recorded samples held both in
Chromium, 27 of 96 in WebKit, 17 of 156 in Firefox, the widest standing 63,864 characters in
front of 250 blocks" — and the shape that proved nothing: "At 900 paragraphs with 150 ms frames
the pull request measured it green at both heads" (`:64-69`).
