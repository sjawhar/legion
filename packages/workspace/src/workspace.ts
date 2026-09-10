import { existsSync } from "node:fs";
import { chmod, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import type { IssueKey } from "@legion/contracts";

export interface RunResult {
  readonly exitCode: number;
  readonly stdout: string;
  readonly stderr: string;
}

export interface WorkspaceSpec {
  readonly repoCloneDir: string;
  readonly workspaceDir: string;
  readonly bookmark: string;
}

export interface WorkspaceCommandOptions {
  readonly cwd?: string;
  readonly env?: Readonly<Record<string, string>>;
}

export interface ProvisionIssueWorkspaceDeps {
  readonly run: (cmd: string[], opts?: WorkspaceCommandOptions) => Promise<RunResult>;
  readonly provisioningToken: () => Promise<string>;
  readonly credentialHelper: string;
  readonly extensionPackage: string;
  readonly stateDir: string;
  /** The single GitHub repository every Legion issue provisions against (`DaemonConfig.repo`) —
   * a Dispatch issue key carries no owner/repo of its own. */
  readonly repo: `${string}/${string}`;
}

function commandFailure(result: RunResult, cmd: string[]): Error {
  return new Error(`Command failed (exit ${result.exitCode}): ${cmd.join(" ")}\n${result.stderr}`);
}

async function runChecked(
  deps: ProvisionIssueWorkspaceDeps,
  cmd: string[],
  opts?: WorkspaceCommandOptions
): Promise<void> {
  const result = await deps.run(cmd, opts);
  if (result.exitCode !== 0) throw commandFailure(result, cmd);
}

const PROVISIONING_TOKEN_ENV = "LEGION_PROVISIONING_TOKEN";
const PROVISIONING_ASKPASS_SCRIPT = `#!/bin/sh
case "$1" in
  *Username*) printf '%s\n' x-access-token ;;
  *Password*) printf '%s\n' "$LEGION_PROVISIONING_TOKEN" ;;
  *) exit 1 ;;
esac
`;

interface ProvisioningCredential {
  readonly directory: string;
  readonly env: Readonly<Record<string, string>>;
}

async function createProvisioningCredential(
  stateDir: string,
  token: string
): Promise<ProvisioningCredential> {
  await mkdir(stateDir, { recursive: true });
  const directory = await mkdtemp(path.join(stateDir, "provisioning-credential-"));
  const askpass = path.join(directory, "askpass");
  await writeFile(askpass, PROVISIONING_ASKPASS_SCRIPT, { mode: 0o700 });
  await chmod(askpass, 0o700);
  return {
    directory,
    env: {
      GIT_ASKPASS: askpass,
      GIT_TERMINAL_PROMPT: "0",
      [PROVISIONING_TOKEN_ENV]: token,
    },
  };
}

async function ensureRepoClone(
  deps: ProvisionIssueWorkspaceDeps,
  repoCloneDir: string,
  owner: string,
  repo: string,
  credentialEnv: Readonly<Record<string, string>>
): Promise<void> {
  const jjDir = path.join(repoCloneDir, ".jj");
  if (existsSync(repoCloneDir)) {
    if (!existsSync(jjDir)) {
      throw new Error(`Incomplete Jujutsu clone at ${repoCloneDir}: missing ${jjDir}`);
    }
    return;
  }

  await mkdir(path.dirname(repoCloneDir), { recursive: true });

  const remote = `https://github.com/${owner}/${repo}`;
  await runChecked(deps, ["jj", "git", "clone", remote, repoCloneDir], {
    env: credentialEnv,
  });
  if (!existsSync(jjDir)) {
    throw new Error(`Incomplete Jujutsu clone at ${repoCloneDir}: missing ${jjDir}`);
  }
}

async function createWorkspace(
  deps: ProvisionIssueWorkspaceDeps,
  repoCloneDir: string,
  workspaceDir: string,
  workspaceName: string,
  bookmark: string
): Promise<void> {
  await mkdir(path.dirname(workspaceDir), { recursive: true });

  const gitDir = path.join(repoCloneDir, ".git");
  const pruneArgs = ["git", `--git-dir=${gitDir}`, "worktree", "prune"];
  const initialWorkspaceArgs = [
    "jj",
    "workspace",
    "add",
    workspaceDir,
    "--name",
    workspaceName,
    "--revision",
    "main",
    "-R",
    repoCloneDir,
  ];

  try {
    await deps.run(pruneArgs);
  } catch {}

  const result = await deps.run(initialWorkspaceArgs);
  if (result.exitCode === 0) return;
  if (!/already (?:registered|exists)/.test(result.stderr)) {
    throw commandFailure(result, initialWorkspaceArgs);
  }

  await runChecked(deps, [
    "jj",
    "workspace",
    "forget",
    workspaceName,
    "--cleanup",
    "--force",
    "-R",
    repoCloneDir,
  ]);
  try {
    await deps.run(pruneArgs);
  } catch {}

  const recoveryWorkspaceArgs = [
    "jj",
    "workspace",
    "add",
    workspaceDir,
    "--name",
    workspaceName,
    "--revision",
    bookmark,
    "-R",
    repoCloneDir,
  ];
  const retry = await deps.run(recoveryWorkspaceArgs);
  if (retry.exitCode !== 0) throw commandFailure(retry, recoveryWorkspaceArgs);
}

async function writeOmpConfig(workspaceDir: string): Promise<void> {
  const ompDir = path.join(workspaceDir, ".omp");
  await mkdir(ompDir, { recursive: true });
  await writeFile(path.join(ompDir, "config.yml"), "", "utf8");
}

export async function provisionIssueWorkspace(
  issue: IssueKey,
  deps: ProvisionIssueWorkspaceDeps
): Promise<WorkspaceSpec> {
  const [owner, repo] = deps.repo.split("/") as [string, string];
  const workspaceName = issue.toLowerCase();
  const repoCloneDir = path.join(deps.stateDir, "repos", "github.com", owner, repo);
  const workspaceDir = path.join(deps.stateDir, "workspaces", owner, repo, workspaceName);
  const gitDir = path.join(repoCloneDir, ".git");
  // The design's PR ↔ issue linkage: branch `legion/<KEY>` (`reducers.ts`'s `issueForBranch`
  // matches exactly this pattern for a Dispatch key).
  const bookmark = `legion/${issue}`;

  const workspaceExists = existsSync(workspaceDir);
  if (workspaceExists) {
    await runChecked(deps, ["jj", "workspace", "update-stale"], { cwd: workspaceDir });
  }

  const credential = await createProvisioningCredential(
    deps.stateDir,
    await deps.provisioningToken()
  );
  try {
    await ensureRepoClone(deps, repoCloneDir, owner, repo, credential.env);
    await runChecked(deps, ["jj", "git", "fetch", "-R", repoCloneDir], {
      env: credential.env,
    });
  } finally {
    await rm(credential.directory, { force: true, recursive: true });
  }

  if (!workspaceExists) {
    await createWorkspace(deps, repoCloneDir, workspaceDir, workspaceName, bookmark);
  }

  await runChecked(deps, ["jj", "bookmark", "set", bookmark, "--allow-backwards"], {
    cwd: workspaceDir,
  });
  await runChecked(deps, [
    "git",
    `--git-dir=${gitDir}`,
    "config",
    "--replace-all",
    "credential.helper",
    "",
  ]);
  await runChecked(deps, [
    "git",
    `--git-dir=${gitDir}`,
    "config",
    "--add",
    "credential.helper",
    deps.credentialHelper,
  ]);
  await runChecked(deps, [
    "git",
    `--git-dir=${gitDir}`,
    "config",
    "--replace-all",
    "credential.https://github.com.helper",
    "",
  ]);
  await runChecked(deps, [
    "git",
    `--git-dir=${gitDir}`,
    "config",
    "--add",
    "credential.https://github.com.helper",
    deps.credentialHelper,
  ]);
  await runChecked(deps, [
    "git",
    `--git-dir=${gitDir}`,
    "config",
    "credential.interactive",
    "false",
  ]);
  await writeOmpConfig(workspaceDir);

  return { repoCloneDir, workspaceDir, bookmark };
}
