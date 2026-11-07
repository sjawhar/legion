import { beforeAll, describe, expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  linkSync,
  mkdirSync,
  realpathSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import { temporaryDirectories, temporaryDirectory } from "./temporary-directories";

const makeDirectory = temporaryDirectories("test-fixtures-per-test");

/** The path the first test made, which the test after it finds removed. */
let fromTheFirstTest = "";
/** The path the one-shot describe below made, which the test after it finds removed. */
let fromTheOneShot = "";

test("makes a directory under tmpdir(), named after the prefix and the suffix", () => {
  fromTheFirstTest = makeDirectory("first");
  expect(existsSync(fromTheFirstTest)).toBe(true);
  expect(dirname(fromTheFirstTest)).toBe(tmpdir());
  expect(basename(fromTheFirstTest)).toStartWith("test-fixtures-per-test-first-");
});

test("removed the first test's directory after that test", () => {
  expect(fromTheFirstTest).not.toBe("");
  expect(existsSync(fromTheFirstTest)).toBe(false);
});

test("makes a fresh directory on every call", () => {
  const one = makeDirectory("one");
  const other = makeDirectory("other");
  expect(one).not.toBe(other);
  expect(existsSync(one)).toBe(true);
  expect(existsSync(other)).toBe(true);
});

// `temporaryDirectory` resolves `tmpdir()` at the call, in a module body or a `describe` body,
// so this row cannot point the fixture's own `TMPDIR` at a symlink after the call: it runs a
// file of its own in a child whose `TMPDIR` is one, as filtered-run.test.ts runs its lock file.
// Linux's `tmpdir()` is no symlink, so a row that read this process's own would hold with `real`
// ignored.
test("real resolves a TMPDIR that is a symlink", () => {
  const root = makeDirectory("symlinked-tmpdir");
  const target = join(root, "target");
  mkdirSync(target);
  const link = join(root, "link");
  symlinkSync(target, link);
  const fixture = JSON.stringify(join(import.meta.dir, "temporary-directories"));
  const file = join(root, "real.test.ts");
  writeFileSync(
    file,
    `import { expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { dirname } from "node:path";
import { temporaryDirectory } from ${fixture};

const resolved = temporaryDirectory("child-real", { real: true });
const plain = temporaryDirectory("child-plain");

test("prints the parent of each reserved path", () => {
  expect(existsSync(resolved)).toBe(true);
  expect(existsSync(plain)).toBe(true);
  console.log(JSON.stringify({ plain: dirname(plain), resolved: dirname(resolved) }));
});
`
  );
  const run = Bun.spawnSync([process.execPath, "test", file], {
    cwd: root,
    env: { ...process.env, TMPDIR: link },
  });
  const output = `${run.stdout.toString()}${run.stderr.toString()}`;
  // `real` is the whole difference between the two: the plain reservation keeps the symlink the
  // child's `TMPDIR` named, and the resolved one names what the symlink points at.
  expect(output).toContain(JSON.stringify({ plain: link, resolved: realpathSync(target) }));
  expect(run.exitCode).toBe(0);
});

describe("a one-shot directory a whole describe shares", () => {
  const one = temporaryDirectory("test-fixtures-one-shot");
  // Read in the describe's body, where the call was: the path is reserved there and the directory
  // is made in the `beforeAll` the call registered, so a run that evaluates this body and runs no
  // hook of it makes nothing.
  const existedAtTheCall = existsSync(one);

  test("makes one directory under tmpdir(), named after the prefix", () => {
    fromTheOneShot = one;
    expect(existedAtTheCall).toBe(false);
    expect(existsSync(one)).toBe(true);
    expect(dirname(one)).toBe(tmpdir());
    expect(basename(one)).toStartWith("test-fixtures-one-shot-");
  });

  test("keeps that directory for the suite's next test", () => {
    expect(existsSync(one)).toBe(true);
  });
});

test("removed the one-shot directory when its suite ended", () => {
  expect(fromTheOneShot).not.toBe("");
  expect(existsSync(fromTheOneShot)).toBe(false);
});

/** The path the skipped describe below reserved, which no hook of that describe ever makes. */
let fromTheSkippedDescribe = "";
/** The path the describe whose rows are all skipped reserved, which its own hooks make and
 *  remove. */
let fromTheSkippedRows = "";
/** Whether the directory of that describe existed once its `beforeAll` had run. */
let madeForTheSkippedRows = false;

// Bun runs the body of a skipped describe and then no hook that body registered, so the fixture
// reserves its path there and makes nothing: the two rows below read both halves of that, and the
// `-t` filter that selects no row has the same shape (filtered-run.test.ts measures it).
describe.skipIf(true)("a describe that is skipped whole", () => {
  fromTheSkippedDescribe = temporaryDirectory("test-fixtures-skipped-describe");
  test("never runs", () => {
    throw new Error("a skipped describe runs no row of its own");
  });
});

describe("a describe whose rows are all skipped", () => {
  fromTheSkippedRows = temporaryDirectory("test-fixtures-skipped-rows");
  beforeAll(() => {
    madeForTheSkippedRows = existsSync(fromTheSkippedRows);
  });
  test.skip("does not run", () => undefined);
});

test("made nothing for a describe that is skipped whole", () => {
  expect(fromTheSkippedDescribe).not.toBe("");
  expect(existsSync(fromTheSkippedDescribe)).toBe(false);
});

test("made and removed the directory of a describe whose rows were all skipped", () => {
  expect(fromTheSkippedRows).not.toBe("");
  expect(madeForTheSkippedRows).toBe(true);
  expect(existsSync(fromTheSkippedRows)).toBe(false);
});

/**
 * The two behaviours this fixture took from `packages/envoy-client/test/temp-dir.ts` when that
 * package's tests moved onto it (cybermonklabs/legion#2911). Each is read by the row after the
 * one that makes it, because what removes the directory is the maker's own `afterEach`: this is
 * the fixture's real removal, not a copy of it called by hand.
 */
describe("the removal", () => {
  const makeDirectory = temporaryDirectories("test-fixtures-removal");
  const outside = temporaryDirectory("test-fixtures-outside");
  /** The read-only tree the first row makes, which the second finds gone. */
  let readOnlyTree = "";
  /**
   * The tree holding a symlink out of itself, the file that symlink points into, and a read-only
   * directory beside it: a chmod walk that followed the link would make that directory writable,
   * which is the fault this pair of rows exists to catch, since `rmSync` follows no symlink of
   * its own and so deletes nothing out there whatever the walk does.
   */
  let treeWithLink = "";
  let fileOutside = "";
  let readOnlyOutside = "";
  /**
   * The tree holding a hard link to a file outside it, and that file: a mode the removal wrote
   * on the link would be that file's own, since the two names are one inode.
   */
  let treeWithHardLink = "";
  let linkedFile = "";

  test("a tree whose directories and files are read-only is made", () => {
    readOnlyTree = makeDirectory("read-only");
    const inner = join(readOnlyTree, "cache", "download");
    mkdirSync(inner, { recursive: true });
    writeFileSync(join(inner, "module.zip"), "x", { mode: 0o444 });
    chmodSync(inner, 0o555);
    chmodSync(dirname(inner), 0o555);
    expect(existsSync(inner)).toBe(true);
  });

  test("and is removed all the same, where rmSync alone fails on it", () => {
    expect(readOnlyTree).not.toBe("");
    expect(existsSync(readOnlyTree)).toBe(false);
  });

  test("a tree holding a symlink to a tree the test does not own is made", () => {
    treeWithLink = makeDirectory("symlink");
    fileOutside = join(outside, "kept.txt");
    writeFileSync(fileOutside, "kept");
    readOnlyOutside = join(outside, "sealed");
    mkdirSync(readOnlyOutside);
    chmodSync(readOnlyOutside, 0o555);
    symlinkSync(outside, join(treeWithLink, "link"));
    expect(existsSync(join(treeWithLink, "link", "kept.txt"))).toBe(true);
    expect(statSync(readOnlyOutside).mode & 0o777).toBe(0o555);
  });

  test("and the removal neither deletes through that symlink nor chmods through it", () => {
    expect(treeWithLink).not.toBe("");
    expect(existsSync(treeWithLink)).toBe(false);
    expect(existsSync(fileOutside)).toBe(true);
    expect(statSync(readOnlyOutside).mode & 0o777).toBe(0o555);
  });

  test("a tree holding a hard link to a file outside it is made", () => {
    treeWithHardLink = makeDirectory("hard-link");
    linkedFile = join(outside, "fixture.txt");
    writeFileSync(linkedFile, "fixture", { mode: 0o444 });
    linkSync(linkedFile, join(treeWithHardLink, "same-inode.txt"));
    expect(statSync(linkedFile).nlink).toBe(2);
  });

  test("and the removal leaves that file's own mode alone", () => {
    expect(treeWithHardLink).not.toBe("");
    expect(existsSync(treeWithHardLink)).toBe(false);
    expect(statSync(linkedFile).mode & 0o777).toBe(0o444);
  });
});

/**
 * One directory the removal cannot remove must not keep the rest on disk. The hook is what
 * removes, and a hook that throws fails the file it runs in, so this reads a `bun test` of its
 * own: a file whose maker made three directories under a root made read-only before the hook
 * ran, where every unlink fails for want of write permission on that root. The run must name all
 * three rather than the first, which is what tells "attempted every one, reported together" from
 * "stopped at the first and left the others".
 */
describe("a removal that cannot finish", () => {
  const makeDirectory = temporaryDirectories("test-fixtures-batch");

  test("names every directory it could not remove, not only the first", () => {
    const work = makeDirectory("run");
    const root = join(work, "root");
    mkdirSync(root);
    writeFileSync(
      join(work, "batch.test.ts"),
      `import { afterAll, test } from "bun:test";
import { chmodSync } from "node:fs";
import { temporaryDirectories } from ${JSON.stringify(join(import.meta.dir, "temporary-directories"))};

const make = temporaryDirectories("left", { root: ${JSON.stringify(root)} });

test("makes three and seals the root", () => {
  make("one");
  make("two");
  make("three");
  // Sealed after the directories are made and before the fixture's afterEach runs: unlinking a
  // directory needs write permission on the directory holding it, so all three now refuse.
  chmodSync(${JSON.stringify(root)}, 0o555);
});

// Registered after the fixture's own afterEach, so it runs after it: the root is writable again
// for the outer suite that owns this scratch.
afterAll(() => chmodSync(${JSON.stringify(root)}, 0o755));
`
    );
    const run = Bun.spawnSync([process.execPath, "test", "./batch.test.ts"], {
      cwd: work,
      env: { ...process.env },
    });
    const output = `${run.stdout.toString()}${run.stderr.toString()}`;

    expect(output).toContain("temporary directories left behind:");
    for (const name of ["left-one-", "left-two-", "left-three-"]) {
      expect(output).toContain(name);
    }
    expect(run.exitCode).not.toBe(0);
  });
});
