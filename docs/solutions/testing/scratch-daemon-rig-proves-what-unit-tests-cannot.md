---
title: "A scratch-daemon rig (own team, state dir, ports, tmux server, short window) proves a lifecycle change end to end when unit tests cannot"
category: testing
tags:
  - e2e
  - scratch-daemon
  - tmux
  - worker-lifecycle
  - idle-retire
  - tester
  - negative-control
date: 2026-09-12
status: active
module: daemon
related_issues:
  - "LEGION-30"
  - "sjawhar/legion#973"
---

# A scratch-daemon rig (own team, state dir, ports, tmux server, short window) proves a lifecycle change end to end when unit tests cannot

## Context

LEGION-30 changed when a phase worker's OMP process is alive. Its acceptance criterion 6 asked
for a live before/after count on the LEGION deployment — which coordinates the very tree doing the
work, and whose restart is an operator action. Spec version 5 split the criterion: a pre-merge half
on a throwaway daemon started from the PR head on the same machine, and a live half after the
post-merge restart. The tester ran the throwaway rig twice (before and after the corrective round)
and each time it decided something the unit suite could not.

## What the rig is

A second Legion daemon that shares nothing with the live one except the machine:

| knob | rig value | why it matters |
| --- | --- | --- |
| team / project | `sjawhar/3030` → slug `sjawhar3030` | separate `legion legions` entry, separate role tokens |
| `state_dir` | `/tmp/legion30-rig/daemon` | its own `state.json`, secrets, workspaces, sockets |
| `port` / `worker_stream_port` | 19570 / 19571 | the live daemon holds 13370/13371 |
| tmux | `tmux -L legion-sjawhar3030` | the private server name is the project slug; the operator idle sweep (`retire-idle-workers.py`) only kills panes it finds on the *live* server's socket, so the rig's panes are invisible to it |
| NATS + Envoy listener | isolated NATS on 14322, listener built from the PR head on 19120, `scripts/smoke/envoy-bridge.ts` for Dispatch events | no role-lane traffic reaches the live listener |
| Dispatch project | `LEGSMOKE`, one root issue | real issue events, nothing in the production project |
| the knob under test | `worker_idle_retire_seconds: 90` | a 600 s default makes a proof take an hour; 90 s keeps the whole run under 20 minutes |

Everything on the retire/resume path was real: daemon-spawned `omp --mode rpc` workers behind
`legion worker-shim`, the real boot handshake, a real model turn per worker, the real `shutdown`
frame, the real `--resume=<session file>` relaunch. The only inert part was the Legion extension
in the root-architect and controller panes (a launch-prefix override for those two panes only),
with the tester driving the daemon's HTTP API as the extension would.

## What it proved, twice

Round 1 (head 13468c96): four real workers finished a turn; three whose role was not the active
phase were retired at exactly the 90 s window with one `[legion] retiring idle worker …` line each;
rig-scoped `omp --mode rpc` count 12 → 6 (each pane is a shim row plus an omp child row, so count
panes as rows / 2); the retired planner was resumed by `spawn_worker` with `--resume=<its file>` and
answered in the same session; the negative control (the reviewer, still the active phase) stayed
resident past the window; the root architect and controller stayed resident throughout.

Then the rig found the defect eleven unit tests had missed: after the planner's resume rewrote
`phases[issue]`, the reviewer — now retirable on every criterion — was still resident 5.7 minutes
later with no clock pending. Nothing in the unit fixtures had suggested `phases[issue]` could move
without the worker's own involvement; on the rig it moved the natural way. The fix (a self-re-
arming expiry) and its unit twin came out of that observation. See
`docs/solutions/daemon/idle-expiry-timers-rearm-on-declines-that-change-while-idle.md`.

Round 3 (head 9932d084, after the rebase onto the `Runtime` boundary): the same orderings through
the real `TmuxRuntime`, including the phase-moves-after-expiry ordering, now retiring within one
further window.

## Why this and not a bigger unit test

- The leak was an *ordering across real events* (`agent_end` → expiry → another role's
  `/worker/started`), not a state. The unit fixture that reproduces it is five lines once you know
  the ordering; the rig is how you learn the ordering exists.
- `pgrep`/`ps` counts and the daemon's own log lines are the user-observable surface the criterion
  names. A passing `processes.test.ts` is a regression lock, not proof of a criterion — the
  legion-worker skill says so, and this issue is the case for it.
- Each pane costs ~240 MB, so "did the count drop" is also "did the box get its memory back".

## Reusable procedure

1. Pick a scratch team whose slug collides with nothing (`legion legions` lists the live ones).
   Give it its own `state_dir`, ports, NATS, listener, Dispatch project; set the knob under test to
   a short value.
2. Record BEFORE: a listing of `omp --mode rpc` processes whose environ carries the rig's
   `LEGION_STATE_DIR` (pid, ppid, rss, age, role, issue), plus `tmux -L <slug> list-panes -a`.
3. Drive the scenario through the daemon's HTTP API (`/process/started`, `/process/ready`,
   `/worker/spawn`) or the extension; let real workers finish real turns.
4. Wait the window plus a margin; record AFTER the same way; capture the daemon log lines the
   criterion names; keep a **negative control** (something that must *not* change) in the listing.
5. Confirm the operator sweep could not have done it (its socket, its `MIN_AGE`): the proof must be
   attributable to the daemon's code.
6. Exercise the recovery half (here: `spawn_worker` → `--resume`) and quote the launch argv.
7. Tear down: `legion stop <team>`, `tmux -L <slug> kill-server`, stop the listener/bridge/NATS,
   `rm -rf` the state dir and the rig's OMP session dirs, close the scratch issue. The live daemon,
   state dir, and tmux server are never touched.
8. Put the listings, log lines, and launch line in the PR body's **E2E** section with the head SHA.

## Gotchas met on the way

- A resumed launch is `omp --resume=<file> --mode rpc`, so a `pgrep -f '/omp --mode rpc'`
  adjacency pattern misses resumed workers; count `--mode rpc` and `/omp` separately.
- `/process/ready` returns 500 when the architect role has no live Envoy holder at ready time — a
  driver that skips `envoy_role_set` (this rig) hits it; the extension never does.
- A `done` PATCH on the rig's root issue lingers the tree for `linger_hours` and leaves its panes
  resident until the sweep; tear the rig down with `kill-server` rather than waiting.
- The rig's daemon is `legion start` in a pane the tester owns; never `legion restart <live team>`
  from a worker pane — that binds a new live daemon to the pane that ran it.
