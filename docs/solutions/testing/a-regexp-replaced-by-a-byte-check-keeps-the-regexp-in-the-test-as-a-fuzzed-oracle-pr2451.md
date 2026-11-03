---
title: "A regexp replaced by a byte check keeps the regexp in the test as a fuzzed oracle"
category: testing
tags:
  - regexp
  - oracle
  - fuzz
  - hot-path
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2451"
---

# A regexp replaced by a byte check keeps the regexp in the test as a fuzzed oracle

- When a hot-path regexp becomes a byte check, keep RE2's exact meaning:
  - `.` does not match `\n`, so a prefix that `(.*)` must match cannot hold a newline;
  - `$` without `(?m)` is the end of the text;
  - a greedy `(.*)` before an anchored fixed-length tail leaves one candidate split, so check the
    tail first and the prefix after it.
- Move the old regexp into the `_test.go` file as the oracle. Keep a table of edge cases (a
  suffix one byte too long, one byte too short, one bad tail byte, a newline in the prefix, an
  empty prefix, the tail pattern repeated) and a fuzz target that compares the new function with
  the oracle on every input. The next change to the function then has its check already in place.

## Evidence

- `pmdoc/node.go:93` `yattrToMarkName` replaced `^(.*)--[a-zA-Z0-9+/=]{8}$` → `$1`. Every reader
  of a Y.Text run calls it for each attribute key. The newline guard is at `:103`.
- `pmdoc/mark_name_test.go:10` keeps the regexp as `yattrMarkSuffixOracle`. `:12` holds the table,
  and `:37` `FuzzYattrToMarkName` ran 1.04 M inputs with no difference.
- The tester took the newline guard out of a head export, and the pull request's own tests failed
  on that mutant (the tester's handoff in commit 6ae486da, `testsJudged`).
- The plan measured `treeOf` 22-26% faster without the regexp, on three LINUX specs (the plan
  handoff in commit bb536ecf).
