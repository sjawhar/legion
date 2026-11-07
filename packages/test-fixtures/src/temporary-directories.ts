import { afterAll, afterEach, beforeAll } from "bun:test";
import { randomBytes } from "node:crypto";
import {
  chmodSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  realpathSync,
  rmSync,
  type Stats,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/**
 * The root both forms take: the directory the temporary one is made in, `tmpdir()` by default.
 * It is for a test that needs another filesystem, such as `/dev/shm` for a rename that must
 * cross one.
 */
export type TemporaryRootOption = {
  root?: string;
};

/** What {@link temporaryDirectories} takes: the root alone, since `mkdtemp` answers each path. */
export type TemporaryDirectoriesOptions = TemporaryRootOption;

/** What {@link temporaryDirectory} takes: the root, and whether to resolve it. */
export type TemporaryDirectoryOptions = TemporaryRootOption & {
  /**
   * Returns the path under `realpathSync` of the root. A macOS `tmpdir()` is a symlink (`/var`
   * to `/private/var`), so a test that compares a path a child process printed with the path the
   * fixture returned needs the resolved one. The one-shot form alone takes it: it builds its own
   * path, where the maker's paths are `mkdtemp`'s.
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
 *
 * `options.root` is the directory the temporary ones are made in, for a test that needs another
 * filesystem.
 *
 * The `afterEach` attempts every directory even when one of them cannot be removed, since the
 * directories are independent and one refusal must not leave the rest on disk, and reports the
 * refusals together at the end.
 */
export function temporaryDirectories(
  prefix: string,
  options: TemporaryDirectoriesOptions = {}
): TemporaryDirectoryMaker {
  const root = options.root ?? tmpdir();
  const made: string[] = [];
  afterEach(() => {
    removeEvery(made.splice(0));
  });
  return (suffix) => {
    const directory = mkdtempSync(join(root, nameTemplate(prefix, suffix)));
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
 * `real` resolves the root once, at the call, and joins the reserved name onto the resolved
 * parent: `realpathSync` needs a path that exists, and the directory itself does not yet.
 * `root` is the directory it is made in, `tmpdir()` by default.
 *
 * The name is the prefix, a dash and eight random hexadecimal characters, two more than
 * `mkdtemp` adds. The directory is made exclusively, so `mode` applies to it and a path that
 * somehow exists already fails the file rather than handing two suites one directory.
 */
export function temporaryDirectory(
  prefix: string,
  options: TemporaryDirectoryOptions = {}
): string {
  const root = options.root ?? tmpdir();
  const parent = options.real === true ? realpathSync(root) : root;
  const directory = join(
    parent,
    `${nameTemplate(prefix, undefined)}${randomBytes(4).toString("hex")}`
  );
  beforeAll(() => {
    mkdirSync(directory, { mode: 0o700 });
  });
  afterAll(() => {
    removeTree(directory);
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

/**
 * Removes every directory of `directories`, newest first, so one made inside another goes before
 * the directory holding it. Every tree is attempted even when one of them refuses, since the
 * directories are independent and one refusal must not leave the rest on disk, and the refusals
 * are reported together at the end: a hook that stopped at the first failure would leave every
 * later directory behind and name only the one it stopped on (the behaviour
 * `packages/envoy-client/test/temp-dir.ts` had, which this fixture took over from it).
 */
function removeEvery(directories: readonly string[]): void {
  const failures: string[] = [];
  for (const directory of [...directories].reverse()) {
    try {
      removeTree(directory);
    } catch (error) {
      failures.push(`${directory}: ${error instanceof Error ? error.message : String(error)}`);
    }
  }
  if (failures.length > 0) {
    throw new Error(`temporary directories left behind:\n  ${failures.join("\n  ")}`);
  }
}

/**
 * Removes one tree, making every directory in it writable, searchable and readable first.
 * `rmSync` alone fails on a tree a test made read-only on purpose, and a test can make one
 * without meaning to: Go's module cache writes 0555 directories and 0444 files, so a suite that
 * runs `go` under its temporary directory leaves a tree nothing can unlink.
 */
function removeTree(directory: string): void {
  makeWritable(directory);
  rmSync(directory, { recursive: true, force: true });
}

/** `chmod -R u+rwx` on a tree's directories, following no symlink. A path already gone is
 * nothing to do. */
function makeWritable(path: string): void {
  const entry = makeSelfWritable(path);
  if (entry === undefined || !entry.isDirectory()) return;
  let entries: string[];
  try {
    entries = readdirSync(path);
  } catch {
    return;
  }
  for (const name of entries) makeWritable(join(path, name));
}

/**
 * `chmod u+rwx` on one path when it is a directory, following no symlink. Answers what it found
 * there, or nothing for a path that is gone or a symlink.
 *
 * A file's own mode is left alone: unlinking a file needs write permission on the directory
 * holding it, never on the file, and a file in a temporary tree can be a hard link to one
 * outside it, whose mode is the same inode's.
 */
function makeSelfWritable(path: string): Stats | undefined {
  let entry: Stats;
  try {
    entry = lstatSync(path);
  } catch {
    return undefined;
  }
  if (entry.isSymbolicLink()) return undefined;
  if (!entry.isDirectory()) return entry;
  const mode = entry.mode & 0o7777;
  const wanted = mode | 0o700;
  if (wanted !== mode) {
    try {
      chmodSync(path, wanted);
    } catch {
      // A mode this user cannot set is left to rmSync to report.
    }
  }
  return entry;
}
