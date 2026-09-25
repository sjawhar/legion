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
   * created and neither the bookmark nor an untracked origin row of it resolved (a resolving
   * bookmark is where the workspace is created instead, and an origin row is tracked and used),
   * moved only by the implementer's push, and never re-created on an existing workspace — so
   * absent on one whose pull request has merged, since the fetch deletes it. The daemon does not
   * read this field. */
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
 * the caller gave up on. Shared with the daemon's own working-copy commands
 * (`ProcessManager.adoptWorkingCopy`), so every jj command against the shared clone reports a
 * kill the same way. */
export function commandFailure(result: RunResult, cmd: string[]): Error {
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

/** What the command helpers need: the runner and its budget. Both `ProvisionIssueWorkspaceDeps`
 * and `RemoveIssueWorkspaceDeps` satisfy it. */
type CommandDeps = Pick<ProvisionIssueWorkspaceDeps, "run" | "commandTimeoutMs">;

/** Every provisioning and removal command goes through here so each carries
 * `deps.commandTimeoutMs`. */
function run(deps: CommandDeps, cmd: string[], opts?: WorkspaceCommandOptions): Promise<RunResult> {
  return deps.run(cmd, { ...opts, timeoutMs: deps.commandTimeoutMs });
}

async function runChecked(
  deps: CommandDeps,
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

/** The credential — and the git configuration — provisioning's own `jj git clone` and
 * `jj git fetch` run with. The token travels only through the askpass script (`GIT_ASKPASS`
 * answers `x-access-token` and `$LEGION_PROVISIONING_TOKEN`), never as a config value or an
 * argument. The clone's persisted config is the pane's: `credential.helper` and the
 * github.com-specific entry name the pane helper (`deps.credentialHelper`), and
 * `credential.interactive=false` keeps a pane's git from ever prompting. Provisioning runs with
 * no grant — the daemon host, a pod's init container — so that helper must not be consulted:
 * it fails there, and from git 2.44 on (the worker image ships 2.47) `credential.interactive=false`
 * then forbids the askpass fallback too, `fatal: unable to get password from user` on every second
 * provisioning of a clone (LEGION-178). So the environment resets the helper chain and re-enables
 * askpass for these commands alone, as `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`
 * pairs rather than `-c` flags: jj, not this code, spawns the git that fetches. Git reads that
 * environment config after the repository's, so the empty `credential.helper` clears every helper
 * read before it — the general entry and the URL-specific one alike — and `credential.interactive`
 * is last-wins. The persisted config is untouched. */
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
      GIT_CONFIG_COUNT: "2",
      GIT_CONFIG_KEY_0: "credential.helper",
      GIT_CONFIG_VALUE_0: "",
      GIT_CONFIG_KEY_1: "credential.interactive",
      GIT_CONFIG_VALUE_1: "true",
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

/** Creates the issue workspace where its bookmark `legion/<KEY>` says, deciding exactly as the Go
 * twin does (`createWorkspace`, packages/daemon-go/internal/workspace/bookmark.go). This is the
 * one place provisioning creates that bookmark. A workspace that already exists gets no
 * `jj bookmark` command at all (see `provisionIssueWorkspace`): after a pull request merges and
 * GitHub deletes its branch, the fetch drops the tracked local bookmark that still matched it, and
 * creating it again would put the issue's branch on whatever the working copy holds (LEGION-28); a
 * bookmark a worker left elsewhere stays there. The implementer's push step (`jj bookmark set` +
 * `jj git push --bookmark`) owns every later move.
 *
 * One read of the bookmark before any other command, its local row and origin's (`readBookmark`),
 * decides, in this order:
 *   - A conflicted local bookmark is refused by name, with its sides. That includes a conflict
 *     with a deleted side — a local deletion never pushed after origin's branch moved, or a local
 *     move never pushed after GitHub deleted the branch — which `bookmarks(exact:legion/<KEY>)`
 *     lists as one commit (verified on jj 0.44 and 0.45). The refusal names two ways out: keep an
 *     added commit (`jj bookmark set`, which jj refuses for the removed side), or start from main,
 *     by deleting the branch on GitHub when origin has it and `jj bookmark delete` when it does
 *     not.
 *   - A local bookmark on one commit is where the workspace starts, whatever origin's row is: a
 *     row with no commit, after GitHub deleted a branch the local bookmark had moved on from, or
 *     one tracked before the first push.
 *   - With no local bookmark, a conflicted origin row, which concurrent fetches in the shared
 *     clone leave, is refused by name; the next provisioning's fetch settles it.
 *   - With no local bookmark, a tracked origin row is a local deletion never pushed (a `jj
 *     bookmark delete`, or a `jj abandon` of the commit the bookmark pointed at, leaves it, and
 *     `jj bookmark track` does nothing to it). It is refused by name, with the operator's three
 *     ways out: restore the bookmark; cancel the deletion (`jj bookmark forget`, which leaves the
 *     row untracked, so the next provisioning adopts origin's branch); or start from main by
 *     deleting the branch on GitHub. A push of the deletion from the shared clone is not offered:
 *     the clone's only credential helper is `legion credential`, which `provisionIssueWorkspace`
 *     writes into its git config and which needs a tree's grant, so it cannot authenticate from an
 *     operator's shell or the controller; and `jj git push --deleted` would push every pending
 *     deletion in the clone, other issues' branches with it.
 *   - With no local bookmark, an untracked origin row is the issue's own branch, pushed from
 *     another clone before this workspace existed: a repository's fixture, or the branch a tree
 *     pushed before its volume was lost. A fresh clone tracks main alone, so such a branch is only
 *     a remote row (LEGION-286). It is tracked, which creates the local bookmark at its commit, and
 *     the workspace starts there. The track takes origin's row as it is when it runs, so the
 *     bookmark is read again after it: a fetch that moved the row in between would leave the
 *     workspace on the listed commit and the bookmark on a newer one, so this provisioning is
 *     refused instead, and the next one starts at the new commit.
 *   - No row with a commit at all is a brand-new issue, or a merged branch GitHub deleted, whose
 *     fetch dropped the bookmark with it: the workspace starts at main, with the bookmark created
 *     on it, and none of a deleted branch comes back (LEGION-28, LEGION-84).
 *
 * A failed read, a row the template cannot have printed, or a refusal throws, naming the bookmark,
 * before anything is pruned, added, or registered: a `jj workspace add --revision` jj cannot
 * resolve still registers the workspace, parented on the root commit, before reporting the error,
 * and the next resume would adopt that empty workspace silently. The add takes the commit id,
 * never the name, for the same reason.
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
  const remote = `${bookmark}@origin`;
  const { local, origin } = await readBookmark(deps, repoCloneDir, workspaceDir, bookmark);
  let bookmarkCommit: string | undefined;
  if (local?.conflict) {
    const [keep, commit] =
      local.added.length > 1
        ? ["one of its added commits", "<commit>"]
        : ["its added commit", local.added[0]];
    const fromMain = origin?.present
      ? `delete the branch on GitHub (the pull request's Delete branch button, or \`gh api -X DELETE repos/${deps.repo}/git/refs/heads/${bookmark}\`)`
      : `\`jj bookmark delete ${bookmark} -R ${repoCloneDir}\``;
    throw new Error(
      `Bookmark ${bookmark} is conflicted ${conflictTargets(local)}; workspace ${workspaceDir} was not created. ` +
        `Keep ${keep}: \`jj bookmark set ${bookmark} -r ${commit} -R ${repoCloneDir}\`. ` +
        `Start from main instead: ${fromMain}, and the next provisioning starts at main.`
    );
  } else if (local?.present) {
    bookmarkCommit = local.added[0];
  } else if (origin?.conflict) {
    throw new Error(
      `Remote bookmark ${remote} is conflicted ${conflictTargets(origin)}, which concurrent fetches leave; workspace ${workspaceDir} was not created. Provision again: the next provisioning's fetch sets the row to origin's branch as it is then.`
    );
  } else if (origin?.present && origin.tracked) {
    throw new Error(
      `Bookmark ${bookmark} was deleted in the shared clone ${repoCloneDir} and the deletion never pushed, while ${remote} is tracked at ${origin.added[0]}; workspace ${workspaceDir} was not created. ` +
        `Restore it: \`jj bookmark set ${bookmark} -r ${remote} -R ${repoCloneDir}\`. ` +
        `Cancel the deletion, and the next provisioning adopts origin's branch: \`jj bookmark forget ${bookmark} -R ${repoCloneDir}\`. ` +
        `Start from main instead: delete the branch on GitHub (the pull request's Delete branch button, or \`gh api -X DELETE repos/${deps.repo}/git/refs/heads/${bookmark}\`), and the next provisioning starts at main.`
    );
  } else if (origin?.present) {
    const listedCommit = origin.added[0];
    await runChecked(deps, ["jj", "bookmark", "track", remote, "-R", repoCloneDir]);
    const tracked = (await readBookmark(deps, repoCloneDir, workspaceDir, bookmark)).local;
    if (!tracked?.present || tracked.conflict || tracked.added[0] !== listedCommit) {
      throw new Error(
        `Bookmark ${remote} moved from ${listedCommit} to ${listed(tracked?.added ?? [])} while it was being tracked; workspace ${workspaceDir} was not created. Provision again: the next provisioning starts at origin's branch as it is then.`
      );
    }
    bookmarkCommit = listedCommit;
  }

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

/** The `jj bookmark list -T` template of `createWorkspace`'s one read, the Go twin's
 * (`bookmarkRowTemplate`): one line per row, its fields separated by `|` — where it is (`local`,
 * `origin`, `git`, any other remote), then present, conflict and tracked as 1 or 0, then the
 * commits it adds and removes, comma-separated. It reads no `normal_target`, which prints an
 * `<Error: …>` and exits 0 for a conflicted row or a row with no commit. */
const BOOKMARK_ROWS =
  'if(remote, remote, "local") ++ "|" ++ if(present, "1", "0") ++ "|" ++ if(conflict, "1", "0") ++ "|" ++ if(tracked, "1", "0") ++ "|" ++ added_targets.map(|c| c.commit_id()).join(",") ++ "|" ++ removed_targets.map(|c| c.commit_id()).join(",") ++ "\\n"';

/** One row of `BOOKMARK_ROWS`: whether the bookmark exists there, whether it is conflicted,
 * whether a remote row is tracked, and the commits it adds and removes (one added commit, none
 * removed, when it is present and not conflicted). */
interface BookmarkRow {
  readonly present: boolean;
  readonly conflict: boolean;
  readonly tracked: boolean;
  readonly added: readonly string[];
  readonly removed: readonly string[];
}

/** The issue's bookmark in the shared clone, every row of it in one `jj bookmark list`, keyed by
 * where each row is; a bookmark that exists nowhere lists nothing. A failed read, and a row that is
 * not the template's shape — the wrong number of fields, a flag that is not 0 or 1, a commit that
 * is not 40 lowercase hex characters (a commit jj cannot load prints an error value where its id
 * goes, which a workspace add would take as a revision), a conflicted row with no added commit, a
 * present row that is not conflicted without exactly one added commit and none removed, a second
 * row for the same place — are refused, naming the bookmark. */
async function readBookmark(
  deps: ProvisionIssueWorkspaceDeps,
  repoCloneDir: string,
  workspaceDir: string,
  bookmark: string
): Promise<Partial<Record<string, BookmarkRow>>> {
  const listArgs = [
    "jj",
    "bookmark",
    "list",
    "--all-remotes",
    `exact:${bookmark}`,
    "-T",
    BOOKMARK_ROWS,
    "--ignore-working-copy",
    "-R",
    repoCloneDir,
  ];
  const listed = await run(deps, listArgs);
  if (listed.exitCode !== 0) {
    throw new Error(
      `Bookmark ${bookmark} could not be resolved; workspace ${workspaceDir} was not created.\n${commandFailure(listed, listArgs).message}`
    );
  }
  const rows: Partial<Record<string, BookmarkRow>> = {};
  for (const line of listed.stdout.split("\n")) {
    if (line.trim() === "") continue;
    const fields = line.trim().split("|");
    const [where = "", present, conflict, tracked, added = "", removed = ""] = fields;
    const row: BookmarkRow = {
      present: present === "1",
      conflict: conflict === "1",
      tracked: tracked === "1",
      added: added === "" ? [] : added.split(","),
      removed: removed === "" ? [] : removed.split(","),
    };
    if (
      fields.length !== 6 ||
      ![present, conflict, tracked].every((flag) => flag === "0" || flag === "1") ||
      ![...row.added, ...row.removed].every((commit) => /^[0-9a-f]{40}$/.test(commit)) ||
      (row.conflict && row.added.length === 0) ||
      (row.present && !row.conflict && (row.added.length !== 1 || row.removed.length !== 0)) ||
      rows[where] !== undefined
    ) {
      throw new Error(
        `Bookmark ${bookmark}'s row ${JSON.stringify(line.trim())} is not the shape ${listArgs.join(" ")} prints; workspace ${workspaceDir} was not created.`
      );
    }
    rows[where] = row;
  }
  return rows;
}

/** A conflicted row's targets as a refusal names them: `(adds …; removes …)`, "nothing" for an
 * empty side, and, when it adds no more commits than it removes, the note that one side of the
 * conflict is a deletion (`- A + B`, where two targets are `- base + A + B`). */
function conflictTargets(row: BookmarkRow): string {
  const deletion = row.added.length <= row.removed.length ? ", one side a deletion" : "";
  return `(adds ${listed(row.added)}; removes ${listed(row.removed)})${deletion}`;
}

/** A row's commits as a refusal names them: comma-separated, or "nothing". */
function listed(commits: readonly string[]): string {
  return commits.join(", ") || "nothing";
}

/** Removes a repository-scoped jj `user.name`/`user.email` from the shared clone. `--repo` on a
 * workspace is the one config file every workspace of the clone shares, so a value there applies
 * to every tree at once. A pane's commits are unaffected — identity rides each pane's environment
 * (`JJ_USER`/`JJ_EMAIL`, read over any config) — but anything else that commits from the clone
 * would take the value as its author and committer. Removed here, once, logged; nothing writes it.
 * Runs on every provisioning, not only workspace creation, so a value written between launches is
 * removed at the next one. A key is probed first (`jj config list --repo` exits 0 with empty
 * stdout when unset) because `jj config unset` exits 1 on a key that does not exist. The probe
 * carries `--include-overridden`: without it jj hides a repository value that a higher layer
 * overrides, and `deps.run` is injected — this package cannot assume a runner that strips
 * `JJ_USER`/`JJ_EMAIL` from the command's environment — so a bare probe could print nothing for a
 * value the repo file does hold and the unset would silently never run. A failed unset is
 * re-probed once, since two issues provisioning at the same time can both see the key and only
 * one of them removes it. */
async function removeRepoScopedIdentity(
  deps: ProvisionIssueWorkspaceDeps,
  repoCloneDir: string
): Promise<void> {
  for (const key of ["user.name", "user.email"]) {
    const probe = [
      "jj",
      "config",
      "list",
      "--repo",
      "--include-overridden",
      "-R",
      repoCloneDir,
      key,
    ];
    const present = await run(deps, probe);
    if (present.exitCode !== 0) throw commandFailure(present, probe);
    if (present.stdout.trim() === "") continue;
    console.error(`[legion] removing repository-scoped jj ${key} from ${repoCloneDir}`);
    const unset = ["jj", "config", "unset", "--repo", "-R", repoCloneDir, key];
    const removed = await run(deps, unset);
    if (removed.exitCode === 0) continue;
    const recheck = await run(deps, probe);
    if (recheck.exitCode === 0 && recheck.stdout.trim() === "") continue;
    throw commandFailure(removed, unset);
  }
}

/** Where `issue`'s jj workspace lives under `stateDir` — the one path `provisionIssueWorkspace`
 * creates and every later daemon command against that working copy must target. */
export function issueWorkspaceDir(
  stateDir: string,
  repo: `${string}/${string}`,
  issue: IssueKey
): string {
  const [owner, name] = repo.split("/") as [string, string];
  return path.join(stateDir, "workspaces", owner, name, issue.toLowerCase());
}

/** Where the one shared clone of `repo` lives under `stateDir` — the `-R` target of every jj
 * command provisioning and removal run against the repository rather than a workspace. */
export function sharedCloneDir(stateDir: string, repo: `${string}/${string}`): string {
  const [owner, name] = repo.split("/") as [string, string];
  return path.join(stateDir, "repos", "github.com", owner, name);
}

/** The jj identity a working-copy adoption runs under: `JJ_USER`/`JJ_EMAIL`, which jj reads over
 * every config scope. */
export interface JjIdentity {
  readonly jjUser: string;
  readonly jjEmail: string;
}

/** The one command that makes an issue's working copy the assigned role's: rewrites the author
 * of the workspace's working-copy commit to the identity in the command's environment
 * (`JJ_USER`/`JJ_EMAIL`), and only when that commit is undescribed — a described working copy is
 * a previous phase's work and keeps its author. Both executors -- `TmuxRuntime` on the daemon
 * host, `legion worker-shim` in a pod's main container -- build it here, so the revset and flags
 * cannot drift apart. */
export function adoptWorkingCopyCommand(workspaceDir: string): string[] {
  return [
    "jj",
    "metaedit",
    "--update-author",
    "-r",
    '@ & description(exact:"")',
    "-R",
    workspaceDir,
  ];
}

/** Runs `adoptWorkingCopyCommand(workspaceDir)` through `run` under `identity` within
 * `timeoutMs`, and throws `commandFailure`'s report (jj's stderr, or the runner's timeout or
 * abort report) when it fails. The two executors -- the tmux runtime on the daemon host and
 * `legion worker-shim` inside a pod -- are this one call; each runner lays the identity over its
 * own process environment. */
export async function runAdoptWorkingCopy(
  run: ProvisionIssueWorkspaceDeps["run"],
  workspaceDir: string,
  identity: JjIdentity,
  timeoutMs: number
): Promise<void> {
  const command = adoptWorkingCopyCommand(workspaceDir);
  const result = await run(command, {
    env: { JJ_USER: identity.jjUser, JJ_EMAIL: identity.jjEmail },
    timeoutMs,
  });
  if (result.exitCode !== 0) throw commandFailure(result, command);
}

export async function provisionIssueWorkspace(
  issue: IssueKey,
  deps: ProvisionIssueWorkspaceDeps
): Promise<WorkspaceSpec> {
  const [owner, repo] = deps.repo.split("/") as [string, string];
  const workspaceName = issue.toLowerCase();
  const repoCloneDir = sharedCloneDir(deps.stateDir, deps.repo);
  const workspaceDir = issueWorkspaceDir(deps.stateDir, deps.repo, issue);
  const gitDir = path.join(repoCloneDir, ".git");
  // The design's PR ↔ issue linkage: branch `legion/<KEY>` (`reducers.ts`'s `issueForBranch`
  // matches exactly this pattern for a Dispatch key). `createWorkspace` alone touches it, and only
  // for a workspace it creates, deciding from the bookmark's rows where that workspace starts or
  // refusing before it registers anything. A workspace that already exists gets no bookmark
  // command here — present or absent, the bookmark is left exactly as the fetch and the workers
  // left it. The fetch may delete the bookmark together with its merged remote branch, but it
  // never abandons the branch's commits or rewrites a workspace's working copy:
  // `git.abandon-unreachable-commits` is `false` in the clone's per-repo jj settings before every
  // fetch (LEGION-84).
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
    // Checked on every provisioning, not only at clone time (the production clone predates this
    // rule), and before the fetch, the one command it governs: in a clone shared by one workspace
    // per issue, a commit Git no longer reaches is still somebody's work, so jj's default of
    // abandoning it (and rebasing the working copy above it) is the wrong rule here. jj keeps a
    // repo's settings in the user's config directory (`jj config path --repo`:
    // `~/.config/jj/repos/<config-id>/config.toml`, the id from `<clone>/.jj/repo/config-id`), so
    // every workspace of the clone and every pane's own jj read it. The read records no operation
    // and snapshots nothing; the write — in place, not atomic, and parsed by every jj command on
    // the box at start-up — runs only when the read did not already say `false`, so the steady
    // state never writes. Neither carries the credential.
    const setting = await runChecked(deps, [
      "jj",
      "config",
      "get",
      "git.abandon-unreachable-commits",
      "-R",
      repoCloneDir,
    ]);
    if (setting.stdout.trim() !== "false") {
      await runChecked(deps, [
        "jj",
        "config",
        "set",
        "--repo",
        "git.abandon-unreachable-commits",
        "false",
        "-R",
        repoCloneDir,
      ]);
    }
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

  return { repoCloneDir, workspaceDir, bookmark };
}

export type RemoveIssueWorkspaceDeps = Pick<
  ProvisionIssueWorkspaceDeps,
  "run" | "stateDir" | "repo" | "commandTimeoutMs"
>;

export interface RemoveIssueWorkspaceResult {
  readonly workspaceDir: string;
  /** False when jj did not register the workspace and its directory did not exist: nothing ran
   * beyond the registration check, and there is nothing to log. */
  readonly removed: boolean;
  /** The commit ids abandoned — the issue's own commits nothing else reached — newest first;
   * empty when every commit was kept (or nothing was removed). */
  readonly abandoned: readonly string[];
}

/** The revset of `workspaceName`'s own commits: every ancestor of its working copy that no other
 * workspace's working copy, bookmark, remote bookmark, or tag reaches. `::main` (and so the root
 * commit and everything merged) is always subtracted because `main` is a bookmark; a commit a
 * bookmark still reaches is pushed work and is kept; a commit another workspace is stacked on is
 * that workspace's business. Verified on jj 0.44.0 and 0.45.1
 * (docs/solutions/legion/jj-bookmark-facts-verified-on-0-44-0-and-0-45-1.md, "Removing a
 * workspace"). */
export function ownCommitsRevset(workspaceName: string): string {
  return `::${workspaceName}@ ~ ::(working_copies() ~ ${workspaceName}@) ~ ::(bookmarks() | remote_bookmarks() | tags())`;
}

/** Removes `issue`'s workspace from the shared clone once nothing runs in it (the daemon calls
 * this when the tree that owns the issue closes — `ProcessManager.removeTreeWorkspaces`): lists
 * the issue's own commits (`ownCommitsRevset`), deletes the directory, abandons those commits
 * (the working-copy commit among them; jj gives the workspace a new empty one), forgets the
 * workspace (which hides that empty commit), and prunes the colocated git worktree. Every jj
 * command targets the clone with `--ignore-working-copy`, so no other workspace's working copy is
 * snapshotted or touched.
 *
 * The directory goes first: a crash between the deletion and the forget leaves a registered
 * workspace whose directory is gone, exactly the shape `createWorkspace` repairs on the next
 * provisioning (`jj workspace add` answers `already exists`, so it forgets, prunes, and adds
 * again). The reverse order would leave a directory jj no longer knows, and every later
 * provisioning would fail at `jj workspace update-stale` (`Nothing checked out in this
 * workspace`). Idempotent: a workspace jj does not register runs no jj command past the list, a
 * missing directory is a no-op `rm`, an empty set skips the abandon; a second call returns
 * `removed: false` having run only the list. No fetch: the bookmarks are as the last provisioning
 * left them. A failing command throws `commandFailure` and leaves the remaining steps undone
 * (the caller logs once; the next provisioning repairs whichever half state it finds). */
export async function removeIssueWorkspace(
  issue: IssueKey,
  deps: RemoveIssueWorkspaceDeps
): Promise<RemoveIssueWorkspaceResult> {
  const workspaceName = issue.toLowerCase();
  const cloneDir = sharedCloneDir(deps.stateDir, deps.repo);
  const workspaceDir = issueWorkspaceDir(deps.stateDir, deps.repo, issue);
  const repoArgs = ["--ignore-working-copy", "-R", cloneDir];

  // No clone, nothing registered: jj cannot be asked and there is nothing to forget or prune.
  const cloneExists = existsSync(path.join(cloneDir, ".jj"));
  let registered = false;
  if (cloneExists) {
    const listed = await runChecked(deps, [
      "jj",
      "workspace",
      "list",
      "-T",
      'name ++ "\n"',
      ...repoArgs,
    ]);
    registered = listed.stdout.split("\n").includes(workspaceName);
  }
  let abandoned: string[] = [];
  if (registered) {
    const own = await runChecked(deps, [
      "jj",
      "log",
      "-r",
      ownCommitsRevset(workspaceName),
      "--no-graph",
      "-T",
      'commit_id ++ "\n"',
      ...repoArgs,
    ]);
    abandoned = own.stdout.split("\n").filter((line) => line.trim() !== "");
  }
  if (!registered && !existsSync(workspaceDir)) {
    return { workspaceDir, removed: false, abandoned: [] };
  }

  await rm(workspaceDir, { recursive: true, force: true });
  if (registered) {
    if (abandoned.length > 0) {
      await runChecked(deps, ["jj", "abandon", "-r", abandoned.join(" | "), ...repoArgs]);
    }
    await runChecked(deps, ["jj", "workspace", "forget", workspaceName, ...repoArgs]);
  }
  if (cloneExists) {
    await runChecked(deps, [
      "git",
      `--git-dir=${path.join(cloneDir, ".git")}`,
      "worktree",
      "prune",
    ]);
  }
  return { workspaceDir, removed: true, abandoned };
}
