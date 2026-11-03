---
title: "A writer that checks its output by reading it back keeps stored bytes only when the new reader joins every step of the old ladder"
category: envoy
tags:
  - pmdoc
  - byte-stability
  - read-back
  - differential
  - renderer
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "INFRA-534"
  - "sjawhar/legion#2360"
---

# A writer that checks its output by reading it back keeps stored bytes only when the new reader joins every step of the old ladder

When you change the reader a writer verifies its output against (pmdoc's renderer reads each
spelling back before it keeps it), and stored output must keep its bytes:

- Keep every reader the old writer read back under, with the same definitions, as a mode of the
  new code. Do not replace them with one reader you think is equivalent, and do not reorder them.
- Add the new reader as a condition to each of the old writer's steps, in the old order: old step
  N becomes "old step N, and it reads back under the new reader". Then, wherever the old writer's
  choice still reads back under the new reader, no earlier candidate can win, so the old bytes
  stand. Only after the old ladder fails, fall back to the new reader alone.
- Apply the same ladder to every writer path that reads back, not only the main one: image alt
  text had its own check (`imageAlt`/`altReadsBack`), with a different old reader set.
- Before claiming zero churn, run the differential on a corpus aimed at the shapes where the
  preserved readers disagree with each other (here `*`/`_` runs beside other `*`/`_` runs, mixed
  with `~`, inside link and image labels, with non-ASCII letters), not only at the new feature.
  Ask a second role for a corpus of its own, and commit each shape it finds as a pinned row.

## Evidence

- In round 1 the old flanking-only reader was swapped for one "goldmark rules" reader, and alt text
  read back under that reader alone. Main had read back under two readers (micromark's `*`/`_`
  markers with goldmark's strikethrough, and flanking alone), and its alt check used the first.
  Review round 1 found `![**a*_](u)` rewritten as `![\*\*a\*\_](u)` and
  `x ~~\_\_\*\*é\_b~~**\ y …` losing its escapes: the reviewer's targeted 30,000-document
  corpus gave GATE churn 5 and engine-tree churn 10. The implementer's generated 20,000-document
  corpus (`gen/delimiter-corpus.ts`, which is built around tilde runs) had shown 0.
- The fix in 1a5c2dc2 keeps both old readers as `storedRules` and `flankingRules`
  (`emphasis.go`, `delimiterRules`). It makes `inlineWithEscapes` (`render_inline.go`) and
  `imageAlt` (`escape.go`) walk the old ladder with the editor reader added, over `readBackReaders`
  (`render_inline.go`). Churn, engine-tree churn and lost fixed points were then 0 against main on
  the reviewer's corpora and the generated ones.
- The five shapes are pinned in `escape_test.go`: four alts in `TestRenderKeepsImageAltText`, and
  one row in `TestRenderKeepsStoredMarkdownThatReadsBack`.
