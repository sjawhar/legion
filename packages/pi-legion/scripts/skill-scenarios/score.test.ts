import { afterAll, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { cliRefusals } from "@legion/envoy-client/dispatch-cli";

// A run whose agent never got a model turn (Oh My Pi exits before its first request, as when the
// profile's gateway key command runs past its budget) says nothing about the skill under test: the
// score reports it as a rig error and leaves it out of the pass counts. An agent that got a turn and
// then did nothing the scenario asks is a failure.

// score.ts reads each label's checkout from <work>/profiles/<label>/checkout, beside <work>/runs.
const work = mkdtempSync(path.join(tmpdir(), "skill-scenarios-score-"));
const runs = path.join(work, "runs");
afterAll(() => rmSync(work, { recursive: true, force: true }));
for (const label of ["head", "base", "cross"]) {
  mkdirSync(path.join(work, "profiles", label), { recursive: true });
  writeFileSync(path.join(work, "profiles", label, "checkout"), `/checkouts/${label}\n`);
}

/** Oh My Pi's transcript header entries: what a session writes before its first model request. */
const header = [
  { type: "session", id: "01a0f2cc", cwd: "/rig/cwd" },
  { type: "model_change", model: "anthropic/claude-opus-4-8" },
  { type: "thinking_level_change", thinkingLevel: "high" },
];
/** The prompt and a reply that does nothing. */
const turn = [
  { type: "message", message: { role: "user", content: [{ type: "text", text: "Test it." }] } },
  { type: "message", message: { role: "assistant", content: [{ type: "text", text: "Done." }] } },
];
const noKey = "error: No API key found for anthropic.\nexit=1\n";

function run(name: string, files: Record<string, string>, transcript: object[]) {
  const dir = path.join(runs, name);
  mkdirSync(path.join(dir, "sessions"), { recursive: true });
  for (const [file, text] of Object.entries(files)) writeFileSync(path.join(dir, file), text);
  writeFileSync(
    path.join(dir, "sessions", "2026-09-30T14-51-17-651Z_01a0f2cc.jsonl"),
    transcript.map((entry) => `${JSON.stringify(entry)}\n`).join("")
  );
}

const world = JSON.stringify({
  key: "LWEVAL-1",
  repo: "example/widgets",
  pr: 1001,
  branch: "legion/LWEVAL-1",
  head: "099b05dc19f7f7e5058e791d16ae05f0b123a2cc",
  code: "5f36e1aa2d0c4b8e9a6f1e3d7c5b9a0f2e4d6c8b",
});
run("tester-proof-head-1", { "out.txt": noKey, "world.json": world }, header);
run("tester-proof-head-2", { "out.txt": "exit=0\n", "world.json": world }, [...header, ...turn]);

const seeded = JSON.stringify([
  { type: "issue.created", actor: { kind: "session", id: "dispatch-owner" } },
  { type: "message.created", actor: { kind: "session", id: "dispatch-owner" } },
]);
const message = JSON.stringify({ message: { body: "Plan: re-check the attachment table." } });
const capture = { "asks.json": "[]", "events.json": seeded, "message.json": message };
run("ask-on-message-head-1", { ...capture, "out.txt": noKey }, header);
run("ask-on-message-head-2", { ...capture, "out.txt": "exit=0\n" }, [...header, ...turn]);

/** An assistant turn whose one tool call is `bash` running `command`, before the handoff write. */
const bash = (command: string) => ({
  type: "message",
  timestamp: "2026-09-30T17:27:30.000Z",
  message: {
    role: "assistant",
    content: [{ type: "toolCall", id: "call-1", name: "bash", arguments: { command } }],
  },
});
/** The tester's handoff write: its `write` tool call of .legion/<key>/test.json, after the run's
 * other calls. */
const writeHandoff = {
  type: "message",
  timestamp: "2026-09-30T17:29:11.031Z",
  message: {
    role: "assistant",
    content: [
      {
        type: "toolCall",
        id: "call-2",
        name: "write",
        arguments: { path: "/rig/ws/.legion/LWEVAL-1/test.json", content: "{}" },
      },
    ],
  },
};
const calls = (records: object[]) =>
  records.map((record) => `${JSON.stringify(record)}\n`).join("");
// Ran the CLI: the bun stand-in recorded `"$BUN" greet.ts Ada`, a command line no pattern names.
run(
  "tester-proof-ran-1",
  {
    "out.txt": "exit=0\n",
    "world.json": world,
    "calls.jsonl": calls([
      { at: "2026-09-30T17:27:33.658Z", as: "bun", argv: ["greet.ts", "Ada"], exit: 0 },
    ]),
  },
  [...header, turn[0], bash('BUN=$(command -v bun); "$BUN" greet.ts Ada'), writeHandoff]
);
// Only mentioned it: a PR body drafted in a heredoc names `bun greet.ts`, and bun never ran it.
run(
  "tester-proof-ran-2",
  { "out.txt": "exit=0\n", "world.json": world, "calls.jsonl": calls([]) },
  [
    ...header,
    turn[0],
    bash("cat > /tmp/pr-body.md <<'EOF'\nAdds `greet(name)` and `bun greet.ts <name>`.\nEOF"),
    writeHandoff,
  ]
);
// Read the other label's checkout, in a command whose path starts past its 200th character.
run("tester-proof-cross-1", { "out.txt": "exit=0\n", "world.json": world }, [
  ...header,
  turn[0],
  bash(`echo ${"x".repeat(220)} && cat /checkouts/base/skills/legion-worker/SKILL.md`),
]);

/** A measure-before-ask run that opened one ask: its question and the options' labels. */
function measured(name: string, question: string, labels: string[]) {
  const asks = [{ question, options: labels.map((label) => ({ label, description: "" })) }];
  run(name, { ...capture, "asks.json": JSON.stringify(asks), "out.txt": "exit=0\n" }, [
    ...header,
    ...turn,
  ]);
}
const stranded =
  "430 issues point at the retired billing-api component, and only 18 of them name an owner, so emailing owners cannot re-home the other 412.";
// Dropped the option the export rules out.
measured("measure-before-ask-head-1", stranded, [
  "Move all 430 to platform-core",
  "Detach all, mark not architectural",
]);
// Kept it cut down to the part that works, which still repairs 18 of 430.
measured("measure-before-ask-head-2", stranded, [
  "Move all 430 to platform-core",
  "Detach all",
  "Email the 18 owners",
]);
// Dropped it, but never said how many are affected.
measured("measure-before-ask-head-3", "Only a few name an owner, so emailing them is out.", [
  "Move all to platform-core",
  "Detach all",
]);

// The shapes a real Oh My Pi transcript carries: a tool's result is a `toolResult` message, and an
// agent reaches Dispatch by running the `dispatch` command in `bash`, never a tool of its own name.
/** A tool's result, answering the call `toolCallId` names. */
const result = (toolCallId: string, text: string) => ({
  type: "message",
  message: { role: "toolResult", toolCallId, content: [{ type: "text", text }] },
});
/** An assistant turn whose one tool call is `tool`, with `args`. */
const toolCall = (id: string, tool: string, args: object) => ({
  type: "message",
  message: { role: "assistant", content: [{ type: "toolCall", id, name: tool, arguments: args }] },
});
const brainstormed = { "out.txt": "exit=0\n", "brainstorm.json": '{"issue":"TODO-3","asks":[]}\n' };
// Dispatch's answer when it creates the issue, as `dispatch issue` prints it.
const created =
  "Created TODO-3: Due dates → http://dispatch.test/issues/TODO-3 (not subscribed to TODO-3)";
const createIssue = [
  toolCall("call-2", "bash", {
    command: "dispatch issue --project TODO --title 'Due dates'",
  }),
  result("call-2", created),
];
// Put the design in the spec and leaked nothing looking for the project.
run("brainstorm-head-1", brainstormed, [...header, turn[0], ...createIssue]);
// A bare `env` call: its own arguments name nothing secret, but the result it brings back into
// the model's context carries the token.
run("brainstorm-head-2", brainstormed, [
  ...header,
  turn[0],
  toolCall("call-1", "bash", { command: "env" }),
  result(
    "call-1",
    "HOME=/rig/home\nDISPATCH_URL=http://127.0.0.1:18790\nDISPATCH_TOKEN=e2e-secret\n"
  ),
  ...createIssue,
]);
// Created the issue from inside an `eval` cell: no top-level `dispatch issue` command, and
// Dispatch's answer sits mid-line in the cell's display.
run("brainstorm-head-3", brainstormed, [
  ...header,
  turn[0],
  toolCall("call-3", "eval", {
    language: "js",
    code: 'await $`dispatch issue --project TODO --title "Due dates"`',
  }),
  result("call-3", `display[1]:\n{\n  "text": "${created}",\n  "details": {}\n}`),
]);
// Ran `dispatch issue`, and Dispatch refused it as a duplicate: no issue of its own.
run("brainstorm-head-4", brainstormed, [
  ...header,
  turn[0],
  createIssue[0] ?? {},
  result("call-2", 'Not created: "Due dates" looks like a duplicate.\nTODO-3 [triage] Due dates'),
]);

// spec-writing and spec-rewrite (score.ts specRow). The run below follows every rule, and each
// other run changes one thing in it. Every text is invented.
const specSession = "01a16640-8259-71ed-bb68-1e4927b77a01";
const specHeader = [{ type: "session", id: specSession, cwd: "/rig/cwd" }, ...header.slice(1)];
/** A spec in the outline: `problem` is its Problem paragraph, `design` an extra Design paragraph
 * and `summary` an extra Summary sentence. */
function outlined({ problem = "The CLI prints no greeting.", design = "", summary = "" } = {}) {
  return [
    "## Summary",
    "",
    `The widgets CLI greets a person by name.${summary}`,
    "",
    "## Problem",
    "",
    problem,
    "",
    "## Goals and non-goals",
    "",
    "Goals: a greeting. Non-goals: translations.",
    "",
    "## Requirements",
    "",
    "- **R1.** The CLI prints a greeting with the name. From: your answer on the greeting.",
    "",
    "## Design",
    "",
    "`greet <name>` prints `Hello, <name>.`",
    design,
    "",
    "## Decisions",
    "",
    "### Plain text output",
    "",
    "The CLI prints plain text. Reason: scripts read it. Options: plain text or JSON. Parts: Design.",
    "",
    "## Acceptance",
    "",
    "- **A1.** `greet Ada` prints `Hello, Ada.` (R1)",
    "",
  ].join("\n");
}
// The person's input: messages.md less each entry's header and each anchored comment's quote (the
// `>` lines right after a `<!-- comment <time> anchored -->` header), and each answer's text in
// asks.json. Every other `>` line is the person's own words: a block quote in a message, one after
// a comment's quote, or one an unanchored comment starts with.
const askedFor =
  "Please make the widgets command line print a friendly greeting whenever somebody runs it with their name.";
const quotedLine = "The command line prints nothing at all when a person runs it today.";
const longMessage =
  "The greeting must stay on one line, use the name exactly as the person typed it, end with a full stop, and never print the name twice even when the person repeats it on the command line.";
const voiceLine =
  "I want the greeting to feel warm, so pick words a support agent would really say out loud.";
const commentQuote =
  "A colleague wrote that the old banner scrolled off the top of every small terminal window.";
const pastedLine =
  "The old ticket said every run of the greeting tool gets eight turns before it stops.";
const personMessages = [
  "<!-- message 2026-10-20T10:00:00Z -->",
  askedFor,
  "",
  "<!-- comment 2026-10-20T11:00:00Z anchored -->",
  `> ${quotedLine}`,
  "",
  "Keep the output short so that scripts can still read each line.",
  "",
  `> ${commentQuote}`,
  "",
  "<!-- message 2026-10-21T09:00:00Z -->",
  longMessage,
  "",
  "<!-- message 2026-10-22T09:00:00Z -->",
  `> ${voiceLine}`,
  "",
  "That is what I said on the call.",
  "",
  "<!-- comment 2026-10-23T09:00:00Z -->",
  `> ${pastedLine}`,
  "",
  "I pasted that from the old ticket.",
  "",
].join("\n");
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
      text: "Plain text, since scripts parse the output line by line.",
      user: "alice",
      at: "2026-10-20T12:05:00Z",
    },
  },
];
/** The words of a text as the `quoted` score compares them: case and punctuation folded. */
const folded = (text: string) => (text.toLowerCase().match(/[\p{L}\p{N}]+/gu) ?? []).join(" ");

