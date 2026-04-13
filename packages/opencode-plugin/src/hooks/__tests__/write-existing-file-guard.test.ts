import { afterEach, beforeEach, describe, expect, it, spyOn } from "bun:test";
import * as fs from "node:fs";
import { createWriteExistingFileGuard } from "../write-existing-file-guard";

// ─── Helpers ─────────────────────────────────────────────────────────────────

function makeBeforeInput(tool: string, sessionID: string, args: Record<string, unknown> = {}) {
  return { tool, sessionID, callID: "c-1", args };
}

function makeBeforeOutput(args: Record<string, unknown> = {}) {
  return { args };
}

function makeAfterInput(tool: string, sessionID: string, args: Record<string, unknown> = {}) {
  return { tool, sessionID, callID: "c-1", args };
}

function makeAfterOutput(output = "", metadata: unknown = {}) {
  return { title: "", output, metadata };
}

// ─── Tests ───────────────────────────────────────────────────────────────────

describe("createWriteExistingFileGuard", () => {
  let existsSyncSpy: ReturnType<typeof spyOn>;

  beforeEach(() => {
    // Default: all files "exist" on disk
    existsSyncSpy = spyOn(fs, "existsSync").mockReturnValue(true);
  });

  afterEach(() => {
    existsSyncSpy.mockRestore();
  });

  describe("core: write to existing file NOT read → warning", () => {
    it("adds a warning when writing to an existing file that was not read", () => {
      const guard = createWriteExistingFileGuard();
      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });

      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      // Should NOT throw (soft warning only)
      // But output.args should contain a warning
      expect(output.args.__warning).toBeDefined();
      expect(output.args.__warning).toMatch(/not been read/i);
    });

    it("adds a warning when editing an existing file that was not read", () => {
      const guard = createWriteExistingFileGuard();
      const output = makeBeforeOutput({
        filePath: "/src/bar.ts",
        oldString: "a",
        newString: "b",
      });

      guard["tool.execute.before"](
        makeBeforeInput("edit", "s-1", { filePath: "/src/bar.ts" }),
        output
      );

      expect(output.args.__warning).toBeDefined();
      expect(output.args.__warning).toMatch(/not been read/i);
    });

    it("does NOT throw — soft warning only", () => {
      const guard = createWriteExistingFileGuard();

      expect(() =>
        guard["tool.execute.before"](
          makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
          makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" })
        )
      ).not.toThrow();
    });
  });

  describe("core: write to existing file that WAS read → no warning", () => {
    it("passes cleanly when file was read before writing", () => {
      const guard = createWriteExistingFileGuard();

      // Read the file first (tracked via tool.execute.after)
      guard["tool.execute.after"](
        makeAfterInput("read", "s-1", { filePath: "/src/foo.ts" }),
        makeAfterOutput("file contents")
      );

      // Now write — should pass without warning
      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeUndefined();
    });

    it("glob does NOT count as reading — writing after glob still warns", () => {
      const guard = createWriteExistingFileGuard();

      // Glob returns file paths but does NOT read their content
      guard["tool.execute.after"](
        makeAfterInput("glob", "s-1", { pattern: "**/*.ts" }),
        makeAfterOutput("/src/foo.ts\n/src/bar.ts")
      );

      // Write to a globbed file — should warn because glob != read
      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeDefined();
    });

    it("tracks reads from the Read tool", () => {
      const guard = createWriteExistingFileGuard();

      guard["tool.execute.after"](
        makeAfterInput("read", "s-1", { filePath: "/src/foo.ts" }),
        makeAfterOutput("file contents here")
      );

      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "new" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeUndefined();
    });

    it("tracks reads from the Grep tool (searching file contents)", () => {
      const guard = createWriteExistingFileGuard();

      // Grep returns file paths that matched — agent has seen these files
      guard["tool.execute.after"](
        makeAfterInput("grep", "s-1", { pattern: "foo", path: "/src" }),
        makeAfterOutput("/src/foo.ts:10: const foo = 1;\n/src/bar.ts:5: foo()")
      );

      // Grep doesn't fully "read" a file — it searches but the agent hasn't
      // seen the full content. Per the issue: "Track which files have been Read"
      // Only the Read tool counts as a "read".
      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "new" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      // Grep alone shouldn't count as reading
      expect(output.args.__warning).toBeDefined();
    });
  });

  describe("core: write to new file → no warning", () => {
    it("passes cleanly when writing a file that does not exist on disk", () => {
      existsSyncSpy.mockReturnValue(false);

      const guard = createWriteExistingFileGuard();
      const output = makeBeforeOutput({
        filePath: "/src/new-file.ts",
        content: "x",
      });

      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/new-file.ts" }),
        output
      );

      expect(output.args.__warning).toBeUndefined();
    });
  });

  describe("per-session isolation", () => {
    it("read in session s-1 does NOT satisfy write in session s-2", () => {
      const guard = createWriteExistingFileGuard();

      // s-1 reads the file
      guard["tool.execute.after"](
        makeAfterInput("read", "s-1", { filePath: "/src/foo.ts" }),
        makeAfterOutput("contents")
      );

      // s-2 writes the same file without reading — should warn
      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-2", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeDefined();
    });

    it("each session tracks reads independently", () => {
      const guard = createWriteExistingFileGuard();

      // Both sessions read the file
      guard["tool.execute.after"](
        makeAfterInput("read", "s-1", { filePath: "/src/foo.ts" }),
        makeAfterOutput("contents")
      );
      guard["tool.execute.after"](
        makeAfterInput("read", "s-2", { filePath: "/src/foo.ts" }),
        makeAfterOutput("contents")
      );

      // Both can write without warning
      const out1 = makeBeforeOutput({ filePath: "/src/foo.ts", content: "a" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        out1
      );
      expect(out1.args.__warning).toBeUndefined();

      const out2 = makeBeforeOutput({ filePath: "/src/foo.ts", content: "b" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-2", { filePath: "/src/foo.ts" }),
        out2
      );
      expect(out2.args.__warning).toBeUndefined();
    });
  });

  describe("session cleanup on session.deleted", () => {
    it("clears file tracking when session is deleted", async () => {
      const guard = createWriteExistingFileGuard();

      // s-1 reads a file
      guard["tool.execute.after"](
        makeAfterInput("read", "s-1", { filePath: "/src/foo.ts" }),
        makeAfterOutput("contents")
      );

      // Delete s-1
      await guard.event({
        event: {
          type: "session.deleted",
          properties: { sessionID: "s-1" },
        },
      });

      // s-1 writes — should warn because tracking was cleared
      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeDefined();
    });

    it("handles session.deleted with info.id format", async () => {
      const guard = createWriteExistingFileGuard();

      guard["tool.execute.after"](
        makeAfterInput("read", "s-1", { filePath: "/src/foo.ts" }),
        makeAfterOutput("contents")
      );

      await guard.event({
        event: {
          type: "session.deleted",
          properties: { info: { id: "s-1" } },
        },
      });

      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeDefined();
    });

    it("does not affect other sessions when one is deleted", async () => {
      const guard = createWriteExistingFileGuard();

      // Both sessions read
      guard["tool.execute.after"](
        makeAfterInput("read", "s-1", { filePath: "/src/foo.ts" }),
        makeAfterOutput("contents")
      );
      guard["tool.execute.after"](
        makeAfterInput("read", "s-2", { filePath: "/src/foo.ts" }),
        makeAfterOutput("contents")
      );

      // Delete s-1
      await guard.event({
        event: {
          type: "session.deleted",
          properties: { sessionID: "s-1" },
        },
      });

      // s-2 can still write without warning
      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-2", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeUndefined();
    });

    it("ignores non-session.deleted events", async () => {
      const guard = createWriteExistingFileGuard();

      guard["tool.execute.after"](
        makeAfterInput("read", "s-1", { filePath: "/src/foo.ts" }),
        makeAfterOutput("contents")
      );

      await guard.event({
        event: {
          type: "session.created",
          properties: { sessionID: "s-1" },
        },
      });

      // File tracking should still be intact
      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeUndefined();
    });

    it("handles session.deleted for unknown session without error", async () => {
      const guard = createWriteExistingFileGuard();

      await expect(
        guard.event({
          event: {
            type: "session.deleted",
            properties: { sessionID: "unknown" },
          },
        })
      ).resolves.toBeUndefined();
    });
  });

  describe("edge cases", () => {
    it("ignores non-write/edit tools in tool.execute.before", () => {
      const guard = createWriteExistingFileGuard();

      const output = makeBeforeOutput({ command: "ls" });
      guard["tool.execute.before"](makeBeforeInput("bash", "s-1", { command: "ls" }), output);

      expect(output.args.__warning).toBeUndefined();
    });

    it("ignores non-read tools in tool.execute.after", () => {
      const guard = createWriteExistingFileGuard();

      // bash tool should not register as a "read"
      guard["tool.execute.after"](
        makeAfterInput("bash", "s-1", { command: "cat /src/foo.ts" }),
        makeAfterOutput("file contents")
      );

      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeDefined();
    });

    it("handles missing filePath gracefully (no-op)", () => {
      const guard = createWriteExistingFileGuard();

      // Write with no filePath — should not crash
      const output = makeBeforeOutput({ content: "x" });
      expect(() =>
        guard["tool.execute.before"](makeBeforeInput("write", "s-1", { content: "x" }), output)
      ).not.toThrow();

      expect(output.args.__warning).toBeUndefined();
    });

    it("handles missing sessionID gracefully (no-op)", () => {
      const guard = createWriteExistingFileGuard();

      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      expect(() =>
        guard["tool.execute.before"](
          { tool: "write", callID: "c-1", args: {}, sessionID: "" },
          output
        )
      ).not.toThrow();
    });

    it("normalizes file paths to avoid double-counting", () => {
      const guard = createWriteExistingFileGuard();

      // Read with one path format
      guard["tool.execute.after"](
        makeAfterInput("read", "s-1", { filePath: "/src/./foo.ts" }),
        makeAfterOutput("contents")
      );

      // Write with normalized path — should still match
      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeUndefined();
    });

    it("warning includes the file path for context", () => {
      const guard = createWriteExistingFileGuard();

      const output = makeBeforeOutput({
        filePath: "/src/important-module.ts",
        content: "x",
      });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", {
          filePath: "/src/important-module.ts",
        }),
        output
      );

      expect(output.args.__warning).toContain("/src/important-module.ts");
    });

    it("handles the mcp_write tool name (alias)", () => {
      const guard = createWriteExistingFileGuard();

      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      guard["tool.execute.before"](
        makeBeforeInput("mcp_write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      // mcp_write is still a write tool — should warn
      expect(output.args.__warning).toBeDefined();
    });

    it("handles the mcp_edit tool name (alias)", () => {
      const guard = createWriteExistingFileGuard();

      const output = makeBeforeOutput({
        filePath: "/src/foo.ts",
        oldString: "a",
        newString: "b",
      });
      guard["tool.execute.before"](
        makeBeforeInput("mcp_edit", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeDefined();
    });

    it("handles the mcp_read tool name (alias)", () => {
      const guard = createWriteExistingFileGuard();

      guard["tool.execute.after"](
        makeAfterInput("mcp_read", "s-1", { filePath: "/src/foo.ts" }),
        makeAfterOutput("contents")
      );

      const output = makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        output
      );

      expect(output.args.__warning).toBeUndefined();
    });

    it("allows multiple writes after a single read", () => {
      const guard = createWriteExistingFileGuard();

      guard["tool.execute.after"](
        makeAfterInput("read", "s-1", { filePath: "/src/foo.ts" }),
        makeAfterOutput("contents")
      );

      // First write — clean
      const out1 = makeBeforeOutput({ filePath: "/src/foo.ts", content: "v1" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        out1
      );
      expect(out1.args.__warning).toBeUndefined();

      // Second write — still clean (read is persistent for the session)
      const out2 = makeBeforeOutput({ filePath: "/src/foo.ts", content: "v2" });
      guard["tool.execute.before"](
        makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
        out2
      );
      expect(out2.args.__warning).toBeUndefined();
    });

    it("logs a console.warn when adding the write-guard warning", () => {
      const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
      try {
        const guard = createWriteExistingFileGuard();

        guard["tool.execute.before"](
          makeBeforeInput("write", "s-1", { filePath: "/src/foo.ts" }),
          makeBeforeOutput({ filePath: "/src/foo.ts", content: "x" })
        );

        expect(warnSpy).toHaveBeenCalledTimes(1);
        expect(warnSpy.mock.calls[0][0]).toMatch(/write.*guard/i);
      } finally {
        warnSpy.mockRestore();
      }
    });
  });

  // ─── Smoke Tests: Real-World Agent Workflow Scenarios ─────────────────────
  // These simulate actual sequences that occur during Legion sessions where
  // agents read, edit, and write files in realistic patterns.

  describe("smoke: real-world agent workflows", () => {
    it("typical implement workflow: read → edit → write cycle", () => {
      const guard = createWriteExistingFileGuard();

      // Agent reads existing source file to understand it
      guard["tool.execute.after"](
        makeAfterInput("read", "ses-impl-42", { filePath: "/src/hooks/my-hook.ts" }),
        makeAfterOutput("export function myHook() { return 42; }")
      );

      // Agent reads test file
      guard["tool.execute.after"](
        makeAfterInput("read", "ses-impl-42", {
          filePath: "/src/hooks/__tests__/my-hook.test.ts",
        }),
        makeAfterOutput('describe("myHook", () => { it("works", () => {}); });')
      );

      // Agent edits source file (after reading) — should pass cleanly
      const editOut = makeBeforeOutput({
        filePath: "/src/hooks/my-hook.ts",
        oldString: "return 42",
        newString: "return 43",
      });
      guard["tool.execute.before"](
        makeBeforeInput("edit", "ses-impl-42", {
          filePath: "/src/hooks/my-hook.ts",
        }),
        editOut
      );
      expect(editOut.args.__warning).toBeUndefined();

      // Agent writes new test file — should pass (new file)
      existsSyncSpy.mockImplementation((p: fs.PathLike) => {
        if (String(p).includes("new-test")) return false;
        return true;
      });
      const newTestOut = makeBeforeOutput({
        filePath: "/src/hooks/__tests__/new-test.test.ts",
        content: 'test("new", () => {});',
      });
      guard["tool.execute.before"](
        makeBeforeInput("write", "ses-impl-42", {
          filePath: "/src/hooks/__tests__/new-test.test.ts",
        }),
        newTestOut
      );
      expect(newTestOut.args.__warning).toBeUndefined();
    });

    it("parallel sessions: implementer and tester don't share read state", () => {
      const guard = createWriteExistingFileGuard();

      // Implementer reads and edits src/server.ts
      guard["tool.execute.after"](
        makeAfterInput("read", "ses-impl", { filePath: "/src/server.ts" }),
        makeAfterOutput("server code")
      );

      // Tester session writes to same file WITHOUT reading — should warn
      const testerOut = makeBeforeOutput({
        filePath: "/src/server.ts",
        content: "modified",
      });
      guard["tool.execute.before"](
        makeBeforeInput("write", "ses-tester", {
          filePath: "/src/server.ts",
        }),
        testerOut
      );
      expect(testerOut.args.__warning).toBeDefined();

      // Implementer writes to same file — should pass (they read it)
      const implOut = makeBeforeOutput({
        filePath: "/src/server.ts",
        content: "updated",
      });
      guard["tool.execute.before"](
        makeBeforeInput("write", "ses-impl", { filePath: "/src/server.ts" }),
        implOut
      );
      expect(implOut.args.__warning).toBeUndefined();
    });

    it("session lifecycle: track reads, delete session, re-create", async () => {
      const guard = createWriteExistingFileGuard();
      const sessionID = "ses-lifecycle";

      // Phase 1: Read and write (clean)
      guard["tool.execute.after"](
        makeAfterInput("read", sessionID, { filePath: "/src/config.ts" }),
        makeAfterOutput("config code")
      );
      const out1 = makeBeforeOutput({ filePath: "/src/config.ts", content: "v1" });
      guard["tool.execute.before"](
        makeBeforeInput("write", sessionID, { filePath: "/src/config.ts" }),
        out1
      );
      expect(out1.args.__warning).toBeUndefined();

      // Phase 2: Session deleted (cleanup)
      await guard.event({
        event: { type: "session.deleted", properties: { sessionID } },
      });

      // Phase 3: New session with same ID — read state is gone
      const out2 = makeBeforeOutput({ filePath: "/src/config.ts", content: "v2" });
      guard["tool.execute.before"](
        makeBeforeInput("write", sessionID, { filePath: "/src/config.ts" }),
        out2
      );
      expect(out2.args.__warning).toBeDefined();
    });

    it("many files read then edited in a large refactoring session", () => {
      const guard = createWriteExistingFileGuard();
      const session = "ses-refactor";
      const files = [
        "/src/hooks/utils.ts",
        "/src/hooks/circuit-breaker.ts",
        "/src/hooks/output-compression.ts",
        "/src/index.ts",
        "/src/config.ts",
      ];

      // Read all files
      for (const f of files) {
        guard["tool.execute.after"](
          makeAfterInput("read", session, { filePath: f }),
          makeAfterOutput(`contents of ${f}`)
        );
      }

      // Edit all files — should all pass without warning
      for (const f of files) {
        const out = makeBeforeOutput({ filePath: f, oldString: "old", newString: "new" });
        guard["tool.execute.before"](makeBeforeInput("edit", session, { filePath: f }), out);
        expect(out.args.__warning).toBeUndefined();
      }

      // Write to an unread file — should warn
      const unreadOut = makeBeforeOutput({
        filePath: "/src/hooks/background-notification.ts",
        content: "replaced",
      });
      guard["tool.execute.before"](
        makeBeforeInput("write", session, {
          filePath: "/src/hooks/background-notification.ts",
        }),
        unreadOut
      );
      expect(unreadOut.args.__warning).toBeDefined();
    });

    it("mcp tool names work correctly in realistic flow", () => {
      const guard = createWriteExistingFileGuard();
      const session = "ses-mcp";

      // Agent uses mcp_read (OpenCode MCP bridge tool name)
      guard["tool.execute.after"](
        makeAfterInput("mcp_read", session, { filePath: "/src/main.ts" }),
        makeAfterOutput("main code")
      );

      // Agent uses mcp_edit — should pass (file was read via mcp_read)
      const out = makeBeforeOutput({
        filePath: "/src/main.ts",
        oldString: "old",
        newString: "new",
      });
      guard["tool.execute.before"](
        makeBeforeInput("mcp_edit", session, { filePath: "/src/main.ts" }),
        out
      );
      expect(out.args.__warning).toBeUndefined();
    });
  });

  describe("createReadFileRegistry export", () => {
    it("is exported from the module for use by other hooks", async () => {
      const mod = await import("../write-existing-file-guard");
      expect(mod.createReadFileRegistry).toBeDefined();
      expect(typeof mod.createReadFileRegistry).toBe("function");
    });

    it("registry tracks files per session", () => {
      const { createReadFileRegistry } = require("../write-existing-file-guard");
      const registry = createReadFileRegistry();

      registry.trackRead("s-1", "/src/foo.ts");
      registry.trackRead("s-1", "/src/bar.ts");

      expect(registry.hasRead("s-1", "/src/foo.ts")).toBe(true);
      expect(registry.hasRead("s-1", "/src/bar.ts")).toBe(true);
      expect(registry.hasRead("s-1", "/src/baz.ts")).toBe(false);
      expect(registry.hasRead("s-2", "/src/foo.ts")).toBe(false);
    });

    it("registry cleanup removes all files for a session", () => {
      const { createReadFileRegistry } = require("../write-existing-file-guard");
      const registry = createReadFileRegistry();

      registry.trackRead("s-1", "/src/foo.ts");
      registry.trackRead("s-1", "/src/bar.ts");
      registry.cleanup("s-1");

      expect(registry.hasRead("s-1", "/src/foo.ts")).toBe(false);
      expect(registry.hasRead("s-1", "/src/bar.ts")).toBe(false);
    });
  });
});
