---
title: "A page component mounted in a second surface names every page-wide claim it makes"
category: dispatch-web
tags:
  - react
  - proof-editor
  - reading-pane
  - singletons
date: 2026-10-31
status: active
module: packages/dispatch/web/src/features/doc
related_issues:
  - "sjawhar/legion#2549"
---

# A page component mounted in a second surface names every page-wide claim it makes

Before you draw a page's component in a pane, drawer or preview, list what it claims for the whole
page: a keymap scope, a registration with another feature, a hash it follows, a toolbar it hands
up, a presence it joins, a write it allows. Gate each one on an explicit surface prop whose default
is the page's existing behaviour, and decide each separately - a single flag passed without that
list leaves a claim behind, and two mounts then fight over it.

Make the prop fixed for the life of a mount and name it in the dependency list of every effect that
reads it. A surface that can change mid-mount turns each of those effects into a teardown path
nobody tests.

A second surface that takes no edits also answers differently about what the reader may do: read-only
is the column's own rule, not the document's, and prompts that repair or rebuild the document belong
to the page that owns it.

## Evidence

`packages/dispatch/web/src/features/doc/ProofDocument.tsx` gained `surface?: "page" | "pane"` (`:145`,
default `"page"` at `:204`), and `const page = surface === "page"` (`:273`) gates every page-wide
claim the component had made unconditionally:

- `useKeymapScope(page ? "document" : null)` (`:403`) and the empty keymap beside it, so two mounted
  documents do not both own the `document` scope.
- the margin's document registration (`:431`), the search highlights and their `fragment.observeDeep`
  (`:459`), the `#b-` hash follow (`:521`) and the toolbar hand-up with its unmount clear (`:463`).
- `awareness` and `readOnly`, where the column's answer is not the page's (`:275`-`:281` for the
  repair and rebuild prompts, which a column offers neither of).

The invariant is written where the flag is read (`:269`-`:273`): `surface` is fixed for the life of
a mount, so every effect lists it and none is rebuilt for it.

The enumeration came before the code: `.legion/INFRA-575/plan.json`'s gap analysis 1.8, 1.10, 1.11
and 4.1 are the four claims found by reading the component rather than by running it, and each is a
line in the list above.
