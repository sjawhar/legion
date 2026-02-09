import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ToolContext } from "@opencode-ai/plugin";
import { writeJsonAtomic } from "../storage";
import { createTaskClaimNextTool } from "../task-claim";
import type { Task } from "../types";
import { LEASE_DURATION_MS, MAX_CLAIM_ATTEMPTS } from "../types";

let tempDir: string;

function makeContext(sessionID = "session-1"): ToolContext {
  return {
    sessionID,
    messageID: "msg-1",
    agent: "orchestrator",
    directory: "/tmp",
    worktree: "/tmp",
    abort: new AbortController().signal,
    metadata: () => {},
    ask: async () => {},
  };
}

function writeTask(dir: string, task: Task): void {
  writeJsonAtomic(path.join(dir, `${task.id}.json`), task);
}

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: "T-claim-test",
    subject: "Claimable",
    description: "",
    status: "pending",
    blocks: [],
    blockedBy: [],
    threadID: "session-1",
    ...overrides,
  };
}

beforeEach(() => {
  tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "task-claim-test-"));
});

afterEach(() => {
  fs.rmSync(tempDir, { recursive: true, force: true });
});

describe("task_claim_next", () => {
  it("claims a ready+pending task", async () => {
    writeTask(tempDir, makeTask({ id: "T-ready" }));

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext("agent-1")));

    expect(result.task).toBeTruthy();
    expect(result.task.id).toBe("T-ready");
    expect(result.task.status).toBe("in_progress");
    expect(result.task.owner).toBe("agent-1");
    expect(result.task.metadata.claimed_by_session).toBe("agent-1");
    expect(result.task.metadata.attempt_count).toBe(1);
    expect(result.task.metadata.lease_expires_at).toBeGreaterThan(Date.now());
  });

  it("returns null when no ready tasks", async () => {
    writeTask(tempDir, makeTask({ id: "T-blocked", blockedBy: ["T-dep"] }));

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    expect(result.task).toBeNull();
  });

  it("picks deterministically by ID sort", async () => {
    writeTask(tempDir, makeTask({ id: "T-bbb", subject: "B" }));
    writeTask(tempDir, makeTask({ id: "T-aaa", subject: "A" }));
    writeTask(tempDir, makeTask({ id: "T-ccc", subject: "C" }));

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    expect(result.task.id).toBe("T-aaa");
  });

  it("skips in_progress tasks", async () => {
    writeTask(tempDir, makeTask({ id: "T-busy", status: "in_progress" }));
    writeTask(tempDir, makeTask({ id: "T-free" }));

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    expect(result.task.id).toBe("T-free");
  });

  it("skips tasks with blockers not completed/cancelled", async () => {
    writeTask(tempDir, makeTask({ id: "T-dep", status: "in_progress" }));
    writeTask(tempDir, makeTask({ id: "T-blocked", blockedBy: ["T-dep"] }));

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    expect(result.task).toBeNull();
  });

  it("claims task blocked by cancelled dep (cancelled satisfies)", async () => {
    writeTask(tempDir, makeTask({ id: "T-cancelled", status: "cancelled" }));
    writeTask(tempDir, makeTask({ id: "T-waiting", blockedBy: ["T-cancelled"] }));

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    expect(result.task).toBeTruthy();
    expect(result.task.id).toBe("T-waiting");
  });

  it("increments attempt_count on each claim", async () => {
    writeTask(tempDir, makeTask({ id: "T-retry", metadata: { attempt_count: 1 } }));

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    expect(result.task.metadata.attempt_count).toBe(2);
  });

  it("excludes tasks at MAX_CLAIM_ATTEMPTS and reports escalation", async () => {
    writeTask(
      tempDir,
      makeTask({
        id: "T-exhausted",
        metadata: { attempt_count: MAX_CLAIM_ATTEMPTS },
      })
    );

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    expect(result.task).toBeNull();
    expect(result.escalated).toHaveLength(1);
    expect(result.escalated[0].id).toBe("T-exhausted");
  });

  it("reclaims expired lease", async () => {
    writeTask(
      tempDir,
      makeTask({
        id: "T-expired",
        status: "in_progress",
        owner: "old-agent",
        metadata: {
          lease_expires_at: Date.now() - 1000,
          claimed_by_session: "old-agent",
          attempt_count: 1,
        },
      })
    );

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext("new-agent")));

    expect(result.task).toBeTruthy();
    expect(result.task.id).toBe("T-expired");
    expect(result.task.owner).toBe("new-agent");
    expect(result.task.metadata.attempt_count).toBe(2);
  });

  it("does not reclaim non-expired lease", async () => {
    writeTask(
      tempDir,
      makeTask({
        id: "T-active",
        status: "in_progress",
        owner: "busy-agent",
        metadata: {
          lease_expires_at: Date.now() + LEASE_DURATION_MS,
          claimed_by_session: "busy-agent",
          attempt_count: 1,
        },
      })
    );

    const tool = createTaskClaimNextTool(undefined, tempDir);
    const result = JSON.parse(await tool.execute({}, makeContext()));

    expect(result.task).toBeNull();
  });
});
