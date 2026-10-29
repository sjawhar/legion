---
title: "An input-list test makes the build report its reads and matches them with the consumer's engine"
category: testing
tags:
  - build-inputs
  - pathspec
  - git
  - bun
  - derived-enumeration
date: 2026-10-29
status: active
module: packages/claude-envoy
---

# An input-list test makes the build report its reads and matches them with the consumer's engine

Extends docs/solutions/testing/a-test-over-a-contract-derives-the-enumeration-and-hand-writes-only-what-the-contract-cannot-prove-about-itself.md.

## The rule

1. When a hand-kept list must name every file a build reads, change the production code so it
   returns what it read: the bundler's metafile inputs, and the source each helper copied or
   converted. The test lists no input itself. A copy of the reads in the test passes when the build
   starts to read a new source.
2. Match the list with the same engine its real consumer uses. For a list of git pathspecs, create
   an empty file at each reported path in a scratch `git init` repository and run
   `git ls-files --others -z -- <pathspecs>` there. Then pathspec magic such as `:(exclude,glob)`
   is checked by git itself, and no workspace git or jj state takes part. Fail when the list has no
   pathspec line, since git then lists everything.
3. The list's header says which entries no test checks (the files scripts read by name) and which
   direction of error is safe. List whole directories and exclude only known non-inputs, so a file
   nobody thought of falls on the safe side.

## Evidence

- sjawhar/legion#2493: `buildBundles` and `buildLegionPlugin` in
  `packages/claude-envoy/scripts/build.ts` return `Promise<string[]>` of absolute reads.
  `packages/claude-envoy/tests/build-inputs.test.ts` merges both results and matches them with git
  (`ls-files` at line 113). It runs in ci.yaml's `test` job, which `ci-ok` sums up.
- Review thread PRRT_kwDORFy7ds6xe-nV: round 1's test copied `buildLegionPlugin`'s reads by hand.
  Fixed in b36421220f41. Removing `packages/pi-legion/agents` from the list then fails the test,
  naming the six agent files from the build's own report.
- Review thread PRRT_kwDORFy7ds6xe-ng, fixed in 38789e6762cf: the docs claimed the test covered
  every read. `packages/claude-envoy/scripts/build-inputs.txt:17` now names the hand-listed rest,
  and line 24 states the safe direction.
