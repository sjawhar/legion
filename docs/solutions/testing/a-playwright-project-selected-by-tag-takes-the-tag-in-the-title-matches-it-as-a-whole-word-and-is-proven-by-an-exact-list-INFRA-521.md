---
title: "A Playwright project selected by tag takes the tag in the title, matches it as a whole word, and is proven by an exact --list"
category: testing
tags:
  - playwright
  - tags
  - grep
  - biome
  - dispatch
date: 2026-10-27
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-521"
  - "sjawhar/legion#2313"
---

# A Playwright project selected by tag takes the tag in the title, matches it as a whole word, and is proven by an exact --list

Extends docs/solutions/testing/a-layout-change-is-proven-by-the-whole-project-and-the-rig-needs-contracts-built-and-three-engines-it-cannot-start-INFRA-92.md.

- **Write the tag at the end of the title** (`test("… @phone", fn)`), not as a `{ tag: "@phone" }`
  details argument. Playwright reads an `@word` in a test or describe title as a tag. A third
  argument breaks Biome's hugging of a test call, so Biome re-indents every tagged test's body.
- **Match the tag as a whole whitespace-separated word**: `grep: /(^|\s)@phone(\s|$)/`. A project's
  `grep` runs on one string that joins the project name, the file path, the describe titles, the
  test title and the details tags. An unanchored `/@phone/` would also match `@phones` or
  `x@phone`.
- **Prove the selection as an exact set.** Run `--list --reporter=json` for every project, key each
  test by file › title path with the tag stripped, and compare it with the intended list: 0 extra,
  0 missing, and every other project's set unchanged. A missing tag fails nothing; the test only
  drops out of the project.

## Evidence

sjawhar/legion#2313: the `{ tag }` form gave 62 files, +9255/−8963, almost all re-indentation; the
title form gave 65 files, +362/−415. The grep string is `_grepTitleWithTags()` in Playwright 1.63
(`lib/common/index.js:2837`); title tags come from `TestCase.tags` (`:2752-2760`). Removing one
tag took `iphone --list` from 222 to 221 tests with no error, and a glued `phone@phone` in an
untagged title left it at 222. The `--list` exact-set check against the per-test table was the
evidence every phase used.
