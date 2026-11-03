---
title: "A Go job's path filter is the closure of what its command compiles, production imports included, and a parent module's go test stops at a nested module"
category: github
tags:
  - paths-filter
  - dorny-paths-filter
  - go-list
  - import-closure
  - nested-go-module
  - ci-cost
date: 2026-10-27
status: active
module: .github/workflows
related_issues:
  - "INFRA-583"
  - "sjawhar/legion#2325"
---

# A Go job's path filter is the closure of what its command compiles

Extends docs/solutions/github/a-worker-image-path-filter-covers-the-code-the-init-container-runs.md
and docs/solutions/testing/a-package-split-holds-each-partition-to-what-it-ships-and-names-the-other-partitions-paths-in-every-filter-LEGION-247.md
with the method for a Go test job.

- **Build the list from the command, not from the test files.** Run `go list -deps -test -f
  '{{if not .Standard}}{{.Dir}}{{end}}'` with the job's own build tags (and `-race` with
  `CGO_ENABLED=1` when the job races), from the job's own working directory. Add the packages of
  every binary its tests build (`go build` inside a test, a `cmdtest.Build`-style helper), and every
  file its tests read outside those packages (`os.ReadFile("../../…")`, a walk of a tree, a fixture
  in another package). Then match every file of every listed directory against the filter with the
  action's own matcher (picomatch `{dot: true}` for dorny/paths-filter) and require zero misses.
- **Write the keep-current rule for production code too.** Any change that makes the job's command,
  or a binary its tests build, compile a new package or read a new file adds it to that job's
  filter in the same pull request. The pull request that adds the import still runs the job and
  passes, so nothing else shows that the filter has fallen behind; say so on the pull request's
  `Not proven / risk` line when no check guards it.
- **Before you call another job the backup, list what its command reaches.** `go test ./...` and
  `go list ./...` stop at any directory with its own `go.mod`: a nested module is never tested by
  the parent module's broad job, so its narrowed job has no backup.
- **Read the open pull requests that add packages to the closure.** A pattern for a path that does
  not exist yet matches nothing, so add the new package's path now and tell the other pull
  request's implementer to keep it when it resolves the conflict; the merge order then stops
  mattering.

## Evidence

- #2325 gave dispatch-memory, dispatch-multiprocess, broker-multiprocess, envoy-go-race and
  envoy-infra-go one filter each in `.github/workflows/envoy-and-contracts.yaml`. The lists came
  from `go list -deps -test` per job (memory 48 package directories, multiprocess 48, broker 29,
  race 70, infra 69 with the root-workspace builds of `cmd/listener` and `cmd/dispatch` that
  `packages/envoy/infra/core/permissions_live_test.go:298-299` runs). A coverage script matched
  every file of each closure: 0 misses; with two patterns removed it reported 1 and 6 misses.
- Review round 1 (thread PRRT_kwDORFy7ds6wmPuo, fixed in d63b45da and c53d0f10): the first rule
  said only "a test that comes to import or read something new", but `dispatch_server` and
  `listener_command` hold only production packages, and open #2229 adds `packages/envoy/listenfd`,
  imported from `cmd/dispatch/main.go`, `cmd/listener/main.go` and `internal/config/config.go`.
- Review round 2 (thread PRRT_kwDORFy7ds6wm6Jj, fixed in 44308a09): the guide said envoy-go "runs
  every untagged unit test on every Envoy change". envoy-go runs `go list ./... | grep -v
  /integration | xargs go test` in `packages/envoy` (`envoy-and-contracts.yaml:1045`), and
  `packages/envoy/infra` has its own `go.mod` and `go.work`, so its `go test ./...` (`:1138`) runs
  only in envoy-infra-go. The tester's `go list ./...` in `packages/envoy` listed 109 packages,
  none under `infra`.
- Measured effect over 987 pull-request runs (2026-10-23T21:05Z to 2026-10-26T20:34Z): 25.0 to
  19.3 billed minutes per pull-request push for the five jobs.
