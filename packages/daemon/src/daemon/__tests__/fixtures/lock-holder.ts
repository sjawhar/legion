// Holds the Legion daemon instance lock from a separate process for instance-lock.test.ts.
//
//   bun lock-holder.ts <stateDir>        acquire through the real module, print "locked", stay alive
//   bun lock-holder.ts <stateDir> --raw  take the kernel lock directly and print "locked" without
//                                        recording a pid; write this pid only once "go" arrives on
//                                        stdin — a holder the parent can observe mid-acquisition
//
// Either way the process lives until its parent kills it or closes its stdin.

import { dlopen, FFIType } from "bun:ffi";
import { constants, ftruncateSync, openSync, writeSync } from "node:fs";
import path from "node:path";
import { acquireInstanceLock } from "../../instance-lock";

const [stateDir, mode] = process.argv.slice(2);
if (!stateDir) throw new Error("usage: lock-holder.ts <stateDir> [--raw]");

let recordPid: (() => void) | undefined;
if (mode === "--raw") {
  const library = process.platform === "darwin" ? "libSystem.B.dylib" : "libc.so.6";
  const { symbols } = dlopen(library, {
    flock: { args: [FFIType.i32, FFIType.i32], returns: FFIType.i32 },
  });
  const fd = openSync(
    path.join(stateDir, "daemon.lock"),
    constants.O_RDWR | constants.O_CREAT,
    0o644
  );
  if (symbols.flock(fd, 2 | 4) !== 0) throw new Error("raw holder could not take the lock");
  recordPid = () => {
    ftruncateSync(fd, 0);
    writeSync(fd, String(process.pid), 0);
  };
} else {
  await acquireInstanceLock(stateDir);
}
console.log("locked");

for await (const chunk of Bun.stdin.stream()) {
  if (new TextDecoder().decode(chunk).includes("go")) recordPid?.();
}
