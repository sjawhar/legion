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
  homeDir: string
): LegionPaths {
  if (!path.isAbsolute(homeDir)) {
    throw new Error(`homeDir must be an absolute path, got: ${homeDir}`);
  }
  function resolveXdgDir(value: string | undefined, fallback: string): string {
    return value && path.isAbsolute(value) ? value : fallback;
  }

  const dataHome = resolveXdgDir(env.XDG_DATA_HOME, path.join(homeDir, ".local", "share"));
  const stateHome = resolveXdgDir(env.XDG_STATE_HOME, path.join(homeDir, ".local", "state"));

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
      const resolvedStateDir = path.resolve(legionStateDir);
      const expectedParent = path.resolve(path.join(stateDir, "legions"));
      if (
        !resolvedStateDir.startsWith(expectedParent + path.sep) &&
        resolvedStateDir !== expectedParent
      ) {
        throw new Error(`Project path would escape legions directory: ${resolvedStateDir}`);
      }
      return {
        legionStateDir,
        workersFile: path.join(legionStateDir, "workers.json"),
        logDir: path.join(legionStateDir, "logs"),
        workspacesDir: path.join(workspacesDir, projectId),
      };
    },
    repoClonePath(host: string, owner: string, repo: string): string {
      const clonePath = path.join(reposDir, host, owner, repo);
      const resolvedPath = path.resolve(clonePath);
      const expectedParent = path.resolve(reposDir);
      if (!resolvedPath.startsWith(expectedParent + path.sep) && resolvedPath !== expectedParent) {
        throw new Error(`Repo path would escape repos directory: ${resolvedPath}`);
      }
      return clonePath;
    },
  };
}
