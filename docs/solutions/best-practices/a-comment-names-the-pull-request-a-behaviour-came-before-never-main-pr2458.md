---
title: "A comment names the pull request a behaviour came before, never \"main\""
category: best-practices
tags:
  - comments
  - wording
  - history
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/pmdoc
---

# A comment names the pull request a behaviour came before, never "main"

Extends docs/solutions/best-practices/a-number-or-a-guarantee-restated-in-prose-is-a-copy-that-drifts-cite-or-generate-it-INFRA-236.md.

- Do not write "main", "main's bytes" or "the old reader" for an earlier writer in a comment, a
  test name or a test message. Main moves, so the word names a different revision at every reading.
- Name the writer by the pull request it came before: "the writer before sjawhar/legion#2360". Find
  it by annotating the line (`jj file annotate`) and reading the pull request in that commit's
  subject. Where rows come from several writers, say "an earlier writer".
- Grep for the bare word at each such cleanup and give every hit a disposition. `func main()` and
  the HTML tag `main` stay. Hits a merge brings into files your change does not touch go to the
  tree that owns them.

## Evidence

- sjawhar/legion#2458 renamed about twenty such sites in pmdoc (commits 7511136b, f3cda761 and
  f4d26d77). They resolved to eight different writers: #1383, #1425, #1429, #1464, #1563, #1892,
  #2360 and #2403. One citation first named #1594, a pull request that only reflowed the line; the
  reviewer corrected it to #1429, the pull request that wrote it.
