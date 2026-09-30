import { expect, test } from "bun:test";
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import * as path from "node:path";

// A phase worker loads `skill://legion-worker` before it acts, and Oh My Pi decides how much of it
// reaches the model: a tool result over 51,200 bytes (`tools.artifactSpillThreshold`, 50 KiB)
// spills to an artifact and arrives with its middle cut out. A `skill://` read is otherwise whole,
// so a file's byte count is the measure. Reads by filesystem path stop at 300 lines, which is why
// the skill links every reference as `skill://legion-worker/<path>`, and why each such link must
// name a file that exists, and each reference must be linked from somewhere a worker reads.
const repoRoot = path.resolve(import.meta.dir, "../../..");
const skillsRoot = path.join(repoRoot, "skills");
const workerRoot = path.join(skillsRoot, "legion-worker");
// Everything a worker reads that can link into the skill: the skills themselves, the TypeScript
// daemon's role prompts, and the Go daemon's prompt overlays.
const linkingRoots = [
  skillsRoot,
  path.join(repoRoot, "packages/pi-envoy/roles"),
  path.join(repoRoot, "packages/daemon-go/internal/prompts"),
];
const SPILL_THRESHOLD_BYTES = 50 * 1024;
// A link's path stops at whitespace, a closing bracket or quote, a code span, or Markdown emphasis
// (`**skill://…/pr-body.md**`); `#anchor` is kept so it can be checked against the target's headings.
const LINK = /skill:\/\/legion-worker\/([^\s)`'"\]>*]+)/g;

function files(directory: string): string[] {
  return readdirSync(directory, { recursive: true, encoding: "utf8" })
    .map((entry) => path.join(directory, entry))
    .filter((file) => statSync(file).isFile());
}

/** GitHub's heading anchor: lower-cased, punctuation dropped, each space a hyphen. */
function anchors(markdown: string): Set<string> {
  return new Set(
    [...markdown.matchAll(/^#{1,6}\s+(.+?)\s*#*$/gm)].map(([, heading]) =>
      (heading ?? "")
        .toLowerCase()
        .replace(/[^\p{L}\p{N}\s_-]/gu, "")
        .replace(/\s/g, "-")
    )
  );
}

/** Every `skill://legion-worker/<path>[#anchor]` link, as `{source, target, anchor}`. */
function links(): { source: string; target: string; anchor: string | undefined }[] {
  return linkingRoots
    .flatMap(files)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) =>
      [...readFileSync(file, "utf8").matchAll(LINK)].map(([, raw]) => {
        // A link that ends a sentence carries its full stop.
        const [target = "", anchor] = (raw ?? "").replace(/[.,;:]+$/, "").split("#");
        return { source: path.relative(repoRoot, file), target, anchor };
      })
    );
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

test("every skill://legion-worker/<path> link names a file that exists, and its #anchor a heading in it", () => {
  const broken = links().flatMap(({ source, target, anchor }) => {
    const file = path.join(workerRoot, target);
    if (!existsSync(file)) return [`${source}: skill://legion-worker/${target} names no file`];
    if (anchor !== undefined && !anchors(readFileSync(file, "utf8")).has(anchor)) {
      return [`${source}: skill://legion-worker/${target}#${anchor} names no heading`];
    }
    return [];
  });
  expect(broken).toEqual([]);
});

test("every legion-worker reference is linked from a skill or a prompt", () => {
  const linked = new Set(links().map(({ target }) => target));
  const unlinked = files(path.join(workerRoot, "references"))
    .map((file) => path.relative(workerRoot, file))
    .filter((reference) => !linked.has(reference));
  expect(unlinked).toEqual([]);
});
