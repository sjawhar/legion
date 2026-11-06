import { expect, test } from "bun:test";
import { mkdirSync, readdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "./temporary-directories";

// The regression lock for the whole class this package's fixtures guard against: under
// `bun test -t <pattern>` that selects no row of a file, Bun 1.4.4 evaluates that file's body and
// the body of every `describe` in it, and runs neither the `beforeAll` nor the `afterAll` those
// bodies registered. Nothing runs at the end of the process either — neither `process.once("exit")`
// nor `"beforeExit"` runs under `bun test` — so anything a fixture makes at the call, rather than
// in a hook, survives the run. The rows below run a file that takes every scratch this package
// hands out, once with a filter that selects nothing and once with no filter at all, each under a
// TMPDIR of its own, and read that TMPDIR afterwards.

const makeDirectory = temporaryDirectories("test-fixtures-filtered-run");

/** The marker the fixture file prints as its module body runs, so a run that made nothing is told
 *  apart from a run that never loaded the file. */
const EVALUATED = "the fixture file was evaluated";

/** A test file that calls every fixture of this package at the two scopes a filtered run
 *  evaluates and runs no hook of: the file's own body, and a `describe` body. */
function fixtureSource(): string {
  const directories = JSON.stringify(join(import.meta.dir, "temporary-directories.ts"));
  const tmpdirFixture = JSON.stringify(join(import.meta.dir, "temporary-tmpdir.ts"));
  return `import { beforeAll, describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories, temporaryDirectory } from ${directories};
import { temporaryTmpdir } from ${tmpdirFixture};

console.log(${JSON.stringify(EVALUATED)});

const root = temporaryTmpdir("lock-tmpdir");
const forTheFile = temporaryDirectory("lock-file");
const makeDirectory = temporaryDirectories("lock-maker");

beforeAll(() => {
  writeFileSync(join(root, "staged"), "x");
});

describe("a describe of its own", () => {
  const forTheDescribe = temporaryDirectory("lock-describe");
  test("alpha row", () => {
    writeFileSync(join(forTheDescribe, "wrote"), "x");
    expect(forTheDescribe).not.toBe("");
  });
});

test("beta row", () => {
  writeFileSync(join(forTheFile, "wrote"), "x");
  writeFileSync(join(makeDirectory("row"), "wrote"), "x");
  expect(process.env["TMPDIR"]).toBe(root);
});
`;
}

/** Runs the fixture file under `tmp` as its TMPDIR, and returns what the run said and left. */
function runFixture(filter: string | null): {
  readonly output: string;
  readonly exitCode: number | null;
  readonly left: string[];
} {
  const work = makeDirectory(filter === null ? "unfiltered" : "filtered");
  const file = join(work, "lock.test.ts");
  writeFileSync(file, fixtureSource());
  const tmp = join(work, "tmp");
  mkdirSync(tmp);
  const argv = [
    process.execPath,
    "test",
    "./lock.test.ts",
    ...(filter === null ? [] : ["-t", filter]),
  ];
  const run = Bun.spawnSync(argv, { cwd: work, env: { ...process.env, TMPDIR: tmp } });
  return {
    output: `${run.stdout.toString()}${run.stderr.toString()}`,
    exitCode: run.exitCode,
    left: readdirSync(tmp),
  };
}

test("a filtered run that selects no row evaluates the file and leaves its TMPDIR empty", () => {
  const run = runFixture("zzz-matches-nothing");
  expect(run.output).toContain(EVALUATED);
  expect(run.left).toEqual([]);
});

test("the same file unfiltered makes its directories and removes them again", () => {
  const run = runFixture(null);
  expect(run.output).toContain(EVALUATED);
  expect(run.output).toContain("2 pass");
  expect(run.exitCode).toBe(0);
  expect(run.left).toEqual([]);
});
