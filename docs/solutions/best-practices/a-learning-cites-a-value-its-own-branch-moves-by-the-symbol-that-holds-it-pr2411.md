---
title: "A learning cites a value its own branch moves by the symbol that holds it"
category: best-practices
tags:
  - retro
  - citations
  - forward-merge
date: 2026-11-01
status: active
module: docs/solutions
related_issues:
  - "sjawhar/legion#2411"
---

# A learning cites a value its own branch moves by the symbol that holds it

Extends docs/solutions/best-practices/a-symbol-citation-rots-when-the-behaviour-moves-under-the-name-pr2580.md.
That page calls a constant's declaration the one literal nothing can move; this is the exception.

- A value your branch keeps in step with main moves at the next forward merge, before the pull request merges. Examples: a closing migration number, a pinned digest, an API version, a snapshot of an allow-list.
  Cite it in a learning by the symbol that holds it (`closings`, `ClosedAt`), never by quoting the value.
- When the value must appear, search for each quoted value with `grep -F` at the merged head, as part of the citation sweep in docs/solutions/legion/a-forward-merge-after-retro-falsifies-the-first-retros-own-citations-pr2542.md.
  That sweep matches only `path:line` citations, so it never finds a quoted value.
- A value written as history ("the series then ended at …") stays as written: it describes a past state, not the file.

## Evidence

The first retro of sjawhar/legion#2411 wrote "`closings`: daemon `0064_…`, Dispatch `0149_…`" in its migration-identifier learning.
Three forward merges then raised the closings to daemon `0067` and Dispatch `0157` in `packages/envoy/migrationid/migrationid.go`, and the learning stated a file that no longer existed.
The second retro's learning cites `migrationid.ClosedAt` and was still true after all three merges.
