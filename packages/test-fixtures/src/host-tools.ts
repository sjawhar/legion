/** The path of the program on this host's `PATH`, or null when the host does not have it. */
export function hostTool(name: string): string | null {
  return Bun.which(name);
}

/**
 * Whether this host is missing any of the named programs, for `describe.skipIf(...)` and
 * `test.skipIf(...)`. It prints one warning that names every missing program and the file whose
 * tests will not run, so a reader of the CI output knows what to install. With no names it is
 * false and prints nothing.
 *
 * Call it once, at the test file's module scope, and hand the result to every `skipIf` of the
 * file: a call per test would print the warning per test.
 */
export function skipWithout(...names: string[]): boolean {
  const missing = names.filter((name) => hostTool(name) === null);
  if (missing.length === 0) return false;
  const file = callerFile(new Error().stack);
  const where = file === undefined ? "in this file" : `in ${file}`;
  console.warn(
    `Skipped the tests ${where}. This host does not have: ${missing.join(", ")}. ` +
      "Install the missing programs and run the tests again."
  );
  return true;
}

/**
 * The file that called `skipWithout`: the first frame of the stack that is not this module's own.
 * Without it the warning says only which programs are missing, and a reader of a run of hundreds of
 * files cannot tell whose tests did not run.
 *
 * A frame is `at <name> (<path>:<line>:<col>)`, or `at <path>:<line>:<col>` where it has no name.
 * The path is read whole, from the parenthesis or from `at ` up to the last `:<line>:<col>` of the
 * frame, so a checkout under a directory whose name holds a space — `/home/u/my repo/pkg/a.test.ts`
 * — is named as it is rather than cut at the space.
 */
function callerFile(stack: string | undefined): string | undefined {
  if (stack === undefined) return undefined;
  for (const line of stack.split("\n")) {
    const frame = line.trimEnd();
    const file = /\((.+):\d+:\d+\)$/.exec(frame)?.[1] ?? /^\s*at (.+):\d+:\d+$/.exec(frame)?.[1];
    if (file !== undefined && file !== import.meta.path) return file;
  }
  return undefined;
}
