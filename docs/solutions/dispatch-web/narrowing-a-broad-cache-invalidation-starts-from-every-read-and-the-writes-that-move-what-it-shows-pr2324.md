---
title: "Narrowing a broad cache invalidation starts from every read and the writes that move what it shows, diffed against what the broad refresh covered"
category: dispatch-web
tags:
  - tanstack-query
  - invalidation
  - event-refresh
  - freshness
date: 2026-10-28
status: active
module: packages/dispatch/web/src/api
---

# Narrowing a broad cache invalidation starts from every read and the writes that move what it shows, diffed against what the broad refresh covered

Extends docs/solutions/daemon/a-rule-on-a-state-that-stands-for-the-head-is-applied-by-every-fact-that-can-make-it-stand-census-the-predicates-writers-LEGION-698.md.

That document takes a census of the writers of one predicate. Here the census is taken across every cached read a page holds, and the base's broad refresh supplies the reference to diff against.

- **Census from the read, not from the event.** Before the first handoff, build a table. Its rows are every query key the app holds, each field that key's view renders, and every write that can move that field, including writes to *other* records that the view shows: a child's write moves the parent's rollup and active time, a blocker edge moves the blocker's count, and a retitle moves every list row that shows the title. Writing rules event by event (`issueWriteTargets`, `appendDocumentPanel` in `web/src/api/event-refresh.ts`) misses the views no event names.
- **Diff against the broad refresh you are removing.** For each event type, list the reads the old code refreshed and the new code does not. For each pair, either justify it ("this view shows nothing that event moves") or add a rule. The old breadth was right by accident in places nobody wrote down.
- **One freshness test per pair:** another party writes, and the open page shows it within a stated time. Run it on the base first. It must pass there; if it fails there, it is not a regression this change caused.
- **A narrowed read needs its own error recovery.** `staleTime: Infinity`, no focus refetch and no whole-cache refresh on a complete reconnect also remove the incidental refetches that used to recover a failed read. Add an error-only path: `refetchOnWindowFocus` returning `"always"` only while the query is in error (`refetchFailedOnFocus`, `features/refs/reference-target.ts`).
- **After every merge of main, look for new event types and new reads.** A new event type with no rule falls into the catch-all broad refresh.
- **Write that a new read needs a refresh rule** where the next author of a read will look: the package's AGENTS.md section on the event rules.

## Evidence

This pull request replaced "every event refetches every issue the page links" with per-event rules. Freshness gaps that the old broad refresh had covered surfaced one round at a time:
- Tester round 1 found seven: a document's Referenced by panel, a reference read in flight while you leave the page, an Inbox close during its fetch, the Architecture work list, a snooze from another tab, the Replies page title, and a child's active time in the parent's Children tab (red tests in `e2e/refresh-freshness.e2e.ts`; fixed by `d649b640`).
- Tester round 2 found a comment edit in a document's panel (fixed by `fe1e97d2`).
- The reviewer found the blocker's `Referenced by (N)` count (fixed by `c04118f6`, `issueEdgeChanges` in `api/issue_event_fields.go`).
- A merge of main brought in `artifact.human_edited`, which needed its own rule (`event-refresh.ts`, the `artifact.human_edited` branch).
- The plan had specified "no focus refetch" for the summary reads. The reviewer's thread showed a failed summary never recovered (fixed by `c04118f6`).

Each item was a field some view renders, moved by a write the plan's event-by-event rules did not list.
