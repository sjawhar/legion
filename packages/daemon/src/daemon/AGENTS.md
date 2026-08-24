# Daemon Module

The daemon is Legion's durable coordinator. It consumes webhook envelopes from core NATS, persists `LegionState`, publishes derived role events through Envoy, and owns the tmux root-process lifecycle.

## HTTP API

The localhost-only Legion API lives in `api.ts`.

| Surface | Purpose |
| --- | --- |
| `GET /legion/v1/state` | Read redacted durable Legion state. |
| `POST /legion/v1/process/started` | Register a root process and its architect role claim. |
| `POST /legion/v1/process/exit` | Release an admission slot or mark a process dead. |
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
| `resync.ts` | Low-frequency board anomaly reporting for the controller. |
| `approval-check.ts` | Human approval status backstop for the current PR head. |

## Operational invariants

- Role lanes use core NATS; the daemon, not the broker, persists failed role delivery.
- Root processes are tmux windows under global admission control. Phase workers are native omp subagents.
- Process failure recovery is exception-driven. A dead root is resurrected under a generation lock; a live root receives a control directive.
- A lingering root releases its admission slot. Expiry kills only its recorded tmux window and removes its role claims.
- `DaemonConfig` supplies all lifecycle defaults: admission cap, worker budget, recursion depth, linger duration, CI quiet period, resync interval, and retry limit.
