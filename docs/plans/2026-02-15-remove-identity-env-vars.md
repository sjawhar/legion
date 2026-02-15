# Remove Redundant Identity Env Vars from Worker Spawn

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Remove 5 unused identity env vars from the worker spawn path in `server.ts`, since workers get their identity from the controller prompt, not env vars.

**Architecture:** The POST `/workers` handler currently passes `LINEAR_ISSUE_ID`, `LINEAR_TEAM_ID`, `LEGION_DIR`, `LEGION_SHORT_ID`, `LEGION_DAEMON_PORT` to `spawnServe()`. Workers never read these. Remove them, keeping caller-provided `env` pass-through intact. Controller spawn in `index.ts` is unchanged (controller skill reads those vars).

**Tech Stack:** TypeScript, Bun

---

### Task 1: Remove identity env vars from POST /workers handler — Independent

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts:243-250`

**Step 1: Edit the env object in server.ts**

In `packages/daemon/src/daemon/server.ts`, find the `spawnServe` call in the POST `/workers` handler (around line 236). The current `env` property is:

```typescript
                env: {
                  LINEAR_ISSUE_ID: issueId,
                  LINEAR_TEAM_ID: opts.teamId,
                  LEGION_DIR: opts.legionDir,
                  LEGION_SHORT_ID: opts.shortId,
                  LEGION_DAEMON_PORT: String(server.port),
                  ...(env as Record<string, string> | undefined),
                },
```

Delete the 5 identity var lines, leaving only the caller-provided env spread:

```typescript
                env: {
                  ...(env as Record<string, string> | undefined),
                },
```

This is a pure deletion — no restructuring. When caller provides `env` in the POST body, it passes through. When not provided, `...(undefined)` is a no-op, resulting in `env: {}`. Both are handled correctly by `serve-manager.ts` which spreads `opts.env` into the process env.

**DO NOT modify:**
- `packages/daemon/src/daemon/index.ts` — Controller spawn (lines 294-299) retains identity vars because the controller skill reads them
- `packages/daemon/src/daemon/serve-manager.ts` — Pass-through only, no changes needed

**Step 2: Run lint**

Run: `bunx biome check packages/daemon/src/daemon/server.ts`
Expected: No errors

**Step 3: Run type check**

Run: `bunx tsc --noEmit`
Expected: Exit 0, no type errors

**Step 4: Run tests**

Run: `bun test`
Expected: All tests pass. Key test: `server.test.ts` "creates workers" uses `toMatchObject({ env: { DEBUG: "1" } })` which is subset matching — still passes when identity vars are removed.

**Step 5: Commit**

```bash
jj describe -m "LEG-127: remove redundant identity env vars from worker spawn"
jj git push
```

### Verification Checklist

- [ ] `packages/daemon/src/daemon/server.ts` no longer explicitly passes `LINEAR_ISSUE_ID`, `LINEAR_TEAM_ID`, `LEGION_DIR`, `LEGION_SHORT_ID`, `LEGION_DAEMON_PORT` to `spawnServe()`
- [ ] Caller-provided `env` from POST body still passed through via `...(env as Record<string, string> | undefined)`
- [ ] `packages/daemon/src/daemon/index.ts` controller spawn unchanged (lines 294-299)
- [ ] `packages/daemon/src/daemon/serve-manager.ts` unchanged
- [ ] `bunx biome check src/` — exit 0
- [ ] `bunx tsc --noEmit` — exit 0
- [ ] `bun test` — all pass
