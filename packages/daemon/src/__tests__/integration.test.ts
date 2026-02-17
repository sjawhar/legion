import { afterAll, beforeAll, describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { WorkerEntry } from "../daemon/serve-manager";
import { startServer } from "../daemon/server";
import { buildCollectedState } from "../state/decision";
import { computeSessionId, type FetchedIssueData } from "../state/types";

const TEAM_ID = "123e4567-e89b-12d3-a456-426614174000";

function randomPort(min = 19900, max = 19999): number {
  return min + Math.floor(Math.random() * (max - min + 1));
}

interface TestServerContext {
  baseUrl: string;
  createSessionCalls: Array<{ port: number; sessionId: string; workspace: string }>;
}

async function withTestServer(run: (ctx: TestServerContext) => Promise<void>): Promise<void> {
  const tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-integration-"));
  const stateFilePath = path.join(tempDir, "workers.json");
  const createSessionCalls: Array<{ port: number; sessionId: string; workspace: string }> = [];
  const sharedServePort = randomPort();

  const serveManager = {
    createSession: async (port: number, sessionId: string, workspace: string): Promise<void> => {
      createSessionCalls.push({ port, sessionId, workspace });
    },
    healthCheck: async (): Promise<boolean> => true,
  };

  const { server, stop } = startServer({
    port: randomPort(),
    hostname: "127.0.0.1",
    teamId: TEAM_ID,
    legionDir: tempDir,
    serveManager,
    sharedServePort,
    stateFilePath,
  });

  try {
    await run({ baseUrl: `http://127.0.0.1:${server.port}`, createSessionCalls });
  } finally {
    stop();
    await rm(tempDir, { recursive: true, force: true });
  }
}

describe("Integration: daemon HTTP lifecycle", () => {
  const originalSpawn = Bun.spawn;

  beforeAll(() => {
    Bun.spawn = ((..._args: Parameters<typeof Bun.spawn>) => {
      throw new Error("Bun.spawn should not be called in integration tests");
    }) as unknown as typeof Bun.spawn;
  });

  afterAll(() => {
    Bun.spawn = originalSpawn;
  });

  it("responds to health check", async () => {
    await withTestServer(async ({ baseUrl }) => {
      const response = await fetch(`${baseUrl}/health`);
      expect(response.ok).toBe(true);
      const data = (await response.json()) as {
        status: string;
        uptime: number;
        workerCount: number;
      };
      expect(data.status).toBe("ok");
      expect(typeof data.uptime).toBe("number");
    });
  });

  it("supports worker CRUD via HTTP", async () => {
    await withTestServer(async ({ baseUrl, createSessionCalls }) => {
      const createResponse = await fetch(`${baseUrl}/workers`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          issueId: "ENG-100",
          mode: "plan",
          workspace: "/tmp/legion-workspace",
          env: { LEGION_TEST: "true" },
        }),
      });
      expect(createResponse.ok).toBe(true);
      const created = (await createResponse.json()) as {
        id: string;
        port: number;
        sessionId: string;
      };
      expect(created.id).toBe("eng-100-plan");
      expect(typeof created.port).toBe("number");
      expect(created.sessionId).toBe(computeSessionId(TEAM_ID, "eng-100", "plan"));
      expect(createSessionCalls.length).toBe(1);
      expect(createSessionCalls[0].port).toBe(created.port);

      const listResponse = await fetch(`${baseUrl}/workers`);
      expect(listResponse.ok).toBe(true);
      const list = (await listResponse.json()) as WorkerEntry[];
      expect(list).toHaveLength(1);
      expect(list[0].id).toBe(created.id);

      const detailResponse = await fetch(`${baseUrl}/workers/${created.id}`);
      expect(detailResponse.ok).toBe(true);
      const detail = (await detailResponse.json()) as WorkerEntry;
      expect(detail.id).toBe(created.id);
      expect(detail.status).toBe("running");

      const deleteResponse = await fetch(`${baseUrl}/workers/${created.id}`, {
        method: "DELETE",
      });
      expect(deleteResponse.ok).toBe(true);
      const deleted = (await deleteResponse.json()) as { status: string };
      expect(deleted.status).toBe("stopped");

      const listAfter = await fetch(`${baseUrl}/workers`);
      expect(listAfter.ok).toBe(true);
      const listAfterJson = (await listAfter.json()) as WorkerEntry[];
      expect(listAfterJson).toHaveLength(0);
    });
  });
});

describe("Integration: state pipeline", () => {
  it("builds collected state with suggested actions", () => {
    const issues: FetchedIssueData[] = [
      {
        issueId: "ENG-1",
        status: "Backlog",
        labels: [],
        hasPr: false,
        prIsDraft: null,
        hasLiveWorker: false,
        workerMode: null,
        workerStatus: null,
        hasUserFeedback: false,
        hasUserInputNeeded: false,
        hasNeedsApproval: false,
        hasHumanApproved: false,
        source: null,
      },
      {
        issueId: "ENG-2",
        status: "Needs Review",
        labels: ["worker-done"],
        hasPr: true,
        prIsDraft: false,
        hasLiveWorker: false,
        workerMode: null,
        workerStatus: null,
        hasUserFeedback: false,
        hasUserInputNeeded: false,
        hasNeedsApproval: false,
        hasHumanApproved: false,
        source: null,
      },
    ];

    const collected = buildCollectedState(issues, TEAM_ID);
    const first = collected.issues["ENG-1"];
    const second = collected.issues["ENG-2"];

    expect(first.suggestedAction).toBe("dispatch_architect");
    expect(first.sessionId.startsWith("ses_")).toBe(true);
    expect(second.suggestedAction).toBe("transition_to_retro");
    expect(second.sessionId.startsWith("ses_")).toBe(true);
  });
});
