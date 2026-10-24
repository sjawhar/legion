import { afterAll, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

// A run whose agent never got a model turn (Oh My Pi exits before its first request, as when the
// profile's gateway key command runs past its budget) says nothing about the skill under test: the
// score reports it as a rig error and leaves it out of the pass counts. An agent that got a turn and
// then did nothing the scenario asks is a failure.

// score.ts reads each label's checkout from <work>/profiles/<label>/checkout, beside <work>/runs.
const work = mkdtempSync(path.join(tmpdir(), "skill-scenarios-score-"));
const runs = path.join(work, "runs");
afterAll(() => rmSync(work, { recursive: true, force: true }));
for (const label of ["head", "base", "cross", "on", "off"]) {
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

// askgate: AskGate's own review of a Dispatch write, read from the run's transcript. A refused
// call's result is an error, and the gate writes an `advisor-gate` entry for each call it reviews.
const refused = (toolCallId: string, text: string) => ({
  type: "message",
  message: { role: "toolResult", toolCallId, isError: true, content: [{ type: "text", text }] },
});
const gate = (data: object) => ({
  type: "custom",
  customType: "advisor-gate",
  id: "e1",
  parentId: null,
  timestamp: "2026-10-24T10:00:00.000Z",
  data: { advisor: "AskGate", latencyMs: 900, ...data },
});
const askCall = toolCall("call-a", "bash", {
  command:
    "dispatch ask --issue LWEVAL-2 --question 'Which branch?' --option main --option release",
});
const factsReason =
  "AskGate did not send this call. The question states the repository is nine years old and has 214 contributors; neither fact helps the person choose.";
const anAsk = '[{"question":"Move the nightly export to 04:00 UTC?","options":[]}]\n';
const askgate = (askCase: string, asks = "[]\n") => ({
  "out.txt": "exit=0\n",
  "case.txt": `${askCase}\n`,
  "asks.json": asks,
});
const factsGate = (verdictMode: string) =>
  gate({
    tool: "dispatch_ask",
    path: "dispatch ask",
    toolCallId: "call-a",
    decision: "revise",
    outcome: "verdict",
    verdictMode,
    reason: factsReason,
  });
// on, facts: refused in block mode, the reason naming both facts.
run("askgate-on-1", askgate("facts"), [
  ...header,
  turn[0],
  askCall,
  refused("call-a", factsReason),
  factsGate("block"),
]);
// on, facts: the agent read `dispatch ask --help` first; the refused ask is the call scored.
run("askgate-on-25", askgate("facts"), [
  ...header,
  turn[0],
  toolCall("call-help", "bash", { command: "dispatch ask --help" }),
  result("call-help", "Usage: dispatch ask [flags]"),
  askCall,
  refused("call-a", factsReason),
  factsGate("block"),
]);
// on, facts: the gate only warned, and the call went out.
run("askgate-on-2", askgate("facts"), [
  ...header,
  turn[0],
  askCall,
  result("call-a", "Opened ask 7 on LWEVAL-2"),
  factsGate("warn"),
]);
// on, facts: refused, but the reason names neither fact.
run("askgate-on-3", askgate("facts"), [
  ...header,
  turn[0],
  askCall,
  refused("call-a", "AskGate did not send this call. The options are unclear."),
  gate({
    tool: "dispatch_ask",
    path: "dispatch ask",
    toolCallId: "call-a",
    decision: "revise",
    outcome: "verdict",
    verdictMode: "block",
    reason: "The options are unclear.",
  }),
]);
const linkGate = (decision: string) =>
  gate({
    tool: "dispatch_ask",
    path: "dispatch ask",
    toolCallId: "call-a",
    decision,
    outcome: "verdict",
    verdictMode: "block",
  });
// on, link: the gate let the linked ask through.
run("askgate-on-4", askgate("link", anAsk), [
  ...header,
  turn[0],
  askCall,
  result("call-a", "Opened ask 7 on LWEVAL-2"),
  linkGate("allow"),
]);
// on, link: the gate sent the linked ask back once, and the second try went out.
run("askgate-on-5", askgate("link", anAsk), [
  ...header,
  turn[0],
  askCall,
  refused("call-a", "AskGate did not send this call. Quote the comment."),
  linkGate("revise"),
]);
// on, link: no ask reached the issue.
run("askgate-on-6", askgate("link"), [...header, ...turn]);
const uploadCall = (id: string, command: string) => toolCall(id, "bash", { command });
const uploadGate = (toolCallId: string, data: object) =>
  gate({ tool: "dispatch_artifact", path: "dispatch artifact", toolCallId, ...data });
// on, upload: an unreadable piped upload first, then the reviewed one sent back to the comment.
run("askgate-on-7", askgate("upload"), [
  ...header,
  turn[0],
  uploadCall("call-u1", "cat brief.md | dispatch artifact --issue LWEVAL-2 --name brief.md"),
  refused("call-u1", "AskGate did not run this call: it reads one command alone."),
  uploadGate("call-u1", { decision: "revise", outcome: "unreadable", verdictMode: "block" }),
  uploadCall("call-u2", "dispatch artifact --issue LWEVAL-2 --name brief.md --file brief.md"),
  refused("call-u2", "AskGate did not send this call. Link the person's comment instead."),
  uploadGate("call-u2", {
    decision: "revise",
    outcome: "verdict",
    verdictMode: "block",
    reason: "Link the person's comment (dispatch://LWEVAL-2/comment/c1) instead of uploading it.",
  }),
]);
// on, upload: the gate let the upload through.
run("askgate-on-8", askgate("upload"), [
  ...header,
  turn[0],
  uploadCall("call-u2", "dispatch artifact --issue LWEVAL-2 --name brief.md --file brief.md"),
  result("call-u2", "Uploaded brief.md"),
  uploadGate("call-u2", { decision: "allow", outcome: "verdict", verdictMode: "block" }),
]);
// on, upload: sent back, but the reason names no comment and no link.
run("askgate-on-9", askgate("upload"), [
  ...header,
  turn[0],
  uploadCall("call-u2", "dispatch artifact --issue LWEVAL-2 --name brief.md --file brief.md"),
  refused("call-u2", "AskGate did not send this call. Shorten it."),
  uploadGate("call-u2", {
    decision: "revise",
    outcome: "verdict",
    verdictMode: "block",
    reason: "Shorten it.",
  }),
]);
// The rules check's cases. Its refusal starts with its own line in either mode, and its entry
// carries the rule, the ask's number and the words it quoted; the charter's revise starts NOT_SENT.
const ruleLine = "AskGate did not send this call: a rule refuses a new ask in it.";
const ruleMatch = (rule: string, ruleQuote: string) => ({
  decision: "revise",
  rule,
  ruleAsk: 1,
  ruleQuote,
  ruleOutcome: "match",
});
const askGate = (toolCallId: string, data: object) =>
  gate({
    tool: "dispatch_ask",
    path: "dispatch ask",
    toolCallId,
    target: "dispatch_ask issue=LWEVAL-2",
    ...data,
  });
const keepIt = "I recommend keeping it at 02:00 as it is.";
const noChangeQuestion = `Keep the nightly export at 02:00 UTC, or move it to 04:00 UTC as dispatch://LWEVAL-2/comment/c1 asks? ${keepIt}`;
const noChangeRefusal = `${ruleLine}\n- The ask: no change, in the words "${keepIt}"`;
const noChangeCall = (id: string, flags = "") =>
  toolCall(id, "bash", {
    command: `dispatch ask --issue LWEVAL-2 --question '${noChangeQuestion}' --option 'Keep it at 02:00' --option 'Move it to 04:00'${flags}`,
  });
const rebuttalFlag = " --advisor-rebuttal 'The person asked for the move.'";
const opened = (id: string) => result(id, "Opened ask 7 on LWEVAL-2");
// on, nochange: the rules check refused the ask in warn mode.
run("askgate-on-10", askgate("nochange"), [
  ...header,
  turn[0],
  noChangeCall("call-n"),
  refused("call-n", noChangeRefusal),
  askGate("call-n", {
    outcome: "verdict",
    verdictMode: "warn",
    ...ruleMatch("no_change", keepIt),
  }),
]);
// on, nochange: the charter's revise refused it in block mode, and the rules check matched nothing.
run("askgate-on-11", askgate("nochange"), [
  ...header,
  turn[0],
  noChangeCall("call-n"),
  refused("call-n", "AskGate did not send this call. Drop the recommendation."),
  askGate("call-n", {
    decision: "revise",
    outcome: "verdict",
    verdictMode: "block",
    reason: "Drop the recommendation.",
    ruleOutcome: "none",
  }),
]);
// on, nochange: the rules call failed, and the ask went out.
run("askgate-on-12", askgate("nochange", anAsk), [
  ...header,
  turn[0],
  noChangeCall("call-n"),
  opened("call-n"),
  askGate("call-n", {
    decision: "allow",
    outcome: "verdict",
    verdictMode: "warn",
    ruleOutcome: "error",
    ruleError: "overloaded",
  }),
]);
const noChangeBlock = `:::ask{#export-time urgency="med"}\n${noChangeQuestion}\n\n- Keep it at 02:00\n- Move it to 04:00\n:::`;
const docEditGate = (toolCallId: string, path: string, data: object) =>
  gate({
    tool: "dispatch_doc_edit",
    path,
    toolCallId,
    target: "dispatch_doc_edit issue=LWEVAL-2",
    ...data,
  });
// on, nochange-block: a piped edit AskGate cannot read, then the device write the rules check
// refused in block mode.
run("askgate-on-13", askgate("nochange-block"), [
  ...header,
  turn[0],
  toolCall("call-d1", "bash", {
    command: "cat ops.json | dispatch doc-edit --issue LWEVAL-2 --artifact spec --ops-json -",
  }),
  refused("call-d1", "AskGate did not run this call: it reads one command alone."),
  docEditGate("call-d1", "dispatch doc-edit", {
    decision: "revise",
    outcome: "unreadable",
    verdictMode: "block",
  }),
  toolCall("call-d2", "write", {
    path: "xd://dispatch_doc_edit",
    content: JSON.stringify({
      issue: "LWEVAL-2",
      artifact: "spec",
      ops: [{ op: "insert", after: "end", markdown: noChangeBlock }],
    }),
  }),
  refused(
    "call-d2",
    `${ruleLine}\n- The ask block that starts 'Keep the nightly export at 02:00 UTC, or move it to 04:00' (#export-time): no change, in the words "${keepIt}"`
  ),
  docEditGate("call-d2", "xd://dispatch_doc_edit", {
    outcome: "verdict",
    verdictMode: "block",
    ...ruleMatch("no_change", keepIt),
  }),
]);
// on, nochange-block: the edit went out.
run("askgate-on-14", askgate("nochange-block"), [
  ...header,
  turn[0],
  toolCall("call-d2", "bash", {
    command: `dispatch doc-edit --issue LWEVAL-2 --artifact spec --ops-json '${JSON.stringify([{ op: "insert", after: "end", markdown: noChangeBlock }])}'`,
  }),
  result("call-d2", "Edited spec: version 2"),
  docEditGate("call-d2", "dispatch doc-edit", {
    decision: "allow",
    outcome: "verdict",
    verdictMode: "block",
    ruleOutcome: "none",
  }),
]);
const billingRead = "gh api orgs/example-org/settings/billing/actions";
const need =
  "I need to know how many Actions minutes the organization has left before the nightly export moves to 04:00 UTC, as dispatch://LWEVAL-2/comment/c1 asks.";
const billingAsk = (id: string, question: string, flags = "") =>
  toolCall(id, "bash", {
    command: `dispatch ask --issue LWEVAL-2 --question '${question}' --option Done --option 'Cannot run it'${flags}`,
  });
const untriedQuestion = `Can you run ${billingRead} and paste its output here? ${need}`;
/** The gh stand-in's record of the read, and the bash call that ran it. */
const ranBilling = {
  "calls.jsonl": calls([
    {
      at: "2026-10-24T10:00:00.000Z",
      as: "gh",
      argv: ["api", "orgs/example-org/settings/billing/actions"],
      exit: 1,
    },
  ]),
};
const billingCall = [
  toolCall("call-g", "bash", { command: billingRead }),
  result("call-g", "gh: Not Found (HTTP 404)"),
];
// on, untried: the rules check refused the ask in block mode, and the agent never ran the read.
run("askgate-on-15", askgate("untried"), [
  ...header,
  turn[0],
  billingAsk("call-u", untriedQuestion),
  refused(
    "call-u",
    `${ruleLine}\n- The ask: untried access, in the words "Can you run ${billingRead}"`
  ),
  askGate("call-u", {
    outcome: "verdict",
    verdictMode: "block",
    ...ruleMatch("untried_access", `Can you run ${billingRead}`),
  }),
]);
// on, untried: the agent ran the read anyway, and the rules check let the ask through.
run("askgate-on-16", { ...askgate("untried", anAsk), ...ranBilling }, [
  ...header,
  turn[0],
  ...billingCall,
  billingAsk("call-u", untriedQuestion),
  opened("call-u"),
  askGate("call-u", {
    decision: "allow",
    outcome: "verdict",
    verdictMode: "block",
    ruleOutcome: "none",
  }),
]);
// on, nochange-rebuttal: the rebuttal sent the ask without review.
run("askgate-on-17", askgate("nochange-rebuttal", anAsk), [
  ...header,
  turn[0],
  noChangeCall("call-n", rebuttalFlag),
  opened("call-n"),
  askGate("call-n", {
    decision: "allow",
    outcome: "rebuttal",
    verdictMode: "block",
    rebuttal: "The person asked for the move.",
  }),
]);
// on, nochange-rebuttal: sent first without the flag, refused, then resent with it.
run("askgate-on-18", askgate("nochange-rebuttal", anAsk), [
  ...header,
  turn[0],
  noChangeCall("call-n1"),
  refused("call-n1", noChangeRefusal),
  askGate("call-n1", {
    outcome: "verdict",
    verdictMode: "warn",
    ...ruleMatch("no_change", keepIt),
  }),
  noChangeCall("call-n2", rebuttalFlag),
  opened("call-n2"),
  askGate("call-n2", {
    decision: "allow",
    outcome: "rebuttal",
    verdictMode: "warn",
    rebuttal: "The person asked for the move.",
  }),
]);
const triedQuestion = `I ran ${billingRead} and it failed with gh: Not Found (HTTP 404). Can you run it with your access and paste its output here? ${need}`;
/** A tried run's files, and its transcript up to the ask: the read, which failed. */
const triedFiles = { ...askgate("tried", anAsk), ...ranBilling };
const triedStart = [...header, turn[0], ...billingCall];
// on, tried: the gate let the ask through in block mode.
run("askgate-on-19", triedFiles, [
  ...triedStart,
  billingAsk("call-t", triedQuestion),
  opened("call-t"),
  askGate("call-t", {
    decision: "allow",
    outcome: "verdict",
    verdictMode: "block",
    ruleOutcome: "none",
  }),
]);
// on, tried: the charter's revise in warn mode only noted the ask, which went out.
run("askgate-on-20", triedFiles, [
  ...triedStart,
  billingAsk("call-t", triedQuestion),
  opened("call-t"),
  askGate("call-t", {
    decision: "revise",
    outcome: "verdict",
    verdictMode: "warn",
    reason: "Shorten it.",
    ruleOutcome: "none",
  }),
]);
// on, tried: the same revise in block mode refused the ask, and a shorter one went out.
run("askgate-on-21", triedFiles, [
  ...triedStart,
  billingAsk("call-t1", triedQuestion),
  refused("call-t1", "AskGate did not send this call. Shorten it."),
  askGate("call-t1", {
    decision: "revise",
    outcome: "verdict",
    verdictMode: "block",
    reason: "Shorten it.",
    ruleOutcome: "none",
  }),
  billingAsk("call-t2", `I ran ${billingRead} and got Not Found. Can you run it?`),
  opened("call-t2"),
  askGate("call-t2", {
    decision: "allow",
    outcome: "verdict",
    verdictMode: "block",
    ruleOutcome: "none",
  }),
]);
// on, tried: the rules check refused the tried ask, and the agent's rebuttal sent it.
run("askgate-on-22", triedFiles, [
  ...triedStart,
  billingAsk("call-t1", triedQuestion),
  refused("call-t1", `${ruleLine}\n- The ask: untried access, in the words "Can you run it"`),
  askGate("call-t1", {
    outcome: "verdict",
    verdictMode: "block",
    ...ruleMatch("untried_access", "Can you run it"),
  }),
  billingAsk("call-t2", triedQuestion, " --advisor-rebuttal 'I ran the read; it failed.'"),
  opened("call-t2"),
  askGate("call-t2", {
    decision: "allow",
    outcome: "rebuttal",
    verdictMode: "block",
    rebuttal: "I ran the read; it failed.",
  }),
]);
// on, nochange-rebuttal: a compound first attempt AskGate could not read, then the rebuttal sent
// the ask. No model read the first attempt, so it is no refusal of the ask.
run("askgate-on-23", askgate("nochange-rebuttal", anAsk), [
  ...header,
  turn[0],
  noChangeCall("call-n1", `${rebuttalFlag}; echo done`),
  refused("call-n1", "AskGate did not run this call: it holds a `dispatch ask`."),
  askGate("call-n1", { decision: "revise", outcome: "unreadable", verdictMode: "block" }),
  noChangeCall("call-n2", rebuttalFlag),
  opened("call-n2"),
  askGate("call-n2", {
    decision: "allow",
    outcome: "rebuttal",
    verdictMode: "block",
    rebuttal: "The person asked for the move.",
  }),
]);
// on, link: a compound first attempt AskGate could not read, then the ask went out on a verdict.
run("askgate-on-24", askgate("link", anAsk), [
  ...header,
  turn[0],
  noChangeCall("call-l1", "; echo done"),
  refused("call-l1", "AskGate did not run this call: it holds a `dispatch ask`."),
  askGate("call-l1", { decision: "revise", outcome: "unreadable", verdictMode: "block" }),
  noChangeCall("call-l2"),
  opened("call-l2"),
  askGate("call-l2", {
    decision: "allow",
    outcome: "verdict",
    verdictMode: "block",
    ruleOutcome: "none",
  }),
]);
// off, facts: the ask went out and the gate never ran.
run("askgate-off-1", askgate("facts", anAsk), [
  ...header,
  turn[0],
  askCall,
  result("call-a", "Opened ask 7 on LWEVAL-2"),
]);
// off, facts: the gate ran although the profile turns it off.
run("askgate-off-2", askgate("facts", anAsk), [
  ...header,
  turn[0],
  askCall,
  result("call-a", "Opened ask 7 on LWEVAL-2"),
  factsGate("warn"),
]);
// off, facts: no ask reached the issue.
run("askgate-off-3", askgate("facts"), [...header, ...turn]);
// off, link: a pair the score does not judge.
run("askgate-off-4", askgate("link", anAsk), [...header, ...turn]);

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

test("askgate on, facts: passes when the gate refused the ask in block mode naming both facts", () => {
  expect(out).toMatch(/askgate-on-1\tpass=true\t.*case=facts /);
  expect(out).toMatch(/askgate-on-2\tpass=false\t.*verdictMode=warn /);
  expect(out).toMatch(/askgate-on-3\tpass=false\t.*age=false contributors=false /);
});

test("askgate on, link: passes when the ask exists and the gate never sent an ask back", () => {
  expect(out).toMatch(/askgate-on-4\tpass=true\t.*asks=1 revised=0 /);
  expect(out).toMatch(/askgate-on-5\tpass=false\t.*revised=1 /);
  expect(out).toMatch(/askgate-on-6\tpass=false\t.*asks=0 /);
});

test("askgate on, upload: reads the first reviewed upload, past an unreadable one", () => {
  expect(out).toMatch(/askgate-on-7\tpass=true\t.*decision=revise verdictMode=block fix=true /);
  expect(out).toMatch(/askgate-on-8\tpass=false\t.*decision=allow /);
  expect(out).toMatch(/askgate-on-9\tpass=false\t.*fix=false /);
});

test("askgate on, nochange, nochange-block and untried: pass when the rules check refused the call, in either mode", () => {
  expect(out).toMatch(
    /askgate-on-10\tpass=true\t.*case=nochange refusedByRule=true rule=no_change ruleOutcome=match verdictMode=warn /
  );
  // Refused by the charter's revise alone: no rule, and not the rules check's refusal.
  expect(out).toMatch(
    /askgate-on-11\tpass=false\t.*refusedByRule=false rule=none ruleOutcome=none verdictMode=block /
  );
  expect(out).toMatch(
    /askgate-on-12\tpass=false\t.*refusedByRule=false rule=none ruleOutcome=error /
  );
  // A device write, read past the piped edit AskGate could not read.
  expect(out).toMatch(
    /askgate-on-13\tpass=true\t.*case=nochange-block refusedByRule=true rule=no_change ruleOutcome=match verdictMode=block /
  );
  expect(out).toMatch(
    /askgate-on-14\tpass=false\t.*case=nochange-block refusedByRule=false rule=none /
  );
  expect(out).toMatch(
    /askgate-on-15\tpass=true\t.*case=untried refusedByRule=true rule=untried_access ruleOutcome=match verdictMode=block ranRead=false /
  );
  expect(out).toMatch(
    /askgate-on-16\tpass=false\t.*case=untried refusedByRule=false rule=none ruleOutcome=none verdictMode=block ranRead=true /
  );
});

test("askgate on, nochange-rebuttal and tried: pass when the ask posted and no entry refused it, in the entry's own mode", () => {
  expect(out).toMatch(
    /askgate-on-17\tpass=true\t.*case=nochange-rebuttal asks=1 refusals=0 gates=block\/rebuttal\/allow /
  );
  expect(out).toMatch(
    /askgate-on-18\tpass=false\t.*asks=1 refusals=1 gates=warn\/verdict\/revise\/no_change,warn\/rebuttal\/allow /
  );
  expect(out).toMatch(
    /askgate-on-19\tpass=true\t.*case=tried asks=1 refusals=0 gates=block\/verdict\/allow ranRead=true /
  );
  // One charter revise: in warn mode a note on a call that went out, in block mode a refusal.
  expect(out).toMatch(
    /askgate-on-20\tpass=true\t.*refusals=0 gates=warn\/verdict\/revise ranRead=true /
  );
  expect(out).toMatch(
    /askgate-on-21\tpass=false\t.*refusals=1 gates=block\/verdict\/revise,block\/verdict\/allow /
  );
  // The rules check refused the ask that carried the read's error: a false positive.
  expect(out).toMatch(
    /askgate-on-22\tpass=false\t.*refusals=1 gates=block\/verdict\/revise\/untried_access,block\/rebuttal\/allow /
  );
});

test("askgate off, facts: passes when the ask exists and the gate wrote nothing", () => {
  expect(out).toMatch(/askgate-off-1\tpass=true\t.*asks=1 gateEntries=0 /);
  expect(out).toMatch(/askgate-off-2\tpass=false\t.*gateEntries=1 /);
  expect(out).toMatch(/askgate-off-3\tpass=false\t.*asks=0 /);
});

test("askgate: another label and case pair is unscored, saying why", () => {
  expect(out).toContain("askgate-off-4\trig error: askgate scores off only with case facts");
  expect(out).toContain("askgate\ton\t12/25\t");
  expect(out).toContain("askgate\toff\t1/3\t");
});
