import { lstat } from "node:fs/promises";

/** The ways a git or jj failure names the lock file it could not take: jj's git backend
 * (gitoxide), and git's own lockfile API, which every git jj runs reports through. */
const GIT_LOCK_REPORTS = [
  /The lockfile at '([^']+)' might need manual deletion/g,
  /Unable to create '([^']+)': File exists/g,
];

/** `ms` as Go prints a time.Duration rounded to the second: `31m4s`, `2h0m5s`, `45s`. */
function age(ms: number): string {
  const total = Math.round(Math.abs(ms) / 1000);
  const sign = ms < 0 && total > 0 ? "-" : "";
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (hours > 0) return `${sign}${hours}h${minutes}m${seconds}s`;
  if (minutes > 0) return `${sign}${minutes}m${seconds}s`;
  return `${sign}${seconds}s`;
}

/**
 * One line for each git lock file `output` names that is still on disk: its path, how long ago it
 * was last written, and what becomes of it. A git or jj interrupted mid-write leaves its lock, and
 * every later write of what it locks fails on it; under a running pod no process removes it, since
 * a live git may still hold it, and the issue pod's next start does (`legion workspace-init
 * provision`). The daemon's shim words the same note for its own jj calls (`staleGitLockNote`,
 * packages/daemon/internal/shim/gitlock.go). Empty when `output` names no lock, or each one it
 * names is already gone.
 */
export async function staleGitLockNote(output: string, now = Date.now()): Promise<string> {
  const locks = new Set<string>();
  for (const report of GIT_LOCK_REPORTS) {
    for (const match of output.matchAll(report)) locks.add(match[1] as string);
  }
  const notes: string[] = [];
  for (const lock of locks) {
    const written = await lstat(lock).then(
      (info) => info.mtimeMs,
      () => undefined
    );
    if (written === undefined) continue;
    notes.push(
      `git lock ${lock} was last written ${age(now - written)} ago and blocks this command. If no git or jj process is still running, an interrupted one left it. Legion never removes a lock under a running pod; on Kubernetes the issue pod's next start removes it before any role runs.`
    );
  }
  return notes.join("\n");
}
