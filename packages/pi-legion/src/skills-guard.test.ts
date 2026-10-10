import { afterAll, beforeAll, expect, test } from "bun:test";
import { readFileSync, rmSync } from "node:fs";
import * as path from "node:path";
import {
  brokenRelativeLinks,
  brokenSkillLinks,
  files,
  longSkillBodies,
  misnamedSkills,
  oversizedFiles,
  REPO_ROOT,
  skillLinks,
  stageSkills,
} from "@legion/pi-shared/test/skills-guard";

// The rules (`@legion/pi-shared/test/skills-guard`) over the skills this package stages into
// dist/skills: the partition scripts/pi-plugin-prepack.sh gives @sjawhar/pi-legion, staged here as
// its prepack stages it. Everything a Legion agent reads that can link into a skill is a linking
// root: the staged skills themselves and the daemon's prompts (its role prompts in roles/ and its
// overlays in go/). A `skill://` link resolves by name through Oh My Pi's discovery, so it is held to
// the repository's skills/, where both plugins' skills live: `skill://dispatch/…` from this
// partition is valid because the Envoy plugin, installed beside this one, ships it.
const repoSkillsRoot = path.join(REPO_ROOT, "skills");
const promptsRoot = path.join(REPO_ROOT, "packages/daemon/internal/prompts");
let staged: string;

beforeAll(() => {
  staged = stageSkills("@sjawhar/pi-legion");
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

test("every skill://<name>/<path> link names a file that exists, and its #anchor a heading in it", () => {
  expect(brokenSkillLinks(skillLinks([staged, promptsRoot]), repoSkillsRoot)).toEqual([]);
});

test("every relative link resolves to a file inside the staged partition", () => {
  expect(brokenRelativeLinks(staged)).toEqual([]);
});

// Reads by filesystem path stop at 300 lines, so a worker reaches a reference only through a
// `skill://legion-worker/<path>` link from somewhere it reads.
test("every legion-worker reference is linked from a skill or a prompt", () => {
  const workerRoot = path.join(staged, "legion-worker");
  const linked = new Set(
    skillLinks([staged, promptsRoot])
      .filter(({ name }) => name === "legion-worker")
      .map(({ target }) => target)
  );
  const unlinked = files(path.join(workerRoot, "references"))
    .map((file) => path.relative(workerRoot, file))
    .filter((reference) => !linked.has(reference));
  expect(unlinked).toEqual([]);
});

// The merger submits the merge itself once READY is accepted (LEGION-631), and Legion enforces no
// GitHub restriction the repository itself does not: the worker skill names the command, and no
// skill a Legion pane loads, and no daemon prompt, says Legion never merges.
test("the worker skill names the merger's merge command, and no skill or prompt says Legion never merges", () => {
  const command = "`gh pr merge <n> -R <owner>/<repo> --auto --squash --match-head-commit <head>`";
  for (const file of ["legion-worker/references/merge-gate.md", "legion-worker/SKILL.md"]) {
    expect(readFileSync(path.join(staged, file), "utf8")).toContain(command);
  }
  const saying = [staged, promptsRoot]
    .flatMap(files)
    .filter(
      (file) =>
        file.endsWith(".md") && /never merges|merges? nothing/i.test(readFileSync(file, "utf8"))
    )
    .map((file) => path.relative(REPO_ROOT, file));
  expect(saying).toEqual([]);
});

// GitHub honours the `skip-checks: true` trailer only as the last line after two empty lines; the
// recipe's first live run wrote one and GitHub started every workflow (LEGION-631). The worker skill
// and the daemon's worker part carry the recipe with two, and no skill or prompt carries one with
// fewer: a `$'\n…skip-checks: true'` form is the recipe, so it ends with exactly three `\n`.
test("the skip-checks recipe writes two empty lines before the trailer, everywhere it is written", () => {
  const recipe = "$'\\n\\n\\nskip-checks: true'";
  expect(readFileSync(path.join(staged, "legion-worker/SKILL.md"), "utf8")).toContain(recipe);
  expect(readFileSync(path.join(promptsRoot, "go/worker-common.md"), "utf8")).toContain(recipe);
  const short = [staged, promptsRoot]
    .flatMap(files)
    .filter((file) => {
      if (!file.endsWith(".md")) return false;
      const forms = readFileSync(file, "utf8").match(/\$'(\\n)*skip-checks: true'/g) ?? [];
      return forms.some((form) => form !== recipe);
    })
    .map((file) => path.relative(REPO_ROOT, file));
  expect(short).toEqual([]);
});
