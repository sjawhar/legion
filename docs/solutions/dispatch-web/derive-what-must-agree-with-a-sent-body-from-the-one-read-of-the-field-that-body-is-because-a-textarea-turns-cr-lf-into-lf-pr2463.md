---
title: "Derive what must agree with a sent body from the one read of the field that body is, because a textarea turns CR LF into LF"
category: dispatch-web
tags:
  - textarea
  - line-endings
  - offsets
  - editor
date: 2026-10-29
status: active
module: packages/dispatch/web/src/features/conversation
---

# Derive what must agree with a sent body from the one read of the field that body is, because a textarea turns CR LF into LF

Extends docs/solutions/integration-patterns/hand-a-code-blocks-text-to-a-line-splitting-library-with-its-line-ends-read-as-lf-pr2311.md.

Offsets, ids or hashes that must line up with a request body are computed from the exact string the request sends, read once when the person presses Send or Save. Never compute them from a parallel copy of the text: React state, the stored original or a prop.

A browser text control is a normalizing copy. `HTMLTextAreaElement.value` returns LF where the value set on it had CR LF. So a composer that seeds its state with a stored CR LF body holds a different string from the field it sends, even when nobody typed.

- Read the field once into a variable, and hand that variable both to the body and to whatever derives data from it.
- In a jsdom test, patch `HTMLTextAreaElement.prototype.value`'s getter to normalize CR LF, since jsdom keeps the text as set.

## Evidence

In sjawhar/legion#2463 the dashboard's comment editor sends block ids with an edit. `carriedIdsOf` was given the composer's `body` state (CR LF, from the stored comment), while the wire carried the field's LF text. So an unchanged save of a comment stored as `Alpha first.\r\n\r\nBeta second.` matched no span and carried no ids. Commit `30157984` passes the read field text to both `draft.body` and `carriedIdsOf` (`MentionComposer.tsx:892-909`). On the harness the same save then sent `base_revision: 1` with `b1` at 0 and `b2` at 14, where the build before sent the body alone. `MentionComposer.test.tsx:889` fails at `881b03e5`.
