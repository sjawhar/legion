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

- The Legion identity family needs no scrub before `scripts/smoke/up.sh` or a scratch daemon.
  Since LEGION-74 the daemon builds every pane's environment from `PANE_ENV_ALLOW_LIST`
  (`environment.ts`), which names no `LEGION_*` variable, and scrubs the private tmux server's
  own tables at boot, so an inherited `LEGION_TREE`, `LEGION_ISSUE`, `LEGION_ROLE`,
  `LEGION_GENERATION`, `LEGION_WORKSPACE`, `LEGION_ROOT_WORKSPACE`, `LEGION_PROJECT`, or
  `LEGION_CONTROLLER` — and the credential family (`LEGION_GRANT`, `LEGION_GRANT_FILE`,
  `LEGION_BOOT_TOKEN_FILE`, `LEGION_CREDENTIAL_HELPER`, `LEGION_CONTROL_SUBJECT`) — never reaches
  a pane. `up.sh` writes `daemon_url` into its `legion.yaml` and passes `LEGION_STATE_DIR`
  explicitly, so the two config keys above are covered too. Checkpoint 14
  (`scripts/smoke/checkpoints.sh 14`, every webhook mode) fails the rig if the controller never
  claimed its role or if any recorded pane's process tree carries an identity the daemon did not
  set for it — proven from a worker pane with nothing unset by LEGION-88, whose canary rig
  (`SMOKE_OMP_LAUNCH_PREFIX='env LEGION_TREE=CANARY-1 …'`) also shows the two silences it made
  speak: the pane's own one-sentence refusal (`packages/pi-envoy/extensions/legion.ts`,
  `CONFLICTING_LAUNCH_MARKERS_NOTICE`) and the daemon's `[legion] retiring the controller: alive in
  pane … but it never claimed its role within its <N>s registration deadline` line (the root
  architect has the same line, `retiring <KEY>'s root architect`; see the registration-deadline
  bullet in `packages/daemon/src/daemon/AGENTS.md`). The LEGION-72 proof's scrub
  (`env -u LEGION_TREE -u LEGION_ISSUE …`) was needed only because those daemons predated LEGION-74:
  the symptom it cured — a controller pane carrying both `LEGION_CONTROLLER` and `LEGION_TREE`, two
  silent six-minute registration cycles — is what checkpoint 14 now fails on and the two lines now
  name. `checkpoints.sh` reads `${SMOKE_DIR}/daemon/state.json` and addresses the private tmux
  server by `SMOKE_PROJECT`'s slug; no `LEGION_*` variable steers it, so it needs no scrub either.
  What a pane may still need to unset is `LEGION_OMP_PATH` (the next two bullets) and the Dispatch
  pair (the bullet after them).
- `LEGION_OMP_PATH` still needs unsetting (`env -u LEGION_OMP_PATH …`) since LEGION-77: the dogfood daemon's private tmux server still
  carries `LEGION_OMP_PATH=…/omp-18.1.15-sami.9bff2014-rpcfix` (see
  `omp-pin-bump-behavioral-proof.md`, "One operator observation"), every pane inherits it, and a
  scratch daemon started from a pane honours it as its OMP override. That binary cannot load its
  `pi_natives` addon (`does not expose the … version sentinel`) — not because the build is broken
  but because OMP keys the natives cache by version string alone (`~/.omp/natives/18.1.15/`) and
  three different builds that all call themselves 18.1.15 share that directory; whichever
  extracted last wins (LEGION-92, the corrected premise: production panes run 18.1.18 with their
  own directory and were never affected). So the `pi.agents` probe fails **definitively** at the
  launch hold — after config loaded and the App keys resolved, before the listening lines — and
  `legion start` exits 1 with `Configured OMP invocation does not expose pi.agents`. Scrubbed, the
  daemon resolves the pinned mise release and boots. Put the scrub on the launching command
  (`env -u LEGION_OMP_PATH …`), never a shell-level `unset` beforehand; see
  `../testing/smoke-rig-from-a-worker-pane-mains-scripts-env-u-app-keys-and-a-session-actor.md` §2.
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
- `scripts/smoke/README.md`, checkpoint 14 (the table row and "Checkpoint 14's negative"): the
  rig's identity check — the controller claim, every pane's daemon-set identity, the server's
  global table — and the canary rig that proves its failure.
- `packages/daemon/src/daemon/AGENTS.md`, the registration-deadline bullet: the two retirement
  lines the daemon logs for a live-but-unregistered controller or root, `describeProcessLocation`
  of its pane, and the `worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals`
  deadline they name (LEGION-88).
