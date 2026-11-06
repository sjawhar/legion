import { beforeAll, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, realpathSync, symlinkSync, writeFileSync } from "node:fs";
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
