---
title: "A first-retro note a later round falsifies is corrected in that round, so the second retro meets a true page"
category: legion
tags:
  - retro
  - second-retro
  - learnings
  - never-edit-rule
date: 2026-10-31
status: active
module: skills/legion-retro
related_issues:
  - "INFRA-336"
  - "sjawhar/legion#2585"
---

# A first-retro note a later round falsifies is corrected in that round, so the second retro meets a true page

Three documents disagree about what a second retro on one pull request may touch:

- `skill://legion-retro/references/learnings.md` — "Never edit a document you did not write this
  retro. Not its front matter, not its body", because two retros can land within the same hour.
- `docs/solutions/legion/a-conflict-round-that-changes-the-fingerprint-is-a-counted-review-round-and-its-approval-starts-a-second-retro-LEGION-578.md`
  and
  `docs/solutions/legion/a-forward-merge-after-retro-reinstalls-when-main-bumped-the-lockfile-keeps-both-changelog-insertions-and-still-runs-test-review-and-a-second-retro-LEGION-634.md`
  — the second retro writes new files and edits none of the first retro's.
- `docs/solutions/legion/a-forward-merge-after-retro-falsifies-the-first-retros-own-citations-pr2542.md`
  — the second retro's first step is a sweep that edits those pages in place, and the never-edit
  rule binds concurrent retros rather than your own round.

The case that reconciles them is the one where the falsifying round is still open:

- **A first-retro page that a later round of the same pull request falsifies is corrected in that
  round, by the role that owns the head.** A reviewer's prose commit reaches it as it reaches the
  prompt, the changelog fragment and a doc comment: one sweep of one claim, at the head that
  ships. The second retro then has nothing to arbitrate, writes only new files, and the three
  documents above agree on that outcome whichever way they read the rule.

- **What the second retro still owes the first is a read, not an edit.** Re-read every page the
  first retro wrote against what the later rounds measured. A claim a later round made false, and
  nobody corrected, is the second retro's finding: name it to the architect so the running phase
  can take it, rather than leaving a wrong page behind.

- **A correction you may not make in place is a new page that extends the old one.** It is the
  same rule as for any other author's note, and it keeps both rounds' evidence readable.

## Evidence

INFRA-336's first retro wrote five pages. A later round measured one of their claims false: the
page on a field that is load-bearing through its bytes said `previousHandoff` is "the commit that
carried the role's previous handoff of the phase", while the revset the prompt then gave answered
retro's own removal commit after a retro. The reviewer's prose commit `59a4e931` corrected the
prompt, the four role residues, the changelog fragment, `score.ts`'s doc comment **and that retro
page**, in one commit at the head that ships. Both the field and that revset are gone since
sjawhar/legion#2354, which made each handoff a versioned record on its Dispatch issue; the rule
above is about correcting a falsified page, not about the field.

The second retro therefore edited none of the five. It re-read them, found one more claim that the
rounds since had narrowed — the first page calls the transcript provenance check binary, and a
later round found a value that passes it and is still false — and wrote that correction as a new
page extending it, which is what the never-edit rule leaves available.
