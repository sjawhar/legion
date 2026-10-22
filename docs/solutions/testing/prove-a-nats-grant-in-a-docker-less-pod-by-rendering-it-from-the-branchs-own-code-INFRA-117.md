---
title: "Prove a NATS grant in a Docker-less pod: render the authorization from the branch's own code, run the real binary, and build both negative controls from the base commit"
category: testing
tags:
  - nats
  - permissions
  - live-proof
  - worker-pod
  - negative-control
date: 2026-10-22
status: active
module: envoy-infra
related_issues:
  - "INFRA-117"
  - "sjawhar/legion#2104"
---

# Prove a NATS grant in a Docker-less pod

A worker pod runs no Docker, so every suite built on `internal/testnats` (testcontainers) is
CI's. That is not a reason to complete a phase without a live proof of a permission change: a
real `nats-server` and the real binary are both reachable from the pod.

- **Install the server version the deployment runs**, not the newest:
  `GOBIN=/tmp/natsbin go install github.com/nats-io/nats-server/v2@v2.15.1` (about 45 s from a warm
  module cache; the recipe and its `mise`/`GOBIN` caveat on a devbox are in
  `docs/solutions/testing/a-nats-stream-deleted-twice-in-quick-succession-comes-back-with-its-messages.md`).
  Record the version beside the image the deployment pins.
- **Render the grant from the branch's own code; never type it.** A hand-typed `authorization`
  block proves the server's behaviour, not the branch's. Build the config with the same
  constructors the deployment uses (`core.ListenerPermissions(machine)`, `core.SessionPermissions()`,
  `core.Authorization{…}.Render(nil)`) and the process environment with the same one the component
  renders (`core.ListenerConfig{…}.Env([]string{uri})`), from a throwaway `package main` inside
  the module so the imports resolve. Print the rendered allow list in the run: its entry count is
  the cheapest before/after number there is.
- **Run the real binary against it** (`go build -o /tmp/listener ./cmd/listener`, started with
  that environment plus the nkey seed) and drive it through the surface a user reaches — HTTP
  here — with a real client subscribed on the other side.
- **Both negative controls come from the base commit, built, not described.** Extract the base
  tree once outside the workspace (`git -C "$LEGION_WORKSPACE" archive <base sha> | tar -x -C
  /tmp/rig/base` — a read, no jj mutation, and no second workspace), then build *the same rig
  program* there: it now renders the pre-change grant from the pre-change code, and
  `go build ./cmd/listener` there gives the pre-change binary. Crossing the two (old grant + new
  binary, new grant + old binary) separates the two halves of a two-part fix, which one control
  cannot.
- **Delete the rig before the phase completes** and check `jj status` is clean: a `go` command
  also rewrites `go.work.sum`, which `jj restore` puts back.

## Evidence

INFRA-117 widened the Envoy hub's listener grant to `notifications.>` and routed uncaptured
notification topics over core NATS. Proof at head `7424e75ad6a1`, in the pod:

- New grant (41 publish entries, headed by `notifications.>`) + new listener:
  `POST /v1/messages/publish {"topic":"notifications.lxspec.probe.all"}` → `200 OK in 8ms`, the
  subscribed session received 319 bytes, and the server logged no violation.
- Old grant (50 entries, rendered by the base commit's own copy of the constructor) + new
  listener → `500 in 4ms`, `Publish Violation - Subject "notifications.lxspec.probe.all"` in the
  server log, subscriber silent for 10 s.
- New grant + listener built from the base commit → `500 … context deadline exceeded` after
  **5.003 s**, which is the reported bug reproduced.

CI's `envoy-infra-go` natslive step (`go test -tags natslive ./core/ ./seed/`) was green at the
same head, so the rig and the repository's own harness agreed. The tester reproduced all three
runs from a driver of its own (curl and a `nats` client instead of the Go program), which is what
made the implementer's proof verifiable after the rig was deleted.
