import { afterEach, describe, expect, it } from "bun:test";
import { mkdirSync } from "node:fs";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { type DaemonConfig, resolveDaemonConfig } from "../config";
import { type FeedbackEvent, FeedbackEventSchema } from "../feedback";
import { type DaemonHandle, startDaemon } from "../index";
import { resolveLegionPaths } from "../paths";
import type { RuntimeAdapter } from "../runtime/types";

function makeAdapter(): RuntimeAdapter {
  return {
    start: async () => {},
    stop: async () => {},
    healthy: async () => true,
    getPort: () => 15500,
    getServePid: () => 0,
    createSession: async (sessionId) => sessionId,
    sendPrompt: async () => {},
    getSessionStatus: async () => ({ data: undefined }),
    deleteSession: async () => {},
    sessionExists: async () => false,
  };
}

function readJsonl(contents: string): FeedbackEvent[] {
  return contents
    .trim()
    .split("\n")
    .filter(Boolean)
    .map((line) => JSON.parse(line) as FeedbackEvent);
}

function buildConfig(
  paths: ReturnType<typeof resolveLegionPaths>,
  stateFilePath: string,
  overrides: Partial<DaemonConfig> = {}
): DaemonConfig {
  const { config } = resolveDaemonConfig({ env: { LEGION_ID: "acme/widgets" } });
  return {
    ...config,
    paths,
    legionId: "acme/widgets",
    stateFilePath,
    daemonPort: 0,
    controllerSessionId: "ses_test",
    ...overrides,
  };
}

describe("feedback logging integration", () => {
  let tempHome: string | null = null;
  let handle: DaemonHandle | null = null;

  afterEach(async () => {
    if (handle) {
      await handle.stop();
      handle = null;
    }

    if (tempHome) {
      await rm(tempHome, { recursive: true, force: true });
      tempHome = null;
    }
  });

  it("writes worker.dispatched, worker.status_changed, and state.collected events to feedback.jsonl", async () => {
    tempHome = await mkdtemp(path.join(os.tmpdir(), "feedback-integration-"));
    const paths = resolveLegionPaths({}, tempHome);

    mkdirSync(paths.stateDir, { recursive: true });

    const stateFilePath = path.join(tempHome, "workers.json");
    handle = await startDaemon(buildConfig(paths, stateFilePath), {
      deps: { adapter: makeAdapter() },
    });

    const baseUrl = `http://127.0.0.1:${handle.server.port}`;
    const workspace = path.join(tempHome, "workspace-one");

    const createResponse = await fetch(`${baseUrl}/workers`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        issueId: "ENG-51",
        mode: "implement",
        workspace,
        version: 7,
      }),
    });
    expect(createResponse.status).toBe(200);
    const created = (await createResponse.json()) as { id: string };

    const patchResponse = await fetch(`${baseUrl}/workers/${created.id}`, {
      method: "PATCH",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        status: "running",
        crashCount: 0,
        lastCrashAt: null,
      }),
    });
    expect(patchResponse.status).toBe(200);

    const collectResponse = await fetch(`${baseUrl}/state/collect`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        backend: "linear",
        issues: [
          {
            identifier: "ENG-51",
            state: { name: "Todo" },
            labels: { nodes: [] },
          },
        ],
      }),
    });
    expect(collectResponse.status).toBe(200);

    const feedbackFile = paths.forLegion("acme/widgets").feedbackFile;
    await handle.stop();
    handle = null;

    const contents = await readFile(feedbackFile, "utf8");
    const events = readJsonl(contents);

    expect(events.length).toBeGreaterThanOrEqual(3);

    for (const event of events) {
      const parsed = FeedbackEventSchema.safeParse(event);
      expect(parsed.success).toBe(true);
      expect(event.schemaVersion).toBe(1);
      expect(event.legionId).toBe("acme/widgets");
      expect(Number.isNaN(Date.parse(event.timestamp))).toBe(false);
    }

    const dispatched = events.find((event) => event.event === "worker.dispatched");
    const statusChanged = events.find((event) => event.event === "worker.status_changed");
    const collected = events.find((event) => event.event === "state.collected");

    expect(dispatched).toBeDefined();
    expect(dispatched?.issueId).toBe("eng-51");
    expect(dispatched?.version).toBe(7);

    expect(statusChanged).toBeDefined();
    expect(statusChanged?.fromStatus).toBe("running");
    expect(statusChanged?.toStatus).toBe("running");

    expect(collected).toBeDefined();
    expect(collected?.issueId).toBe("ENG-51");
    expect(collected?.status).toBe("Todo");

    const timestamps = events.map((event) => Date.parse(event.timestamp));
    expect([...timestamps].sort((a, b) => a - b)).toEqual(timestamps);
  });

  it("does not create feedback.jsonl when feedback logging is disabled", async () => {
    tempHome = await mkdtemp(path.join(os.tmpdir(), "feedback-integration-"));
    const paths = resolveLegionPaths({}, tempHome);
    mkdirSync(paths.stateDir, { recursive: true });

    const stateFilePath = path.join(tempHome, "workers.json");
    handle = await startDaemon(buildConfig(paths, stateFilePath, { feedbackDisabled: true }), {
      deps: { adapter: makeAdapter() },
    });

    const baseUrl = `http://127.0.0.1:${handle.server.port}`;
    const response = await fetch(`${baseUrl}/state/collect`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        backend: "linear",
        issues: [],
      }),
    });
    expect(response.status).toBe(200);

    const feedbackFile = paths.forLegion("acme/widgets").feedbackFile;
    await handle.stop();
    handle = null;

    const feedbackStat = await stat(feedbackFile).catch(() => null);
    expect(feedbackStat).toBeNull();
  });

  it("rotates feedback.jsonl when feedbackMaxBytes is very small", async () => {
    tempHome = await mkdtemp(path.join(os.tmpdir(), "feedback-integration-"));
    const paths = resolveLegionPaths({}, tempHome);
    mkdirSync(paths.stateDir, { recursive: true });

    const stateFilePath = path.join(tempHome, "workers.json");
    handle = await startDaemon(buildConfig(paths, stateFilePath, { feedbackMaxBytes: 100 }), {
      deps: { adapter: makeAdapter() },
    });

    const baseUrl = `http://127.0.0.1:${handle.server.port}`;

    for (let index = 0; index < 5; index += 1) {
      const response = await fetch(`${baseUrl}/state/collect`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          backend: "linear",
          issues: [
            {
              identifier: `ENG-${index}`,
              state: { name: "Todo" },
              labels: { nodes: [] },
            },
          ],
        }),
      });
      expect(response.status).toBe(200);
    }

    const feedbackFile = paths.forLegion("acme/widgets").feedbackFile;
    const backupFile = `${feedbackFile}.1`;
    await handle.stop();
    handle = null;

    const activeStat = await stat(feedbackFile);
    const backupStat = await stat(backupFile);

    expect(activeStat.isFile()).toBe(true);
    expect(backupStat.isFile()).toBe(true);
  });
});
