---
title: "A decider in a no-I/O transaction reads a seq-fenced mirror the first handler writes"
category: daemon
tags:
  - intake
  - handler-order
  - mirror-table
  - admission
  - dispatch
date: 2026-10-26
status: active
module: packages/daemon
related_issues:
  - "INFRA-225"
  - "sjawhar/legion#2188"
---

# A decider in a no-I/O transaction reads a seq-fenced mirror the first handler writes

Extends docs/solutions/daemon/a-sweep-for-a-condition-a-reducer-handles-live-shares-its-predicate-and-executor-and-surfaces-bare-fixtures.md.

When admission or the workflow must judge outside state (Dispatch's blocked-by links, an issue's
Dispatch status) inside `intake.ApplyFact`, where no handler does I/O:

- Keep a mirror table of that state. Write it from a handler of its own placed **first** in the
  chain (`[mirror, engine, admission]`), so the fact's own row is visible to every decider in the
  same transaction. A decider never writes the mirror itself: the engine returns early on exactly
  the fact such a feature exists for (`fact.Seq <= issue.LastDispatchSeq || fact.Status ==
  issue.Status` is a blocked-by-only `issue.updated`). Pin the order by behaviour: a probe in the
  last decider's place already sees the row.
- Fence every mirror write in SQL on the source's sequence
  (`on conflict (key) do update … where t.seq < excluded.seq`), and drop zero-sequence facts the
  daemon mints from its record: on a new key nothing conflicts, so a zero-seq row would land and
  then refuse every real observation. Send the boot listing and the periodic re-read through the
  same fenced write, and the boot needs no ordering against the consumer.
- Read the mirror whole, once per look, and judge in memory; pass the rows on to whatever else
  that look starts. Pin it with a counting store (one read for N candidates).
- Every exit of a decider whose input can change what something waits on still runs the look,
  including the exits for issues it keeps no record of. A blocker nobody handed to Legion closes
  through those exits.
- Decide what a missing row means for each side: a blocker with no row is unfinished (its
  creation event may not be consumed yet), an issue with no row waits on nothing.
- Never filter a list other readers depend on to express a skip (`record.Waiting` feeds
  `TreeLive`; dropping a blocked root orphans its children). Skip in the decider and in the slot
  count only.
- Never use an empty result as a predicate's converse. `Holds` is nil both for "nothing holds it"
  and for "cannot be held", so the start gate asks `Holdable` and an empty `Unfinished`.
- An API that answers about records a later event creates (its own write's echo) answers from
  what the source showed the request, never from the local record.

## Evidence

- `daemon/workflow.go` puts `blockers.NewMirror` first; `intake/handler_order_test.go` pins it.
  `record/dispatch_issues.go` holds the fence and the zero-seq drop.
- The driven proof found admission's unrecorded-issue exits returning before promoting: a root
  whose blocker nobody had handed to Legion waited about 58 s for the controller tick. Every exit
  now promotes; the same step admits it in under 110 ms with both tick intervals at an hour.
- `release_children` first answered `held` from the children's records. A child released out of
  backlog then had none until its `todo` event landed after the answer, so it read `held: []`. It
  judges `blockers.UnfinishedOf` over the blocked-by list Dispatch showed the request. The release
  now records such a child from its own write in the request (`api.applyOwnStatusWrite`), and the
  answer still judges the wait from Dispatch's list, which also covers a written child the
  workflow could not record yet (its parent unrecorded).
- A root already in its slot was first judged like a child by the gate's opening and froze at
  `admitted` holding its slot, named nowhere: `Holdable` excludes tree roots for that reason.
