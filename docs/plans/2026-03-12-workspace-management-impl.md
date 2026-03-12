# First-Class Workspace Management — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Isolate Legion workspaces from personal jj repos, support multiple repos per project, use XDG-compliant directories, rename "team" to "legion."

**Architecture:** New modules for XDG paths, legions registry, and repo management sit below the daemon. The server's `POST /workers` delegates to repo-manager for cloning and workspace creation. CLI dispatch becomes a thin client passing `{issueId, mode, repo}`. `legions.json` tracks running daemon instances for multi-legion port discovery.

**Tech Stack:** TypeScript/Bun, jj CLI for VCS, zod for validation, Bun test for TDD.

**Design Doc:** `docs/plans/2026-03-12-workspace-management-design.md`

---

## Task 1: XDG Path Resolution

New module that computes all Legion directory paths from XDG environment variables.

**Files:**
- Create: `packages/daemon/src/daemon/paths.ts`
- Create: `packages/daemon/src/daemon/__tests__/paths.test.ts`

**Step 1: Write failing tests**

```typescript
// packages/daemon/src/daemon/__tests__/paths.test.ts
import { describe, expect, it } from "bun:test";
import { resolveLegionPaths } from "../paths";

describe("resolveLegionPaths", () => {
  it("uses XDG defaults when env vars unset", () => {
    const paths = resolveLegionPaths({}, "/home/testuser");
    expect(paths.dataDir).toBe("/home/testuser/.local/share/legion");
    expect(paths.stateDir).toBe("/home/testuser/.local/state/legion");
    expect(paths.reposDir).toBe("/home/testuser/.local/share/legion/repos");
    expect(paths.workspacesDir).toBe("/home/testuser/.local/share/legion/workspaces");
    expect(paths.legionsFile).toBe("/home/testuser/.local/state/legion/legions.json");
  });

  it("respects XDG_DATA_HOME", () => {
    const paths = resolveLegionPaths({ XDG_DATA_HOME: "/custom/data" }, "/home/testuser");
    expect(paths.dataDir).toBe("/custom/data/legion");
    expect(paths.reposDir).toBe("/custom/data/legion/repos");
  });

  it("respects XDG_STATE_HOME", () => {
    const paths = resolveLegionPaths({ XDG_STATE_HOME: "/custom/state" }, "/home/testuser");
    expect(paths.stateDir).toBe("/custom/state/legion");
    expect(paths.legionsFile).toBe("/custom/state/legion/legions.json");
  });

  it("computes legion-specific paths", () => {
    const paths = resolveLegionPaths({}, "/home/testuser");
    const legion = paths.forLegion("sjawhar/42");
    expect(legion.legionStateDir).toBe("/home/testuser/.local/state/legion/legions/sjawhar/42");
    expect(legion.workersFile).toBe("/home/testuser/.local/state/legion/legions/sjawhar/42/workers.json");
    expect(legion.logDir).toBe("/home/testuser/.local/state/legion/legions/sjawhar/42/logs");
    expect(legion.workspacesDir).toBe("/home/testuser/.local/share/legion/workspaces/sjawhar/42");
  });

  it("computes repo clone path", () => {
    const paths = resolveLegionPaths({}, "/home/testuser");
    expect(paths.repoClonePath("github.com", "acme", "widgets")).toBe(
      "/home/testuser/.local/share/legion/repos/github.com/acme/widgets"
    );
  });
});
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/paths.test.ts`
Expected: FAIL — module not found

**Step 3: Implement paths module**

```typescript
// packages/daemon/src/daemon/paths.ts
import path from "node:path";

export interface LegionPaths {
  dataDir: string;
  stateDir: string;
  reposDir: string;
  workspacesDir: string;
  legionsFile: string;
  forLegion(projectId: string): LegionInstancePaths;
  repoClonePath(host: string, owner: string, repo: string): string;
}

export interface LegionInstancePaths {
  legionStateDir: string;
  workersFile: string;
  logDir: string;
  workspacesDir: string;
}

export function resolveLegionPaths(
  env: Record<string, string | undefined>,
  homeDir: string,
): LegionPaths {
  const dataHome = env.XDG_DATA_HOME ?? path.join(homeDir, ".local", "share");
  const stateHome = env.XDG_STATE_HOME ?? path.join(homeDir, ".local", "state");

  const dataDir = path.join(dataHome, "legion");
  const stateDir = path.join(stateHome, "legion");
  const reposDir = path.join(dataDir, "repos");
  const workspacesDir = path.join(dataDir, "workspaces");
  const legionsFile = path.join(stateDir, "legions.json");

  return {
    dataDir,
    stateDir,
    reposDir,
    workspacesDir,
    legionsFile,
    forLegion(projectId: string): LegionInstancePaths {
      const legionStateDir = path.join(stateDir, "legions", projectId);
      return {
        legionStateDir,
        workersFile: path.join(legionStateDir, "workers.json"),
        logDir: path.join(legionStateDir, "logs"),
        workspacesDir: path.join(workspacesDir, projectId),
      };
    },
    repoClonePath(host: string, owner: string, repo: string): string {
      return path.join(reposDir, host, owner, repo);
    },
  };
}
```

**Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/paths.test.ts`
Expected: PASS

**Step 5: Commit**

```bash
jj describe -m "feat: add XDG path resolution module for Legion directories"
jj new
```

---

## Task 2: Legions Registry

New module for reading/writing `legions.json` — tracks running daemon instances with ports.

**Files:**
- Create: `packages/daemon/src/daemon/legions-registry.ts`
- Create: `packages/daemon/src/daemon/__tests__/legions-registry.test.ts`
- Modify: `packages/daemon/src/daemon/schemas.ts` — add `LegionEntrySchema`

**Step 1: Write failing tests**

```typescript
// packages/daemon/src/daemon/__tests__/legions-registry.test.ts
import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  readLegionsRegistry,
  writeLegionEntry,
  removeLegionEntry,
  allocatePort,
  findLegionByProjectId,
} from "../legions-registry";

describe("legions-registry", () => {
  let tempDir: string | null = null;

  afterEach(async () => {
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  it("returns empty registry when file missing", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const registry = await readLegionsRegistry(path.join(tempDir, "legions.json"));
    expect(registry).toEqual({});
  });

  it("writes and reads a legion entry", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");
    const entry = { port: 13370, servePort: 13381, pid: 1234, startedAt: "2026-03-12T00:00:00Z" };

    await writeLegionEntry(filePath, "sjawhar/42", entry);
    const registry = await readLegionsRegistry(filePath);

    expect(registry["sjawhar/42"]).toEqual(entry);
  });

  it("removes a legion entry", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");
    await writeLegionEntry(filePath, "sjawhar/42", {
      port: 13370, servePort: 13381, pid: 1234, startedAt: "2026-03-12T00:00:00Z",
    });

    await removeLegionEntry(filePath, "sjawhar/42");
    const registry = await readLegionsRegistry(filePath);

    expect(registry["sjawhar/42"]).toBeUndefined();
  });

  it("allocates next available port", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");
    await writeLegionEntry(filePath, "proj/1", {
      port: 13370, servePort: 13381, pid: 1234, startedAt: "2026-03-12T00:00:00Z",
    });

    const registry = await readLegionsRegistry(filePath);
    const { daemonPort, servePort } = allocatePort(registry);

    expect(daemonPort).toBe(13371);
    expect(servePort).toBe(13382);
  });

  it("reclaims port from dead PID", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");
    // Use PID 999999999 which should not exist
    await writeLegionEntry(filePath, "proj/1", {
      port: 13370, servePort: 13381, pid: 999999999, startedAt: "2026-03-12T00:00:00Z",
    });

    const registry = await readLegionsRegistry(filePath);
    const { daemonPort } = allocatePort(registry);

    expect(daemonPort).toBe(13370); // Reclaimed
  });

  it("findLegionByProjectId returns entry when it exists", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-reg-"));
    const filePath = path.join(tempDir, "legions.json");
    await writeLegionEntry(filePath, "sjawhar/42", {
      port: 13370, servePort: 13381, pid: process.pid, startedAt: "2026-03-12T00:00:00Z",
    });

    const entry = await findLegionByProjectId(filePath, "sjawhar/42");
    expect(entry).toBeDefined();
    expect(entry!.port).toBe(13370);
  });
});
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/legions-registry.test.ts`
Expected: FAIL — module not found

**Step 3: Add schema**

Add `LegionEntrySchema` to `packages/daemon/src/daemon/schemas.ts`:

```typescript
export const LegionEntrySchema = z.object({
  port: z.number(),
  servePort: z.number(),
  pid: z.number(),
  startedAt: z.string(),
});

