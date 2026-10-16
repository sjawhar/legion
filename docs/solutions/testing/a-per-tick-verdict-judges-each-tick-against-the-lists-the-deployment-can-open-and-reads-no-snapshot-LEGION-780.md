---
title: "A per-tick verdict judges each tick against the lists the deployment can open and reads no snapshot"
category: testing
tags:
  - live-proof
  - stage4b
  - controller
  - capabilities
  - jq
date: 2026-10-16
status: active
module: scripts/e2e/lib/controller-ticks.jq
related_issues:
  - "LEGION-780"
  - "sjawhar/legion#1982"
---

# A per-tick verdict judges each tick against the lists the deployment can open and reads no snapshot

Extends docs/solutions/testing/a-checkpoint-compares-live-state-to-a-comparand-read-at-the-assertion-and-waits-only-for-what-the-current-producer-can-deliver-LEGION-578.md.

- The residual that document records (one `daemon_state` read for a series of ticks holds only
  while no restart and no live-row change fall inside the window) is closed by judging each tick
  alone against the lists the deployment can open: `ticks_name_deployment_open`
  (`scripts/e2e/lib/controller-ticks.jq`) is true when there is at least one tick and each tick's
  `openCapabilities`, as a set, is `["secrets"]` or `["secrets","pool-capacity"]`. `ticks_name_open`
  and the `daemon_open_rows` read it compared against are gone.
- Write the arms from what the deployment can leave open, not from a read: `secrets` because the
  proof's `legion.yaml` configures no `agent_secrets`, `pool-capacity` because each `legion start`
  re-probes the image and may find the pool cold. A session whose ticks carry both arms passes
  (the pool scaled at one start and was warm at the next); a tick naming anything else, a live row
  included, fails with the tick quoted, which is the stricter reading the checkpoint's window
  (before the first checkpoint that opens a live row) intends.
- Prove the verdict through the real renderer rather than a hand-written card: `renderInbound`
  (`packages/envoy-client/src/delivery.ts`) over a tick envelope writes the `openCapabilities[1]:
  secrets` line the module reads, so a change to the TOON shape shows up in the proof.

## Evidence

sjawhar/legion#1982 (LEGION-780) replaced the snapshot comparison #1866 (LEGION-578) had added.
The unit cases pin both arms, either order, the restart case, `secrets` missing, a divergent name,
a live row (`web-search`) and one bad tick among good ones; the implementer's and tester's proofs
rendered ticks with `renderInbound` into a temp controller session and ran the head's lifted
`controller_ticks` over it, true for the two arms and false once a tick without a list was in,
under jq 1.6, 1.7.1 and 1.8.2. No Stage 4b run was requested for the change; the next operator-lane
run is its first live exercise.
