import { afterEach, beforeEach, describe, expect, it, mock } from "bun:test";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";

import { createLogger } from "../log";

let logDir: string;

beforeEach(async () => {
  logDir = await mkdtemp(path.join(tmpdir(), "envoy-plugin-log-"));
});

afterEach(async () => {
  await rm(logDir, { recursive: true, force: true });
});

describe("envoy-plugin logger", () => {
  // The whole point of this module: plugins run in-process with OpenCode's
  // TUI, so any console.warn/console.error byte goes straight to the terminal
  // and corrupts the render. Plugin diagnostics must go to a file instead.
  it("writes warn/error/info to the configured log file, NOT to console", async () => {
    const consoleWarn = mock(() => {});
    const consoleError = mock(() => {});
    const consoleLog = mock(() => {});
    const originalWarn = console.warn;
    const originalError = console.error;
    const originalLog = console.log;
    console.warn = consoleWarn;
    console.error = consoleError;
    console.log = consoleLog;
    try {
      const logger = createLogger({ logDir });
      logger.warn("a-warn");
      logger.error("an-error");
      logger.info("some-info");
      await logger.flush();

      expect(consoleWarn).not.toHaveBeenCalled();
      expect(consoleError).not.toHaveBeenCalled();
      expect(consoleLog).not.toHaveBeenCalled();

      const logFile = path.join(logDir, "envoy-plugin.log");
      const contents = await readFile(logFile, "utf-8");
      expect(contents).toContain("a-warn");
      expect(contents).toContain("an-error");
      expect(contents).toContain("some-info");
      expect(contents).toContain("WARN");
      expect(contents).toContain("ERROR");
      expect(contents).toContain("INFO");
    } finally {
      console.warn = originalWarn;
      console.error = originalError;
      console.log = originalLog;
    }
  });

  it("creates the log directory if it doesn't exist", async () => {
    const nested = path.join(logDir, "deeply", "nested");
    const logger = createLogger({ logDir: nested });
    logger.warn("hello");
    await logger.flush();
    const contents = await readFile(path.join(nested, "envoy-plugin.log"), "utf-8");
    expect(contents).toContain("hello");
  });

  it("prefixes each line with an ISO timestamp so logs are sortable", async () => {
    const logger = createLogger({ logDir });
    logger.warn("x");
    await logger.flush();
    const contents = await readFile(path.join(logDir, "envoy-plugin.log"), "utf-8");
    // Match leading ISO-8601 datetime like 2026-05-30T22:35:01.123Z
    expect(contents).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z /);
  });

  it("appends across calls instead of truncating", async () => {
    const logger = createLogger({ logDir });
    logger.warn("first");
    logger.warn("second");
    await logger.flush();
    const contents = await readFile(path.join(logDir, "envoy-plugin.log"), "utf-8");
    expect(contents).toContain("first");
    expect(contents).toContain("second");
    expect(contents.split("\n").filter((l) => l.length > 0).length).toBe(2);
  });

  it("never throws on logging errors — diagnostics must not crash the host", async () => {
    // Point at a path that cannot be created (a parent that exists as a file).
    const blockingFile = path.join(logDir, "blocker");
    const fs = await import("node:fs/promises");
    await fs.writeFile(blockingFile, "");
    const logger = createLogger({ logDir: path.join(blockingFile, "child") });
    expect(() => logger.warn("x")).not.toThrow();
    await expect(logger.flush()).resolves.toBeUndefined();
  });
});

describe("no console.* in plugin source (regression guard)", () => {
  // Plugins load in-process with OpenCode's TUI; any direct console.warn /
  // console.error / console.log write goes straight to the terminal and
  // corrupts the render. All diagnostics must route through the file logger.
  it("does not write to console anywhere outside __tests__ and log.ts", async () => {
    const { readdir, readFile, stat } = await import("node:fs/promises");
    const root = path.resolve(import.meta.dir, "..");

    async function walk(dir: string): Promise<string[]> {
      const entries = await readdir(dir);
      const out: string[] = [];
      for (const name of entries) {
        if (name === "__tests__" || name === "node_modules") continue;
        const full = path.join(dir, name);
        const s = await stat(full);
        if (s.isDirectory()) out.push(...(await walk(full)));
        else if (name.endsWith(".ts") || name.endsWith(".tsx")) out.push(full);
      }
      return out;
    }

    const files = await walk(root);
    const offenders: Array<{ file: string; line: number; text: string }> = [];
    for (const file of files) {
      // log.ts is allowed to mention console in its module docstring — it's
      // the module explaining the prohibition.
      if (file.endsWith("/log.ts")) continue;
      const lines = (await readFile(file, "utf-8")).split("\n");
      lines.forEach((text, idx) => {
        if (/^\s*(?:\/\/|\*| \*)/.test(text)) return; // comments are fine
        if (/\bconsole\.(warn|error|log)\s*\(/.test(text)) {
          offenders.push({ file: path.relative(root, file), line: idx + 1, text });
        }
      });
    }
    expect(offenders).toEqual([]);
  });
});
