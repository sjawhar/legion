import path from "node:path";
import {
  type FirstTurnNotice,
  type FirstTurnNoticeMessage,
  firstTurnNoticeMessage,
} from "./first-turn-notice";

/**
 * The notice an agent gets when Legion's setup command could not install this repository's
 * packages in its container. `scripts/legion-setup.sh` runs as the deployment's setup command
 * before every start of a role's Oh My Pi and never exits non-zero — a non-zero exit would stop
 * the agent from starting and charge the launch a failure — so a failed install reaches the agent
 * as the one report it leaves behind instead, and a successful install deletes that report.
 */

/** The custom message type the notice is saved in the session under. */
export const SETUP_REPORT_MESSAGE = "legion-setup-report";

/** The report's name under `$LEGION_STATE_DIR`, as `scripts/legion-setup.sh` writes it. */
export const SETUP_REPORT_FILE = "legion-setup-report.json";

/** The last characters of the report's `detail` (the install log's tail) the notice quotes. */
const DETAIL_TAIL = 400;

/**
 * The report `scripts/legion-setup.sh` writes, or undefined when this environment names no state
 * directory and the script therefore wrote none.
 */
export function setupReportPath(
  environment: Readonly<Record<string, string | undefined>>
): string | undefined {
  const stateDirectory = environment.LEGION_STATE_DIR;
  if (stateDirectory === undefined || stateDirectory === "") return undefined;
  return path.join(stateDirectory, SETUP_REPORT_FILE);
}

/**
 * The notice for a report this session's own setup run left, read with `readFile` (injected, so a
 * test hands this function one file and the extension the real one), undefined when there is none
 * to tell. A missing file, an unreadable one, malformed JSON and a report of another `kind` are
 * each no notice and never an exception: a failed install must never cost the agent its start.
 *
 * The report's `issue`, `role` and `generation` must all equal this environment's, because
 * `LEGION_STATE_DIR` outlives a generation in a pod and, under the tmux runtime, is shared by
 * every pane of the host: a report another role or an earlier generation left is not this agent's
 * news, and a field the report is missing matches nothing.
 */
export function setupReportNotice(
  environment: Readonly<Record<string, string | undefined>>,
  readFile: (file: string) => string
): FirstTurnNotice | undefined {
  const file = setupReportPath(environment);
  if (file === undefined) return undefined;
  let report: unknown;
  try {
    report = JSON.parse(readFile(file));
  } catch {
    return undefined;
  }
  if (typeof report !== "object" || report === null) return undefined;
  const fields = report as Readonly<Record<string, unknown>>;
  if (fields.kind !== "install-failed") return undefined;
  const mine = (
    [
      ["issue", "LEGION_ISSUE"],
      ["role", "LEGION_ROLE"],
      ["generation", "LEGION_GENERATION"],
    ] as const
  ).every(([field, variable]) => {
    const value = fields[field];
    const expected = environment[variable];
    return typeof value === "string" && typeof expected === "string" && value === expected;
  });
  if (!mine) return undefined;

  const reason = typeof fields.reason === "string" ? fields.reason : "unknown";
  const exitStatus = typeof fields.exitStatus === "number" ? String(fields.exitStatus) : "unknown";
  const detail = typeof fields.detail === "string" ? fields.detail.trim() : "";
  const tail = detail.length > DETAIL_TAIL ? `…${detail.slice(-DETAIL_TAIL)}` : detail;
  // Only the three reasons that name a `bun install` have an install log behind their detail
  // (`scripts/legion-setup.sh`'s failure branch quotes the log's tail there). A `lock-timeout`
  // run never held the lock and a `no-install-root` run never had a directory to write in, and
  // neither ran an install, so introducing their detail as a log is a report asserting something
  // that did not happen — the defect this notice exists to prevent one level up.
  const ranInstall =
    reason === "install-failed" || reason === "install-timeout" || reason === "install-killed";
  return {
    id: crypto.randomUUID(),
    content:
      "Legion's setup command could not install this repository's packages in this container: " +
      `reason "${reason}", exit status ${exitStatus}.` +
      (tail === ""
        ? ""
        : ranInstall
          ? ` The install log ended: ${tail}`
          : ` It reported: ${tail}`) +
      "\n\nUntil they are installed, this repository's own check commands cannot run here: each " +
      "says so rather than passing silently, so a check you cannot run is no evidence that it " +
      "passes. CI's required jobs are the authority for lint, types and the unit suites meanwhile." +
      "\n\nTo retry the install by hand, run `sh scripts/legion-setup.sh` from your workspace: " +
      "it takes the lock every role of this issue shares, installs with the cache this pod " +
      "names, and records what happened. A bare `bun install` does none of those three — it can " +
      "run beside another role's install of the same workspace, it leaves no marker, and where " +
      "nothing names a cache its downloads land on the container's own disk, whose breach " +
      "evicts this pod.",
  };
}

/** The custom message that carries `notice`, as the session saves it. */
export function setupReportMessage(notice: FirstTurnNotice): FirstTurnNoticeMessage {
  return firstTurnNoticeMessage(notice, SETUP_REPORT_MESSAGE);
}
