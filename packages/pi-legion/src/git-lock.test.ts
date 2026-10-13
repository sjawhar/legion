import { afterEach, expect, test } from "bun:test";
import { mkdtemp, rm, utimes, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { staleGitLockNote } from "./git-lock";

const scratch: string[] = [];
afterEach(async () => {
  for (const directory of scratch.splice(0)) await rm(directory, { recursive: true, force: true });
});

const NOW = Date.parse("2026-10-13T21:53:36Z");

/** A git lock file at `name` in a scratch directory, last written `ageMs` before NOW. */
async function staleLock(name: string, ageMs: number): Promise<string> {
  const dir = await mkdtemp(path.join(tmpdir(), "legion-git-lock-"));
  scratch.push(dir);
  const lock = path.join(dir, name);
  await writeFile(lock, "");
  const written = new Date(NOW - ageMs);
  await utimes(lock, written, written);
  return lock;
}

const note = (lock: string, age: string) =>
  `git lock ${lock} was last written ${age} ago and blocks this command. If no git or jj process is still running, an interrupted one left it. Legion never removes a lock under a running pod; on Kubernetes the issue pod's next start removes it before any role runs.`;

test("names each git lock the output reports that is still there, with how long ago it was written", async () => {
  const index = await staleLock("index.lock", (31 * 60 + 4) * 1000);
  const ref = await staleLock("main.lock", (2 * 3600 + 5) * 1000);
  const output = [
    "Error: Failed to reset Git HEAD state",
    `2: The lock for resource '${index.slice(0, -5)}' could not be obtained after 1.00s after 16 attempt(s). The lockfile at '${index}' might need manual deletion.`,
    `error: cannot lock ref 'refs/heads/main': Unable to create '${ref}': File exists.`,
  ].join("\n");
  expect(await staleGitLockNote(output, NOW)).toBe(
    `${note(index, "31m4s")}\n${note(ref, "2h0m5s")}`
  );
});

test("says nothing of output that names no lock, or names one already gone", async () => {
  const gone = path.join(tmpdir(), "legion-git-lock-never-made", "index.lock");
  expect(await staleGitLockNote('Error: There is no jj repo in "."', NOW)).toBe("");
  expect(await staleGitLockNote(`fatal: Unable to create '${gone}': File exists.`, NOW)).toBe("");
});
