import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { DISPATCH_KEY_PATTERN, type LegionRole } from "@legion/contracts";
import type { LegionPhase } from "@legion/contracts/legion-api";

/**
 * The commit a `handoff_complete` reports, found in the pane with its own jj: what
 * `legion handoff complete` did before the `legion handoff` group was deleted (LEGION-631), plus
 * the push check. The daemon reads no handoff file and no branch head; it records the commit this
 * module finds, and refuses only one the role already reported for its previous phase
 * (`HANDOFF_NOT_NEW`).
 *
 * A file-backed phase the session's role works (`FILE_BACKED`: the phase from the issue record,
 * the role from the session) ends with `.legion/<issue>/<phase>.json`, and its completion reports
 * the newest commit on the issue branch that carries that file. The file must have no change
 * still in the working copy, must exist, must be committed on this branch rather than inherited
 * from the base, must be carried by a commit this pane's `JJ_USER`/`JJ_EMAIL` authored (every role
 * of an issue shares the workspace, so a handoff written into the previous role's commit would
 * otherwise be reported as this role's), and that commit must already be on
 * `legion/<issue>@origin`, which the pane's own `jj git push` moves. Each refusal is an error
 * naming the remedy. Every other completion — retro, the merger's READY, the production check, a
 * role reporting a phase it does not run (which the daemon refuses naming whose phase it is) —
 * reports the commit the workspace stands on, `@-`.
 */

/** Runs the pane's jj with `args` against the workspace at `cwd` and answers its trimmed stdout;
 * throws on a non-zero exit. */
export type JjRunner = (args: readonly string[], cwd: string) => Promise<string>;

export interface HandoffCommitInput {
  /** The issue's phase, from its record (`read_record`'s `phase`). */
  readonly phase: LegionPhase;
  /** The session's claim role. */
  readonly role: LegionRole;
  /** The pane's environment: `LEGION_WORKSPACE`, `LEGION_ISSUE`, and the identity the daemon
   * puts on every pane it gives one, `JJ_USER`/`JJ_EMAIL`. */
  readonly env: NodeJS.ProcessEnv;
  /** PATH's jj by default, the one the pane's own shell runs. */
  readonly jj?: JjRunner;
}

/** The handoff a file-backed phase ends with, by the phase's word, and the role that works it:
 * the daemon's `phase.HandoffFile` and `workflow.RoleFor` tables, mirrored. */
const FILE_BACKED: Readonly<Partial<Record<LegionPhase, { word: string; role: LegionRole }>>> = {
  planning: { word: "plan", role: "planner" },
  implementing: { word: "implement", role: "implementer" },
  testing: { word: "test", role: "tester" },
  reviewing: { word: "review", role: "reviewer" },
};

const execFileAsync = promisify(execFile);

/** PATH's jj, run as `jj -R <workspace> …` from the workspace, so a root-anchored fileset names
 * the same path from any directory. The stderr of a failure is the error. */
export const pathJj: JjRunner = async (args, cwd) => {
  try {
    const { stdout } = await execFileAsync("jj", ["-R", cwd, ...args], {
      cwd,
      maxBuffer: 16 * 1024 * 1024,
    });
    return stdout.trim();
  } catch (error) {
    const detail =
      error !== null && typeof error === "object" && "stderr" in error
        ? String(error.stderr).trim()
        : error instanceof Error
          ? error.message
          : String(error);
    throw new Error(`jj ${args.join(" ")} failed: ${detail}`);
  }
};

function requiredVariable(env: NodeJS.ProcessEnv, name: string): string {
  const value = env[name];
  if (value === undefined || value === "") {
    throw new Error(`${name} is not set, so handoff_complete cannot find the commit to report`);
  }
  return value;
}

/** The commit a completion of `phase` by `role` reports. */
export async function findHandoffCommit(input: HandoffCommitInput): Promise<string> {
  const { phase, role, env } = input;
  const jj = input.jj ?? pathJj;
  const workspace = requiredVariable(env, "LEGION_WORKSPACE");
  const handoff = FILE_BACKED[phase];
  if (handoff === undefined || handoff.role !== role) {
    return jj(["log", "-r", "@-", "--no-graph", "-T", "commit_id"], workspace);
  }
  // The issue key names the handoff's directory and the branch, and a worker sets its own
  // environment: a value such as ../x would otherwise name a path outside the tree's own.
  const issue = requiredVariable(env, "LEGION_ISSUE");
  if (!DISPATCH_KEY_PATTERN.test(issue)) {
    throw new Error(
      `LEGION_ISSUE ${JSON.stringify(issue)} is not a Dispatch issue key like LEGION-1`
    );
  }
  const file = `.legion/${issue}/${handoff.word}.json`;
  const fileset = `root:${JSON.stringify(file)}`;
  if ((await jj(["diff", "-r", "@", "--name-only", fileset], workspace)) !== "") {
    throw new Error(
      `${file} has changes in the working copy that are not committed: commit this phase's handoff before completing`
    );
  }
  if ((await jj(["file", "list", "-r", "@", fileset], workspace)) === "") {
    throw new Error(
      `${file} is missing from the workspace: write this phase's handoff, then commit and push it before completing`
    );
  }
  const carrying = await jj(
    [
      "log",
      "-r",
      `latest((::@- ~ ::trunk()) & files(${fileset}))`,
      "--no-graph",
      "-T",
      "commit_id",
    ],
    workspace
  );
  if (carrying === "") {
    throw new Error(
      `${file} is not committed on this issue's branch (only the base branch carries it): write and commit this phase's handoff`
    );
  }
  const user = env.JJ_USER;
  if (user !== undefined && user !== "") {
    const author = await jj(
      ["log", "-r", carrying, "--no-graph", "-T", 'author.name() ++ "\\n" ++ author.email()'],
      workspace
    );
    const [name = "", email = ""] = author.split("\n", 2);
    if (name !== user || email !== (env.JJ_EMAIL ?? "")) {
      throw new Error(
        `${file} is carried by commit ${carrying}, authored by ${name} <${email}>, not this pane's ${user}: run jj new, then write and commit this phase's handoff again`
      );
    }
  }
  const bookmark = `legion/${issue}`;
  const pushed = await jj(
    [
      "log",
      "-r",
      `${carrying} & ::remote_bookmarks(exact:${JSON.stringify(bookmark)}, exact:"origin")`,
      "--no-graph",
      "-T",
      "commit_id",
    ],
    workspace
  );
  if (pushed === "") {
    throw new Error(
      `${file} is carried by commit ${carrying}, which is not on ${bookmark}@origin: push the issue branch, then complete again`
    );
  }
  return carrying;
}
