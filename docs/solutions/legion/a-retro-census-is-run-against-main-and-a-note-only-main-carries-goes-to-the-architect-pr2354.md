---
title: "A retro's census is run against main, not the branch, and a note only main carries goes to the architect because retro may not merge"
category: legion
tags:
  - docs-solutions
  - census
  - retro
  - forward-merge
  - ready
date: 2026-11-02
status: active
module: docs/solutions
related_issues:
  - "sjawhar/legion#2354"
---

# A retro's census is run against main, not the branch, and a note only main carries goes to the architect because retro may not merge

Extends docs/solutions/legion/a-mechanism-removal-reruns-its-learnings-census-at-every-forward-merge-because-main-keeps-writing-notes-that-teach-it-pr2354.md.

That note says to rerun the census at every forward merge and at the head handed to retro. This
adds what retro does when main has moved again, where the remedy that note assumes — a forward
merge — is the one thing retro must not do.

- **Run the census over main as it is now, and over your branch, and diff the two file lists.**
  Your branch's tree is main as of its last merge. A note main added after that merge is invisible
  to a census run in your workspace, and it is the note most likely to teach the mechanism, since
  it was written while the mechanism was still true.

- **A note your branch does not carry is not yours to correct in retro.** READY compares the
  reviewer's approved head with the head on GitHub and refuses anything above it outside
  `docs/solutions/` (`READY_UNREVIEWED_CHANGE`), so a forward merge made in retro sends the change
  back for another review round. Name each such note to the architect with the line that teaches
  the mechanism, and let the architect decide: a review round that merges main, or a follow-up
  issue once this one merges.

- **Correct in place every teaching note your branch does carry, whoever wrote it.** This is the
  one case that overrides
  `skill://legion-retro/references/learnings.md`'s "never edit a document you did not write this
  retro": that rule exists so two retros landing in one hour do not conflict, while a note that
  teaches a mechanism the merge deletes is false for every reader from the merge onward. Keep each
  edit to the sentences that are false, so the conflict surface stays small.

- **Correcting a note is dating it, not deleting its evidence.** A rule that still holds keeps its
  words; the clause that names the removed mechanism gets its tense and a pointer to what replaced
  it. An evidence paragraph stays as it was written and gains one sentence saying the mechanism it
  measured is gone and which change removed it.

## Evidence

sjawhar/legion#2354 moved Legion's handoffs from committed `.legion/<issue>/<phase>.json` files to
versioned records on each Dispatch issue. Its third retro ran the census at head `72f4da54`
against `main@origin` `079e70fe`, which the branch's last merge (`0642a37a`) predates:

- The branch held 1,126 active notes; 108 matched the mechanism's vocabulary; 61 of those the pull
  request had already corrected, and of the 64 it had not, eleven still carried a step or a
  present-tense sentence that was false. Those eleven were corrected in place.
- Main carried twelve notes the branch does not. One of them teaches the mechanism:
  `docs/solutions/legion/a-term-added-to-an-expression-another-open-pull-request-rewrites-is-guarded-by-a-test-file-of-its-own-pr2620.md:28`
  reads "`.legion/<issue>/plan.json` is gone the moment the merge lands. Put it in the learning
  document". It went to the architect rather than into the retro commit, because bringing it into
  the branch means merging main above an approved head.
- The ratio is what makes the diff worth running: at this retro, one note in twelve that only main
  carried taught the mechanism, against eleven in 1,126 of the branch's own.
