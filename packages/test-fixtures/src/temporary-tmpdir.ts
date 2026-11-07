import { afterAll, beforeAll } from "bun:test";
import { captureEnvironment, type EnvironmentRestore } from "./environment";
import { temporaryDirectory } from "./temporary-directories";

/**
 * Reserves one directory for the whole test file, points `TMPDIR` at it before the file's first
 * test, and returns its path: every scratch directory a test of the file makes through `tmpdir()`
 * lands inside it, so one `afterAll` removal takes them all, and a file the suite forgot is still
 * gone when the run ends.
 *
 * In `afterAll` it puts `TMPDIR` back to what the caller had, deletes the name again when the
 * caller had none, and removes the directory.
 *
 * Call it at the test file's module scope, above anything that reads `tmpdir()`: it registers its
 * hooks where it is called, and a call inside a test body would point `TMPDIR` at a directory the
 * tests declared before it already ran without. The directory is made, and `TMPDIR` is written,
 * in a `beforeAll`, not at the call: a run that selects no row of the file — `bun test -t
 * <pattern>` — evaluates the file's body and runs neither its `beforeAll` nor its `afterAll`, so
 * a `TMPDIR` written at the call would stay written for every later file of the same process.
 * Nothing exists at the path, and `TMPDIR` still holds the caller's value, until that `beforeAll`
 * has run; a caller that stages something under the root does it in a hook of its own, registered
 * below this call.
 *
 * The path is `realpathSync`'d, so it compares equal to what a child process reports on a machine
 * whose `tmpdir()` is a symlink.
 */
export function temporaryTmpdir(prefix: string): string {
  const root = temporaryDirectory(prefix, { real: true });
  let restore: EnvironmentRestore | undefined;
  beforeAll(() => {
    // `??=`, so a `beforeAll` that runs a second time without its `afterAll` between captures
    // nothing: it would capture the root this fixture set, and the restore would then point
    // every later file of the process at a root this one removed.
    restore ??= captureEnvironment(["TMPDIR"]);
    process.env["TMPDIR"] = root;
  });
  afterAll(() => {
    restore?.();
    restore = undefined;
  });
  return root;
}
