import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { existsSync, realpathSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname } from "node:path";
import { temporaryTmpdir } from "./temporary-tmpdir";

// The value this file's own run started with. The describe below deletes TMPDIR before the
// fixture captures it, because the case under test is the one the fixture has to restore by
// deleting the name again; this hook gives the rest of the run the TMPDIR it started with. Both
// are hooks, so a run that selects no row of this file deletes nothing and restores nothing.
const runnerTmpdir = process.env["TMPDIR"];
afterAll(() => {
  if (runnerTmpdir === undefined) delete process.env["TMPDIR"];
  else process.env["TMPDIR"] = runnerTmpdir;
});

/** The root the describe made, which the test after the describe finds removed. */
let suiteRoot = "";

describe("temporaryTmpdir, called where TMPDIR has no value", () => {
  // The root's parent, resolved where the fixture resolves it: at the call, under the TMPDIR this
  // run started with. The hook below deletes the name, and Bun runs it before the fixture's own
  // hooks because it was registered above them, so the fixture captures TMPDIR unset — the case
  // whose restore has to delete the name again rather than set it empty.
  const parent = realpathSync(tmpdir());
  beforeAll(() => {
    delete process.env["TMPDIR"];
  });
  suiteRoot = temporaryTmpdir("test-fixtures-tmpdir");

  test("points TMPDIR and tmpdir() at the directory it made", () => {
    expect(existsSync(suiteRoot)).toBe(true);
    expect(process.env["TMPDIR"]).toBe(suiteRoot);
    expect(tmpdir()).toBe(suiteRoot);
    expect(dirname(suiteRoot)).toBe(parent);
  });

  test("keeps that directory for the suite's next test", () => {
    expect(tmpdir()).toBe(suiteRoot);
    expect(existsSync(suiteRoot)).toBe(true);
  });
});

test("removed the directory and deleted TMPDIR, which had no value when it was captured", () => {
  expect(suiteRoot).not.toBe("");
  expect(existsSync(suiteRoot)).toBe(false);
  expect(process.env["TMPDIR"]).toBeUndefined();
});
