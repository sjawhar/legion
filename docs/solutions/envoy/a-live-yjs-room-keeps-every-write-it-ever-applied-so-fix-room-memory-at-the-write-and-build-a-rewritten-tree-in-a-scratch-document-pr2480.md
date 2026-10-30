---
title: "A live Yjs room keeps every write it ever applied, so fix room memory at the write, and build a rewritten tree in a scratch document"
category: envoy
tags:
  - yjs
  - crdt
  - memory
  - dispatch-rooms
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2480"
---

# A live Yjs room keeps every write it ever applied, so fix room memory at the write, and build a rewritten tree in a scratch document

- A live Dispatch room holds its stored history decoded, deleted items included: ygo keeps each
  deleted item as a tombstone. Retiring the room, reloading it or compacting its rows frees
  nothing while the room is in use. When a room's memory grows, measure what each kind of write
  adds to the history, and fix the write that adds the most.
- A write that leaves content unchanged must leave its Yjs items unchanged. Every attribute that
  differs from the live item costs a tombstone in the room and again in its rendered replica. So
  carry over identity the input cannot express, such as an untyped block's `blockId`, before the
  tree reaches `pmdoc.Update`.
- When a write has to rewrite the tree and then fix it up, for example the text first and then its
  anchors put back, do both in a private scratch document (`newDocumentCopy`). Read the result
  back, then write it into the room with one `pmdoc.Update`. Each strip-and-re-add made in the
  live document is a tombstone the room keeps for the document's life, even on text the write
  never changed.
- Never move the carry or the fix-up into `pmdoc.Update` itself: settlement repairs, `ApplyOps`,
  ask writes and the seed call it too. Put it in the writer that knows its input lacks the
  identity (`pmdoc.ParseForWrite` for a whole-document upload).

## Evidence

- A room's document heap equalled a fresh decode of its own `EncodeStateAsUpdateV1` (ratio
  0.9–1.0). Compacting 464 rows to 1 after 400 load-mix writes changed the resident room's heap by
  under 1 MB. This was measured with a throwaway test in round 1 of sjawhar/legion#2480.
- On a 25 KB spec, a one-token upload changed 1,658 of 1,658 block ids and kept about 490 KB of
  room and replica heap on main. After `carryBlockIDs` (`pmdoc/carry_block_ids.go`) and the
  scratch-document write (`docs/mutation.go`, `ReplaceText`), it kept 676 B; over 100 open
  anchors, 550 KB fell to about 3 KB.
- On the cluster copy, an hour at ten times grew Dispatch's working set by 0.84 GiB at the head
  (run `lt-20261029t085434-3a20`), against 9.98 GiB on main on 2026-10-28.
