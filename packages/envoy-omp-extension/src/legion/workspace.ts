import { existsSync } from "node:fs";
import { mkdir, writeFile } from "node:fs/promises";
import path from "node:path";
import { type IssueKey, parseIssueKey } from "@legion/contracts";

const EXTENSION_PACKAGE = path.resolve(import.meta.dir, "../..");

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

export interface ProvisionIssueWorkspaceDeps {
  readonly run: (cmd: string[], opts?: { cwd?: string }) => Promise<RunResult>;
  readonly daemonUrl: string;
  readonly stateDir: string;
}

function maxRecursionDepth(): number {
  const raw = process.env.LEGION_MAX_RECURSION_DEPTH;
  const depth = Number(raw);
  if (!raw || !Number.isSafeInteger(depth) || depth <= 0) {
    throw new Error("LEGION_MAX_RECURSION_DEPTH must be a positive integer");
  }
  return depth;
}

function commandFailure(result: RunResult, cmd: string[]): Error {
  return new Error(`Command failed (exit ${result.exitCode}): ${cmd.join(" ")}\n${result.stderr}`);
}

async function runChecked(
  deps: ProvisionIssueWorkspaceDeps,
  cmd: string[],
  opts?: { cwd?: string }
): Promise<void> {
  const result = await deps.run(cmd, opts);
  if (result.exitCode !== 0) throw commandFailure(result, cmd);
}

async function createWorkspace(
  deps: ProvisionIssueWorkspaceDeps,
  repoCloneDir: string,
  workspaceDir: string,
  workspaceName: string,
  bookmark: string
): Promise<void> {
  const gitDir = `${repoCloneDir}/.jj/repo/store/git`;
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
async function writeOmpConfig(workspaceDir: string, depth: number): Promise<void> {
  const ompDir = path.join(workspaceDir, ".omp");
  await mkdir(ompDir, { recursive: true });
  await writeFile(
    path.join(ompDir, "config.yml"),
    `task:\n  maxRecursionDepth: ${depth}\nextensions:\n  - ${EXTENSION_PACKAGE}\n`,
    "utf8"
  );
}

export async function provisionIssueWorkspace(
  issue: IssueKey,
  deps: ProvisionIssueWorkspaceDeps
): Promise<WorkspaceSpec> {
  const parsedIssue = parseIssueKey(issue);
  if (!parsedIssue) throw new Error(`Invalid IssueKey: ${issue}`);

  const { owner, repo, number } = parsedIssue;
  const repoCloneDir = path.join(deps.stateDir, "repos", "github.com", owner, repo);
  const workspaceDir = path.join(deps.stateDir, "workspaces", owner, repo, `issue-${number}`);
  const bookmark = `legion/issue-${number}`;

  if (!existsSync(workspaceDir)) {
    await runChecked(deps, ["jj", "git", "fetch", "-R", repoCloneDir]);
    await createWorkspace(deps, repoCloneDir, workspaceDir, `issue-${number}`, bookmark);
  }

  await runChecked(deps, ["jj", "bookmark", "set", bookmark], { cwd: workspaceDir });
  await runChecked(deps, [
    "git",
    "-C",
    workspaceDir,
    "config",
    "credential.helper",
    "!legion credential",
  ]);
  await writeOmpConfig(workspaceDir, maxRecursionDepth());

  return { repoCloneDir, workspaceDir, bookmark };
}
