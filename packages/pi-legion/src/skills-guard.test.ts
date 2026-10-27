import { afterAll, beforeAll, expect, test } from "bun:test";
import { readFileSync, rmSync, statSync } from "node:fs";
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

// The daemon submits the merge itself once it accepts READY, and Legion enforces no GitHub
// restriction the repository itself does not: no skill a Legion pane loads, and no daemon prompt,
// says Legion never merges.
test("no skill or prompt says Legion never merges", () => {
  const saying = [staged, promptsRoot]
    .flatMap(files)
    .filter(
      (file) =>
        file.endsWith(".md") && /never merges|merges? nothing/i.test(readFileSync(file, "utf8"))
    )
    .map((file) => path.relative(REPO_ROOT, file));
  expect(saying).toEqual([]);
});

// An issue's disk dies with its pod (INFRA-80), so nothing writes the recovery marker
// `.legion/<issue>/workspace-recovered.json` any more: `workspace-init` lost it with the lost-disk
// verdict. The `Your workspace was recreated…` notice now reaches every resume in a new pod, so a
// skill or prompt that still sends an agent to the marker sends every resumed agent after a file
// that never exists.
test("no skill or prompt sends an agent to the workspace recovery marker, which nothing writes", () => {
  const sending = [staged, promptsRoot]
    .flatMap(files)
    .filter(
      (file) =>
        file.endsWith(".md") && readFileSync(file, "utf8").includes("workspace-recovered.json")
    )
    .map((file) =>
      file.startsWith(staged)
        ? path.join("skills", path.relative(staged, file))
        : path.relative(REPO_ROOT, file)
    );
  expect(sending).toEqual([]);
});

// The controller's `gh` acts as the review App (LEGION-668): the skill must say how it checks a
// pull request's state, and nothing anywhere in the sweep's roots may still claim the controller
// (or any role) cannot read GitHub. The commands live in the two references SKILL.md sends the
// controller to (its session and its triage), so SKILL.md must link both, and the strings are
// read from those three files only.
test("the controller skill reads pull requests as the review App, and nothing says the controller holds no GitHub credential", () => {
  const controllerDir = path.join(staged, "legion-controller");
  const entry = readFileSync(path.join(controllerDir, "SKILL.md"), "utf8");
  for (const reference of ["session.md", "triage.md"]) {
    expect(entry).toContain(`skill://legion-controller/references/${reference}`);
  }
  const controllerSkill = ["SKILL.md", "references/session.md", "references/triage.md"]
    .map((file) => readFileSync(path.join(controllerDir, file), "utf8"))
    .join("\n");
  expect(controllerSkill).toContain(
    "gh pr view 'https://github.com/<owner>/<repo>/pull/<n>' --json state -q .state"
  );
  expect(controllerSkill).not.toContain("gh pr view <url>");
  expect(controllerSkill).toContain("not a pull-request URL");
  expect(controllerSkill).toContain("--label '<each current label>'");
  expect(controllerSkill).toContain("cannot be quoted");
  for (const token of ["OPEN", "MERGED", "CLOSED", "unreadable", '"phase":"controller"']) {
    expect(controllerSkill).toContain(token);
  }

  const forbidden =
    /no GitHub credential|never reads GitHub|has no App|cannot read GitHub|holds no repository credential/;
  const extensions = new Set([".md", ".go", ".ts", ".yaml", ".yml", ".example", ".sh"]);
  const roots = [
    path.join(REPO_ROOT, "skills/legion-controller"),
    path.join(REPO_ROOT, "docs/kubernetes.md"),
    path.join(REPO_ROOT, "packages/daemon/internal"),
    path.join(REPO_ROOT, "docs/site/src/content/docs"),
    path.join(REPO_ROOT, "deploy/kubernetes/daemon/controller.yaml.example"),
    path.join(REPO_ROOT, "AGENTS.md"),
    path.join(REPO_ROOT, "README.md"),
  ];
  const skipDirs = /(^|\/)(node_modules|dist|testdata)(\/|$)/;
  const candidates = roots.flatMap((root) => (statSync(root).isDirectory() ? files(root) : [root]));
  const offending = candidates
    .filter((file) => extensions.has(path.extname(file)) && !skipDirs.test(file))
    .flatMap((file) => {
      const text = readFileSync(file, "utf8");
      if (!forbidden.test(text)) return [];
      return text
        .split("\n")
        .map((line, index) => ({ line, number: index + 1 }))
        .filter(({ line }) => forbidden.test(line))
        .map(({ number }) => `${path.relative(REPO_ROOT, file)}:${number}`);
    });
  expect(offending).toEqual([]);
});

