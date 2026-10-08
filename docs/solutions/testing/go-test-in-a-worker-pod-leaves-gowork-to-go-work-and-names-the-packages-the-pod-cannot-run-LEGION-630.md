---
title: "go test ./... in a worker pod: leave GOWORK to go.work (GOWORK=off breaks the daemon's envoy import), and name the packages the pod cannot run so a wall of FAIL reads as the environment, not the branch"
category: testing
tags:
  - worker-pod
  - go-toolchain
  - go-work
  - testcontainers
  - proof-environment
  - daemon-go
date: 2026-10-08
status: active
module: packages/daemon
related_issues:
  - "LEGION-630"
  - "sjawhar/legion#1845"
---

# go test ./... in a worker pod: leave GOWORK to go.work, and name the packages the pod cannot run

Extends docs/solutions/testing/a-worker-pod-ships-no-go-or-tmux-provision-them-under-home-each-relaunch-and-record-every-figure-in-the-handoff.md.

- Do not export `GOWORK=off` for `go test` inside `$LEGION_WORKSPACE`. `packages/daemon/go.mod`
  replaces `github.com/sjawhar/envoy => ../envoy`, and the go.sum entries that module's packages
  need (`aws-sdk-go-v2/service/kms` for `internal/broker/policy`) live in `packages/envoy/go.sum`,
  which only the `go.work` workspace mode consults. With `GOWORK=off`,
  `internal/agentsecrets` fails at setup with `missing go.sum entry for module providing package
  … (imported by github.com/sjawhar/envoy/internal/broker/policy)`. The recipe's `GOWORK=off` is
  for a scratch copy of a package outside the repository only; its sentence that the daemon has
  no intra-repository module dependency is no longer true.
- Expect these packages to fail in a pod for the environment, and say so in the handoff instead of
  reading them as the branch's defects: `internal/daemon`, `internal/intake`, `internal/natsauth`,
  `internal/testnats` (testcontainers: `panic: rootless Docker not found`); `internal/admit`
  (`LEGION_TEST_PG_DSN is required`); `internal/config` (`python3: not found` in a
  `private_key_command` test); `internal/supervise`'s table tests (type-checking `net` needs cgo:
  `go tool cgo: exit status 2`, no C compiler in the image); `cmd/legion`'s
  `TestProbeImageWithPodSafetyProbesOnThePodsBaseline` (it reads the pod's own
  `/etc/legion-operator/overlay.yml`, which exists in a pod and not on a runner) and
  `TestThreadsResolveInTheReviewersPaneAsksTheDaemon` (it reads the pane's real `LEGION_GRANT_FILE`
  from the environment; run it with `env -u LEGION_GRANT_FILE -u LEGION_GRANT`);
  `internal/launcher`'s `TestAGenerationsCredentialsLiveExactlyAsLongAsItsChild` (timing-sensitive
  under gVisor: two different assertions failed in two runs).
- Run the packages the change touches by name (`go test ./internal/prompts/` for a prompt edit),
  and point at CI's `test` job (`.github/workflows/pr-and-main.yaml`) for the whole module; a
  branch that changes no Go source has nothing more to prove in the pod.
- A full `go test -count=1 ./...` costs about six minutes in the pod, most of it `cmd/legion`
  (300 s) and the first module download.

## Evidence (LEGION-630, sjawhar/legion#1845)

The branch changed two prompt markdown files under `packages/daemon/internal/prompts/roles` and no
Go source. Go 1.26.8 was provisioned per the recipe; `go test -count=1 ./...` with the recipe's
`env.sh` sourced failed in the ten packages above and passed everywhere else, `internal/prompts`
included. Re-running `internal/agentsecrets` with `GOWORK` unset passed (Go downloaded
`aws-sdk-go-v2/service/kms v1.61.1` through the workspace's view of `packages/envoy/go.sum`);
`TestThreadsResolveInTheReviewersPaneAsksTheDaemon` passed with the two grant variables unset. CI's
`test` job at b2364ace (run 37729581888) ran the module green.
