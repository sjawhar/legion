---
title: "Prove a rolling-release claim with the two-build rig"
category: testing
tags:
  - rolling-deploy
  - two-builds
  - dispatch
  - compatibility
date: 2026-11-04
status: active
module: packages/envoy/cmd/dispatch
related_issues:
  - "sjawhar/legion#2461"
---

# Prove a rolling-release claim with the two-build rig

- A claim that Dispatch's previous release and this head can share one database and one NATS during a roll is proven on real processes of both builds, with the rig this repository already has, not with a pair of pods or a hand-built old binary.
- Build the previous release with `bash packages/envoy/scripts/build-dispatch-build.sh --previous-release --out <dir>` (`--resolve` prints the tag and commit without building). Then run `DISPATCH_PREVIOUS_BUILD=<dir> go test -tags twobuild -count=1 -timeout 30m ./cmd/dispatch` from `packages/envoy`, and the browser direction from `packages/dispatch` with `DISPATCH_E2E_BINARY_B=<dir>/envoy-dispatch DISPATCH_E2E_WEB_DIST_B=<dir>/web bun run e2e:multiprocess -- cross-build.multiprocess`.
- The script resolves the newest `legion-envoy-v*` tag in the base's history, so resolve it again after every merge of main: the previous release moves.
- Add a new compatibility case as a test under the `twobuild` tag, so CI's `dispatch-two-builds` job runs it on every change that job's filter selects.

## Evidence

- The rig (`packages/envoy/scripts/build-dispatch-build.sh`, `packages/envoy/cmd/dispatch/twobuild_harness_test.go`, `packages/dispatch/e2e/cross-build.multiprocess.e2e.ts`, CI job `dispatch-two-builds`) was added by this pull request. It builds from `git archive`, refuses a commit older than the first build that shares a database, and the Go suite fails naming what is missing when the directory is incomplete.
- It found a real defect, not only confirmations: the shutdown order between the delivery intakes and the pool (f0425c2a, fixed in cc73bbec).
- Across this pull request's merges of main the resolved previous release moved from `legion-envoy-v24.7.0` to `v29.0.0`, and the suite was re-run against each.
