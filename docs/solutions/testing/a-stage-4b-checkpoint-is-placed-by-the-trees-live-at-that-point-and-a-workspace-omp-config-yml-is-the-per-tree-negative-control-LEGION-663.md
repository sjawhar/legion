---
title: "A Stage 4b checkpoint is placed by the trees live at that point, and a workspace .omp/config.yml is the per-tree negative control"
category: testing
tags:
  - stage4b
  - e2e
  - checkpoint
  - negative-control
  - omp-config
  - tree-lifecycle
  - controller
date: 2026-10-10
status: active
module: scripts/e2e
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---
# A Stage 4b checkpoint is placed by the trees live at that point, and a workspace .omp/config.yml is the per-tree negative control

- Before a plan or a change names where a new `stage4b-sandbox-tree.sh` checkpoint goes, read
  the script's tree lifecycle at that point — `grep -n '^begin ' scripts/e2e/stage4b-sandbox-tree.sh`,
  then each checkpoint's `take_out`, `set_status … backlog` and `end_claim_process` — and name
  the tree the checkpoint drives by what is live then, not by which checkpoint precedes it. A
  negative control that writes into a workspace needs a tree that never opens a pull request
  (nothing of it reaches the smoke main), live while the roles the predicate reads are live (a
  controller for a tick notice). A checkpoint that needs a window inside an existing checkpoint
  splits it at the `begin`/`pass` level (`controller` → `full-agent-negative` → `controller-walk`),
  each `begin` at column 0 for the `STAGE4B_UNTIL` validation, and the skip branch marks every new
  checkpoint `skipped`.
- A per-tree negative control for a capability Oh My Pi reads from settings is a workspace
  `.omp/config.yml` written through `pod_exec` into the tree's shared workspace (it survives a
  container restart; a container-local file does not), followed by one `end_claim_process … kill`
  so the relaunch reads it at boot. The checkpoint removes the file at its end, records the pod
  and path in globals the `cleanup()` trap removes best-effort on an abort, and the lib tests that
  run `cleanup()` under `set -u` (`stage4b-verdict.test.ts`, `smoke-cleanup.test.ts`) seed those
  globals empty. This is the deliberate inverse of
  `docs/solutions/legion/a-husk-file-the-daemon-wrote-check-the-writer-then-remove-the-write-not-the-symptom.md`:
  a test-authored `.omp/config.yml` with its own cleanup, not a stray one.
- A fixture "server" a checkpoint reads a marker from must speak the protocol the client expects
  once a check judges the connection: `sh -c 'touch marker && sleep 600'` never reaches
  `connected`; a 40-line bun stdio MCP server (`initialize` echoing `protocolVersion`, `ping`,
  `tools/list`, `tools/call`, notifications ignored) does, in under 0.5 s, and still writes the
  marker. Prove it against Oh My Pi's own client (`discoverMCPServers` from a probe extension)
  before the live run.

## Evidence

LEGION-663's plan placed `full-agent-negative` "after `controller`" on tree 2's planner. Tree 2 is
moved to backlog at `issue-cap-moves`, twenty checkpoints before a controller registers, and tree 1
opens the pull request that merges to the smoke main; tree 3, held between its hold and its
take-out from the operator shell, is the one live tree without a pull request while a controller
runs. The implementer split `controller` at the held notice, ran the negative control on tree 3's
planner, and moved the take-out and walk into `controller-walk` (`1add7813`); the lib tests that
extract `cleanup()` failed `negative_config_pod: unbound variable` until the harness seeded the two
globals (`891d3b64`). The MCP fixture became `.omp/mcp-fixture.ts`, connected under both names in
382 ms through Oh My Pi's client with both markers written.
