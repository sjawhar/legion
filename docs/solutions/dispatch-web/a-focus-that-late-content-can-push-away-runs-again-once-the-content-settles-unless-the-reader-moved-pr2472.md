---
title: "A focus that late content can push away runs again once the content settles, unless the reader moved"
category: dispatch-web
tags:
  - scroll
  - focus
  - deep-links
  - async-rendering
date: 2026-10-29
status: active
module: packages/dispatch/web/src/features/doc
related_issues:
  - "sjawhar/legion#2472"
---

# A focus that late content can push away runs again once the content settles, unless the reader moved

- When content above a link's target can still grow after the page scrolled to it (a diagram, an
  image, an embed drawn asynchronously), run the same focus once more when that content has
  settled. Do not correct the scroll as each piece lands: a script scroll during a smooth scroll
  cuts it short.
- Arm the re-run only while something is still pending. With nothing pending, nothing can move
  the target, and a second focus only pulses it twice.
- Drop the re-run on the reader's first press, wheel, touch or key (capture listeners on the
  window), so a reader who moved on is never pulled back.
- Send every entry point that focuses (a hash link, a margin card, a query-string link, a button
  that jumps to a block) through one function that owns the single pending re-run: a newer focus
  replaces the older one's, and unbinding the view drops it. A path that bypasses it is the bug
  the next review finds.
- Prove each entry point with a browser row that turns scroll anchoring off
  (docs/solutions/testing/a-mutation-that-stays-green-because-the-platform-implements-the-behaviour-too-INFRA-43.md),
  holds the content until the target has landed, then releases it.

## Evidence

sjawhar/legion#2472 added `refocusAfterDiagrams`
(`packages/dispatch/web/src/features/doc/refocus-after-diagrams.ts`) and first used it only for
the `#b-` link and `VersionView`. Review round 1 found the live document's `?comment=<id>` link
(focused once from `useMarginSheet.ts`) still landed off-screen after a late diagram: quote at
y=657 before the draw, y=10668 after. Round 2 routed every focus through `ProofDocument`'s
`focusInEditor`. An earlier version that always re-ran focused a past version's highlight twice,
and `VersionView.test.tsx` failed on it; the re-run now returns at once when
`mermaidDrawsWaiting()` is false. Negative controls: the bridge's `focusMark` called directly →
the comment-link row fails with `viewport ratio 0`; a wheel scroll after the link landed kept
`scrollTop` unchanged after the draw.
