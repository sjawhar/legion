> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

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
  - LEGION_OMP_PATH
date: 2026-09-12
status: active
module: packages/daemon
related_issues:
  - "LEGION-21"
  - "sjawhar/legion#962"
  - "LEGION-40"
  - "sjawhar/legion#1011"
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

The retired rig and scratch daemons were launched from inside Legion panes. A daemon launched from
a pane-shaped environment inherits the **outer** daemon's values. With `LEGION_DAEMON_URL` that
is not a refusal but a misroute: the inner daemon tells every process it spawns to register with
the outer daemon's URL (this box's dogfood daemon at `http://127.0.0.1:13370`), while its own API
listens on a different port. Nothing fails loudly; the inner daemon never hears from its roots.

`DISPATCH_TOKEN` and the `LEGION_BOOT_TOKEN*` family were once handled by a strip-list; since
LEGION-74 the daemon builds every child and pane environment from an allow-list
(`PANE_ENV_ALLOW_LIST`, `environment.ts`), so no `LEGION_*`/`DISPATCH_*` value is inherited at all.
A URL still has to be checked.

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

The original CLI proof used a pane environment carrying the dogfood URL: a mismatched loopback
`daemon_url` and an inherited outer-daemon URL both refused with the expected value in the error.
The retired rig's generated configuration was accepted because it named its own loopback port.
Current pre-merge proof belongs in the daemon test harness (`packages/daemon/src/daemon/__tests__/`)
and real-process fixtures; record any live confirmation on the pull request for the operator's
next authorized daemon restart.

`LEGION_STATE_DIR` has no equivalent guard and does not need one: an inherited state directory
is the outer daemon's, and the per-`state_dir` instance lock refuses a second daemon on it while
the first is alive. The message blames a duplicate daemon rather than an inherited variable, so
an isolated fixture must set `state_dir` in its configuration rather than inheriting it from a pane.

## Supported verification

Do not start rigs or scratch daemons. The daemon test harness and real-process fixtures must model
the two relevant boundaries: `PANE_ENV_ALLOW_LIST` prevents `LEGION_*`, `DISPATCH_*`, `GH_*`, and
commit-identity variables from reaching a child; daemon configuration is read before that allow-list
applies, so a pane-provided configuration value can still affect the daemon itself. A test must set
or remove each input on the exact child process it exercises rather than relying on a shell-level
mutation.

The OMP override remains an important edge case: multiple OMP builds with the same version string
share a native-module cache, so a version command can pass while a boot probe fails. The supported
harness proof exercises the `pi.agents` probe with a controlled `LEGION_OMP_PATH`; it never relies
on a pane's inherited setting. Likewise, `DISPATCH_URL`, `DISPATCH_TOKEN`, and
`DISPATCH_TOKEN_FILE` must be controlled by the fixture so a test does not pass through an ambient
configuration fallback.
- `PATH` needs no scrub. Since LEGION-54 a pane's `PATH` starts with `<state_dir>/worker-bin` for
  the pane's life (the `gh` shim that execs `legion gh`), and `mise env` keeps an inherited PATH
  head, so a daemon started from inside a pane would otherwise resolve its own `gh` to the shim
  and hand every new pane a second `worker-bin` entry. `resolveDaemonEnvironment` therefore
  strips every inherited `worker-bin` entry at the daemon boundary (`pathWithoutWorkerBin`),
  exactly as it strips the inherited pane secrets, and `ProcessManager` then prepends the pane's
  own entry exactly once. `legion gh` applies the same strip before spawning `gh`, so it never
  re-enters itself through the shim either.
- Daemon configuration must write `daemon_url` and `state_dir` explicitly. The tmux guard turns
  an omission into a refusal rather than a misroute.
- When reading a daemon's environment for evidence, print variable *names* only
  (`tr '\0' '\n' < /proc/<pid>/environ | grep -E '^(LEGION_|DISPATCH_)' | sed 's/=.*//'`);
  the same family carries secrets.

## Rule for the next config key

Before adding a `LEGION_*` environment key to `resolveDaemonConfig`, grep the pane env builders
in `processes.ts` for the same name. A `LEGION_*` key is never inherited by a pane (the allow-list
in `environment.ts` passes none of them), so there is no strip decision to make. What remains, when
the daemon exports the key to panes, is to decide up front between refusing an inherited value (a
per-daemon address, as `daemon_url` does) and requiring the file key in each deployment — and to
name the hazard in the `config.ts` row of `packages/daemon/src/daemon/AGENTS.md`, as the
`daemon_url` row now does.

## Related

- `docs/solutions/daemon/config-resolution-patterns.md`: `resolveValue` precedence and source
  tracking, which is what lets the refusal name its source.
- `docs/solutions/legion/worker-pane-shell-gotchas.md`: the other pane-environment facts
  (`env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test`, stacked grants) a worker needs first.
- `docs/solutions/testing/fresh-state-dir-boot-beside-a-busy-rig.md`: the suspended fresh-state
  rationale and the supported harness-based replacement.
