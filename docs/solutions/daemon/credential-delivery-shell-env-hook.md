---
title: "shell.env credential delivery: JIT pull replaces per-role serve processes"
category: daemon
tags:
  - credentials
  - github-apps
  - shell-env
  - token-management
  - plugin
  - architecture
date: 2026-04-12
status: active
module: daemon
related_issues:
  - "445"
symptoms:
  - "Workers need per-org GitHub App tokens"
  - "multi-serve.ts memory overhead"
  - "Token expired during long-running worker session"
---

# shell.env Credential Delivery: JIT Pull Replaces Per-Role Serve Processes

## Architecture Before (#445)

Each GitHub App role (`impl`, `review`) ran its own `opencode serve` process via
`RoleServeManager` in `multi-serve.ts`. Credentials were baked into the serve's
environment at startup. Workers were routed to the appropriate serve based on their
mode.

**Problems:**
- N roles × 1 serve process each = wasted memory for light workloads
- Tokens baked at startup expired after 1 hour with no refresh
- Single `installationId` per role — couldn't support multiple GitHub orgs
- Per-role serves needed health monitoring and restart logic

## Architecture After (#445)

Single shared serve + per-session JIT credential injection:

```
legion.yaml (config)
  → daemon/config.ts (parse installations map: org → installationId)
    → daemon/github-apps.ts (TokenManager.getToken(role, owner))
      → daemon/server.ts (GET /workers/:id/token endpoint)
        → opencode-plugin/hooks/github-app-credentials.ts (shell.env hook)
```

**Flow:**
1. Worker's shell command triggers `shell.env` hook in the plugin
2. Hook identifies worker via `sessionID` (primary) or `cwd` (fallback)
3. Hook calls `GET /workers/:workerId/token` on the daemon
4. Daemon resolves role from worker mode, owner from worker `repo` field
5. `TokenManager.getToken(role, owner)` returns fresh or cached token
6. Hook injects `GH_TOKEN`, git identity, `LEGION_APP_ROLE` into shell env

**Key insight:** Credentials are needed at shell command execution time, not session
creation time. The shell.env hook fires per-command, so tokens are always fresh.

## Owner-Aware Token Caching

Cache key changed from `role` to `${role}:${owner}`:

```typescript
// Before: one token per role
cache.get(role)

// After: one token per role + org
cache.get(`${role}:${owner}`)
```

The `pending` map for concurrent request dedup also uses the compound key.

**Why:** A daemon may serve workers across multiple GitHub orgs (e.g., `acme` and
`globex`). Each org has a different installation ID and therefore a different token.

## Two-Layer Caching (Intentional)

1. **Daemon layer** (`TokenManager`): Caches per `role:owner`, refreshes 5 min before
   expiry. Prevents redundant GitHub API calls across all workers in the same org.
2. **Plugin layer** (shell.env hook): Caches per `workerId`, refreshes 5 min before
   expiry. Prevents redundant daemon HTTP calls within a single session.

Both layers are needed — the daemon cache deduplicates across workers, the plugin
cache deduplicates within a session.

## Token Endpoint Design

`GET /workers/:id/token` returns:

```json
{
  "role": "implement",
  "owner": "acme",
  "expiresAt": "2026-04-12T15:00:00.000Z",
  "env": {
    "GH_TOKEN": "ghs_...",
    "GIT_AUTHOR_NAME": "legion-implement[bot]",
    "GIT_AUTHOR_EMAIL": "...",
    "GIT_COMMITTER_NAME": "legion-implement[bot]",
    "GIT_COMMITTER_EMAIL": "...",
    "LEGION_APP_ROLE": "implement"
  }
}
```

**Error codes:** 404 for "not configured" (not 500) — the endpoint is optional
infrastructure. A 404 tells the plugin "this feature isn't available here" and the
plugin no-ops gracefully.

## OPENCODE_CONFIG_CONTENT

The shared serve receives `OPENCODE_CONFIG_CONTENT` as an env var to auto-load the
Legion plugin:

```typescript
OPENCODE_CONFIG_CONTENT: JSON.stringify({ plugin: ["@sjawhar/opencode-legion@latest"] })
```

**Coupling note:** If the plugin package name changes, `buildServeEnv()` in
`daemon/index.ts` must be updated.

**Open question:** Whether `OPENCODE_CONFIG_CONTENT` merges additively with the
user's global config or replaces it was not verified in production.

## Testing Patterns

### shell.env Hook Testing

Use the factory pattern with injected `fetchFn` and `now`:

```typescript
const hook = createGitHubAppCredentialsHook({
  fetchFn: mockFetch as unknown as typeof fetch,
  now: () => simulatedTime,
});
```

The `credentialCache` is module-level. Tests that check caching behavior must
control `now()` to simulate time passing.

**Bun mock() + typeof fetch:** Bun's `mock()` type lacks `preconnect` required by
newer `typeof fetch`. Use `as unknown as typeof fetch` double-cast.

### Worker Identification Fallback

The hook identifies workers by:
1. `input.sessionID` matched against `worker.sessionId` (exact)
2. `input.cwd.startsWith(worker.workspace)` (fallback)

The cwd fallback exists because the OpenCode plugin SDK's `shell.env` input type
may not include `sessionID` in all versions. Both paths are tested.

## Adding New Credential Types

1. Add a daemon endpoint: `GET /workers/:id/<credential-type>`
2. Add a `shell.env` hook that calls the endpoint
3. The hook should: check for `LEGION_DAEMON_PORT`, find worker, cache with refresh
   window, no-op on any error
4. The endpoint should: return 404 when feature isn't configured

## Dead Code Note

After multi-serve removal, `buildRoleEnv()` and `SCRUBBED_ENV_KEYS`/
`SCRUBBED_ENV_PREFIX` in `github-apps.ts` are dead code — no production callers
remain. Cleanup deferred to a follow-up.
