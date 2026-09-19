---
title: "An isolated scratch daemon started from inside a Legion pane: env -i, unroutable Envoy/Dispatch, a launch prefix that runs the probes but no agent — and the two things it cannot reach"
category: testing
tags:
  - scratch-daemon
  - e2e
  - throwaway-instance
  - env-i
  - omp_launch_prefix
  - self-report-fixture
  - negative-control
  - tmux
  - jetstream
  - fault-injection
  - LEGION_TMUX_PATH
  - kill-pane
  - StopFailed
date: 2026-09-18
status: active
module: packages/daemon
related_issues:
  - "LEGION-105"
  - "sjawhar/legion#1187"
  - "LEGION-25"
  - "LEGION-203"
symptoms:
  - "The plan cites a 2026-09-13 order forbidding rigs while the deployment instructions name a scratch daemon as the daemon's proof surface"
  - "A seeded root's self-report fixture writes 403 to its result file instead of 200"
  - "A real root launch on the scratch daemon would start an architect agent against issues that do not exist, with real provider keys"
  - "The E2E line says a failed root retire cannot be seeded on the rig because kill-pane always succeeds after the stop timeout"
  - "A stuck-omp pane retires cleanly (`root stopped`) although the wrapped process ignored its shutdown frame"
---

# An isolated scratch daemon started from inside a Legion pane

## Which order applies

Sami's 2026-09-13 order withdrew rigs; his 2026-09-14 19:41Z correction (LEGION-25 spec v21,
recorded in
[`../legion/pre-merge-surface-withdrawn-by-standing-order-what-the-pr-carries-instead.md`](../legion/pre-merge-surface-withdrawn-by-standing-order-what-the-pr-carries-instead.md))
allows a throwaway instance of the worker's own — its own team slug, state directory, ports, NATS
container, tmux socket, and OMP profile — torn down after. Off limits, still: the shared dev-box
daemon and its state, `~/.omp/profiles/legion`, `~/.config/legion/*`, and every pane you did not
start. Loading the real profile plugin tree read-only through `OMP_PROFILE=legion` is loading, not
editing, and is what the deployment instructions ask for. Two documents in this directory carry a
"Suspended 2026-09-13" banner; their procedures are historical, this one is current.

## The recipe that worked (LEGION-105, twice, plus the tester's own)

Everything under one scratch directory; the driver, seed, and publish scripts stay beside the
evidence for the tester and reviewer to re-run.

- **Config:** copy the production `legion.yaml`, replace every shared resource: `project:
  sjawhar/105` (the slug `sjawhar105` names the private tmux socket `legion-sjawhar105`), `port`
  and `worker_stream_port` of your own, `state_dir` under the scratch dir, `nats_urls` pointing at
  a scratch JetStream container (`docker run -d --name legion105-nats -p 127.0.0.1:14224:4222
  nats:2.10 -js`; create the `ENVOY_NOTIFICATIONS` stream with subjects `notifications.>` before
  publishing), a project key that does not exist in Dispatch (`LEGSMOKE`), `envoy_url:
  http://127.0.0.1:9` (unroutable: no wake ever reaches the real listener), the production
  `omp_invocation`, `github_apps`, and `instructions` verbatim. `daemon_url` stays the default
  loopback — the tmux guard refuses anything else. `legion start … --check-config` first.
- **Environment:** a pane carries the outer daemon's `LEGION_*`, `DISPATCH_*`, `GH_*`,
  `JJ_USER`/`JJ_EMAIL`, and `GIT_*` identity. Start the daemon *and* fork the scratch tmux server
  from `env -i` with only `HOME USER LOGNAME LANG SHELL TERM PATH XDG_RUNTIME_DIR` plus
  `XDG_STATE_HOME=<scratch>` (so the legions registry is yours), `OMP_PROFILE=legion`,
  `DISPATCH_URL=http://127.0.0.1:9`, `DISPATCH_TOKEN=<any string>` (required whenever
  `dispatch_url` is set; every PATCH fails and is recorded in `pendingStatusWrites`, which is
  itself evidence), `ENVOY_NATS_URL` at the scratch bus; then `eval "$(mise env -s bash)"` inside.
  The tmux server forked from a pane-shaped environment hands the outer daemon's values to every
  pane it opens — the daemon's own boot scrub removes 44 of them and says so, but fork it clean.
- **Boot settled:** poll the scratch registry file (`<XDG_STATE_HOME>/legion/legions.json`) for
  the project id — `startDaemon` resolves and `legion start` writes it only once the launch hold
  releases and boot admission has settled. Polling the API port is not that: it answers throughout
  the probes.
