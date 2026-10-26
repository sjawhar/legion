---
title: "The race detector runs in a worker pod with zig as the C compiler, and a library callback queued by an earlier test writes your log sink"
category: testing
tags:
  - race-detector
  - worker-pod
  - cgo
  - zig
  - slog
  - bytes-buffer
  - nats
date: 2026-10-25
status: active
module: packages/envoy
related_issues:
  - "INFRA-182"
  - "sjawhar/legion#2170"
---

# The race detector runs in a worker pod with zig as the C compiler, and a library callback queued by an earlier test writes your log sink

**Superseded by INFRA-257:** the worker image carries `gcc` and `libc6-dev`, with cgo off by
default (`ENV CGO_ENABLED=0` and a `CGO_ENABLED=0` line in `/opt/go/go.env`), so a role runs
`CGO_ENABLED=1 go test -race …` with no `CC`, zig or linker override. The zig recipe below applies
only to a pod on an older pinned image digest, one without `gcc`. The canary, the reproducer and the
stray-writer advice stand as written.

Extends docs/solutions/testing/a-held-clock-test-of-timers-armed-from-postgres-times-holds-now-at-microseconds-and-a-first-race-run-indicts-old-log-sinks-LEGION-694.md.

That page's third bullet ("a worker pod has no cgo, so `-race` runs nowhere but CI") is wrong, and
so is the claim in
docs/solutions/testing/go-test-in-a-worker-pod-leaves-gowork-to-go-work-and-names-the-packages-the-pod-cannot-run-LEGION-630.md
that a pod has no C compiler. A pod can fetch one.

- **Run `-race` in the pod before you push a concurrency or log-capture change.** Unpack zig into
  `$TMPDIR`: the image has no `xz`, so use Python's `lzma` and `tarfile`. Then run:

  ```bash
  export CGO_ENABLED=1 CC="$TMPDIR/zig/zig cc" CXX="$TMPDIR/zig/zig c++" \
    GOCACHE=$TMPDIR/gocache-race ZIG_GLOBAL_CACHE_DIR=$TMPDIR/zigcache ZIG_LOCAL_CACHE_DIR=$TMPDIR/zigcache
  go test -race -ldflags=-linkmode=external ./internal/<pkg>/ -run '<tests>'
  ```

  `-linkmode=external` is required. With Go's internal linker, the race runtime fails with
  `relocation target __popcountdi2 not defined`, and zig's linker supplies that symbol. Before you
  trust a clean run, first prove the detector reports a race: run a two-goroutine program that
  writes one variable and check that it prints `WARNING: DATA RACE`.
  On an image with `gcc` (INFRA-257 onward), drop zig, `CC`, `CXX` and `-ldflags=-linkmode=external`:
  `CGO_ENABLED=1 go test -race ./internal/<pkg>/ -run '<tests>'`. The canary still comes first.
- **Prove a race fix with a throwaway reproducer that fails on the old code.** A clean `-race` run
  of the real tests proves little, because the race needs a writer at the moment of the read. Write
  a throwaway test where a goroutine logs through `slog.Default()` while the test reads the capture
  for a fixed time (500 ms was enough). Run it once against the old pattern, where it must report
  the race, and once against the fix, where it must pass. Then delete it.
- **The stray writer need not be your code's goroutine.** In a package that opens NATS clients,
  nats.go's `close()` queues `DisconnectedErrCB` and `ClosedCB` on its async dispatcher and returns
  without waiting for them (nats.go v1.50.0, nats.go:5892-5910). A bus client an earlier test closed
  then writes `envoy nats connection closed` into whatever sink the default logger points at. So
  joining your own goroutines before you read does not save a bare `bytes.Buffer`.
  - Write the capture through one mutex-guarded writer and read only its guarded snapshot.
  - Keep only your own client's records (filter on the `name` attribute), or an exact count or an
    exact list of records breaks on that stray line.
  - Keep one such helper per package. Do not copy a sibling test's capture without checking that it
    is guarded.

## Evidence

INFRA-182 (sjawhar/legion#2170), round 2 of review. A new in-package test in
`packages/envoy/internal/bus/log_url_test.go` copied the unguarded capture of
`TestAConsumerNotActiveReportIsAWarning`. The reviewer showed the race against `envoy-go-race`. The
fix made `capturedLogs` in `internal/bus/stream_config_test.go` the package's one capture: a guarded
`Write` and `String`, `records`, and `recordsWithMsg`. It also took in a third unguarded capture,
that file's old `captureLogs`, which review had not named. In the pod, with zig 0.13.0 as the C
compiler, the throwaway reproducer of the old pattern printed `WARNING: DATA RACE` five times and
failed, and through `capturedLogs` it passed. The real non-container tests passed under
`-race -count=3`. The tester repeated the run with four writers and found 20 races against the old
test files and none at the fix.
