---
title: "A collaborative binding deletes a node its schema refuses, so relax only validContent, from the server's list"
category: dispatch-web
tags:
  - y-prosemirror
  - yjs
  - prosemirror
  - content-rule
  - collaboration
date: 2026-10-29
status: active
module: packages/proof-editor
related_issues:
  - "sjawhar/legion#2433"
---

# A collaborative binding deletes a node its schema refuses, so relax only validContent, from the server's list

Extends docs/solutions/dispatch-web/every-milkdown-markdown-parse-path-needs-the-content-rule-check-registered-after-the-mark-plugins-and-locked-by-a-valid-block-INFRA-565.md.
That note covers the markdown parse paths. This one covers the path that builds nodes from the
shared Yjs document.

- If two concurrent edits can leave a node type holding content its rule refuses, the browser
  schema must accept that content when it builds the node. y-prosemirror builds every node with
  `createChecked`. When the build throws, it deletes the whole element from the shared document,
  and every peer and the server lose it.
- Replace only the node type's `validContent`. `createChecked`, a step's own check and
  `Node.check` read it. Keep `contentMatch`, `canReplace` and `createAndFill` strict, so commands,
  input rules, paste and parse still refuse to make the shape locally.
- Take the accept-set from the list the server validates against: one embedded file that Go and
  TypeScript both read, plus a drift test. Never write a second list by hand.
- Install the relaxation in both the live engine and the headless engine, before the document is
  bound. Then flag the shape on the server (settlement marks the block invalid), so the shape is
  kept and seen, and never silently rebuilt.

## Evidence

- `packages/proof-editor/src/ask-content.ts:24` (`relaxAskContentCheck`) is installed in
  `src/lib.ts` before the collab binding and in `src/lib-headless.ts` after `new Schema`.
- The accept-set is `packages/envoy/internal/dispatch/pmdoc/schema/block-node-types.json`, which
  `pmdoc/node.go` embeds, and the drift test in `packages/proof-editor/tests/ask-concurrent-edit.test.ts`
  reads it.
- Negative control: at the base `2df3353c`, `packages/dispatch/e2e/ask-concurrent-edit.e2e.ts`
  fails with the decision and all its words deleted in both editors and in storage.
