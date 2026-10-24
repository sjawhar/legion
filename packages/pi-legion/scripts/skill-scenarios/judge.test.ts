import { afterAll, expect, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { answerTurn, judgeRun } from "./judge";
import { FAULT_ROWS } from "./judge-prompts";
import { sharedRuns } from "./score";

// judge.ts gives each prompt a fresh directory under <work>/judging/<run>/ that holds only that
// prompt's inputs and prompt.txt, starts the judge there through an injected runner (rig.sh
// judge-omp in production), and copies the judge's validated answer into the run directory. The
// fake runner below records what each judge saw and writes a fixed answer. Every text is invented.

const work = mkdtempSync(path.join(tmpdir(), "skill-scenarios-judge-"));
afterAll(() => rmSync(work, { recursive: true, force: true }));
mkdirSync(path.join(work, "profiles", "head"), { recursive: true });
writeFileSync(path.join(work, "profiles", "head", "checkout"), "/checkouts/head\n");

const sessionId = "01a16640-8259-71ed-bb68-1e4927b77a01";
const spec = (problem: string) =>
  [
    "## Summary",
    "",
    "The widgets CLI greets a person by name.",
    "",
    "## Problem",
    "",
    problem,
    "",
    "## Goals and non-goals",
    "",
    "Goals: a greeting.",
    "",
    "## Requirements",
    "",
    "- **R1.** The CLI prints a greeting. From: your answer on the greeting.",
    "- **R2.** The greeting ends with a full stop. From: your message on the format.",
    "",
    "## Design",
    "",
    "The CLI prints one line.",
    "",
    "## Decisions",
    "",
    "### Plain text",
    "",
    "Plain text, because scripts read it.",
    "",
    "## Acceptance",
    "",
    "- **A1.** `greet Ada` prints one line. (R1, R2)",
    "",
  ].join("\n");
// Two passages of the person's input that the run's texts copy: one in the spec, one in a document.
const asked = "Please make the widgets command line print a friendly greeting for each person.";
const format = "Each greeting must end with a full stop so that our scripts can split the lines.";
const messages = [
  "<!-- message 2026-10-20T10:00:00Z -->",
  asked,
  "",
  "<!-- comment 2026-10-20T11:00:00Z -->",
  "> The old spec said nothing about the format.",
  "",
  format,
  "",
].join("\n");
const v1 = spec("The CLI prints nothing.");
const v3 = spec(`The CLI prints nothing. ${asked}`);
const sourceAsks = [
  {
    id: "src-1",
    kind: "question",
    question: "Which output format?",
    options: [{ label: "Plain text" }, { label: "JSON" }],
    multiple: false,
    state: "answered",
    created_at: "2026-10-20T12:00:00Z",
    answer: {
      selected: ["Plain text"],
      text: "Plain text.",
      user: "alice",
      at: "2026-10-20T12:05:00Z",
    },
  },
];
const ask = (id: string, question: string, createdAt: string) => ({
  id,
  kind: "question",
  question,
  options: [{ label: "Ada" }, { label: "Grace", description: "The second name." }],
  multiple: false,
  state: "open",
  answer: null,
  created_at: createdAt,
});
/** An assistant turn at `at` whose one tool call is `tool` with `args`. */
const call = (at: string, id: string, tool: string, args: object) => ({
  type: "message",
  timestamp: at,
  message: { role: "assistant", content: [{ type: "toolCall", id, name: tool, arguments: args }] },
});
const answered = (at: string, id: string, tool: string, text: string, details: object = {}) => ({
  type: "message",
  timestamp: at,
  message: {
    role: "toolResult",
    toolName: tool,
    toolCallId: id,
    isError: false,
    details,
    content: [{ type: "text", text }],
  },
});
/** A doc-edit of the run's spec, answered as Dispatch answers one that made a version. */
const docEdit = (at: string, id: string, version: number) => [
  call(at, id, "bash", {
    command: "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json",
  }),
  answered(at, id, "bash", `Applied 1 ops (version ${version})\nDocument token: sha256:abc`, {
    exitCode: 0,
  }),
];
const transcript = [
  { type: "session", id: sessionId, cwd: "/rig/cwd" },
  { type: "message", message: { role: "user", content: [{ type: "text", text: "Design it." }] } },
  // A check that names a path under the checkout: not a spec-only check.
  call("2026-10-24T10:00:00.000Z", "c0", "task", {
    context: `${v1}\nRead /checkouts/head/packages/cli/greet.ts too.`,
    tasks: [{ name: "CodeCheck", task: "Check it against the code." }],
  }),
  answered("2026-10-24T10:00:30.000Z", "c0", "task", "<task-result>The code agrees.</task-result>"),
  // A spec-only check that answers in its own result.
  call("2026-10-24T10:01:00.000Z", "c1", "task", {
    context: `Read this spec alone.\n\n${v1}`,
    tasks: [{ name: "SpecCheck1", task: "List each gap." }],
  }),
  answered(
    "2026-10-24T10:01:30.000Z",
    "c1",
    "task",
    '<task-result id="SpecCheck1">R2 names no source.</task-result>'
  ),
  ...docEdit("2026-10-24T10:02:00.000Z", "w1", 2),
  // A spec-only check that runs in the background: its report arrives as a message of its own.
  call("2026-10-24T10:04:00.000Z", "c2", "task", {
    context: `Read this spec alone.\n\n${v3}`,
    tasks: [{ name: "SpecCheck2", task: "List each gap." }],
  }),
  answered(
    "2026-10-24T10:04:01.000Z",
    "c2",
    "task",
    "Spawned agent `SpecCheck2` (job `SpecCheck2`).\n\nResults auto-deliver."
  ),
  {
    type: "custom_message",
    customType: "async-result",
    timestamp: "2026-10-24T10:05:00.000Z",
    content:
      '<system-notice>\nBackground job SpecCheck2 has completed.\n<task-result id="SpecCheck2" agent="task" status="completed">\nA1 tests no requirement alone.\n</task-result>\n</system-notice>',
  },
  ...docEdit("2026-10-24T10:06:00.000Z", "w2", 3),
];

/** A spec-writing run directory as rig.sh and seed.ts write one; `seeded-1` is a seeded ask that
 * capture-spec failed to leave out, so judge.ts must leave it out itself. */
function runDir(name: string, extra: Record<string, string> = {}): string {
  const dir = path.join(work, "runs", name);
  for (const sub of ["sessions", "source", "documents", "spec-versions"])
    mkdirSync(path.join(dir, sub), { recursive: true });
  const files: Record<string, string> = {
    "out.txt": "exit=0\n",
    "source.txt": "acme-1\n",
    "fixture.json": JSON.stringify({ issue: "LWEVAL-7", spec_artifact: "art-spec" }),
    "seeded.json": JSON.stringify(["seeded-1"]),
    "turns.txt": "1 first exit=0\n2 answered exit=0\n3 approved exit=0\n",
    "spec.md": v3,
    "spec-versions.json": JSON.stringify([
      {
        version: 1,
        created_at: "2026-10-24T10:02:01Z",
        authors: [{ kind: "session", id: sessionId }],
      },
      { version: 2, created_at: "2026-10-24T10:03:00Z", authors: [{ kind: "user", id: "alice" }] },
      {
        version: 3,
        created_at: "2026-10-24T10:06:01Z",
        authors: [{ kind: "session", id: sessionId }],
      },
    ]),
    "spec-versions/1.md": v1,
    "spec-versions/2.md": v1,
    "spec-versions/3.md": v3,
    "agent-asks.json": JSON.stringify([
      ask("ask-2", "Which second name? Recommendation: Grace.", "2026-10-24T10:07:00Z"),
      ask("seeded-1", "Which format did you want?", "2026-10-24T09:00:00Z"),
      ask("ask-1", "Which name? Recommendation: Ada.", "2026-10-24T10:03:00Z"),
      { id: "appr-1", kind: "approval", question: "Approve spec.md v3?", options: [] },
    ]),
    "agent-texts.json": JSON.stringify([
      { id: "msg-1", kind: "message", body: "The spec is ready.", created_at: "" },
      {
        id: "seeded-1",
        kind: "ask",
        body: "Which format did you want?\nPlain text",
        created_at: "",
      },
    ]),
    "state-1.json": JSON.stringify({
      turn: 1,
      open_asks: [
        {
          id: "ask-1",
          question: "Which name?",
          options: [{ label: "Ada" }, { label: "Grace" }],
          multiple: false,
        },
        {
          id: "ask-3",
          question: "Which city?",
          options: [{ label: "Oslo" }, { label: "Rome" }],
          multiple: false,
        },
      ],
      approval: null,
    }),
    "state-2.json": JSON.stringify({
      turn: 2,
      open_asks: [],
      approval: { ask_id: "appr-1", version: 3 },
    }),
    "state-3.json": JSON.stringify({ turn: 3, open_asks: [], approval: null }),
    "source/title.txt": "Greet a person\n",
    "source/messages.md": messages,
    "source/asks.json": JSON.stringify(sourceAsks),
    "documents/brief.md": `# Brief\n\n${format}\n`,
    ...extra,
  };
  for (const [file, text] of Object.entries(files)) writeFileSync(path.join(dir, file), text);
  writeFileSync(
    path.join(dir, "sessions", "2026-10-24T10-00-00-000Z_01a16640.jsonl"),
    transcript.map((entry) => `${JSON.stringify(entry)}\n`).join("")
  );
  return dir;
}

/** Every file under `dir`, relative and sorted. */
function filesUnder(dir: string): string[] {
  return readdirSync(dir, { recursive: true, encoding: "utf8" })
    .filter((file) => statSync(path.join(dir, file)).isFile())
    .sort();
}
/** Every file's text under `dir`, joined. */
const textUnder = (dir: string) =>
  filesUnder(dir)
    .map((file) => readFileSync(path.join(dir, file), "utf8"))
    .join("\n");

interface Seen {
  profile: string;
  dir: string;
  files: string[];
  prompt: string;
  text: string;
}
/** A judge's fixed answer, by the base name of its directory. */
function answerFor(dir: string): [string, unknown] {
  const name = path.basename(dir).replace(/(-\d+)+$/, "");
  const inDir = (file: string) => readFileSync(path.join(dir, file), "utf8");
  const faults = Object.fromEntries(FAULT_ROWS.map((row) => [row, 0]));
  const answers: Record<string, () => [string, unknown]> = {
    faults: () => ["faults.json", { faults, ste_breaks: 1, items: [] }],
    decisions: () => ["decisions.json", { rows: [] }],
    findings: () => [
      "findings.json",
      {
        rows: [
          { check: 1, finding: "R2 names no source.", verdict: "fixed" },
          { check: 2, finding: "A1 tests no requirement alone.", verdict: "neither" },
        ],
      },
    ],
    checkable: () => [
      "checkable.json",
      {
        rows: [
          { requirement: "R1", check: "Run greet.", uncheckable: false },
          { requirement: "R2", check: "Read the last character.", uncheckable: false },
        ],
      },
    ],
    questions: () => [
      "questions.json",
      {
        rows: ["ask-1", "ask-2"].map((id) => ({
          id,
          dominated: false,
          settled: false,
          kind: "taste",
          reason: "A name is taste.",
        })),
      },
    ],
    numbers: () => ["numbers.json", { rows: [{ id: "msg-1", names_number: false, quote: null }] }],
    preamble: () => ["preamble.json", { rows: [] }],
    proposal: () => ["proposal.json", { rows: [] }],
    "shared-runs": () => [
      "shared-run.json",
      { run: inDir("run.txt").trim(), verdict: "copies", reason: "It copies the message." },
    ],
    answer: () => [
      "answer.json",
      {
        selected: [inDir("ask.json").includes("Oslo") ? "Oslo" : "Grace"],
        text: "The input says so.",
      },
    ],
  };
  const found = answers[name];
  if (found === undefined) throw new Error(`no answer for ${dir}`);
  return found();
}
function fakeRunner(seen: Seen[], answer: (dir: string) => [string, unknown] = answerFor) {
  return async (profile: "ste" | "bare", dir: string) => {
    const prompt = readFileSync(path.join(dir, "prompt.txt"), "utf8");
    seen.push({ profile, dir, files: filesUnder(dir), prompt, text: textUnder(dir) });
    const [file, body] = answer(dir);
    writeFileSync(path.join(dir, file), JSON.stringify(body));
    return 0;
  };
}

const judged = runDir("spec-writing-head-1");
const seen: Seen[] = [];
await judgeRun(judged, { runner: fakeRunner(seen) });
// The judges of one run start in parallel, so their order is their directories' order.
const byPrompt = (name: string) =>
  seen
    .filter((s) => path.basename(s.dir).replace(/(-\d+)+$/, "") === name)
    .sort((a, b) => a.dir.localeCompare(b.dir, "en", { numeric: true }));
const only = (name: string): Seen => {
  const [first, ...rest] = byPrompt(name);
  if (first === undefined || rest.length > 0)
    throw new Error(`${name} ran ${byPrompt(name).length} times`);
  return first;
};

test("each judge's working directory holds only its prompt's inputs and prompt.txt", () => {
  const expected: Record<string, string[]> = {
    faults: ["faults.md", "prompt.txt", "spec.md"],
    decisions: ["prompt.txt", "spec.md"],
    findings: [
      "check-1/assignment.md",
      "check-1/report.md",
      "check-1/spec-after.md",
      "check-2/assignment.md",
      "check-2/report.md",
      "check-2/spec-after.md",
      "prompt.txt",
    ],
    checkable: ["prompt.txt", "spec.md"],
    questions: ["agent-asks.json", "asks.json", "messages.md", "prompt.txt"],
    numbers: ["agent-texts.json", "prompt.txt"],
    preamble: ["agent-texts.json", "documents/brief.md", "messages.md", "prompt.txt", "spec.md"],
    proposal: ["agent-asks.json", "prompt.txt", "spec.md"],
  };
  for (const [name, files] of Object.entries(expected)) {
    const judge = only(name);
    expect(judge.dir).toBe(path.join(work, "judging", "spec-writing-head-1", name));
    expect(judge.files).toEqual(files);
    expect(judge.profile).toBe("ste");
  }
  // The faults judge gets the five rows of the fault table.
  for (const row of FAULT_ROWS) expect(only("faults").text).toContain(row);
});

test("each prompt names the one file the judge writes its answer to", () => {
  for (const judge of seen) {
    const [file] = answerFor(judge.dir);
    expect(judge.prompt).toContain(path.join(judge.dir, file));
  }
});

test("the inputs of questions, numbers and preamble hold no seeded ask", () => {
  for (const name of ["questions", "numbers", "preamble", "proposal"])
    expect(only(name).text).not.toContain("seeded-1");
  // The rest of the run's asks reach the questions judge, oldest first, the approval request not.
  const asks = JSON.parse(
    readFileSync(path.join(only("questions").dir, "agent-asks.json"), "utf8")
  );
  expect(asks.map((a: { id: string }) => a.id)).toEqual(["ask-1", "ask-2"]);
});

test("findings reads each spec-only check's assignment and report and the version written after it", () => {
  const dir = only("findings").dir;
  const read = (file: string) => readFileSync(path.join(dir, file), "utf8");
  expect(read("check-1/assignment.md")).toContain(v1);
  expect(read("check-1/report.md")).toContain("R2 names no source.");
  expect(read("check-1/spec-after.md")).toBe(v1);
  expect(read("check-2/report.md")).toContain("A1 tests no requirement alone.");
  // Version 2 is the person's: the agent's write after the second check made version 3.
  expect(read("check-2/spec-after.md")).toBe(v3);
  expect(only("findings").text).not.toContain("/checkouts/head/packages");
});

test("checkable reads the approved version of the spec alone", () => {
  expect(readFileSync(path.join(only("checkable").dir, "spec.md"), "utf8")).toBe(v3);
  const out = JSON.parse(readFileSync(path.join(judged, "checkable.json"), "utf8"));
  expect(out.version).toBe(3);
});

test("the shared-runs judge runs once for each shared run, with no skills, on the run, its context and the rule", () => {
  const runs = sharedRuns(judged);
  expect(runs.length).toBe(2);
  const judges = byPrompt("shared-runs");
  expect(judges.length).toBe(runs.length);
  for (const [index, judge] of judges.entries()) {
    expect(judge.profile).toBe("bare");
    expect(judge.dir).toBe(
      path.join(work, "judging", "spec-writing-head-1", `shared-runs-${index + 1}`)
    );
    expect(judge.files).toEqual(["context.md", "prompt.txt", "rule.md", "run.txt"]);
    expect(readFileSync(path.join(judge.dir, "run.txt"), "utf8").trim()).toBe(
      runs[index]?.run ?? ""
    );
    expect(judge.text).not.toContain("seeded-1");
  }
  const out = JSON.parse(readFileSync(path.join(judged, "shared-runs.json"), "utf8"));
  expect(out.rows).toEqual(
    runs.map((run) => ({
      source: run.source,
      run: run.run,
      verdict: "copies",
      reason: "It copies the message.",
    }))
  );
  expect(runs.map((run) => run.source)).toEqual(["spec.md", "documents/brief.md"]);
});

test("every judge output reaches the run directory", () => {
  for (const file of [
    "faults.json",
    "decisions.json",
    "findings.json",
    "checkable.json",
    "questions.json",
    "numbers.json",
    "preamble.json",
    "proposal.json",
    "shared-runs.json",
  ])
    expect(existsSync(path.join(judged, file))).toBe(true);
  expect(seen.length).toBe(8 + 2);
});

test("a judge that writes no answer, or one that does not parse, fails the run naming the file", async () => {
  const dir = runDir("spec-writing-head-2");
  const silent = fakeRunner([], (judge) =>
    path.basename(judge) === "decisions" ? ["other.json", {}] : answerFor(judge)
  );
  await expect(judgeRun(dir, { runner: silent })).rejects.toThrow(
    path.join(work, "judging", "spec-writing-head-2", "decisions", "decisions.json")
  );
  const wrong = fakeRunner([], (judge) =>
    path.basename(judge) === "numbers"
      ? ["numbers.json", { rows: [{ id: "msg-1" }] }]
      : answerFor(judge)
  );
  await expect(judgeRun(dir, { runner: wrong })).rejects.toThrow(
    `${path.join(work, "judging", "spec-writing-head-2", "numbers", "numbers.json")} does not parse`
  );
});

test("answer runs the stand-in once for each open ask and writes its answers", async () => {
  const dir = runDir("spec-writing-head-3");
  const asked: Seen[] = [];
  await answerTurn(dir, 1, { runner: fakeRunner(asked) });
  expect(asked.map((judge) => path.basename(judge.dir)).sort()).toEqual([
    "answer-1-1",
    "answer-1-2",
  ]);
  for (const judge of asked) {
    expect(judge.profile).toBe("ste");
    expect(judge.files).toEqual(["ask.json", "asks.json", "messages.md", "prompt.txt"]);
  }
  expect(readFileSync(path.join(dir, "answers-1.jsonl"), "utf8")).toBe(
    [
      JSON.stringify({ ask: "ask-1", selected: ["Grace"], text: "The input says so." }),
      JSON.stringify({ ask: "ask-3", selected: ["Oslo"], text: "The input says so." }),
      "",
    ].join("\n")
  );
});

test("answer refuses a label the ask does not offer, and writes an empty file when no ask is open", async () => {
  const dir = runDir("spec-writing-head-4");
  const invented = fakeRunner([], () => ["answer.json", { selected: ["Paris"], text: "No." }]);
  await expect(answerTurn(dir, 1, { runner: invented })).rejects.toThrow(
    `${path.join(work, "judging", "spec-writing-head-4", "answer-1-1", "answer.json")} does not parse`
  );
  const none: Seen[] = [];
  await answerTurn(dir, 3, { runner: fakeRunner(none) });
  expect(none).toEqual([]);
  expect(readFileSync(path.join(dir, "answers-3.jsonl"), "utf8")).toBe("");
});

// fetch-spec-sources.ts writes `>` lines for a comment's anchored quote alone, at the start of the
// comment's text. Any other `>` line, such as a block quote in a message, is the person's words.
test("each prompt that reads messages.md says only a comment's leading `>` lines quote the spec", async () => {
  const asked: Seen[] = [];
  await answerTurn(runDir("spec-writing-head-5"), 1, { runner: fakeRunner(asked) });
  const prompts = [...asked, only("questions"), only("preamble")].map((judge) => judge.prompt);
  expect(prompts).toHaveLength(4);
  for (const prompt of prompts) {
    expect(prompt).toContain(
      "The `>` lines directly after a `<!-- comment <time> -->` line quote the spec that the comment is about. They are not the person's words. Every other line is the person's words."
    );
    expect(prompt).not.toContain("A line that starts with `>` quotes the spec");
  }
});