// INFRA-83 took the `CI:` line out of the pull request body, and the forward merge of main put the
// paragraph that defined it back into skills/legion-worker/references/pr-body.md with no conflict
// for anyone to read: a merge keeps both sides by design, so a deletion is undone in silence. The
// body carries no such line, and that reference is the file every worker reads before it touches a
// body, so no staged skill and no daemon prompt may say what the line holds or tell anyone to
// write one.
//
// What it pins is three shapes the regression took, not a class. First, the reference (`CI` line,
// `CI:` line, `**CI**` line, `"CI"` line or bare CI line) as the subject of one of seven verbs
// that say what it contains: `names`, `holds`, `carries`, `lists`, `records`, `shows`, `gives`.
// Second, the reference as the object of one of ten spellings of seven verbs that say to write it
// — `writes`/`written`, `fills`/`filled`, `updates`, `cites`/`cited`, `defines`, `quotes`,
// `records`, each optionally `re-` prefixed — behind a definite determiner (`the`, `a`, `its`,
// `this`, `each`) up to two words after the verb, with a lookbehind that drops `never`, `no
// longer` and `not`. Third, the template's own `**CI:**` field line. The determiner and the
// lookbehind are what let a sentence naming the line while saying it is gone pass: "has no `CI:`
// line", "no `CI:` line to fill", "never writes the `CI` line".
//
// Four shapes it does not catch. The copular: "The body's CI line is the runs GitHub started for
// the code head" uses no verb from either list. The nominalisation: "that reference is the sole
// definition of the CI line", which is the sentence this branch deletes from
// packages/daemon/internal/prompts/roles/implementer.md. A writing verb outside the list: "must
// populate the CI line from one `gh run list`", "Keep the body's CI line current after every
// push". And the anaphora: a sentence saying only "the implementer writes it" passes, which is
// why the anchor is the defining sentence such a paragraph opens with, not its later pronouns.
//
// So this test pins those three shapes and closes no class: a reader who needs a new shape caught
// adds it here rather than assuming it is covered.
test("no staged skill or daemon prompt says what a pull request body `CI` line holds, or tells anyone to write one", () => {
  // `CI` line, `CI:` line, **CI** line, "CI" line, or bare CI line, wrapped across lines or not.
  const reference = String.raw`(?:\*\*|\`|")?CI:?(?:\*\*|\`|")?\s+line`;
  const instructions = [
    // The line as the subject of a verb that says what it contains.
    new RegExp(
      String.raw`${reference}\s+(?:names|holds|carries|lists|records|shows|gives)\b`,
      "gi"
    ),
    // The line as the object of a verb that says to write one, behind a definite determiner so
    // that `no …` and `any …` cannot fill that slot, and not itself negated.
    new RegExp(
      String.raw`(?<!\b(?:never|no longer|not)\s)\b(?:re-)?(?:writes?|written|fills?|filled|updates?|cites?|cited|defines?|quotes?|records?)\s+(?:\w+\s+){0,2}(?:the|a|its|this|each)\s+${reference}`,
      "gi"
    ),
    // The template's own field line, which is an instruction to fill it in.
    /^\*\*CI:?\*\*/gm,
  ];
  const instructing = [staged, promptsRoot]
    .flatMap(files)
    .filter((file) => file.endsWith(".md"))
    .flatMap((file) => {
      const text = readFileSync(file, "utf8");
      // A hit in the staged partition reads as its own layout has it
      // (`legion-worker/references/pr-body.md`); a hit in the prompts, which live in the
      // repository, reads from the repository root.
      const name = file.startsWith(staged)
        ? path.relative(staged, file)
        : path.relative(REPO_ROOT, file);
      return instructions
        .flatMap((instruction) => [...text.matchAll(instruction)])
        .map(({ index }) => `${name}:${text.slice(0, index).split("\n").length}`);
    });
  expect(instructing).toEqual([]);
});
