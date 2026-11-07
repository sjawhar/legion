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
 * leaves the next file the caller's own environment.
 *
 * The `afterEach` undoes what a test set, after every test. The `afterAll` is for what no
 * `afterEach` runs after: a file whose rows are each `.skip`, which runs both of these hooks and
 * no `afterEach` at all, and anything set once the last test's `afterEach` has run.
 *
 * A file sets one of these names in a test or in a hook, never at module scope: `bun test -t
 * <pattern>` that selects no row of the file evaluates the module body and runs neither hook, so
 * a name set there is still set for every later file of the process (measured: a file that sets
 * `TQ3_NAME` at module scope, and a later file that reads `set at module scope` under `-t
 * later`).
 *
 * A name a `describe` sets for its own tests — in its body or in its `beforeAll` — must not be in
 * the captured list: the `afterEach` puts the captured value back after that describe's first
 * test, and every test of it after the first would run without the value the describe set. Such a
 * name is the describe's own to set, in a `beforeEach` of its own.
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