- **App keys:** on this box the production config reads them through `private_key_command: cat
  /etc/legion/<role>.pem`, readable by uid `legion`; there is no `secrets` CLI on `PATH`. A plan
  that says `secrets KEY… -- bun run …` was written for another box — check the config's key
  source before scripting the launch.
- **Panes:** real `legion worker-shim --socket <sock> -- bun <fixture>` panes around the repo's
  OMP stand-ins (`packages/daemon/src/cli/__tests__/fixtures/`): `self-report-omp-rpc.ts` for a
  root (it POSTs `/process/exit` when its stdin closes and writes the status to a result file),
  `fake-omp-rpc.ts` for a worker. Seed `state.json` with a script that imports `newLegionState`
  and `saveState` from the *source under test* (main's export for the control run, the workspace
  for the branch) so the schema version matches; record each pane's `panePid` and
  `/proc/<pid>/stat` start ticks in its locator or the probe answers `not-recorded-process`.
- **Driving events:** publish one Dispatch envelope on the scratch bus
  (`notifications.dispatch.issue.<KEY>.issue.closed`, then `….issue.updated` carrying `todo`) with
  `nats.jetstream().publish`; the daemon's durable consumer logs `consumed event …`.
- **Control run:** the same driver against a `git archive` export of the base (`main`, or the
  previous PR head in a corrective round) with its own `bun install --frozen-lockfile --backend
  copyfile` — the hard-link backend fails with EPERM in a fresh workspace on this box.
- **Isolation check:** `tmux -L legion-<live project> list-panes -a -F '#{window_id} #{pane_id}
  #{pane_pid}'` hashed before and after; byte-identical or the run is invalid.
- **Teardown:** kill the scratch daemon (SIGTERM runs its persist-and-exit), `tmux -L <scratch>
  kill-server`, `docker rm -f` the container. Leave the evidence directory.

## A real launch on the scratch daemon: run the probes, not an agent

