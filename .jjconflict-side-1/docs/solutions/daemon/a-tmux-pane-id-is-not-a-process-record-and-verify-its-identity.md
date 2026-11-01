---
title: "A tmux pane id is not a process: record pid + /proc start ticks at launch and verify them at every probe, kill, and window reuse"
category: daemon
tags:
  - tmux
  - pane-id
  - process-identity
  - liveness-probe
  - runtime-tmux.ts
  - processes.ts
  - kill-pane
  - locator
date: 2026-09-13
status: active
module: packages/daemon
problem_type: correctness
severity: high
related_issues:
  - "LEGION-27"
  - "sjawhar/legion#981"
  - "LEGION-9"
  - "LEGION-16"
applies_when:
  - A daemon path reads, kills, or splits into a tmux pane it recorded earlier
  - The private tmux server can be recreated under a running daemon (operator kill-server, reboot)
  - A locator persisted by an older daemon has no process identity
---

# A tmux pane id is not a process: record pid + /proc start ticks at launch and verify them at every probe, kill, and window reuse

## The incident this exists for

When Legion's private tmux server is recreated under a running daemon, tmux hands out pane ids
from `%1` again. Every locator the daemon saved still names its old pane id, so each now points at
whatever pane got the same id. On 2026-09-12 the daemon believed LEGION-9's dead architect was
alive because its recorded `%1` had become the controller's pane (the probe asked "is there a live
pane at this id running OMP?" -- and the controller runs OMP); five dead LEGION-9 workers kept
counting as running and starved the worker queue; and a `kill-pane` against a stale id killed
LEGION-16's live architect.

## The contract

A pane id names a slot, not a process. Every locator `TmuxRuntime` writes (root, worker,
controller) records the pane's root pid as tmux reported it when the pane opened (`panePid`) and
that process's start time in clock ticks since boot -- field 22 of `/proc/<pid>/stat`
(`paneStartTicks`, `proc-stat.ts`). A pid alone can be reused; pid plus start time cannot name a
different process.

One chokepoint, `TmuxRuntime.verifyPaneProcess`, answers "is the process this locator recorded
still this pane's process": the pane's current pid must equal `panePid`, its stat start ticks
must equal `paneStartTicks`, then the OMP command-line check. Everything that trusts a pane goes
through it:

| path | on a pane that is not the recorded process |
| --- | --- |
| `probe` (resync tick, registration deadline, boot watchdog, exception lane) | `dead`, `reason: "not-recorded-process"`, both identities in `detail`; the caller logs once at its decision point |
| `stop` (tree close, worker retire, controller replacement, resurrection) | the kill is refused; the locator clears as if the pane were already gone |
| `probedWindowId` (where a new worker splits in) | the window is not reused; a fresh window opens |

A locator with no identity (persisted before the fields existed, or with no pane id at all) never
verifies. It is not repaired or backfilled: its first probe reports it dead and the root is
resurrected with `--resume` onto a fully recorded locator. This is deliberate -- an identity you
did not record at launch cannot be recovered later without trusting the very pane id the contract
distrusts.

## Three things that look like shortcuts and are not

**"Dead" has two consequences, not one -- keep them as separate flags.** A `gone` pane needs
nothing; a pane that is present but not ours may still hold *our* process somewhere else (a legacy
locator whose window is live) or a stranger's. So `stopProcess` takes `skipGraceful` and
`refuseKill` as independent options derived from the same verdict: the graceful `shutdown` frame
over the process's own role-scoped socket is skipped only for `gone`, while the destroy step is
refused for `not-recorded-process`. Collapsing them either lets a live legacy root run beside its
replacement (no graceful ask) or kills a bystander (no refusal). `controllerAlive` and
`resurrectDeadTree` hand the verdict they already took down to the stop, so it neither re-decides
nor re-logs it.

**A locator's window id is where its pane lives; never rewrite it while the pane may be live.** The
first attempt repointed live legacy locators to the fresh window a resurrection opened, so their
real window dropped out of the orphan sweep's known set and got `kill-window`ed. `probedWindowId`
now picks the first *recorded* window whose pane verifies (root locator, each worker claim, the
in-memory `issueWindows` entry, in that order) and otherwise opens a fresh one remembered in memory
only; each stale locator clears through its own probe.

**Identity comparisons are by content.** `/process/started` and `/worker/started` spread-copy the
locator into state, so an object-identity dedupe of the in-memory window entry is dead after
registration. Use `sameProcess` (pane id + pid + start ticks) -- `runtime.ts`.

## What the log line must carry

One line per decision, at the decision point, with both identities:
`treating LEGION-42's root as dead: pane %3 now runs pid 899077 (recorded pid 800726 start 163616468)`;
for a legacy record, `pane %2 has no recorded process identity (locator predates identity tracking)`.
Tests assert both identities appear (observed and `recorded pid N start M`) plus the `/proc` reads
that were made -- never the sentence around them.

## Proving it

The unit tests pin the contract; the proof that mattered ran on a real tmux server in an isolated
test environment with its own state dir, slug, ports and NATS container (never the deployment's
`legion-<project>` server), a `main` daemon's v-old state upgraded under live identity-less panes,
then `kill-server` and stranger `sleep` panes on the recorded ids. Every stranger pid was alive after
every transition, the decision lines named the stranger pids, and root and controller resurrected
with `--resume`. Evidence: `dispatch://LEGION-27/artifact/smoke-rig-evidence-md`.

## Related

- `tmux-list-panes-target-is-its-window.md` -- `list-panes -t <pane>` lists the whole window; select
  the row by `#{pane_id}`.
- `a-failed-list-panes-proves-nothing-about-the-pane.md` -- the third verdict the probe needs.
- `../testing/gated-e2e-suites-catch-real-pane-fixtures-without-identity.md` -- the fixture rule the
  contract imposes on every test that seeds a locator.
