import { afterAll, beforeAll, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, statSync } from "node:fs";
import * as os from "node:os";
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

// The Claude Code plugin is generated from this repository's skills/ and this package's agents/,
// and only main's Rebuild Claude Envoy plugin workflow commits packages/claude-envoy/legion/, so
// a check of what a Claude Code role loads builds a fresh one, as
// packages/claude-envoy/tests/legion-plugin-fresh.test.ts does. The build runs as its own command,
// the way stageSkills runs the prepack script: imported, that package's sources would be
// typechecked under this package's `tsc`, whose `lib` is older than the one they are written
// against.
const PLUGIN_BUILDER = path.join(REPO_ROOT, "packages/claude-envoy/scripts/build.ts");

// The rules (`@legion/pi-shared/test/skills-guard`) over the skills this package stages into
// dist/skills: the partition scripts/pi-plugin-prepack.sh gives @sjawhar/pi-legion, staged here as
// its prepack stages it. Everything a Legion agent reads that can link into a skill is a linking
// root: the staged skills themselves and the daemon's prompts (its role prompts in roles/ and its
// overlays in go/). A `skill://` link resolves by name through Oh My Pi's discovery, so it is held to
// the repository's skills/, where both plugins' skills live: `skill://dispatch/…` from this
// partition is valid because the Envoy plugin, installed beside this one, ships it.
const repoSkillsRoot = path.join(REPO_ROOT, "skills");
const promptsRoot = path.join(REPO_ROOT, "packages/daemon/internal/prompts");
const agentsRoot = path.join(REPO_ROOT, "packages/pi-legion/agents");
let staged: string;

beforeAll(() => {
  staged = stageSkills("@sjawhar/pi-legion");
});

afterAll(() => {
  rmSync(path.dirname(staged), { recursive: true, force: true });
});

