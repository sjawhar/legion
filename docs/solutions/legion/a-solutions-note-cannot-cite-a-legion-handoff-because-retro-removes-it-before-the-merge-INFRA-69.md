---
title: "A solutions note cannot cite a Legion handoff, because retro removes .legion/<issue>/ before the merge: put anything a reader must check in the pull request body or a Dispatch record"
category: legion
tags:
  - legion-retro
  - legion-worker
  - docs-solutions
  - handoff
  - evidence
date: 2026-10-21
status: superseded by sjawhar/legion#2354 — a phase's handoff is a record on its Dispatch issue, and no phase commits or pushes one; see docs/solutions/legion/handoffs-are-dispatch-records-and-the-notes-that-teach-handoff-commits-are-superseded-pr2354.md
module: docs/solutions
related_issues:
  - "INFRA-69"
  - "sjawhar/legion#2088"
---

# A solutions note cannot cite a Legion handoff

Retro's last commit removes `.legion/<issue>/` from the head that merges. A figure whose only home
is `implement.json`, `test.json` or `review.json` becomes unopenable the moment the change lands,
so a durable document may never point at one.

- **Before you write a figure into a `docs/solutions/` note, ask where a reader opens it after the
  merge.** The pull request body survives as `main`'s commit message under this repository's squash
  settings; a Dispatch comment survives on its issue; a repository file survives as itself. A
  handoff does not.
- **A measurement another role took goes in the pull request body first, then into the note by
  reference to the pull request.** Do not cite the role's handoff and do not reprint the figure as
  if it were yours.
- **The same applies to the note's own provenance.** A reader who cannot open what a sentence rests
  on has a sentence they must take on trust, which is what a learning note exists to avoid.

## Evidence

INFRA-69's note needed a memory reading the reviewer had taken in its own container. Its only home
was `.legion/INFRA-69/review.json`, which retro deletes before the merge. The sentence went in as
*"a second role container of the same pod read …"* with both readings pointed at
`sjawhar/legion#2088`, and the pull request body carries the figures — so the claim is openable on
the merged commit message rather than resting on a file that no longer exists.

The reverse case is in the same note and is its one remaining weakness: every deployment fact it
carries — the eviction events, the Go-cache sizes, the evening's two ephemeral-storage changes —
lives in `dispatch://INFRA-51` comments that exist in no file and that only a session with the
`dispatch` CLI can open. That is reachable for a Legion worker and unreachable for anyone else. A
note whose most perishable claims rest on its least reachable records is sound for its intended
reader and opaque to any other; decide which you are writing for, and say so.
