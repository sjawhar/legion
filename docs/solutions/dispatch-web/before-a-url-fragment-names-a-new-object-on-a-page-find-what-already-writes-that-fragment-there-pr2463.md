---
title: "Before a URL fragment names a new object on a page, find what already writes that fragment there"
category: dispatch-web
tags:
  - url-grammar
  - deep-links
  - references
date: 2026-10-29
status: active
module: packages/dispatch/web/src/features/refs
---

# Before a URL fragment names a new object on a page, find what already writes that fragment there

- Before a fragment such as `#b-<id>` gets a meaning on a URL shape, search the code that writes the page's hash (`rg -n 'location\.hash' packages/dispatch/web/src`) and the path builders for that page.
- When the page already writes that fragment for another object, the new object takes no fragment on that page: name it through its `dispatch://` reference, or a page of its own.
- Add the rows to the one shared href table that every parser tests against (`packages/contracts/src/dispatch-href.ts`), so the Go, web and envoy-client readers move together.

## Evidence

sjawhar/legion#2463 first read a document page's `?comment=<id>#b-x` as block `x` of the comment. `ProofDocument.tsx` writes the document block's hash on the same URL after a comment deep link, so a copied address named `comment/<id>#<document block id>`, a block that never exists, and the link dropped out of Referenced by. Review round 1 found it; the fix restores the rule that `?comment=` names the comment whatever its fragment, in Go `documentPage` (`text/refs.go`), web `pageRoutes` and envoy-client `dispatchRefFromUrl`, and `buildReferencePath` links a project-document comment's block to `?comment=<id>` with no fragment.
