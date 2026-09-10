# Daemon Module

The daemon is Legion's durable coordinator. It consumes webhook envelopes from core NATS and a durable per-repo JetStream consumer, persists `LegionState`, publishes derived role events through Envoy, and owns the tmux root-process lifecycle.

**Operational contract:** the daemon must run under a supervisor that restarts it on a non-zero exit — tmux alone is not one, and a durable-lane failure after a reducer has mutated live state exits the process deliberately (see `events.ts`'s `fatal` hook) expecting exactly that restart. Exactly one daemon may run per project at a time: `instance-lock.ts` enforces this with a pidfile lock scoped to `state_dir`, so every daemon for a project must share the same `state_dir`.

## HTTP API

The localhost-only Legion API lives in `api.ts`.

| Surface | Purpose |
| --- | --- |
| `GET /legion/v1/state` | Read redacted durable Legion state. |
| `POST /legion/v1/process/started` | Register a root process with transcript-derived architect role backing. |
| `POST /legion/v1/process/exit` | Authenticated architect exit that releases an admission slot or marks its root process dead. |
| `POST /legion/v1/issues`, `/waves/release`, `/issues/comment`, `/issues/body`, `/issues/labels`, `/issues/close` | Scoped architect writes. |
| `POST /legion/v1/worker-session`, `/grants`, `/git-credential`, `/gh-token` | Durable architect and worker capability recovery, and credential grants. |
| `POST /legion/v1/worker/started` | Registers a headless phase worker's session against its daemon-minted boot token; mints its session capability, git identity lease, and records its tmux/socket locator and a hash of its boot token on the claim (so `/worker-session` can rebind it after a daemon restart). |
| `POST /legion/v1/worker/ready` | Verifies the worker's session capability and sends its queued assignment (`pendingAssignment`, cleared once sent) as an OMP RPC `prompt` frame over the worker's `legion worker-shim` socket. |
| `POST /legion/v1/worker/spawn` | Architect-only: spawns a phase worker for `{issue, role, task}` — resumes an already-live worker over its socket (`resumed`), respawns a dead one with `--resume` (`spawned`), opens a fresh pane (`spawned`), or — at the configured running-worker cap — enqueues the task in FIFO order for promotion once a slot frees (`queued`). |
| `POST /legion/v1/phase/complete` | Authenticates via a short-lived grant (`{grantId, summary}`, resolved read-only — the same grant mechanism `/git-credential` and `/gh-token` use, never a live session secret) rather than a session capability. Verifies the claim for the grant's (issue, role) still belongs to the grant's session (409 `Grant does not match the worker currently holding this role` otherwise) and that the issue's active phase still matches (409 `Phase for <issue> is no longer owned by this worker` otherwise), then captures and clears the phase synchronously — before the publish `await`, so a second concurrent completion for the same phase always 409s on that check instead of both publishing — and publishes `{type:"phase-complete", issue, role, summary}` to the tree's architect role. A non-404 publish failure restores the captured phase and returns 502 (idempotent retry). A 404 no-holder never drops the completion: the phase is recorded as `phases[issue].completed = {summary, at}` instead of cleared, and the response is 202 — `overseerCatchup` replays it to a reconnecting architect, and `routeActive` treats a completed phase as no active phase (routes to the architect) until a fresh assignment overwrites the record. A save failure restores the captured phase and returns 500 (retry redoes the whole attempt). |
| `POST /legion/v1/controller/ready`, `/gates/approve`, `/admission`, `/backlog` | Controller lifecycle and control-plane actions. |

## Files

| File | Responsibility |
| --- | --- |
| `index.ts` | Boots state, core-NATS intake, process manager, API, resync, linger expiry, and signal persistence. |
| `config.ts` | Validates file and environment lifecycle configuration. |
| `events.ts` | Routes raw webhook envelopes through pure reducers and executes effects; the core-NATS role lanes propagate a publish failure straight to the caller (nothing is held for redelivery — a missed wake is recovered by resuming the worker/controller with a state-derived catch-up, never a replay), while the durable JetStream lane dispatches every effect and saves before acking, going fatal (not nak) on any failure past the reducer. A 404 no-holder recovery (`onUndeliverable`) runs only after that save commits, best-effort, so the durable transaction stays a single state mutation. |
| `processes.ts` | Admission, tmux root/controller spawning, worker exception recovery, resurrection, linger, and tmux window/pane reconciliation. |
| `worker-boot-watchdog.ts` | Watches a freshly-launched worker's boot against `worker_boot_timeout_seconds`, probing liveness before ever retiring an unconfirmed boot — never a hard SLA, an observation interval that re-arms on any sign of life. |
| `worker-admission.ts` | Owns the running-worker cap: the admission decision, the FIFO queue, the reservation set covering a decision-to-effect gap, and the promotion drain — `processes.ts` calls into it for every admission decision instead of holding this logic itself. |
| `api.ts` | Localhost extension/controller write surface and session-bound credential grants. |
| `legion-state.ts` | Strict versioned state schema and atomic persistence. |
| `catchup.ts` | Derived overseer and worker catch-up payloads. |
| `resync.ts` | Low-frequency board convergence, re-emitted triage for unadmitted tracked roots, and residual anomaly reporting. |
| `approval-check.ts` | Human approval status backstop for the current PR head. |
| `worker-rpc.ts` | Minimal OMP RPC protocol v2 client (`negotiate_protocol`/`prompt`/`get_state`/`shutdown`) reached through a worker's `legion worker-shim` unix socket rather than a spawned process's stdio. |
| `tmux.ts` | Pure tmux command construction/parsing (open/split a window, probe pane liveness and pid, kill a window or a single pane, list a session's unknown owned windows and its unrecorded worker-shim panes) over an injected `run` callback — no daemon state. |

## Operational invariants

- Role lanes use core NATS; the daemon holds no events. A role-lane delivery exception or a durable lane's 404 no-holder probes the worker's own locator and, if dead, resumes the same agent through it (`--resume`, never fresh) and sends it a state-derived catch-up instead of replaying the missed event; the controller is a role holder like any other, so a controller exception calls `ensureController` the same way.
- Root processes and the controller are tmux windows under global admission control. The daemon stores tmux window IDs; names are cosmetic, escaped issue labels.
- Process failure recovery is exception-driven. A root is trusted only when its recorded window's pane is live and running OMP; a dead root is resurrected under a generation lock.
- A lingering or closed root releases its admission slot, gracefully stops every process recorded for the tree (its own and every worker's — a `{type:"shutdown"}` frame over each one's `legion worker-shim` socket, its pane killed directly only if it fails to close within the configured timeout, or immediately if its shim is unreachable or already confirmed dead), clears its locator, and removes its role claims. The linger sweep also removes session windows not recorded by a tree or controller.
- `DaemonConfig` supplies all lifecycle defaults: admission cap, worker cap, recursion depth, linger duration, resync interval, retry limit, the two stop timeouts (`workerStopTimeoutSeconds`, default 10; `treeStopTimeoutSeconds`, default 60), and worker boot timeout (`worker_boot_timeout_seconds`/`LEGION_WORKER_BOOT_TIMEOUT_SECONDS`, default 120).
- Boot never resurrects trees; it only reconciles admission so slots opened by a raised cap promote queued issues in order.

- Phase workers (planner/implementer/tester/reviewer/merger, and sub-architects for child issues) run headless — `omp --mode rpc` behind a `legion worker-shim` process — one tmux window per issue, one pane per worker. The issue's first worker opens the window; every later worker on that issue splits into it. Closing a tree stops every process gracefully through its own shim socket first (never SIGTERM), each one's pane killed directly only if it fails to close within the timeout — so a sibling worker that shuts down cleanly in the same window is never collaterally killed by one that hangs.
- `spawn_worker` always resumes the same agent for an existing phase: a live claim below the running-worker cap gets the new task prompted directly over its socket (`resumed`) — at the cap, an idle-live claim's task queues instead without touching its locator (see the cap bullet below); a claim whose boot has not yet been confirmed by `/worker/started` gets its task queued as `pendingAssignment` without launching a second pane (`resumed`); a dead claim with a recorded OMP session file respawns with `--resume`; only a role with no claim at all gets a genuinely fresh spawn. `/worker/ready` only delivers a queued task when both the caller's session id and generation match the claim's current ones, so a stale respawned session can never drain a newer claim's pending assignment. Both the resumed-socket prompt and `/worker/ready`'s queued-assignment delivery re-register `state.phases[issue]` for this phase.
- `config.workerCap` bounds only running or currently-prompted phase workers and sub-architects — never the root architect or controller, which are admitted separately via `state.trees`/`state.controllerLocator`. At the cap, a task for a role whose pane is already live and idle (between tasks, not dead) queues in `state.workerAdmission.queue` without touching its locator, instead of prompting it past the cap; a role with no live pane queues the same way, its claim's locator cleared. Each promotion re-derives the running count fresh and re-checks the current cap under a single admission lock before deciding, so a queued idle-live worker is prompted in place once a slot frees and a queued cold claim launches fresh — both promoted in FIFO order, one at a time.
- A queued idle-resume prompt (see above) that keeps rejecting is not retried forever: an ordinary rejection restores the client's run state to idle silently (never firing its idle trigger — a transition-fired restore would retrigger promotion synchronously mid-rejection, an unbounded retry storm) and counts against `promptFailures`, mirroring `launchFailures`. At the same threshold, the worker is retired (pane killed, locator cleared) so the still-queued assignment falls through to a fresh cold launch on the next drain — a worker that keeps refusing prompts is broken, and replacing it is the recovery. A successful prompt resets `promptFailures` to 0.
- `closeTree` tracks in-flight teardown only in memory (`ProcessManager`'s `closingTrees` map), never in the persisted `TreeState.status` — a crash mid-close leaves the durable status exactly where it was (`active`/`lingering`), so boot never needs a dedicated recovery path; the periodic linger sweep simply retries. `spawnWorker`/`workerReady` reject a closing tree with `TreeClosingError` (409); a process `closeTree` cannot confirm stopped (a real `kill-pane` failure, not the routine "already gone" race) raises `StopFailed` (502), keeps that claim/locator untouched, and leaves the tree `lingering` instead of `closed` for the sweep to retry.
- A freshly-launched worker's boot is watched (`WorkerBootWatchdog.arm`) against `worker_boot_timeout_seconds`, but that value is an observation interval, never a hard SLA: at each interval, a boot still unconfirmed by `/worker/started` is probed (its tmux pane's pid, or a reachable/answering shim socket) before anything happens — a live pane or socket just re-arms the watch for another interval, touching neither the claim nor `launchFailures`, however many intervals pass. Only a boot whose pane is gone *and* whose socket refuses a connection is retired, counted as a launch failure, and either retried (same pending task, `--resume`) or escalated to `worker-died` at `MAX_LAUNCH_FAILURES`. `launchFailures` itself is reset only by a confirmed `/worker/started`, never merely by reopening a pane — a boot that keeps launching but never completing must still reach the threshold.

## OMP invocation and daemon tools

Set `omp_invocation` in `legion.yaml` or `LEGION_OMP_INVOCATION` to the required `mise x <tool> -- omp` command that launches the root architect and controller. The default is:

```sh
mise x github:sjawhar/oh-my-pi@18.1.15-sami.20260908-220934 -- omp
```

Launch the daemon normally with `bun run ...`, not inside that scoped `mise x` command. At startup it obtains the complete `mise env --json` environment, resolves absolute `jj`, `git`, `gh`, and `tmux` paths, and uses `mise where` to turn the default invocation into the pinned OMP binary path. All daemon subprocesses use those absolute paths; root and controller panes receive the complete `PATH` and execute that same OMP path.

Every root, worker, and controller pane also receives `DISPATCH_URL` and `DISPATCH_TOKEN` when
`dispatch_url` is configured: `DISPATCH_URL` is the configured service base URL (no `/mcp` suffix),
and `DISPATCH_TOKEN` is read from the `DISPATCH_TOKEN` environment variable (required whenever
`dispatch_url` is set — `resolveDaemonConfig` refuses to start otherwise) so each pane's native
dispatch tool can authenticate and register. Neither variable is exported when `dispatch_url` is
unset; those panes fall back to their own `envoy.json` dispatch config. The daemon never emits the
retired `DISPATCH_MCP_URL` alias and strips it from every child process it spawns, pane or
otherwise.

Before loading state, opening core NATS, or serving the API, the daemon probes the exact resolved OMP executable with an isolated extension and refuses startup unless it confirms `pi.agents`. It also refuses startup with every missing required tool listed. Set `LEGION_MISE_PATH`, `LEGION_JJ_PATH`, `LEGION_GIT_PATH`, `LEGION_GH_PATH`, `LEGION_TMUX_PATH`, or `LEGION_OMP_PATH` to an absolute executable path to override discovery. The `mise x <tool> -- omp` form is required for `omp_invocation`; set `LEGION_OMP_PATH` when selecting a direct OMP binary.

Before loading state, opening core NATS, or serving the API, the daemon also reads the installed `@sjawhar/pi-legion-envoy` plugin's manifest (`~/.omp/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json`, or the active `OMP_PROFILE`/`PI_PROFILE`'s profile root, or an existing `$XDG_DATA_HOME/omp` root — the same precedence OMP's own `DirResolver` uses) and refuses startup unless `omp.extensions` lists `dist/legion.js`. The daemon never passes `--extension` to spawned sessions; they load `envoy.ts`/`legion.ts` solely from this installed plugin, so a version that doesn't ship `dist/legion.js` would silently spawn sessions with no Legion tooling. The required version is `legion.minPluginVersion` in `package.json`.
