import { expect, test } from "bun:test";
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import * as path from "node:path";
import {
  dispatchFirstSkillFile,
  readDispatchFirstContext,
} from "@legion/envoy-client/dispatch-first";

// The skills this package stages into dist/skills are read by agents, and how much of each reaches
// the model is decided by Oh My Pi: a tool result over 51,200 bytes (`tools.artifactSpillThreshold`,
// 50 KiB) spills to an artifact that keeps only 20 KB at each end, so a longer skill arrives with
// its middle cut out. A `skill://` read is otherwise whole (no line limit, no line-length cap), so
// the file's own byte count is the measure. Reads by filesystem path stop at 300 lines, which is
// why every reference is linked as `skill://<name>/<path>`, and why each such link must resolve:
// the Go daemon's boot gate checks only the name before the first `/`.
const repoRoot = path.resolve(import.meta.dir, "../../..");
const skillsRoot = path.join(repoRoot, "skills");
const rolesRoot = path.join(repoRoot, "packages/pi-envoy/roles");
const SPILL_THRESHOLD_BYTES = 50 * 1024;

function markdownFiles(directory: string): string[] {
  return readdirSync(directory, { recursive: true, encoding: "utf8" })
    .map((entry) => path.join(directory, entry))
    .filter((file) => statSync(file).isFile());
}

test("every skill file is under Oh My Pi's spill threshold, so a skill:// read arrives whole", () => {
  const oversized = markdownFiles(skillsRoot)
    .map((file) => ({ file: path.relative(repoRoot, file), bytes: statSync(file).size }))
    .filter(({ bytes }) => bytes >= SPILL_THRESHOLD_BYTES);
  expect(oversized).toEqual([]);
});

test("the dispatch skill's body stays under 500 lines, its detail in references", () => {
  const body = readFileSync(path.join(skillsRoot, "dispatch/SKILL.md"), "utf8");
  expect(body.split("\n").length).toBeLessThan(500);
});

test("the injected dispatch-first skill fits its budget on every host", () => {
  const file = dispatchFirstSkillFile(skillsRoot);
  const skill = readFileSync(file, "utf8");
  expect(skill.split("\n").length).toBeLessThan(60);
  // Oh My Pi and Claude Code inject the marker-wrapped body; OpenCode adds the whole file under an
  // `Instructions from: <path>` line. Claude Code keeps a hook's context whole up to 10,000
  // characters; 6,000 leaves room for a longer install path and a later edit.
  expect(readDispatchFirstContext(file).length).toBeLessThan(6_000);
  expect(`Instructions from: ${file}\n${skill}`.length).toBeLessThan(6_000);
});

test("every skill://<name>/<path> link in a skill or a role prompt names a file that exists", () => {
  const link = /skill:\/\/([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)\/([^\s)`'"\]>]+)/g;
  const missing: string[] = [];
  for (const file of [...markdownFiles(skillsRoot), ...markdownFiles(rolesRoot)]) {
    if (!file.endsWith(".md")) continue;
    for (const match of readFileSync(file, "utf8").matchAll(link)) {
      const [whole, name, target] = match;
      // A link that ends a sentence carries its full stop; an anchor names a heading, not a file.
      const relative = (target ?? "").replace(/[.,;:]+$/, "").split("#")[0] ?? "";
      if (!existsSync(path.join(skillsRoot, name ?? "", relative))) {
        missing.push(`${path.relative(repoRoot, file)}: ${whole}`);
      }
    }
  }
  expect(missing).toEqual([]);
});
