---
title: "A capability row reports what a worker can do, not what the image contains: tooling the image carries but no process loads is installed, never present, and a check nothing runs is pending, never passed"
category: daemon
tags:
  - capabilities
  - probe-image
  - worker-image
  - legion-state
  - codegraph
  - status-vocabulary
date: 2026-10-08
status: active
module: packages/daemon/internal/capabilities
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1846"
  - "LEGION-629"
---

# A capability row reports what a worker can do, not what the image contains

- A report whose reader asks "what can a worker do" must not answer "what does the image hold". A
  check that measures a file on disk — a binary on PATH, a plugin enabled in a lock — proves the
  image's inventory; whether a worker's agent can use it is decided by the launch of the process
  that would use it. Read that launch's argv (`runtime/sandbox/manifest.go` `agentArgv`, the lane
  `bootgate.go` probes) before choosing a row's status.
- Where the two diverge, give the row its own status rather than `present`: `installed`, with the
  evidence first and then one sentence naming what the worker still needs and the issue that
  delivers it (`Capability.Awaits`). Render it on every surface the row appears on — the probe's
  table line and `legion state --json`'s row — and keep the gate's refusal as it was: an image
  lacking the tooling is still `missing` and still fails the build.
- A row whose check nothing in the tree runs yet says so: `to be proved by a live check against a
  running pod (<issue>)`, never `checked live`. A site label is not a verdict.
- When the launch changes (the pod lane loads profile plugins), the `Awaits` sentence goes and the
  row reads `present`; that edit belongs to the change that alters the launch, not to the one that
  wrote the row.

## Evidence

sjawhar/legion#1846's capability table rendered `codegraph: present` from
`/opt/codegraph/bin/codegraph` on PATH and `@bopstack/pi-codegraph` enabled in the profile lock —
both true of the worker image, neither true of a worker: every pod's agent was launched
`--no-extensions --extension <envoy> --extension <legion>`, so the profile plugin never loaded and
no pod had the tool (the spec's own audit had measured this; LEGION-629 turns discovery on). Round
2's blocking review finding. The fix: `Capability.Awaits` on the CodeGraph row, a new status
`installed` on both surfaces (the zod enum and fixtures widened in place at the branch's own
contract number, which no release had shipped at the time — at a released number the widening
needs a bump; that number later moved to the one `DaemonAPIVersion` in
`packages/daemon/internal/api/version.go` declares, once `@sjawhar/pi-legion@8.3.0` shipped 15
without `capabilities`: `docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md`),
the live rows reworded as pending, and `docs/kubernetes.md`'s row saying the same. The image
build at the fixed head printed
`capability codegraph: installed (… on PATH; … enabled in …; a pod's agent gets the codegraph tool
once its launch loads profile plugins (dispatch://LEGION-629))` and still passed; the stub removed
from PATH in `cmd/legion/probe_image_test.go` still reads `missing` and exits 1.
