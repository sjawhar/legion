---
title: "A new package subdirectory needs its own filter line where the filters glob one level"
category: github
tags:
  - ci-filters
  - path-filter
  - go-package
date: 2026-10-30
status: active
module: .github/workflows
related_issues:
  - "INFRA-562"
  - "sjawhar/legion#2387"
---

# A new package subdirectory needs its own filter line where the filters glob one level

- A path filter written `dir/*` matches the files directly in `dir`, never `dir/sub/file.go`. When
  you add a package under a directory a filter already names that way, add a `dir/sub/*` line beside
  it in every filter that has the parent, and in any `on.push.paths` list the check holds equal to a
  filter (`docs-pages.yaml`'s equals the `docs` filter). A filter written `dir/**` needs nothing.
- Run `bash .github/scripts/check-ci-filters.sh` before the push. It needs PyYAML, which an issue pod
  lacks: `uv run --with pyyaml==6.0.2 bash .github/scripts/check-ci-filters.sh`.
- Read its result on the tree CI judges, the head merged with `main@origin` (`git merge-tree
  --write-tree <head> main@origin`). A problem the head alone reports in a file your branch does not
  touch may be main's, already fixed on main, and absent from CI's merge ref.

## Evidence

The new package `packages/envoy/internal/dispatch/githubapp/ratelimit/` was read by `test`, `docs`,
`dispatch`, `dispatch-browser-multiprocess` and `listener-server-change`, whose filters named only
`packages/envoy/internal/dispatch/githubapp/*`; `ci-filters` failed with five `… reads
…githubapp/ratelimit/ratelimit.go, which its filter … does not match, so a change to it skips the
job`. One line each in `.github/workflows/ci.yaml` (the `dispatch_server` and
`listener_server_change` filters) and `.github/workflows/docs-pages.yaml` cleared them. The same run
on the head alone still reported `listener-server-change` reading
`api/testdata/decision-block-inserts.json`, a gap main had at the branch's base and has since fixed,
so CI's merge ref did not report it. Pull request sjawhar/legion#2387.
