---
title: "A contract bump's sweep includes test expectations spelling the bare number; replace them with the package's derived constant"
category: legion
tags:
  - daemon-api-version
  - contract-bump
  - tests
  - worker-pod
date: 2026-10-16
status: active
module: packages/daemon/cmd/legion, packages/daemon/internal/daemon
related_issues:
  - "LEGION-636"
  - "sjawhar/legion#1984"
---

# A contract bump's sweep includes test expectations spelling the bare number; replace them with the package's derived constant

Extends docs/solutions/legion/a-contract-numbers-prose-is-declaration-history-or-current-write-current-as-a-pointer-and-sweep-docs-solutions-too-LEGION-578.md.

- That document's sweep regex finds prose mentions; a test's expected output is a "current"
  mention it does not match: `PluginContract: 24` in a fake daemon's `supervise.Deps`, a golden
  stdout line `… architect LEGION-208 suspended 1 24\n`, a sentence `this daemon requires 24`.
  Sweep the bare number under tests too before the bump is built:
  `grep -rn '\b<old>\b' packages/daemon --include='*_test.go'`.
- Replace each hit with the constant, not the new literal: `api.DaemonAPIVersion` where an int is
  wanted, and the package's existing derived string where the expectation is text
  (`cmd/legion/probe_image_test.go` already has
  `var thisBinarysContract = strconv.Itoa(api.DaemonAPIVersion)`; `fmt.Sprintf("… requires %d …",
  api.DaemonAPIVersion)` in `internal/daemon`). The next bump then changes nothing there.
- The packages that hold these tests are the ones a worker pod cannot run (`internal/daemon`
  needs Docker for NATS; `cmd/legion`'s claims tests ran only when the package was run whole),
  so a filtered local `go test` never sees them and CI does. The grep is the local check.

## Evidence (LEGION-636, sjawhar/legion#1984)

The 24 → 25 bump touched `version.go`, the fixtures, the TypeScript mirror and the plugin's pin
together, and the Tests job still went red at 4035e7c0 and 53cb8bdf: `cmd/legion/claims_test.go`
expected `suspended 1 24` and `ready 1 24` with `PluginContract: 24` in its deps, and
`internal/daemon/plugin_test.go` expected `this daemon requires 24` twice. Each was found by CI
only, since the pod's filtered runs skip those tests; each was fixed by the constant.
