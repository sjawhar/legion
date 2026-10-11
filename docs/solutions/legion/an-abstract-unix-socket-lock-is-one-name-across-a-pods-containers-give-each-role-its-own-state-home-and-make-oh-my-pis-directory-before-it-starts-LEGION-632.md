---
title: "An abstract unix socket lock is one name across a pod's containers: give each role its own state home, and make Oh My Pi's directory under it before Oh My Pi starts"
category: legion
tags:
  - agent-sandbox
  - oh-my-pi
  - xdg-state-home
  - browser-broker
  - abstract-unix-socket
  - pod-safety
date: 2026-10-09
status: active
module: packages/daemon/internal/runtime/sandbox, packages/daemon/internal/podsafety, packages/daemon/internal/ompdirs
related_issues:
  - "LEGION-632"
  - "sjawhar/legion#1842"
---

# An abstract unix socket lock is one name across a pod's containers: give each role its own state home, and make Oh My Pi's directory under it before Oh My Pi starts

- An abstract unix socket (a name beginning with a NUL byte) is scoped to the network namespace,
  not the filesystem. Containers of one pod share the network namespace while each has a
  filesystem of its own, so a lock taken as an abstract socket collides across containers even
  where the lock file's path exists on every container separately. Oh My Pi's browser broker takes
  its lock that way, named from `<state root>/run/daemons/<hash of the workspace's real path>/broker.pid`:
  two roles of one issue pod with one state root and one workspace path get one lock name, the
  first role's broker holds it, and every other role's `browser.open` times out while its
  `broker.sock` sits unreachable on the first container's filesystem.
- Isolate by changing what the name is derived from, not by separating filesystems: each role's
  agent is told a state home of its own, `XDG_STATE_HOME=/home/legion/.local/state/<role>`
  (`roleStateHome`, `packages/daemon/internal/runtime/sandbox/manifest.go`), carried in the
  launcher's start command rather than the pod template, so the pod spec and its goldens do not
  change per container.
- Oh My Pi honours `XDG_STATE_HOME` only where `$XDG_STATE_HOME/omp/profiles/<profile>` (or
  `$XDG_STATE_HOME/omp` with no profile) already exists when it starts; otherwise it roots its
  state under the profile's config root as if the variable were unset. Make that directory before
  Oh My Pi starts (`podsafety.EnsureStateHome`, run by the shim after `podsafety.Apply`), and port
  the rule rather than guess it (`ompdirs.StateRoot`, `packages/daemon/internal/ompdirs`). A
  `PI_CODING_AGENT_DIR` that names anything but the profile's own agent directory turns the XDG
  lookup off entirely; setting the variable alone is not enough.
- Prove the rule against the pinned binary with a command that writes state. `omp models list`
  writes `logs/omp.<date>.<pid>.log` under the state root it chose; `omp config get` writes nothing
  and proves nothing. The test runs the pinned `omp` both ways (directory made, directory absent)
  and asserts where the log landed and that the port answers the same root
  (`TestThePinnedOhMyPiRootsItsStateUnderTheStateHomeOnlyWhereTheShimMadeTheProfileDirectory`,
  `internal/podsafety/podsafety_test.go`, `LEGION_TEST_OMP=<pinned omp>`).
- Fence the agents' state home against operator mounts at `--check-config`, not at launch: a mount
  at, under or above `/home/legion/.local/state` would have every role's shim refuse to start
  naming a directory it could not make, so `CheckPod` refuses it when the file is read
  (`internal/runtime/sandbox/operatorpod.go`), naming the path and why.
- Keep the state home on the container's own filesystem, not the pod's in-memory state volume:
  Chromium's `browser-profiles` and Oh My Pi's logs live under it, and an `emptyDir` in memory
  would charge them to the pod's memory reservation.

## Evidence

Discovered live in the issue's own pod on the production cluster (PR #1842, `E2E (implementer)`,
2026-10-09): from the implementer's container, with `XDG_STATE_HOME=/home/legion/.local/state/implementer`
and `omp/profiles/legion` made under it, the image's `omp --no-extensions --no-session -p '…open about:blank…'`
answered `BROWSER_OK about:blank` with its broker at
`/home/legion/.local/state/implementer/omp/profiles/legion/run/daemons/<hash>/broker.{pid,sock}` and
lock `@omp-file-lock-22fb8be8…`, while the session's own broker (`~/.omp/profiles/legion/run/daemons/<hash>/`,
its own lock) kept running: two brokers in one network namespace. Control under the default
`XDG_STATE_HOME=/home/legion/.local/state`: the broker under `~/.omp/profiles/legion/…` with lock
`@omp-file-lock-3e97ae56…` — the lock name follows the state root. Before the fix, `browser.open`
from the second role of a pod timed out at 30 s against the first role's lock (LEGION-632 spec v5,
`.legion/LEGION-632/implement.json` `discoveredComplexity`). Stage 4a's `pod-baseline` reads the
role's agent environment from `/proc/<pid>/environ` and asserts the state home and the shim-made
directory; the reviewer's remaining risk (`review.json` `notProven`) is that the probe pod runs Oh
My Pi with no state home of its own and the shim's self-check is the port against itself, left to a
fast-follow.
