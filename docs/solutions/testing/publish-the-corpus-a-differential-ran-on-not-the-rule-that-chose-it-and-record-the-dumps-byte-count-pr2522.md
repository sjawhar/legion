---
title: "Publish the corpus a differential ran on, not the rule that chose it, and record the dump's byte count"
category: testing
tags:
  - differential
  - corpus
  - evidence
  - reproducibility
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2522"
---

# Publish the corpus a differential ran on, not the rule that chose it, and record the dump's byte count

Extends docs/solutions/testing/a-committed-corpus-differential-compiles-at-base-dumps-deterministically-and-each-surface-gets-a-negative-control-its-input-can-trigger-pr2451.md,
which says to fetch a large corpus read-only under `$TMPDIR` and commit none of it. It says
nothing about the identity of the set that was chosen, which is what a pod's death takes.

- **Publish the selected list where the issue keeps its evidence** — one line per document with
  its size — as soon as the first run succeeds. A corpus that lives only on a pod's disk is gone
  with the pod, and a sentence describing how it was chosen does not bring it back.
- **Record the dump's byte count and SHA-256 in the pull request body.** Those two numbers are
  what lets a later rebuild be shown to be the same corpus rather than a similar one.
- **Where the selector must decide what the system accepts, call the system's own reader.** An
  imitation of a parser disagrees with it on the inputs that matter, and the disagreement looks
  like a selection rule that works.
- **A rule is reproducible only as the program you ran.** Ship the selector with the list.

## Evidence

- The 23-spec write-corpus differential of sjawhar/legion#2522 was first run in October against a
  list that lived only on the pod that ran it. The pod was drained; only the rule survived, "the
  largest 23 of the replay bundle's documents".
- That rule does not seed. Two rebuild attempts died after about nine minutes each on one
  document, refused with `a table row holding 7 cells where its table has 5 … a cell ends at every
  | not written \|, in code and links too, and at a | after an even run of backslashes`.
- A hand-written markdown rule for the same condition let that document through: counting cells by
  splitting on unescaped pipes says five where the parser says seven.
- The selector that worked calls the reader — `pmdoc.Parse` then `Node.Validate` over the bundle,
  keep what it accepts, take the 23 largest; 869 of the bundle's documents pass. The list, its
  sizes and the selector were published as a Dispatch artifact and taken as the corpus of record
  for the issue.
- The rebuild is provably the same corpus because the dump is 11,374,504 bytes, the byte count the
  October run recorded, with SHA-256 `c25acc05a6b134c382fe9260de5f37020d93aaef6c7f18f959b24c9aa23d726e`
  read on two pods, two builds and two different base pairs.
- When base and head carry different copies of the differential's own test file, run both sides
  with the head's copy and say so: the base of this pull request held an older
  `write_corpus_test.go` without the seven extra edit kinds, and a dump whose format differs
  compares as a difference in behaviour.
