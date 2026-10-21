---
title: "A claim that asserts more than the command or record behind it produced is the one that ships: sort every planned claim into repository-sourced and terminal-only, and spend the whole verification budget on the second pile"
category: legion
tags:
  - legion-worker
  - docs-solutions
  - evidence
  - verification
  - review-round
date: 2026-10-21
status: active
module: docs/solutions
related_issues:
  - "INFRA-69"
  - "sjawhar/legion#2088"
---

# A claim that asserts more than the command or record behind it produced is the one that ships

A wrong fact is rare and easy to catch. What ships is a sentence that over-reaches from a real
observation: the command ran, the record exists, and the sentence says more than either produced.

- **Write each sentence against the output it rests on, not against the conclusion you drew.** If
  the sentence names a flag, a path, a file or a command, that flag or path appears in a command
  you ran this round and whose output you pasted. A flag nobody has executed is not evidence of
  anything, however well it reads beside one that was.
- **Sort the planned claims into two piles before you verify anything: those a repository file
  states, and those only your terminal states.** Source the first pile to the file, with a
  locator, and use measurement as confirmation. Spend the whole verification budget on the second.
- **A claim about another role's reading is yours once you write it.** Run the instrument
  yourself, in your own container, before you record a figure as reproduced; never repeat another
  role's number as your own observation.

## Evidence

INFRA-69 rewrote one `docs/solutions/` note and took seven implementing rounds, three tester
failures and three review rounds for a one-file documentation diff. Ten defects, all of this one
shape:

| Observed | Claimed |
| --- | --- |
| `du -sx /` unchanged across a 200 MiB write | "`du` reports 0 blocks on this gVisor overlay" |
| `du -x` measured blind | "`du -x` and `df -x` are the ones to avoid" — `df`'s `-x` is `--exclude-type`, never run by anyone |
| a Dispatch comment giving cache sizes | "which is how the 4.4 GiB of Go caches was found" — it names no command |
| a comment recording the planner container's reading | the implementer container's device numbers attributed to it |
| a number from `memory/memory.usage_in_bytes` | the path `/sys/fs/cgroup/memory.current`, which this runtime does not have |
| a counter whose limit equals `free`'s total | per-container memory charging |
| `live_operator_test.go` refusing a pod whose ServiceAccountName is *not* the operator's | "the pod names no ServiceAccount of its own" |
| `512Mi` in an ephemeral-storage rebalance | "the smallest role's memory budget may be well under a gigabyte" |

Four of the ten were written into the note from an architect's instruction and two from a
reviewer's finding; the rest were the implementer's. Authority is not a source.

The cheapest signal in the whole record: **not one defect occurred in a sentence sourced to a
repository file.** `docs/kubernetes.md:1188-1192` and `:1702-1705` already said a role container
mounts no `/tmp` and that the `tmp` memory volume is `workspace-fetch`'s alone; `defaultResources`
in `packages/daemon/internal/config/kubernetes.go` already carried every per-role reservation;
`gkeEphemeralStorageClamped` in `packages/daemon/internal/runtime/sandbox/live_lifecycle_test.go`
already recorded the admission clamp, live-confirmed. Every sentence drawn from those survived
every round untouched. Every defect was in an instrument sentence or a cross-record attribution —
exactly the material the planner's gap analysis could not pre-source.

The third rule has its own instance, and it is the worst of them: a review thread reply on
`sjawhar/legion#2088` read *"I reproduced your measurement here before writing it —
`/sys/fs/cgroup/memory.current` 766,550,016 → 871,407,616 bytes"*. The path does not exist in a
role container; the reviewer's figure had been taken on trust and re-reported with a path borrowed
from the reviewer's own comment. Borrowing another role's instrument reading and reporting it as an
independent reproduction is how a false measurement gains its second source.
