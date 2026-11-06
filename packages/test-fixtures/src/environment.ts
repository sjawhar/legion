import { afterAll, afterEach } from "bun:test";

/** Puts every captured name back to the value it held when it was captured. */
export type EnvironmentRestore = () => void;

/**
 * Captures the value of each name now and returns the function that puts them back, for a
 * `try`/`finally` inside one test. It registers no hook, so a test body may call it.
 *
 * A name that holds no value is captured as unset, and the restore deletes it again: setting it to
 * the empty string instead would make a later read of it find a value the caller never had.
 */
export function captureEnvironment(names: readonly string[]): EnvironmentRestore {
  const captured = names.map((name) => [name, process.env[name]] as const);
  return () => {
    for (const [name, value] of captured) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
  };
}

/**
 * Captures the value of each name now and puts them back in `afterEach` and in `afterAll`, so a
 * test that sets one of them needs no save and restore of its own, and a test that throws still
 * leaves the next file the caller's own environment. The `afterAll` covers a name the file's module
 * scope or a `describe` body set, which no `afterEach` would reach.
 *
 * Call it at the test file's module scope, before anything sets the names: it registers its hooks
 * where it is called, and it captures what the names hold at the moment of the call.
 *
 * Set and delete the names themselves, as this does; never replace `process.env` with a copy to
 * restore it. That swaps the runtime's environment object for a plain one, so later writes reach
 * neither the real environment nor `Bun.env`, which other packages read, and `bun test` runs every
 * package's files in one process.
 */
export function restoreEnvironment(names: readonly string[]): void {
  const restore = captureEnvironment(names);
  afterEach(restore);
  afterAll(restore);
}
