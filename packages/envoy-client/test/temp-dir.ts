/**
 * The temporary directory a test makes, removed when the process that made it ends.
 *
 * `mkdtempSync` alone leaves its directory behind: measured on a shared development machine on
 * 2026-11-07, its temporary directory held 709 `dispatch-cli-*` directories (598 MiB) and 680
 * `dispatch-config-*` (175 MiB), left by this package's own test files across many sessions,
 * among 7,063 directories of six such families. A test that removes its directory in the test body
 * keeps it on every failure and every early return, so the removal is registered with the process
 * instead: whatever a test does, the directory goes when the process exits.
 *
 * Two details, both learned from removals that reported success and freed nothing:
 *
 *   - a tree the caller owns outright still defeats `rm -rf` when its directories are not writable
 *     (Go's module cache writes 555 and 444 on purpose), so every directory gets `u+rwX` first;
 *   - a symlink is never followed, so a test that links to a tree it does not own cannot have the
 *     removal reach through it.
 */
import { chmodSync, lstatSync, mkdtempSync, readdirSync, rmSync, type Stats } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";

/** Every directory this process made and has not removed yet, oldest first. */
const made: string[] = [];
let removalRegistered = false;

/**
 * A fresh directory named `<prefix><random>`, under the system temporary directory or under
 * `root`, which this process removes before it exits. `prefix` ends in `-` by convention, as
 * `mkdtemp`'s callers here already write it; `root` is for a test that needs another filesystem,
 * such as `/dev/shm`.
 *
 * Bun's test runner (1.4.4) emits neither `exit` nor `beforeExit`, so under `bun test` the
 * registration below never runs: every test file that makes one of these calls
 * `afterAll(removeTemporaryDirectories)` of its own, and `temp-dir.test.ts` fails when a file in
 * this package makes a temporary directory without it.
 */
export function temporaryDirectory(prefix: string, root: string = tmpdir()): string {
  const directory = mkdtempSync(join(root, prefix));
  made.push(directory);
  if (!removalRegistered) {
    // Synchronous work only: an exit listener runs with no event loop left. This covers a plain
    // process (a command, a script); see above for what covers the test runner. It reports a
    // removal it could not make rather than throwing out of the exit, which node prints as a
    // crash of a process that in fact did its work.
    process.on("exit", () => {
      try {
        removeTemporaryDirectories();
      } catch (error) {
        process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
      }
    });
    removalRegistered = true;
  }
  return directory;
}

/**
 * Removes every directory `temporaryDirectory` made and has not removed, newest first — a
 * directory made inside another one therefore goes before the directory holding it. The process
 * exit runs this; a test calls it to assert the removal, or to free a large tree early.
 *
 * Every tree is attempted even when one fails, since the directories are independent and one
 * refusal must not keep the rest on disk; the failures are reported together at the end.
 */
export function removeTemporaryDirectories(): void {
  const failures: string[] = [];
  for (let directory = made.pop(); directory !== undefined; directory = made.pop()) {
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
 * Removes one tree, making each directory in it writable, searchable and readable first — and
 * the directory holding it too when that one is ours, since unlinking a directory needs write
 * permission on its parent and `temporaryDirectory`'s `root` may be a tree this helper made
 * read-only to test it.
 */
function removeTree(directory: string): void {
  const parent = dirname(directory);
  if (made.includes(parent)) {
    makeSelfWritable(parent);
  }
  makeWritable(directory);
  rmSync(directory, { recursive: true, force: true });
}

/** `chmod -R u+rwx` on a tree's DIRECTORIES, following no symlink. A path already gone is
 * nothing to do. */
function makeWritable(path: string): void {
  const entry = makeSelfWritable(path);
  if (entry === undefined || !entry.isDirectory()) {
    return;
  }
  let entries: string[];
  try {
    entries = readdirSync(path);
  } catch {
    return;
  }
  for (const name of entries) {
    makeWritable(join(path, name));
  }
}

/**
 * `chmod u+rwx` on one path when it is a directory, following no symlink. Returns what it found
 * there, or nothing for a path that is gone or a symlink.
 *
 * A file's own mode is left alone: unlinking a file needs write permission on the directory
 * holding it, never on the file, and a file in a temporary tree can be a hard link to one
 * outside it, whose mode is the same inode's (an external 0444 fixture came back 0644 while this
 * function wrote `u+rw` on files too).
 */
function makeSelfWritable(path: string): Stats | undefined {
  let entry: Stats;
  try {
    entry = lstatSync(path);
  } catch {
    return undefined;
  }
  if (entry.isSymbolicLink()) {
    return undefined;
  }
  if (!entry.isDirectory()) {
    return entry;
  }
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
