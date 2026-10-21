import path from "node:path";
import {
  type FirstTurnNotice,
  type FirstTurnNoticeMessage,
  firstTurnNoticeMessage,
} from "./first-turn-notice";

/**
 * The notice an agent gets from the report Legion's setup command left in its container.
 * `scripts/legion-setup.sh` runs as the deployment's setup command before every start of a
 * role's Oh My Pi and never exits non-zero — a non-zero exit would stop the agent from starting
 * and charge the launch a failure — so whatever it has to say reaches the agent as the one
 * report it leaves behind instead. It writes two kinds: `install-failed`, the packages the
 * agent's checks will not find, and `setting-ignored`, an install that succeeded while a
 * setting of this container's environment could not be used. A run with neither to tell deletes
 * the report.
 */

/** The custom message type the notice is saved in the session under. */
export const SETUP_REPORT_MESSAGE = "legion-setup-report";

/** The report's name under `$LEGION_STATE_DIR`, as `scripts/legion-setup.sh` writes it. */
export const SETUP_REPORT_FILE = "legion-setup-report.json";

/** The last characters of the report's `detail` the notice quotes. */
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
 * to tell. A missing file, an unreadable one, malformed JSON and a report of a `kind` this
 * version does not know are each no notice and never an exception: a failed install must never
 * cost the agent its start.
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
  const kind = fields.kind;
  if (kind !== "install-failed" && kind !== "setting-ignored") return undefined;
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

  const detail = typeof fields.detail === "string" ? fields.detail.trim() : "";
  const tail = detail.length > DETAIL_TAIL ? `…${detail.slice(-DETAIL_TAIL)}` : detail;
  const id = crypto.randomUUID();
  switch (kind) {
    // The install the agent's checks will not find. Only the reasons that name a `bun install`
    // have an install log behind their detail (`scripts/legion-setup.sh`'s failure branch quotes
    // the log's tail there), `install-hold-timeout` — an attempt cut off where it yielded the
    // cache's lock — among them. A `lock-timeout` run whose first attempt never got the lock and
    // a `no-install-root` run that had no directory to write in ran no install, so introducing
    // their detail as a log is a report asserting something that did not happen — the defect
    // this notice exists to prevent one level up.
    case "install-failed": {
      const reason = typeof fields.reason === "string" ? fields.reason : "unknown";
      const exitStatus =
        typeof fields.exitStatus === "number" ? String(fields.exitStatus) : "unknown";
      const ranInstall =
        reason === "install-failed" ||
        reason === "install-timeout" ||
        reason === "install-hold-timeout" ||
        reason === "install-killed";
      return {
        id,
        content:
          "Legion's setup command could not install this repository's packages in this " +
          `container: reason "${reason}", exit status ${exitStatus}.` +
          (tail === ""
            ? ""
            : ranInstall
              ? ` The install log ended: ${tail}`
              : ` It reported: ${tail}`) +
          "\n\nUntil they are installed, this repository's own check commands cannot run here: " +
          "each says so rather than passing silently, so a check you cannot run is no evidence " +
          "that it passes. CI's required jobs are the authority for lint, types and the unit " +
          "suites meanwhile." +
          "\n\nTo retry the install by hand, run `sh scripts/legion-setup.sh` from your " +
          "workspace: it takes the lock every role of this issue shares, installs with the " +
          "cache this pod names, and records what happened. A bare `bun install` does none of " +
          "those three — it can run beside another role's install of the same workspace, it " +
          "leaves no marker, and where nothing names a cache its downloads land on the " +
          "container's own disk, whose breach evicts this pod.",
      };
    }
    // The packages are installed; what the report carries is the one setting of this
    // container's environment the setup command could not use, and the default it used in its
    // place. Nothing about it says an install failed, because none did: a notice that hedged
    // would cost the agent the trust in its own checks that this whole mechanism is for.
    case "setting-ignored":
      return {
        id,
        content:
          "Legion's setup command installed this repository's packages in this container, and " +
          "could not use one setting of this container's environment: " +
          (tail === "" ? "its report does not say which." : tail) +
          "\n\nNothing failed to install, and this repository's own check commands run here as " +
          "usual. That setting belongs to the deployment rather than to this workspace, so no " +
          "change you make here takes it back: tell a human if the value was meant to have an " +
          "effect on this run.",
      };
  }
}

/** The custom message that carries `notice`, as the session saves it. */
export function setupReportMessage(notice: FirstTurnNotice): FirstTurnNoticeMessage {
  return firstTurnNoticeMessage(notice, SETUP_REPORT_MESSAGE);
}
