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
  - control-run
  - pi-envoy
date: 2026-09-12
status: active
module: daemon
related_issues:
  - "LEGION-30"
  - "sjawhar/legion#973"
  - "LEGION-73"
  - "sjawhar/legion#1032"
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

- The leak was an *ordering across real events* (`agent_end` → expiry → another role's phase
  write — at the time its `/worker/started`; since LEGION-37, #991, the delivery of the
  architect's assignment for it), not a state. The unit fixture that reproduces it is five lines
  once you know the ordering; the rig is how you learn the ordering exists.
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

## Proving a negative: a logging proxy in front of Dispatch

LEGION-59 (PR #998) needed the rig to prove the daemon did **not** do something — that an
implementer completion at `retro` sends no PATCH to Dispatch — and, in round 2, to settle a
design question about *who* writes a status. A daemon log line and a `GET` of the issue afterwards
are weak evidence of absence: a write can fail silently, land late, or come from another writer.
The tester's answer was a logging reverse proxy between the rig daemon and Dispatch:

- the rig daemon's `DISPATCH_URL` pointed at `http://127.0.0.1:19580`, a small forwarder to the
  real Dispatch (`http://sami-agents:8766`) that appends one line per request — timestamp, method,
  path, body — to a file;
- BEFORE/AFTER snapshots around each driven call recorded the proxy line count next to the daemon
  log line count, `state.issues[key].status`, and `pendingStatusWrites`;
- the whole proxy log for the run was quoted in the PR body's `E2E` section, every line
  attributed to its writer.

What it settled that nothing else could:

- **Absence, at request level.** For the completion at `retro`: `proxy log lines added: NONE`,
  Dispatch still `retro`, one daemon log line (the no-holder catch-up record). The negative control
  — the same completion from `in_progress` — added exactly one line, `PATCH … {"status":"testing"}`.
- **Attribution of a write nobody had traced.** The round-2 design assumed a child released to
  `todo` needed a spawn-time `in_progress` write. The proxy log showed `PATCH LEGSMOKE-98 todo` from
  `/waves/release` at 06:25:25 and `PATCH LEGSMOKE-98 in_progress` at 06:25:27 with **no spawn
  involved** — `spawnTree` had admitted the child as its own tree. One log, one look, and a
  spec row plus three documents were corrected
  (`../daemon/dispatch-status-writes-one-writer-per-transition-and-the-pending-write-fence.md`).

When a criterion is phrased as "does not call", "never PATCHes", or "writes nothing", put the
proxy in from the start and quote its complete log; a `GET` after the fact and a unit test's fake
client are the regression lock, not the proof.

## Proving a plugin-side race through a real architect pane

LEGION-73 (PR #1032) fixed a race inside the pi-envoy plugin: two `legion` tool calls in one
batch, right after a daemon restart, raced each other's session-secret recovery and one was
refused 403. The daemon was unchanged, so the rig's variable was **which plugin build the
architect pane loads** — a scratch OMP profile per build (`legion73pos` from the PR head,
`legion73neg` from the parent commit `c8faafec`), each a copy of the live profile's
`plugins/package.json` with `@sjawhar/pi-legion-envoy` pointed at a `file:` copy packed from that
commit. Confirm the variable from the pane's own OMP log line
`extension instance loaded … file:///…/pi-legion-envoy-<sha>/dist/legion.js`, never from the
profile name. Four things about that proof carry over to any restart-triggered race:

- **The racing action must be the first capability-bearing action after the event.** Every
  bash call in a Legion pane mints a grant through the `tool_call` hook, and that `/grants`
  request goes through the same recovering client — so a bash call before the batch (or a lone
  `legion` call) recovers the secret first and the batch never races. `read`, Dispatch, and Envoy
  tools never touch the daemon, but forbid them too: the prompt has to leave the model no step
  before the batch, name the exact two calls, and say "in ONE assistant message, two tool_use
  blocks". The transcript is the evidence: the first assistant message after the restart carries
  both `tool_use` blocks and no tool call precedes it.
- **A headless architect takes its prompt over the shim socket, not `send-keys`.** The root
  architect is `omp --mode rpc` behind `legion worker-shim`; there is no TUI to type into. The
  tester delivered the prompt as an RPC `prompt` frame with the daemon's own `connectWorkerRpc`
  client on the pane's unix socket — the same frame the daemon uses to hand a worker its
  assignment. (The plan had said `tmux send-keys`; it would have typed into nothing.)
- **The control run on the parent commit is what proves the batch raced.** The model may
  serialise two calls into two messages, or the restart may land such that no race is possible;
  a head-only success proves nothing about the race. The same choreography with the parent's
  plugin reproduced the incident's exact shape — batch A `POST /legion/v1/escalate failed with
  403: {"error":"Invalid session secret"}`, batch B `{}`, only B reaching the controller — and
  that reproduction is what gives the head run's two `{}` results their meaning. Repeat the
  restart-and-prompt once before calling a non-reproducing control inconclusive; read the
  transcript for a preceding tool call or serialised calls first.
- **A negative control must be chosen to pass the nearer layer.** The plan's negative control —
  a bogus escalation `kind` through the pane returns the daemon's `400 Unknown escalation kind` —
  never reached the daemon: the plugin's own tool-argument schema refused it first
  (`Validation failed for tool "legion": kind must be "re-file", "capacity", "cross-tree" …`).
  That is a refusal, but of the wrong component. When the surface under test has its own
  validation ahead of the one you mean to exercise, pick an input the nearer layer admits (here a
  wrong secret → the daemon's `403 Invalid session secret`), or drive the far layer directly
  (`curl` to the rig daemon got the 400) and say which layer produced each result. A negative
  control that shows *a* refusal without naming the refusing layer proves less than it looks.

`escalate` was the right probe for this rig: on a scratch team it only publishes a controller
notice (`handleEscalate`), changes no issue status, and a distinct `context` per call makes each
one's arrival at the controller's transcript countable. The pattern under test is in
[credential-recovery-is-shared-per-credential-not-per-request](../envoy/credential-recovery-is-shared-per-credential-not-per-request.md).

## Gotchas met on the way

- A resumed launch is `omp --resume=<file> --mode rpc`, so a `pgrep -f '/omp --mode rpc'`
  adjacency pattern misses resumed workers; count `--mode rpc` and `/omp` separately.
- `/process/ready` returns 500 when the architect role has no live Envoy holder at ready time — a
  driver that skips `envoy_role_set` (this rig) hits it; the extension never does.
- A `done` PATCH on the rig's root issue lingers the tree for `linger_hours` and leaves its panes
  resident until the sweep; tear the rig down with `kill-server` rather than waiting.
- The rig's daemon is `legion start` in a pane the tester owns; never `legion restart <live team>`
  from a worker pane — that binds a new live daemon to the pane that ran it.
- `GET /legion/v1/state` is redacted (`api/state.ts`) and carries no `phases` at all, so a proof
  about the active phase — who holds `phases[<KEY>]` before and after a relaunch, a catch-up, a
  `spawn_worker` — must read the rig daemon's `state.json` on disk (poll it; the daemon writes it
  atomically, and every phase write persists before the route responds). `/state` is fine for
  `roles`/`trees`/`admission`; it is the wrong instrument for `phases` (LEGION-37's tester,
  six rounds).
