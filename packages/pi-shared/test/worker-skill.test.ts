// The one rule written over the repository's own skills/, rather than over a plugin's staged
// partition: it holds whichever plugin ships the worker skill, so it is declared once, here,
// rather than in skills-guard.ts, which five test files import and which would run it once per
// importer.
import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { retiredWorkerLifecycleClaims, WORKER_SKILL } from "./skills-guard";

test("the worker skill says a released pod is not a stop, and never that a role stays live", () => {
  expect(retiredWorkerLifecycleClaims(readFileSync(WORKER_SKILL, "utf8"))).toEqual([]);
});
