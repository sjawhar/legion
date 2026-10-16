---
title: "A lease holder's heartbeat ends only on another token: an unreadable read is retried inside the stale budget, and the release shares no predicate with it"
category: daemon
tags:
  - lease
  - heartbeat
  - cross-process
  - v9fs
  - stale-takeover
  - fail-open
  - fail-closed
  - simplify-pass
  - codegraph
  - worker-shim
date: 2026-10-16
status: active
module: packages/daemon/internal/workspace/codegraph.go
applies_when:
  - A lease or lock holder refreshes a file's mtime on a tick and another process judges the holder dead by that mtime's age
  - A holder may be taken over while it still runs, and must then stop touching the file
  - A read of the lease can fail transiently (a remove-and-create in flight, a network filesystem's EIO)
  - A reuse or simplify pass proposes one shared predicate for two callers that read the same file
related_issues:
  - "LEGION-638"
  - "sjawhar/legion#1981"
---

# A lease holder's heartbeat ends only on another token; an unreadable read is retried; the release shares no predicate with it

Extends `docs/solutions/daemon/a-file-presence-lease-on-a-shared-volume-exclusive-create-is-the-only-atomic-step-judge-a-lease-by-mtime-and-token-never-inode-LEGION-629.md`.
That note gives the token rule for the **release**: remove the file only while it still carries
this holder's token. The heartbeat needs the same token and the opposite failure direction.

- **Three-state the tick.** Own token → set the mtime to now, go on. Another token → a successor
  took the lease over while this holder stalled; stop ticking for good and leave the file alone
  (the build runs on; ending it is a separate decision). Unreadable → refresh nothing and read
  again at the next tick. Only a positive foreign identity ends the heartbeat.
- **A failed read is not evidence the lease is gone.** A takeover is a remove and a create, two
  steps; a tick between them finds no file. The tree volume (9p) can fail a read outright while
  the lease is still the holder's. The stale budget (six ticks, `warmLeaseStale`) exists to absorb
  missed ticks; a heartbeat that exits on the first unreadable tick spends that budget in one step,
  the running build's lease ages past it, a sibling takes it over, and two builds run on one
  workspace.
- **The release is fail-closed, the refresh fail-open. Give them no shared predicate.** Both ask
  "does the file carry my token?", so a reuse pass will offer one helper for both — and the helper's
  `err == nil && token matches` is right for the release (unreadable → do not remove) and wrong for
  the refresh (unreadable → do not stop). Before sharing a predicate between two callers, write down
  what each does when it returns false; if the answers point in opposite directions, keep two
  functions and say in each comment which direction it fails.
- **A write failure is not proof of health either.** `os.Chtimes` failing six ticks running ages a
  live lease with no line logged; the first failure deserves one log line (open, Fast-follow of
  sjawhar/legion#1981).
- **Test both halves with the inode pinned.** The taken-over case (successor's token written into a
  hard-linked re-creation of the file, 30 s old; ten ticks; same inode, same mtime, same token; the
  release leaves it) and the unreadable case (the file renamed away for three ticks, renamed back
  30 s old; the next tick moves the mtime; the release removes it). Each fails against the other
  contract through `go test -overlay` of the pre-fix file.

## Evidence

- LEGION-638 round 1 (sjawhar/legion#1981): the token guard landed as `if !warmLeaseHasToken {
  return }` in the ticker loop, after the simplify pass shared that predicate with the release. The
  reviewer's probe — `holdWarmLease(lease, "A", 50ms)`, the lease renamed away for two ticks and
  back with an old mtime — found the heartbeat dead for good. On the real shim, building a four-copy
  workspace on the tree volume: the lease moved away for 25 s (two 10 s ticks) and back 30 s old
  was refreshed by the next tick with the fix, and never again with the pre-fix binary.
- The taken-over half held in both rounds: a holder frozen 2 s into its build, taken over 61 s
  later by a sibling frozen the instant its token was in the lease, then resumed, left the
  successor's lease untouched over two ticks (same token, same mtime, inode 83657 reused on 9p);
  the unguarded binary moved its mtime at the first tick.
