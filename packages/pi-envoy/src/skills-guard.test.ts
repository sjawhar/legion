import { afterAll, beforeAll, expect, test } from "bun:test";
import { readFileSync, rmSync } from "node:fs";
import * as path from "node:path";
import { dispatchToolSpecs } from "@legion/contracts";
import {
  dispatchFirstSkillFile,
  readDispatchFirstContext,
} from "@legion/envoy-client/dispatch-first";
import {
  anchors,
  brokenRelativeLinks,
  brokenSkillLinks,
  bundledAgents,
  files,
  longSkillBodies,
  misnamedSkills,
  oversizedFiles,
  REPO_ROOT,
  shippedAgents,
  skillLinks,
  stageSkills,
  unresolvedAgentDispatches,
} from "@legion/pi-shared/test/skills-guard";

// The rules (`@legion/pi-shared/test/skills-guard`) over the skills this package stages into
// dist/skills: the partition scripts/pi-plugin-prepack.sh gives @sjawhar/pi-envoy, staged here as
// its prepack stages it. A `skill://` link resolves by name through Oh My Pi's discovery, so it is
// held to the repository's skills/, where both plugins' skills live.
const repoSkillsRoot = path.join(REPO_ROOT, "skills");
const skillAndPromptRoots = [
  repoSkillsRoot,
  path.join(REPO_ROOT, "packages/daemon/internal/prompts"),
];
const packageRoot = path.resolve(import.meta.dir, "..");
// LEGION_TEST_OMP names the pinned Oh My Pi binary, as in extensions/dispatch-first-omp.test.ts: the
// fork pin CI's pi-envoy job installs. A run without one skips the rule that reads it, except on
// GitHub Actions.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";
let staged: string;

beforeAll(() => {
  staged = stageSkills("@sjawhar/pi-envoy");
});

afterAll(() => {
  rmSync(path.dirname(staged), { recursive: true, force: true });
});

test("every staged file is under Oh My Pi's spill threshold, so a skill:// read arrives whole", () => {
  expect(oversizedFiles(staged)).toEqual([]);
});

test("every skill body stays under 500 lines, its detail in references", () => {
  expect(longSkillBodies(staged)).toEqual([]);
});

test("every skill's frontmatter name is its directory's name", () => {
  expect(misnamedSkills(staged)).toEqual([]);
});

test("the injected dispatch-first skill fits its budget on every host", () => {
  const file = dispatchFirstSkillFile(staged);
  const skill = readFileSync(file, "utf8");
  expect(skill.split("\n").length).toBeLessThan(60);
  // Oh My Pi and Claude Code inject the marker-wrapped body; OpenCode adds the whole file under an
  // `Instructions from: <path>` line. Claude Code keeps a hook's context whole up to 10,000
  // characters; 6,000 leaves room for a longer install path and a later edit.
  expect(readDispatchFirstContext(file).length).toBeLessThan(6_000);
  expect(`Instructions from: ${file}\n${skill}`.length).toBeLessThan(6_000);
});

// Agents reach Dispatch through the `dispatch` command, so no skill or daemon prompt may tell one
// to call a native tool it no longer has: each names the command (`dispatch ask`) instead.
test("no skill or daemon prompt names a native Dispatch tool", () => {
  const toolName = new RegExp(`\\b(${dispatchToolSpecs.map((spec) => spec.name).join("|")})\\b`);
  const named = skillAndPromptRoots
    .flatMap(files)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) =>
      readFileSync(file, "utf8")
        .split("\n")
        .flatMap((line, index) => {
          const match = toolName.exec(line);
          return match === null
            ? []
            : [`${path.relative(REPO_ROOT, file)}:${index + 1}: ${match[0]}`];
        })
    );
  expect(named).toEqual([]);
});

test("every skill://<name>/<path> link names a file that exists, and its #anchor a heading in it", () => {
  expect(brokenSkillLinks(skillLinks([staged]), repoSkillsRoot)).toEqual([]);
});

test("every relative link resolves to a file inside the staged partition", () => {
  expect(brokenRelativeLinks(staged)).toEqual([]);
});

// This plugin's skills reach every session with Dispatch, including one with no other plugin
// installed (the Envoy entry injects dispatch-first into each request), so a task agent they
// dispatch must come with this plugin (`agents/`, which ships none today) or with Oh My Pi itself
// (what `omp agents unpack` writes, read off the pinned binary rather than listed here). A
// `skill://legion-*` mention in these skills is held to no such partition, on purpose: each is a
// sentence conditioned on a Legion role (skills/dispatch/SKILL.md:218 and :385,
// skills/dispatch-brainstorming/SKILL.md:3, :9 and :32, skills/dispatch-first/SKILL.md:53), inert
// for a person and right for a pane with both plugins, and `brokenSkillLinks` above resolves a
// `skill://<name>/<path>` link against the repository's skills/ for the same reason.
test.skipIf(omp === undefined && !onActions)(
  "every task agent the staged skills dispatch is one this plugin ships or Oh My Pi bundles",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const available = new Set([...shippedAgents(packageRoot), ...(await bundledAgents(omp))]);
    expect(unresolvedAgentDispatches([staged], available)).toEqual([]);
  },
  60_000
);

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
