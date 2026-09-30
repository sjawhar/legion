import { describe, expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

const program = fileURLToPath(new URL("./approvals-while-blocks-open.jq", import.meta.url));

/** Runs the stage 4b design-gate check over an issue's asks, as the driver does. */
function earlyRequests(asks: unknown[]): string[] {
  const run = Bun.spawnSync(["jq", "-c", "--arg", "artifact", "spec-1", "-f", program], {
    stdin: new TextEncoder().encode(JSON.stringify(asks)),
  });
  if (run.exitCode !== 0) throw new Error(`jq exited ${run.exitCode}: ${run.stderr.toString()}`);
  return JSON.parse(run.stdout.toString());
}

const block = {
  kind: "question",
  block_id: "block-1",
  block_artifact: { id: "spec-1" },
  question: "Where does the smoke file go?",
  created_at: "2026-09-30T10:00:00.5Z",
  answer: { at: "2026-09-30T10:05:00.123456789Z" },
};
const approval = (version: number, createdAt: string, state: string) => ({
  kind: "approval",
  block_id: null,
  approval: { artifact_id: "spec-1", name: "spec.md", version },
  question: `Approve spec.md (version ${version})? Proposes the file under docs/smoke/.`,
  created_at: createdAt,
  state,
});

describe("approvals-while-blocks-open.jq", () => {
  test("a request made after every block was settled passes", () => {
    expect(earlyRequests([block, approval(5, "2026-09-30T10:06:00.7Z", "answered")])).toEqual([]);
  });

  // The version-3 request was retracted when the block's answer wrote version 4, and the approved
  // request at version 5 came after the answer; the early one still counts.
  test("an early request is caught though it was retracted and a later one was approved", () => {
    expect(
      earlyRequests([
        block,
        approval(3, "2026-09-30T10:02:00Z", "resolved"),
        approval(5, "2026-09-30T10:06:00.7Z", "answered"),
      ])
    ).toEqual(["version 3: Where does the smoke file go?"]);
  });

  test("a block never answered keeps every later request early, and another document's asks are ignored", () => {
    const unanswered = { ...block, answer: null };
    const elsewhere = {
      ...approval(2, "2026-09-30T10:03:00Z", "open"),
      approval: { artifact_id: "other", name: "notes.md", version: 2 },
    };
    expect(
      earlyRequests([unanswered, elsewhere, approval(5, "2026-09-30T10:06:00Z", "open")])
    ).toEqual(["version 5: Where does the smoke file go?"]);
  });
});
