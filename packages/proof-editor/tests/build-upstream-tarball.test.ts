/**
 * `packUpstream`, the half of `scripts/build-upstream-tarball.ts` that writes the committed
 * tarball from a source tree (the other half fetches that tree, and a test of it would fetch
 * 31 MB from api.github.com on every run — the network dependency the tarball exists to remove).
 *
 * The bytes matter rather than the contents: `bun.lock` records the tarball's `sha512`, so a
 * rebuild of a pin already committed must produce the file already committed, or a lockfile check
 * fails for a change nobody made. Each case below pins one of the reasons the bytes are a
 * function of the source tree alone: the member list, the order, the mtimes, the ownership and
 * the gzip header's timestamp.
 */

import { afterAll, beforeAll, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { packUpstream } from "../scripts/build-upstream-tarball";

const MTIME = "2026-10-17T22:41:59Z";
const MEMBERS = [
  "package/LICENSE",
  "package/package.json",
  "package/src/editor/plugins/marks.ts",
  "package/src/lib.ts",
];

let workDir: string;
let sourceDir: string;
let first: string;
let second: string;

beforeAll(() => {
  workDir = mkdtempSync(join(tmpdir(), "proof-sdk-pack-test-"));
  sourceDir = join(workDir, "source");
  mkdirSync(join(sourceDir, "src", "editor", "plugins"), { recursive: true });
  writeFileSync(join(sourceDir, "src", "lib.ts"), "export const lib = 1;\n");
  writeFileSync(
    join(sourceDir, "src", "editor", "plugins", "marks.ts"),
    "export const marks = 2;\n"
  );
  writeFileSync(join(sourceDir, "package.json"), '{ "name": "proof-sdk" }\n');
  writeFileSync(join(sourceDir, "LICENSE"), "MIT\n");
  // Neither under src/ nor one of the two named files, so the archive must not hold it.
  writeFileSync(join(sourceDir, "tsconfig.json"), '{ "compilerOptions": {} }\n');
  first = join(workDir, "first.tgz");
  second = join(workDir, "second.tgz");
  packUpstream({ sourceDir, mtime: MTIME, outputPath: first });
  packUpstream({ sourceDir, mtime: MTIME, outputPath: second });
});

afterAll(() => {
  rmSync(workDir, { recursive: true, force: true });
});

/** `tar --list --verbose`'s lines for an archive, one per member, with dates in UTC. */
function listing(archive: string): string[] {
  const result = spawnSync(
    "tar",
    ["--list", "--verbose", "--utc", "--numeric-owner", "--file", archive],
    { encoding: "utf8" }
  );
  expect(result.status).toBe(0);
  return result.stdout.trimEnd().split("\n");
}

test("two runs over one source tree write the same bytes", () => {
  expect(readFileSync(second)).toEqual(readFileSync(first));
});

test("the archive holds the three sources under package/, in sorted order", () => {
  expect(listing(first).map((line) => line.split(" ").pop())).toEqual(MEMBERS);
});

test("a file beside src/, package.json and LICENSE stays out of the archive", () => {
  expect(listing(first).join("\n")).not.toContain("tsconfig.json");
});

test("every member carries the given mtime and is owned by 0:0", () => {
  for (const line of listing(first)) {
    const [, owner, , date, time] = line.split(/\s+/);
    expect(owner).toBe("0/0");
    expect(`${date}T${time}`).toBe("2026-10-17T22:41");
  }
});

test("the gzip header carries no timestamp", () => {
  expect(Array.from(readFileSync(first).subarray(4, 8))).toEqual([0, 0, 0, 0]);
});
