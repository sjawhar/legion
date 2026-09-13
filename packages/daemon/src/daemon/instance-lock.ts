import { dlopen, FFIType, type Library, type Pointer, read as readMemory } from "bun:ffi";
import { closeSync, constants, ftruncateSync, openSync, readFileSync, writeSync } from "node:fs";
import { mkdir } from "node:fs/promises";
import path from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

export interface InstanceLock {
  release(): Promise<void>;
}

const LOCK_EX = 2;
const LOCK_NB = 4;

/** A refused contender's wait for the holder to record its pid. The holder writes it in the same
 * synchronous sequence as its lock call, so this covers scheduling slack, never a protocol step. */
const HOLDER_PID_REREADS = 20;
const HOLDER_PID_REREAD_MS = 25;

interface CLibrary {
  /** Shared object `dlopen` resolves through the loader's own search path. */
  library: string;
  /** The function returning `int *` to the calling thread's errno. */
  errnoSymbol: string;
  /** errno of a non-blocking flock refused because another open file description holds the lock. */
  wouldBlock: number;
  /** open(2) flag value; `fs.constants.O_CLOEXEC` is undefined under Bun, so it is spelled here. */
  cloexec: number;
}

const C_LIBRARIES: Partial<Record<NodeJS.Platform, CLibrary>> = {
  linux: {
    library: "libc.so.6",
    errnoSymbol: "__errno_location",
    wouldBlock: 11,
    cloexec: 0o2000000,
  },
  darwin: {
    library: "libSystem.B.dylib",
    errnoSymbol: "__error",
    wouldBlock: 35,
    cloexec: 0x1000000,
  },
};

interface LibC extends Pick<CLibrary, "wouldBlock" | "cloexec"> {
  flock(fd: number, operation: number): number;
  errno(): number;
}

let libc: LibC | undefined;

/** Binds flock(2) and the errno accessor from the platform's C library, once per process and only
 * when a lock is first taken: the `legion` CLI imports this module for every subcommand and must
 * neither pay for nor risk a `dlopen` at import. */
function loadLibC(): LibC {
  if (libc) return libc;
  const row = C_LIBRARIES[process.platform];
  if (!row) {
    throw new Error(
      `Legion daemon instance lock needs flock(2) from the C library, and no C library name is known for platform ${process.platform}`
    );
  }
  const symbols = {
    flock: { args: [FFIType.i32, FFIType.i32], returns: FFIType.i32 },
    [row.errnoSymbol]: { args: [], returns: FFIType.ptr },
  } as const;
  let library: Library<typeof symbols>;
  try {
    library = dlopen(row.library, symbols);
  } catch (error) {
    throw new Error(
      `Cannot load flock and ${row.errnoSymbol} from ${row.library}: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  const errnoLocation = library.symbols[row.errnoSymbol] as () => Pointer;
  libc = {
    flock: library.symbols.flock,
    errno: () => readMemory.i32(errnoLocation(), 0),
    wouldBlock: row.wouldBlock,
    cloexec: row.cloexec,
  };
  return libc;
}

/** True if `pid` names a live process this user can see (or one owned by another user — still live). */
function processAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return typeof error === "object" && error !== null && "code" in error && error.code === "EPERM";
  }
}

/**
 * Ensures at most one Legion daemon runs per `stateDir` (one per project): two daemons sharing a
 * project would both bind the same durable JetStream consumer, split its messages, and clobber
 * each other's saved state.
 *
 * The guard is the kernel's advisory lock: an exclusive, non-blocking `flock` on an open
 * descriptor of `daemon.lock`, held for the daemon's lifetime and dropped by the kernel the
 * moment the process dies — so a crashed daemon leaves nothing to reclaim. The file is opened
 * without exclusive creation or truncation, and its name is never moved, hard-linked, or removed:
 * every contender's lock attaches to the one inode under that name, and `release()` only closes the
 * descriptor. The pid written into the file is informational, for the refusal message and for
 * humans; the lock is the truth, not the text.
 */
export async function acquireInstanceLock(stateDir: string): Promise<InstanceLock> {
  await mkdir(stateDir, { recursive: true });
  const lockFile = path.join(stateDir, "daemon.lock");
  const libc = loadLibC();
  const fd = openSync(lockFile, constants.O_RDWR | constants.O_CREAT | libc.cloexec, 0o644);
  if (libc.flock(fd, LOCK_EX | LOCK_NB) !== 0) {
    const errno = libc.errno();
    closeSync(fd);
    if (errno !== libc.wouldBlock) throw new Error(`flock(${lockFile}) failed with errno ${errno}`);
    throw await alreadyRunning(lockFile);
  }
  try {
    ftruncateSync(fd, 0);
    writeSync(fd, String(process.pid), 0);
  } catch (error) {
    closeSync(fd);
    throw error;
  }
  let released = false;
  return {
    async release(): Promise<void> {
      // Idempotent: a failed boot probe tears down through `stop()` (which releases) and then
      // rejects into `startDaemon`'s catch, which releases again.
      if (released) return;
      released = true;
      closeSync(fd);
    },
  };
}

/** Another process holds the kernel lock; name it. The holder writes its pid right after locking,
 * so a blank file, or one still naming the previous dead holder, is re-read for a bounded moment. */
async function alreadyRunning(lockFile: string): Promise<Error> {
  let holder = "holder pid not recorded yet";
  for (let attempt = 1; attempt <= HOLDER_PID_REREADS; attempt += 1) {
    const holderPid = Number(readFileSync(lockFile, "utf8").trim());
    if (Number.isSafeInteger(holderPid) && holderPid > 0 && processAlive(holderPid)) {
      holder = `pid ${holderPid}`;
      break;
    }
    if (attempt < HOLDER_PID_REREADS) await sleep(HOLDER_PID_REREAD_MS);
  }
  return new Error(
    `Legion daemon already running for this project (${holder}, lock file ${lockFile})`
  );
}