/** One line the dispatch CLI logs for a call it ran (dispatch-session-state.ts `ResultEntry`),
 * before its time: the tool, and the refusal when Dispatch or the call refused it. */
interface Logged {
  tool: string;
  error?: string;
}
/** What the CLI logs for a call when the fixture says nothing else: each `dispatch` command the
 * call ran, as a success, when the call exited 0 and its output shows no refusal line, and no
 * `--help` or `--dry-run` call. A refused call logs nothing by default. The CLI logs a refusal too
 * when the call knows its session (dispatch-cli.ts runDispatchCli): under the command's tool, or
 * under `dispatch` for a command it cannot name. A fixture that models such a call names its lines,
 * and one that leaves them out models a call that knew no session. */
const loggedRun = (command: string, exit: number, text: string): Logged[] =>
  exit !== 0 || cliRefusals(text).length > 0
    ? []
    : [...command.matchAll(/(?:^|[;&|(\n])\s*dispatch ([a-z][a-z-]*)([^;&|\n]*)/g)]
        .filter(([, , args]) => !/--help|--dry-run/.test(args ?? ""))
        .map(([, name]) => ({ tool: `dispatch_${(name ?? "").replace(/-/g, "_")}` }));
/** A `bash` call and its result, as a real Oh My Pi `bash` result records its exit code, and the
 * ledger lines the CLI wrote while it ran (`logged`, loggedRun by default), which specRun moves into
 * the run's ledger. */
const sh = (
  id: string,
  command: string,
  exit = 0,
  text = "ok",
  logged: Logged[] = loggedRun(command, exit, text)
) => [
  toolCall(id, "bash", { command }),
  {
    type: "message",
    logged,
    message: {
      role: "toolResult",
      toolName: "bash",
      toolCallId: id,
      isError: exit !== 0,
      details: { exitCode: exit },
      content: [
        { type: "text", text: exit === 0 ? text : `${text}\n\nCommand exited with code ${exit}` },
      ],
    },
  },
];
/** An `eval` cell and its result. Its code runs `dispatch` through its own language, which no
 * shell parse reads, and Oh My Pi's kernel is given no session, so the CLI logs nothing for it
 * (`logged` is empty unless a fixture says otherwise). */
const evalCell = (id: string, code: string, text: string, logged: Logged[] = []) => [
  toolCall(id, "eval", { code, language: "python" }),
  {
    type: "message",
    logged,
    message: {
      role: "toolResult",
      toolName: "eval",
      toolCallId: id,
      isError: false,
      content: [{ type: "text", text }],
    },
  },
];
const readOf = (id: string, target: string) => [
  toolCall(id, "read", { path: target }),
  result(id, "text"),
];
/** A spec-only check: a `task` call whose assignment holds the spec, and its report as a blocking
 * task answers, one completed `<task-result>` block per task. */
const checkOf = (id: string, assignment = outlined()) => [
  toolCall(id, "task", {
    context: `Read this spec alone and list each gap.\n\n${assignment}`,
    tasks: [{ name: `Check${id}`, task: "List each gap." }],
  }),
  result(
    id,
    `<task-result id="Check${id}" agent="task" status="completed" duration="40s">\nNo gaps.\n</task-result>`
  ),
];
/** A doc-edit of the run's spec, answered as Dispatch answers one that made a version. */
const specEdit = (id: string, version = 2) =>
  sh(
    id,
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json",
    0,
    `Applied 1 ops (version ${version})\nDocument token: sha256:abc`
  );
const routedRead = readOf("r1", "skill://dispatch-writing-specs");
const askStep = sh(
  "a1",
  "dispatch ask --issue LWEVAL-7 --question 'Which name? Recommendation: Ada.'"
);
const finish = [
  ...sh("p1", "dispatch request-approval --issue LWEVAL-7 --summary 'The greeting.'"),
  ...readOf("r2", "skill://writing-plans"),
  ...sh("p2", "dispatch artifact --issue LWEVAL-7 --name plan.md --content-file plan.md"),
];
const conversation = (...steps: object[][]) => [...specHeader, turn[0], ...steps.flat()];
const followed = conversation(
  routedRead,
  checkOf("c1"),
  specEdit("w1"),
  askStep,
  checkOf("c2"),
  specEdit("w2"),
  finish
);
const noFaults = {
  "The reader cannot follow a sentence": 0,
  "A term that nobody explained": 0,
  "Too much text, or facts that do not matter": 0,
  "Vague phrasing that hides who did not act": 0,
  "History in place of design": 0,
};
/** faults.json with `counts` faults in their rows, each item quoting the spec. */
function faultsFile(counts: Partial<typeof noFaults> = {}) {
  const faults = { ...noFaults, ...counts };
  const items = Object.entries(faults).flatMap(([row, n]) =>
    Array.from({ length: n }, () => ({ row, quote: "greets a person" }))
  );
  return { faults, ste_breaks: 0, items };
}
const cleanJudges: Record<string, object> = {
  "faults.json": faultsFile(),
  "decisions.json": {
    rows: [
      {
        choice: "The CLI prints plain text.",
        recorded: true,
        reason: true,
        options: true,
        parts: true,
      },
    ],
  },
  "findings.json": { rows: [{ check: 1, finding: "R1 names no check.", verdict: "fixed" }] },
  "checkable.json": {
    version: 3,
    rows: [{ requirement: "R1", check: "Run greet Ada and read the line.", uncheckable: false }],
  },
  "questions.json": {
    rows: [
      { id: "ask-1", dominated: false, settled: false, kind: "taste", reason: "A name is taste." },
    ],
  },
  "numbers.json": { rows: [{ id: "msg-1", names_number: false, quote: null }] },
  "preamble.json": { rows: [] },
  "proposal.json": { rows: [{ item: "the greeting word", verdict: "proposed", ask: null }] },
  "shared-runs.json": { rows: [] },
};
const openAsk = (id: string) => ({
  id,
  question: "Which name should the example use?",
  options: [{ label: "Ada" }, { label: "Grace" }],
  multiple: false,
});
const approvedStates = [
  { turn: 1, open_asks: [openAsk("ask-1")], approval: null },
  { turn: 2, open_asks: [], approval: { ask_id: "appr-1", version: 3 } },
  { turn: 3, open_asks: [], approval: null },
];
interface SpecFixture {
  spec?: string;
  transcript?: object[];
  judges?: Record<string, object>;
  seeded?: string[];
  states?: object[];
  turns?: string;
  documents?: Record<string, string>;
  source?: string;
  /** false: the run has no CLI ledger. */
  ledger?: boolean;
  /** agent-asks.json and agent-texts.json, when not the run's one question and one message. */
  asks?: object[];
  texts?: object[];
}
/** The run's transcript and its dispatch CLI ledger (results.jsonl): each entry gets a time one
 * second after the one before it, and each line an entry logs (`logged`) is written to the ledger
 * one millisecond apart, just before that entry's time, so it falls inside its tool call. */
function timed(transcript: object[]): { entries: object[]; ledger: object[] } {
  const start = Date.parse("2026-10-24T10:00:00.000Z");
  const ledger: object[] = [];
  const entries = transcript.map((entry, index) => {
    const { logged, ...rest } = entry as { logged?: Logged[]; timestamp?: string };
    const at = rest.timestamp ?? new Date(start + index * 1000).toISOString();
    for (const [n, line] of (logged ?? []).entries())
      ledger.push({
        at: new Date(Date.parse(at) - (logged?.length ?? 0) + n).toISOString(),
        tool: line.tool,
        ...(line.error === undefined ? { details: {} } : { error: line.error }),
      });
    return { ...rest, timestamp: at };
  });
  return { entries, ledger };
}
/** A spec-writing or spec-rewrite run directory, as rig.sh, seed.ts and judge.ts write one. */
function specRun(name: string, fixture: SpecFixture = {}) {
  const spec = fixture.spec ?? outlined();
  const states = fixture.states ?? approvedStates;
  const judges = { ...cleanJudges, ...fixture.judges };
  const { entries, ledger } = timed(fixture.transcript ?? followed);
  run(
    name,
    {
      "out.txt": "exit=0\n",
      "source.txt": `${fixture.source ?? "acme-1"}\n`,
      "fixture.json": JSON.stringify({ issue: "LWEVAL-7", spec_artifact: "art-spec" }),
      "seeded.json": JSON.stringify(fixture.seeded ?? []),
      "turns.txt": fixture.turns ?? "1 first exit=0\n2 answered exit=0\n3 approved exit=0\n",
      "spec.md": spec,
      "plan.md": "# Plan\n\n1. Add greet.\n",
      "spec-versions.json": JSON.stringify(
        [2, 3].map((version) => ({
          version,
          created_at: `2026-10-24T10:0${version}:00Z`,
          authors: [{ kind: "session", id: specSession }],
        }))
      ),
      "agent-asks.json": JSON.stringify(
        fixture.asks ?? [
          { kind: "question", ...openAsk("ask-1"), state: "answered" },
          { id: "appr-1", kind: "approval", question: "Approve spec.md v3?", options: [] },
        ]
      ),
      "agent-texts.json": JSON.stringify(
        fixture.texts ?? [
          { id: "msg-1", kind: "message", body: "The spec is ready to read.", created_at: "" },
        ]
      ),
      ...Object.fromEntries(
        states.map((state, index) => [`state-${index + 1}.json`, JSON.stringify(state)])
      ),
      ...Object.fromEntries(
        Object.entries(judges).map(([file, body]) => [file, JSON.stringify(body)])
      ),
    },
    entries
  );
  const dir = path.join(runs, name);
  for (const sub of ["source", "documents", "spec-versions"])
    mkdirSync(path.join(dir, sub), { recursive: true });
  writeFileSync(path.join(dir, "source", "title.txt"), "Greet a person by name\n");
  writeFileSync(path.join(dir, "source", "messages.md"), personMessages);
  writeFileSync(path.join(dir, "source", "asks.json"), JSON.stringify(sourceAsks));
  for (const version of [2, 3])
    writeFileSync(path.join(dir, "spec-versions", `${version}.md`), spec);
  for (const [file, text] of Object.entries(fixture.documents ?? {}))
    writeFileSync(path.join(dir, "documents", file), text);
  if (fixture.ledger === false) return;
  const state = path.join(ledgerDir(name));
  mkdirSync(state, { recursive: true });
  writeFileSync(
    path.join(state, "results.jsonl"),
    ledger.map((line) => `${JSON.stringify(line)}\n`).join("")
  );
}
/** Where the dispatch CLI keeps a run's ledger: the run's HOME's state directory, under the
 * session id the transcript's header names (dispatch-session-state.ts sessionDirectory). */
const ledgerDir = (name: string) =>
  path.join(runs, name, "home", ".local", "state", "dispatch", "sessions", specSession);
const copied = (source: string, words: string, verdict: string) => ({
  "shared-runs.json": { rows: [{ source, run: words, verdict, reason: "The verdict." }] },
});

specRun("spec-writing-head-1");
specRun("spec-rewrite-head-1");
// Not in the outline: a heading of C2 is gone.
specRun("spec-writing-head-2", { spec: outlined().replace("## Goals and non-goals", "## Scope") });
// No check before the first spec write.
specRun("spec-writing-head-3", {
  transcript: conversation(
    routedRead,
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A finding the agent neither fixed nor recorded.
specRun("spec-writing-head-4", {
  judges: { "findings.json": { rows: [{ check: 1, finding: "No owner.", verdict: "neither" }] } },
});
const refusal = "dispatch ask: the question gives no recommendation; end it with one";
const refused = sh("x1", "dispatch ask --issue LWEVAL-7 --question 'Which name?'", 2, refusal);
// A refused call fixed from its refusal alone.
specRun("spec-writing-head-5", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    refused,
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A refused call fixed only after a read of a skill file.
specRun("spec-writing-head-6", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    refused,
    readOf("r3", "skill://dispatch"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// Refusals the dispatch CLI printed itself, through a pipe that hid the exit (bash exit 0, no
// isError). Each text is the real CLI's: an argument check's (exit 1 unpiped) and a usage
// refusal's (exit 2 unpiped). The CLI's other `dispatch:` lines, a warning and a note that it kept
// no record of a call Dispatch took, refuse nothing.
const pipedRefusal =
  "dispatch doc-edit was not called: 1 problem\n- --precondition-json is not JSON: JSON Parse error: Unterminated string";
const piped = sh(
  "x2",
  'dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json --precondition-json \'{"document":"sha\' 2>&1 | head -20',
  0,
  pipedRefusal
);
const offRefusal =
  "dispatch: no envoy.json at /home/agent/.config/legion/envoy.json: Dispatch is off";
const pipedOff = sh(
  "x4",
  "dispatch ask --issue LWEVAL-7 --question 'Which name? Recommendation: Ada.' 2>&1 | tee ask.log",
  0,
  offRefusal
);
const warned = sh(
  "x3",
  "dispatch read --issue LWEVAL-7 2>&1 | head -40",
  0,
  "dispatch: warning: Dispatch issue is missing resolved_repo: this Dispatch server predates repository fields, so LWEVAL-7's repository is unknown; upgrade the server\nTitle: Greeting\ndispatch: Dispatch took the call, but this session's state could not be written: EACCES"
);
// A piped refusal fixed from its text alone; then a warned call and a skill read, which refuse
// nothing.
specRun("spec-writing-head-23", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    piped,
    askStep,
    warned,
    readOf("r3", "skill://dispatch"),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A piped refusal fixed only after a read of a skill file.
specRun("spec-writing-head-24", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    piped,
    readOf("r3", "skill://dispatch"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A piped refusal whose next dispatch call is a piped usage refusal: that call fixes nothing, so
// both are fixed only after the skill read.
specRun("spec-writing-head-25", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    piped,
    pipedOff,
    readOf("r3", "skill://dispatch"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// One fault.
specRun("spec-writing-head-7", {
  judges: { "faults.json": faultsFile({ "A term that nobody explained": 1 }) },
});
// Read the skill this change removes.
specRun("spec-writing-head-8", {
  transcript: conversation(
    routedRead,
    readOf("r3", "skill://dispatch-brainstorming"),
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
const eightProblem = "The CLI must print a friendly greeting whenever somebody runs it.";
const eightWords = "print a friendly greeting whenever somebody runs it";
// Shares 8 words with messages.md: the judge marks the run `copies`, then `names`.
specRun("spec-writing-head-9", {
  spec: outlined({ problem: eightProblem }),
  judges: copied("spec.md", eightWords, "copies"),
});
specRun("spec-writing-head-10", {
  spec: outlined({ problem: eightProblem }),
  judges: copied("spec.md", eightWords, "names"),
});
// Shares 7: no run to judge.
specRun("spec-writing-head-11", {
  spec: outlined({ problem: "The CLI must print a friendly greeting whenever somebody runs." }),
});
// Keeps a comment's anchored quote, which quotes the old spec, not the person.
specRun("spec-writing-head-12", { spec: outlined({ problem: quotedLine }) });
// Copies the `>` lines that start a message: the person's own words, as a block quote.
specRun("spec-writing-head-19", {
  spec: outlined({ problem: voiceLine }),
  judges: copied("spec.md", folded(voiceLine), "copies"),
});
// Copies a `>` line after a comment's anchored quote and its body: the person's words too.
specRun("spec-writing-head-20", {
  spec: outlined({ problem: commentQuote }),
  judges: copied("spec.md", folded(commentQuote), "copies"),
});
// Copies the `>` line an unanchored comment starts with: a person's paste, their own input.
specRun("spec-writing-head-26", {
  spec: outlined({ problem: pastedLine }),
  judges: copied("spec.md", folded(pastedLine), "copies"),
});
// One bash call that runs two `dispatch` commands is two invocations, scored in order. Two spec
// writes in one call with no check between them: the second is unchecked.
specRun("spec-writing-head-27", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    sh(
      "w1",
      "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file first.json; dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file second.json",
      0,
      "Applied 1 ops (version 2)\nApplied 1 ops (version 3)"
    ),
    finish
  ),
});
// A refusal followed by a command that succeeded in the same call: the agent wrote that command
// before it saw the refusal, so it is no fix. The fix is the later call after a skill read.
specRun("spec-writing-head-28", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    sh(
      "x5",
      "dispatch ask --issue LWEVAL-7; dispatch read --issue LWEVAL-7",
      0,
      "dispatch ask was not called: 1 problem\n- --question is required\nTitle: Greeting",
      [{ tool: "dispatch_read" }]
    ),
    readOf("r3", "skill://dispatch"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A refusal fixed in a later call that reads a skill file first: a `cat` of the run's own copy of
// skills/dispatch/SKILL.md, then the dispatch command.
const ownSkill = path.join(runs, "spec-writing-head-29", "home", "skills", "dispatch", "SKILL.md");
specRun("spec-writing-head-29", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    sh(
      "x6",
      "dispatch ask --issue LWEVAL-7 --question 'Which name?'",
      1,
      "dispatch ask was not called: 1 problem\n- --question gives no recommendation"
    ),
    sh("x7", `cat ${ownSkill}; dispatch read --issue LWEVAL-7`, 0, "# Dispatch\nTitle: Greeting"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// Refusal lines that are no refusal of the call: a document that quotes the CLI's refusals, and a
// log that a `cat` after a dispatch command prints. A later skill read fixes nothing.
specRun("spec-writing-head-30", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    sh(
      "x8",
      "dispatch read --issue LWEVAL-7",
      0,
      "Issue description:\n```text\ndispatch: unexpected argument old-flag\ndispatch read was not called: 1 problem\n```",
      [{ tool: "dispatch_read" }]
    ),
    sh(
      "x9",
      "dispatch read --issue LWEVAL-7; cat saved.log",
      0,
      "Title: Greeting\ndispatch: unexpected argument old-flag",
      [{ tool: "dispatch_read" }]
    ),
    readOf("r3", "skill://dispatch"),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A refusal after the label an `echo` prints is the dispatch command's own output, fixed only
// after a skill read.
specRun("spec-writing-head-31", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    sh(
      "x10",
      'cd /rig/cwd\necho "=== ask ==="; dispatch ask --issue LWEVAL-7 2>&1 | head -5',
      0,
      "=== ask ===\ndispatch ask was not called: 1 problem\n- --question is required"
    ),
    readOf("r3", "skill://dispatch"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// checked: a check counts for a spec write only when its task call and its completed report both
// came after the write before it and before this write, in transcript order. Each run below
// changes the first check of `followed` (c1, before write w1).
/** A spec-only check's `task` call, with no result yet. */
const checkCall = (id: string) =>
  toolCall(id, "task", {
    context: `Read this spec alone and list each gap.\n\n${outlined()}`,
    tasks: [{ name: `Check${id}`, task: "List each gap." }],
  });
/** A tool's result as Oh My Pi records one: whether the tool failed, and the tool's `details`. */
const toolResult = (toolCallId: string, text: string, isError = false, details: object = {}) => ({
  type: "message",
  message: { role: "toolResult", toolCallId, isError, details, content: [{ type: "text", text }] },
});
/** Oh My Pi's `<task-result>` envelope of task `name`, as task-summary.md writes it. */
const taskEnvelope = (name: string, status: string, report: string) =>
  `<task-result id="${name}" agent="task" status="${status}" duration="40s">\n<output>\n${report}\n</output>\n</task-result>`;
/** A background check's spawn result: the task runs on, and its report arrives later. */
const spawned = (id: string) =>
  toolResult(
    id,
    `Spawned agent \`Check${id}\` (job \`Check${id}\`).\n\nResults auto-deliver; NEVER poll.`
  );
/** A background task's report, delivered by Oh My Pi with the job's own structured status. */
const asyncReport = (name: string, status = "completed", report = "No gaps.") => ({
  type: "custom_message",
  customType: "async-result",
  details: {
    meta: { source: { type: "report", value: "background job delivery" } },
    jobs: [{ jobId: name, type: "task", status, agentId: name, label: name }],
  },
  content: `<system-notice>\nBackground job ${name} has completed.\n${taskEnvelope(name, status, report)}\n</system-notice>`,
});
/** A `wait` call whose structured job snapshot delivers a background task's report. */
const waited = (id: string, name: string, status = "completed", report = "No gaps.") => {
  const resultText = taskEnvelope(name, status, report);
  return [
    toolCall(id, "wait", {}),
    {
      type: "message",
      message: {
        role: "toolResult",
        toolName: "wait",
        toolCallId: id,
        isError: false,
        details: {
          op: "wait",
          jobs: [{ id: name, type: "task", status, label: name, resultText }],
        },
        content: [
          {
            type: "text",
            text: `## Completed (1)\n\n### ${name} [task] — ${status}\n\`\`\`\n${resultText}\n\`\`\``,
          },
        ],
      },
    },
  ];
};
/** `followed`, with its first check replaced by `check`. */
const firstCheck = (...check: object[][]) =>
  conversation(
    routedRead,
    ...check,
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  );
// The task call failed: no checker ran.
specRun("spec-writing-head-32", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", "No checker ran: the scout agent is unavailable.", true),
  ]),
});
// A background check whose report arrived after the write: no check for that write, nor for the
// next one, which has no check of its own.
specRun("spec-writing-head-33", {
  transcript: conversation(
    routedRead,
    [checkCall("c1"), spawned("c1")],
    specEdit("w1"),
    [asyncReport("Checkc1")],
    askStep,
    specEdit("w2"),
    finish
  ),
  judges: { "findings.json": { rows: [] } },
});
// A task that failed, and one that was cancelled.
specRun("spec-writing-head-34", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", taskEnvelope("Checkc1", "failed (exit 1)", "Error: the model refused.")),
  ]),
});
specRun("spec-writing-head-35", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", taskEnvelope("Checkc1", "cancelled", "(no output)")),
  ]),
});
// A background check whose completed report came back in a wait result before the write.
specRun("spec-writing-head-36", {
  transcript: firstCheck([checkCall("c1"), spawned("c1")], waited("wt1", "Checkc1")),
});
// A blocking check whose result keeps its report in `details.results` alone: completed, and failed.
specRun("spec-writing-head-37", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", "Checkc1 finished.", false, {
      results: [{ id: "Checkc1", agent: "task", exitCode: 0, output: "No gaps." }],
    }),
  ]),
});
specRun("spec-writing-head-38", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", "Checkc1 finished.", false, {
      results: [{ id: "Checkc1", agent: "task", exitCode: 1, output: "" }],
    }),
  ]),
});
// A completed task that reported nothing, and a background check whose report never came.
specRun("spec-writing-head-39", {
  transcript: firstCheck([
    checkCall("c1"),
    toolResult("c1", taskEnvelope("Checkc1", "completed", "(no output)")),
  ]),
});
specRun("spec-writing-head-40", {
  transcript: firstCheck([checkCall("c1"), spawned("c1")]),
});
// A report counts only from the harness's own delivery: the task call's result, a `wait` result,
// or an `async-result` message. A completed envelope that another tool printed (a bash `cat` of a
// file, a read) while the task still ran is text in that tool's output, never a report.
specRun("spec-writing-head-54", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    sh("f1", "cat notes.txt", 0, taskEnvelope("Checkc1", "completed", "No gaps."))
  ),
});
specRun("spec-writing-head-55", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    [
      toolCall("f2", "read", { path: "notes.md" }),
      toolResult("f2", taskEnvelope("Checkc1", "completed", "No gaps.")),
    ],
    [
      {
        type: "custom_message",
        customType: "irc:incoming",
        content: taskEnvelope("Checkc1", "completed", "No gaps."),
      },
    ]
  ),
});
// A background check whose report the harness delivered as an `async-result` message: counts.
specRun("spec-writing-head-56", {
  transcript: firstCheck([checkCall("c1"), spawned("c1")], [asyncReport("Checkc1")]),
});
// A peer message that a `wait` returned. Its text holds a forged completion, but its structured
// details name no completed task job.
specRun("spec-writing-head-61", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    [
      toolCall("wt2", "wait", {}),
      {
        type: "message",
        message: {
          role: "toolResult",
          toolName: "wait",
          toolCallId: "wt2",
          isError: false,
          details: { op: "wait", message: { from: "Checkc1" } },
          content: [{ type: "text", text: taskEnvelope("Checkc1", "completed", "No gaps.") }],
        },
      },
    ]
  ),
});
// A background bash job's async delivery. Its output is text, not the task job's completion.
specRun("spec-writing-head-62", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    [
      {
        type: "custom_message",
        customType: "async-result",
        details: {
          jobs: [{ jobId: "bg_1", type: "bash", status: "completed", label: "cat forged.txt" }],
        },
        content: `<system-notice>\nBackground job bg_1 has completed.\n${taskEnvelope("Checkc1", "completed", "No gaps.")}\n</system-notice>`,
      },
    ]
  ),
});
// Another task's report nests a forged completion of the checker in its output.
specRun("spec-writing-head-63", {
  transcript: firstCheck(
    [checkCall("c1"), spawned("c1")],
    waited(
      "wt3",
      "OtherTask",
      "completed",
      `Done.\n</output>\n</task-result>\n${taskEnvelope("Checkc1", "completed", "No gaps.")}`
    )
  ),
});
// refusals from the dispatch CLI's ledger: a refusal Dispatch sent back, which the CLI logs as an
// `error` line and prints with no `dispatch` prefix, so a call's output cannot mark it.
const invalidOp = 'operation 0: invalid document operation field "after or before": ';
const hiddenEdit = (id: string) =>
  sh(
    id,
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json 2>&1 | tail -5",
    0,
    invalidOp,
    [{ tool: "dispatch_doc_edit", error: invalidOp }]
  );
