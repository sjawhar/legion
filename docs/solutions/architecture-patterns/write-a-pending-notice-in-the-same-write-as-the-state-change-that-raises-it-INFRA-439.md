---
title: "Write a pending notice in the same write as the state change that raises it"
category: architecture-patterns
tags:
  - persistence
  - notices
  - crash-safety
  - replay
date: 2026-10-27
status: active
module: packages/envoy-client
related_issues:
  - "INFRA-439"
  - "sjawhar/legion#2228"
---

# Write a pending notice in the same write as the state change that raises it

## Rule

When a persisted state change must also be told to a person once (a cursor jump, a reset, a
rewind), do these in order:

1. Add the notice to a pending list kept in the persisted state.
2. Write the state change and the pending list in one write.
3. Tell the notice, only if the process is still alive after that write.
4. Remove it from the pending list and write again.
5. On restore, tell every notice still pending, once, and clear it the same way.

Either other order breaks across a crash or an end of session mid-write:
- Telling first, then writing the state: the restart finds the old state, raises the notice again,
  and tells it twice.
- Writing the state first and telling after, with no pending list: a crash between them loses
  the notice for good, because the persisted state no longer shows what raised it.

The window that remains (told, then killed before step 4) tells a notice twice; it never loses one.
When a host holds a notice before showing it (for example until it is idle), it records that hold
durably too, and requeues it on restore.

Parse the pending list tolerantly: skip kinds this build does not know, so a state a later build
wrote still loads.

## Evidence

- In sjawhar/legion#2228 (`packages/envoy-client/src/replay.ts`, `pending`, `tell`, `restore`):
  - Review round 5 found a retention-gap notice told twice when a sweep was cut between the notice
    and the page-end write (fixed in fcdf28e2 by writing the cursor first).
  - That fix exposed round 6's finding: with claude-envoy's async file write, a session that ended
    while the write was in flight persisted the lifted cursor and skipped the notice, which no
    restore could find again.
  - 66cca5a7 writes the untold notice in the same state write as the cursor change.
- The engine tests cut the session during an async `write` and restore a new engine from what was
  written. They tell exactly one notice across both engines, and they fail with the pending add
  moved after the write.
- pi-envoy records a held notice as an `envoy-replay-notice` transcript entry (489fd64d), and the
  tester's SIGKILL-while-held leg then showed it once after the resume.
