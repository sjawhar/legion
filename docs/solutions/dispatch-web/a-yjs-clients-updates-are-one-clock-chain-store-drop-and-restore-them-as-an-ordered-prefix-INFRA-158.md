---
title: "A Yjs client's updates are one clock chain: store, drop and restore them as an ordered prefix"
category: dispatch-web
tags:
  - yjs
  - pending-edits
  - indexeddb
  - offline
date: 2026-10-27
status: active
module: packages/dispatch
related_issues:
  - "INFRA-158"
  - "sjawhar/legion#2233"
---

# A Yjs client's updates are one clock chain: store, drop and restore them as an ordered prefix

- Each local update of a Yjs client continues that client's clock. If an update is stored without an
  earlier update of the same client, it parks when applied after a reload, because the structs it
  continues are missing. The typing in it is then lost.
- Never drop one stored row out of the middle of a client's rows. You may drop a client's rows only as
  a prefix the server is known to hold, or all of them.
- If you keep some local updates out of the store (uncounted, held in memory), write them to the
  store first, in order, before you store a later update of the same client. Drop a held update only
  once the server has acknowledged it.
- Judge whether stored rows "change nothing" per client, with the rows applied in order, never one row
  at a time.

## Evidence

- In sjawhar/legion#2233, dropping only the heading-id stamp's row parked the typing stored after it,
  which `packages/dispatch/e2e/offline-edits.e2e.ts` caught. Round 5's browser case on a spec with a
  heading was refused "could not be applied: the document was rebuilt since" until the stamp was
  stored ahead of the typing.
- `packages/dispatch/web/src/features/doc/pending-sync.ts` (acaf0337): the `derived` list's comment
  says a stored row typed after a held update parks on a reload, so `onUpdate` records the held
  updates first, in order, before a person's edit. `clearAcknowledged` drops a held update on its
  Update frame's acknowledgement or on a SyncStep2 that covers its ordinal. `changesNothing` judges
  one browser client's stored rows applied in order.
- `connection.test.ts` restores typing stored after an offline stamp with no rebuild notice. The
  tester's probe 8 held stamps that were never acknowledged, then typed offline and reloaded: every
  heading and list item appeared exactly once.