export const LegionsRegistrySchema = z.record(z.string(), LegionEntrySchema);
```

**Step 4: Implement legions-registry module**

```typescript
// packages/daemon/src/daemon/legions-registry.ts
import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import path from "node:path";
import { LegionsRegistrySchema } from "./schemas";

export interface LegionEntry {
  port: number;
  servePort: number;
  pid: number;
  startedAt: string;
}

export type LegionsRegistry = Record<string, LegionEntry>;

const BASE_DAEMON_PORT = 13370;
const BASE_SERVE_PORT = 13381;

function isPidAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

export async function readLegionsRegistry(filePath: string): Promise<LegionsRegistry> {
  try {
    const raw = await readFile(filePath, "utf-8");
    if (!raw.trim()) return {};
    const parsed = JSON.parse(raw);
    const result = LegionsRegistrySchema.safeParse(parsed);
    return result.success ? result.data : {};
  } catch (error) {
    const err = error as NodeJS.ErrnoException;
    if (err.code === "ENOENT") return {};
    throw error;
  }
}

export async function writeLegionEntry(
  filePath: string,
  projectId: string,
  entry: LegionEntry,
): Promise<void> {
  const registry = await readLegionsRegistry(filePath);
  registry[projectId] = entry;
  await writeRegistry(filePath, registry);
}

export async function removeLegionEntry(filePath: string, projectId: string): Promise<void> {
  const registry = await readLegionsRegistry(filePath);
  delete registry[projectId];
  await writeRegistry(filePath, registry);
}

export function allocatePort(
  registry: LegionsRegistry,
): { daemonPort: number; servePort: number } {
  const usedDaemonPorts = new Set<number>();
  const usedServePorts = new Set<number>();

  for (const entry of Object.values(registry)) {
    if (isPidAlive(entry.pid)) {
      usedDaemonPorts.add(entry.port);
      usedServePorts.add(entry.servePort);
    }
  }

  let daemonPort = BASE_DAEMON_PORT;
  while (usedDaemonPorts.has(daemonPort)) daemonPort++;

  let servePort = BASE_SERVE_PORT;
  while (usedServePorts.has(servePort)) servePort++;

  return { daemonPort, servePort };
}

export async function findLegionByProjectId(
  filePath: string,
  projectId: string,
): Promise<LegionEntry | undefined> {
  const registry = await readLegionsRegistry(filePath);
  return registry[projectId];
}

async function writeRegistry(filePath: string, registry: LegionsRegistry): Promise<void> {
  const dir = path.dirname(filePath);
  await mkdir(dir, { recursive: true });
  const tempPath = `${filePath}.tmp-${process.pid}-${Date.now()}`;
  await writeFile(tempPath, JSON.stringify(registry, null, 2), "utf-8");
  await rename(tempPath, filePath);
}
```

**Step 5: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/legions-registry.test.ts`
Expected: PASS

**Step 6: Commit**

```bash
jj describe -m "feat: add legions registry for tracking daemon instances"
jj new
```

---

## Task 3: Repo Manager

New module that clones repos and creates jj workspaces. Shells out to `jj` CLI.

**Files:**
- Create: `packages/daemon/src/daemon/repo-manager.ts`
- Create: `packages/daemon/src/daemon/__tests__/repo-manager.test.ts`

**Step 1: Write failing tests**

Tests use DI for the jj command runner to avoid real cloning in tests.

