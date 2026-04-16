# Daemon Module

HTTP server + shared `opencode serve` instance. One long-lived serve process handles all worker and controller sessions. Exposes REST API for the controller skill.

## HTTP API (server.ts)

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/health` | `{status, uptime, workerCount}` |
| `GET` | `/workers` | List all `WorkerEntry[]` (controller NOT included — tracked separately) |
| `POST` | `/workers` | Create session on shared serve — `{issueId, mode, workspace}` → `{id, port, sessionId}` |
| `GET` | `/workers/:id` | Single worker details |
| `PATCH` | `/workers/:id` | Update status (`running`, `dead`) |
| `DELETE` | `/workers/:id` | Remove tracking (session goes idle naturally) |
| `GET` | `/workers/:id/status` | Proxy to worker's OpenCode `/session/status` |
| `POST` | `/workers/prune` | Bulk-remove workers + crash history by issue ID — `{issueIds: string[]}` → `{pruned, crashHistoryPruned}` |
| `POST` | `/shutdown` | Graceful shutdown — stop shared serve, persist state |
| `POST` | `/restart` | Graceful restart — stop daemon but keep serve alive for session continuity |
| `GET` | `/dashboard` | Aggregated worker summary grouped by repo+issue, with activity, stats, and recent events |
| `GET` | `/dashboard/ui` | Single-page HTML dashboard — fetches `/dashboard` JSON, auto-refreshes 30s, responsive |
| `GET`    | `/state/track`          | List tracked issue IDs — `{trackedIssues: string[]}` |
| `POST`   | `/state/track`          | Manually track an issue — `{issueId}` → `{tracked: true}` |
| `DELETE` | `/state/track/:issueId` | Manually untrack an issue → `{untracked: true}` |
| `GET`    | `/state/materialized`   | Tracked issues from cache + new-issues accumulator (resets on read) — `{issues, titles, newIssues}` |

**Worker ID format:** `{issueId}-{mode}` lowercase (e.g., `eng-21-implement`)

**Controller:** The controller is NOT listed in `/workers` responses. It runs as a session on the shared serve, tracked separately in the state file's `controller` field.

## Files

| File | Responsibility |
|------|---------------|
| `index.ts` | Daemon lifecycle: `startDaemon()`, shared serve startup, health tick loop, signal handlers. Controller creates a session on shared serve. Uses DI via `DaemonDependencies` interface for testability. |
| `server.ts` | HTTP routing, request validation, in-memory worker map. `POST /workers` creates session on shared serve (instant). `DELETE /workers` removes tracking only. |
| `dashboard-ui.ts` | `getDashboardHtml()` — single-page HTML/CSS/JS dashboard served at `/dashboard/ui`. No framework; inline styles and vanilla JS. |
| `serve-manager.ts` | `spawnSharedServe()` — runs one `opencode serve`. `waitForHealthy()` — polls readiness. `createSession()` — creates session on shared serve. `stopServe()` — graceful shutdown. `healthCheck()` — `/global/health`. |
| `config.ts` | `DaemonConfig` interface, `loadConfig()` reads env vars. Defaults: daemon port 13370, shared serve port 13381 (`baseWorkerPort`), check interval 60s. **Controller mode:** `LEGION_CONTROLLER_SESSION_ID` env var (optional, must start with `ses_` if set, hard fails on invalid format). |
| `state-file.ts` | `readStateFile()` / `writeStateFile()` — atomic JSON persistence to `~/.legion/{legionId}/workers.json`. Includes `controller?: ControllerState` field for controller lifecycle. Legacy `controller-controller` worker entries are stripped on read. |
| `ports.ts` | `isPortFree()` utility only. `PortAllocator` removed — all workers share one port. |
| `repo-manager.ts` | Repo clone management: `ensureRepoClone()`, `ensureWorkspace()`, `startBackgroundFetch()`, `fetchAllTrackedRepos()`, `cleanupWorkspace()`, `verifyBranchPushed()`. |

## Key Patterns

- **Shared serve** — one `opencode serve` process serves all sessions. Spawned on daemon startup, shared by workers and controller.
- **DI in `startDaemon()`** — all deps passed via `overrides` param, enabling unit tests without spawning real processes
- **Health tick** — every 60s, checks shared serve health. On failure: restart serve, re-create sessions for active workers.
- **State persistence** — worker map persisted to disk, sessions re-created on daemon restart using deterministic IDs.
- **Session IDs** — deterministic UUIDv5 from `computeSessionId(legionId, issueId, mode)`, enables idempotent session creation.
- **Controller lifecycle** — runs as session on shared serve. For external mode, daemon stores session ID without spawning.
- **Repo clone freshness** — implement dispatches do a blocking `jj git fetch` before workspace creation. Other modes fetch non-blocking after workspace creation. Done issue cleanup triggers a background fetch. Daemon startup warms all tracked clones via `fetchAllTrackedRepos()`.
