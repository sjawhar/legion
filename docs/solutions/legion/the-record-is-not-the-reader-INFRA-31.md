---
title: "The record is not the reader: capture is not propagation, so grep your own handoffs before you write an instruction"
category: legion
tags:
  - handoffs
  - documentation
  - instructions
  - retro
date: 2026-10-21
status: active
related_issues:
  - "INFRA-31"
  - "sjawhar/legion#2085"
---

# The record is not the reader: capture is not propagation

Legion's handoffs work. `rm -rf` of a Go module cache failed in this issue's own pod — 19
`Permission denied` lines, exit 1, the extracted source left behind — and the implementer did
exactly what the process asks: it recorded the failure as a `trickyParts` entry in
`.legion/INFRA-31/implement.json`, at commit `9798c9f3a0f6` of this issue's branch. Capture
succeeded. (The commit is named because the entry is not readable at the merged head: every later
round overwrote that file, and retro removes the directory before the merge — so the record this
document is about outlives itself only as a commit id, which is the document's own point one turn
further on.)

A round later, the same role wrote an operator instruction in `docs/kubernetes.md` telling someone
to delete those directories with a plain `rm -rf`. It had watched the command fail, had written
the failure down, and then wrote the instruction anyway. The record existed; nobody read it. A
reviewer caught it, and the paragraph needed two further corrections after that.

So the handoff is a write-only medium unless something makes you read it. The rule that makes you:

**Before you write an instruction, grep your own handoffs for what it tells someone to do.**

```sh
grep -i 'rm -rf\|chmod\|docker\|curl\|kubectl' .legion/<ISSUE>/*.json
```

Pair this with, do not merge it into, the tester's rule that **an instruction is a claim and is
checked by running it**. They catch different failures: running the instruction catches one you
have never tried, and grepping the record catches one you have already seen fail and forgotten.
Tonight's defect was the second kind, and running it would have caught it too — but only after
someone thought to run a sentence in a documentation paragraph, which nobody does by default,
whereas grepping four JSON files costs a second.

The general form, worth holding beyond handoffs: **a process that captures a finding has not
propagated it.** Any artefact you write — a plan, a comment, a solutions file, this file — is
inert until a later step is defined that reads it. When you add a capture step to a process, name
the step that consumes it, or you have added bookkeeping rather than learning.
