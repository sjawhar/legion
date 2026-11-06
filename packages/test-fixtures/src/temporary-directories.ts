import { afterAll, afterEach } from "bun:test";
import { mkdtempSync, realpathSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/** When the directories a maker made are removed: after each test, or after the whole suite. */
export type TemporaryDirectoryScope = "test" | "suite";

export type TemporaryDirectoryOptions = {
  /**
   * `"test"`, the default, removes every directory the maker made in `afterEach`. `"suite"` removes
   * them in `afterAll`, for a directory a test file makes at module scope or in a `describe` body
   * and every test of that scope reads.
   */
  scope?: TemporaryDirectoryScope;
  /**
   * Returns `realpathSync` of the directory. A macOS `tmpdir()` is a symlink (`/var` to
   * `/private/var`), so a test that compares a path a child process printed with the path the
   * maker returned needs the resolved one.
   */
  real?: boolean;
};

/**
 * Makes one fresh directory and returns its path. `suffix` goes into the directory's name, so a
 * test that makes several says which is which.
 */
export type TemporaryDirectoryMaker = (suffix?: string) => string;

/**
 * Registers the removal of every directory the returned maker makes, and returns the maker.
 *
 * Call it at the test file's module scope or in a `describe` body, never from inside a test body:
 * it registers an `afterEach` or `afterAll` hook where it is called, and `bun:test` takes the scope
 * of a hook from the point of the call. The fixture's own module body registers nothing, because
 * Bun evaluates an imported module once per process: a hook there would land in the scope of the
 * first file that imported it, and every later file would leave its directories behind.
 *
 * The maker is synchronous, so a test body uses it without `await`.
 */
export function temporaryDirectories(
  prefix: string,
  options: TemporaryDirectoryOptions = {}
): TemporaryDirectoryMaker {
  const made: string[] = [];
  const remove = (): void => {
    for (const directory of made.splice(0)) rmSync(directory, { recursive: true, force: true });
  };
  if (options.scope === "suite") afterAll(remove);
  else afterEach(remove);
  return (suffix) => {
    const directory = mkdtempSync(join(tmpdir(), nameTemplate(prefix, suffix)));
    made.push(directory);
    return options.real === true ? realpathSync(directory) : directory;
  };
}

/**
 * The template `mkdtemp` adds six random characters to: the prefix, the suffix when the caller
 * named one, and one dash, whether or not the prefix already ends with dashes.
 */
function nameTemplate(prefix: string, suffix: string | undefined): string {
  const stem = suffix === undefined || suffix === "" ? prefix : `${prefix}-${suffix}`;
  return `${stem.replace(/-+$/, "")}-`;
}
