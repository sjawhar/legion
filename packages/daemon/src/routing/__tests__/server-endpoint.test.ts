import { afterEach, describe, expect, it } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { FeedbackLogger, type FeedbackWriter } from "../../daemon/feedback";
import type { RuntimeAdapter } from "../../daemon/runtime/types";
import { startServer } from "../../daemon/server";
import { writeStateFile } from "../../daemon/state-file";
import { ROUTING_CONFIG_PATH } from "../schema";

const legionId = "test-routing-endpoint";

class RecordingFeedbackWriter implements FeedbackWriter {
  lines: string[] = [];

  async append(line: string): Promise<void> {
    this.lines.push(line);
  }

  async flush(): Promise<void> {}
}

function makeAdapter(): RuntimeAdapter {
  return {
    start: async () => {},
    stop: async () => {},
    healthy: async () => true,
    getPort: () => 15600,
    getServePid: () => 0,
    createSession: async (sessionId: string) => sessionId,
    sendPrompt: async () => {},
    getSessionStatus: async () => ({ data: undefined }),
  };
}

function writeConfig(workspace: string, content: string): void {
  const configDir = path.dirname(path.join(workspace, ROUTING_CONFIG_PATH));
  mkdirSync(configDir, { recursive: true });
  writeFileSync(path.join(workspace, ROUTING_CONFIG_PATH), content, "utf-8");
}

const VALID_CONFIG = `
domains:
  - name: envoy
    paths:
      - "packages/envoy/**"
      - "packages/contracts/**"
    reviewers:
      - envoy-expert
  - name: daemon
    paths:
      - "packages/daemon/**"
    reviewers:
      - daemon-expert
`;

