---
title: "A point probe places what it measures, asks the framework for the hit test, and asserts its margins before the checks that need them"
category: testing
tags:
  - playwright
  - flake
  - hit-testing
  - actionability
  - assertion-order
  - dispatch-web
date: 2026-10-25
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-95"
  - "sjawhar/legion#2156"
---

# A point probe places what it measures, asks the framework for the hit test, and asserts its margins before the checks that need them

A browser check that asks "what is at this point" is three claims, and each needs its own
guarantee: that the thing being measured is where the check put it, that the point is the
element's hit target, and that the geometry the point was derived from still holds.

- **Place what you measure.** Scroll the element where the check wants it and hold until its rect
  stops moving. Do not measure wherever the page happens to be when a locator first resolves.
- **`toBeVisible()` is a non-empty box and nothing more** — not in the viewport, not unoccluded,
  not settled. It is not a gate for a point probe.
- **Ask the framework for the hit test.** `locator.click({ position, trial: true })` waits and
  retries until the element is attached, visible, stable, scrolled into view and the hit target at
  the action point, performs no click, and names the element that intercepted the point when it
  refuses. `document.elementFromPoint` is a one-shot snapshot that waits for nothing and names
  nothing. A `position` is measured from the element's **padding box**, which is what makes a
  probe inside padding go red when the padding is removed. Keep a hand-made hit test only for a
  claim a trial click cannot express, such as "nothing is here".
- **A fixed-offset probe rests on a margin: assert it, at the value the probe needs, before the
  checks that use it.** A probe one pixel below an element's text is inside its neighbour as soon
  as the neighbour's box comes within two pixels, and that margin moves with a font stack or a
  line pitch.
- **A guard asserted after the thing it protects never prints.** The protected check fails first,
  with its own message, and that message is what the next reader diagnoses — which is how a
  pitch regression is read as a padding regression. Assert every precondition where it is
  measured, before the checks that depend on it.
- **Prove a guard by building its condition, not by raising its bound.** Raising the bound shows
  the `expect` fires. Building the state the guard exists for shows the guard beats the failure it
  was written to pre-empt.
- **Derive the offset from the thing the check is about.** A probe taken from an element's own
  padded rectangle moves with the padding, so it cannot detect the padding's absence.

## Evidence

`packages/dispatch/e2e/phone.e2e.ts:393` ("an inline link keeps its line and grows its hit box
without covering its neighbours", iPhone profile) failed on six CI runs and 16 of 30 repeats in one
issue pod, on code identical to the code it passed on. Its gate was `toBeVisible()` on a seeded
link and its probe was `document.elementFromPoint` at the link's text bottom.

The cause, from the recorded element stack of an instrumented run (3 of 10 repeats failed): the
Conversation jumps to its newest turn about 1.3 s after load
(`packages/dispatch/web/src/features/conversation/use-follow-latest.ts`), and when its
`[data-turn]` query finds nothing it scrolls to the top instead. The seeded turn then sat at y≈515
of a 664 px viewport, inside the comment composer's fixed box (`form.z-[8].fixed.inset-x-0.bottom-16`,
y423 h177), so `elementsFromPoint` returned that form's `FIELDSET` and `closest("a")` was null —
the recorded `"not-a-link"`. Every failing repeat had `window.scrollY` 0; every passing one had it
at the jump's target. All four retained CI traces carry identical geometry, which is why no
geometry explained the flip. The landing defect itself is tracked separately as
dispatch://INFRA-303; the row was made independent of it either way.

After the fix: 30 of 30 repeats at each of three heads, against 16 failures of 30 at the parent
commit on the same machine and lane.

Three review rounds found the same two shapes, each time in a place the previous round had not
touched:

- A precondition asserted after the checks it protects. Round 1: the composer guard, 95 lines
  below the trial clicks, so in the state it names the first click's 5 s actionability timeout
  pre-empted it. Round 2: `lowerClearance`, below the same clicks and bounded at 0 where the probe
  needs 2. Round 3 found none.
- A measured fact stated beyond the population measured. The stylesheet note's Inbox pitch was
  measured in a `text-sm` fixture where the call site is `text-[15px] leading-relaxed`, and a
  comment called 24 px lines "the tightest pitch a body reaches" when the same change's own note
  records a `.prose` body at 12 px on 16 px lines.

When one instance of either shape is found, sweep the file for the rest: this change paid a review
round for each repeat.

The controls that closed them, in the tester's lane:

- `.prose a { padding-block }` set to 4px, bundle rebuilt, which puts the lower link's padded box
  one pixel below the upper link's text (`lowerClearance` 1): the fixed row is red 3 of 3 in ~6.9 s
  at the clearance assertion with its own message (`Expected: >= 2 / Received: 1`) and no
  `TimeoutError`; the previous head against that same bundle is red 3 of 3 with a bare 5 s
  `locator.click` timeout at the mutation-sensitive click — the misdiagnosis, reproduced and then
  removed.
- The same rule deleted (clearance widens to 5): the clearance guard is silent in five repeats and
  the row still reds at the mutation-sensitive click, so the new guard masks nothing beneath it.
- The settle disabled (`scrollIntoView` replaced by `window.scrollTo(0, 0)`): red 3 of 3 with the
  composer guard's own message and no `TimeoutError` in the run.

A reader-level measurement is worth its cost and may refute the premise it was written for: 240
taps per arm through Chromium's own touch events, the point recomputed before each tap, showed the
link is reached at that edge with the padding present **and** with it deleted — so the measurement
did not bear on the padding, and the page was left unchanged. A first attempt that tapped a fixed
point recorded 28 of 50 taps on the wrong link; that was the page moving 20 px under a stationary
finger when the unread marks cleared, which is the same class of error as the flake itself.
