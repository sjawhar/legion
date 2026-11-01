import { test as register } from "bun:test";

/**
 * The suites in this directory arrived from proof-sdk as standalone scripts: each case ran the
 * moment it was written, in file order, and the file reported its own tally. Running the body
 * here and replaying its outcome in a registered case keeps that execution order — the suites
 * share module-level editor state, so it matters — while `bun test` still reports every case.
 */
export async function test(name: string, fn: () => Promise<void> | void): Promise<void> {
  let failure: unknown;
  let failed = false;
  try {
    await fn();
  } catch (error) {
    failure = error;
    failed = true;
  }
  register(name, () => {
    if (failed) throw failure;
  });
}
