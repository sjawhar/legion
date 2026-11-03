---
title: "A compare-and-set writer keeps every save that may have landed, and adopts a moved row only on matching bytes"
category: daemon
tags:
  - compare-and-set
  - at-most-once
  - postgres
  - session-table
date: 2026-10-29
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2186"
---

# A compare-and-set writer keeps every save that may have landed, and adopts a moved row only on matching bytes

- A save that returns an error may still have committed (a timeout during `COMMIT`, a connection
  lost after the server applied it). Record every such save as pending, with the length it would
  leave, the offset it wrote from and a hash of the bytes it sent, until a save is acknowledged.
  Never replace the record of one unacknowledged save with the next: the window of doubt is
  everything since the last acknowledged write, not the last write.
- When a later compare-and-set is refused because the row moved, read the row and take it as
  your own only if its length equals what a pending save would leave **and** its bytes from that
  save's offset hash to what that save sent. A length alone cannot tell two of your saves of the
  same length apart, nor your save from another writer's row of that length. Anything else is
  another writer: stop, as before.
- After adopting, save again at once. The last save of a run (a final flush at exit) has no later
  tick to retry it.
- Test it with an injected writer fault, not by reasoning: one save that lands and returns an
  error, then one that fails outright, then assert the next save commits, for an appended file, a
  fresh file, a same-length rewrite, and another writer's row.

## Evidence

`packages/daemon/internal/launcher/claude.go` (`pendingSave`, `saveRow`, `adopt`) keeps a Claude
Code transcript and the phase-reminder record as rows of Oh My Pi's session table. Its first
version kept one `unacked` slot: a landed-with-error save followed by a failed save erased the
record of the first, the next save was refused with the row at the first save's length, and the
keeper stopped saving for the rest of the generation. On a fresh transcript the `.legion-saved`
marker was never written, so the worker never registered. The test
`TestAClaudeSaveTakesEverySaveThatMayHaveLandedAsSavedWhenTheRowMovedToIt` failed in all four
cases before the fix (commit 51415dcb) and passes after.
