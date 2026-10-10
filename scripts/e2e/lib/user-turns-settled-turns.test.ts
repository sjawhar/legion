import { afterAll, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { jsonl } from "./omp-session-fixtures";
import { scriptFunctions } from "./script-functions";

// settled, from dispatch-user-turns.sh, against what else starts a turn after a stopped answer on
// the pinned Oh My Pi. A message sent into the session (a user message, an Envoy card) is a turn of
// its own, and a background job the model started (`details.async.state: "running"` on its tool
// result) starts one when its result is delivered (`async-result`, the host's yield-queue flush),
// unless the model already took the result with the job tool, whose result lists the job settled.
// Transcript entries of type `custom` (an extension's own record) and `compaction` start nothing.
const fn = scriptFunctions(join(import.meta.dir, "..", "dispatch-user-turns.sh"));
const dir = mkdtempSync(join(tmpdir(), "user-turns-settled-turns-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));

const user = {
  type: "message",
  message: { role: "user", tag: "m-1", timestamp: 1, content: [{ type: "text", text: "Hi." }] },
};
const answer = (timestamp: number) => ({
  type: "message",
  message: {
    role: "assistant",
    stopReason: "stop",
    timestamp,
    content: [{ type: "text", text: "Hello." }],
  },
});
const backgroundStart = (jobId: string) => ({
  type: "message",
  message: {
    role: "toolResult",
    toolName: "bash",
    timestamp: 2,
    details: { async: { jobId, state: "running", type: "bash" }, timeoutSeconds: 300 },
    content: [{ type: "text", text: `Backgrounded as job ${jobId}` }],
  },
});
const delivered = (jobId: string) => ({
  type: "custom_message",
  customType: "async-result",
  display: true,
  attribution: "agent",
  details: { jobs: [{ jobId, type: "bash" }] },
  content: `Background job ${jobId} has completed.`,
});
const waitedFor = (jobId: string) => ({
  type: "message",
  message: {
    role: "toolResult",
    toolName: "job",
    timestamp: 3,
    details: { op: "wait", jobs: [{ id: jobId, status: "completed", type: "bash" }] },
    content: [{ type: "text", text: `${jobId} completed` }],
  },
});
const card = {
  type: "custom_message",
  customType: "envoy-message",
  display: true,
  content: "A card from another session.",
};

let sessions = 0;
function isSettled(...entries: unknown[]): boolean {
  const file = join(dir, `session-${++sessions}.jsonl`);
  writeFileSync(file, jsonl(entries));
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -uo pipefail\nsession_file=${JSON.stringify(file)}\n${fn("settled")}\nsettled`,
    ],
    { env: process.env }
  );
  return ran.exitCode === 0;
}

describe("settled", () => {
  test("a card sent in after the stopped answer is not settled: it starts a turn", () => {
    expect(isSettled(user, answer(10), card)).toBe(false);
  });

  test("an extension's record and a compaction after the stopped answer are settled: they start no turn", () => {
    const record = { type: "custom", customType: "envoy-dispatch-handled-attempt", data: {} };
    const compaction = { type: "compaction", summary: "…", firstKeptEntryId: "e1" };
    expect(isSettled(user, answer(10), record, compaction)).toBe(true);
  });

  test("a background job still running at the stopped answer is not settled: its result starts a turn", () => {
    expect(isSettled(user, backgroundStart("bg_9"), answer(10))).toBe(false);
  });

  test("a background job whose result was delivered and answered is settled", () => {
    expect(
      isSettled(user, backgroundStart("bg_9"), answer(10), delivered("bg_9"), answer(20))
    ).toBe(true);
  });

  test("a background job the model took with the job tool is settled: its result is never delivered", () => {
    expect(isSettled(user, backgroundStart("bg_9"), waitedFor("bg_9"), answer(10))).toBe(true);
  });
});
