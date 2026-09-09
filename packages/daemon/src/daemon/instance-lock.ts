import { mkdir, readFile, rename, unlink, writeFile } from "node:fs/promises";
import path from "node:path";

export interface InstanceLock {
  release(): Promise<void>;
}

function hasErrnoCode(error: unknown, code: string): boolean {
  return typeof error === "object" && error !== null && "code" in error && error.code === code;
}

/** True if `pid` names a live process this user can see (or one owned by another user — still live). */
function processAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return hasErrnoCode(error, "EPERM");
  }
}

/**
 * Ensures at most one Legion daemon runs per `stateDir` (one per project):
 * two daemons sharing a project would both bind the same durable JetStream
 * consumer, split its messages, and clobber each other's saved state. A
 * pidfile with exclusive creation (`wx`) makes acquisition atomic; a lock
 * left behind by a crashed process is detected by checking whether its pid
 * is still alive.
 *
 * Reclaiming a stale lock is itself race-free: rather than unlinking it
 * directly (two contenders could both see it as stale and both proceed,
 * each believing it alone removed it), each contender first `rename`s it
 * to a name unique to its own pid. `rename` is atomic, so at most one
 * contender's rename can succeed against the original path; that
 * contender alone unlinks the file (under its new name) and retries the
 * `wx` create. A contender whose rename fails with `ENOENT` lost the race
 * — the file it looked at is already gone — and simply retries the `wx`
 * create too, which will see either the winner's fresh live lock (correct
 * "already running" outcome) or an empty slot (if the winner's own daemon
 * has since exited).
 */
export async function acquireInstanceLock(stateDir: string): Promise<InstanceLock> {
  await mkdir(stateDir, { recursive: true });
  const lockFile = path.join(stateDir, "daemon.lock");
  const pid = process.pid;

  for (let attempt = 0; attempt < 2; attempt += 1) {
    try {
      await writeFile(lockFile, String(pid), { encoding: "utf8", flag: "wx" });
      return {
        async release(): Promise<void> {
          // Verify the file still holds our pid before unlinking: a stale
          // takeover elsewhere may have since replaced it with another
          // daemon's live lock.
          const current = await readFile(lockFile, "utf8").catch(() => "");
          if (current.trim() !== String(pid)) return;
          try {
            await unlink(lockFile);
          } catch (error) {
            if (!hasErrnoCode(error, "ENOENT")) throw error;
          }
        },
      };
    } catch (error) {
      if (!hasErrnoCode(error, "EEXIST")) throw error;
      const existingPidText = await readFile(lockFile, "utf8").catch(() => "");
      const existingPid = Number(existingPidText.trim());
      if (Number.isSafeInteger(existingPid) && existingPid > 0 && processAlive(existingPid)) {
        throw new Error(
          `Legion daemon already running for this project (pid ${existingPid}, lock file ${lockFile})`
        );
      }
      const staleName = `${lockFile}.stale-${pid}`;
      try {
        await rename(lockFile, staleName);
      } catch (renameError) {
        if (!hasErrnoCode(renameError, "ENOENT")) throw renameError;
        continue; // lost the race to reclaim it; just retry the wx create
      }
      await unlink(staleName).catch((unlinkError) => {
        if (!hasErrnoCode(unlinkError, "ENOENT")) throw unlinkError;
      });
    }
  }
  throw new Error(`Failed to acquire Legion daemon instance lock: ${lockFile}`);
}
