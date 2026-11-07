// Whether a process id names this session's own chain. A tmux server answers about a pane with
// the process that pane was started for, and tmux hands a popup, `run-shell` job or hook a
// `TMUX_PANE` left over from wherever the server was started, so a pane is this session's only
// when its first process is one of this process's ancestors. Both the session title and the pane
// a session reports read it, so the walk lives here rather than in either host plugin.

import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";

/** How many parents `isAncestor` follows before it stops. */
const MAX_ANCESTORS = 64;

/** How long the `ps` off Linux is given to name the process table. One walk reads it once while
 *  the caller waits, so a process table that has stopped answering must not hold the caller past
 *  roughly the bound the tmux call it serves is already held to. */
const PARENT_LOOKUP_MS = 1_000;

/** The parent of process `pid` as `/proc` names it, or undefined when it cannot be read. */
function procParentPid(pid: number): number | undefined {
  try {
    // The command name, in parentheses, may hold spaces; the parent is the second field after it.
    const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
    const parent = Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[1]);
    return Number.isInteger(parent) ? parent : undefined;
  } catch {
    return undefined;
  }
}

/** Every running process's parent, from one `ps`. Off Linux this is what a whole walk is read
 *  from: a `ps` per step would cost up to `MAX_ANCESTORS` of them inside the one bound the
 *  caller is held to. A `ps` that fails or does not answer in time leaves the table empty, and
 *  the walk then stops where it started, as a failed parent lookup did. */
function processTable(): ReadonlyMap<number, number> {
  const table = new Map<number, number>();
  try {
    const listing = execFileSync("ps", ["-Ao", "pid=,ppid="], {
      encoding: "utf8",
      killSignal: "SIGKILL",
      timeout: PARENT_LOOKUP_MS,
    });
    for (const line of listing.split("\n")) {
      const [, pid = "", parent = ""] = line.match(/^\s*(\d+)\s+(\d+)\s*$/) ?? [];
      if (pid !== "") table.set(Number(pid), Number(parent));
    }
  } catch {
    // Nothing could be read: an empty table, which the walk reads as a chain it cannot follow.
  }
  return table;
}

/** Whether process `candidate` is `pid` or one of its ancestors, walking up from `pid` and
 *  stopping at the first match, at init, or after `MAX_ANCESTORS` steps. */
export function isAncestor(candidate: string, pid: number = process.pid): boolean {
  // Linux reads each parent from `/proc`, which costs no process at all. Everywhere else the
  // whole walk is read from one snapshot, taken when the walk first asks for a parent: a match
  // on the process itself still runs nothing.
  let table: ReadonlyMap<number, number> | undefined;
  const parentPid = (child: number): number | undefined => {
    if (process.platform === "linux") return procParentPid(child);
    table ??= processTable();
    return table.get(child);
  };
  let current: number | undefined = pid;
  for (let step = 0; current !== undefined && current > 1 && step < MAX_ANCESTORS; step++) {
    if (String(current) === candidate) return true;
    current = parentPid(current);
  }
  return false;
}