```typescript
// packages/daemon/src/daemon/__tests__/repo-manager.test.ts
import { describe, expect, it } from "bun:test";
import {
  parseIssueRepo,
  resolveWorkspacePath,
  type RepoManagerDeps,
  ensureRepoClone,
  ensureWorkspace,
} from "../repo-manager";
import type { LegionPaths } from "../paths";
import { resolveLegionPaths } from "../paths";

describe("parseIssueRepo", () => {
  it("parses explicit owner/repo", () => {
    const result = parseIssueRepo("acme/widgets");
    expect(result).toEqual({ host: "github.com", owner: "acme", repo: "widgets" });
  });

  it("returns null for invalid repo string", () => {
    expect(parseIssueRepo("")).toBeNull();
    expect(parseIssueRepo("noslash")).toBeNull();
  });
});

describe("resolveWorkspacePath", () => {
  it("builds workspace path from paths + projectId + issueId", () => {
    const paths = resolveLegionPaths({}, "/home/test");
    const result = resolveWorkspacePath(paths, "sjawhar/42", "acme-widgets-7");
    expect(result).toBe("/home/test/.local/share/legion/workspaces/sjawhar/42/acme-widgets-7");
  });
});

describe("ensureRepoClone", () => {
  it("clones when directory does not exist", async () => {
    const commands: string[][] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => { commands.push(args); return { exitCode: 0, stdout: "", stderr: "" }; },
      exists: async () => false,
    };

    const paths = resolveLegionPaths({}, "/home/test");
    await ensureRepoClone(paths, { host: "github.com", owner: "acme", repo: "widgets" }, deps);

    expect(commands[0]).toContain("git");
    expect(commands[0]).toContain("clone");
    expect(commands[0]).toContain("https://github.com/acme/widgets");
  });

  it("fetches when directory already exists", async () => {
    const commands: string[][] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => { commands.push(args); return { exitCode: 0, stdout: "", stderr: "" }; },
      exists: async () => true,
    };

    const paths = resolveLegionPaths({}, "/home/test");
    await ensureRepoClone(paths, { host: "github.com", owner: "acme", repo: "widgets" }, deps);

    expect(commands[0]).toContain("git");
    expect(commands[0]).toContain("fetch");
  });
});

describe("ensureWorkspace", () => {
  it("creates jj workspace when it does not exist", async () => {
    const commands: string[][] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => { commands.push(args); return { exitCode: 0, stdout: "", stderr: "" }; },
      exists: async (p) => p.includes("repos/"), // repo exists, workspace does not
    };

    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    const wsPath = await ensureWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);

    expect(wsPath).toBe("/home/test/.local/share/legion/workspaces/sjawhar/42/acme-widgets-7");
    const wsCmd = commands.find((c) => c.includes("workspace"));
    expect(wsCmd).toBeDefined();
    expect(wsCmd).toContain("add");
  });

  it("skips workspace creation when it already exists", async () => {
    const commands: string[][] = [];
    const deps: RepoManagerDeps = {
      runJj: async (args) => { commands.push(args); return { exitCode: 0, stdout: "", stderr: "" }; },
      exists: async () => true,
    };

    const paths = resolveLegionPaths({}, "/home/test");
    const repo = { host: "github.com", owner: "acme", repo: "widgets" };
    await ensureWorkspace(paths, "sjawhar/42", "acme-widgets-7", repo, deps);

    const wsCmd = commands.find((c) => c.includes("workspace"));
    expect(wsCmd).toBeUndefined(); // No workspace add needed
  });
});
```

**Step 2: Run tests to verify they fail**

Run: `bun test packages/daemon/src/daemon/__tests__/repo-manager.test.ts`
Expected: FAIL

**Step 3: Implement repo-manager**

```typescript
// packages/daemon/src/daemon/repo-manager.ts
import path from "node:path";
import type { LegionPaths } from "./paths";

export interface RepoRef {
  host: string;
  owner: string;
  repo: string;
}

export interface JjResult {
  exitCode: number;
  stdout: string;
  stderr: string;
}

export interface RepoManagerDeps {
  runJj: (args: string[]) => Promise<JjResult>;
  exists: (path: string) => Promise<boolean>;
}

const defaultDeps: RepoManagerDeps = {
  runJj: async (args) => {
    const result = Bun.spawnSync(["jj", ...args], {
      stdio: ["ignore", "pipe", "pipe"],
      timeout: 120_000,
    });
    return {
      exitCode: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
    };
  },
  exists: async (p) => {
    const { existsSync } = await import("node:fs");
    return existsSync(p);
  },
};

export function parseIssueRepo(repoStr: string): RepoRef | null {
  const parts = repoStr.split("/");
  if (parts.length !== 2 || !parts[0] || !parts[1]) return null;
  return { host: "github.com", owner: parts[0], repo: parts[1] };
}

export function resolveWorkspacePath(
  paths: LegionPaths,
  projectId: string,
  issueId: string,
): string {
  return path.join(paths.forLegion(projectId).workspacesDir, issueId);
}

export async function ensureRepoClone(
  paths: LegionPaths,
  repo: RepoRef,
  deps: RepoManagerDeps = defaultDeps,
): Promise<string> {
  const clonePath = paths.repoClonePath(repo.host, repo.owner, repo.repo);

  if (await deps.exists(clonePath)) {
    const result = await deps.runJj(["git", "fetch", "-R", clonePath]);
    if (result.exitCode !== 0) {
      console.warn(`[repo-manager] jj git fetch failed for ${clonePath}: ${result.stderr}`);
    }
  } else {
    const url = `https://${repo.host}/${repo.owner}/${repo.repo}`;
    const result = await deps.runJj(["git", "clone", url, clonePath]);
    if (result.exitCode !== 0) {
      throw new Error(`Failed to clone ${url}: ${result.stderr}`);
    }
  }

  return clonePath;
}

