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
// why every reference is linked as `skill://<name>/<path>`, why each such link must resolve (the
// daemon's boot gate checks only the name before the first `/`), and why each legion-worker
// reference must be linked from somewhere a worker reads.
const repoRoot = path.resolve(import.meta.dir, "../../..");
const skillsRoot = path.join(repoRoot, "skills");
const workerRoot = path.join(skillsRoot, "legion-worker");
// Everything an agent reads that can link into a skill: the skills themselves, the role prompts,
// and the daemon's prompt overlays.
const linkingRoots = [
  skillsRoot,
  path.join(repoRoot, "packages/pi-envoy/roles"),
  path.join(repoRoot, "packages/daemon-go/internal/prompts"),
];
const SPILL_THRESHOLD_BYTES = 50 * 1024;
// A link's path stops at whitespace, a closing bracket or quote, a code span, or Markdown emphasis
// (`**skill://…/pr-body.md**`); `#anchor` is kept so it can be checked against the target's headings.
const LINK = /skill:\/\/([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)\/([^\s)`'"\]>*]+)/g;

function files(directory: string): string[] {
  return readdirSync(directory, { recursive: true, encoding: "utf8" })
    .map((entry) => path.join(directory, entry))
    .filter((file) => statSync(file).isFile());
}

/** Every `skills/<name>/SKILL.md`. */
function skillFiles(): string[] {
  return readdirSync(skillsRoot)
    .map((name) => path.join(skillsRoot, name, "SKILL.md"))
    .filter((file) => existsSync(file));
}

/**
 * The lines outside fenced code blocks, where a Markdown reader finds headings: a fence opens on
 * three or more backticks or tildes (up to three spaces in), closes on a line holding only a run of
 * the same character at least as long, and an unclosed fence runs to the end of the file.
 */
function outsideFences(markdown: string): string[] {
  const kept: string[] = [];
  let fence: string | undefined;
  for (const line of markdown.split("\n")) {
    const marker = /^ {0,3}(`{3,}|~{3,})/.exec(line)?.[1];
    if (fence === undefined) {
      if (marker === undefined) kept.push(line);
      else fence = marker;
    } else if (
      marker?.[0] === fence[0] &&
      marker.length >= fence.length &&
      line.trim() === marker
    ) {
      fence = undefined;
    }
  }
  return kept;
}

/** GitHub's heading anchor: lower-cased, punctuation dropped, each space a hyphen. */
function anchors(markdown: string): Set<string> {
  return new Set(
    outsideFences(markdown).flatMap((line) => {
      const heading = /^#{1,6}\s+(.+?)\s*#*$/.exec(line)?.[1];
      if (heading === undefined) return [];
      return [
        heading
          .toLowerCase()
          .replace(/[^\p{L}\p{N}\s_-]/gu, "")
          .replace(/\s/g, "-"),
      ];
    })
  );
}

/** Every `skill://<name>/<path>[#anchor]` link, as `{source, name, target, anchor}`. */
function links(): { source: string; name: string; target: string; anchor: string | undefined }[] {
  return linkingRoots
    .flatMap(files)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) =>
      [...readFileSync(file, "utf8").matchAll(LINK)].map(([, name = "", raw]) => {
        // A link that ends a sentence carries its full stop.
        const [target = "", anchor] = (raw ?? "").replace(/[.,;:]+$/, "").split("#");
        return { source: path.relative(repoRoot, file), name, target, anchor };
      })
    );
}

test("every skill file is under Oh My Pi's spill threshold, so a skill:// read arrives whole", () => {
  const oversized = files(skillsRoot)
    .map((file) => ({ file: path.relative(repoRoot, file), bytes: statSync(file).size }))
    .filter(({ bytes }) => bytes >= SPILL_THRESHOLD_BYTES);
  expect(oversized).toEqual([]);
});

test("every skill body stays under 500 lines, its detail in references", () => {
  const long = skillFiles()
    .map((file) => ({
      file: path.relative(repoRoot, file),
      lines: readFileSync(file, "utf8").split("\n").length,
    }))
    .filter(({ lines }) => lines >= 500);
  expect(long).toEqual([]);
});

// Oh My Pi resolves `skill://<name>` by the frontmatter name, and the link check below resolves it
// by the directory, so the two must agree.
test("every skill's frontmatter name is its directory's name", () => {
  const misnamed = skillFiles()
    .map((file) => {
      const frontmatter = /^---\n([\s\S]*?)\n---\n/.exec(readFileSync(file, "utf8"))?.[1] ?? "";
      return {
        directory: path.basename(path.dirname(file)),
        name: /^name:\s*(.*?)\s*$/m.exec(frontmatter)?.[1],
      };
    })
    .filter(({ directory, name }) => name !== directory);
  expect(misnamed).toEqual([]);
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

test("every skill://<name>/<path> link names a file that exists, and its #anchor a heading in it", () => {
  const broken = links().flatMap(({ source, name, target, anchor }) => {
    const link = `skill://${name}/${target}`;
    const file = path.join(skillsRoot, name, target);
    if (!existsSync(file)) return [`${source}: ${link} names no file`];
    if (anchor !== undefined && !anchors(readFileSync(file, "utf8")).has(anchor)) {
      return [`${source}: ${link}#${anchor} names no heading`];
    }
    return [];
  });
  expect(broken).toEqual([]);
});

test("a heading inside a code fence is no anchor, since a Markdown reader renders none", () => {
  const markdown = [
    "## Outside",
    "```",
    "## In backticks",
    "```",
    "~~~~",
    "## In tildes",
    "```",
    "~~~~",
    "# After",
  ].join("\n");
  expect([...anchors(markdown)]).toEqual(["outside", "after"]);
});

test("every legion-worker reference is linked from a skill or a prompt", () => {
  const linked = new Set(
    links()
      .filter(({ name }) => name === "legion-worker")
      .map(({ target }) => target)
  );
  const unlinked = files(path.join(workerRoot, "references"))
    .map((file) => path.relative(workerRoot, file))
    .filter((reference) => !linked.has(reference));
  expect(unlinked).toEqual([]);
});
