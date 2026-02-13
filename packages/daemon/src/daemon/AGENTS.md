# Daemon Module

HTTP server + worker process management. Spawns `opencode serve` instances, tracks their lifecycle, exposes REST API for the controller skill.

## HTTP API (server.ts)

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/health` | `{status, uptime, workerCount}` |
| `GET` | `/workers` | List all `WorkerEntry[]` |
| `POST` | `/workers` | Spawn worker — `{issueId, mode, workspace, env?}` → `{id, port, sessionId}` |
| `GET` | `/workers/:id` | Single worker details |
| `PATCH` | `/workers/:id` | Update status (`running`, `dead`) |
| `DELETE` | `/workers/:id` | Kill process, release port, remove |
| `GET` | `/workers/:id/status` | Proxy to worker's OpenCode `/session/status` |
| `POST` | `/shutdown` | Graceful shutdown — kill all, persist state |

**Worker ID format:** `{issueId}-{mode}` lowercase (e.g., `eng-21-implement`)

## Files

| File | Responsibility |
|------|---------------|
| `index.ts` | Daemon lifecycle: `startDaemon()`, health tick loop, signal handlers. Uses DI via `DaemonDependencies` interface for testability. |
| `server.ts` | HTTP routing, request validation, in-memory worker map. Imports `computeSessionId` from `../state/types` (only cross-module dep). |
| `serve-manager.ts` | `spawnServe()` — runs `opencode serve --port X`. `killWorker()` — SIGTERM. `healthCheck()` — `/global/health`. `adoptExistingWorkers()` — restore from state file. |
| `config.ts` | `DaemonConfig` interface, `loadConfig()` reads env vars. Defaults: daemon port 13370, worker base port 13381, check interval 60s. |
| `state-file.ts` | `readStateFile()` / `writeStateFile()` — atomic JSON persistence to `~/.legion/{teamId}/workers.json`. |
| `ports.ts` | `PortAllocator` class — sequential allocation from base port, tracks allocated set. Seeded from existing workers on startup. |

## Key Patterns

- **DI in `startDaemon()`** — all deps passed via `overrides` param, enabling unit tests without spawning real processes
- **Health tick** — every 60s, checks each worker's `/global/health`, marks dead, cleans up
- **State persistence** — worker map persisted to disk, restored on daemon restart via `adoptExistingWorkers()`
- **Session IDs** — deterministic UUIDv5 from `computeSessionId(teamId, issueId, mode)`, enables idempotent worker spawning