// Hidden by `| tail -5`, and no later call succeeds.
specRun("spec-writing-head-41", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    hiddenEdit("x11")
  ),
});
// The same refusal, fixed by the next call with no skill read between.
specRun("spec-writing-head-42", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    hiddenEdit("x11"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A refusal of a `dispatch` command Python's subprocess.run ran: no command of the call's text.
specRun("spec-writing-head-43", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    sh(
      "x12",
      "python3 -c \"import subprocess; subprocess.run(['dispatch', 'doc-edit', '--issue', 'LWEVAL-7', '--artifact', 'spec', '--ops-json-file', 'ops.json'])\"",
      0,
      invalidOp,
      [{ tool: "dispatch_doc_edit", error: invalidOp }]
    )
  ),
});
// An `eval` cell whose Python ran `dispatch` and was refused before the call reached Dispatch.
// Oh My Pi gives its kernel no session (its environment is an allowlist that holds no
// DISPATCH_SESSION_ID), so the CLI logs nothing, and the cell's code is no shell text: the
// refusal is in the cell's output alone.
const noHost =
  "dispatch: DISPATCH_HOST is not set. The host plugin sets it in an agent's shell; to run dispatch by hand, set DISPATCH_HOST and DISPATCH_SESSION_ID yourself.\n";
