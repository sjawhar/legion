---
title: "A no-op refactor's oracle is the fork-point binary's output diffed in the worker pod, and a derived set is asserted exactly"
category: testing
tags:
  - refactor
  - proof
  - probe-image
  - pack-plugin
  - fork-point
  - worker-pod
  - capability-report
date: 2026-10-16
status: active
module: packages/daemon/cmd/legion, packages/daemon/internal/capabilities
related_issues:
  - "LEGION-636"
  - "sjawhar/legion#1984"
---

# A no-op refactor's oracle is the fork-point binary's output diffed in the worker pod, and a derived set is asserted exactly

- When the acceptance is "unchanged in meaning" or "byte-identical", the oracle is a binary built
  at the fork point running the same command on the same input, and `diff` of the two outputs.
  Build it without touching the workspace:

  ```bash
  git -C "$LEGION_WORKSPACE" archive --format=tar <fork point> go.work go.work.sum packages/daemon packages/envoy | tar -x -C "$(mktemp -d)"
  (cd <that dir>/packages/daemon && go build -o /tmp/legion-main ./cmd/legion)
  ```

  Run the branch's and the fork point's `legion probe-image` on the pod's own tree and diff the
  `probe-image: capability` lines; run both `--check-config`s on the same scratch file and diff
  the output. An empty diff is the proof; the before/after display of a `jj diff` is not.
- A daemon API bump moves the probe to a contract the pod's installed plugin does not declare, so
  the branch's binary refuses `/opt/legion/pi-legion`. Pack the branch's own plugin as the release
  does and point the probe at it — no Docker needed:

  ```bash
  tarball=$(bash scripts/e2e/lib/pack-plugin.sh pi-legion <out dir>)
  tar -xzf "$tarball" -C <unpack dir>     # <unpack dir>/package holds package.json and dist/
  LEGION_OMP_PATH=/opt/omp/bin/omp /tmp/legion probe-image --plugin-root <unpack dir>/package \
    --envoy-plugin-root /opt/legion/pi-envoy --skip-agent-models
  ```

  The OK line then ends `daemon-api-version=<new>`; the table is comparable with the fork
  point's run on `/opt/legion/pi-legion` because the capability rows do not depend on the plugin.
- Inject a fault at the surface with `--omp <wrapper>`: a `#!/bin/sh` that `exec`s
  `/opt/omp/bin/omp "$@"` except for the argv it fails (`case "$*" in *"config get
  retry.modelFallback"*) exit 1;; esac`). The fork-point binary under the same wrapper is the
  negative control that shows the behaviour the change removed.
- A worker pod's `/tmp` does not survive its process's relaunch into a new pod generation. Keep
  every proof fixture reproducible from the commands above (and a scratch `legion.yaml` from
  `deploy/kubernetes/daemon/legion.yaml.example` with its named files generated), and record those
  commands in the handoff, so a later round rebuilds them in a minute instead of re-deriving them.
- When a refactor replaces an explicit list with a derived predicate (a name list becoming
  `state.Source == SourceConfiguration`), the set it yields is an emergent property of every
  branch that feeds the predicate. Pin it with a table over every (row × runtime × state) shape
  asserting the exact slice (`slices.Equal(names, want)`), never membership, and assert the
  excluded members' reason too (every open row left out carries the other source). Give every
  surface that must not carry the new field a negative (`has("source") == 0` in the e2e jq).
- A test of a skip builds the positive twin beside it: `lookpath.On("relative/bin", "tool")`
  answering nothing proves "skipped as relative" only when `relative/bin/tool` exists under the
  test's `t.Chdir` and the same directory's absolute path answers the file. A rewritten text
  (a log tail cut to its last 64 KiB) is driven through its real readers — the `(?m)^`-anchored
  OK-line matchers — not a `strings.Contains`, since a marker glued onto a line is what those
  readers miss.

## Evidence (LEGION-636, sjawhar/legion#1984)

A 48-file refactor of the capability check (one row type and one `Table` walk for the probe and
the daemon, `OpenFromConfiguration` from a name list to a `Source` predicate, contract 24 → 25)
passed review with no behaviour finding. What held it: the 20-row table of the branch's
`probe-image` at contract 25 (the plugin packed by `scripts/e2e/lib/pack-plugin.sh`) diffed empty
against a fork-point binary's run at every head, re-run after the simplify pass and after each
review round; `TestOpenFromConfigurationNamesTheGapsTheFileAloneDecides` as a table over the
deployment rows asserting the exact set and each `Source`; Stage 1's jq asserting `source` on
exactly the four deployment rows. The `--omp` wrapper gave `model-fallback=unread` with exit 0
where the fork-point binary refused. After the implementer's process died mid-phase, the pod's
`/tmp` was empty: the fork-point binary, the packed plugin, the wrapper and the scratch
configuration were rebuilt from the commands above. The reviewer's two hardening findings were
the skip test proving "absent" rather than "skipped" and the tail's `…` glued onto its first line.
