---
title: "The filter-drift gate is one-sided: a filter wider than its closure passes, so a long job's narrowness is a review obligation"
category: github
tags:
  - github-actions
  - ci-router
  - paths-filter
  - check-ci-filters
  - go-list
  - review
date: 2026-10-29
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2516"
---

# The filter-drift gate is one-sided: a filter wider than its closure passes

Extends docs/solutions/github/a-go-jobs-path-filter-is-the-closure-of-what-its-command-compiles-production-imports-included-and-stops-at-a-nested-module-INFRA-583.md.

`.github/scripts/check-ci-filters.sh` fails a filter that misses a file its job reads. It does
not fail one that matches far more, so `packages/envoy/**` on a half-hour job passes the gate
exactly as the job's own closure does. Whether an expensive job runs on changes it cannot be
affected by is a judgment no program makes here.

- **Write the narrow closure for any job long enough to matter**, and say in the file why it is
  not the broad pattern. In this repository the line is about twenty minutes: a job of that
  length must not start on every change under a whole module.
- **Compute the closure with the gate's own engine, not by hand.** `ci_filter_coverage.closure()`
  takes a `Run` and returns every `Read` with its reason, so one call gives the exact pattern
  list before a line of YAML is written, and the gate then passes first time.
- **The closure is imports plus embeds plus testdata plus the hand-kept out-of-package reads.**
  `go list -deps -test` gives the directories; the engine adds `<dir>/testdata/**` for every
  directory a test run compiles and the `EmbedPatterns` of each package, and the rows of
  `.github/scripts/ci_job_reads.py` give the files a test opens through `..`.
- **A filter whose narrowness you chose is a filter a reviewer must read.** Say so in the body:
  the gate's green is not evidence that the filter is right, only that it is not too small.

## Check it

```sh
PATH=<a python with PyYAML>:$PATH bash .github/scripts/check-ci-filters.sh
```

Delete one pattern and run it again as the control: it must exit 1 and name each uncovered file
with the go command that reads it.

## Evidence

sjawhar/legion#2516 gave `packages/envoy/internal/dispatch/docs` a race job whose filter is the
package's own closure: 24 first-party directories as `<dir>/*`, the three `testdata` trees in
that closure, the migrations and the pmdoc schema those packages embed, the workspace module
files, `.github/workflows/ci.yaml`, and the one fixture directory a docs test opens outside the
module. The gate reported `envoy-go-race-docs: 755 files, 0 uncovered` — and, in the same run,
`envoy-go-race: 2072 files, 0 uncovered` for a job whose filter is still `packages/envoy/**`.
Both pass. Copying the broad pattern to the new job would have passed too, and would have kept
starting a job of twenty to thirty minutes on every change under that tree, which was the whole
point of the work.

The docs filter was built the long way — `go list -e -deps -test -race -f '{{.Dir}}'`, then a
hand check of each closure directory for `testdata` and of each package for `EmbedPatterns` —
and converged against the gate. Later in the same issue two more such filters were needed, and
`ci_filter_coverage.closure()` was called directly on each candidate `go test` command instead:
it printed 18 globs with their reasons for one and 81 for the other, those lists went into the
YAML unchanged, and the gate passed on the first run with no convergence round at all. That is
the method to use.

The controls: deleting `packages/envoy/internal/dispatch/text/*` took the gate to
`721 files, 8 uncovered`, exit 1, each error naming
`go test -race -timeout 60m ./internal/dispatch/docs/` as the reader; deleting
`packages/envoy/internal/dispatch/pmdoc/schema/**` took it to `755 files, 1 uncovered` naming
`blocks.json` through its embed. Restoring each returned `0 uncovered`, exit 0.
