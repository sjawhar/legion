---
title: "Upgrading a live box to the interactive controller: the old headless pane probes alive, so the operator kills it once after the daemon restart, and the first interactive spawn starts fresh"
category: daemon
tags:
  - legion
  - controller
  - upgrade
  - runbook
  - process-identity
  - merge-queue
date: 2026-09-13
status: active
module: daemon
related_issues:
  - "LEGION-16"
  - "LEGION-27"
  - "LEGION-91"
  - "sjawhar/legion#961"
symptoms:
  - "after deploying the interactive controller, the daemon keeps the old headless controller as the claimed one and never opens the interactive pane"
  - "legion gh -- pr merge fails in the controller pane and the merge queue does nothing, with no error in the daemon log"
  - "a runbook said the old controller would be judged dead on the first probe; on production it was judged alive"
---

# Upgrading a live box to the interactive controller: the old headless pane probes alive

LEGION-16 turned the Legion controller from a headless `omp --mode rpc` process behind
`legion worker-shim` into an interactive OMP terminal session in the daemon's private tmux server,
and made it the merge queue. The code was right from the first review round; what was wrong, for
ten passes, was one paragraph of the upgrade runbook and the pull request body's deployment
order. This note records what the upgrade really does on a production box, how the mistake
happened, and the check that would have caught it on day one.

## What actually happens on the first boot after the upgrade

The daemon trusts a recorded pane only while it still runs the process the locator recorded:
the pane's root process id and that process's `/proc/<pid>/stat` start ticks (LEGION-27,
`verifyPaneProcess` in `runtime-tmux.ts`). The controller's liveness check, `controllerAlive` in
`processes.ts`, is a pure probe on that verdict: it returns true or false and mutates nothing.

Production's controller locator was written by a daemon that already had LEGION-27, so it carries
`panePid` and `paneStartTicks` beside the tmux ids. The v32 → v33 migration
(`migrateV32State`) strips only `socketPath`; the identity stays. On the first probe after the
upgrade the old headless controller — still alive in its pane — matches pid and start ticks, its
command line contains `omp`, `controllerAlive` returns true, and `ensureController` sees the
existing controller claim and returns. Nothing spawns the interactive controller. The headless,
contract-4 controller stays the claimed controller indefinitely; its bash calls are not wrapped by
the old plugin, so `legion gh -- pr merge` fails in it and **the merge queue is not serving**,
silently — nothing in the daemon log says so.

Only a locator with no recorded identity — a controller spawned by a daemon older than LEGION-27
— takes the path the runbook used to describe: `not-recorded-process`, logged with both
identities, judged dead, replaced on the next wake with the old window reaped by the orphan sweep.

## The order that works

1. Deploy the daemon at or after LEGION-54 and LEGION-16.
2. Install the `@sjawhar/pi-legion-envoy` release built from that commit into the active profile
   (daemon API contract 5; the daemon refuses every release at contract 1 through 4 by name).
3. Restart the daemon.
4. Kill the surviving headless controller pane **once, after the restart** — the new daemon's boot
   log names it: `controller locator carried a headless shim socket (pane <id>); after this boot
   run: tmux -L legion-<project> kill-pane -t <id> so the interactive controller spawns (LEGION-16
   upgrade step 4)` (logged once by the v32 -> v33 migration hook; the daemon never runs it):
   `tmux -L legion-<project> kill-pane -t <controllerLocator.tmuxPaneId>`, the pane id from
   `legion state --json`.

Killing before the restart is wrong the other way round: the old daemon finds its controller gone
and respawns another headless one. After step 4 the next controller wake (a `no_holder`
exception, a pending notice, or the registration deadline) probes the pane dead and
`spawnController` opens the interactive pane. That first interactive spawn **starts fresh**: a
headless-era `/controller/ready` never carried an `ompSessionFile`, so
`spawnController(this.deps.state.controllerLocator?.ompSessionFile)` has nothing to resume and
logs so. Every later respawn resumes the file the interactive pane reports.

The pure-probe / single-writer split is why the daemon cannot self-heal this: the probe never
mutates, and the one writer (`spawnController`) only runs once something judged the pane dead.
That split is the right design — it is what lets a dead pane's recorded transcript survive until
the replacement pane exists — but it means a live process the daemon should no longer want is
kept, and a human has to remove it. A daemon that logged "controller locator has no
`ompSessionFile` and the plugin contract just changed" at boot would turn this silent state into a
loud one; nothing does today.

## How the runbook got it wrong, and the check that finds this class

The runbook described the code path the unit tests exercised — identity-less legacy locators,
which is what every fixture from before LEGION-27 looked like — and nobody read the production
state file. The reviewer did, in round five: `controllerLocator` at version 28 with
`panePid: 1680480`, `paneStartTicks: 165754263`; `tmux -L legion-<project> list-panes -a`
showing that pid on the recorded pane; `/proc/<pid>/stat` field 22 equal to the recorded ticks.
Three reads, no code run, and the paragraph was disproved.

The transferable rule: **an upgrade runbook is a claim about a real state file, so check it
against the production state file, not against fixtures.** For any change that alters what the
daemon does with a recorded process, read `<state_dir>/state.json` (or its `.vN.bak` twin — see
`../testing/live-proof-over-real-daemon-state-snapshots.md`) and walk the code with that record,
not with the shape the tests construct.

## The same mistake in a smaller form: asserting an environment nobody observed

Five sentences in the same tree said the controller pane (and every worker pane) has
`<state_dir>/worker-bin` first on `PATH`, because the daemon renders that `-e PATH=` pair. It
does, and tmux drops it at pane creation (LEGION-91, the tester's finding); `GH_CONFIG_DIR`,
`LEGION_GRANT_FILE`, and the emptied token variables arrive, `PATH` is the daemon's own. `legion
gh` works regardless because `legion` resolves through `<state_dir>/bin`, which every pane inherits
from the daemon. The check is one read on a real pane: `tr '\0' '\n' < /proc/<pane pid>/environ`.
A sentence about a pane's environment that no one has read off a pane is a guess, however
faithfully it restates the daemon's code.

## What did not change, and one thing to know about a stuck pane

The registration deadline still kills a live-but-unclaimed controller pane directly
(`retireAndRespawnStuckController`, `skipGraceful: true`): the pane has no shim socket to ask, and
a pane that never reached `/controller/ready` has no conversation to interrupt. A takeover session
(a hand-started `/legion-claim-controller`) caches the controller secret it started with; after the
daemon respawns its own pane the secret rotates and the takeover's bash calls fail with 403, and
the role does not follow `/new` or `/fork` in a takeover — `skills/legion-controller/SKILL.md`
carries both caveats.

## Related

- `packages/daemon/src/daemon/AGENTS.md`, "Upgrading a live box to the interactive controller":
  the four steps as the current runbook.
- `../legion/controller-gate-2-required-checks-live-reads.md`: the merge queue's required-checks
  gate, verified live on this repository and the smoke sandbox.
- `../legion/schema-bump-branch-rechecks-mains-version-at-every-rebase.md` and
  `../legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md`:
  why this branch's numbers moved with every rebase, landing at state v33 and contract 5.