The reopen half relaunches a root for real: the daemon provisions a workspace (a `jj git clone` of
the repository with the implement App's token, ~65 MB, into the scratch state dir) and opens a
pane running the production `omp_invocation` under the production `omp_launch_prefix` — which
would start an architect agent, with real provider keys, against an issue that does not exist.
Wrap the prefix:

```sh
#!/bin/sh
# real OMP for the boot probes; a real root/worker launch sleeps and exits
set -eu
for arg in "$@"; do case "$arg" in --mode|--resume=*) exec sleep 45 ;; esac; done
exec /home/legion/.local/bin/legion-pane-env "$@"
```

The probes (`omp models …`, the `pi.agents`/plugin-load probes) still load the real profile plugin
tree — the contract check is real — and the daemon's own decisions are what the smoke reads: the
pane's `#{pane_start_command}` carries `--resume=<kept file> --mode rpc`, `state.json` carries the
generation, slot, and `pendingStatusWrites`, `daemon.log` carries `resurrecting <KEY> by resuming
OMP session …`.

## The two things it cannot reach

1. **A seeded root holds no daemon-minted capability, so its `/process/exit` answers 403.** The
   pane was stopped, the locator cleared, the claims removed exactly as the acceptance asks, but
   the `200 → reportRootExit` round trip is proven only by the real-tmux E2E fixture
   (`real-shutdown-e2e.test.ts`, `LEGION_E2E=1`), which mints a boot token and registers through
   `/process/started` before the fixture posts. Say so in the E2E line; do not describe the 403 as
   the round trip.
2. **A race inside a fixture-driven window.** The self-report fixture posts its exit the instant
   its stdin closes, so the rig cannot land a `todo` between the retire's shutdown frame and the
   root's exit report; the review's exit-report-during-retire defect is proven by the unit tests
   only. Name that gap in the E2E line rather than let the rig imply coverage it does not have.

## The third thing — reachable after all: a fault at the tmux boundary

Round 2's blocker (a reopen over a root the retire could not stop; the daemon doc above) needs a
**failed** root leg: a `StopFailed` from `kill-pane`. The implementer's rig concluded it could not
be seeded — a `stuck-omp-rpc.ts` root ignores its shutdown frame, but the runtime's `kill-pane`
succeeds after `tree_stop_timeout_seconds`, so the retire reports `root stopped` — and the E2E
line said the case stood on unit tests only. The tester reached it without touching the daemon or
the tmux server: the daemon's own tool-discovery override, `LEGION_TMUX_PATH`, pointed at a
wrapper script.

```sh
#!/bin/sh
# LEGION_TMUX_PATH wrapper: fail kill-pane with a stderr that proves nothing while a flag exists
set -eu
if [ -e /tmp/<scratch>/inject-kill-failure ]; then
  for arg in "$@"; do
    case "$arg" in kill-pane) echo "tmux: injected failure (kill-pane refused by the wrapper)" >&2; exit 1 ;; esac
  done
fi
exec /usr/bin/tmux "$@"
```

What makes it a fault the daemon must handle rather than one it may explain away:

- The stderr **must not match `PANE_GONE_STDERR`** (`tmux.ts`: `no server running`, `can't find
  pane`, `error connecting to … (No such file or directory)`). Those shapes are the contract for
  "the pane is gone, clear the locator"; anything else on a non-zero `kill-pane` is
  `ProcessStopFailed` → `StopFailed`, locator and claim kept, sweep retries. A wrapper that printed
  `no server running` would prove the opposite of what you want.
- `list-panes` and every other verb pass through untouched, so the probe still answers `alive`
  and the graceful path still runs (the stuck fixture ignores it); only the destroy step fails.
- The **flag file** is the injection's clock: present for the `issue.closed` and the sweep's
  retry, removed before the `issue.updated` carrying `todo`, so one run walks the exact sequence
  the finding described — fail, retry-and-fail, lift, reopen — and `daemon.log` narrates it:
  `root process failed to stop while retiring the processes of <KEY>: … StopFailed: kill-pane %1
  exited 1: tmux: injected failure …` → `retiring the processes of lingering tree <KEY> failed
  after linger; its surviving locators and claims are kept and the linger sweep retries` → (next
  tick) the same two lines → (flag removed, `todo`) `stopping <KEY>'s retained root process before
  its relaunch: a teardown could not confirm it stopped (alive)` **before** `resurrecting <KEY> by
  resuming OMP session …`. `state.json` shows `resumeSessionFile` recorded although the stop
  failed, the architect claim kept, then generation 2 with the new locator carrying that
  `ompSessionFile`, pane `%1` gone, one pane `%2` with `--resume=<the same file> --mode rpc`.
- **Negative control on the previously approved tip:** the same driver against a `git archive`
  export of `ba7a1dc8` shows the two-roots shape the finding described — a fresh pane opened
  beside the live `%1`, the locator overwritten, no `stopping … retained root process` line.
- **What it still cannot see:** the boot token's `expectedSessionId` is in memory, never in state;
  that half of the same-agent guarantee is locked by the unit test, and the E2E line says so.

The recipe generalises: `LEGION_JJ_PATH`, `LEGION_GIT_PATH`, `LEGION_GH_PATH`, `LEGION_TMUX_PATH`,
`LEGION_MISE_PATH`, and `LEGION_OMP_PATH` are the daemon's discovery overrides
(`environment.ts`), so a wrapper at any of them injects a fault at that boundary — a
`list-panes` that fails *without* proving the pane gone (`tmux.lookupPane`'s `failed` verdict),
a `jj git clone` killed at the slow budget, a `gh` that answers 5xx — on a daemon whose code is
the exact export under test. The daemon `stat`s the override at boot and runs it for the probes,
so the wrapper must be executable and must `exec` the real binary for everything it does not
fault.

Do not reach for `SIGSTOP` on the scratch tmux server instead: a tmux client blocked on a frozen
server ignores the runner's 30 s kill until the server thaws, so probes and stops hang rather than
fail (the tester's first attempt saw `root stopped` only after the thaw; filed as LEGION-203).
The wrapper fails fast and deterministically; a frozen server tests the runner's timeout, which
is a different defect.

## Related

- [`scratch-daemon-rig-proves-what-unit-tests-cannot.md`](./scratch-daemon-rig-proves-what-unit-tests-cannot.md)
  and [`smoke-rig-from-a-worker-pane-mains-scripts-env-u-app-keys-and-a-session-actor.md`](./smoke-rig-from-a-worker-pane-mains-scripts-env-u-app-keys-and-a-session-actor.md)
  — the suspended predecessors; their gotchas (boot probe vs version command, sibling-rig noise on
  a shared project) still hold.
- [`../daemon/config-env-keys-that-panes-also-carry.md`](../daemon/config-env-keys-that-panes-also-carry.md)
  — why `daemon_url` refuses an inherited value and `state_dir` does not.
- [`gated-e2e-suites-catch-real-pane-fixtures-without-identity.md`](./gated-e2e-suites-catch-real-pane-fixtures-without-identity.md)
  — the pane identity a locator needs.
