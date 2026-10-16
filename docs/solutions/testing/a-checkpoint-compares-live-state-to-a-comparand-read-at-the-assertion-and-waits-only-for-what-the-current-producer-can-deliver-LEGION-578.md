---
title: "A checkpoint compares live state to a comparand read at the assertion, and waits only for what the current producer can deliver"
category: testing
tags:
  - live-proof
  - stage4b
  - capability-report
  - wait-predicate
  - daemon-restart
date: 2026-10-16
status: active
module: scripts/e2e/stage4b-sandbox-tree.sh
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1866"
---

# A checkpoint compares live state to a comparand read at the assertion, and waits only for what the current producer can deliver

- A value recorded at an earlier checkpoint is that moment's, not the system's: when the producer
  restarts between the record and the assertion, read the comparand again at the assertion. Every
  `legion start` re-probes the image and `pool-capacity` is that probe's own verdict
  (`packages/daemon/internal/capabilities/deployment.go`); a tick carries the running daemon's
  `Open()` at enqueue (`packages/daemon/internal/admit/admit.go`). Three daemon restarts sit between
  Stage 4b's `boot` and `controller`, so the ticks are the fourth daemon's and `boot`'s open rows
  may not be. Keep the earlier record for the note and the fail line, and say in the fail line
  which comparand was used.
- Name the invariant a single read still rests on. One `daemon_state` read for a series of ticks
  equals "the daemon running when each tick was sent" only while no restart and no live-row change
  fall inside the series' window; a per-tick shape (each tick a subset of the rows ever open, with
  `secrets` in every one) would need no snapshot at all. Record the residual where the proof keeps
  its risks.
- Aim a wait predicate at what the current producer can deliver, and pin that premise with a unit
  test over a fixture of the producer's real wire shape, lifted from the script by name. Under
  LEGION-663 the six live capability rows render `unchecked` until a session reports, and `boot`
  starts an empty Postgres, so a wait for "no row `unchecked`" can never return: `boot` waits on
  the seven image rows alone, and `lib/stage4b-boot-capabilities.test.ts` runs `capabilities_probed`
  over `packages/contracts/fixtures/daemon-api/state.json` (every live row `unchecked`, every image
  row `present`), asserting 0 there and 1 with one image row `unchecked`.
- When a comparison moves out of inline bash into a jq module, put the module's arms in a unit:
  both branches the live run may take (the pool warm at boot; a cold pool healed at a later start),
  order-insensitivity, one divergent element, the required member absent, the empty series.

## Evidence

sjawhar/legion#1866 (LEGION-578): review round 1 (thread PRRT_kwDORFy7ds6tAG7V) found `controller`
comparing every tick to `$open_rows` from `boot`, read from the first daemon, where
`restart-mid-tree`, `address-moved-env-same-pod` and `address-moved-stream-new-pod` each run a full
`legion start` before the controller registers: a run that scaled the pool at boot (open
`["secrets","pool-capacity"]`) and found it warm at the restarts (ticks `["secrets"]`) would fail
~3.7 h in with the daemon behaving correctly. The run of record had `pool-capacity` present at boot,
so neither divergence was exercised. The fix (d25908b1) reads `daemon_open_rows` at the assertion
and compares through `lib/controller-ticks.jq`'s `ticks_name_open`; `lib/controller-ticks.test.ts`
pins the healed arm (ticks `["secrets"]`: true against `["secrets"]`, false against boot's two rows).
The architect ruled the change a narrower re-proof rather than a sixth live run: the delta changed
how the checkpoint judges, not what it drives. The reviewer named the one-snapshot residual as the
fast-follow. Earlier in the round, the planner's gap analysis found `boot`'s original wait
(`capabilities_probed`: no row `unchecked`) could never return under LEGION-663's rendering; the
re-aim and its fixture unit landed at the forward merge (ae31c9de) and `CHECK boot: PASS` read
`every live row unchecked, awaiting the first session's report`.
