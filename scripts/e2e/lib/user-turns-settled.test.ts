import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { jsonl } from "./omp-session-fixtures";
import { scriptFunctions } from "./script-functions";

// settled, taken from dispatch-user-turns.sh by name and run against a session written as Oh My Pi
// writes it: the wait every slash-command check runs before it sends from the page, so that no
// command lands inside another turn. The page's composer refuses Enter while the session's stream
// shows a turn running (assistant-ui's ComposerInput returns early when the thread is running), so
// a check that sends while one runs never sends at all: the page driver waits for a message that
// never left the composer, and the check fails. Oh My Pi writes an assistant message to the
// transcript only when it ends, and the result of a background job the model started is a custom
// message that starts the next turn, so the last message entry being a stopped answer does not
// mean the session is idle.
const fn = scriptFunctions(join(import.meta.dir, "..", "dispatch-user-turns.sh"));
const dir = mkdtempSync(join(tmpdir(), "user-turns-settled-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const userTurn = {
  type: "message",
  timestamp: "2026-10-10T15:17:42.996Z",
  message: {
    role: "user",
    tag: "m-file-command",
    timestamp: 1791645456245,
    content: [{ type: "text", text: "my deploy's code word is DEPLOY1. What is it?" }],
  },
};
const answer = {
  type: "message",
  timestamp: "2026-10-10T15:17:44.861Z",
  message: {
    role: "assistant",
    stopReason: "stop",
    timestamp: 1791645463000,
    content: [{ type: "text", text: "Your deploy's code word is **DEPLOY1**." }],
  },
};
// The host's delivery of a background job the model started in its last turn; Oh My Pi runs the
// session's next turn on it.
const backgroundJobResult = {
  type: "custom_message",
  customType: "async-result",
  timestamp: "2026-10-10T15:17:44.863Z",
  display: true,
  attribution: "agent",
  details: { jobs: [{ jobId: "bg_7", type: "bash" }] },
  content:
    "<system-notice>\nBackground job bg_7 has completed. Resume your work using the result below.",
};

let sessions = 0;
// isSettled runs settled over a session of ENTRIES and returns whether it found the session idle.
function isSettled(...entries: unknown[]): boolean {
  const file = join(dir, `session-${++sessions}.jsonl`);
  writeFileSync(file, jsonl(entries));
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -uo pipefail
session_file=${JSON.stringify(file)}
${fn("settled")}
settled`,
    ],
    { env: process.env }
  );
  return ran.exitCode === 0;
}

describe("settled", () => {
  test("a session whose last entry is a stopped answer is settled", () => {
    expect(isSettled(userTurn, answer)).toBe(true);
  });

  test("a background job's result after the stopped answer is not settled: it starts the next turn", () => {
    expect(isSettled(userTurn, answer, backgroundJobResult)).toBe(false);
  });
});

const backgroundStart = (jobId: string) => ({
  type: "message",
  message: {
    role: "toolResult",
    toolName: "bash",
    timestamp: 1791645462000,
    details: { async: { jobId, state: "running", type: "bash" }, timeoutSeconds: 300 },
    content: [{ type: "text", text: `Backgrounded as job ${jobId}` }],
  },
});
const answerAt = (timestamp: number) => ({
  type: "message",
  message: {
    role: "assistant",
    stopReason: "stop",
    timestamp,
    content: [{ type: "text", text: "Done." }],
  },
});
const card = {
  type: "custom_message",
  customType: "envoy-message",
  display: true,
  content: "A card from another session.",
};
const waitedFor = (jobId: string) => ({
  type: "message",
  message: {
    role: "toolResult",
    toolName: "job",
    timestamp: 1791645462500,
    details: { op: "wait", jobs: [{ id: jobId, status: "completed", type: "bash" }] },
    content: [{ type: "text", text: `${jobId} completed` }],
  },
});

test("a card after the stopped answer is not settled: it starts the next turn", () => {
  expect(isSettled(userTurn, answer, card)).toBe(false);
});

test("an extension record and a compaction after the stopped answer are settled: they start no turn", () => {
  const record = { type: "custom", customType: "envoy-dispatch-handled-attempt", data: {} };
  const compaction = { type: "compaction", summary: "…", firstKeptEntryId: "e1" };
  expect(isSettled(userTurn, answer, record, compaction)).toBe(true);
});

test("a background job still running at the stopped answer is not settled: its result starts a turn", () => {
  expect(isSettled(userTurn, backgroundStart("bg_9"), answer)).toBe(false);
});

test("a background job whose result was delivered and answered is settled", () => {
  expect(
    isSettled(
      userTurn,
      backgroundStart("bg_9"),
      answer,
      { ...backgroundJobResult, details: { jobs: [{ jobId: "bg_9", type: "bash" }] } },
      answerAt(1791645464000)
    )
  ).toBe(true);
});

test("a background job the model took with the job tool is settled: its result is never delivered", () => {
  expect(isSettled(userTurn, backgroundStart("bg_9"), waitedFor("bg_9"), answer)).toBe(true);
});
