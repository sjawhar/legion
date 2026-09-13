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

Legion workers run the smoke rig, `legion start --check-config`, and real scratch daemons from
inside a Legion pane. Such a daemon inherits the **outer** daemon's values through its
environment. With `LEGION_DAEMON_URL` that is not a refusal but a misroute: the inner daemon
tells every process it spawns to register at the outer daemon's URL (this box's dogfood daemon
at `http://127.0.0.1:13370`), while its own API listens on a different port. Nothing fails
loudly; the inner daemon simply never hears from its own roots.

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
  -u LEGION_GRANT -u LEGION_GRANT_FILE -u LEGION_BOOT_TOKEN_FILE -u LEGION_CREDENTIAL_HELPER
  -u LEGION_CONTROL_SUBJECT -u LEGION_MAX_RECURSION_DEPTH -u LEGION_CONTROLLER -u LEGION_OMP_PATH
  -u DISPATCH_URL -u DISPATCH_TOKEN -u DISPATCH_TOKEN_FILE -u TMUX -u GH_CONFIG_DIR -u GH_TOKEN
  -u GITHUB_TOKEN -u GH_HOST -u JJ_USER -u JJ_EMAIL -u GIT_AUTHOR_NAME -u GIT_AUTHOR_EMAIL
  -u GIT_COMMITTER_NAME -u GIT_COMMITTER_EMAIL …`. Two kinds of key are in that list. One kind
  never reaches a child whether or not the shell scrubbed it: since LEGION-74 the daemon builds
  every child and pane environment from `PANE_ENV_ALLOW_LIST` (`environment.ts`), which passes no
  `LEGION_*`, `DISPATCH_*`, `GH_*`, or commit-identity name (`JJ_USER`/`JJ_EMAIL`, the Git
  author/committer variables — LEGION-44's pane-only exports), so a worker pane's App identity
  never reaches the private tmux server, the root-architect pane, or the controller pane, and the
  daemon's own `jj workspace add` runs under the user config, never the launching pane's App. The
  other kind the daemon **reads for itself** before any allow-list applies, and scrubbing it is
  what makes the run yours: `LEGION_DAEMON_URL` is refused on tmux when it names another daemon
  (the guard above) and is honoured as the daemon's URL on kubernetes; `LEGION_STATE_DIR` names the
  outer daemon's state directory, whose instance lock refuses a second daemon; `LEGION_OMP_PATH` is
  honoured as the OMP override (next bullet); `LEGION_MAX_RECURSION_DEPTH` and every other
  `LEGION_*` key `resolveDaemonConfig` reads is the outer daemon's setting. The checkpoints script
  must run under the same scrub (`env -u LEGION_DAEMON_URL`), or it reads the outer daemon's state.
- `LEGION_OMP_PATH` is in that list since LEGION-77: the dogfood daemon's private tmux server still
  carries `LEGION_OMP_PATH=…/omp-18.1.15-sami.9bff2014-rpcfix` (see
  `omp-pin-bump-behavioral-proof.md`, "One operator observation"), every pane inherits it, and a
  scratch daemon started from a pane honours it as its OMP override. That hand-built binary's
  `pi_natives` addon no longer matches its loader (`does not expose the … version sentinel`), so the
  `pi.agents` probe fails **definitively** at the launch hold — after config loaded and the App keys
  resolved, before the listening lines — and `legion start` exits 1 with `Configured OMP invocation
  does not expose pi.agents`. Scrubbed, the daemon resolves the pinned mise release and boots.
- The smoke rig honours the same inherited `LEGION_OMP_PATH` (LEGION-40): `scripts/smoke/up.sh`
  treats it as the operator's explicit override of the pinned OMP, so inherited it silently changes
  which OMP the rig runs. The first LEGION-40 run printed
  `GREEN OMP build: …/omp-18.1.15-sami.9bff2014-rpcfix` (the pane's hand-built binary, not the pin),
  reached `RIG READY`, and then its daemon died at the OMP probe on that binary's own loader
  mismatch (`Failed to load pi_natives native addon … does not expose the … version sentinel`).
  Nothing in the README's start block would produce that state in an operator's shell; the scrub
  is what makes a pane run equal an operator run.
- `DISPATCH_URL`/`DISPATCH_TOKEN`/`DISPATCH_TOKEN_FILE` are scrubbed, not re-supplied. Since
  LEGION-40 the rig reads both values from `${XDG_CONFIG_HOME:-$HOME/.config}/opencode/envoy.json`
  when the variables are unset, which is what an operator's shell has; every pane carries
  `DISPATCH_URL` and `DISPATCH_TOKEN_FILE`, so a run that leaves them would take the URL from the
  pane and the token from the file — a fallback proof that passes for the wrong reason. Export the
  pair only when the environment-wins path is the thing under test.
- `PATH` needs no scrub. Since LEGION-54 a pane's `PATH` starts with `<state_dir>/worker-bin` for
  the pane's life (the `gh` shim that execs `legion gh`), and `mise env` keeps an inherited PATH
  head, so a daemon started from inside a pane would otherwise resolve its own `gh` to the shim
  and hand every new pane a second `worker-bin` entry. `resolveDaemonEnvironment` therefore
  strips every inherited `worker-bin` entry at the daemon boundary (`pathWithoutWorkerBin`),
  exactly as it strips the inherited pane secrets, and `ProcessManager` then prepends the pane's
  own entry exactly once. `legion gh` applies the same strip before spawning `gh`, so it never
  re-enters itself through the shim either.
- Write `daemon_url` (and `state_dir`) into every generated `legion.yaml`. The guard makes an
  omission a refusal on tmux, which is the point: a rig that forgets is told, not misrouted.
- When reading a daemon's environment for evidence, print variable *names* only
  (`tr '\0' '\n' < /proc/<pid>/environ | grep -E '^(LEGION_|DISPATCH_)' | sed 's/=.*//'`);
  the same family carries secrets.

## Rule for the next config key

Before adding a `LEGION_*` environment key to `resolveDaemonConfig`, grep the pane env builders
in `processes.ts` for the same name. A `LEGION_*` key is never inherited by a pane (the allow-list
in `environment.ts` passes none of them), so there is no strip decision to make. What remains, when
the daemon exports the key to panes, is to decide up front between refusing an inherited value (a
per-daemon address, as `daemon_url` does) and requiring the file key in every rig — and to name
the hazard in the `config.ts` row of `packages/daemon/src/daemon/AGENTS.md`, as the `daemon_url`
row now does.

## Related

- `docs/solutions/daemon/config-resolution-patterns.md`: `resolveValue` precedence and source
  tracking, which is what lets the refusal name its source.
- `docs/solutions/legion/worker-pane-shell-gotchas.md`: the other pane-environment facts
  (`env -u DISPATCH_URL -u DISPATCH_TOKEN_FILE bun test`, stacked grants) a worker needs first.
- `docs/solutions/testing/fresh-state-dir-boot-beside-a-busy-rig.md`: the scratch-daemon recipe
  that applies this scrub end to end.
