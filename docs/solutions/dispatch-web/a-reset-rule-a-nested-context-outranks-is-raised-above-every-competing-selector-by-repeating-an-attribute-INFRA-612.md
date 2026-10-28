---
title: "A reset rule that a nested context outranks is raised above every competing selector by repeating an attribute"
category: dispatch-web
tags:
  - css
  - specificity
  - nested-blocks
  - typed-blocks
date: 2026-10-28
status: active
module: packages/dispatch
related_issues:
  - "INFRA-612"
  - "sjawhar/legion#2405"
---

# A reset rule that a nested context outranks is raised above every competing selector by repeating an attribute

Extends docs/solutions/dispatch-web/a-typed-block-that-can-nest-reaches-its-own-parts-only-through-child-selectors-INFRA-526.md.

When a component's reset rule (for example "first child: margin-top 0") loses inside a nested
context:

- List every rule that matches the element in that context, and compute each specificity. Check
  cascade layers first: an unlayered rule beats every layered rule
  (docs/solutions/testing/a-line-pitch-is-read-on-the-mounted-route-and-a-pixel-figure-names-its-font-and-rounding-INFRA-304.md).
- Raise the reset above the highest one by repeating one of its own attribute selectors
  (`[data-x][data-x]`). Say in the CSS comment which rules it must outrank and their specificity.
- Do not add `:not()` to the competing rule. That raises its specificity, and it can then outrank
  a third rule it used to lose to. Do not use `!important`.
- Fix both edges and assert both edges (top and bottom margin). One competitor can win on one edge
  and a second competitor on the other.

## Evidence

- A decision card drawn inside a reply in another decision's footer matched the footer's
  `prose-sm` rules `[data-dispatch-ask-footer] .dispatch-markdown :is(p, ul, ol, blockquote, pre)`
  (0,4,1) and `[data-dispatch-ask-footer] li .dispatch-markdown :is(ul, ol)` (0,4,2). These beat
  the content hole's first- and last-child rules at (0,4,0).
- `packages/dispatch/web/src/styles.css:364-384` repeats `[data-proof-block-content]` to reach
  (0,5,0), and its comment names both footer rules.
- `packages/dispatch/e2e/text-typed-blocks.e2e.ts:226-266` asserts `margin-top` and
  `margin-bottom` are `0px`. At base `a9f5c529` the top was `16px`. With only the first-child rule
  raised, the bottom of a nested list was still `8.00001px`, from the (0,4,2) rule.
- A `:not()` on the footer rules was rejected in the plan: it raises them to (0,5,1), and they would
  then outrank the (0,4,2) nested-list rule for a reply's own lists.
