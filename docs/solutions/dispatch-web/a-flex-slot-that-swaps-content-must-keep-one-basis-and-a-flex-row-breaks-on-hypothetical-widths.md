---
title: "A flex slot that swaps its content must keep one flex-basis, and a wrapping flex row breaks lines on hypothetical widths, not rendered ones"
category: dispatch-web
tags:
  - flexbox
  - flex-basis
  - flex-wrap
  - line-clamp
  - swallowed-click
  - mousedown-blur
  - responsive-header
  - tailwind
  - playwright
date: 2026-09-14
status: active
module: packages/dispatch/web/src/features/issue/IssueHeader.tsx
related_issues:
  - "LEGION-62"
  - "sjawhar/legion#1088"
symptoms:
  - "a heading beside other controls is clamped to a few characters at one window width and shown in full a few pixels wider or narrower"
  - "the first click on any control after finishing an inline edit does nothing; the second click works"
  - "a `flex-wrap` row puts a long item beside its neighbours although the item is visibly wider than the space left"
  - "a Tailwind `grow` on one child halves the width of a sibling that used to take the whole remainder"
---

# A Flex Slot That Swaps Its Content Must Keep One Basis; a Wrapping Row Breaks on Hypothetical Widths

The issue header (`IssueHeader.tsx`) is one `flex flex-wrap` row holding three children: the
title slot (key, `<h1>` or `<input>`, pin), the state controls, and the details line. Three of
the five review rounds on #1088 were the same flexbox fact seen from three sides. Recording the
fact once so the next header, toolbar, or card row does not rediscover it.

## Flex line-breaking uses each item's hypothetical main size

When a `flex-wrap` container decides which items share a line, it sums the items' *hypothetical*
main sizes — `flex-basis` clamped by `min-width`/`max-width` — and never the width an item would
have after growing or shrinking. So:

- `flex-1` (`flex: 1 1 0%`) with `min-w-[12rem]` tells the algorithm the item is **12rem wide**.
  Neighbours whose own widths fit beside 12rem join the line, the item then grows only into what
  is left, and a title that needed 600px gets 70px and clamps. Main had this at 1279 (title 62px);
  the PR reproduced it at 1260–1279 with a badge, one label, Route and Subscribers (70–104px).
- `flex-auto` (`flex: 1 1 auto`) makes the basis the item's content width, so neighbours join the
  line only when they fit beside the *whole* title. That is the right default for a heading that
  must never be squeezed by siblings.
- `grow` on a sibling that used to have `flex: 0 1 auto` splits the free space with the heading
  instead of leaving it all to the heading. On a row where both are present, the heading loses
  half of what it had. `grow` was not needed for the sibling to scroll (see below); it was
  removed in round two.

Recognise it: measure `getBoundingClientRect().width` of the heading and check
`scrollHeight > clientHeight` (a `line-clamp` overflow) at a sweep of widths just below each
breakpoint (1200–1279 here). The band where a shared row barely fits is where it shows; a single
width such as 1024 can pass while 1265 fails. The regression lock in `e2e/issue.e2e.ts`
("gives the title the row's free space beside a short details line") sweeps 1024/1200/1279 with
a bare issue and 1265/1279 with a busy details line, with and without the GitHub link — the
link makes the line too wide to share a row, so the *no-link* case is the one that detects a
relapse.

## The slot's basis must not change with its content

Round three sized the slot `flex-auto` when it showed the `<h1>` and `basis-full` when it showed
the `<input>` (an input's intrinsic width is about 20 characters, so `flex-auto` would have let
the state controls jump up beside a short input). That looked harmless and broke every control
in the header in a way no test caught until the reviewer pressed a button by hand:

1. The user finishes a title edit by clicking Close (or Status, the pin, Edit labels…).
2. `mousedown` on the new target blurs the input. React flushes the discrete `onBlur` →
   `setEditingTitle(false)` synchronously; the slot's basis changes; the whole row re-lays out.
3. `mouseup` lands on whatever is now under the pointer. Chromium dispatches `click` to the
   common ancestor of the mousedown and mouseup targets — the wrapping `<div>` — so the button's
   `onClick` never fires. The title saved; the second action silently did nothing.

The rule: **whatever a slot renders, its flex-basis is one value per breakpoint.** In the header
that is `basis-full 2xl:basis-auto` for view and edit alike — below 2xl the title always owns its
row and the state controls and details line share the row beneath it; from 2xl the slot is
content-sized (`grow` kept, so it is main's `flex: 1 1 auto`). The cost is one extra row where
main fitted title, state and details in one (a bare issue at 1200–1279: 124px vs 72), accepted
by the architect over a hidden-mirror-span alternative because it is one deterministic layout
with no measuring element.

Recognise it: any conditional class on a flex item whose condition is toggled by focus or blur.
Test it with a real pointer sequence, not `locator.click()` — Playwright's `click()` re-resolves
the element between events and will not catch it:

```ts
await page.getByRole("heading", { level: 1 }).click();       // open the editor
await expect(page.getByLabel("Issue title")).toBeFocused();
const box = await control.boundingBox();
await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
await page.mouse.down();
await page.mouse.up();
// then assert the control's effect (aria-pressed flipped, status PATCHed, banner shown)
```

`issue.e2e.ts` "delivers the click that ends a title edit to the control under the pointer" does
this for the pin and Close at 1024 and 1279; it was red on the round-three head and green after.

## A nowrap scroll container needs `min-w-0`, not `grow`, and is a real box

The details line scrolls sideways inside the card instead of wrapping or widening the page. What
makes that work is `flex min-w-0 flex-nowrap overflow-x-auto` on the line plus `min-w-0` on
every flex ancestor up to the main column: `min-w-0` lets the line shrink below its content
width so `overflow-x-auto` has something to scroll; without it the line's implicit
`min-width: auto` pushes the card and the page wider (document `scrollWidth` 2129 at a 1280
viewport in the tester's first run). Two details that cost rounds:

- The container must be a box. The earlier `xl:contents 2xl:flex` trick (dissolving the line's
  wrapper so its children sit directly in the header row at one breakpoint) cannot scroll — a
  `display: contents` element has no overflow. Every breakpoint now keeps the wrapper.
- `grow` is irrelevant to scrolling. Alone on its line the container already fills the line
  (`min-w-0` + default `shrink`); on a shared line `grow` only steals from the heading.

Recognise it: `document.documentElement.scrollWidth === clientWidth` at 1280 and 390 with eight
30-character labels seeded through the API, while the line's own `scrollWidth > clientWidth`
and `getComputedStyle(line).overflowX === "auto"` (`labels.e2e.ts`).

## Two smaller facts from the same header

- Below 1280 `styles.css` forces every `input` and `select` to `font-size: 1rem` (the iOS
  focus-zoom guard) and every control to 44px, so Tailwind text-size utilities cannot shrink a
  phone pill. Width has to come from content: the priority select became `appearance-none`
  (no native arrow, like the status pill) with the unset label `Priority` instead of `Priority —`,
  which is what let Status, Priority and Close share a 316px row.
- A React inline callback ref (`ref={(node) => node?.focus()}`) runs on every render, so using
  it to autofocus a second field steals focus from the first on each re-render. Focus once from
  an effect keyed on the editing flag (`useEffect(() => { if (routeEditing) ref.current?.focus() }, [routeEditing])`).

## Related

- `docs/solutions/legion/a-layout-fix-is-tested-at-the-band-where-the-old-and-new-rule-disagree.md`
  — why this took three rounds and how to sweep the widths up front.
