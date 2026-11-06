---
title: "After a forward merge, a drop in a project's row count is a failure until the plan explains it"
category: testing
tags:
  - forward-merge
  - playwright
  - test-selection
  - phone
date: 2026-10-28
status: active
module: packages/dispatch/e2e
---

# After a forward merge, a drop in a project's row count is a failure until the plan explains it

Extends docs/solutions/legion/at-a-forward-merge-hold-your-branchs-readers-of-the-same-input-to-the-incoming-changes-input-rules-pr2311.md.

- After each forward merge, compare your spec's per-project row counts with the last run before
  the merge (`grep -oE "\[(chromium|iphone|webkit|firefox)\]" <run log> | sort | uniq -c`, or
  `--list` per project). Main can change which tests a project selects, and a project that now
  runs none of your rows passes the run without a word.
- A count that dropped is a failure of the merge until it is checked against the plan: if the plan
  proves those rows on that project, restore them (for Dispatch's `iphone` project, ` @phone` at the
  end of each title, docs/solutions/testing/a-playwright-project-selected-by-tag-takes-the-tag-in-the-title-matches-it-as-a-whole-word-and-is-proven-by-an-exact-list-INFRA-521.md)
  in the same merge round. Explaining the drop in the handoff is not restoring it.

## Evidence

sjawhar/legion#2311: the merge of main 460facf5 (81b5d79e) brought in #2313, which runs the
`iphone` project only for `@phone`-tagged titles. The mermaid suite went from 32 rows to 24 (8 each
in three projects, none on `iphone`), and the implementer recorded that as main's change. The plan
proves the comment, ask, message and reply rows on the iPhone viewport, so the tester failed the
round; tagging all eight titles (622614ce) brought the run back to 32, 8 on each project.
