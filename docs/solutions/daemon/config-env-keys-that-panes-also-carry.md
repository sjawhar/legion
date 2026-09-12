---
title: "Config env keys the daemon also exports to its panes: LEGION_DAEMON_URL, LEGION_STATE_DIR, and the tmux daemon_url guard"
category: daemon
tags:
  - config
  - environment
  - LEGION_DAEMON_URL
  - LEGION_STATE_DIR
  - daemon_url
  - tmux
  - inherited-environment
  - smoke-rig
date: 2026-09-12
status: active
module: packages/daemon
related_issues:
  - "LEGION-21"
  - "sjawhar/legion#962"
---

# Config Env Keys the Daemon Also Exports to Its Panes

## The hazard

The daemon reads its configuration through `resolveValue` (CLI override, then file, then
environment, then default — `docs/solutions/daemon/config-resolution-patterns.md`). Two of the
environment keys it reads are also variables it *writes* into every pane it spawns
(`processes.ts`, the root/worker/controller env builders):

| variable | as config input | as pane output |
| :--- | :--- | :--- |
| `LEGION_DAEMON_URL` | `daemon_url` (LEGION-21) | the URL the pane's processes register with |
| `LEGION_STATE_DIR` | `state_dir` | the pane's secrets/handoff root |

Legion workers run the smoke rig, `legion start --check-config`, and real scratch daemons from
inside a Legion pane. Such a daemon inherits the **outer** daemon's values through its
environment. With `LEGION_DAEMON_URL` that is not a refusal but a misroute: the inner daemon
tells every process it spawns to register at the outer daemon's URL (this box's dogfood daemon
at `http://127.0.0.1:13370`), while its own API listens on a different port. Nothing fails
loudly; the inner daemon simply never hears from its own roots.

`DISPATCH_TOKEN` and the `LEGION_BOOT_TOKEN*` family are the same hazard class, already handled:
`stripDispatchEnv` (`environment.ts`) removes them from every child the daemon spawns, because a
pane-started daemon inherits the pane's secret and its private tmux server would otherwise hand
it to every pane. Secrets are stripped; a URL has to be checked.

## The guard (tmux)

Under `runtime: tmux` the only correct `daemon_url` is the daemon's own loopback address, so
`resolveDaemonConfig` (`config.ts`) refuses any other resolved value, whatever its source:

```
daemon_url must be http://127.0.0.1:<port> when runtime is tmux (got <value>; an inherited LEGION_DAEMON_URL from an outer Legion pane?)
```

The default is that loopback value, an explicit equal value is accepted, and the message names
the field, the resolved value, the expected value, and the likely source. Under
`runtime: kubernetes` the URL is free (a pod cannot reach the daemon's loopback) and the file key
outranks the environment as usual, so a kubernetes daemon started from a pane with `daemon_url`
in its file is not misrouted either.

Proof on the real CLI (all from a pane whose environment carried the dogfood URL):

- yaml `port: 19370` + `daemon_url: http://127.0.0.1:19371` → refused, exit 1, message above
  with `got http://127.0.0.1:19371`.
- yaml `port: 19370`, no `daemon_url`, environment `LEGION_DAEMON_URL=http://127.0.0.1:13370`
  → refused naming `:13370`.
- the smoke rig's own `legion.yaml` (`scripts/smoke/up.sh` writes
  `daemon_url: http://127.0.0.1:${daemon_port}`) → `Config OK`.

`LEGION_STATE_DIR` has no equivalent guard and does not need one: an inherited state directory
is the outer daemon's, and the per-`state_dir` instance lock refuses a second daemon on it while
the first is alive. The message blames a duplicate daemon rather than an inherited variable, so
the rig sets `state_dir` in the file and `LEGION_STATE_DIR` for the pane explicitly.

## Working from a pane

- Start rigs and scratch daemons with the inherited family scrubbed. The pattern that worked:
  `env -u LEGION_DAEMON_URL -u LEGION_STATE_DIR -u LEGION_TREE -u LEGION_ISSUE -u LEGION_ROLE
  -u LEGION_GENERATION -u LEGION_WORKSPACE -u LEGION_ROOT_WORKSPACE -u LEGION_PROJECT
  -u LEGION_GRANT -u LEGION_BOOT_TOKEN_FILE -u LEGION_CREDENTIAL_HELPER -u LEGION_CONTROL_SUBJECT
  -u LEGION_MAX_RECURSION_DEPTH -u LEGION_CONTROLLER -u DISPATCH_TOKEN_FILE -u TMUX
  -u GH_CONFIG_DIR …`, with `DISPATCH_URL`/`DISPATCH_TOKEN` re-supplied deliberately. The
  checkpoints script must run under the same scrub (`env -u LEGION_DAEMON_URL`), or it reads the
  outer daemon's state.
- Write `daemon_url` (and `state_dir`) into every generated `legion.yaml`. The guard makes an
  omission a refusal on tmux, which is the point: a rig that forgets is told, not misrouted.
- When reading a daemon's environment for evidence, print variable *names* only
  (`tr '\0' '\n' < /proc/<pid>/environ | grep -E '^(LEGION_|DISPATCH_)' | sed 's/=.*//'`);
  the same family carries secrets.

## Rule for the next config key

Before adding a `LEGION_*` environment key to `resolveDaemonConfig`, grep the pane env builders
in `processes.ts` for the same name. If the daemon exports it to panes, decide up front which of
three treatments applies — strip it from children (a secret), refuse an inherited value (a
per-daemon address), or require the file key in every rig — and name the hazard in the
`config.ts` row of `packages/daemon/src/daemon/AGENTS.md`, as the `daemon_url` row now does.

## Related

- `docs/solutions/daemon/config-resolution-patterns.md`: `resolveValue` precedence and source
  tracking, which is what lets the refusal name its source.
- `docs/solutions/legion/worker-pane-shell-gotchas.md`: the other pane-environment facts
  (`env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test`, stacked grants) a worker needs first.
- `docs/solutions/testing/fresh-state-dir-boot-beside-a-busy-rig.md`: the scratch-daemon recipe
  that applies this scrub end to end.
