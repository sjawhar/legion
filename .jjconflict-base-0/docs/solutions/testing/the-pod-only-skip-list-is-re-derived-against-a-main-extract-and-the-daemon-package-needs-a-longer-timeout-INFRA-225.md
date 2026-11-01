---
title: "The pod-only skip list is re-derived against a main extract, and the daemon package needs a longer timeout"
category: testing
tags:
  - worker-pod
  - gvisor
  - go-test
  - skip-list
  - nats
date: 2026-10-26
status: active
module: packages/daemon
related_issues:
  - "INFRA-225"
  - "sjawhar/legion#2188"
---

# The pod-only skip list is re-derived against a main extract, and the daemon package needs a longer timeout

Extends docs/solutions/testing/the-daemons-nats-tests-run-in-a-docker-less-pod-against-a-local-nats-server-named-by-two-variables-INFRA-388.md.

- Do not copy a skip list. Run the package, and for each failure run the same tests on an
  extract of the base (`git --git-dir=<clone>/.git archive <base sha> | tar -x -C $TMPDIR/main-tree`,
  no new workspace). A test that fails the same way there is the pod's; say so in the handoff.
  A Docker panic (`rootless Docker not found`) aborts the whole package run, so skip those tests
  first or every later test goes unrun.
- `TestChooseNATSConnectionNamesTheUserTheDaemonConnectsAs` and `TestNoLaunchCarriesTheDaemonSeed`
  pass without Docker; skip only the `natsseed_test.go` tests that start a container.
- Give `internal/daemon` `-timeout 25m`: it ran 459–853 s here and has hit Go's 10-minute default
  under load.
- Run one NATS-backed `go test` process at a time per server, the daemon package included: a
  second process's reset deletes the first one's streams (`stream not found` in `intake`).

## Evidence

At 135d22e0, in this gVisor pod, five `internal/daemon` failures reproduced on the main extract:
the clone credential-helper test, the controller keeper retry, both terminal-attempt tests and
the NATS-seed container test. Three more tests start a NATS container and panic.
