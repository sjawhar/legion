import { describe, expect, test } from "bun:test";
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
  // The same tmpdir() symlink the maker's `real` is read under, pointed at while the two
  // directories below are made and put back before anything else runs.
  const root = temporaryDirectory("test-fixtures-one-shot-root");
  const target = join(root, "target");
  mkdirSync(target);
  const link = join(root, "link");
  symlinkSync(target, link);
  const previousTmpdir = process.env["TMPDIR"];
  process.env["TMPDIR"] = link;
  const resolved = temporaryDirectory("test-fixtures-one-shot-real", { real: true });
  const throughTheLink = temporaryDirectory("test-fixtures-one-shot-link");
  if (previousTmpdir === undefined) delete process.env["TMPDIR"];
  else process.env["TMPDIR"] = previousTmpdir;

  test("makes one directory under tmpdir(), named after the prefix", () => {
    fromTheOneShot = one;
    expect(existsSync(one)).toBe(true);
    expect(dirname(one)).toBe(tmpdir());
    expect(basename(one)).toStartWith("test-fixtures-one-shot-");
  });

  test("keeps that directory for the suite's next test", () => {
    expect(existsSync(one)).toBe(true);
  });

  test("real returns the resolved path, where tmpdir() is a symlink", () => {
    expect(dirname(resolved)).toBe(realpathSync(target));
    expect(dirname(throughTheLink)).toBe(link);
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
