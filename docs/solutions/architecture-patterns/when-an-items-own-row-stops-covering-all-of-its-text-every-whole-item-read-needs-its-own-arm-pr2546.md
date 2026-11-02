---
title: "When an item's own row stops covering all of its text, every whole-item read needs its own arm, and a path that newly writes edges must stamp them"
category: architecture-patterns
tags:
  - reference-graph
  - derived-index
  - change-feed
  - census
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch/refs
related_issues:
  - "sjawhar/legion#2546"
---

# When an item's own row stops covering all of its text, every whole-item read needs its own arm, and a path that newly writes edges must stamp them

An item whose own row used to hold every link it has, and now holds only some of them, breaks a
class of reads rather than one read.

- **Enumerate every read that joined the item's own row, and give each one its own arm.** Fixing
  the read you noticed does not fix the class: the readers were written independently, one per
  direction and one per shape of answer, and each believes the row is the whole item. Find them
  by the table name, not by the function you are editing.
- **Write the arms once.** Two arms that differ only in which end they select are one fragment
  plus two wrappers; a copied arm is where the next change is applied in one place and missed in
  the other.
- **A path that newly produces edges must index inside the row's own transaction and stamp them
  with the event it appends.** A path that never wrote references before has no stamping call to
  copy, so it is easy to miss — and the edges it writes carry a null source sequence, which means
  an incremental reader that asks for everything since a sequence never sees them. Census the
  writers of the newly indexed columns, not the writers that already touch the reference tables.

## Evidence

sjawhar/legion#2546 made an ask's question, each option and its answer stored block nodes, while
an ask's own `refs` row keeps the links of its question alone.

- Two whole-item reads assumed that row was the whole item. `refs.Closure`
  (`packages/envoy/internal/dispatch/refs/closure.go:34`) was the one the implementer found: it
  gained an arm over `ask_block_refs` in `cfb9c8b5dd81`. The reviewer found the second,
  `refs.ReferencedBy`'s `WholeDocuments` read (`refs/refs.go:496`, `refs/graph.go:59-62`), where a
  document cited only from an option was listed by nobody; `dd2f17197c3f` added
  `asksCitingTheDocument` to `refs/graph.go`.
- The two arms were then copies of one query. The round-2 fix `1b769383daf4` extracted the shared
  `from` and `where` as `askPartLinks` (`refs/ask_blocks.go:289-313`), which `closure.go:46` now
  uses too.
- `refs.Stamp` (`refs/refs.go:218`) was missing on three paths that had never written references
  before this change: `closeAskTx` (`api/ask_transitions.go:346`), `answerApprovalAsk`
  (`docs/approval_carry.go:418`) and `restoreIssueAsksTx` (`api/issue_ask_lifecycle.go:139`).
  Without it the edges an answer produces carry a null `source_seq`, so a `?since=` reader never
  sees them. `2d0a906233d3` added the call to all three (`:378`, `:453`, `:189`).
