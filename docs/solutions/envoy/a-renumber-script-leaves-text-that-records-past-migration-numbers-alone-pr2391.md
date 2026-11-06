---
title: "A renumber script leaves text that records past migration numbers alone"
category: envoy
tags:
  - migrations
  - forward-merge
  - renumber
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2391"
---

# A renumber script leaves text that records past migration numbers alone

Extends docs/solutions/envoy/a-merge-that-only-renumbers-migrations-is-proven-in-both-directions-pr2391.md.

- Exclude from a migration renumber every file that records numbers as history: the branch's own
  `docs/solutions/` learnings, and any changelog or note that says which number a migration had
  before. Those numbers were true when written and stay as written.
- After the renumber, grep the excluded paths for the numbers the script moved, and read each hit.
  Neither proof in the extended document sees this edit. The text is the branch's own, so the diff
  from `main@origin` removes no line of main's. And the normalised fingerprint writes the old and
  the new number as the same placeholder.

## Evidence

Pull request sjawhar/legion#2391, forward merge f6aae9bade24. The script that moved the branch's
migrations from 0145-0151 to 0148-0154 skipped only lines present in main's file. It also rewrote
the history in the learning above ("moved from 0141-0147 to 0142-0148 … and then to 0145-0151").
The fingerprint (`6fa0b746c92d`) and the main-side check both passed. The edit was caught by
reading the renumber's diff, and the file was restored from e027da2c9b7a before the push (PR
comment 6517841104).
