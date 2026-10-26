---
title: "An exclusion case that empties the fixture proves nothing about the filter beside it"
category: testing
tags:
  - test-coverage
  - mutation-check
  - sql-filters
  - dispatch
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch/refs/refs_test.go
related_issues:
  - "INFRA-364"
  - "sjawhar/legion#2201"
---

# An exclusion case that empties the fixture proves nothing about the filter beside it

Extends docs/solutions/testing/assert-the-rendering-in-the-shape-the-consumer-passes-not-the-default-INFRA-62.md.

- **A query with an optional argument has a shape per argument; test the one production passes.**
  When a new filter sits beside an optional exclusion, cover it with the exclusion every production
  caller passes (Dispatch's ask reads always pass `refs.AskBacklinkExclusions`), not only with none.
- **The fixture must hold a row the exclusion keeps and the new filter drops.** An exclusion that
  removes every fixture row returns empty whether the new filter applies or not, so that case
  locks the exclusion and nothing else. Keep it, and add the case where the exclusion leaves rows
  in and the new filter is what changes the answer.
- **Name the mutation before writing the case:** "the new filter applies only when no exclusion is
  passed" is the plausible bug when the SQL is built by appending; run it and see the case fail.

## Evidence

The first round of `TestGraphReadsSkipANodeKindThisBuildCannotAddress` counted with no exclusion
(`{CORE-1: 1}`) and with `excludeKinds = [mentions]` (empty). Every fixture row was a `mentions`
row, so the second case was empty with or without the node-kind filter. The reviewer built the
mutation above in a copy of `packages/envoy`: all three new tests passed, while every ask card and
Inbox row would have counted an unknown-kind citer again. The added case,
`BacklinkCounts(…, AskBacklinkExclusions)` requiring `{CORE-1: 1}`, fails under it with
`refs_test.go:386: issue backlink counts with the ask exclusions = map[CORE-1:2 OPS-1:1]; want
map[CORE-1:1]`.
