import { existsSync } from "node:fs";
import { chmod, mkdir, mkdtemp, rename, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import type { IssueKey } from "@legion/contracts";

export interface RunResult {
  readonly exitCode: number;
  readonly stdout: string;
  readonly stderr: string;
  /** Set by the runner when it killed the command at `limitMs`; `elapsedMs` is the wall time the
   * command actually ran. See `CommandResult` in the daemon's command runner. */
  readonly timedOut?: { readonly limitMs: number; readonly elapsedMs: number };
  /** Set by the runner when it killed the command because the caller's `signal` aborted: the
   * caller gave the command up, and nothing about the command itself is being reported. At most
   * one of `timedOut` and `aborted` is present. */
  readonly aborted?: true;
}

export interface WorkspaceSpec {
  readonly repoCloneDir: string;
  readonly workspaceDir: string;
  /** `legion/<KEY>`: created by `createWorkspace` on the fresh working copy when the workspace is
   * created and no bookmark resolved (a resolving bookmark is where the workspace is created
   * instead), moved only by the implementer's push, and never re-created on an existing workspace
   * — so absent on one whose pull request has merged, since the fetch deletes it. The daemon does
   * not read this field. */
  readonly bookmark: string;
}

export interface WorkspaceCommandOptions {
  readonly cwd?: string;
  readonly env?: Readonly<Record<string, string>>;
  /** Budget after which the runner kills the command. */
  readonly timeoutMs?: number;
  /** Kills the command when aborted, as the budget does; the result carries `aborted`. */
  readonly signal?: AbortSignal;
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
  /** Budget for every provisioning command (`jj git clone`/`fetch`, `jj workspace add`, the git
   * config writes): each waits on the network or a credential helper, so the daemon passes its
   * `slow_command_timeout_seconds` here rather than the runner's generic default. */
  readonly commandTimeoutMs: number;
}

/** Neither kill is an ordinary `Command failed (exit N)`: the runner's own report is what the
 * operator needs — the budget and wall time for a timeout, the fact of the abort for a command
 * the caller gave up on. */
function commandFailure(result: RunResult, cmd: string[]): Error {
  if (result.aborted) {
    const message = `Command aborted: ${cmd.join(" ")}`;
    return new Error(result.stderr ? `${message}\n${result.stderr}` : message);
  }
  if (result.timedOut) {
    const { limitMs, elapsedMs } = result.timedOut;
    const message = `Command timed out after ${limitMs / 1000} s (ran ${(elapsedMs / 1000).toFixed(1)} s): ${cmd.join(" ")}`;
    return new Error(result.stderr ? `${message}\n${result.stderr}` : message);
  }
  return new Error(`Command failed (exit ${result.exitCode}): ${cmd.join(" ")}\n${result.stderr}`);
}

/** Every provisioning command goes through here so each carries `deps.commandTimeoutMs`. */
function run(
  deps: ProvisionIssueWorkspaceDeps,
  cmd: string[],
  opts?: WorkspaceCommandOptions
): Promise<RunResult> {
  return deps.run(cmd, { ...opts, timeoutMs: deps.commandTimeoutMs });
}

async function runChecked(
  deps: ProvisionIssueWorkspaceDeps,
  cmd: string[],
  opts?: WorkspaceCommandOptions
): Promise<RunResult> {
  const result = await run(deps, cmd, opts);
  if (result.exitCode !== 0) throw commandFailure(result, cmd);
  return result;
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

/** Clones the repository into `repoCloneDir` unless a complete clone (one with `.jj`) is already
 * there. The clone is written into a temporary sibling (`<repoCloneDir>.clone-XXXXXX`, same
 * parent, so the final step is a same-filesystem directory move) and renamed into place only
 * after `jj git clone` exits 0 and `.jj` exists: a clone the runner killed at its budget, or one
 * a daemon crash interrupted, can never be left at the final path looking like a finished clone.
 * A final directory without `.jj` (left by an older daemon) is removed and cloned again, with a
 * log line, instead of failing every launch forever. Leftover `.clone-*` siblings from a crash are
 * inert — nothing ever mistakes one for a clone — and are deliberately not swept: a sweep would
 * race a concurrent in-flight clone of the same repository. */
async function ensureRepoClone(
  deps: ProvisionIssueWorkspaceDeps,
  repoCloneDir: string,
  owner: string,
  repo: string,
  credentialEnv: Readonly<Record<string, string>>
): Promise<void> {
  const jjDir = path.join(repoCloneDir, ".jj");
  if (existsSync(repoCloneDir)) {
    if (existsSync(jjDir)) return;
    console.error(
      `[legion] removing incomplete clone at ${repoCloneDir} (no .jj) before cloning again`
    );
    await rm(repoCloneDir, { recursive: true, force: true });
  }

  await mkdir(path.dirname(repoCloneDir), { recursive: true });
  const tempDir = await mkdtemp(`${repoCloneDir}.clone-`);
  try {
    const remote = `https://github.com/${owner}/${repo}`;
    await runChecked(deps, ["jj", "git", "clone", remote, tempDir], { env: credentialEnv });
    const tempJjDir = path.join(tempDir, ".jj");
    if (!existsSync(tempJjDir)) {
      throw new Error(`Incomplete Jujutsu clone at ${tempDir}: missing ${tempJjDir}`);
    }
    try {
      await rename(tempDir, repoCloneDir);
    } catch (error) {
      // Two issues provisioning the same repository for the first time concurrently: if the
      // other clone has landed, it won and ours is surplus.
      const code = (error as NodeJS.ErrnoException).code;
      if ((code === "ENOTEMPTY" || code === "EEXIST") && existsSync(jjDir)) return;
      throw error;
    }
  } finally {
    // Best-effort: a cleanup failure must never replace the clone's own error (a timeout, a
    // rename collision) as the reason the launch failed. A leftover `.clone-*` sibling is inert.
    try {
      await rm(tempDir, { recursive: true, force: true });
    } catch (error) {
      console.error(`[legion] failed to remove temporary clone at ${tempDir}:`, error);
    }
  }
}

/** Creates the issue workspace on top of its bookmark `legion/<KEY>` when that bookmark resolves
 * to exactly one commit, or at `main` — creating the bookmark on the fresh working copy — when it
 * resolves to none. This is the one place provisioning creates or moves that bookmark. A workspace
 * that already exists gets no `jj bookmark` command at all (see `provisionIssueWorkspace`): after
 * a pull request merges and GitHub deletes its branch, the fetch drops the tracked local bookmark
 * that still matched it, and creating it again would put the issue's branch on whatever the
 * working copy holds (LEGION-28); a bookmark a worker left elsewhere stays there. The implementer's
 * push step (`jj bookmark set` + `jj git push --bookmark`) owns every later move.
 *
 * The resolution runs before any other command. `bookmarks(exact:legion/<KEY>)` lists the
 * bookmark's local targets one commit id per line — none when it is missing or only a remote row
 * survives, one normally, two or more when it is conflicted (verified on jj 0.44 and 0.45). A
 * failed command or more than one commit throws, naming the bookmark, before anything is pruned,
 * added, or registered: a `jj workspace add --revision legion/<KEY>` for a name jj cannot resolve
 * still registers the workspace, parented on the root commit, before reporting the error, and the
 * next resume would adopt that empty workspace silently. The add takes the commit id, never the
 * name, for the same reason.
 *
 * A brand-new workspace and one jj still registers but whose directory is gone start from the same
 * resolution: on `already registered|exists` the registration is forgotten, the colocated worktree
 * pruned, and the add repeated at the same revision. Only when nothing resolved does the add end
 * with `jj bookmark set legion/<KEY> -r @` in the new workspace, and only when the registration had
 * to be forgotten is one line logged — a brand-new issue has no bookmark to miss. */
async function createWorkspace(
  deps: ProvisionIssueWorkspaceDeps,
  repoCloneDir: string,
  workspaceDir: string,
  workspaceName: string,
  bookmark: string
): Promise<void> {
  const resolveArgs = [
    "jj",
    "log",
    "-r",
    `bookmarks(exact:${bookmark})`,
    "--no-graph",
    "-T",
    'commit_id ++ "\n"',
    "--ignore-working-copy",
    "-R",
    repoCloneDir,
  ];
  const resolved = await run(deps, resolveArgs);
  if (resolved.exitCode !== 0) {
    throw new Error(
      `Bookmark ${bookmark} could not be resolved; workspace ${workspaceDir} was not created.\n${commandFailure(resolved, resolveArgs).message}`
    );
  }
  const commits = resolved.stdout.split("\n").filter((line) => line.trim() !== "");
  if (commits.length > 1) {
    throw new Error(
      `Bookmark ${bookmark} is conflicted (${commits.join(", ")}); workspace ${workspaceDir} was not created. Resolve it with \`jj bookmark set ${bookmark} -r <commit> -R ${repoCloneDir}\`.`
    );
  }
  const bookmarkCommit = commits[0];

  await mkdir(path.dirname(workspaceDir), { recursive: true });
  const gitDir = path.join(repoCloneDir, ".git");
  const pruneArgs = ["git", `--git-dir=${gitDir}`, "worktree", "prune"];
  const addArgs = [
    "jj",
    "workspace",
    "add",
    workspaceDir,
    "--name",
    workspaceName,
    "--revision",
    bookmarkCommit ?? "main",
    "-R",
    repoCloneDir,
  ];

  try {
    await run(deps, pruneArgs);
  } catch {}

  const result = await run(deps, addArgs);
  const forgotten = result.exitCode !== 0;
  if (forgotten) {
    if (!/already (?:registered|exists)/.test(result.stderr)) {
      throw commandFailure(result, addArgs);
    }
    // `jj workspace forget` takes only workspace names (jj 0.44 and 0.45); the `git worktree prune`
    // that follows is what cleans the colocated worktree.
    await runChecked(deps, ["jj", "workspace", "forget", workspaceName, "-R", repoCloneDir]);
    try {
      await run(deps, pruneArgs);
    } catch {}
    await runChecked(deps, addArgs);
  }

  if (bookmarkCommit !== undefined) return;
  if (forgotten) {
    console.error(
      `[legion] bookmark ${bookmark} is gone (its pull request merged or the branch was deleted); re-added the forgotten workspace ${workspaceDir} at main, creating the bookmark on its fresh working copy`
    );
  }
  await runChecked(deps, ["jj", "bookmark", "set", bookmark, "-r", "@"], { cwd: workspaceDir });
}

async function writeOmpConfig(workspaceDir: string): Promise<void> {
  const ompDir = path.join(workspaceDir, ".omp");
  await mkdir(ompDir, { recursive: true });
  await writeFile(path.join(ompDir, "config.yml"), "", "utf8");
}

/** Removes the repository-scoped jj identity an earlier worker boot left on the shared clone.
 * Before LEGION-44 the extension ran `jj config set --repo user.name`/`user.email` at every
 * worker boot; `--repo` on a workspace writes the one config file every workspace of the clone
 * shares, so the last worker to boot, in any tree, set the author and committer for every other
 * tree's commits. Identity now rides each pane's environment (`JJ_USER`/`JJ_EMAIL`, read over
 * any config), so a leftover value is inert for panes but still wrong for anything else that
 * commits from the clone; it is removed here, once, logged, and nothing writes it again. Runs on
 * every provisioning so a pane still on the pre-upgrade plugin during a rollout is healed at the
 * next launch. A key is probed first (`jj config list --repo` exits 0 with empty stdout when
 * unset) because `jj config unset` exits 1 on a key that does not exist; a failed unset is
 * re-probed once, since two issues provisioning at the same time can both see the key and only
 * one of them removes it. */
async function removeRepoScopedIdentity(
  deps: ProvisionIssueWorkspaceDeps,
  repoCloneDir: string
): Promise<void> {
  for (const key of ["user.name", "user.email"]) {
    const probe = ["jj", "config", "list", "--repo", "-R", repoCloneDir, key];
    const present = await run(deps, probe);
    if (present.exitCode !== 0) throw commandFailure(present, probe);
    if (present.stdout.trim() === "") continue;
    console.error(
      `[legion] removing repository-scoped jj ${key} from ${repoCloneDir}: an earlier worker boot wrote it, and one repo config is shared by every workspace of the clone; identity rides each pane's environment now`
    );
    const unset = ["jj", "config", "unset", "--repo", "-R", repoCloneDir, key];
    const removed = await run(deps, unset);
    if (removed.exitCode === 0) continue;
    const recheck = await run(deps, probe);
    if (recheck.exitCode === 0 && recheck.stdout.trim() === "") continue;
    throw commandFailure(removed, unset);
  }
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
  // matches exactly this pattern for a Dispatch key). `createWorkspace` alone touches it: it
  // creates the workspace on the bookmark's one commit, or at `main` creating the bookmark when
  // none resolved, and stops before registering anything when the bookmark is conflicted. A
  // workspace that already exists gets no bookmark command here — present or absent, the bookmark
  // is left exactly as the fetch and the workers left it.
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
  await removeRepoScopedIdentity(deps, repoCloneDir);
  await writeOmpConfig(workspaceDir);

  return { repoCloneDir, workspaceDir, bookmark };
}
