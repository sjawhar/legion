import { afterAll } from "bun:test";
import { captureEnvironment } from "./environment";
import { temporaryDirectories } from "./temporary-directories";

/**
 * Makes one directory for the whole test file, points `TMPDIR` at it, and returns its path: every
 * scratch directory a test of the file makes through `tmpdir()` lands inside it, so one `afterAll`
 * removal takes them all, and a file the suite forgot is still gone when the run ends.
 *
 * In `afterAll` it puts `TMPDIR` back to what the caller had, deletes the name again when the
 * caller had none, and removes the directory.
 *
 * Call it at the test file's module scope, before any test reads `tmpdir()`: it registers its hook
 * where it is called, and a call inside a test body would point `TMPDIR` at a directory the tests
 * declared before it already ran without.
 *
 * The path is `realpathSync`'d, so it compares equal to what a child process reports on a machine
 * whose `tmpdir()` is a symlink.
 */
export function temporaryTmpdir(prefix: string): string {
  const root = temporaryDirectories(prefix, { scope: "suite", real: true })();
  const restore = captureEnvironment(["TMPDIR"]);
  process.env["TMPDIR"] = root;
  afterAll(restore);
  return root;
}
