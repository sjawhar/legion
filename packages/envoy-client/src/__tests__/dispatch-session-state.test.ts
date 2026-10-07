import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, utimesSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { adviceMemory, resetAdviceMemory } from "../dispatch-execute";
import { forgetShownPictures, shownPictures } from "../dispatch-picture-tools";
import {
  appendResult,
  loadSessionMemory,
  pruneSessions,
  readResultsSince,
  saveSessionMemory,
  sessionDirectory,
  writePicture,
} from "../dispatch-session-state";

const root = () => mkdtempSync(join(tmpdir(), "dispatch-state-"));

describe("per-session state", () => {
  test("memory survives a process: what one call saved, the next call loads", () => {
    const env = { DISPATCH_STATE_DIR: root() };
    const dir = sessionDirectory(env, "s1");
    resetAdviceMemory();
    adviceMemory().add("triage:LEGION-2");
    shownPictures("s1").add("dispatch://LEGION-2/artifact/a@v1");
    saveSessionMemory(dir, "s1", new Set(["ask-1"]));
    resetAdviceMemory();
    forgetShownPictures("s1");
    const follows = loadSessionMemory(dir, "s1");
    expect(adviceMemory().has("triage:LEGION-2")).toBe(true);
    expect(shownPictures("s1").has("dispatch://LEGION-2/artifact/a@v1")).toBe(true);
    expect(follows.has("ask-1")).toBe(true);
  });

  test("a session id that could leave the state directory is refused", () => {
    expect(() => sessionDirectory({ DISPATCH_STATE_DIR: root() }, "../x")).toThrow();
  });

  test("the ledger returns only what was appended after an offset", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s2");
    appendResult(dir, { tool: "dispatch_search", details: {} });
    const first = readResultsSince(dir, 0);
    appendResult(dir, { tool: "dispatch_ask", details: { ask: "a" } });
    const second = readResultsSince(dir, first.offset);
    expect(first.entries.map((e) => e.tool)).toEqual(["dispatch_search"]);
    expect(second.entries.map((e) => e.tool)).toEqual(["dispatch_ask"]);
  });

  test("a picture is written once under its hash", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s3");
    const png = Buffer.from("89504e470d0a1a0a0000", "hex").toString("base64");
    const a = writePicture(dir, { data: png, mimeType: "image/png" });
    const b = writePicture(dir, { data: png, mimeType: "image/png" });
    expect(a.path).toBe(b.path);
    expect(a.path.endsWith(".png")).toBe(true);
    expect(readFileSync(a.path).subarray(0, 4).toString("hex")).toBe("89504e47");
  });

  test("pruning removes sessions idle for 14 days and keeps the rest", () => {
    const r = root();
    const old = join(r, "sessions", "old");
    const fresh = join(r, "sessions", "fresh");
    mkdirSync(old, { recursive: true });
    mkdirSync(fresh, { recursive: true });
    const now = Date.now();
    const fifteenDays = new Date(now - 15 * 86_400_000);
    utimesSync(old, fifteenDays, fifteenDays);
    pruneSessions(r, now);
    expect(existsSync(old)).toBe(false);
    expect(existsSync(fresh)).toBe(true);
  });
});
