---
title: "Everything under /legion dies with the issue pod: keep the recipe durable and rebuild after a recreated workspace"
category: legion
tags:
  - issue-pod
  - harness
  - pod-replacement
  - ephemeral-volume
  - bun-pin
date: 2026-10-28
status: active
module: skills/legion-worker
supersedes:
  - docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md
  - docs/solutions/legion/a-roles-scratch-on-the-container-filesystem-is-what-an-issue-pod-is-replaced-over-INFRA-43.md
  - docs/solutions/testing/a-layout-change-is-proven-by-the-whole-project-and-the-rig-needs-contracts-built-and-three-engines-it-cannot-start-INFRA-92.md
  - docs/solutions/legion/the-issue-workspace-outlives-your-turn-INFRA-31.md
related_issues:
  - "INFRA-569"
  - "sjawhar/legion#2363"
---

# Everything under /legion dies with the issue pod: keep the recipe durable and rebuild after a recreated workspace

This supersedes one claim in each of the four files above: that `/legion` (the issue volume, its
harness, scratch and workspace) survives a pod replacement. Since INFRA-80 it does not. Everything
else in those files stands: build on `/legion` and not on the container filesystem, so the pod is
not evicted over ephemeral storage; use one idempotent setup script that every role in the pod
re-runs; put nothing under `$LEGION_WORKSPACE`; give each role its own lane and database.

- Treat everything under `/legion` as lasting only as long as the pod: a harness, scratch under
  `$TMPDIR`, fetched binaries, caches and any unpushed commit. It survives a role relaunched into
  the live pod, and nothing more. The issue pod's disk is a generic ephemeral volume, deleted with
  the pod (`docs/kubernetes.md`, "The disk dies with its pod"; `issuePod.disk` in
  `packages/daemon/internal/runtime/sandbox/podkind.go`). Only the pushed branch, its committed
  handoffs and the daemon's records reach the next pod.
- Keep the recipe, not only the result, where the next pod can read it. The setup script
  under `/legion/harness` dies with the harness. Rebuild from the steps of the testing documents
  above, or record your exact commands in your handoff.
- The signal for a replaced pod is the "Your workspace was recreated…" message
  (`LEGION_WORKSPACE_RECREATED`), not an intact workspace beside an empty `/tmp`. After it, check
  that the harness exists before you claim a run that needs it. When you skip one, say so in the
  handoff and the PR body. At a forward merge with an unchanged fingerprint, the bare gates are all
  that is owed.
- A pod runs the deployment's worker image, whose Bun is fixed when the image is built. A forward
  merge that raises `.bun-version` leaves the pod's `bun` behind, and
  `packages/claude-envoy/scripts/build.ts` refuses to build. Fetch the pinned release onto
  `/legion` and invoke it by path, not `bun run`: the release zip
  `https://github.com/oven-sh/bun/releases/download/bun-v<pin>/bun-linux-x64-baseline.zip`, unpacked
  and run as `<dir>/bun-linux-x64-baseline/bun scripts/build.ts`. The image has no `mise`, so
  `mise x bun@<pin>` does not work in a pod. The fetched binary dies with the pod too.

## Evidence

- INFRA-569's implementer built the Postgres 17 and Playwright harness at `/legion/harness`, per
  the INFRA-43 recipe, and kept proof scripts under `/legion/scratch/tmp/i569`. After a pod
  replacement on 28 October, both were gone: `ls: cannot access '/legion/harness/env.sh'`, and a
  redirect into `$TMPDIR/i569` failed with `No such file or directory`. The forward merge at
  169d0445 therefore ran only the bare gates. Its fingerprint was unchanged, and the handoff
  recorded that the database-backed Go suites were not rerun.
- At the same merge, main had raised `.bun-version` to 1.4.4 while the pod's `bun` was 1.3.14.
  The bundle rebuild refused with `refusing to build under Bun 1.3.14: .bun-version pins 1.4.4`
  until the 1.4.4 baseline zip was fetched onto `/legion` and its binary run by path.
