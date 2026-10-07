import { afterAll, beforeAll, expect, test } from "bun:test";
import { readFileSync, rmSync } from "node:fs";
import * as path from "node:path";
import {
  dispatchFirstSkillFile,
  readDispatchFirstContext,
} from "@legion/envoy-client/dispatch-first";
import {
  anchors,
  brokenRelativeLinks,
  brokenSkillLinks,
  longSkillBodies,
  misnamedSkills,
  oversizedFiles,
  REPO_ROOT,
  skillLinks,
  stageSkills,
} from "@legion/pi-shared/test/skills-guard";

// The rules (`@legion/pi-shared/test/skills-guard`) over the skills this package stages into
// dist/skills: the partition scripts/pi-plugin-prepack.sh gives @sjawhar/pi-envoy, staged here as
// its prepack stages it. A `skill://` link resolves by name through Oh My Pi's discovery, so it is
// held to the repository's skills/, where both plugins' skills live.
const repoSkillsRoot = path.join(REPO_ROOT, "skills");
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

test("every skill://<name>/<path> link names a file that exists, and its #anchor a heading in it", () => {
  expect(brokenSkillLinks(skillLinks([staged]), repoSkillsRoot)).toEqual([]);
});

test("every relative link resolves to a file inside the staged partition", () => {
  expect(brokenRelativeLinks(staged)).toEqual([]);
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
