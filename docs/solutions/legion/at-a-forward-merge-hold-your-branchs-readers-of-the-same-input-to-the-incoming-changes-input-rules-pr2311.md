---
title: "At a forward merge, hold your branch's readers of the same input to the incoming change's input rules"
category: legion
tags:
  - forward-merge
  - review
  - line-endings
  - sibling-issues
date: 2026-10-27
status: active
module: skills/legion-worker
---

# At a forward merge, hold your branch's readers of the same input to the incoming change's input rules

- When main brings in a sibling change that reads the same stored text as code on your branch,
  list the input rules that change and its `docs/solutions/` learning state (line ends, a leading
  BOM, CR and CRLF test rows), and check every reader on your branch against them.
- Code that is byte-identical across the merge is not exempt: a rule can arrive with the merge
  while the code that breaks it was already there. The unchanged-diff fingerprint leaves out
  `docs/solutions/`, so a rule that arrives as a learning never changes it.
- Add a stored-body test row for each rule your reader has to follow, not only rows for the
  feature the two changes share.

## Evidence

sjawhar/legion#2311 merged main's INFRA-529 change (#2303), whose learning
`docs/solutions/integration-patterns/cut-source-text-by-mdast-offsets-from-the-text-micromark-counted-INFRA-529.md`
says to end a line at CR, LF or CRLF. The tester exercised directives together with the mermaid
probe and found nothing, and skipped repeating its probes because `mermaid-diagrams.ts` was
byte-identical. The reviewer compared that file with INFRA-529's `drawDirective` and found the probe
still cutting at `\n` alone (since 9607c1b9). The fix, 6b9f6b50, then exposed a second lone-CR
defect in the same file (59dacf3a).
