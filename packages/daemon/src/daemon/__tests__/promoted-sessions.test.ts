import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  demoteSession,
  listPromotedSessions,
  promoteSession,
  readPromotedSessions,
  writePromotedSessions,
} from "../promoted-sessions";

describe("promoted-sessions", () => {
  let tempDir: string | null = null;

  afterEach(async () => {
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  function filePath(): string {
    return path.join(tempDir as string, "promoted.json");
  }

  describe("readPromotedSessions", () => {
    it("returns empty when file missing", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      const data = await readPromotedSessions(filePath());
      expect(data).toEqual({ sessions: {} });
    });

    it("returns empty when file is empty", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      await writeFile(filePath(), "", "utf-8");
      const data = await readPromotedSessions(filePath());
      expect(data).toEqual({ sessions: {} });
    });

    it("returns empty when file has invalid JSON", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      await writeFile(filePath(), "not json", "utf-8");
      const data = await readPromotedSessions(filePath());
      expect(data).toEqual({ sessions: {} });
    });

    it("returns empty when file has invalid schema", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      await writeFile(filePath(), JSON.stringify({ wrong: "shape" }), "utf-8");
      const data = await readPromotedSessions(filePath());
      expect(data).toEqual({ sessions: {} });
    });

    it("reads valid promoted sessions file", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      const content = {
        sessions: {
          ses_aabbccddee01ABCDEFghijkl: {
            sessionId: "ses_aabbccddee01ABCDEFghijkl",
            role: "legion-po",
            repo: "sjawhar/legion",
            promotedAt: "2026-04-16T00:00:00.000Z",
          },
        },
      };
      await writeFile(filePath(), JSON.stringify(content), "utf-8");
      const data = await readPromotedSessions(filePath());
      expect(data).toEqual(content);
    });
  });

  describe("writePromotedSessions", () => {
    it("writes and reads roundtrip", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      const data = {
        sessions: {
          ses_aabbccddee01ABCDEFghijkl: {
            sessionId: "ses_aabbccddee01ABCDEFghijkl",
            role: "legion-po",
            promotedAt: "2026-04-16T00:00:00.000Z",
          },
        },
      };
      await writePromotedSessions(filePath(), data);
      const result = await readPromotedSessions(filePath());
      expect(result).toEqual(data);
    });

    it("writes atomically without leaving temp files", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      await writePromotedSessions(filePath(), { sessions: {} });
      const entries = await readdir(tempDir);
      expect(entries).toEqual(["promoted.json"]);
    });

    it("creates parent directories", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      const nested = path.join(tempDir, "sub", "dir", "promoted.json");
      await writePromotedSessions(nested, { sessions: {} });
      const raw = await readFile(nested, "utf-8");
      expect(JSON.parse(raw)).toEqual({ sessions: {} });
    });
  });

  describe("promoteSession", () => {
    it("adds a session to empty file", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      const session = await promoteSession(
        filePath(),
        "ses_aabbccddee01ABCDEFghijkl",
        "legion-po",
        "sjawhar/legion"
      );
      expect(session.sessionId).toBe("ses_aabbccddee01ABCDEFghijkl");
      expect(session.role).toBe("legion-po");
      expect(session.repo).toBe("sjawhar/legion");
      expect(session.promotedAt).toBeTruthy();

      const data = await readPromotedSessions(filePath());
      expect(Object.keys(data.sessions)).toHaveLength(1);
      expect(data.sessions.ses_aabbccddee01ABCDEFghijkl).toEqual(session);
    });

    it("overwrites existing session with same ID", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      await promoteSession(filePath(), "ses_aabbccddee01ABCDEFghijkl", "old-role");
      const session = await promoteSession(filePath(), "ses_aabbccddee01ABCDEFghijkl", "new-role");
      expect(session.role).toBe("new-role");

      const data = await readPromotedSessions(filePath());
      expect(Object.keys(data.sessions)).toHaveLength(1);
    });

    it("adds multiple sessions", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      await promoteSession(filePath(), "ses_aabbccddee01ABCDEFghijkl", "legion-po");
      await promoteSession(filePath(), "ses_112233445566ABCDEFghijkl", "legion-reviewer");

      const data = await readPromotedSessions(filePath());
      expect(Object.keys(data.sessions)).toHaveLength(2);
    });

    it("omits repo when not provided", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      const session = await promoteSession(filePath(), "ses_aabbccddee01ABCDEFghijkl", "legion-po");
      expect(session.repo).toBeUndefined();
    });
  });

  describe("demoteSession", () => {
    it("removes an existing session", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      await promoteSession(filePath(), "ses_aabbccddee01ABCDEFghijkl", "legion-po");
      const removed = await demoteSession(filePath(), "ses_aabbccddee01ABCDEFghijkl");
      expect(removed).toBe(true);

      const data = await readPromotedSessions(filePath());
      expect(Object.keys(data.sessions)).toHaveLength(0);
    });

    it("returns false for non-existent session", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      const removed = await demoteSession(filePath(), "ses_aabbccddee01ABCDEFghijkl");
      expect(removed).toBe(false);
    });

    it("preserves other sessions", async () => {
      tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-"));
      await promoteSession(filePath(), "ses_aabbccddee01ABCDEFghijkl", "legion-po");
      await promoteSession(filePath(), "ses_112233445566ABCDEFghijkl", "legion-reviewer");

      await demoteSession(filePath(), "ses_aabbccddee01ABCDEFghijkl");

      const data = await readPromotedSessions(filePath());
      expect(Object.keys(data.sessions)).toHaveLength(1);
      expect(data.sessions.ses_112233445566ABCDEFghijkl).toBeTruthy();
    });
  });

  describe("listPromotedSessions", () => {
    it("returns empty array for empty file", () => {
      const result = listPromotedSessions({ sessions: {} });
      expect(result).toEqual([]);
    });

    it("returns all sessions as array", () => {
      const sessions = {
        ses_aabbccddee01ABCDEFghijkl: {
          sessionId: "ses_aabbccddee01ABCDEFghijkl",
          role: "legion-po",
          promotedAt: "2026-04-16T00:00:00.000Z",
        },
        ses_112233445566ABCDEFghijkl: {
          sessionId: "ses_112233445566ABCDEFghijkl",
          role: "legion-reviewer",
          promotedAt: "2026-04-16T01:00:00.000Z",
        },
      };
      const result = listPromotedSessions({ sessions });
      expect(result).toHaveLength(2);
      expect(result.map((s) => s.sessionId).sort()).toEqual([
        "ses_112233445566ABCDEFghijkl",
        "ses_aabbccddee01ABCDEFghijkl",
      ]);
    });
  });
});
