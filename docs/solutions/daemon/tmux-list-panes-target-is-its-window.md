---
title: "tmux `list-panes -t <pane>` lists the pane's whole window: select the row by `#{pane_id}`, never by position"
category: daemon
tags:
  - tmux
  - list-panes
  - liveness-probe
  - pane-id
  - process-manager
  - worker-boot-watchdog
date: 2026-09-12
status: active
module: daemon
problem_type: correctness
component: tmux.ts
severity: high
applies_when:
  - A daemon probe reads a per-pane fact (pid, start command, activity) through `list-panes -t <target>`
  - The target may be a pane id (`%N`) of a pane that is not the first in its window
  - A new tmux format string is added to a probe and its callers pass `tmuxPaneId ?? tmuxWindowId`
related_issues:
  - "LEGION-9"
  - "sjawhar/legion#945"
---

# tmux `list-panes -t <pane>` Lists the Pane's Whole Window: Select the Row by `#{pane_id}`, Never by Position

## Context

`lookupPane` (`packages/daemon/src/daemon/tmux.ts`, formerly `panePid`) is the liveness probe behind
`ProcessManager.probe`, `controllerAlive`, and `WorkerBootWatchdog.probeAlive`, through `TmuxRuntime.verifyPaneProcess`.
Until `sjawhar/legion#945` it ran `list-panes -t <target> -F "#{pane_pid}"` and took the first token of
stdout. That is correct for a window id and for a window's first pane — and wrong for every pane split into an
existing window, which is how every phase worker after the first lands in its issue's window.

Reproduced on the rig's private server (`tmux -L legion-sjawharlegion`; window `@1` holds the architect `%1`
and the workers):

```
$ tmux -L legion-sjawharlegion list-panes -t %3 -F '#{pane_id} #{pane_pid}'
%1 3715931
%3 4141285
$ tmux -L legion-sjawharlegion display-message -p -t %3 '#{pane_pid}'
4141285
```

The old `panePid(server, "%3")` (the function's name before it grew its three-way verdict) returned `3715931` — the architect's pid. A dead worker whose architect was alive
probed alive, and the boot watchdog skipped its socket probe on the strength of a sibling's process.

## The tmux fact

`list-panes -t <target>` resolves `<target>` to a **window** and lists every pane in it. A pane id is accepted as a
target, but it only picks the window; there is no single-pane listing without `-a`/`-s` (which list the whole
server/session). Row order is layout order, not "the target first".

## The fix shape

Ask for the id alongside the fact and select the row by id:

```ts
const panes = await server.run(argv(server, "list-panes", "-t", paneId, "-F", "#{pane_id} #{pane_pid}"));
if (panes.exitCode !== 0) {
  return PANE_GONE_STDERR.test(panes.stderr ?? "") ? { status: "absent" } : { status: "failed", detail };
}
const row = panes.stdout.split(/\r?\n/).map((l) => l.trim().split(/\s+/)).find((r) => r[0] === paneId);
```

- The target is always a pane id (`verifyPaneProcess` is the one caller, and every locator it checks carries
  `tmuxPaneId` -- one without a pane id has no process identity either and is dead before any tmux call). The row
  is selected with exact equality (`r[0] === paneId`; see `docs/solutions/testing/mutation-proof-probe-tests.md` for
  why the test fixture must prove the equality is exact). The pid it returns is only the first half of the check: the
  caller then compares it, and the process's `/proc/<pid>/stat` start ticks, against the identity the locator
  recorded at launch, so a reissued pane id never passes as the recorded process.
- A pane id absent from a successful listing is `absent` — never approximated by a sibling's pid. tmux exits 1 for
  an unknown pane (`can't find pane: %999999`), but a *known* pane's window listing can also simply not contain a
  stale id; the row filter covers both.
- A nonzero exit is `absent` only when its stderr proves the pane gone (`PANE_GONE_STDERR`: `can't find pane`, `no
  server running`, `error connecting … No such file or directory`). Any other nonzero exit — a client killed by the
  runner's 30 s timeout (exit code, empty stderr), a server not responding — is `failed`: it proves nothing about the
  pane, and the runtime must never read it as either alive or gone. `TmuxRuntime.stop` throws `ProcessStopFailed`
  on it (the tree stays lingering for the sweep to retry), `TmuxRuntime.probe` throws (every caller logs and retries
  later), and `probedWindowId` simply does not reuse that window. Collapsing a failed listing to "gone" was the
  round-2 review finding on `sjawhar/legion#981`: a stop that returned clean on it let the caller clear the locator of a
  possibly-live process.

Rejected at the design gate (do not re-open): `display-message -p -t <pane> '#{pane_pid}'` (kept out of the daemon —
the live check uses it as the independent oracle to compare `lookupPane` against), and `list-panes -a` with a global
filter. Callers passing only pane ids was also rejected then, as a rule imposed on callers that still had window-only
locators; it holds today by construction instead -- a locator's pane id and its process identity are recorded
together at launch, and a locator lacking them is never probed through tmux at all.

## When you will meet this again

Any new probe that reads a per-pane fact — `#{pane_start_command}`, `#{pane_activity}`, `#{pane_dead}` — through
`list-panes -t` with a target that may be a split-in pane. Request `#{pane_id}` in the format and filter; the
existing `listUnknownPanes` (`list-panes -a` with a tab-separated format led by `#{pane_id}`) is the other worked
example in `tmux.ts`.

## Live verification

The tester's/reviewer's driver: for every `roles[*].locator.tmuxPaneId` in the daemon's `state.json`, compare
`lookupPane(server, paneId)` against `display-message -p -t <paneId> '#{pane_pid}'`, plus a negative control
(`%999999` → `{ status: "absent" }`). Run from a pane shell — it inherits `TMUX_TMPDIR=/home/ubuntu/.tmux/sockets`, where the
private server's socket lives; a kernel subprocess without it reaches no server. Against `main`'s `tmux.ts` every
non-first pane read the architect's pid; against the fix, `ALL MATCH`.
