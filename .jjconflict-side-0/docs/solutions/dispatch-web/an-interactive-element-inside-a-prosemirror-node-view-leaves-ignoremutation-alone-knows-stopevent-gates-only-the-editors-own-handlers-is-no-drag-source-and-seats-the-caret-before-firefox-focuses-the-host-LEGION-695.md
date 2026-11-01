---
title: "An interactive element inside a ProseMirror node view leaves ignoreMutation alone, knows stopEvent gates only the editor's own handlers, is no drag source, and seats the caret before Firefox focuses the host"
category: dispatch-web
tags:
  - prosemirror
  - node-view
  - milkdown
  - stopEvent
  - ignoreMutation
  - firefox
  - drag-and-drop
  - proof-editor
date: 2026-10-12
status: active
module: packages/dispatch/web/src/features/doc
related_issues:
  - "LEGION-695"
  - "sjawhar/legion#1928"
---

# An interactive element inside a ProseMirror node view leaves ignoreMutation alone, knows stopEvent gates only the editor's own handlers, is no drag source, and seats the caret before Firefox focuses the host

The Dispatch document editor is Milkdown over prosemirror-view 1.42.3, loaded with proof-sdk's
plugins through `@legion/proof-editor`. A plain-DOM node view that puts a clickable element (an
`<a>`, a button) inside its `contenteditable="false"` root meets four rules the library's
documentation does not spell out:

- **Do not override `ignoreMutation` on a view without a `contentDOM`.** The leaf default is
  `!this.contentDOM && mutation.type != "selection"` (`prosemirror-view/dist/index.js:1110-1111`):
  every DOM change the view makes to itself is already ignored, and the one record it keeps — the
  synthetic selection record — is the one `() => true` would swallow, leaving `state.selection`
  wherever the caret last was. A view that owns editable DOM (`AskBlockView`, which excludes its
  slots) is the only kind that needs one.
- **`stopEvent` gates ProseMirror's own handlers, and plugin `handleDOMEvents` only for event
  types ProseMirror itself handles.** `eventBelongsToView` (`:3173-3183`) runs for the built-in
  handlers (mousedown, keydown, dragstart, drop, copy, …). For a type with no built-in handler —
  `click`, `mousemove`, `mouseover` — `ensureListeners` (`:3160-3165`) registers a bare
  `runCustomHandler` listener that never asks `stopEvent`. So proof-sdk's `markdownLinkClickPlugin`
  (`handleDOMEvents.click` and `mousemove`) reaches any `a[href]` a node view renders: on a
  non-editable view it `preventDefault`s and opens the href in a new tab; on an editable view it
  shows its `Open link` hover card for every `a[href]` outside `[data-mark-id]` and lets an
  unmodified primary click through to `ProofDocument`'s article handler. Write the behaviour of a
  node view's link from that plugin's source, not from the absence of your own click handler.
- **An `a[href]` is the browser's drag source; set `draggable = false` on it.** ProseMirror sets
  `draggable` on the view's root when the node spec is draggable (`:1317-1322`), but the browser
  picks the nearest draggable ancestor-or-self of the mousedown target, and an anchor with an href
  qualifies. With `stopEvent` true for the anchor, `handlers.dragstart` never runs, `view.dragging`
  stays null, and the drop takes `handleDrop`'s external-HTML branch: the browser's own
  `<a href="http://…">words</a>` is parsed into the document as a link mark while the node stays
  put. With `draggable = false` the root is the source and the node moves. Return false from
  `stopEvent` for `dragstart` too, so a drag of a DOM selection made over the anchor's words also
  goes through ProseMirror; keep `copy`/`cut` stopped so a native copy of those words works.
- **Seat the editor's caret beside the node on a primary-button mousedown in the element.**
  Chromium focuses the `<a>` itself; Firefox focuses the editing host, and 20 ms later
  `handlers.focus` (`:3904-3916`) writes the editor's *current* selection into the DOM — the
  document's start for a reader who has not clicked into the text — which scrolls a long document
  away from the element before the mouseup, so the `click` target is the common-ancestor `<div>`
  and nothing navigates. Dispatching a collapsed `TextSelection` at `getPos() + node.nodeSize`
  from the mousedown makes that the selection ProseMirror restores; nothing scrolls, and the click
  lands with a collapsed selection, which the article handler requires. Chromium is unaffected: an
  unfocused editor moves its caret without touching the DOM selection (`editorOwnsSelection`,
  `:2261-2264`). Never `preventDefault` the mousedown instead: Firefox then starts no drag from
  the element.

Keep the node view's reason and its wiring in one module: `pictureNodeView(view)` in
`features/doc/picture-view.ts` returns the `NodeViewConstructor` that closes over `view` and
`getPos`, and `installTypedBlocks` installs it with one line.

## Open edges (recorded, not changed here)

- `stopEvent` is a denylist with one exception, so `dragover`/`drop` whose target is inside the
  anchor never reach ProseMirror: the anchor's words are a dead drop zone a few pixels wide.
- The `dragstart` exception is pinned by no test: the unit cases dispatch `mousedown`, and the e2e
  drag's real `dragstart` targets the wrapper because the anchor is not draggable.
- `ProofDocument`'s article handler steps aside for a non-collapsed DOM selection, and this anchor
  is the first in the editor with a real same-origin `href` (a link mark's is sanitized to `""`),
  so in that state the browser follows it with a full page load. Right page, lost editor state.
- The fallback link shows two hover cards in the live editor — `RefPreviewHost`'s on
  `data-dispatch-href` and the library's on `a[href]` — the one element that does.
- `installTypedBlocks` writes `image:` after spreading the library's node views, so a picture node
  view a later proof-sdk or Milkdown pin ships is replaced silently; grep an upgraded pin for one.
- The `<img>` is `loading="lazy"`, so an undrawable picture shows nothing until it is scrolled
  near, then becomes the link; any assertion about the link scrolls first.

## Evidence (LEGION-695, sjawhar/legion#1928)

Round 1's review traced `ignoreMutation: () => true` and `stopEvent` covering `dragstart`
(`.legion/LEGION-695/review.json` round 1, threads PRRT_kwDORFy7ds6rjBua and …Bud); 8af418cf removed
the override and set `link.draggable = false`. On `bun run dispatch:local` in Chromium and Firefox a
drag of the fallback link then moved the picture line once with no `[CORE-8 notes-md](` in the
stored markdown; with the line commented out and `web/dist` rebuilt, `pictures.e2e.ts`'s drag row
failed (the moved-order poll never became true). The tester's round 2 red test
(`e2e/picture-link-click.e2e.ts`, forty filler paragraphs, nothing clicked, Firefox) failed with the
URL still `/issues/CORE-1/spec`; its capture-phase probe saw the caret jump to the document's first
text and `scrollY` fall from 1956 to 342 between mousedown and mouseup. fcf0ad1b seats the caret;
with the one `placeCaretAfter()` call removed the Firefox row fails exactly so, with it 3 passed on
chromium, firefox and iphone. The `click`/`mousemove` rule is `markdown-link-click.ts`
`handleLinkClick` (`shouldOpenLinkForEvent(event, false)` → `window.open(href, "_blank",
"noopener,noreferrer")`) and `handleMouseMove` → `showCardForLink`, which the tester observed as a
`targetcreated` for the artifact page from the version view.
