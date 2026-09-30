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

/** An assistant turn whose one tool call is `bash` running `command`. */
const bash = (command: string) => ({
  type: "message",
  message: {
    role: "assistant",
    content: [{ type: "toolCall", id: "call-1", name: "bash", arguments: { command } }],
  },
});
/** The legion stand-in's record of an accepted test handoff write, after the run's other calls. */
const write = {
  at: "2026-09-30T17:29:11.031Z",
  as: "legion",
  argv: ["handoff", "write", "--phase", "test"],
  exit: 0,
};
const calls = (records: object[]) =>
  [...records, write].map((record) => `${JSON.stringify(record)}\n`).join("");
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
  [...header, turn[0], bash('BUN=$(command -v bun); "$BUN" greet.ts Ada')]
);
// Only mentioned it: a PR body drafted in a heredoc names `bun greet.ts`, and bun never ran it.
run(
  "tester-proof-ran-2",
  { "out.txt": "exit=0\n", "world.json": world, "calls.jsonl": calls([]) },
  [
    ...header,
    turn[0],
    bash("cat > /tmp/pr-body.md <<'EOF'\nAdds `greet(name)` and `bun greet.ts <name>`.\nEOF"),
  ]
);
// Read the other label's checkout, in a command whose path starts past its 200th character.
run("tester-proof-cross-1", { "out.txt": "exit=0\n", "world.json": world }, [
  ...header,
  turn[0],
  bash(`echo ${"x".repeat(220)} && cat /checkouts/base/skills/legion-worker/SKILL.md`),
]);

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