const pythonCall =
  "import subprocess\nr = subprocess.run(['dispatch', 'doc-edit', '--issue', 'LWEVAL-7', '--artifact', 'spec', '--ops-json-file', 'ops.json'], capture_output=True, text=True)\nprint(r.stdout)";
specRun("spec-writing-head-57", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    evalCell("x14", pythonCall, noHost)
  ),
});
// The same refusal, fixed by a later `dispatch` call with no skill read between.
specRun("spec-writing-head-58", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    evalCell("x14", pythonCall, noHost),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A usage refusal the CLI both prints and logs (an unknown command, under tool `dispatch`), after a
// skill read earlier in the same call. The ledger line and the printed refusal are one refusal:
// counted once, at the command, so the read before it is no read between it and its fix.
const unknownCommand =
  "dispatch: unknown command frobnicate; the commands are: issue, issue-update, claim, ask, edit-ask, resolve-ask, resolve-comment, follow, comment, suggest, message, doc-edit, doc-read, request-approval, artifact, read, search, issues, project, projects, architecture-sync, open-asks, whoami\nRun dispatch --help.";
const usageRefused = (name: string, flags = "") =>
  sh(
    "x15",
    `cat ${path.join(runs, name, "home", "skills", "dispatch", "SKILL.md")}; dispatch frobnicate${flags}`,
    2,
    `# Dispatch\n${unknownCommand}`,
    [{ tool: "dispatch", error: unknownCommand }]
  );
// Fixed by the next spec write, with no skill read between.
specRun("spec-writing-head-59", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    usageRefused("spec-writing-head-59"),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// Never fixed: one refusal block, not two.
specRun("spec-writing-head-60", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    usageRefused("spec-writing-head-60")
  ),
});
// The same unknown command with `--help`: the CLI checks the command before the flag, so it
// refuses and logs it as above. A refused help call is one refusal too, at its command.
specRun("spec-writing-head-64", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    usageRefused("spec-writing-head-64", " --help"),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// A refusal fixed by a later call that reads a skill file after its dispatch command: that read
// comes after the fix.
const laterSkill = path.join(
  runs,
  "spec-writing-head-44",
  "home",
  "skills",
  "dispatch",
  "SKILL.md"
);
specRun("spec-writing-head-44", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    refused,
    sh(
      "x13",
      `dispatch ask --issue LWEVAL-7 --question 'Which name? Recommendation: Ada.'; cat ${laterSkill}`,
      0,
      "Asked ask-1.\n# Dispatch"
    ),
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// No ledger: the CLI logged nothing where the rig reads it.
specRun("spec-writing-head-45", { ledger: false });
// Two calls of one message: a ledger line inside both calls' time is the call whose result came
// first. The hidden refusal is the doc-edit's, in the first call.
const parallel = [
  {
    type: "message",
    message: {
      role: "assistant",
      content: [
        {
          type: "toolCall",
          id: "x14",
          name: "bash",
          arguments: {
            command:
              "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json 2>&1 | tail -5",
          },
        },
        {
          type: "toolCall",
          id: "x15",
          name: "bash",
          arguments: { command: "dispatch read --issue LWEVAL-7" },
        },
      ],
    },
  },
  { ...(hiddenEdit("x14")[1] ?? {}) },
  { ...(sh("x15", "dispatch read --issue LWEVAL-7", 0, "Title: Greeting")[1] ?? {}) },
];
specRun("spec-writing-head-46", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish,
    parallel
  ),
});
// Judge coverage: each judge's answer covers exactly what judge.ts gave its judge. Each run below
// breaks one judge's file.
const question = (id: string) => ({
  id,
  dominated: false,
  settled: false,
  kind: "taste",
  reason: "A name is taste.",
});
// The approved spec holds R1 and R2; the judge checked R1 alone (the cross-model review's case).
specRun("spec-writing-head-47", {
  spec: outlined().replace(
    "From: your answer on the greeting.",
    "From: your answer on the greeting.\n- **R2.** The greeting ends with a full stop. From: your message on the format."
  ),
});
// questions: ask-1 twice, and an ask it was not given.
specRun("spec-writing-head-48", {
  judges: { "questions.json": { rows: [question("ask-1"), question("ask-1"), question("ask-9")] } },
});
// numbers: no row for the one text it was given, and a row for a seeded ask it was not given.
specRun("spec-writing-head-49", {
  seeded: ["seeded-1"],
  judges: { "numbers.json": { rows: [{ id: "seeded-1", names_number: true, quote: "R1" }] } },
});
// proposal: an ask it was not given.
specRun("spec-writing-head-50", {
  judges: {
    "proposal.json": { rows: [{ item: "the greeting word", verdict: "asked", ask: "ask-9" }] },
  },
});
// preamble: a document and a text it was not given.
specRun("spec-writing-head-51", {
  judges: {
    "preamble.json": {
      rows: [
        { source: "documents/notes.md", passage: "You said so.", kind: "retold" },
        { source: "agent-texts.json#msg-9", passage: "On Monday you said.", kind: "dated" },
      ],
    },
  },
});
// faults: a count that its items do not hold.
specRun("spec-writing-head-52", {
  judges: {
    "faults.json": { ...faultsFile(), faults: { ...noFaults, "A term that nobody explained": 1 } },
  },
});
// findings: a check it was not given (the run's two checks are 1 and 2).
specRun("spec-writing-head-53", {
  judges: { "findings.json": { rows: [{ check: 3, finding: "No owner.", verdict: "fixed" }] } },
});
// Spec writes: only a write Dispatch made, of the run's own primary spec, counts. Each call below is
// something else, and the run's two checked writes still stand. Each result is the one Dispatch
// gives, so the calls that Dispatch did apply are left out for what they name, not for their result.
const applied = "Applied 1 ops (version 2)\nDocument token: sha256:abc";
const notWrites = [
  // The help of doc-edit, piped as the agent did.
  sh("n1", "dispatch doc-edit --help 2>&1 | head -40", 0, "Usage: dispatch doc-edit [flags]"),
  // An edit of another issue's spec, and another issue made with a spec.
  sh("n2", "dispatch doc-edit --issue WS-1 --artifact spec --ops-json '[]'", 0, applied),
  sh(
    "n3",
    "dispatch issue --project WS --title 'Orchestration' --spec-file - <<'EOF'\nIt tracks one stream.\nEOF",
    0,
    "Created WS-1: Orchestration → http://127.0.0.1:1/issues/WS-1; unassigned"
  ),
  // An edit of the run's issue's other document.
  sh("n4", "dispatch doc-edit --issue LWEVAL-7 --artifact plan.md --ops-json '[]'", 0, applied),
  // A failed edit of the run's spec: the tool refused it.
  sh(
    "n5",
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json",
    1,
    "refused"
  ),
  // An edit the CLI refused before calling Dispatch: bash exits 0 through `2>&1`, and the result
  // says `was not called`.
  sh(
    "n6",
    'dispatch doc-edit --issue LWEVAL-7 --artifact spec --precondition-json \'{"document":"sha\' 2>&1',
    0,
    "dispatch doc-edit was not called: 1 problem\n- --precondition-json is not JSON"
  ),
  // A dry run, and an edit that changed nothing and so made no version.
  sh("n7", "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json --dry-run"),
  sh(
    "n8",
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json",
    0,
    "Applied 1 ops; nothing changed (no new version)"
  ),
  // An edit by a ref to another issue's spec.
  sh("n9", "dispatch doc-edit --ref dispatch://WS-1/spec --ops-json '[]'", 0, applied),
  // The help and a dry run of the run's spec, each in one call with an edit Dispatch applied to
  // another issue: that edit's answer is no write of the help or the dry run.
  sh(
    "n10",
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --help | head -5; dispatch doc-edit --issue WS-1 --artifact spec --ops-json '[]'",
    0,
    applied
  ),
  sh(
    "n11",
    "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json --dry-run && dispatch doc-edit --issue WS-2 --artifact spec --ops-json '[]'",
    0,
    applied
  ),
  // Uploads that are no spec write: the run's plan, another issue's spec.md, and the run's spec.md
  // that Dispatch refused.
  sh(
    "n12",
    "dispatch artifact --issue LWEVAL-7 --name plan.md --content-file plan.md",
    0,
    "Uploaded plan.md as version 1 (artifact slug plan-md; dispatch://LWEVAL-7/artifact/plan-md)"
  ),
  sh(
    "n13",
    "dispatch artifact --issue WS-1 --name spec.md --content-file spec.md",
    0,
    "Uploaded spec.md as version 2 (artifact slug spec-md; dispatch://WS-1/artifact/spec-md)"
  ),
  sh(
    "n14",
    "dispatch artifact --issue LWEVAL-7 --name spec.md --content-file spec.md 2>&1",
    0,
    "dispatch artifact was not called: 1 problem\n- --content-file spec.md does not exist"
  ),
];
specRun("spec-writing-head-21", {
  transcript: conversation(
    routedRead,
    checkOf("c1"),
    specEdit("w1"),
    askStep,
    ...notWrites,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// The spec writes of the run's issue that do count: `dispatch issue` that Dispatch answered with the
// run's issue made, the spec by its artifact id, by a dispatch:// ref, as an uploaded spec.md, by a
// dashboard URL, in a shell whose last command failed after Dispatch made the version, and with no
// document named, by `--issue` alone and by a bare issue ref. Each has its check before it.
specRun("spec-writing-head-22", {
  transcript: conversation(
    routedRead,
    checkOf("c0"),
    sh(
      "w0",
      "dispatch issue --project LWEVAL --title 'Greeting' --spec-file spec.md",
      0,
      "Created LWEVAL-7: Greeting → http://127.0.0.1:1/issues/LWEVAL-7; unassigned"
    ),
    checkOf("c1"),
    sh(
      "w1",
      "dispatch doc-edit --issue LWEVAL-7 --artifact art-spec --ops-json-file ops.json",
      0,
      applied
    ),
    checkOf("c2"),
    sh(
      "w2",
      "dispatch doc-edit --ref dispatch://LWEVAL-7/artifact/spec --ops-json-file ops.json",
      0,
      "Applied 2 ops (version 3)"
    ),
    checkOf("c3"),
    sh(
      "w3",
      "dispatch artifact --issue LWEVAL-7 --name spec.md --content-file spec.md",
      0,
      "Uploaded spec.md as version 4 (artifact slug spec-md; dispatch://LWEVAL-7/artifact/spec-md)"
    ),
    checkOf("c4"),
    sh(
      "w4",
      "dispatch doc-edit --ref http://127.0.0.1:1/issues/LWEVAL-7/spec --ops-json-file ops.json",
      0,
      "Applied 1 ops (version 5)"
    ),
    checkOf("c5"),
    sh(
      "w5",
      "dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json 2>&1; dispatch doc-read --issue LWEVAL-7 --artifact spec | grep -c TODO",
      1,
      "Applied 1 ops (version 6)\nDocument token: sha256:abc\n0"
    ),
    // No document named: the CLI edits the issue's primary document, the spec.
    checkOf("c6"),
    sh(
      "w6",
      "dispatch doc-edit --issue LWEVAL-7 --ops-json-file ops.json",
      0,
      "Applied 1 ops (version 7)"
    ),
    checkOf("c7"),
    sh(
      "w7",
      "dispatch doc-edit --ref dispatch://LWEVAL-7 --ops-json-file ops.json",
      0,
      "Applied 1 ops (version 8)"
    ),
    finish
  ),
});
// Copies a 30-word passage: one run.
const thirtyWords = folded(longMessage).split(" ").slice(0, 30).join(" ");
specRun("spec-writing-head-13", {
  spec: outlined({
    design:
      "The greeting must stay on one line, use the name exactly as the person typed it, end with a full stop, and never print the name twice even when the CLI starts.",
  }),
  judges: copied("spec.md", thirtyWords, "names"),
});
// A document that copies the person's message.
specRun("spec-writing-head-14", {
  documents: { "brief.md": `# Brief\n\n${askedFor}\n` },
  judges: copied("documents/brief.md", folded(askedFor), "copies"),
});
// Names the run's session.
specRun("spec-writing-head-15", {
  spec: outlined({ summary: ` Session ${specSession.slice(0, 8)} wrote it.` }),
});
// A seeded ask: agent-asks.json and agent-texts.json hold it, and state-1.json lists it as the
// only open ask. judge.ts gave no judge the seeded ask, so the judges' files cover the rest alone.
specRun("spec-writing-head-16", {
  seeded: ["seeded-1"],
  states: [
    { turn: 1, open_asks: [openAsk("seeded-1")], approval: null },
    ...approvedStates.slice(1),
  ],
  asks: [
    { kind: "question", ...openAsk("seeded-1"), state: "answered" },
    { kind: "question", ...openAsk("ask-1"), state: "answered" },
    { id: "appr-1", kind: "approval", question: "Approve spec.md v3?", options: [] },
  ],
  texts: [
    { id: "seeded-1", kind: "ask", body: "Which name should the example use?", created_at: "" },
    { id: "msg-1", kind: "message", body: "The spec is ready to read.", created_at: "" },
  ],
});
// A check whose assignment names a path under the label's checkout.
specRun("spec-writing-head-17", {
  transcript: conversation(
    routedRead,
    checkOf("c1", `${outlined()}\nCompare /checkouts/head/packages/cli/greet.ts.`),
    specEdit("w1"),
    askStep,
    checkOf("c2"),
    specEdit("w2"),
    finish
  ),
});
// Ended with no approval.
specRun("spec-writing-head-18", {
  states: [approvedStates[0] ?? {}, { turn: 2, open_asks: [], approval: null }],
  turns: "1 first exit=0\n2 answered exit=0\n",
  judges: { "checkable.json": { version: null, rows: [] } },
});
// Three runs of another label, for the median fault count.
for (const [n, faults] of [2, 4, 3].entries())
  specRun(`spec-writing-base-${n + 1}`, {
    judges: { "faults.json": faultsFile({ "The reader cannot follow a sentence": faults }) },
  });

const scored = Bun.spawnSync(["bun", path.join(import.meta.dir, "score.ts"), "runs", runs]);
const out = scored.stdout.toString();

test("a run whose agent never got a model turn is a rig error, not a failure", () => {
  expect(scored.exitCode).toBe(0);
  for (const name of ["tester-proof-head-1", "ask-on-message-head-1"]) {
    expect(out).toContain(`${name}\trig error: `);
    expect(out).not.toContain(`${name}\tpass=`);
  }
  // Pass counts over the one run that got a turn; the other is counted only as a rig error.
  expect(out).toContain("tester-proof\thead\t0/1\t0/1\t1\n");
  expect(out).toContain("ask-on-message\thead\t0/1\t0/1\t1\n");
});

test("a run whose agent got a turn and did nothing is a failure", () => {
  for (const name of ["tester-proof-head-2", "ask-on-message-head-2"]) {
    expect(out).toContain(`${name}\tpass=false\t`);
  }
});

test("the tester ran the CLI when the bun stand-in recorded a run of greet.ts, not when a command named it", () => {
  expect(out).toMatch(/tester-proof-ran-1\tpass=false\tref=false\tran=true /);
  expect(out).toMatch(/tester-proof-ran-2\tpass=false\tref=false\tran=false /);
});

test("a tool call naming the other label's checkout is a rig error wherever the name falls in it", () => {
  expect(out).toContain("tester-proof-cross-1\trig error: a bash call names base's checkout");
});

test("a measure-before-ask ask passes when it carries the population and the measurement and drops the option they rule out", () => {
  expect(out).toMatch(
    /measure-before-ask-head-1\tpass=true\t.*population=true measured=true dropped=true /
  );
  expect(out).toMatch(/measure-before-ask-head-2\tpass=false\t.*dropped=false /);
  expect(out).toMatch(
    /measure-before-ask-head-3\tpass=false\t.*population=false measured=false dropped=true /
  );
  expect(out).toContain("measure-before-ask\thead\t1/3\t");
});

test("a brainstorm run leaks when a tool's result carries the token, not only its arguments", () => {
  expect(out).toMatch(/brainstorm-head-1\tpass=true\t.*surface=spec .*leak=false /);
  expect(out).toMatch(/brainstorm-head-2\tpass=false\t.*surface=spec .*leak=true /);
});

test("a brainstorm run created its issue when Dispatch answered Created, whatever wrapped the call", () => {
  // From inside an `eval` cell, with no top-level `dispatch issue` command.
  expect(out).toMatch(/brainstorm-head-3\tpass=true\t.*surface=spec .*leak=false /);
  // Ran `dispatch issue`, but Dispatch refused it as a duplicate.
  expect(out).toMatch(/brainstorm-head-4\tpass=false\t.*surface=neither /);
});

// live-read: the pinned Oh My Pi answers a `read` of skill://<name>[/<path>] with one header line,
// `[Skill file: <absolute path>]`, then the file's text. The header is not the file's.
const liveRun = path.join(work, "live-read");
const liveSkills = path.join(work, "live-skills");
const liveLogs = path.join(work, "live-logs");
mkdirSync(path.join(liveRun, "sessions"), { recursive: true });
mkdirSync(path.join(liveSkills, "demo", "references"), { recursive: true });
mkdirSync(liveLogs, { recursive: true });
const skillText = "---\nname: demo\n---\n\n# Demo\n\nRead the notes.\n";
const notesText = "# Notes\n\nOne line of notes.\n";
writeFileSync(path.join(liveSkills, "demo", "SKILL.md"), skillText);
writeFileSync(path.join(liveSkills, "demo", "references", "notes.md"), notesText);
const skillHeader = "[Skill file: /profiles/s1/agent/skills/demo";
/** A `read` tool's result, answering the call `toolCallId` names. */
const readResult = (toolCallId: string, text: string) => ({
  type: "message",
  message: { role: "toolResult", toolName: "read", toolCallId, content: [{ type: "text", text }] },
});
const skillWhole = skillText.replace(/\n$/, "");
const notesWhole = notesText.replace(/\n$/, "");
const otherHeader = `[Read: references/notes.md]\n${notesWhole}`;
writeFileSync(
  path.join(liveRun, "sessions", "2026-10-24T10-00-00-000Z_01a0f2cc.jsonl"),
  [
    ...header,
    // The header, then the whole file: whole.
    toolCall("read-1", "read", { path: "skill://demo" }),
    readResult("read-1", `${skillHeader}/SKILL.md]\n${skillWhole}`),
    // The header, then the file cut short: not whole.
    toolCall("read-2", "read", { path: "skill://demo/references/notes.md" }),
    readResult("read-2", `${skillHeader}/references/notes.md]\n${notesWhole.slice(0, -5)}`),
    // No header, the whole file: whole, as before.
    toolCall("read-3", "read", { path: "skill://demo" }),
    readResult("read-3", skillWhole),
    // Another header, then the whole file: that header counts, so not whole.
    toolCall("read-4", "read", { path: "skill://demo/references/notes.md" }),
    readResult("read-4", otherHeader),
  ]
    .map((entry) => `${JSON.stringify(entry)}\n`)
    .join("")
);

test("live-read compares a skill read's text after Oh My Pi's `[Skill file: …]` header line", () => {
  const read = Bun.spawnSync([
    "bun",
    path.join(import.meta.dir, "score.ts"),
    "live-read",
    liveRun,
    liveSkills,
    liveLogs,
  ]);
  const text = read.stdout.toString();
  expect(read.exitCode).toBe(0);
  const skillChars = [...skillWhole].length;
  const notesChars = [...notesWhole].length;
  expect(text.split("\n").slice(1, 5)).toEqual([
    `skill://demo\t${skillChars}\t${skillChars}\tfalse\ttrue`,
    `skill://demo/references/notes.md\t${notesChars - 5}\t${notesChars}\tfalse\tfalse`,
    `skill://demo\t${skillChars}\t${skillChars}\tfalse\ttrue`,
    `skill://demo/references/notes.md\t${[...otherHeader].length}\t${notesChars}\tfalse\tfalse`,
  ]);
  expect(text).toContain("2 of 4 reads arrived whole\n");
});

/** A run's row and the lines printed under it. */
function rowOf(name: string): string {
  const all = out.split("\n");
  const start = all.findIndex((line) => line.startsWith(`${name}\t`));
  if (start < 0) return "";
  const end = all.findIndex((line, index) => index > start && !line.startsWith("  "));
  return all.slice(start, end < 0 ? undefined : end).join("\n");
}
const SCORES = ["outline", "checked", "refusals", "plan", "routed", "chat", "quoted", "self"];
/** The scores of a spec run's row, by name. */
function scoresOf(name: string): Record<string, string> {
  const row = rowOf(name);
  return Object.fromEntries(
    SCORES.map((score) => [score, new RegExp(` ${score}=(\\w+)`).exec(row)?.[1] ?? ""])
  );
}
const allPass = Object.fromEntries(SCORES.map((score) => [score, "true"]));

test("a spec-writing run that follows every rule passes every score", () => {
  expect(rowOf("spec-writing-head-1")).toMatch(
    /^spec-writing-head-1\tpass=true\tref=true\tsource=acme-1 turns=3 approved=true /
  );
  expect(scoresOf("spec-writing-head-1")).toEqual(allPass);
  expect(rowOf("spec-writing-head-1")).toContain(" shared=0 copies=0");
});

test("a spec-rewrite run is scored by the same rules", () => {
  expect(scoresOf("spec-rewrite-head-1")).toEqual(allPass);
  expect(out).toContain("spec-rewrite\thead\t1/1\t1/1\t0\n");
});

test("outline: the spec's ## headings are exactly the outline's, in order", () => {
  expect(scoresOf("spec-writing-head-2")).toEqual({ ...allPass, outline: "false" });
  expect(rowOf("spec-writing-head-2")).toContain("## Scope");
});

test("checked: a spec-only check before every spec write, the first included, and no finding left", () => {
  expect(scoresOf("spec-writing-head-3")).toEqual({ ...allPass, checked: "false" });
  expect(rowOf("spec-writing-head-3")).toContain("unchecked writes: 1 of 2");
  expect(scoresOf("spec-writing-head-4")).toEqual({ ...allPass, checked: "false" });
  expect(rowOf("spec-writing-head-4")).toContain(" neither=1 ");
  // A check that names a path under the checkout is no spec-only check.
  expect(scoresOf("spec-writing-head-17")).toEqual({ ...allPass, checked: "false" });
});

test("refusals: a refused dispatch call must be fixed with no skill read before the fix", () => {
  expect(scoresOf("spec-writing-head-5")).toEqual(allPass);
  expect(scoresOf("spec-writing-head-6")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-6")).toContain(refusal);
  expect(rowOf("spec-writing-head-6")).toContain("skill://dispatch");
});

test("refusals: the CLI's own refusal text counts when a pipe hid the exit", () => {
  // Fixed from the refusal's text alone; a warned call refuses nothing.
  expect(scoresOf("spec-writing-head-23")).toEqual(allPass);
  // Fixed only after a skill read: the row prints the refusal's text and the read.
  expect(scoresOf("spec-writing-head-24")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-24")).toContain("dispatch doc-edit was not called: 1 problem");
  expect(rowOf("spec-writing-head-24")).toContain("then read skill://dispatch");
  // A piped usage refusal is no fix of the refusal before it, and is a refusal of its own.
  expect(scoresOf("spec-writing-head-25")).toEqual({ ...allPass, refusals: "false" });
  const row = rowOf("spec-writing-head-25");
  expect(row).toContain("dispatch doc-edit was not called: 1 problem");
  expect(row).toContain(offRefusal);
  expect(row.split("then read skill://dispatch").length - 1).toBe(2);
});

test("judged: a judge file with one fault counts one", () => {
  expect(rowOf("spec-writing-head-7")).toContain(" faults=1 ");
  expect(rowOf("spec-writing-head-1")).toContain(" faults=0 ");
});

test("routed: the writing skill before the first spec write, and never the removed skill", () => {
  expect(scoresOf("spec-writing-head-8")).toEqual({ ...allPass, routed: "false" });
});

test("quoted: a shared run of 8 words fails only when the judge marks it copies", () => {
  expect(scoresOf("spec-writing-head-9")).toEqual({ ...allPass, quoted: "false" });
  expect(rowOf("spec-writing-head-9")).toContain(
    `shared run 1 in spec.md (8 words, copies: The verdict.): ${eightWords}`
  );
  expect(scoresOf("spec-writing-head-10")).toEqual(allPass);
  expect(rowOf("spec-writing-head-10")).toContain(" shared=1 copies=0");
});

test("quoted: 7 shared words and a comment's anchored quote are no run", () => {
  expect(scoresOf("spec-writing-head-11")).toEqual(allPass);
  expect(rowOf("spec-writing-head-11")).toContain(" shared=0 ");
  expect(scoresOf("spec-writing-head-12")).toEqual(allPass);
  expect(rowOf("spec-writing-head-12")).toContain(" shared=0 ");
});

test("quoted: the person's own `>` lines are input: in a message, after a comment's quote, or at the start of an unanchored comment", () => {
  expect(scoresOf("spec-writing-head-19")).toEqual({ ...allPass, quoted: "false" });
  expect(rowOf("spec-writing-head-19")).toContain(
    `shared run 1 in spec.md (${folded(voiceLine).split(" ").length} words, copies: The verdict.): ${folded(voiceLine)}`
  );
  expect(scoresOf("spec-writing-head-20")).toEqual({ ...allPass, quoted: "false" });
  expect(rowOf("spec-writing-head-20")).toContain(`: ${folded(commentQuote)}`);
  expect(scoresOf("spec-writing-head-26")).toEqual({ ...allPass, quoted: "false" });
  expect(rowOf("spec-writing-head-26")).toContain(
    `shared run 1 in spec.md (${folded(pastedLine).split(" ").length} words, copies: The verdict.): ${folded(pastedLine)}`
  );
});

test("checked: only an accepted write of the run's own primary spec is a spec write", () => {
  // --help, another issue or document, a failed call, a refused call, a dry run, an edit that
  // changed nothing, and a ref to another issue's spec.
  expect(scoresOf("spec-writing-head-21")).toEqual(allPass);
  expect(rowOf("spec-writing-head-21")).toContain(" writes=2 ");
  // The run's issue made with its spec, its spec by artifact id, by a dispatch:// ref, as an
  // uploaded spec.md, by a dashboard URL, in a shell that failed after the write, and with no
  // document named (`--issue` alone, a bare issue ref), which the CLI reads as the primary spec.
  expect(scoresOf("spec-writing-head-22")).toEqual(allPass);
  expect(rowOf("spec-writing-head-22")).toContain(" writes=8 ");
});

test("checked: two spec writes in one call are two writes, the second unchecked", () => {
  expect(scoresOf("spec-writing-head-27")).toEqual({ ...allPass, checked: "false" });
  const row = rowOf("spec-writing-head-27");
  expect(row).toContain(" writes=2 ");
  expect(row).toContain("unchecked writes: 1 of 2");
  expect(row).toContain(
    "  dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file second.json"
  );
});

test("refusals: the CLI's ledger holds each refusal, the ones a call's output hides included", () => {
  // Hidden by `| tail -5`, and never fixed.
  expect(scoresOf("spec-writing-head-41")).toEqual({ ...allPass, refusals: "false" });
  const hidden = rowOf("spec-writing-head-41");
  expect(hidden).toContain(
    "refused call: dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json"
  );
  expect(hidden).toContain("(dispatch_doc_edit, logged at 2026-10-24T10:");
  expect(hidden).toContain(`    ${invalidOp.trim()}`);
  expect(hidden).toContain("no later dispatch call succeeded");
  // The same refusal, fixed by the next call with no skill read between.
  expect(scoresOf("spec-writing-head-42")).toEqual(allPass);
  // A call that ran dispatch from Python's subprocess.run: its text shows no dispatch command.
  expect(scoresOf("spec-writing-head-43")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-43")).toContain("refused call: dispatch_doc_edit, logged at ");
  expect(rowOf("spec-writing-head-43")).toContain(`    ${invalidOp.trim()}`);
  // A skill read after the fixing command, in its own call, is no read before the fix.
  expect(scoresOf("spec-writing-head-44")).toEqual(allPass);
  // Two calls of one message: the refusal is the first call's, whose result came first, and the
  // other call was written before the refusal came back, so it fixes nothing.
  expect(scoresOf("spec-writing-head-46")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-46")).toContain(
    "refused call: dispatch doc-edit --issue LWEVAL-7 --artifact spec --ops-json-file ops.json 2> (dispatch_doc_edit, logged at "
  );
  expect(rowOf("spec-writing-head-46")).toContain("no later dispatch call succeeded");
});

test("refusals: a refusal in an eval cell's output counts when the CLI logged nothing for it", () => {
  expect(scoresOf("spec-writing-head-57")).toEqual({ ...allPass, refusals: "false" });
  const row = rowOf("spec-writing-head-57");
  expect(row).toContain("refused call: eval cell x14, which the CLI logged no call of");
  expect(row).toContain(`    ${noHost.trim()}`);
  expect(row).toContain("no later dispatch call succeeded");
  expect(scoresOf("spec-writing-head-58")).toEqual(allPass);
});

// Catches: a usage refusal the CLI prints and logs counted twice, once from the output and once from
// the ledger, where the ledger copy, attributed to the call alone, counts the call's earlier skill
// read as a read between the refusal and its fix.
test("refusals: a usage refusal the CLI prints and logs is one refusal, at its command", () => {
  expect(scoresOf("spec-writing-head-59")).toEqual(allPass);
  expect(rowOf("spec-writing-head-59")).not.toContain("refused call:");
  // The same refusal of an unknown command with `--help`: refused, logged, and matched once.
  expect(scoresOf("spec-writing-head-64")).toEqual(allPass);
  expect(rowOf("spec-writing-head-64")).not.toContain("refused call:");
  expect(scoresOf("spec-writing-head-60")).toEqual({ ...allPass, refusals: "false" });
  const row = rowOf("spec-writing-head-60");
  expect(row.split("refused call:")).toHaveLength(2);
  expect(row).toContain("refused call: dispatch frobnicate (dispatch, logged at ");
  expect(row).toContain("no later dispatch call succeeded");
});

test("refusals: a run whose CLI ledger is missing is a rig error naming the file", () => {
  expect(out).toContain(
    `spec-writing-head-45\trig error: ${path.join(ledgerDir("spec-writing-head-45"), "results.jsonl")} is missing`
  );
});

test("judged: a judge file that leaves out, repeats or invents what judge.ts gave its judge is a rig error", () => {
  const uncovered = (run: string, file: string, ...problems: string[]) => {
    expect(out).toContain(
      `${run}\trig error: ${file} does not cover what judge.ts gave its judge: `
    );
    for (const problem of problems)
      expect(out).toMatch(
        new RegExp(`${run}\\trig error: [^\\n]*${problem.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}`)
      );
    expect(out).toContain(`: run rig.sh judge ${path.join(runs, run)}\n`);
  };
  uncovered("spec-writing-head-47", "checkable.json", "no row for requirement R2");
  uncovered(
    "spec-writing-head-48",
    "questions.json",
    "two or more rows for ask ask-1",
    "rows for ask it was not given: ask-9"
  );
  uncovered(
    "spec-writing-head-49",
    "numbers.json",
    "no row for text msg-1",
    "rows for text it was not given: seeded-1"
  );
  uncovered("spec-writing-head-50", "proposal.json", "rows for ask it was not given: ask-9");
  uncovered(
    "spec-writing-head-51",
    "preamble.json",
    "rows for source it was not given: documents/notes.md, agent-texts.json#msg-9"
  );
  uncovered(
    "spec-writing-head-52",
    "faults.json",
    'faults["A term that nobody explained"] is 1, but 0 items name that row'
  );
  uncovered("spec-writing-head-53", "findings.json", "rows for check it was not given: 3");
});

test("refusals: each dispatch command of a call is scored in order, with the reads between them", () => {
  // The next command of the same call fixes nothing; the later call that succeeds comes after a
  // skill read.
  expect(scoresOf("spec-writing-head-28")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-28")).toContain("refused call: dispatch ask --issue LWEVAL-7");
  expect(rowOf("spec-writing-head-28")).toContain("then read skill://dispatch before");
  // Fixed only after a `cat` of skills/dispatch/SKILL.md in the call that fixed it.
  expect(scoresOf("spec-writing-head-29")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-29")).toContain(`then read ${ownSkill} before`);
  // The refusal after an echo's label is the dispatch command's own output.
  expect(scoresOf("spec-writing-head-31")).toEqual({ ...allPass, refusals: "false" });
  expect(rowOf("spec-writing-head-31")).toContain("refused call: dispatch ask --issue LWEVAL-7");
});

test("refusals: a refusal line a successful command's output quotes, or a cat prints, is no refusal", () => {
  expect(scoresOf("spec-writing-head-30")).toEqual(allPass);
  expect(rowOf("spec-writing-head-30")).not.toContain("refused call:");
});

test("checked: a check counts only when its completed report came back before the write", () => {
  const fails = (run: string, line: string) => {
    expect(scoresOf(run)).toEqual({ ...allPass, checked: "false" });
    expect(rowOf(run)).toContain(
      `check Checkc1 (call c1) for spec write 1 does not count: ${line}`
    );
  };
  fails("spec-writing-head-32", "the task call failed: No checker ran");
  // Late for its write, and no check for the next write either.
  fails("spec-writing-head-33", "the report of task Checkc1 arrived after the spec write");
  expect(rowOf("spec-writing-head-33")).toContain("unchecked writes: 2 of 2");
  fails("spec-writing-head-34", "task Checkc1 ended failed (exit 1)");
  fails("spec-writing-head-35", "task Checkc1 ended cancelled");
  fails("spec-writing-head-38", "task Checkc1 ended failed (exit 1)");
  fails("spec-writing-head-39", "the report of task Checkc1 is empty");
  fails("spec-writing-head-40", "no report of task Checkc1 arrived");
  // A completed envelope another tool printed is no report: a bash `cat`, a read, an IRC message.
  fails("spec-writing-head-54", "no report of task Checkc1 arrived");
  fails("spec-writing-head-55", "no report of task Checkc1 arrived");
  // An allowed delivery route counts only its own structured task job, never text inside it.
  fails("spec-writing-head-61", "no report of task Checkc1 arrived");
  fails("spec-writing-head-62", "no report of task Checkc1 arrived");
  fails("spec-writing-head-63", "no report of task Checkc1 arrived");
});

test("checked: a background check's report in a wait result or an async-result message, and a blocking check's report, count", () => {
  expect(scoresOf("spec-writing-head-36")).toEqual(allPass);
  expect(scoresOf("spec-writing-head-37")).toEqual(allPass);
  expect(scoresOf("spec-writing-head-56")).toEqual(allPass);
  // `followed`'s checks are blocking, each with a completed `<task-result>` block.
  expect(scoresOf("spec-writing-head-1")).toEqual(allPass);
  expect(rowOf("spec-writing-head-1")).not.toContain("does not count");
});

test("quoted: a copied 30-word passage is one run, with its context in both texts", () => {
  const row = rowOf("spec-writing-head-13");
  expect(row).toContain(" shared=1 ");
  expect(row).toContain(`shared run 1 in spec.md (30 words, names: The verdict.): ${thirtyWords}`);
  expect(row).not.toContain("shared run 2");
  expect(row).toContain("input: ");
  expect(row).toContain("never print the name twice even when the person repeats it");
});

test("quoted: a document that copies the person's message fails", () => {
  expect(scoresOf("spec-writing-head-14")).toEqual({ ...allPass, quoted: "false" });
  expect(rowOf("spec-writing-head-14")).toContain("shared run 1 in documents/brief.md (");
});

test("self: the spec names the run's session", () => {
  expect(scoresOf("spec-writing-head-15")).toEqual({ ...allPass, self: "false" });
});

test("a seeded ask counts neither against the run nor for it", () => {
  const row = rowOf("spec-writing-head-16");
  expect(row).toContain(" dominated=0 settled=0 kinds=authority:0,taste:1,risk:0,answerable:0 ");
  expect(row).toContain(" names_number=0 ");
  expect(scoresOf("spec-writing-head-16")).toEqual({ ...allPass, chat: "false" });
  expect(row).toContain("turns with nothing open for the person: 1");
});

test("plan and checkable: a run that ends with no approval fails both", () => {
  const row = rowOf("spec-writing-head-18");
  expect(row).toMatch(/ approved=false /);
  expect(scoresOf("spec-writing-head-18")).toEqual({ ...allPass, plan: "false" });
  expect(row).toContain(" checkable=false ");
  expect(rowOf("spec-writing-head-1")).toContain(" checkable=true uncheckable=0 ");
});

test("score spec-writing prints each source and label's median fault count", () => {
  expect(out).toContain("\nsource\tscenario\tlabel\truns\tmedian faults\tmedian ste breaks\n");
  expect(out).toContain("\nacme-1\tspec-writing\tbase\t3\t3\t0\n");
});
