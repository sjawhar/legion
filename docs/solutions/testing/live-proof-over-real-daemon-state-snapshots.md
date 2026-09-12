---
title: "Live proof over real daemon state snapshots: copy the state file first, and pick the snapshot that actually carries the bug"
category: testing
tags:
  - live-proof
  - daemon-state
  - loadState
  - resync
  - state-snapshot
  - migration-backup
  - tester
date: 2026-09-12
status: active
module: packages/daemon
related_issues:
  - "LEGION-14"
  - "sjawhar/legion#952"
---

# Live Proof Over Real Daemon State Snapshots

## Context

LEGION-14 ("Resync reports every admission-queued todo root as zero-owner-tree") was a bug in
`runResync`, the Legion daemon's periodic self-check in `packages/daemon/src/daemon/resync.ts`.
The spec required proof on the real surface: run the fixed check over this machine's actual
daemon state and show that the roots waiting in the admission queue are no longer reported. The
planner, tester, and reviewer each ran that proof. Two things about it are worth keeping for the
next person who proves a daemon change against real state: the state loader writes a file as a
side effect, and the snapshot that shows the bug is usually not the current one.

## 1. `loadState` writes a backup beside any older-version input, so never load a live state file in place

`loadState` in `packages/daemon/src/daemon/legion-state.ts` migrates an older-schema file forward
and, when it does, writes the original bytes to `<input>.v<N>.bak` next to the input
(`legion-state.ts:895`, opened with the `wx` flag so an existing backup is left alone). That is
harmless for the daemon's own `state.json`. It is not harmless for a proof driver pointed at the
daemon's directory: loading `$LEGION_STATE_DIR/state.json.v22.bak` in place, on a daemon whose
current schema is v23, would create `state.json.v22.bak.v22.bak` inside the live daemon's state
directory. The tester confirmed the side effect is real: the `.v22.bak` files landed beside the
`/tmp` copies, and only there.

The rule, and the guard that enforces it:

- Copy every input under `/tmp` first (`cp "$LEGION_STATE_DIR/state.json.v22.bak" /tmp/<dir>/…`).
- The driver refuses any input path that is not under `/tmp/` before it reads anything
  (`if (!stateCopyPath.startsWith("/tmp/")) throw …`). Both the tester and the reviewer ran the
  negative control: pointing the driver at `$LEGION_STATE_DIR/state.json` exits 1 with
  `refusing to load …: state input must be a copy under /tmp` and writes no report.
- List the state directory before and after, record the sha256 of every backup file, and put the
  listing in the handoff. If the listing differs from what the plan predicted, say so as a
  deviation rather than adjusting the expectation; on this rig a fourth file
  (`state.json.bak-20260912T051244Z`) had appeared before the tester began, from an operator
  copy during a daemon outage, and the tester reported it instead of hiding it.

## 2. The snapshot that carries the bug is usually a migration backup, not `state.json`

The daemon's state changes constantly, so "run it over the current `state.json`" often proves
nothing. On LEGION-14, three snapshots of the same directory told three different stories over
about seven hours:

| snapshot | written | what the admission queue held | pre-fix anomalies |
| :--- | :--- | :--- | :--- |
| `state.json.v22.bak` | 2026-09-11 23:04Z, inside the controller's observed wake window | two `todo` roots, LEGION-10 and LEGION-11, tree status `queued` | `zero-owner-tree` for both: **the bug** |
| `state.json` at plan time | 2026-09-12 04:14Z | two queued entries, but both children of LEGION-19 (skipped by the root-only loop) | none |
| `state.json` at test time | 2026-09-12 05:50Z | nothing; the queued children had escalated to `launch-failed` | none |

Only the first exhibits the condition, and it exists only because a schema bump (v22 to v23)
happened to land inside the incident window. Two consequences for the plan:

- Before naming a proof input, open each candidate snapshot and check that it actually contains
  the condition (here: a root in `admission.queue` whose issue status is `todo` and whose tree
  status is `queued`). Name the snapshot that does. Record its write time against the incident's
  observed timeline so the reader can see it is from inside the window.
- The current `state.json` is still worth running, as a no-regression control: before and after
  must produce identical anomaly lists and identical effect counts. But call it a control. The
  plan told the tester: "if its queue holds no roots at run time, say so explicitly rather than
  presenting `[] -> []` as the proof". The tester did exactly that (`currentQueueAtCopyTime.note`
  in the test handoff), and the reviewer could tell proof from control at a glance.

## 3. Driver shape that worked

The full driver is quoted verbatim in the plan for this issue,
`docs/plans/2026-09-12-resync-queued-root-anomaly.md`, Task 2 Step 2. The parts that generalize:

- **One driver, two modules.** The module under test is selected at runtime from argv, so the same
  driver runs the pre-fix and the fixed `runResync`. The pre-fix module comes from history:
  `jj file show -r "<fix-change>-" packages/daemon/src/daemon/resync.ts`, with a `sed` pass that
  rewrites its relative imports (`../state/fetch`, `./config`, and so on) to absolute workspace
  paths so it runs from `/tmp`. The `import type` from `@legion/contracts` needs no rewrite because
  bun erases it. The fixed module is the workspace file itself; no copy, no sed.
- **Every side effect is a mock that either records or throws.** `saveState` and
  `fetchCiStatusBatch` are no-ops; `applyEffects` records into an array so effect counts can be
  compared before and after (here `probe 1 -> 1` on the incident snapshot and `probe 12 -> 12`,
  `controller 2 -> 2` on the current one); `dispatchClient.listIssues` returns `[]` so no drift is
  healed; `dispatchClient.getIssue` answers a pending status write with that write's own intended
  status, which makes `retryPendingWrite` resolve it in memory and never send a PATCH;
  `dispatchClient.setStatus` throws, because any call to it means the proof reached the network.
- **Run with the working directory set to `packages/daemon`.** bun started from `/tmp` stalls for
  about thirty seconds on module resolution; started from the package it resolves immediately.
- **Write the report to a file and end with `process.exit(0)`.** `runResync` itself logs to the
  console, so stdout is not a clean channel, and the imported daemon modules can hold handles that
  keep the process alive.
- **Include one negative control that flips the verdict.** The tester edited the incident copy so
  LEGION-10's tree status was `dead` and confirmed the fixed module then reports exactly
  `zero-owner-tree:LEGION-10` and nothing for the still-queued LEGION-11. Without that control,
  "zero anomalies after the fix" cannot be told apart from "the gate now skips everything".

## 4. Observed, not predicted

The planner ran the driver against the pre-fix code and against a scratch `/tmp` copy of
`resync.ts` with the exact planned edits applied, and wrote the observed lists into the plan's
expected-values table (`2 -> 0` on the incident snapshot, `0 -> 0` on the current one, the
negative control's single anomaly, unchanged probe counts). The implementer, tester, and reviewer
then each reproduced those numbers independently. A plan whose expected values were observed
before implementation costs the planner one extra hour and saves every later phase a
"why does my number differ from the plan" investigation.

## Related

- `docs/plans/2026-09-12-resync-queued-root-anomaly.md` Task 2: the driver, verbatim, and the
  step-by-step staging and negative-control commands.
- `docs/solutions/testing/race-regression-tests-that-fail-before-the-fix.md`: the unit-test half
  of the same discipline, proving the pre-fix failure mechanically and recording which assertion
  failed.
- `docs/solutions/legion/worker-pane-shell-gotchas.md`: the pane environment facts (the
  `env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE` prefix for `bun test`, the stacked-grant
  workaround) a tester needs before running any of the above.
