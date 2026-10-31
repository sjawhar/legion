---
title: "A forward merge into an opt-in test selection classifies every test main added before the exact-set check"
category: testing
tags:
  - playwright
  - tags
  - forward-merge
  - test-selection
date: 2026-10-27
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-521"
  - "sjawhar/legion#2313"
---

# A forward merge into an opt-in test selection classifies every test main added before the exact-set check

Extends docs/solutions/testing/a-playwright-project-selected-by-tag-takes-the-tag-in-the-title-matches-it-as-a-whole-word-and-is-proven-by-an-exact-list-INFRA-521.md.

When a branch makes a project run only opted-in tests (a tag, a grep, an allowlist), a test that
nobody classified is left out with no failure. A forward merge of main brings in such tests, and
the merge conflict shows only the files both sides edited. After every forward merge:

1. List every test main added or edited since the old fork point: the full project's `--list` at
   the merged head minus the selection table's keys, and the test bodies main's diff touches.
2. Apply the branch's rule to each of them, and tag the ones that qualify, clean merges included.
3. Then run the exact-set check: the selected project lists the old keep rows plus the new ones,
   and every other project lists what main lists plus the branch's own changes.

Comparing against the old table alone cannot catch the gap, because main's new tests are in
neither list.

## Evidence

sjawhar/legion#2313, merge 71eaed83 of main aaf24e34: the conflict touched only
`playwright.config.ts` and `theme.e2e.ts`, where `theme.e2e.ts:169` was tagged while resolving it.
Main had added 22 tests. Seven more of them took a phone-only path and merged cleanly with no tag:
`pictures.e2e.ts:367`, `text-typed-blocks.e2e.ts:34` and `:79`, and `task-items.e2e.ts:236` and
`:267` in light and dark. Main's authors had written them for a project that ran every test, so
they branch on the phone layout (`conversationThreadScope`, `openCommentThread`,
`openChecklistThread`). Untagged, `iphone --list` showed 224 tests; tagged, 231 (table v2's 223 plus
8), with `chromium` at 808 (786 plus main's 22).
