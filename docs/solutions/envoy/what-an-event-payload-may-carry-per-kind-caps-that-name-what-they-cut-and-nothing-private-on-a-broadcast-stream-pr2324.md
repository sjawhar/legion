---
title: "What an event payload may carry: per-kind caps that name what they cut, and nothing private on a stream every reader receives"
category: envoy
tags:
  - dispatch-events
  - references_changed
  - privacy
  - payload-size
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch
---

# What an event payload may carry: per-kind caps that name what they cut, and nothing private on a stream every reader receives

- **Cap each kind in a "what changed" list separately, and size the cap in bytes against the transport.** One shared cap with all-or-nothing truncation lets an ordinary body push the list over and make every reader fall back to its broadest refresh. `refs.CountedChanges` caps issue, ask, document and block entries at 100 each (`refs/refs.go`, the comment on `CountedChangeLimit`: about 100 KB per event against NATS's 1 MiB).
- **A truncated list names the kinds it cut** (`references_changed_truncated_kinds`), so a reader falls back only for those kinds. Keep the old boolean flag set so older readers behave as before.
- **Treat the reader's "refresh everything" fallback as the original bug.** Any change that adds entries to a capped list re-runs the budget test with a body at that scale: one document citing more entries than the old cap (`e2e/refresh-freshness.e2e.ts`, the 25-document budget test).
- **Find out who receives a stream before adding a field to it.** Dispatch's event stream has no per-viewer filter (`api/events.go`): every signed-in person and agent replays every project event. A field is acceptable only if every recipient could already read it through a GET they may make. Thread participants pass this test; which ask one person snoozed does not.
- **A person's private change carries only their login and the names of the changed fields** (`{login, changed: ["snoozed"]}`, `askSnoozeEvent` in `api/ask_snooze.go`). Their own tabs refetch their own state.
- **Prove it** by reading another user's `GET /api/v1/events?since=0` replay and checking the identifier is absent (`e2e/refresh-freshness.e2e.ts`, the snooze privacy test).
- **A privacy or budget decision in the plan binds every later fix.** Re-read the plan's decisions before a fix round adds a payload field or a list entry.

## Evidence

On sjawhar/legion#2324, round 2's fix for a document's Referenced by panel put document and block entries into `references_changed`, under a shared cap of 20. An index document citing 25 documents then truncated the list, and every open tab refetched its issue details, asks and Inbox: the fan-out the change existed to remove. Round 3 moved to per-kind caps (`fe1e97d2`).

The same round 2 added `ask_id` and the writer's issue state to the snooze event, against plan decision 4 ("`user_state.updated` gets `changed`, not an issue key, so it does not broadcast which issue each person reads"). Round 3 removed both.

Three of the four round-3 defects were introduced by round-2 fixes.