describe("POST /routing/match", () => {
  let tempDir: string | null = null;
  let stopServer: (() => void) | null = null;
  let baseUrl = "";
  const originalFetch = globalThis.fetch;

  async function startTestServer(options?: { feedbackLogger?: FeedbackLogger }) {
    tempDir = mkdtempSync(path.join(os.tmpdir(), "routing-endpoint-"));
    const stateFilePath = path.join(tempDir, "workers.json");
    await writeStateFile(stateFilePath, { workers: {}, crashHistory: {} });

    const { server, stop } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      legionId,
      legionDir: tempDir,
      adapter: makeAdapter(),
      stateFilePath,
      feedbackLogger: options?.feedbackLogger,
    });
    stopServer = stop;
    baseUrl = `http://127.0.0.1:${server.port}`;
  }

  async function postRouting(body: unknown) {
    return originalFetch(`${baseUrl}/routing/match`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    });
  }

  afterEach(async () => {
    globalThis.fetch = originalFetch;
    if (stopServer) {
      stopServer();
      stopServer = null;
    }
    if (tempDir) {
      rmSync(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  it("returns matched reviewers for files matching a domain", async () => {
    await startTestServer();
    const workspace = mkdtempSync(path.join(os.tmpdir(), "routing-ws-"));
    writeConfig(workspace, VALID_CONFIG);

    try {
      const response = await postRouting({
        workspace,
        files: ["packages/envoy/src/foo.ts"],
      });
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        reviewers: string[];
        matchedDomains: { name: string; reviewers: string[] }[];
        configWarning: string | null;
      };
      expect(body.reviewers).toEqual(["envoy-expert"]);
      expect(body.matchedDomains).toHaveLength(1);
      expect(body.matchedDomains[0].name).toBe("envoy");
      expect(body.configWarning).toBeNull();
    } finally {
      rmSync(workspace, { recursive: true, force: true });
    }
  });

  it("returns empty result when no config file exists", async () => {
    await startTestServer();
    const workspace = mkdtempSync(path.join(os.tmpdir(), "routing-ws-"));

    try {
      const response = await postRouting({
        workspace,
        files: ["packages/envoy/src/foo.ts"],
      });
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        reviewers: string[];
        matchedDomains: unknown[];
        configWarning: string | null;
      };
      expect(body.reviewers).toEqual([]);
      expect(body.matchedDomains).toEqual([]);
      expect(body.configWarning).toBeNull();
    } finally {
      rmSync(workspace, { recursive: true, force: true });
    }
  });

  it("returns empty result with warning for invalid config", async () => {
    await startTestServer();
    const workspace = mkdtempSync(path.join(os.tmpdir(), "routing-ws-"));
    writeConfig(workspace, "domains: []");

    try {
      const response = await postRouting({
        workspace,
        files: ["packages/envoy/src/foo.ts"],
      });
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        reviewers: string[];
        matchedDomains: unknown[];
        configWarning: string | null;
      };
      expect(body.reviewers).toEqual([]);
      expect(body.matchedDomains).toEqual([]);
      expect(body.configWarning).toContain("Invalid routing config");
    } finally {
      rmSync(workspace, { recursive: true, force: true });
    }
  });

  it("returns empty reviewers when no files match", async () => {
    await startTestServer();
    const workspace = mkdtempSync(path.join(os.tmpdir(), "routing-ws-"));
    writeConfig(workspace, VALID_CONFIG);

    try {
      const response = await postRouting({
        workspace,
        files: ["README.md", "package.json"],
      });
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        reviewers: string[];
        matchedDomains: unknown[];
      };
      expect(body.reviewers).toEqual([]);
      expect(body.matchedDomains).toEqual([]);
    } finally {
      rmSync(workspace, { recursive: true, force: true });
    }
  });

  it("returns 400 for missing workspace", async () => {
    await startTestServer();
    const response = await postRouting({ files: ["foo.ts"] });
    expect(response.status).toBe(400);
  });

  it("returns 400 for non-absolute workspace", async () => {
    await startTestServer();
    const response = await postRouting({
      workspace: "relative/path",
      files: ["foo.ts"],
    });
    expect(response.status).toBe(400);
  });

  it("returns 400 for missing files array", async () => {
    await startTestServer();
    const response = await postRouting({ workspace: "/tmp/ws" });
    expect(response.status).toBe(400);
  });

  it("returns 400 for non-string files", async () => {
    await startTestServer();
    const response = await postRouting({
      workspace: "/tmp/ws",
      files: [123, 456],
    });
    expect(response.status).toBe(400);
  });

  it("logs feedback event when issueId is provided", async () => {
    const writer = new RecordingFeedbackWriter();
    const logger = new FeedbackLogger(writer, legionId);
    await startTestServer({ feedbackLogger: logger });

    const workspace = mkdtempSync(path.join(os.tmpdir(), "routing-ws-"));
    writeConfig(workspace, VALID_CONFIG);

    try {
      const response = await postRouting({
        workspace,
        files: ["packages/envoy/src/foo.ts"],
        issueId: "sjawhar-legion-316",
      });
      expect(response.status).toBe(200);

      await logger.flush();
      expect(writer.lines.length).toBeGreaterThanOrEqual(1);
      const event = JSON.parse(writer.lines[0]);
      expect(event.event).toBe("routing.matched");
      expect(event.issueId).toBe("sjawhar-legion-316");
      expect(event.reviewersAdded).toEqual(["envoy-expert"]);
      expect(event.matchedDomains).toHaveLength(1);
    } finally {
      rmSync(workspace, { recursive: true, force: true });
    }
  });

  it("does not log feedback when issueId is not provided", async () => {
    const writer = new RecordingFeedbackWriter();
    const logger = new FeedbackLogger(writer, legionId);
    await startTestServer({ feedbackLogger: logger });

    const workspace = mkdtempSync(path.join(os.tmpdir(), "routing-ws-"));
    writeConfig(workspace, VALID_CONFIG);

    try {
      await postRouting({
        workspace,
        files: ["packages/envoy/src/foo.ts"],
      });

      await logger.flush();
      expect(writer.lines).toHaveLength(0);
    } finally {
      rmSync(workspace, { recursive: true, force: true });
    }
  });

  it("matches multiple domains and deduplicates reviewers", async () => {
    await startTestServer();
    const workspace = mkdtempSync(path.join(os.tmpdir(), "routing-ws-"));
    writeConfig(workspace, VALID_CONFIG);

    try {
      const response = await postRouting({
        workspace,
        files: ["packages/envoy/src/foo.ts", "packages/daemon/src/bar.ts"],
      });
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        reviewers: string[];
        matchedDomains: { name: string; reviewers: string[] }[];
      };
      expect(body.reviewers).toContain("envoy-expert");
      expect(body.reviewers).toContain("daemon-expert");
      expect(body.matchedDomains).toHaveLength(2);
    } finally {
      rmSync(workspace, { recursive: true, force: true });
    }
  });
});
