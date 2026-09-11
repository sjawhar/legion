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

`panePid` (`packages/daemon/src/daemon/tmux.ts`) is the liveness probe behind `ProcessManager.probe`,
`controllerAlive`, and `WorkerBootWatchdog.probeAlive`. Callers pass `locator.tmuxPaneId ?? locator.tmuxWindowId`.
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

The old `panePid(server, "%3")` returned `3715931` — the architect's pid. A dead worker whose architect was alive
probed alive, and the boot watchdog skipped its socket probe on the strength of a sibling's process.

## The tmux fact

`list-panes -t <target>` resolves `<target>` to a **window** and lists every pane in it. A pane id is accepted as a
target, but it only picks the window; there is no single-pane listing without `-a`/`-s` (which list the whole
server/session). Row order is layout order, not "the target first".

## The fix shape

Ask for the id alongside the fact and select the row by id:

```ts
const panes = await server.run(argv(server, "list-panes", "-t", target, "-F", "#{pane_id} #{pane_pid}"));
if (panes.exitCode !== 0) return undefined;
const rows = panes.stdout.split(/\r?\n/).map((l) => l.trim().split(/\s+/)).filter((r) => r[0] !== "");
const row = /^%\d+$/.test(target) ? rows.find((r) => r[0] === target) : rows[0];
```

- A pane-id target selects **its own** row with exact equality (`r[0] === target`; see
  `docs/solutions/testing/mutation-proof-probe-tests.md` for why the test fixture must prove the equality is exact).
- A window-id target keeps the **first** row. That is deliberate: `probe` backfills a pane-id-less locator's
  `tmuxPaneId` from `firstPaneId`, which reads the same first line, so the pid and the backfilled id describe the
  same pane.
- A pane-id target absent from the listing is `undefined` — never approximated by a sibling's pid. tmux exits 1 for
  an unknown pane (`can't find pane: %999999`), but a *known* pane's window listing can also simply not contain a
  stale id; the row filter covers both.

Rejected at the design gate (do not re-open): `display-message -p -t <pane> '#{pane_pid}'` (kept out of the daemon —
the live check uses it as the independent oracle to compare `panePid` against), `list-panes -a` with a global filter,
and changing callers to always pass a pane id.

## When you will meet this again

Any new probe that reads a per-pane fact — `#{pane_start_command}`, `#{pane_activity}`, `#{pane_dead}` — through
`list-panes -t` with a target that may be a split-in pane. Request `#{pane_id}` in the format and filter; the
existing `listUnknownPanes` (`list-panes -a` with a tab-separated format led by `#{pane_id}`) is the other worked
example in `tmux.ts`.

## Live verification

The tester's/reviewer's driver: for every `roles[*].locator.tmuxPaneId` in the daemon's `state.json`, compare
`panePid(server, paneId)` against `display-message -p -t <paneId> '#{pane_pid}'`, plus a negative control
(`%999999` → `undefined`). Run from a pane shell — it inherits `TMUX_TMPDIR=/home/ubuntu/.tmux/sockets`, where the
private server's socket lives; a kernel subprocess without it reaches no server. Against `main`'s `tmux.ts` every
non-first pane read the architect's pid; against the fix, `ALL MATCH`.
