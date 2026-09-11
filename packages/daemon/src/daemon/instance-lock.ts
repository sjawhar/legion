import { randomUUID } from "node:crypto";
import { link, mkdir, readFile, rename, unlink, writeFile } from "node:fs/promises";
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
 * Reclaiming a stale lock moves it to a unique name atomically, then verifies the moved file still
 * contains the stale value that was read. If another contender replaced the lock meanwhile, its
 * live lock is restored with an exclusive hard link rather than overwritten; only the owner of an
 * unchanged stale inode removes it before every contender retries exclusive creation.
 */
export async function acquireInstanceLock(stateDir: string): Promise<InstanceLock> {
  await mkdir(stateDir, { recursive: true });
  const lockFile = path.join(stateDir, "daemon.lock");
  const pid = process.pid;

  // Two attempts is not a proven-sufficient bound: a reclaim that discovers the lock changed
  // between this contender's own stale-read and its own `rename` (another contender's fresh,
  // live write raced in) has made no failed attempt of its own -- it is real progress by
  // someone else -- yet the loop below still counts it against this budget before retrying.
  // Under real scheduling variance (observed to fail under CPU/IO contention), two genuinely
  // racing contenders can each need more than one such restore-and-retry cycle before either
  // side's own `writeFile` lands cleanly on a definitive live-pid EEXIST. Each cycle is a cheap,
  // local, already-bounded fs round trip (this runs once at daemon startup, never a hot path),
  // so a generous fixed bound costs nothing while comfortably covering every interleaving two
  // contenders can produce.
  for (let attempt = 0; attempt < 8; attempt += 1) {
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
      const staleName = `${lockFile}.stale-${pid}-${randomUUID()}`;
      try {
        await rename(lockFile, staleName);
      } catch (renameError) {
        if (!hasErrnoCode(renameError, "ENOENT")) throw renameError;
        continue;
      }
      const staleText = await readFile(staleName, "utf8").catch(() => "");
      const stalePid = Number(staleText.trim());
      if (
        staleText !== existingPidText ||
        (Number.isSafeInteger(stalePid) && stalePid > 0 && processAlive(stalePid))
      ) {
        try {
          await link(staleName, lockFile);
        } catch (restoreError) {
          if (!hasErrnoCode(restoreError, "EEXIST")) throw restoreError;
        }
        await unlink(staleName).catch((unlinkError) => {
          if (!hasErrnoCode(unlinkError, "ENOENT")) throw unlinkError;
        });
        continue;
      }
      await unlink(staleName).catch((unlinkError) => {
        if (!hasErrnoCode(unlinkError, "ENOENT")) throw unlinkError;
      });
    }
  }
  throw new Error(`Failed to acquire Legion daemon instance lock: ${lockFile}`);
}
