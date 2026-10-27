---
title: "A control the page draws keeps its native element and hands its look back to the system under forced colors"
category: dispatch-web
tags:
  - css
  - accessibility
  - forced-colors
  - high-contrast
  - checkbox
  - appearance
  - mask
date: 2026-10-27
status: active
module: packages/dispatch/web
related_issues:
  - "INFRA-527"
  - "sjawhar/legion#2322"
---

# A control the page draws keeps its native element and hands its look back to the system under forced colors

When you restyle a native control (a checkbox, a radio, a select) with `appearance: none`:

- **Keep the native element.** Its role, its checked or selected state, its `disabled` (no tab
  stop, no keyboard shortcut swallowed) and what assistive technology reads all stay; only the
  look changes. Do not swap it for a `<span>` with ARIA.
- **Add `@media (forced-colors: active) { appearance: auto }` for it**, and drop there anything
  that draws its state (a mask, a background image). Under forced colors (Windows High Contrast)
  the browser forces every author fill and border to a system colour, so a state shown only by an
  author fill (a filled box against an empty one) disappears. With `appearance: auto` the system
  draws the control in its own high-contrast colours. Match the specificity of each rule you
  override (a `:checked` rule needs a `:checked` override).
- **Draw a mark on an `<input>` with a mask or a background over the fill, never `::before` or
  `::after`.** Firefox draws no pseudo-element on an input.
- **Prefer a mask that cuts the mark out of a fill taken from a token** (`background-color:
  var(--tw-prose-links)`, `mask: url("data:image/svg+xml,…")`) to an image that paints the mark in
  a colour. The mask's SVG sets only alpha (`white`, `black`), so the mark shows whatever is
  behind the control and contrasts with the fill in either scheme with no colour of its own.
  A colour written inside a data URI as `%23rrggbb` is not seen by
  `web/src/theme/styles-css-pin.test.ts`, which matches `#rrggbb` and `rgb()` only, so it would
  skip the palette pin.
- **Prove the forced-colors case in the browser:** `page.emulateMedia({ forcedColors: "active" })`
  in light and dark, with a check that fails when the `@media (forced-colors: active)` block is
  removed.

## Evidence

INFRA-527 (sjawhar/legion#2322) draws a read-only checkbox for each Markdown task item
(`web/src/features/refs/task-checkboxes.ts`; rules in `web/src/styles.css`, the task-item block
after `.dispatch-markdown [data-markdown-link]`).

- The first version used the native disabled box. Chromium paints a disabled checkbox pale grey,
  and on the light card its tick was hard to see, so the box was drawn with `appearance: none`.
- Review round 1 found that under emulated forced colors the drawn box showed a ticked and an
  unticked item alike: the fill became the system background (`rgb(255, 255, 255)` in light,
  `rgb(0, 0, 0)` in dark), and the tick, a `url()` image in a fixed colour, matched it. Adding
  `appearance: auto` and `mask: none` under `@media (forced-colors: active)` drew the native box
  with a visible tick.
- The same review found the tick colours `%23ffffff` and `%230f172b` inside the data URIs skipped
  the colour-pin test. The mask version writes no colour; `grep -n '%23[0-9a-fA-F]\{3,6\}'
  web/src/styles.css` prints nothing.
- `e2e/task-items.e2e.ts`, forced-colors rows: with only the `@media (forced-colors: active)` block
  removed, 20 of 20 runs failed (`--repeat-each=5`, Chromium and iPhone 13, light and dark); with
  it, all passed.
- Firefox and WebKit under emulated forced colors draw the disabled native box light grey with a
  faint tick (tester, round 2). Chromium uses the Windows High Contrast palette and passes. The
  handback is still right there: it is the system's own drawing of a disabled control.
