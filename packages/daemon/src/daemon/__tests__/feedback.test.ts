import { afterEach, describe, expect, it, mock } from "bun:test";
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import {
  type FeedbackEvent,
  FeedbackEventSchema,
  FeedbackLogger,
  type FeedbackWriter,
  FileFeedbackWriter,
} from "../feedback";

class RecordingWriter implements FeedbackWriter {
  lines: string[] = [];
  flushCalls = 0;

  constructor(
    private readonly options: {
      failAppendCount?: number;
      onAppend?: (line: string) => Promise<void> | void;
    } = {}
  ) {}

  async append(line: string): Promise<void> {
    const nextCount = this.lines.length + 1;
    if (this.options.failAppendCount !== undefined && nextCount === this.options.failAppendCount) {
      throw new Error("append failed");
    }
    await this.options.onAppend?.(line);
    this.lines.push(line);
  }

  async flush(): Promise<void> {
    this.flushCalls += 1;
  }
}

function validEvents(): FeedbackEvent[] {
  return [
    {
      schemaVersion: 1,
      timestamp: "2026-04-04T12:00:00.000Z",
      legionId: "team/project",
      event: "worker.dispatched",
      issueId: "ENG-21",
      mode: "implement",
      workerId: "eng-21-implement",
      sessionId: "ses_123",
      version: 2,
      workspace: "/tmp/workspace",
      crashCount: 0,
    },
    {
      schemaVersion: 1,
      timestamp: "2026-04-04T12:00:00.000Z",
      legionId: "team/project",
      event: "worker.status_changed",
      workerId: "eng-21-implement",
      issueId: "ENG-21",
      mode: "implement",
      sessionId: "ses_123",
      version: 2,
      fromStatus: "starting",
      toStatus: "running",
      crashCount: 0,
      uptimeMs: 250,
    },
    {
      schemaVersion: 1,
      timestamp: "2026-04-04T12:00:00.000Z",
      legionId: "team/project",
      event: "state.collected",
      issueId: "ENG-21",
      status: "Todo",
      suggestedAction: "dispatch_worker",
      hasLiveWorker: true,
      workerMode: "implement",
      workerStatus: "running",
      hasPr: true,
      prIsDraft: false,
      ciStatus: "success",
      mergeableStatus: "MERGEABLE",
      labels: ["worker-active"],
    },
    {
      schemaVersion: 1,
      timestamp: "2026-04-04T12:00:00.000Z",
      legionId: "team/project",
      event: "daemon.health_tick",
      tick: 3,
      workerCount: 2,
      serveHealthy: true,
      uptimeS: 60,
      serveRestarted: false,
      sessionsRecreated: 0,
      rssRestarts: 0,
    },
  ];
}

describe("FeedbackEventSchema", () => {
  it("accepts valid events for every event type", () => {
    for (const event of validEvents()) {
      const result = FeedbackEventSchema.safeParse(event);
      expect(result.success).toBe(true);
    }
  });

  it("rejects events with missing or invalid fields", () => {
    const missingSessionId = FeedbackEventSchema.safeParse({
      schemaVersion: 1,
      timestamp: "2026-04-04T12:00:00.000Z",
      legionId: "team/project",
      event: "worker.dispatched",
      issueId: "ENG-21",
      mode: "implement",
      workerId: "eng-21-implement",
      version: 0,
      workspace: "/tmp/workspace",
      crashCount: 0,
    });

    const wrongVersion = FeedbackEventSchema.safeParse({
      schemaVersion: 1,
      timestamp: "2026-04-04T12:00:00.000Z",
      legionId: "team/project",
      event: "worker.status_changed",
      workerId: "eng-21-implement",
      issueId: "ENG-21",
      mode: "implement",
      sessionId: "ses_123",
      version: -1,
      fromStatus: "starting",
      toStatus: "running",
      crashCount: 0,
      uptimeMs: 100,
    });

    expect(missingSessionId.success).toBe(false);
    expect(wrongVersion.success).toBe(false);
  });
});