// A Legion architect reads leading-a-work-stream. Each edit of a root spec after approval is a new
// version, which closes the design gate, so the record of accepted requests and waits lives in an
// artifact of its own and never in the spec.
test("leading-a-work-stream keeps open items in their own artifact, never the spec", () => {
  const skill = readFileSync(path.join(staged, "leading-a-work-stream", "SKILL.md"), "utf8");
  expect(skill).toContain("a line in that issue's `open-items.md` artifact, never its spec");
  expect(skill).not.toContain("## Open items");
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

// Background work is on for every role: Legion writes no Oh My Pi settings
// overlay, so a long bash call becomes a background job and a `task` subagent runs as one, and a
// role reads a result with the `wait` tool, which a message to its session also returns from. No
// task agent declares `blocking`, and nothing interrupts a turn: a CI red that ends a
// phase starts the implementer at once and tells the role whose phase ended by one message, which
// stops nothing of its. So no skill, no shipped agent and no file of the plugin a Claude Code role
// loads may say that a takeover cancels a turn or tell a role to keep its work in
// the foreground, and each that dispatches a task subagent names the tool that reads its result.
//
// Three of the four trees that carry this text are read here — the repository's skills/, the
// plugin's agents/, and a
// fresh `buildLegionPlugin` output, which is generated from those two and so is the only copy of
// `packages/claude-envoy/legion/` a pull request can check (that tree is committed by main's
// Rebuild Claude Envoy plugin workflow alone). The daemon's prompts are the fourth, read by
// packages/daemon/internal/prompts/roles_test.go, which walks both of its embed trees and so
// reaches go/claude-mechanics.md, a part `Compose` never joins.
//
// Both halves are exact: the retired sentences as literals, and the dispatch as the boot gate's
// own `task(agent="` against `` `wait` `` as a backticked token, since the English word "wait" is
// in many of these files and one of them forbids waiting (packages/daemon/internal/prompts/go/
// implementer.md, "never wait for CI"). The files that dispatch are listed rather than counted, so
// one that stopped dispatching fails as loudly as one that started, and a reader that found
// nothing cannot pass while checking nothing.
test("no skill, agent or plugin file keeps work in the foreground, and each that dispatches a task names `wait`", () => {
  const retired = [
    "interrupts your turn",
    "tool calls are cancelled",
    "interrupted its turn",
    "run none in the background",
    "one foreground command",
    "blocking: true",
  ];
  const out = path.join(mkdtempSync(path.join(os.tmpdir(), "legion-plugin-")), "build");
  try {
    const built = Bun.spawnSync([process.execPath, PLUGIN_BUILDER, "--out", out]);
    if (built.exitCode !== 0) {
      throw new Error(
        `${PLUGIN_BUILDER} --out ${out} exited ${built.exitCode}:\n${built.stderr.toString()}`
      );
    }
    const plugin = path.join(out, "legion");
    const saidAnyway: string[] = [];
    const dispatching: string[] = [];
    const silent: string[] = [];
    for (const file of [repoSkillsRoot, agentsRoot, plugin].flatMap(files)) {
      const name = file.startsWith(plugin)
        ? `the fresh Claude plugin's ${path.relative(plugin, file)}`
        : path.relative(REPO_ROOT, file);
      const text = readFileSync(file, "utf8");
      for (const phrase of retired) {
        if (text.includes(phrase)) saidAnyway.push(`${name}: ${phrase}`);
      }
      if (text.includes('task(agent="')) {
        dispatching.push(name);
        if (!text.includes("`wait`")) silent.push(name);
      }
    }
    expect(saidAnyway).toEqual([]);
    expect(silent).toEqual([]);
    expect(dispatching.sort()).toEqual([
      "skills/AGENTS.md",
      "skills/ce-simplify-code/SKILL.md",
      "skills/dispatch-writing-specs/SKILL.md",
      "skills/legion-architect/references/decomposition.md",
      "skills/legion-architect/references/design-gate.md",
      "skills/legion-oracle/SKILL.md",
      "skills/legion-retro/SKILL.md",
      "skills/legion-worker/references/merge-gate.md",
      "the fresh Claude plugin's skills/ce-simplify-code/SKILL.md",
      "the fresh Claude plugin's skills/legion-architect/references/decomposition.md",
      "the fresh Claude plugin's skills/legion-architect/references/design-gate.md",
      "the fresh Claude plugin's skills/legion-oracle/SKILL.md",
      "the fresh Claude plugin's skills/legion-retro/SKILL.md",
      "the fresh Claude plugin's skills/legion-worker/references/merge-gate.md",
    ]);
  } finally {
    rmSync(path.dirname(out), { recursive: true, force: true });
  }
}, 60_000);

// dispatch://INFRA-332's eight guidance rules, each pinned where it was written. A pin fails when
// a later edit deletes the rule's key sentence; it does not show that an agent follows the rule,
// which the issue's session runs measure. Whitespace is collapsed before the match, as
// shipped-agents.test.ts collapses it, so a pin survives a rewrap of the file it reads.
// Read once per file: a dozen pins read the same four references.
const collapsedText = new Map<string, string>();
function collapsed(file: string): string {
  const seen = collapsedText.get(file);
  if (seen !== undefined) return seen;
  const text = readFileSync(file, "utf8").replace(/\s+/g, " ");
  collapsedText.set(file, text);
  return text;
}

// A sentence copied into several files is counted once per file, never over a composed prompt,
// where a sentence in both roles/<role>.md and go/worker-common.md would appear twice. A file
// that lost the sentence and a file that gained a second copy both fail, and the files are listed
// rather than counted, so one that stopped carrying it fails as loudly as one that started.
// A `skills/…` path is read from the staged partition, as every other skill pin here is, so what
// is counted is the copy the plugin ships.
function countedOncePerFile(rule: string, relativePaths: string[]) {
  const counts = relativePaths.map((relative) => ({
    file: relative,
    found:
      collapsed(
        relative.startsWith("skills/")
          ? path.join(staged, relative.slice("skills/".length))
          : path.join(REPO_ROOT, relative)
      ).split(rule).length - 1,
  }));
  expect(counts).toEqual(relativePaths.map((file) => ({ file, found: 1 })));
}

// R1: the simplify pass covers every file of the diff, docs-only diffs included, and a defect it
// sees is reported rather than dropped as out of scope (dispatch://INFRA-198).
test("the simplify pass reads every file of its scope and reports the defects it sees", () => {
  const simplify = collapsed(path.join(staged, "ce-simplify-code", "SKILL.md"));
  expect(simplify).toContain(
    "Every file of the scope is in the pass: documentation, workflow and configuration files as much as code, and a scope that holds no code at all still runs."
  );
  expect(simplify).toContain("A defect you see is reported, never dropped.");
  expect(simplify).not.toContain("For mixed scopes, retain only the code.");
  const prBody = collapsed(path.join(staged, "legion-worker", "references", "pr-body.md"));
  expect(prBody).toContain("whatever the diff touches, a docs-only diff included.");
  expect(prBody).toContain(
    "The pass covers every file of the diff, workflow YAML and documentation with the code, and a defect it reports is fixed outside the pass with the proof run again, never dropped as out of scope; the `Thermo` line's `defects reported` half names how many and the commit that fixed them"
  );
  // The Thermo line is written on every diff now, and carries what the pass reported; only its
  // pair half is omitted on a docs-only one, which leaves the reviewer's pair where it was.
  expect(prBody).toContain("defects reported: <none | <n>, fixed in <sha>>");
  expect(prBody).toContain(
    "(on a docs-only PR the simplify half and the defects are recorded as on any other diff, and the pair half reads `thermonuclear pair: not run (docs-only)`)"
  );
  expect(prBody).not.toContain("a docs-only diff gets none");
  const mergeGate = collapsed(path.join(staged, "legion-worker", "references", "merge-gate.md"));
  expect(mergeGate).toContain(
    "On a docs-only PR the `Thermo` line still records the simplify pass and the defects it reported, and its pair half reads `thermonuclear pair: not run (docs-only)`."
  );
  expect(mergeGate).not.toContain("Skip the `Thermo` line entirely on a docs-only PR.");
});

// R2: a check that cannot fail is not a proof, and a documented procedure counts only when it ran
// (dispatch://INFRA-142, dispatch://INFRA-198).
test("a check that cannot fail is not a proof, and a documented procedure counts only when it ran", () => {
  const prBody = collapsed(path.join(staged, "legion-worker", "references", "pr-body.md"));
  expect(prBody).toContain("**A check that cannot fail is not a proof.**");
  expect(prBody).toContain(
    "A documented procedure counts only when it ran: reading it, or reading the document that describes it, is not a run"
  );
});

// R3: the unchanged-diff fingerprint covers the branch's own diff alone, so a forward merge can
// bring a new instance of a class the branch already answered (dispatch://INFRA-83). The `E2E`
// re-check wording carries the class search in both files that hold it.
test("the forward-merge re-check searches the new head for each earlier finding's class", () => {
  expect(
    collapsed(path.join(staged, "legion-worker", "references", "conflicts-and-rewrites.md"))
  ).toContain(
    "The fingerprint answers one question only: whether the branch's own diff changed. It cannot show an instance the merge brought in from the base"
  );
  expect(collapsed(path.join(staged, "legion-architect", "references", "acceptance.md"))).toContain(
    "the tester re-runs the bare gates only and the class search of every earlier round's findings at the new head;"
  );
  countedOncePerFile(
    "forward-merge re-check <old-sha> → <new-sha>: fingerprint unchanged, bare gates only, class search of earlier findings: <clean | what it found>",
    [
      "skills/legion-worker/references/pr-body.md",
      "packages/daemon/internal/prompts/roles/tester.md",
    ]
  );
});

// R4: a planner waits on a decision its plan needs rather than completing around it
// (dispatch://INFRA-206). The role prompts carry the rest of R4; roles_test.go pins those.
test("a planner waits on an open decision instead of completing around it", () => {
  expect(collapsed(path.join(staged, "legion-worker", "SKILL.md"))).toContain(
    'A planner is the one exception to "otherwise proceed": a decision the plan itself needs waits, with a WAITING line naming it'
  );
});

// R5: a review agent returns its whole report, and the reviewer refuses a result without one
// (dispatch://INFRA-331). shipped-agents.test.ts pins the two agent definitions.
test("the reviewer treats a result with no report as a failed run, not a verdict", () => {
  const mergeGate = collapsed(path.join(staged, "legion-worker", "references", "merge-gate.md"));
  expect(mergeGate).toContain(
    "A result that carries no report, or that names another place for it, is a failed run, not a verdict"
  );
  expect(mergeGate).toContain(
    "reading the diff yourself in the agent's place is the same thing wearing more work"
  );
  expect(mergeGate).toContain(
    "that half of the round has no review at all: say so to the architect, record no verdict for it, and do not fill the gap yourself"
  );
});

// R6: a pipe's status is the last command's, so `$?` after `cmd | tail` reads the pager's
// (dispatch://INFRA-334). The sentence goes in the always-loaded prompts and agent definitions,
// because skills/legion-worker/references/tools.md loads only on a tool or test question.
test("every shipped prompt and bash-running agent says how to read a piped command's status", () => {
  countedOncePerFile(
    // biome-ignore lint/suspicious/noTemplateCurlyInString: `${PIPESTATUS[0]}` is the shell expansion the rule teaches, quoted verbatim.
    "Read a piped command's status with `${PIPESTATUS[0]}`, or with `set -o pipefail` inside a subshell of that one line; never set `pipefail` at the top level of your shell, where a later `cmd | head` exits 141 and reads as a failure although it did its job. Never pipe a write — a `dispatch` command that records something, or any command whose own output tells you what it did — into `head`, `tail` or `grep`: the status you read would be the pager's, and the line that says what happened is the line you cut.",
    [
      "packages/daemon/internal/prompts/go/worker-common.md",
      "packages/daemon/internal/prompts/go/architect-common.md",
      "packages/daemon/internal/prompts/go/controller-headless.md",
      "packages/daemon/internal/prompts/go/controller-interactive.md",
      "packages/pi-legion/agents/deep-worker.md",
      "packages/pi-legion/agents/plan-gap-analyst.md",
      "packages/pi-legion/agents/plan-reviewer.md",
      "packages/pi-legion/agents/thermonuclear-deep-review.md",
      "packages/pi-legion/agents/thermonuclear-code-quality.md",
    ]
  );
});

// R7: `READY_UNREVIEWED_CHANGE` has five causes, and the merger's own prompt part is the one
// complete list, so neither file gets a second copy of it (dispatch://INFRA-427).
test("retro and the merge gate say READY_UNREVIEWED_CHANGE has more causes than retro's own paths", () => {
  countedOncePerFile(
    "A path outside retro's own is one of five causes of `READY_UNREVIEWED_CHANGE`: the others are a compare GitHub cut at its 300-file limit, a head that does not descend from the approved one, a compare that gives no file list, and an approved head GitHub does not know. Every one of them sends the head back to review and none is an error to retry; the complete list, with each remedy, is the merger's own role prompt part.",
    ["skills/legion-worker/references/merge-gate.md", "skills/legion-retro/SKILL.md"]
  );
});

// R8: every role of an issue shares one $TMPDIR on the issue's volume, so a glob sweep deletes
// another role's open directory (dispatch://INFRA-654).
test("every prompt and agent that may write says to make its own scratch and delete only what it made", () => {
  countedOncePerFile(
    'Make your own scratch with `mktemp -d` and delete only the paths you made, each by its exact path. Never delete by a glob in a shared directory: every role of this issue writes into the same `$TMPDIR` on the issue\'s volume, so a sweep like `rm -rf "$TMPDIR"/tmp.*` deletes directories another role has open.',
    [
      "packages/pi-legion/agents/deep-worker.md",
      "packages/pi-legion/agents/thermonuclear-deep-review.md",
      "packages/pi-legion/agents/thermonuclear-code-quality.md",
      "skills/thermonuclear-deep-review/SKILL.md",
      "skills/thermonuclear-code-quality/SKILL.md",
      "packages/daemon/internal/prompts/go/worker-common.md",
      "packages/daemon/internal/prompts/go/architect-common.md",
    ]
  );
});
