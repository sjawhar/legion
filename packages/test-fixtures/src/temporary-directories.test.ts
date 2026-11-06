import { beforeAll, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, realpathSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import { temporaryDirectories, temporaryDirectory } from "./temporary-directories";

const makeDirectory = temporaryDirectories("test-fixtures-per-test");
const makeResolved = temporaryDirectories("test-fixtures-resolved", { real: true });

/** The path the first test made, which the test after it finds removed. */
let fromTheFirstTest = "";
/** The path the describe below made, which the test after the describe finds removed. */
let fromTheDescribe = "";
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

test("real returns the resolved path, where tmpdir() is a symlink", () => {
  const root = makeDirectory("symlinked-tmpdir");
  const target = join(root, "target");
  mkdirSync(target);
  const link = join(root, "link");
  symlinkSync(target, link);
  const previousTmpdir = process.env["TMPDIR"];
  process.env["TMPDIR"] = link;
  try {
    expect(dirname(makeResolved())).toBe(realpathSync(target));
    expect(dirname(makeDirectory("through-the-link"))).toBe(link);
  } finally {
    if (previousTmpdir === undefined) delete process.env["TMPDIR"];
    else process.env["TMPDIR"] = previousTmpdir;
  }
});

describe("a maker whose scope is the suite", () => {
  const makeForTheSuite = temporaryDirectories("test-fixtures-suite", { scope: "suite" });

  test("makes a directory the suite's first test can use", () => {
    fromTheDescribe = makeForTheSuite();
    expect(existsSync(fromTheDescribe)).toBe(true);
  });

  test("keeps that directory for the suite's next test", () => {
    expect(existsSync(fromTheDescribe)).toBe(true);
  });
});

describe("a one-shot directory whose scope is the suite", () => {
  const one = temporaryDirectory("test-fixtures-one-shot");
  // Read in the describe's body, where the call was: the path is reserved there and the directory
  // is made in the `beforeAll` the call registered, so a run that evaluates this body and runs no
  // hook of it makes nothing.
  const existedAtTheCall = existsSync(one);
  const resolved = temporaryDirectory("test-fixtures-one-shot-real", { real: true });

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

  // `realpathSync` needs a path that exists and the reserved directory does not, so `real`
  // resolves the parent — `tmpdir()`, which always exists — and joins the reserved name onto it.
  // The maker's row above reads the same resolution under a `tmpdir()` that is a symlink, which a
  // reserved path cannot: a symlink made in this body would itself outlive a run that skips it.
  test("real joins the reserved name onto the resolved tmpdir()", () => {
    expect(dirname(resolved)).toBe(realpathSync(tmpdir()));
    expect(existsSync(resolved)).toBe(true);
  });
});

test("removed the one-shot directory when its suite ended", () => {
  expect(fromTheOneShot).not.toBe("");
  expect(existsSync(fromTheOneShot)).toBe(false);
});

test("removed the suite's directory when that suite ended", () => {
  expect(fromTheDescribe).not.toBe("");
  expect(existsSync(fromTheDescribe)).toBe(false);
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
