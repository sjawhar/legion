---
title: "Two defects a unit row cannot catch, and how a CI-only detach race is reproduced"
category: testing
tags:
  - playwright
  - react-testing-library
  - fireEvent
  - flake
  - dispatch-e2e
date: 2026-10-31
status: active
module: packages/dispatch
related_issues:
  - "sjawhar/legion#2549"
---

# Two defects a unit row cannot catch, and how a CI-only detach race is reproduced

Two shapes pass a React unit row and fail in a browser. Write the browser row for them, with the
pre-fix head as the control.

- **Focus put into a node the same press reveals.** `fireEvent` wraps the click in `act()`, which
  flushes the render inside the press, so code that focuses an element React has not committed yet
  finds it and the row is green. In a browser the click only schedules the render, `document.activeElement`
  is still `BODY`, and every later key goes to the page. Put the focus where the node is drawn - the
  component that reveals it, on the commit that mounts it - rather than in the key handler.
- **An element a layout switch remounts.** A locator resolved at one width names a node a resize
  unmounts, so an action taken straight after `setViewportSize` races the unmount
  (`Element is not attached to the DOM`). Resolve the element again after the resize, through the
  helper that knows which layout a width draws.

Reproduce such a race before you fix it: `client.send("Emulation.setCPUThrottlingRate", { rate: 20 })`
on the page's CDP session, inside the suite's own timeout, slows the browser enough that the detach
beats Playwright's retry. Then run the same pair under a wider timeout. If old and new code both
pass there, the control discriminates at the margin only, and the report must say so.

## Evidence

- Focus: round 6 of this pull request. In a browser, one Enter left `aria-expanded="true"` with the
  box drawn and `document.activeElement === BODY`; with the fix's one line neutered the tester's row
  fails `toBeFocused ... unexpected value "inactive"`, while the defective code passed a one-press
  unit row. The fix is in the card that owns the disclosure (`AskCard.tsx`'s `reasonFieldShown`),
  not in the Enter binding.
- Remount: `e2e/references.e2e.ts:26` screenshotted the Inbox card straight after resizing 390 -> 1440 px.
  The probe printed `before connected:true` and `after connected:false`, one card drawn again. The row
  now resolves the card through `askCard` (`e2e/inbox-pane.ts`), opens that card's own backlink panel
  and waits for its text.
- Throttle: the old row copied into a scratch spec and run at `setCPUThrottlingRate` 20 under the
  suite's 30 s budget fails with CI's exact error, and is green unthrottled and at rate 6. At rate 20
  with a 120 s budget both copies pass - Playwright's detach retry has room - so that pair was reported
  as discriminating at the margin only.