export async function ensureWorkspace(
  paths: LegionPaths,
  projectId: string,
  issueId: string,
  repo: RepoRef,
  deps: RepoManagerDeps = defaultDeps,
): Promise<string> {
  const clonePath = await ensureRepoClone(paths, repo, deps);
  const workspacePath = resolveWorkspacePath(paths, projectId, issueId);

  if (!(await deps.exists(workspacePath))) {
    const result = await deps.runJj([
      "workspace", "add", workspacePath,
      "--name", issueId.toLowerCase(),
      "-R", clonePath,
    ]);
    if (result.exitCode !== 0) {
      throw new Error(`Failed to create workspace ${workspacePath}: ${result.stderr}`);
    }
  }

  return workspacePath;
}

export async function cleanupWorkspace(
  paths: LegionPaths,
  projectId: string,
  issueId: string,
  repo: RepoRef,
  deps: RepoManagerDeps = defaultDeps,
): Promise<void> {
  const clonePath = paths.repoClonePath(repo.host, repo.owner, repo.repo);
  const workspacePath = resolveWorkspacePath(paths, projectId, issueId);

  // Forget workspace in jj
  await deps.runJj(["workspace", "forget", issueId.toLowerCase(), "-R", clonePath]);

  // Remove directory
  const { rm } = await import("node:fs/promises");
  await rm(workspacePath, { recursive: true, force: true });
}
```

**Step 4: Run tests to verify they pass**

Run: `bun test packages/daemon/src/daemon/__tests__/repo-manager.test.ts`
Expected: PASS

**Step 5: Commit**

```bash
jj describe -m "feat: add repo manager for cloning repos and creating jj workspaces"
jj new
```

---

## Task 4: Update DaemonConfig to Use XDG Paths

Wire `paths.ts` into `config.ts`. Replace hardcoded `~/.legion` paths.

**Files:**
- Modify: `packages/daemon/src/daemon/config.ts`
- Modify: `packages/daemon/src/daemon/__tests__/config.test.ts`

**Step 1: Update config.ts**

Add `LegionPaths` to `DaemonConfig`. Replace `resolveStateFilePath` with paths from `resolveLegionPaths`. Keep `legionDir` temporarily for backward compat but deprecate it. Add `paths` field to `DaemonConfig`.

Key changes:
- Add `paths: LegionPaths` to `DaemonConfig`
- `stateFilePath` and `logDir` computed from `paths.forLegion(teamId)` when teamId is available
- `loadConfig()` calls `resolveLegionPaths(env, os.homedir())`

**Step 2: Update config tests**

Update expectations: `stateFilePath` should now point to `~/.local/state/legion/legions/<teamId>/workers.json` instead of `~/.legion/daemon/workers.json`.

**Step 3: Run tests, fix any breakage**

Run: `bun test packages/daemon/src/daemon/__tests__/config.test.ts`

**Step 4: Commit**

```bash
jj describe -m "feat: wire XDG paths into DaemonConfig"
jj new
```

---

## Task 5: Server — POST /workers Resolves Workspace Internally

Change `POST /workers` to accept an optional `repo` param and delegate to repo-manager. Workspace path is computed by the daemon, not passed by the caller.

**Files:**
- Modify: `packages/daemon/src/daemon/server.ts`
- Modify: `packages/daemon/src/daemon/server.ts:ServerOptions` — add `paths` and `projectId`
- Modify: `packages/daemon/src/daemon/__tests__/server.test.ts`

**Step 1: Update ServerOptions**

Add to `ServerOptions`:
```typescript
paths: LegionPaths;
projectId: string;
```

**Step 2: Update POST /workers handler**

Change the handler to:
- Accept `{issueId, mode}` (required) + `repo` (optional, `"owner/repo"` format) + `workspace` (optional, for backward compat)
- If `repo` is provided: use `parseIssueRepo` + `ensureWorkspace` to compute workspace path
- If `workspace` is provided (legacy): use it directly
- If neither: return `badRequest("missing repo or workspace")`

**Step 3: Add cleanup endpoint**

Add `DELETE /workers/:id/workspace` endpoint that calls `cleanupWorkspace()`.

**Step 4: Update server tests**

Add tests for the new repo-based workspace resolution flow. Use DI — pass a mock `repoManager` or use the existing adapter pattern.

**Step 5: Run all server tests**

Run: `bun test packages/daemon/src/daemon/__tests__/server.test.ts`

**Step 6: Commit**

```bash
jj describe -m "feat: POST /workers resolves workspace from repo param"
jj new
```

---

## Task 6: Daemon Lifecycle — Legions Registry Integration

On `startDaemon()`: register in `legions.json`. On shutdown: deregister. Controller runs in a generic working directory.

**Files:**
- Modify: `packages/daemon/src/daemon/index.ts`
- Modify: `packages/daemon/src/daemon/__tests__/index.test.ts`

**Step 1: Update startDaemon()**

- After server starts, call `writeLegionEntry(paths.legionsFile, projectId, { port, servePort, pid, startedAt })`
- In `shutdown()`, call `removeLegionEntry(paths.legionsFile, projectId)`
- Controller session workspace: use `paths.forLegion(projectId).legionStateDir` instead of `config.legionDir`
- Pass `paths` and `projectId` to `startServer()`

**Step 2: Update buildControllerEnv()**

- Remove `LEGION_DIR` (controller no longer needs a repo path)
- Keep `LEGION_TEAM_ID`, `LEGION_ISSUE_BACKEND`, `LEGION_DAEMON_PORT`, `LEGION_SHORT_ID`

**Step 3: Update tests**

Mock `writeLegionEntry`/`removeLegionEntry` or use temp dirs. Verify registry is written on start and cleaned on shutdown.

**Step 4: Run tests**

Run: `bun test packages/daemon/src/daemon/__tests__/index.test.ts`

**Step 5: Commit**

```bash
jj describe -m "feat: register/deregister daemon in legions.json on start/stop"
jj new
```

---

## Task 7: CLI — Dispatch Uses Repo, Port Discovery from Registry

Make `legion dispatch` pass `repo` to daemon instead of creating workspaces. CLI commands discover port from `legions.json`.

**Files:**
- Modify: `packages/daemon/src/cli/index.ts`
- Modify: `packages/daemon/src/cli/team-resolver.ts` — update cache path to XDG
- Modify: `packages/daemon/src/cli/__tests__/index.test.ts`
- Modify: `packages/daemon/src/cli/__tests__/team-resolver.test.ts`

**Step 1: Add `--repo` flag to dispatchCommand**

```typescript
repo: { type: "string", alias: "r", description: "Repository (owner/repo)" },
```

**Step 2: Update cmdDispatch()**

- Remove jj workspace creation logic (lines 309-319 of current index.ts)
- POST to daemon with `{issueId, mode, repo}` instead of `{issueId, mode, workspace}`
- `legionDir` and workspace resolution no longer needed

**Step 3: Update port discovery**

Add a `getDaemonPortForProject(projectId)` function:
- Check `LEGION_DAEMON_PORT` env var first (controller path)
- Fall back to reading `legions.json` and looking up by project ID
- Fall back to default port 13370

Update `cmdStatus`, `cmdStop`, `cmdAttach` to use this.

**Step 4: Rename team → legion in CLI text**

Update help text, console.log messages, command descriptions. Rename `teamsCommand` → keep command name `teams` for now (avoid breaking scripts) but update the description.

**Step 5: Update team-resolver cache path**

Change default cache dir from `~/.legion` to XDG state dir.

**Step 6: Run CLI tests**

Run: `bun test packages/daemon/src/cli/__tests__/`

**Step 7: Commit**

```bash
jj describe -m "feat: CLI dispatch passes repo to daemon, port discovery from legions.json"
jj new
```

---

## Task 8: Update Controller Skill

Remove `LEGION_DIR` references, add `--repo` to dispatch commands, update cleanup.

**Files:**
- Modify: `.opencode/skills/legion-controller/SKILL.md`
- Modify: `.claude/skills/legion-controller/SKILL.md` (mirror)

**Step 1: Update Environment section**

Remove `LEGION_DIR` from required env vars.

**Step 2: Update dispatch examples**

Change from:
```bash
legion dispatch "$ISSUE_IDENTIFIER" "$MODE" \
  --prompt "..."
