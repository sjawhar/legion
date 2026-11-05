---
title: "A change that removes a mechanism reruns its docs/solutions census at every forward merge, because main keeps writing notes that teach the mechanism"
category: legion
tags:
  - docs-solutions
  - supersedes
  - forward-merge
  - census
date: 2026-10-31
status: active
module: docs/solutions
related_issues:
  - "sjawhar/legion#2354"
---

# A change that removes a mechanism reruns its docs/solutions census at every forward merge, because main keeps writing notes that teach the mechanism

Extends docs/solutions/legion/handoffs-are-dispatch-records-and-the-notes-that-teach-handoff-commits-are-superseded-pr2354.md.

That note's rule 3 says to grep `docs/solutions/` for the removed mechanism and to supersede or
correct each note that teaches it. This adds when to run the grep, and how wide it is.

- **Run the census after every forward merge with main, and at the head you hand to retro.** While
  your pull request is open, other pull requests land on main with new learnings. Each one is
  true of main when it is written, so it teaches the mechanism your branch removes. A forward
  merge brings it in without a conflict.
- **Search the mechanism's whole vocabulary, not its best-known words.** List every name a worker
  meets the mechanism by: its files and directories, its commit and push shapes, its commands,
  its error codes and the phase step that served it. Then grep active notes for each one.
- **Read each hit for what a worker would do today.** Correct in place a step or a present-tense
  sentence that is now false. Supersede a note whose title or whole rule is the mechanism. Leave
  dated evidence alone.
- **The chase ends at the merge.** A note main writes after your change merges describes the new
  main, so it is not your change's to correct.

## Evidence

sjawhar/legion#2354 moved Legion's handoffs from committed `.legion/<issue>/<phase>.json` files to
Dispatch records. Its round-1 census marked the matching notes "historical, kept". Later rounds
each found more:

- Review round 13 found two active notes that merge 588dad34 had brought in (pr2512's step "let
  the handoff follow in a `.legion/`-only commit carrying `skip-checks: true`", and pr2504).
- The tester's round 17 failed the next head on an older note with the same step: round 13 had
  corrected only the two notes the reviewer named, with no new search. The search then run
  (`skip-checks`, `.legion/`, `handoff-only`, `handoff commit`, the handoff file names, retro's
  removal) found 38 active notes that taught the mechanism.
- The tester's round 18 failed the next head on main's pr2542 note, which merge 1064e218 had
  brought in after that census ran.

Each was corrected in place or superseded, and listed in the superseding note.
