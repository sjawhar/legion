---
title: "Measure a CPU fix with full pprof focus totals, a per-line split, and a restore past boot backfills"
category: best-practices
tags:
  - pprof
  - cpu-profile
  - load-test
  - dispatch
date: 2026-10-28
status: active
module: packages/envoy
---

# Measure a CPU fix with full pprof focus totals, a per-line split, and a restore past boot backfills

- Read a focused total from `go tool pprof -top` only with `-nodecount=1000000` (and
  `-nodefraction=0 -edgefraction=0`). Its `Showing nodes accounting for X` line sums the flat
  time of the nodes it shows. With `-nodecount=1` that is one node, not the focus.
- To prove a call no longer runs, split the function's cost by call site with
  `go tool pprof -lines -peek '<func regexp>' <binary> <profiles…>`. A call site with no samples
  at the head and its cost at the base is the proof. Add no counter for it.
- Before measuring a server restored from a database dump, start it once on the base build until
  its boot-time backlogs finish, and dump that state. Every measured run on both sides restores
  the warmed dump. For Dispatch, the backlog is the block-index backfill: wait for
  `owed_documents=0` in its `dispatch: block index` log line, then for the CPU to fall idle. A
  backlog left in the measured window gives each run a different share of it, which swamps a
  small fix.
- Alternate base and head runs (base-1, head-1, base-2, …), so drift on the box falls on both
  sides.

## Evidence

- With `-nodecount=1`, the settlement's render row read 0.11-0.14 % of each base profile. With
  `-nodecount=1000000` it read 3.29-3.33 % (PR #2416 evidence comment, W14 per run).
- `-lines -peek` over three profiles each: base 27.79 s at `service.go:1428` plus 27.09 s at the
  version render. Head 27.35 s at `:1429` and no samples at the version render (PR #2416 body).
- Restored from the unwarmed seed dump, the first two runs measured the backfill of 1,137
  documents: base 525.15, head 626.24 CPU-seconds (PR #2416 evidence comment, "Departures"). From
  the warmed dump the six runs read base 567.90-576.00 and head 540.85-573.47 (the same
  comment's table).
