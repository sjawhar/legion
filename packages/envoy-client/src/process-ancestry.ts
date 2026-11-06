// Whether a process id names this session's own chain. A tmux server answers about a pane with
// the process that pane was started for, and tmux hands a popup, `run-shell` job or hook a
// `TMUX_PANE` left over from wherever the server was started, so a pane is this session's only
// when its first process is one of this process's ancestors. Both the session title and the pane
// a session reports read it, so the walk lives here rather than in either host plugin.

import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";

/** How many parents `isAncestor` follows before it stops. */
const MAX_ANCESTORS = 64;

/** How long the `ps` off Linux is given to name one parent. A walk of the chain runs it once per
 *  step while the caller waits, so a process table that has stopped answering must not hold the
 *  caller past roughly the bound the tmux call it serves is already held to. */
const PARENT_LOOKUP_MS = 1_000;

/** The parent of process `pid`, or undefined when it cannot be read. */
function parentPid(pid: number): number | undefined {
  try {
    if (process.platform === "linux") {
      // The command name, in parentheses, may hold spaces; the parent is the second field after it.
      const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
      const parent = Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[1]);
      return Number.isInteger(parent) ? parent : undefined;
    }
    const parent = Number(
      execFileSync("ps", ["-o", "ppid=", "-p", String(pid)], {
        encoding: "utf8",
        timeout: PARENT_LOOKUP_MS,
        killSignal: "SIGKILL",
      }).trim()
    );
    return Number.isInteger(parent) ? parent : undefined;
  } catch {
    return undefined;
  }
}

/** Whether process `candidate` is `pid` or one of its ancestors, walking up from `pid` and
 *  stopping at the first match, at init, or after `MAX_ANCESTORS` steps. */
export function isAncestor(candidate: string, pid: number = process.pid): boolean {
  let current: number | undefined = pid;
  for (let step = 0; current !== undefined && current > 1 && step < MAX_ANCESTORS; step++) {
    if (String(current) === candidate) return true;
    current = parentPid(current);
  }
  return false;
}
