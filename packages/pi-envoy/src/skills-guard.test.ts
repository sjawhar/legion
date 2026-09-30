import { expect, test } from "bun:test";
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import * as path from "node:path";

// A phase worker loads `skill://legion-worker` before it acts, and Oh My Pi decides how much of it
// reaches the model: a tool result over 51,200 bytes (`tools.artifactSpillThreshold`, 50 KiB)
// spills to an artifact and arrives with its middle cut out. A `skill://` read is otherwise whole,
// so a file's byte count is the measure. Reads by filesystem path stop at 300 lines, which is why
// the skill links every reference as `skill://legion-worker/<path>`, and why each such link must
// name a file that exists.
const repoRoot = path.resolve(import.meta.dir, "../../..");
const skillsRoot = path.join(repoRoot, "skills");
const rolesRoot = path.join(repoRoot, "packages/pi-envoy/roles");
const workerRoot = path.join(skillsRoot, "legion-worker");
const SPILL_THRESHOLD_BYTES = 50 * 1024;

function files(directory: string): string[] {
  return readdirSync(directory, { recursive: true, encoding: "utf8" })
    .map((entry) => path.join(directory, entry))
    .filter((file) => statSync(file).isFile());
}

test("every legion-worker file is under Oh My Pi's spill threshold, so a skill:// read arrives whole", () => {
  const oversized = files(workerRoot)
    .map((file) => ({ file: path.relative(repoRoot, file), bytes: statSync(file).size }))
    .filter(({ bytes }) => bytes >= SPILL_THRESHOLD_BYTES);
  expect(oversized).toEqual([]);
});

test("the legion-worker body stays under 500 lines, its detail in references", () => {
  const body = readFileSync(path.join(workerRoot, "SKILL.md"), "utf8");
  expect(body.split("\n").length).toBeLessThan(500);
});

test("every skill://legion-worker/<path> link in a skill or a role prompt names a file that exists", () => {
  const link = /skill:\/\/legion-worker\/([^\s)`'"\]>]+)/g;
  const missing: string[] = [];
  for (const file of [...files(skillsRoot), ...files(rolesRoot)]) {
    if (!file.endsWith(".md")) continue;
    for (const [whole, target] of readFileSync(file, "utf8").matchAll(link)) {
      // A link that ends a sentence carries its full stop; an anchor names a heading, not a file.
      const relative = (target ?? "").replace(/[.,;:]+$/, "").split("#")[0] ?? "";
      if (!existsSync(path.join(workerRoot, relative))) {
        missing.push(`${path.relative(repoRoot, file)}: ${whole}`);
      }
    }
  }
  expect(missing).toEqual([]);
});
