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
  rmDir: async (p) => {
    const { rm } = await import("node:fs/promises");
    await rm(p, { recursive: true, force: true });
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
    const result = await deps.runJj(["git", "fetch", "-R", clonePath]);
    if (result.exitCode !== 0) {
      throw new Error(`jj git fetch failed for ${clonePath}: ${result.stderr}`);
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
      "-R",
      clonePath,
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
  deps: RepoManagerDeps = defaultDeps
): Promise<void> {
  const clonePath = paths.repoClonePath(repo.host, repo.owner, repo.repo);
  const workspacePath = resolveWorkspacePath(paths, projectId, issueId);

  await deps.runJj(["workspace", "forget", issueId.toLowerCase(), "-R", clonePath]);

  await deps.rmDir(workspacePath);
}
