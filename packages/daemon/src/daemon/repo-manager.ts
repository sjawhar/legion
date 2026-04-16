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
  runGit?: (args: string[]) => Promise<JjResult>;
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
      throw error;
    }
  },
  runGit: async (args) => {
    const proc = Bun.spawn(["git", ...args], {
      stdio: ["ignore", "pipe", "pipe"],
    });
    const timeout = setTimeout(() => proc.kill(), 30_000);
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
    // Prune stale git worktree registrations before creating workspace.
    // This handles cases where workspace directories were deleted (daemon restart,
    // worker pruning) without cleaning up the git worktree registration.
    const runGit = deps.runGit ?? defaultDeps.runGit;
    const gitDir = `${clonePath}/.jj/repo/store/git`;
    if (runGit) {
      try {
        await runGit([`--git-dir=${gitDir}`, "worktree", "prune"]);
      } catch {
        // Non-fatal: prune failure shouldn't block workspace creation
      }
    }

    const wsArgs = [
      "workspace",
      "add",
      workspacePath,
      "--name",
      issueId.toLowerCase(),
      "--revision",
      "main",
      "-R",
      clonePath,
    ];
    const result = await deps.runJj(wsArgs);
    if (result.exitCode !== 0) {
      // If workspace add failed with "already registered", force-add the git worktree
      // directly and retry the jj workspace add
      if (result.stderr.includes("already registered") && runGit) {
        try {
          await runGit([`--git-dir=${gitDir}`, "worktree", "add", "-f", workspacePath, "HEAD"]);
          // Clean up the git worktree we just forced — jj needs to own it
          await runGit([`--git-dir=${gitDir}`, "worktree", "remove", "--force", workspacePath]);
          await runGit([`--git-dir=${gitDir}`, "worktree", "prune"]);
        } catch {
          // Non-fatal: best-effort to clear the stale registration
        }

        // Retry jj workspace add after clearing the registration
        const retry = await deps.runJj(wsArgs);
        if (retry.exitCode !== 0) {
          throw new Error(`Failed to create workspace ${workspacePath}: ${retry.stderr}`);
        }
      } else {
        throw new Error(`Failed to create workspace ${workspacePath}: ${result.stderr}`);
      }
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

/**
 * Fetch all tracked repo clones under `paths.reposDir`.
 *
 * Walks the directory tree `reposDir/{host}/{owner}/{repo}` and fires
 * `jj git fetch` on each clone. Failures are collected but do not abort the
 * overall run — callers get a summary of successes and errors.
 */
export async function fetchAllTrackedRepos(
  paths: LegionPaths,
  deps: RepoManagerDeps = defaultDeps
): Promise<{ fetched: string[]; errors: Array<{ repo: string; error: string }> }> {
  const listDir = deps.listDir ?? defaultDeps.listDir;
  if (!listDir) {
    return { fetched: [], errors: [] };
  }

  const fetched: string[] = [];
  const errors: Array<{ repo: string; error: string }> = [];

  const hosts = await listDir(paths.reposDir);
  for (const host of hosts) {
    const hostPath = path.join(paths.reposDir, host);
    const owners = await listDir(hostPath);
    for (const owner of owners) {
      const ownerPath = path.join(hostPath, owner);
      const repos = await listDir(ownerPath);
      for (const repo of repos) {
        const clonePath = path.join(ownerPath, repo);
        if (!(await deps.exists(clonePath))) {
          continue;
        }
        const label = `${host}/${owner}/${repo}`;
        try {
          const result = await deps.runJj(["git", "fetch", "-R", clonePath]);
          if (result.exitCode !== 0) {
            errors.push({ repo: label, error: result.stderr });
          } else {
            fetched.push(label);
          }
        } catch (err) {
          errors.push({
            repo: label,
            error: err instanceof Error ? err.message : String(err),
          });
        }
      }
    }
  }

  return { fetched, errors };
}

/**
 * Check whether a bookmark (branch) for the given issue has been pushed to origin.
 *
 * Returns `{ safe: true }` when either:
 * - No local bookmark matching the issueId exists (nothing to lose), OR
 * - The local bookmark exists and is not ahead of origin (all commits pushed).
 *
 * Returns `{ safe: false, reason: string }` when unpushed commits would be lost.
 */
export async function verifyBranchPushed(
  clonePath: string,
  issueId: string,
  deps: RepoManagerDeps = defaultDeps
): Promise<{ safe: boolean; reason?: string }> {
  // List all remotes for this bookmark using a template that outputs one line per entry:
  //   "local" for the local bookmark
  //   "remote:<name> ahead:<N>" for each tracked remote
  const result = await deps.runJj([
    "bookmark",
    "list",
    issueId.toLowerCase(),
    "--all",
    "-T",
    'name ++ if(remote, " remote:" ++ remote ++ " ahead:" ++ tracking_ahead_count.lower(), " local") ++ "\n"',
    "-R",
    clonePath,
  ]);

  // If jj fails or returns no output, the bookmark doesn't exist — safe to clean
  if (result.exitCode !== 0 || !result.stdout.trim()) {
    return { safe: true };
  }

  const lines = result.stdout
    .trim()
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);

  // Check if there's a local bookmark (indicates work was done on this branch)
  const hasLocal = lines.some((line) => line.endsWith(" local"));
  if (!hasLocal) {
    // No local bookmark — only remote tracking entries exist, safe to clean
    return { safe: true };
  }

  // Check the origin remote specifically — is the local bookmark ahead of origin?
  const originLine = lines.find((line) => line.includes(" remote:origin "));
  if (!originLine) {
    // Local bookmark exists but no origin remote tracking — not pushed at all
    return {
      safe: false,
      reason: `Bookmark ${issueId.toLowerCase()} exists locally but has no remote tracking on origin — unpushed work would be lost`,
    };
  }

  // Parse ahead count from the origin line
  const aheadMatch = originLine.match(/ahead:(\d+)/);
  const aheadCount = aheadMatch ? parseInt(aheadMatch[1], 10) : 0;
  if (aheadCount > 0) {
    return {
      safe: false,
      reason: `Bookmark ${issueId.toLowerCase()} is ${aheadCount} commit(s) ahead of origin — unpushed work would be lost`,
    };
  }

  return { safe: true };
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

  // Verify the branch has been pushed before destroying the workspace
  const pushCheck = await verifyBranchPushed(clonePath, issueId, deps);
  if (!pushCheck.safe) {
    throw new Error(`Refusing to clean workspace for ${issueId}: ${pushCheck.reason}`);
  }

  await deps.runJj(["workspace", "forget", issueId.toLowerCase(), "-R", clonePath]);

  // Prune stale git worktree registrations — handles cases where workers were
  // reaped before cleanup ran and directories were deleted without git knowing
  const runGit = deps.runGit ?? defaultDeps.runGit;
  const gitDir = `${clonePath}/.jj/repo/store/git`;
  if (runGit) {
    try {
      const pruneResult = await runGit([`--git-dir=${gitDir}`, "worktree", "prune"]);
      if (pruneResult.exitCode !== 0) {
        console.warn(`[cleanup] git worktree prune failed (non-fatal): ${pruneResult.stderr}`);
      }
    } catch {
      // Non-fatal — git worktree prune may fail if git dir doesn't exist
    }
  }

  await deps.rmDir(workspacePath);
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
