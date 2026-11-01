---
title: "A line pitch is read on the mounted route, and a pixel figure names its font and its rounding"
category: testing
tags:
  - dispatch-web
  - css
  - cascade-layers
  - tailwind
  - fonts
  - geometry
  - playwright
date: 2026-10-26
status: active
module: packages/dispatch/web
related_issues:
  - "INFRA-304"
  - "sjawhar/legion#2218"
---

# A line pitch is read on the mounted route, and a pixel figure names its font and its rounding

Extends docs/solutions/testing/a-point-probe-places-what-it-measures-and-asserts-its-margins-before-the-checks-that-need-them-INFRA-95.md.

That document says a measured fact holds only for the population it was measured on. Two deltas
for any figure about Dispatch's text geometry (a line height, a padding, a clearance):

- **Read the pitch with `getComputedStyle` on the real route, with the feature mounted, never
  from the utility classes or a scratch page.** Tailwind v4 puts its own rules and
  `@tailwindcss/typography`'s inside cascade layers, and a declaration in no layer beats every
  layered one whatever its specificity. The document editor loads proof-editor's
  `packages/proof-editor/src/lib.css` when it mounts (`web/src/features/doc/editor.ts`), in no
  layer, and its `.milkdown p { line-height: 1.6 }` overrides Typography's 1.75: a document is
  16px on 25.6px, not the 16px on 28px that `prose` alone gives. A scratch page built from
  `styles.css` never loads that file, so it cannot show this.
- **A figure measured in a pod or in CI is a DejaVu Sans figure.** No other sans font is
  installed there, so every pixel the browser suite reports is DejaVu's. A claim about another
  font is either measured through a `FONTCONFIG_FILE` overlay that installs it (the tester did
  this for Roboto and Inter) or written as arithmetic from that font's published metrics, saying
  so and saying which rounding it uses.
- **Chromium in the pod rounds a font's ascent and descent to whole pixels, each on its own.**
  DejaVu's text box is 14px at 12px, 16px at 14px, 18px at 15px and 19px at 16px: its ascent
  (0.928em) and descent (0.236em) rounded separately, which no single em ratio reproduces (at
  15px, 1.164em is 17.5px, and the box is 18px). Arithmetic from unrounded metrics can be off by
  half a pixel: Segoe UI at 14px on 20px is -0.22px from `r = 1.33` unrounded, and -0.6px if its
  ascent (15.1px) and descent (3.5px) are rounded the same way, which is untested on Windows.
  Both overlap, but a figure stated to two decimals without its rounding is a claim nobody
  measured.

## Evidence

- The plan's table and the first stylesheet note recorded document links at 16px on 28px, 4.4px
  of padding and 4.6px of clearance, from a scratch page. Review round 1 of sjawhar/legion#2218
  found the editor at 16px on 25.6px, and the tester confirmed it on the real
  `/issues/<key>/spec` page: line height 25.6px, padding 3.197px, clearance 3.41px, inside
  `.milkdown p`.
- The Segoe UI sentence went through two figures for 14px on 20px: the reviewer's "about 0.6px"
  and the implementer's -0.22px, each right under its own rounding. Neither was measured; no
  Segoe UI is installed in the pod. The note in `packages/dispatch/web/src/styles.css` gives the
  unrounded value and says it comes from published metrics.
- With Inter installed through the overlay, the existing message-turn row's `lowerClearance >= 2`
  precondition fails at 1.61px, a font sensitivity that CI, which has only DejaVu, cannot show.
