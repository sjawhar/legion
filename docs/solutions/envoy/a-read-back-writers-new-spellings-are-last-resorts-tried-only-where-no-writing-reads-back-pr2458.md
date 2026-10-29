---
title: "A read-back writer's new spellings are last resorts, tried only where no writing reads back"
category: envoy
tags:
  - pmdoc
  - read-back
  - byte-stability
  - renderer
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/pmdoc
---

# A read-back writer's new spellings are last resorts, tried only where no writing reads back

Extends docs/solutions/envoy/a-writer-that-checks-its-output-by-reading-it-back-keeps-stored-bytes-only-when-the-new-reader-joins-every-step-of-the-old-ladder-INFRA-534.md.
That page covers adding a reader; this one covers adding candidate spellings to fix shapes the
writer refuses.

- Score each writing by its depth: how many of the readers, counted from the first, it reads back
  under. Keep the earliest writing of the greatest depth, the first writing counting first. That
  one rule is the old step-by-step ladder; rewrite the ladder as it, proven by a byte-identical
  differential (`cmp` of both dumps).
- Put a new spelling, one written to fix a shape the writer refuses, in its own tier after every
  existing spelling. Consult that tier only where no writing so far reads back under the first
  reader (depth 0). A run with a depth-1 writing already stores; any new winner there changes
  stored bytes.
- Gate the change on the differential: churn, lost fixed points and newly refused must each be 0,
  and the count of refused documents must fall.

## Evidence

- The rule: `inlineWithEscapes` in `run_spellings.go` (`keepsDeeper(spellings) || firstReads == 0
  && keepsDeeper(lastResorts(needs, spellings))`, line 126), with `lastResorts` at line 296. The
  same depth replaces the alt-text switch: `altReadsBack` returns 0, 1 or 2 (`escape.go:1009`), and
  `imageAlt` takes the escaped alt only at a greater depth. The refactor commit b5a09ee4 dumped
  byte-identical to its base b295a3fb.
- The tier: the doubled backslash before a flanking character reference (`pairBackslash`) let
  compete with the existing spellings churned 1,644 documents and lost 892 fixed points on the
  committed corpora. As a last resort it churned 0. The final head of sjawhar/legion#2458 against
  main 2b427f93: every GATE 0, refused by the writer 0, fixed 181, over 435,640 documents.
