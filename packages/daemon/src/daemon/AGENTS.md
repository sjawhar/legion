# Daemon Module

The daemon is Legion's durable coordinator. It consumes webhook envelopes from core NATS, persists `LegionState`, publishes derived role events through Envoy, and owns the tmux root-process lifecycle.

## HTTP API

The localhost-only Legion API lives in `api.ts`.

| Surface | Purpose |
| --- | --- |
| `GET /legion/v1/state` | Read redacted durable Legion state. |
| `POST /legion/v1/process/started` | Register a root process and its architect role claim. |
| `POST /legion/v1/process/exit` | Authenticated architect exit that releases an admission slot or marks its root process dead. |
| `POST /legion/v1/issues`, `/waves/release`, `/issues/comment`, `/issues/body`, `/issues/labels`, `/issues/close` | Scoped architect writes. |
| `POST /legion/v1/phase`, `/role-backing`, `/grants`, `/git-credential`, `/gh-token` | Session attribution and credential grants. |
| `POST /legion/v1/controller/ready`, `/gates/approve`, `/admission`, `/backlog` | Controller lifecycle and control-plane actions. |

## Files

| File | Responsibility |
| --- | --- |
| `index.ts` | Boots state, core-NATS intake, process manager, API, resync, linger expiry, and signal persistence. |
| `config.ts` | Validates file and environment lifecycle configuration. |
| `events.ts` | Routes raw webhook envelopes through pure reducers and executes publication, linger, probe, and approval effects. |
| `processes.ts` | Admission, tmux root/controller spawning, exception recovery, resurrection, and linger. |
| `api.ts` | Localhost extension/controller write surface and session-bound credential grants. |
| `legion-state.ts` | Strict versioned state schema and atomic persistence. |
| `catchup.ts` | Derived overseer and worker catch-up payloads. |
| `resync.ts` | Low-frequency board convergence and residual anomaly reporting for the controller. |
| `approval-check.ts` | Human approval status backstop for the current PR head. |

## Operational invariants

- Role lanes use core NATS; the daemon, not the broker, persists failed role delivery.
- Root processes and the controller are tmux windows under global admission control. The daemon stores tmux window IDs; names are cosmetic, escaped issue labels.
- Process failure recovery is exception-driven. A root is trusted only when its recorded window's pane is live and running OMP; a dead root is resurrected under a generation lock.
- A lingering or closed root releases its admission slot, kills its recorded window, clears its locator, and removes its role claims. The linger sweep also removes session windows not recorded by a tree or controller.
- `DaemonConfig` supplies all lifecycle defaults: admission cap, worker budget, recursion depth, linger duration, CI quiet period, resync interval, and retry limit.

## OMP invocation and daemon tools

Set `omp_invocation` in `legion.yaml` or `LEGION_OMP_INVOCATION` to the required `mise x <tool> -- omp` command that launches the root architect and controller. The default is:

```sh
mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp
```

Launch the daemon normally with `bun run ...`, not inside that scoped `mise x` command. At startup it obtains the complete `mise env --json` environment, resolves absolute `jj`, `git`, `gh`, and `tmux` paths, and uses `mise where` to turn the default invocation into the pinned OMP binary path. All daemon subprocesses use those absolute paths; root and controller panes receive the complete `PATH` and execute that same OMP path.

Before loading state, opening core NATS, or serving the API, the daemon probes the exact resolved OMP executable with an isolated extension and refuses startup unless it confirms `pi.agents`. It also refuses startup with every missing required tool listed. Set `LEGION_MISE_PATH`, `LEGION_JJ_PATH`, `LEGION_GIT_PATH`, `LEGION_GH_PATH`, `LEGION_TMUX_PATH`, or `LEGION_OMP_PATH` to an absolute executable path to override discovery. The `mise x <tool> -- omp` form is required for `omp_invocation`; set `LEGION_OMP_PATH` when selecting a direct OMP binary.
