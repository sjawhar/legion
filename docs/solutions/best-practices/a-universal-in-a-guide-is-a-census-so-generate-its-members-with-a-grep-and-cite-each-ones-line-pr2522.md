---
title: "A universal in a guide is a census, so generate its members with a grep and cite each one's line"
category: best-practices
tags:
  - documentation
  - agents-md
  - review-rounds
  - verification
date: 2026-10-31
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2522"
---

# A universal in a guide is a census, so generate its members with a grep and cite each one's line

Extends docs/solutions/best-practices/deleting-a-ui-concept-is-a-vocabulary-sweep-and-every-sentence-it-falsifies-is-re-derived-from-the-code-INFRA-42.md.
That file's instruments are a vocabulary sweep and re-derivation by reading. Neither reaches a
sentence whose words all stay true while its quantifier goes false, and re-derivation by reading
is what fails here.

- **A sentence of the form "every X does Y", "those reads have one form" or "each tree is
  validated once" is an enumeration, not a description.** Produce its members with a grep or a
  call-site search whose output is the list, and write each member with its file and line.
- **Paste that command into the pull request body**, so the next reader re-runs it instead of
  re-reading the code. A quantifier derived by reading is wrong about the member you did not think
  of, and so is the correction you derive the same way.
- **Where the list is open or long, write the sentence with "for example" and name where the whole
  list lives.** A closed list that is nearly complete reads as a promise; an open list named as one
  costs the reader nothing.
- **Hold a guide's claim to the same evidence as a test.** A guide this repository auto-loads into
  every session is read by agents as an instruction, so a false universal there is a defect that
  reproduces itself in later work.

## Evidence

One paragraph of `packages/envoy/AGENTS.md`, the fork-reader paragraph, was corrected in three
successive review rounds of sjawhar/legion#2522, each time by reading:

- Round 1 (thread `r4423453543`): "Every read of a write transaction's fork goes the same way" was
  false — the repair read goes through `pmdoc.Read`; the version capture re-validated; the method
  list named `BlockIndex` where the edit path calls `BlockIndexes`. The changelog fragment carried
  the same claim.
- Round 2 (thread `r4432039545`), on the rewritten paragraph: "both … read through `pmdoc.Read`"
  was false for two of the three repair reads; `startTree`'s re-read was missing; "every later
  pmdoc call over the same tree takes it" was false for `pmdoc.MoveBlock` and `pmdoc.FindHeading`;
  "those reads have one form, the method" was false for `pmdoc.DeleteBlock` and
  `pmdoc.EncodeTokenJSON`.
- Round 3, the reviewer's own prose commit `b41ed1dd`: the sentence naming two calls that validate
  again now lists six — `MoveBlock`, `FindHeading`, `DeleteTableRow`, `DeleteTableColumn`,
  `TableRowMarks` and `TableColumnMarks`, each with its `docs/edits.go` line — and its round-2
  finding had named only two of them. The enumerated form, with a line per member, is the one that
  held.
