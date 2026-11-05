---
title: "A proof re-run at a merge head moves its control too, and a harness-dependent figure carries its seed"
category: github
tags:
  - forward-merge
  - evidence
  - benchmarks
  - harness
date: 2026-11-03
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2641"
---

# A proof re-run at a merge head moves its control too, and a harness-dependent figure carries its seed

Extends docs/solutions/github/after-a-forward-merge-rerun-the-suites-no-required-job-runs-and-date-every-proof-to-the-merged-head-pr2354.md,
which says to re-run the suites and date every proof to the merged head. Two things it does not
say, both met when a forward merge brought 94 changed production files of one package into a
branch.

## Rules

- **The control moves with the head.** A before/after measurement at a merge head is only an
  answer when its base side is the *base's new tip*, built the same way at the same time: the old
  base build measures a world neither side is in any more. Build both from `git archive` exports
  of their own commits rather than from the working copy, record each binary's `sha256` beside its
  commit, and let one harness — one fake upstream, one seeder, one script — serve both sides, so
  the binary is the only difference between them.
- **Separate the figures a harness can move from the ones it cannot.** Counts of operations
  (messages published, requests made, lookups spent) reproduce exactly across harnesses; absolute
  byte sizes do not, because they carry the seeded record's own fields and key lengths. State the
  seed's shape beside any absolute byte figure, or commit the generator, or report the count and
  leave the byte figure as a measurement of that run.
- **A harness under `$TMPDIR` dies with its pod**
  (docs/solutions/legion/a-proof-harness-a-later-round-reruns-lives-on-the-pull-request-not-the-issue-disk-pr2362.md),
  so a later round rebuilds it from the pull request body. That body is the specification: it has
  to name the fake's behaviour, the seed and the command, or the rebuild measures something else
  and the difference looks like a regression.
- **Report a moved row rather than quietly restating it.** A row that moved with the harness and a
  row that moved with the code look identical in a table; only the text beside them tells a
  reviewer which is which.

## Evidence

- sjawhar/legion#2641 re-measured both of its production-surface proofs at the merge head
  `a2266ca4` against main's new tip `ce857df0` (binaries `sha256 0ef95864…` and `32f607e7…`, one
  harness, `envoy-dispatch` the only difference). Every expectation held: a sibling source's sync
  answered 200 in 0.015 s against 5.878 s, two concurrent syncs cost 1 installation and 1 head
  lookup against 2 and 2, and one sweep over 10,000 open not-due deliveries took 6 messages /
  2,783 bytes against 20,007 / 10,980,136.
- Two rows moved and neither is the change's: the bucket's cost read +118.0 bytes per open
  delivery and +153.0 per request, against +138.0 and +127.0 from the harness that went with its
  pod. Those two are the size of a seeded record and of an entry key; the same measurement's
  counts — 20,001 messages with the index against 10,000 without, 30,001 after one move of each
  entry — reproduced exactly.
- The branch's own figures were measured by a harness at `/legion/scratch/tmp/infra627`, rebuilt
  twice in this issue's life from the pull request body's `## Measurements` section.
