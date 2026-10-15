---
title: "A browser test of an editor node view scrolls the drop target first, clicks far below the fold with nothing focused in Firefox, and proves in-app navigation with a page marker"
category: testing
tags:
  - playwright
  - drag-and-drop
  - firefox
  - prosemirror
  - e2e
  - dispatch-web
date: 2026-10-12
status: active
module: packages/dispatch/e2e
related_issues:
  - "LEGION-695"
  - "sjawhar/legion#1928"
---

# A browser test of an editor node view scrolls the drop target first, clicks far below the fold with nothing focused in Firefox, and proves in-app navigation with a page marker

- **Before `locator.dragTo(target)`, scroll the target into view and assert the source is still in
  the viewport.** Playwright auto-scrolls the target at the move, after the press, and desktop
  Chromium begins no drag when the page scrolled between the two (observed: mousedown on the
  source, mousemove on the target, mouseup, click — no `dragstart`, document unchanged). The
  failure reads as a product bug ("the node did not move"), not as a harness one. The phone
  project's viewport fits both, so the row passes there and fails on desktop alone.
- **A click lock for a focusable element inside a node view clicks it on a fresh load, far below
  the fold, with the editor never focused, in Firefox.** The same click passes in Firefox on a
  five-line document and in Chromium on any; the engine difference shows only when ProseMirror's
  focus handler has somewhere else to scroll the caret to (forty filler paragraphs, nothing
  clicked). Add the spec to the `firefox` project's `testMatch` and to the exhaustive list of its
  specs in `packages/dispatch/AGENTS.md`, with the reason.
- **`expect(page).toHaveURL(...)` cannot tell an in-app navigation from a full page load.** When
  the behaviour promised is the SPA's click handler (editor state and scroll kept), set a window
  property with `page.evaluate` before the click and expect it to survive after the URL changed.
- **Read the stored document back, not the DOM, after a drag.** The defect a wrong drag source
  causes is written into the markdown (the browser's link HTML as a link mark) while the DOM looks
  plausible; poll the text route until the order changes, then assert the node's line occurs once
  and the invented words do not.
- **When the suite's rows are on Chromium only, say so in the proof.** The tester's independent
  Firefox run is what found the focus defect; a proof that names one engine lets the next role pick
  the other.

## Evidence (LEGION-695, sjawhar/legion#1928)

`packages/dispatch/e2e/pictures.e2e.ts` Test B drags the picture view's fallback link onto the last
paragraph: without the pre-scroll the chromium row saw no `dragstart` at 1280×720 (the paragraph
sat at y=756), with it the drag went through on both projects; the row polls
`GET /api/v1/issues/<KEY>/artifacts/<slug>/text` for the moved order and asserts one occurrence
and no `[KEY slug](`. The tester's `e2e/picture-link-click.e2e.ts` builds forty paragraphs and
clicks the link on a fresh load; it was red on firefox alone (URL still `/spec`) and green on
chromium and iphone until fcf0ad1b. In-app navigation was proved only by hand, with
`window.__marker = "kept"` surviving the click in both engines (`.legion/LEGION-695/implement.json`
round-3 proof); both specs still assert `toHaveURL` alone, the marker line is the open improvement.
