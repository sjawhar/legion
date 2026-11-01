---
title: "A long line a worker read through a truncating window comes back truncated: check its byte count against the fork point before committing"
category: best-practices
tags:
  - documentation-edits
  - verification
  - delegation
  - truncation
  - e2e-readme
date: 2026-10-16
status: active
module: scripts/e2e/README.md
related_issues:
  - "LEGION-636"
  - "sjawhar/legion#1984"
---

# A long line a worker read through a truncating window comes back truncated: check its byte count against the fork point before committing

Extends docs/solutions/best-practices/a-multi-kilobyte-single-line-agents-md-bullet-is-edited-whole-and-checked-with-a-word-diff-against-the-committed-line-LEGION-752.md.

- The read tools an agent edits through show a long line cut at a few hundred bytes with a
  trailing `…`. A worker that edits such a line from what it saw writes the cut back whole: the
  line shrinks to the window, ends in a literal `…`, and a table row loses its closing `|`. The
  edit tool echoes the new line, which looks complete.
- Before editing a file, list its long lines so you know which edits are at risk:
  `awk 'length > 700 {print FILENAME ":" FNR " " length}' <file>`. The checkpoint tables of
  `scripts/e2e/README.md` and several `docs/kubernetes.md` paragraphs qualify.
- After the edit, compare the line's byte count with the fork point's and read the word diff;
  the count may shrink by the deletion you meant and no more:

  ```bash
  jj -R "$LEGION_WORKSPACE" file show -r <fork point> <file> | awk 'NR==<n>' | wc -c
  awk 'NR==<n>' <file> | wc -c
  diff <(jj -R "$LEGION_WORKSPACE" file show -r <fork point> <file> | awk 'NR==<n>' | tr ' ' '\n') \
       <(awk 'NR==<n>' <file> | tr ' ' '\n')
  ```

- Before every push, scan the whole diff for new lines ending in the window's marker:
  `jj -R "$LEGION_WORKSPACE" diff --from <fork point> --git | grep -n '^+.*…$'` must print nothing
  (a line that legitimately ends in `…` is rare enough to read by hand).
- An orchestrator reviewing a worker's diff sees the same truncated window on the `-`/`+` pair,
  so the pair looks like a one-word change. Trust the byte counts, not the display. A worker that
  reports "that file needed no change" while the diff shows the file changed is a second sign:
  read the diff, not the report.

## Evidence (LEGION-636, sjawhar/legion#1984)

Task I dropped the three words `and none \`installed\`` from the Stage 4b `boot` row of
`scripts/e2e/README.md:1031`, a 2229-byte table row. The worker read the row through a 768-byte
window and wrote that window back: 772 bytes, ending in `…`, no closing `|`, and the worker's
report said the README had needed no change. The orchestrator's `jj diff --git` showed a `-`/`+`
pair whose display was itself cut at 768 bytes, so the loss was invisible there too. The reviewer
found it (thread PRRT_kwDORFy7ds6tDz39: "the shape a 768-character tool truncation leaves") and
the round-1 fix restored the fork point's row with the one deletion, checked by `wc -c`
(2229 → 2208) and the word diff above (one run removed, nothing added). The lost text was the
documented contract of `capabilities-at-boot.json` that Stage 4b asserts.
