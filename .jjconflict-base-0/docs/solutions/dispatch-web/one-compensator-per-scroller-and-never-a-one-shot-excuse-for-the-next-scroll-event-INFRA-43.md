---
title: "One compensator per scroller, and never a one-shot excuse for the next scroll event"
category: dispatch-web
tags:
  - scroll-anchoring
  - overflow-anchor
  - scrolltop
  - browser-engines
  - react-hooks
date: 2026-10-20
status: active
module: packages/dispatch/web/src/features
related_issues:
  - "INFRA-43"
  - "sjawhar/legion#2084"
---

# One compensator per scroller, and never a one-shot excuse for the next scroll event

- **Before you write code that moves `scrollTop` to hold a reader's place, decide whether the
  browser's own scroll anchoring is also live on that element, and record the decision at the
  element.** Two compensators on one scroller hide each other: each makes the other's absence
  invisible, in the browser that implements it.
- **The discriminator is when your compensation runs.** Compensating in the layout phase of the
  commit that changed the content leaves anchoring on, and the browser then has nothing to adjust
  (`features/shell/ViewportAnchor.tsx`). Compensating anywhere else — a deferred commit, a
  `MutationObserver`, a `requestAnimationFrame` — means taking anchoring away with
  `[overflow-anchor:none]` on the scroller, because the browser's adjustment lands after yours.
- **`[overflow-anchor:none]` is not a tip.** The same class is spent in this tree for the
  opposite purpose: `web/src/app.tsx` puts it on the navigation rail so Chromium stops anchoring
  on the rail and anchors inside `<main>` instead. Write down which of the two you mean wherever
  you spend it.
- **Never arm a flag that excuses "the next scroll event".** The browser may fire none (your
  adjustment moved nothing), or more than one, and the flag then survives into a gesture that is
  the reader's. Where a window is genuinely needed, bound it by something the browser cannot
  change under you — `features/margin/useCardHold.ts` counts frames for exactly this reason — and
  prefer removing the second compensator so no excuse is needed at all.
- **An excuse is engine-dependent even when the behaviour it excuses is not.** WebKit fires no
  anchoring adjustment, so an arm waiting to be consumed by one stays live there after every
  update, and the defect inverts between engines on code that reads identically.

## Evidence

The agent live view (`features/agent-view/use-live-follow.ts`) holds a reader's place while a
session writes, and compensates the reader's turn back to its offset after a reconnect rebuilds
the transcript. That settle runs in a deferred commit (the runtime applies a rebuild in a passive
effect), so Chromium's anchoring adjusted the same rebuild a frame later.

- Downward: with anchoring on, the browser row that exists to lock the compensation passed with
  the compensation deleted — Chromium held the turn. With `[overflow-anchor:none]` on the
  viewport the same mutation fails `Expected: "m12" Received: "m20"`, and passes restored.
- Upward: the hook tried to tell the anchoring adjustment from a reader's scroll with a one-shot
  fingerprint (the `scrollHeight` the settle left behind). When the settle moved `scrollTop` by
  nothing, no scroll event came to consume it, and the reader's next scroll back was swallowed:
  measured `position 300 atEnd true` against a control of `position 100 atEnd false` with no
  rebuild. On WebKit the adjustment never arrives at all, so the arm stayed live after every
  rebuild.
- A fold opened mid-screen is the same fact from the reader's side: with anchoring on it moved
  the reader by the fold's height; with it off the turn they were on stays put
  (`{"id":"m17","top":-20}` before and after).

Both faces went away with one line, not with a smarter excuse: the excuse branches were deleted,
and the hook now reads one signal, a fall in `scrollTop` after the at-the-live-end branch.
