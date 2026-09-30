import { afterAll, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

// A run whose agent never got a model turn (Oh My Pi exits before its first request, as when the
// profile's gateway key command runs past its budget) says nothing about the skill under test: the
// score reports it as a rig error and leaves it out of the pass counts. An agent that got a turn and
// then did nothing the scenario asks is a failure.

const runs = mkdtempSync(path.join(tmpdir(), "skill-scenarios-score-"));
afterAll(() => rmSync(runs, { recursive: true, force: true }));

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
