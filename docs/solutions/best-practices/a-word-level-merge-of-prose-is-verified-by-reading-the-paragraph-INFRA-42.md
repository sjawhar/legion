---
title: "A word-level merge of prose is verified by reading the paragraph, not by diffing against both parents"
category: best-practices
tags:
  - agents-md
  - documentation-edits
  - merge-conflicts
  - verification
  - jj
date: 2026-10-20
status: active
module: packages/dispatch/AGENTS.md, packages/envoy/AGENTS.md
related_issues:
  - "INFRA-42"
  - "sjawhar/legion#2086"
---

# A word-level merge of prose is verified by reading the paragraph, not by diffing against both parents

Extends `docs/solutions/best-practices/a-multi-kilobyte-single-line-agents-md-bullet-is-edited-whole-and-checked-with-a-word-diff-against-the-committed-line-LEGION-752.md`.
That entry's word diff proves no text was lost or doubled when one side edits such a line. This is
the delta for the case where **both** sides edited it and the conflict must be merged:

- A word-level three-way merge of two long markdown lines is legitimate when each side's edits are
  disjoint in the base's words — but its verification cannot be a diff. Diffing the result against
  each parent passes on a result whose sentences are in the wrong order, because every word is
  present on both sides and no hunk is missing.
- So **read the merged passage as prose** before committing, from the first sentence of the
  paragraph to the last, and check that each sentence's subject, parenthetical and continuation are
  still its own. That is the only check that sees a scramble.
- A conflict in a file inside the unchanged-diff fingerprint's fileset — anything but `.legion/`
  and `docs/solutions/` — moves the fingerprint even when no runtime file changed; a documentation
  file is enough. Record both hashes, why the hash moved and the two-way verification on the pull
  request, and leave the gate choice to the tester rather than arguing it is equivalent.

## Evidence (INFRA-42, sjawhar/legion#2086)

`main` and the issue branch had each rewritten the same two ~30 KB bullets of
`packages/dispatch/AGENTS.md`: `main` added the Activity tab's `t v` chord and its keymap wording,
the branch rewrote the Inbox's marks rule, the band vocabulary and the drawer's callback. The
resolution merged their word-level edits and was verified both ways — against `main` it differed by
the branch's six hunks, against the branch by `main`'s changes alone, with no markers and none of
the deleted vocabulary left.

It still shipped a scrambled paragraph. At `packages/dispatch/AGENTS.md:967-971` the
`seedWorkspace() expects an empty database…` sentence had been lifted above the parenthetical
belonging to the sentence before it, so `…asserts every human-only surface it fills` ran straight
into it and `it waits for…` followed a closing parenthesis rather than its own subject. Both
parents read correctly; the reviewer caught it by reading the file, and the repair was `+1/-1`
(`b87105c7246a`). The fingerprint had moved from `7cbe0076…` to `a73e7599…` for that one file,
which is what told the tester this was a full round rather than a confirmation.
