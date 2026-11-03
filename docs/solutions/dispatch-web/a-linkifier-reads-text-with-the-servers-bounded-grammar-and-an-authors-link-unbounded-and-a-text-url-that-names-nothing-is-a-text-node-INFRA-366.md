---
title: "A linkifier reads text with the server's bounded grammar and an author's link unbounded, and a text URL that names nothing becomes a text node"
category: dispatch-web
tags:
  - references
  - linkify
  - markdown
  - hover-card
  - grammar-parity
date: 2026-10-26
status: active
module: packages/dispatch/web/src/features/refs
related_issues:
  - "INFRA-366"
  - "sjawhar/legion#2210"
---

# A linkifier reads text with the server's bounded grammar and an author's link unbounded, and a text URL that names nothing becomes a text node

- Keep two parsers, and pick one per input:
  - **Plain text,** including a URL an author left as text: use the grammar the server's index
    uses (here `text.Extract`, with its length bounds). Then the page links exactly what the graph
    cites.
  - **A link an author wrote, a picture, or a reference the server returned:** use the request
    grammar (`text.ParseRef`, unbounded). Opening it is a read, and a read never fails on length.
- An anchor is text only when its words equal its own target. Read the target from
  `data-dispatch-href ?? href`, because Proof's serializer moves a `dispatch://` target out of
  `href` and empties it. Remember the forms that render as such an anchor: a bare URL,
  `<dispatch://…>`, `[X](X)`, and a `www.` URL whose target gains `http://`.
- When a text URL names nothing, do one of two things:
  - an `http(s)://` one stays the plain link it is;
  - a `dispatch://` one becomes its words: `anchor.replaceWith(document.createTextNode(text))`.

  Never leave `<a href="">` or a `data-dispatch-href` behind: it clicks to the current page, and
  the hover card's trigger still finds it.
- Before you change what the dashboard accepts, check the server's grammar. If the server refuses
  a form too (here upper-case `DISPATCH://`), leave it, so the page and the graph agree.
- Test each form in the real browser: no link, no trigger attribute, and no tooltip after a wait
  of a multiple of the hover-open delay constant, which the test imports, never a literal number
  of milliseconds.

## Evidence

INFRA-366 (sjawhar/legion#2210) spent four review rounds on which anchors count as text:

- ea0496c2 read a dashboard URL left as text with the bounded grammar.
- 4ff4dc39 added `<dispatch://…>` and `[X](X)`.
- b4e4a2d2 replaced the dead `<a href="">` that the second round left with a text node.

The parsers are `parseDispatchReference`/`referenceRouteFromHref` (bounded) and
`parseNodeReference`/`nodeRouteFromHref` (unbounded) in `web/src/features/refs/routes.ts`. The
swap is in `collectReferenceAnchors` (`RefLink.tsx`). The browser proof is
`e2e/pictures.e2e.ts`. It failed at e3d09251: a link was still there, and a tooltip opened on
hover. The upper-case check was a Go probe at f586da9d: `text.Extract` and `text.ParseRef` cite
nothing for `DISPATCH://CORE-1/spec`.