describe("FeedbackLogger", () => {
  let originalConsoleError: typeof console.error = console.error;

  afterEach(() => {
    console.error = originalConsoleError;
  });

  it("enqueues and drains valid events to the writer", async () => {
    const writer = new RecordingWriter();
    const logger = new FeedbackLogger(writer, "team/project");

    logger.log({
      event: "worker.dispatched",
      issueId: "ENG-21",
      mode: "implement",
      workerId: "eng-21-implement",
      sessionId: "ses_123",
      version: 1,
      workspace: "/tmp/workspace",
      crashCount: 0,
    });

    await logger.flush();

    expect(writer.lines).toHaveLength(1);
    const parsed = JSON.parse(writer.lines[0]) as FeedbackEvent;
    expect(parsed.schemaVersion).toBe(1);
    expect(parsed.legionId).toBe("team/project");
    expect(parsed.event).toBe("worker.dispatched");
  });

  it("drops invalid events and logs an error", async () => {
    originalConsoleError = console.error;
    const consoleError = mock(() => {});
    console.error = consoleError;

    const writer = new RecordingWriter();
    const logger = new FeedbackLogger(writer, "team/project");

    logger.log({
      event: "worker.dispatched",
      issueId: "ENG-21",
      mode: "implement",
      workerId: "eng-21-implement",
      sessionId: "ses_123",
      version: -1,
      workspace: "/tmp/workspace",
      crashCount: 0,
    } as never);

    await logger.flush();

    expect(writer.lines).toHaveLength(0);
    expect(consoleError).toHaveBeenCalledTimes(1);
  });

  it("flush drains every queued event in order", async () => {
    const writer = new RecordingWriter();
    const logger = new FeedbackLogger(writer, "team/project");

    logger.log({
      event: "worker.dispatched",
      issueId: "ENG-21",
      mode: "implement",
      workerId: "eng-21-implement",
      sessionId: "ses_123",
      version: 1,
      workspace: "/tmp/workspace/one",
      crashCount: 0,
    });
    logger.log({
      event: "worker.dispatched",
      issueId: "ENG-22",
      mode: "test",
      workerId: "eng-22-test",
      sessionId: "ses_456",
      version: 2,
      workspace: "/tmp/workspace/two",
      crashCount: 1,
    });

    await logger.flush();

    expect(writer.lines).toHaveLength(2);
    expect((JSON.parse(writer.lines[0]) as FeedbackEvent).issueId).toBe("ENG-21");
    expect((JSON.parse(writer.lines[1]) as FeedbackEvent).issueId).toBe("ENG-22");
    expect(writer.flushCalls).toBe(1);
  });

  it("swallows writer failures and continues draining", async () => {
    originalConsoleError = console.error;
    const consoleError = mock(() => {});
    console.error = consoleError;

    const writer = new RecordingWriter({ failAppendCount: 1 });
    const logger = new FeedbackLogger(writer, "team/project");

    logger.log({
      event: "worker.dispatched",
      issueId: "ENG-21",
      mode: "implement",
      workerId: "eng-21-implement",
      sessionId: "ses_123",
      version: 1,
      workspace: "/tmp/workspace",
      crashCount: 0,
    });

    await expect(logger.flush()).resolves.toBeUndefined();
    expect(consoleError).toHaveBeenCalledTimes(1);
  });

  it("writes daemon.health_tick events", async () => {
    const writer = new RecordingWriter();
    const logger = new FeedbackLogger(writer, "team/project");

    logger.log({
      event: "daemon.health_tick",
      tick: 4,
      workerCount: 3,
      serveHealthy: true,
      uptimeS: 120,
      serveRestarted: false,
      sessionsRecreated: 1,
      rssRestarts: 0,
    });

    await logger.flush();

    const event = JSON.parse(writer.lines[0]) as FeedbackEvent;
    expect(event.event).toBe("daemon.health_tick");
    if (event.event === "daemon.health_tick") {
      expect(event.workerCount).toBe(3);
    }
  });
});

describe("FileFeedbackWriter", () => {
  let tempDir: string | null = null;

  afterEach(async () => {
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  it("appends JSONL lines to the target file", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "feedback-writer-"));
    const filePath = path.join(tempDir, "feedback.jsonl");
    const writer = new FileFeedbackWriter(filePath, 1024);

    await writer.append('{"event":"one"}');
    await writer.append('{"event":"two"}');

    const contents = await readFile(filePath, "utf8");
    expect(contents).toBe('{"event":"one"}\n{"event":"two"}\n');
  });

  it("rotates the current file into a single .1 backup when maxBytes is exceeded", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "feedback-writer-"));
    const filePath = path.join(tempDir, "feedback.jsonl");
    await writeFile(filePath, "1234567890");

    const writer = new FileFeedbackWriter(filePath, 5);
    await writer.append('{"event":"after-rotation"}');

    const backupPath = `${filePath}.1`;
    const backupContents = await readFile(backupPath, "utf8");
    const activeContents = await readFile(filePath, "utf8");
    const activeStats = await stat(filePath);

    expect(backupContents).toBe("1234567890");
    expect(activeContents).toBe('{"event":"after-rotation"}\n');
    expect(activeStats.size).toBe(activeContents.length);
  });

  it("keeps exactly one backup file during rotation", async () => {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "feedback-writer-"));
    const filePath = path.join(tempDir, "feedback.jsonl");
    const backupPath = `${filePath}.1`;
    await writeFile(filePath, "1234567890");
    await writeFile(backupPath, "stale-backup");

    const writer = new FileFeedbackWriter(filePath, 5);
    await writer.append('{"event":"fresh"}');

    const backupContents = await readFile(backupPath, "utf8");
    const missingSecondBackup = await stat(`${filePath}.2`).catch(() => null);

    expect(backupContents).toBe("1234567890");
    expect(missingSecondBackup).toBeNull();
  });
});
