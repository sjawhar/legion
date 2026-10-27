---
title: "Judge an automated write by what it changes, not by the state or timing it arrived in"
category: dispatch-web
tags:
  - yjs
  - milkdown
  - pending-edits
  - review-rounds
  - invariants
date: 2026-10-27
status: active
module: packages/dispatch
related_issues:
  - "INFRA-158"
  - "sjawhar/legion#2233"
---

# Judge an automated write by what it changes, not by the state or timing it arrived in

Extends docs/solutions/envoy/a-condition-over-shared-state-is-a-data-model-change-INFRA-246.md.

- When a false signal comes from a program's own writes (an editor plugin, a normalizer, a
  migration), classify each write by what it changes and exclude that class wherever the signal is
  computed. A guard keyed on connection state, admission or ordering closes one path, and the next
  ordering opens another.
- Name the class in one table, with its source. Keep it equal to the set the server already treats
  as changing nothing a reader sees, so the client and the server agree.
- Read the change from the library's own record of it (Yjs's `transaction.changed`, which the
  `update` event passes as its fourth argument). Do not diff copies of the whole document on every
  keystroke.
- When the same thing comes back on a third path, stop fixing paths. Find what every path shares and
  fix that.

## Evidence

Round 1's false notice in sjawhar/legion#2233 ("1 edit saved in this browser can't be sent: this
document is read-only", shown to a person who typed nothing) came back on three paths. Every time,
the cause was Milkdown's heading-id stamp stored as a person's unsent edit. Five commits gated it on
state or order, and each was beaten by the next path:

- round 2: a read-only admission stores none of the editor's own updates;
- 289c6397: a later writable admission stores typing again;
- 988beb33: the restore waits for that socket's sync;
- 52159aaf: the read-only flag is taken at authentication;
- 61b165e7: the held read-only stamp is removed.

A writable tab reconnecting to an issue closed in the meantime still stored the stamp before
`Authenticated(readonly)` arrived.

The fix that held is acaf0337. `packages/dispatch/web/src/features/doc/pending-sync.ts` lists
`DERIVED_ATTRIBUTES` (a heading's `id`; an ordered list item's `label` and `listType`, the set
`TestAttributesTheEditorDerivesSettleWithoutAVersion` in `packages/envoy/internal/dispatch/docs`
settles without a version). `changesOnlyDerived` reads `transaction.changed`, and `onUpdate` holds
such an update in memory, never counted or stored. With 83649d6a's sources, the reconnect test in
`connection.test.ts` stored one row of the tab's own client.