```

To:
```bash
legion dispatch "$ISSUE_IDENTIFIER" "$MODE" \
  --repo "$OWNER/$REPO" \
  --prompt "..."
```

The controller already derives `$OWNER/$REPO` from the issue identifier (format: `owner-repo-number`). Document this derivation.

**Step 3: Update cleanup step (step 6)**

Replace:
```bash
WORKSPACES_DIR=$(dirname "$LEGION_DIR")
ISSUE_LOWER=$(echo "$ISSUE_IDENTIFIER" | tr '[:upper:]' '[:lower:]')
jj workspace forget "$ISSUE_LOWER" -R "$LEGION_DIR"
rm -rf "$WORKSPACES_DIR/$ISSUE_LOWER"
```

With:
```bash
legion cleanup "$ISSUE_IDENTIFIER" --repo "$OWNER/$REPO"
```

Or use the daemon HTTP endpoint:
```bash
curl -s -X DELETE "http://127.0.0.1:$LEGION_DAEMON_PORT/workers/$WORKER_ID/workspace" \
  -H 'Content-Type: application/json' \
  -d '{"repo": "'"$OWNER/$REPO"'"}'
```

**Step 4: Update heartbeat path**

Change from:
```bash
mkdir -p ~/.legion/$LEGION_SHORT_ID && touch ~/.legion/$LEGION_SHORT_ID/heartbeat
```

To: let the daemon handle heartbeat writing (it already knows its state dir), or update to use the new XDG path. The daemon writes heartbeat to `~/.local/state/legion/legions/<project-id>/heartbeat`.

**Step 5: Commit**

```bash
jj describe -m "feat: update controller skill for multi-repo workspace management"
jj new
```

---

## Task 9: Remove Legacy Code & Update Remaining Tests

Clean up old `~/.legion` references, remove `loadTeamsCache`, update integration tests.

**Files:**
- Modify: `packages/daemon/src/cli/index.ts` — remove `loadTeamsCache` function
- Modify: `packages/daemon/src/daemon/config.ts` — remove old `resolveStateFilePath`
- Modify: `packages/daemon/src/__tests__/integration.test.ts`
- Modify: `packages/daemon/src/daemon/__tests__/session-id-contract.test.ts` (if affected)

**Step 1: Remove loadTeamsCache()**

This function reads `~/.legion/teams.json`. Replace with `readLegionsRegistry` in the `teamsCommand`.

**Step 2: Remove resolveStateFilePath()**

This function hardcoded `~/.legion/daemon/workers.json`. State file paths now come from `paths.forLegion(projectId).workersFile`.

**Step 3: Update integration tests**

Ensure integration tests use the new XDG paths and temp directories.

**Step 4: Run full test suite**

Run: `bun test`
Expected: All tests pass

**Step 5: Lint and type check**

Run: `bunx biome check src/ && bunx tsc --noEmit`
Expected: No errors

**Step 6: Commit**

```bash
jj describe -m "refactor: remove legacy ~/.legion paths and loadTeamsCache"
jj new
```

---

## Task 10: End-to-End Verification

Verify the complete flow works: start daemon, dispatch with repo, workspace created in XDG dir.

**Step 1: Manual smoke test**

```bash
# Verify XDG paths
ls ~/.local/share/legion/    # Should not exist yet
ls ~/.local/state/legion/    # Should not exist yet

# Start a legion
LEGION_ISSUE_BACKEND=github legion start sjawhar/2

# Check legions.json was created
cat ~/.local/state/legion/legions.json

# Dispatch a worker (from another terminal, or via controller)
legion dispatch sjawhar-legion-93 implement --repo sjawhar/legion

# Verify repo was cloned
ls ~/.local/share/legion/repos/github.com/sjawhar/legion/

# Verify workspace was created
ls ~/.local/share/legion/workspaces/sjawhar-2/sjawhar-legion-93/

# Verify personal jj repo is NOT polluted
cd ~/legion/default && jj workspace list  # Should NOT show sjawhar-legion-93

# Stop
legion stop sjawhar/2
cat ~/.local/state/legion/legions.json  # Entry should be removed
```

**Step 2: Clean up old state**

```bash
rm -rf ~/.legion
```

**Step 3: Final commit**

```bash
jj describe -m "docs: workspace management implementation complete"
jj new
```
