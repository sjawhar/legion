---
title: "A section that grows with server data inside a dialog is a height failure, and its live region needs aria-busy"
category: dispatch-web
tags:
  - dialog
  - layout
  - accessibility
  - aria-live
  - playwright
date: 2026-10-29
status: active
module: packages/dispatch/web/src/features
---

# A section that grows with server data inside a dialog is a height failure, and its live region needs aria-busy

Adding a section whose height follows what a server answered to a dialog breaks the dialog's own
actions before it breaks anything else, and gives a screen reader a new way to be talked over.
Four rules, each earned by a round that was sent back.

- **Cap the dialog's height and let it scroll, before the growing child lands.** The dashboard's
  other dialogs and sheets already do (`ReferencePicker`, `InboxDrawer`, `ShortcutHelp`,
  `SearchPalette`, `MarginSheet`, `DrillDown`, `SessionGroupsEditor`); a dialog without the cap is
  one nothing in it had ever grown. Cap below the wrapper's own padding rather than over it, so
  the sheet is capped at the window and the desktop card at the window less its offset.
- **The repository's fit helper measures width, not height.** `e2e/unclipped.ts`'s
  `assertPageFits` compares `innerWidth` with `scrollWidth`. A control painted past the bottom
  edge needs a vertical reachability check of its own: bring it into view as a browser would
  (`scrollIntoViewIfNeeded`), read its box against `page.viewportSize()`, and put the measurement
  in the failure message so a negative control can quote it.
- **Fill the section in the fixture by defeating the product's own duplicate guard.** Several
  near-identical titles seeded with `force: true` both fill the list and make the typed title a
  possible duplicate, which is the only path that renders the refusal's override — the control
  that sits lowest and is lost first.
- **A polite live region fed by a debounced read sets `aria-busy` for exactly the window in which
  no answer for the current input exists.** Otherwise a screen reader hears the pending line at
  every pause in someone's typing rather than the answer. Assert both values, and make removing
  `aria-busy` a different red row from removing the display gate, or one control covers for the
  other.

Two more that the same change earned:

- **Detail about a list item goes inside that item's `<li>`, through a `children` slot rather than
  a boolean flag.** A flag renders one fixed extra and forces the next host's detail outside the
  item, where a screen reader reads a bare link. Scope the test with
  `within(screen.getByRole("listitem"))`: a bare `getByText` passes wherever the text lands.
- **A query the dialog runs unconditionally is stubbed in that dialog's shared render helper, with
  `mockRestore` in its teardown, in the same change.** Otherwise every existing row of the suite
  makes a real network call for a reason unrelated to the change.

## Evidence

sjawhar/legion#2511, the Dispatch create dialog.

The tester's red row measured it on the real surface at 375x667, the phone window the dialog's
full-bleed sheet layout below `xl` is for: with the section full the dialog stood 712.53 px and
Create sat at 651.53-695.53, and after the server's `POSSIBLE_DUPLICATE` refusal `Create anyway`
sat at 723.53-767.53, wholly off screen, with nothing to scroll — `document.scrollingElement`'s
`scrollHeight` equal to its `clientHeight`. Not phone-only: at 1280x720 the same refusal put
`Create anyway` 39 px of 44 below the fold. The fix is
`max-h-[100dvh] overflow-y-auto xl:max-h-[calc(100dvh-6rem)]` on the form; removing those classes
makes the row red 2 of 2 with its own message, "Create stands outside the 667px window: top
651.53125, bottom 695.53125".

The architect decided the announcement separately: the section stays a polite live region and
carries `aria-busy` while a check is out. Removing `aria-busy` makes the display-gate row red 2 of
2 at `expect(related).toHaveAttribute("aria-busy", "true")`, `Expected: "true", Received: ""`.

The `children` slot replaced a `withAsk` flag in review: with the flag, the past decision's
"Answered by …" line and its ask state rendered after the `</ul>`, so the list item that named the
decision held nothing but a link. Moving them back outside the `<li>` fails the unit row with
"Unable to find an element with the text: /Answered by alice/", printing the link-only item.
