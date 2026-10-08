import { describe, expect, test } from "bun:test";
import {
  appendFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { adviceMemory, resetAdviceMemory } from "../dispatch-execute";
import { forgetShownPictures, shownPictures } from "../dispatch-picture-tools";
import {
  appendResult,
  loadSessionMemory,
  pruneSessions,
  readResultsSince,
  readSessionTitle,
  recordOutcome,
  resultsEnd,
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
    const unexpected = (problem: string) => {
      throw new Error(problem);
    };
    appendResult(dir, { tool: "dispatch_search", details: {} });
    const first = readResultsSince(dir, 0, unexpected);
    appendResult(dir, { tool: "dispatch_ask", details: { ask: "a" } });
    const second = readResultsSince(dir, first.offset, unexpected);
    expect(first.entries.map((e) => e.tool)).toEqual(["dispatch_search"]);
    expect(second.entries.map((e) => e.tool)).toEqual(["dispatch_ask"]);
  });

  test("a reader that starts at the ledger's end reads only what is appended after it", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s4");
    expect(resultsEnd(dir)).toBe(0);
    appendResult(dir, { tool: "dispatch_search", details: {} });
    const end = resultsEnd(dir);
    appendResult(dir, { tool: "dispatch_ask", details: { ask: "a" } });
    const read = readResultsSince(dir, end, (problem) => {
      throw new Error(problem);
    });
    expect(read.entries.map((e) => e.tool)).toEqual(["dispatch_ask"]);
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

  test("a malformed ledger line is named once, with its file and line, and the reader reads on", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s5");
    appendResult(dir, { tool: "dispatch_search", details: {} });
    appendFileSync(join(dir, "results.jsonl"), '{"tool":"dispatch_ask","det\n');
    appendResult(dir, { tool: "dispatch_ask", details: { ask: "a" } });
    const problems: string[] = [];

    const read = readResultsSince(dir, 0, (problem) => problems.push(problem));

    expect(read.entries.map((entry) => entry.tool)).toEqual(["dispatch_search", "dispatch_ask"]);
    expect(read.offset).toBe(resultsEnd(dir));
    expect(problems).toHaveLength(1);
    expect(problems[0]).toContain(`${join(dir, "results.jsonl")}:2`);
    // The line is behind the returned offset, so the next read neither returns nor names it again.
    expect(readResultsSince(dir, read.offset, (problem) => problems.push(problem)).entries).toEqual(
      []
    );
    expect(problems).toHaveLength(1);
  });

  test("a ledger line counts from the start of the file even when the read starts later", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s6");
    appendResult(dir, { tool: "dispatch_search", details: {} });
    appendResult(dir, { tool: "dispatch_search", details: {} });
    const offset = resultsEnd(dir);
    appendFileSync(join(dir, "results.jsonl"), "garbage\n");
    const problems: string[] = [];
    readResultsSince(dir, offset, (problem) => problems.push(problem));
    expect(problems[0]).toContain(`${join(dir, "results.jsonl")}:3`);
  });

  test("a corrupted state.json throws naming the file", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s7");
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, "state.json"), "{not json");
    expect(() => loadSessionMemory(dir, "s7")).toThrow(join(dir, "state.json"));
  });

  test("a missing title, state and ledger read as empty without creating anything", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s8");
    expect(readSessionTitle(dir)).toBeUndefined();
    expect(loadSessionMemory(dir, "s8").size).toBe(0);
    expect(resultsEnd(dir)).toBe(0);
    expect(readResultsSince(dir, 0, () => undefined)).toEqual({ entries: [], offset: 0 });
    expect(existsSync(dir)).toBe(false);
  });

  test("one outcome writes one ledger line and the memory together", () => {
    const dir = sessionDirectory({ DISPATCH_STATE_DIR: root() }, "s9");
    recordOutcome(dir, "s9", new Set(["ask-9"]), {
      tool: "dispatch_ask",
      details: { ask: "ask-9" },
    });
    expect(readResultsSince(dir, 0, () => undefined).entries.map((entry) => entry.tool)).toEqual([
      "dispatch_ask",
    ]);
    expect(loadSessionMemory(dir, "s9").has("ask-9")).toBe(true);
  });

  test("pruning runs at most once a day", () => {
    const r = root();
    const now = Date.now();
    pruneSessions(r, now);
    const old = join(r, "sessions", "old");
    mkdirSync(old, { recursive: true });
    const fifteenDays = new Date(now - 15 * 86_400_000);
    utimesSync(old, fifteenDays, fifteenDays);
    pruneSessions(r, now + 3_600_000);
    expect(existsSync(old)).toBe(true);
    pruneSessions(r, now + 2 * 86_400_000);
    expect(existsSync(old)).toBe(false);
  });
});
