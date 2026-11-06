import { afterAll, afterEach, beforeAll } from "bun:test";
import { randomBytes } from "node:crypto";
import { mkdirSync, mkdtempSync, realpathSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

export type TemporaryDirectoryOptions = {
  /**
   * Returns the path under `realpathSync` of `tmpdir()`. A macOS `tmpdir()` is a symlink (`/var`
   * to `/private/var`), so a test that compares a path a child process printed with the path the
   * fixture returned needs the resolved one.
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
 * Call it at the test file's module scope, or in the body of a `describe`: it registers the
 * `afterEach` that removes the directories where it is called, and `bun:test` takes the scope of
 * a hook from the point of the call. The call itself makes nothing, so a scope whose hooks never
 * run leaves nothing behind.
 *
 * **Call the maker itself from a test body or from a hook, never at module scope or in a
 * `describe` body.** The maker makes its directory at the call, and Bun runs a module body and a
 * `describe` body in three cases where it then runs no hook of that scope: a `bun test -t
 * <pattern>` that selects no row of the file, one that selects no row of the `describe`, and
 * `describe.skipIf(true)`. In each of them a maker called there makes its directory and nothing
 * removes it again, and nothing runs at the end of a `bun test` process to catch it.
 * {@link temporaryDirectory} is the form for one directory a whole file or `describe` shares: it
 * reserves the path at the call and makes the directory in a `beforeAll`, so the making and the
 * removal run together or not at all.
 *
 * The fixture's own module body registers nothing, because Bun evaluates an imported module once
 * per process: a hook there would land in the scope of the first file that imported it, and every
 * later file would leave its directories behind.
 *
 * The maker is synchronous, so a test body uses it without `await`.
 */
export function temporaryDirectories(prefix: string): TemporaryDirectoryMaker {
  const made: string[] = [];
  afterEach(() => {
    for (const directory of made.splice(0)) rmSync(directory, { recursive: true, force: true });
  });
  return (suffix) => {
    const directory = mkdtempSync(join(tmpdir(), nameTemplate(prefix, suffix)));
    made.push(directory);
    return directory;
  };
}

/**
 * Reserves one directory for the whole suite and returns its path: the one-shot form of
 * {@link temporaryDirectories}, for a test file or a `describe` body that needs a single scratch
 * directory every one of its tests reads.
 *
 * Call it at the test file's module scope, or in the body of a `describe`, never inside a test
 * body: it registers the `beforeAll` that makes the directory and the `afterAll` that removes it
 * where it is called, and `bun:test` takes the scope of a hook from the point of the call. The
 * path is reserved at the call and nothing is made there, so a scope Bun evaluates and then runs
 * no hook of — a `-t` filter that selects no row of the file or of the `describe`, and
 * `describe.skipIf(true)` — makes nothing and leaves nothing behind.
 *
 * Nothing exists at the path until that `beforeAll` has run. A caller writes into the directory
 * from a test body, or from a hook of its own registered below this call: Bun runs the hooks of a
 * scope in the order they were registered.
 *
 * `real` resolves `tmpdir()` once, at the call, and joins the reserved name onto the resolved
 * parent: `realpathSync` needs a path that exists, and the directory itself does not yet.
 *
 * The name is the prefix, a dash and eight random hexadecimal characters, two more than
 * `mkdtemp` adds. The directory is made exclusively, so `mode` applies to it and a path that
 * somehow exists already fails the file rather than handing two suites one directory.
 */
export function temporaryDirectory(
  prefix: string,
  options: TemporaryDirectoryOptions = {}
): string {
  const parent = options.real === true ? realpathSync(tmpdir()) : tmpdir();
  const directory = join(
    parent,
    `${nameTemplate(prefix, undefined)}${randomBytes(4).toString("hex")}`
  );
  beforeAll(() => {
    mkdirSync(directory, { mode: 0o700 });
  });
  afterAll(() => {
    rmSync(directory, { recursive: true, force: true });
  });
  return directory;
}

/**
 * The name both forms build on: the prefix, the suffix when the caller named one, and one dash,
 * whether or not the prefix already ends with dashes. `temporaryDirectories` hands it to
 * `mkdtemp`, which adds six random characters to it; `temporaryDirectory` adds eight of its own.
 */
function nameTemplate(prefix: string, suffix: string | undefined): string {
  const stem = suffix === undefined || suffix === "" ? prefix : `${prefix}-${suffix}`;
  return `${stem.replace(/-+$/, "")}-`;
}
