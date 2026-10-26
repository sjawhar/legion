import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { DISPATCH_KEY_PATTERN, type LegionRole } from "@legion/contracts";
import type { LegionPhase } from "@legion/contracts/legion-api";
import { DispatchClient, DispatchServiceError } from "@legion/envoy-client/dispatch-http";
import { messageFor } from "@legion/envoy-client/errors";
import {
  type SessionConfigFile,
  sessionDispatchSettings,
} from "@legion/envoy-client/session-settings";
import { staleGitLockNote } from "./git-lock";

/**
 * What a `handoff_complete` reports beside its summary, found in the pane before anything is
 * posted: the Dispatch handoff record the phase ended with, and the commit the completion is of.
 * The daemon reads the record back from Dispatch and holds it to the issue, the phase, the version
 * and the claim's session; this module compares no session itself.
 *
 * A file-backed phase the session's role works (`FILE_BACKED`: the phase from the issue record,
 * the role from the session) ends with a handoff record on the issue, which the worker writes with
 * `dispatch handoff-write`; its completion reports the newest record of the phase's word,
 * `handoffId` and `handoffVersion`, read from Dispatch here. No record is refused naming the
 * command that writes one; a Dispatch that does not answer, or refuses the read, is refused with
 * its answer. The implementer's implementing round also reports `commit`, the commit
 * `legion/<issue>@origin` names in the pane's jj (its pushed tip, the head of the pull request), and
 * is refused when `@-` is not on that branch yet, naming the push. The planner, the tester and the
 * reviewer report no commit. Every other completion — retro, the merger's READY, a role reporting
 * a phase it does not run (which the daemon refuses naming whose phase it is) — reports the commit
 * the workspace stands on, `@-`, and no record. The implementer's report on its closed issue's
 * production check resolves neither: `legion-phase-ops.ts` never asks this module for one.
 */

/** Runs the pane's jj with `args` against the workspace at `cwd` and answers its trimmed stdout;
 * throws on a non-zero exit. */
export type JjRunner = (args: readonly string[], cwd: string) => Promise<string>;

/** The newest handoff record of `issue`'s phase `word` (envoy-client's
 * `DispatchClient.latestHandoff`): throws Dispatch's answer, a `404 HANDOFF_NOT_FOUND` when the
 * phase has none. */
export type LatestHandoff = (
  issue: string,
  word: string
) => Promise<{ readonly id: string; readonly version: number }>;

/** The fields a completion adds to its request (`LegionHandoffCompleteRequest`), each present only
 * when the completion carries it. */
export interface HandoffReport {
  readonly commit?: string;
  readonly handoffId?: string;
  readonly handoffVersion?: number;
}

export interface HandoffReportInput {
  /** The issue's phase, from its record (`read_record`'s `phase`). */
  readonly phase: LegionPhase;
  /** The session's claim role. */
  readonly role: LegionRole;
  /** The pane's environment: `LEGION_WORKSPACE` and `LEGION_ISSUE`. */
  readonly env: NodeJS.ProcessEnv;
  /** The pane's Dispatch (`paneLatestHandoff`). */
  readonly latestHandoff: LatestHandoff;
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

/** PATH's jj, run as `jj -R <workspace> …` from the workspace, so a revset names the same commits
 * from any directory. The stderr of a failure is the error, followed by a note on each stale git
 * lock it names (`staleGitLockNote`). */
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
    const note = await staleGitLockNote(detail);
    throw new Error(`jj ${args.join(" ")} failed: ${detail}${note === "" ? "" : `\n${note}`}`);
  }
};

/**
 * The pane's Dispatch as its `dispatch` command reaches it: `file` is the session's envoy.json (the
 * one its plugin hands its shell as `ENVOY_CONFIG_FILE`), read again on each call with `env`, so a
 * file that moved its server takes effect without a restart. A pane whose Dispatch is off throws
 * why.
 */
export function paneLatestHandoff(
  file: SessionConfigFile,
  env: NodeJS.ProcessEnv,
  cwd: string,
  fetchImpl: typeof fetch = fetch
): LatestHandoff {
  return async (issue, word) => {
    const { dispatch } = sessionDispatchSettings(file, env, { cwd });
    if (!dispatch.enabled) {
      throw new Error(`Dispatch is off for this pane: ${dispatch.error ?? dispatch.notice}`);
    }
    return new DispatchClient(dispatch.url, dispatch.token, fetchImpl).latestHandoff(issue, word);
  };
}

function requiredVariable(env: NodeJS.ProcessEnv, name: string): string {
  const value = env[name];
  if (value === undefined || value === "") {
    throw new Error(`${name} is not set, so handoff_complete cannot find what to report`);
  }
  return value;
}

/** The jj command that answers the commit the workspace stands on, `@-`. */
const STANDING = ["log", "-r", "@-", "--no-graph", "-T", "commit_id"] as const;

/** What a completion of `phase` by `role` reports. */
export async function findHandoffReport(input: HandoffReportInput): Promise<HandoffReport> {
  const { phase, role, env } = input;
  const jj = input.jj ?? pathJj;
  const handoff = FILE_BACKED[phase];
  if (handoff === undefined || handoff.role !== role) {
    return { commit: await jj(STANDING, requiredVariable(env, "LEGION_WORKSPACE")) };
  }
  // The issue key names the record's issue and the branch, and a worker sets its own environment:
  // a value carrying `#` or `/` would otherwise name another reference or path.
  const issue = requiredVariable(env, "LEGION_ISSUE");
  if (!DISPATCH_KEY_PATTERN.test(issue)) {
    throw new Error(
      `LEGION_ISSUE ${JSON.stringify(issue)} is not a Dispatch issue key like LEGION-1`
    );
  }
  let record: { readonly id: string; readonly version: number };
  try {
    record = await input.latestHandoff(issue, handoff.word);
  } catch (error) {
    if (
      error instanceof DispatchServiceError &&
      error.fromDispatch &&
      error.code === "HANDOFF_NOT_FOUND"
    ) {
      throw new Error(
        `${issue} has no ${handoff.word} handoff in Dispatch: write it with \`dispatch handoff-write --issue ${issue} --phase ${handoff.word} --body-file <file>\`, then complete again`
      );
    }
    const answer =
      error instanceof DispatchServiceError && error.fromDispatch
        ? `${error.status} ${error.code}: ${error.message}`
        : messageFor(error);
    throw new Error(
      `handoff_complete could not read ${issue}'s newest ${handoff.word} handoff from Dispatch: ${answer}; complete again once Dispatch answers`
    );
  }
  const report = { handoffId: record.id, handoffVersion: record.version };
  if (role !== "implementer") return report;

  // The implementer's round is of its pushed tip: the pull request's head, which the daemon holds
  // the round's CI to. Work still under `@-` that the branch does not carry is not pushed.
  const workspace = requiredVariable(env, "LEGION_WORKSPACE");
  const bookmark = `legion/${issue}`;
  const remote = `remote_bookmarks(exact:${JSON.stringify(bookmark)}, exact:"origin")`;
  const stands = await jj(STANDING, workspace);
  const pushed = await jj(
    ["log", "-r", `${stands} & ::${remote}`, "--no-graph", "-T", "commit_id"],
    workspace
  );
  if (pushed === "") {
    throw new Error(
      `the commit this workspace stands on, ${stands}, is not on ${bookmark}@origin: push it with \`jj bookmark set ${bookmark} -r @- && jj git push --bookmark ${bookmark}\`, then complete again`
    );
  }
  const tip = await jj(["log", "-r", remote, "--no-graph", "-T", "commit_id"], workspace);
  return { commit: tip, ...report };
}
