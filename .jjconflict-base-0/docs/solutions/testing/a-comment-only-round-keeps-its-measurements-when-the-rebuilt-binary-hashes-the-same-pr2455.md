---
title: "A comment-only round keeps its measurements when the rebuilt binary hashes the same"
category: testing
tags:
  - go
  - benchmark
  - load-test
  - simplify-pass
  - review-round
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2455"
---

# A comment-only round keeps its measurements when the rebuilt binary hashes the same

When a simplify pass or a review round changes only comments in runtime Go code after you have
measured it (load-test replays, profiles, benchmarks), prove the measured build is unchanged
instead of measuring again:

- Rewrite a comment at the same number of lines. Go records line numbers in the binary's line
  table, so a line added or removed above a function changes the binary.
- Build both heads the same way: `go build -trimpath` from `git archive` exports of each head
  (`GOWORK=off` in an export of one module), or both from the same export directory. Without
  `-trimpath` the directory path is in the binary, and a build inside a git working copy stamps
  the commit.
- Compare the two binaries' SHA-256. Equal: the earlier runs stand for the new head; say so in
  the PR body with the hash. Different: measure again.
- Compare hashes from one build pipeline only: two roles' builds of the same head differ.
- The hash covers runtime code only. A round that changes tests still runs those tests.

## Evidence

- On sjawhar/legion#2455 the six base-against-head replays ran on the server built at efb63bc6.
  The simplify pass then rewrote a 3-line comment in `pmdoc/walk.go` as 3 lines; the rebuild had
  the same SHA-256 (`fdc01114…26d57`), so the replays stood. Review round 1's contract comment
  (32065ac7) was rewritten at 2 lines, and the builds at 914ac117 and cfec98cf both hashed
  `9aa858a8…a4b9`. The tester's `-trimpath` builds of the same pair hashed `b81b9e1d…` both,
  while base adee0aba, built the same way, hashed otherwise.
