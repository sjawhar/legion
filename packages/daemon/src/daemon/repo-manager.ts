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
  rmDir: (path: string) => Promise<void>;
  symlink: (target: string, linkPath: string) => Promise<void>;
  listDir?: (path: string) => Promise<string[]>;
}

export const defaultDeps: RepoManagerDeps = {
  runJj: async (args) => {
    const proc = Bun.spawn(["jj", ...args], {
      stdio: ["ignore", "pipe", "pipe"],
    });
    const timeout = setTimeout(() => proc.kill(), 120_000);
    try {
      const [exitCode, stdout, stderr] = await Promise.all([
        proc.exited,
        new Response(proc.stdout).text(),
        new Response(proc.stderr).text(),
      ]);
      return { exitCode, stdout, stderr };
    } finally {
      clearTimeout(timeout);
    }
  },
  exists: async (p) => {
    const { existsSync } = await import("node:fs");
    return existsSync(p);
  },
  rmDir: async (p) => {
    const { rm } = await import("node:fs/promises");
    await rm(p, { recursive: true, force: true });
  },
  symlink: async (target, linkPath) => {
    const { symlink } = await import("node:fs/promises");
    await symlink(target, linkPath);
  },
  listDir: async (p) => {
    const { readdir } = await import("node:fs/promises");
    try {
      return await readdir(p, { encoding: "utf8" });
    } catch (err) {
      const error = err as NodeJS.ErrnoException;
      if (error.code === "ENOENT") {
        return [];
      }
      throw err;
    }
  },
};

/**
 * Parse a repository reference string into owner and repo components.
 *
 * @param repoStr - Repository string in format "owner/repo" (e.g., "facebook/react")
 * @returns RepoRef with parsed owner and repo, or null if format is invalid
 *
 * @note Currently hardcoded to assume github.com as the host. This is acceptable for
 * the current scope (GitHub.com only). For GitHub Enterprise (GHE) support, this
 * function would need to accept an optional host parameter.
 */
export function parseIssueRepo(repoStr: string): RepoRef | null {
  const parts = repoStr.split("/");
  if (parts.length !== 2 || !parts[0] || !parts[1]) {
    return null;
  }
  return { host: "github.com", owner: parts[0], repo: parts[1] };
}

export function resolveWorkspacePath(
  paths: LegionPaths,
  projectId: string,
  issueId: string
): string {
  const workspacePath = path.join(paths.forLegion(projectId).workspacesDir, issueId);
  const resolvedPath = path.resolve(workspacePath);
  const expectedParent = path.resolve(paths.forLegion(projectId).workspacesDir);
  if (!resolvedPath.startsWith(expectedParent + path.sep) && resolvedPath !== expectedParent) {
    throw new Error(`Workspace path would escape workspaces directory: ${resolvedPath}`);
  }
  return workspacePath;
}

export async function ensureRepoClone(
  paths: LegionPaths,
  repo: RepoRef,
  deps: RepoManagerDeps = defaultDeps
): Promise<string> {
  const clonePath = paths.repoClonePath(repo.host, repo.owner, repo.repo);

  if (await deps.exists(clonePath)) {
    // Clone exists — skip fetch here, caller fires background fetch separately.
    // Workers fetch again on startup, so a slightly stale clone is fine.
    return clonePath;
  }

  const url = `https://${repo.host}/${repo.owner}/${repo.repo}`;
  const result = await deps.runJj(["git", "clone", url, clonePath]);
  if (result.exitCode !== 0) {
    throw new Error(`Failed to clone ${url}: ${result.stderr}`);
  }

  return clonePath;
}

export async function ensureWorkspace(
  paths: LegionPaths,
  projectId: string,
  issueId: string,
  repo: RepoRef,
  deps: RepoManagerDeps = defaultDeps
): Promise<string> {
  const clonePath = await ensureRepoClone(paths, repo, deps);
  const workspacePath = resolveWorkspacePath(paths, projectId, issueId);

  if (!(await deps.exists(workspacePath))) {
    const result = await deps.runJj([
      "workspace",
      "add",
      workspacePath,
      "--name",
      issueId.toLowerCase(),
      "--revision",
      "main",
      "-R",
      clonePath,
    ]);
    if (result.exitCode !== 0) {
      throw new Error(`Failed to create workspace ${workspacePath}: ${result.stderr}`);
    }
  }

  // Create .opencode symlink if .claude exists and .opencode doesn't
  const claudeDir = path.join(workspacePath, ".claude");
  const opencodePath = path.join(workspacePath, ".opencode");
  if ((await deps.exists(claudeDir)) && !(await deps.exists(opencodePath))) {
    try {
      await deps.symlink(".claude", opencodePath);
    } catch {
      // Non-fatal: symlink creation failure shouldn't block workspace setup
    }
  }

  return workspacePath;
}

/**
 * Fire a non-blocking `jj git fetch` on an existing clone.
 *
 * Rejects on non-zero exit codes so callers can log failures via `.catch()`.
 */
export async function startBackgroundFetch(
  paths: LegionPaths,
  repo: RepoRef,
  deps: RepoManagerDeps = defaultDeps
): Promise<JjResult> {
  const clonePath = paths.repoClonePath(repo.host, repo.owner, repo.repo);
  const result = await deps.runJj(["git", "fetch", "-R", clonePath]);
  if (result.exitCode !== 0) {
    throw new Error(`Background fetch failed for ${clonePath}: ${result.stderr}`);
  }
  return result;
}

export async function cleanupWorkspace(
  paths: LegionPaths,
  projectId: string,
  issueId: string,
  repo: RepoRef,
  deps: RepoManagerDeps = defaultDeps
): Promise<void> {
  const clonePath = paths.repoClonePath(repo.host, repo.owner, repo.repo);
  const workspacePath = resolveWorkspacePath(paths, projectId, issueId);

  await deps.runJj(["workspace", "forget", issueId.toLowerCase(), "-R", clonePath]);

  await deps.rmDir(workspacePath);
}

/**
 * List direct children of a workspace directory (shallow scan only — never recurse).
 * Returns an empty array if the directory does not exist.
 */
export async function listWorkspaceEntries(
  workspacesDir: string,
  deps: RepoManagerDeps = defaultDeps
): Promise<string[]> {
  const listFn = deps.listDir ?? defaultDeps.listDir;
  if (!listFn) {
    return [];
  }
  return listFn(workspacesDir);
}

/**
 * Remove a directory using the provided deps (or defaultDeps).
 */
export async function removeDir(
  dirPath: string,
  deps: RepoManagerDeps = defaultDeps
): Promise<void> {
  return deps.rmDir(dirPath);
}
