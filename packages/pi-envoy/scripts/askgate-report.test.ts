import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  computeStats,
  formatReport,
  type GateRecord,
  isRefusal,
  isSentBack,
  parseArgs,
  parseSessionText,
  readSessions,
  sampleLine,
  sampleSentBack,
} from "./askgate-report";

interface EntryInput {
  id: string;
  ts?: string;
  tool?: string;
  outcome?: string;
  decision?: string;
  mode?: string;
  latency?: number;
  cost?: number | null;
  reason?: string;
  delivered?: string;
  reviewed?: string;
}

function gate(e: EntryInput): string {
  const data: Record<string, unknown> = {
    advisor: "AskGate",
    tool: e.tool ?? "dispatch_ask",
    path: "dispatch ask",
    toolCallId: e.id,
    decision: e.decision ?? "allow",
    outcome: e.outcome ?? "verdict",
    verdictMode: e.mode ?? "warn",
    latencyMs: e.latency ?? 100,
    argsDigest: "d",
    revisesForKey: "k",
  };
  if (e.cost !== undefined) {
    data.usage =
      e.cost === null ? null : { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, cost: e.cost };
  }
  if (e.reason !== undefined) data.reason = e.reason;
  if (e.delivered !== undefined) data.deliveredReason = e.delivered;
  if (e.reviewed !== undefined) data.reviewedText = e.reviewed;
  return JSON.stringify({
    type: "custom",
    customType: "advisor-gate",
    id: `g-${e.id}`,
    parentId: "p",
    timestamp: e.ts ?? "2026-10-21T12:00:00.000Z",
    data,
  });
}

function call(id: string, name: string, args: Record<string, unknown>): string {
  return JSON.stringify({
    type: "message",
    message: {
      role: "assistant",
      content: [
        { type: "text", text: "doing it" },
        { type: "toolCall", id, name, arguments: args },
      ],
    },
  });
}

// Session A (top level): five reviews of dispatch_ask, one old entry, one non-review.
const sessionA = [
  "not json at all",
  call("a1", "bash", { command: "dispatch ask --issue ACME-1 --question 'Ship it?'" }),
  gate({ id: "a1", decision: "revise", mode: "block", latency: 100, cost: 0.01, reason: "r1" }),
  call("a2", "bash", { command: "dispatch ask --issue ACME-2" }),
  gate({ id: "a2", decision: "revise", latency: 200, cost: 0.02, reason: "r2", delivered: "d2" }),
  gate({ id: "a3", decision: "allow", latency: 300, cost: 0.03 }),
  gate({ id: "a4", decision: "allow", latency: 400, cost: null }),
  gate({ id: "a5", decision: "allow", latency: 500 }),
  gate({ id: "old", decision: "revise", latency: 9999, ts: "2026-10-01T00:00:00.000Z" }),
  gate({ id: "a6", outcome: "unreadable", decision: "revise", latency: 5 }),
  gate({ id: "a7", outcome: "timeout", decision: "allow", latency: 7 }),
].join("\n");

// Session B (a subagent in a subdirectory): three dispatch_comment reviews.
const sessionB = [
  call("b1", "dispatch_comment", { issue: "ACME-3", body: "hello" }),
  gate({ id: "b1", tool: "dispatch_comment", decision: "revise", latency: 600, cost: 0.04 }),
  gate({ id: "b2", tool: "dispatch_comment", decision: "revise", latency: 700, reason: "r" }),
  gate({ id: "b3", tool: "dispatch_comment", decision: "allow", latency: 800 }),
  "{broken",
].join("\n");

const SINCE = "2026-10-20T00:00:00Z";
let dir = "";
let records: GateRecord[] = [];

beforeAll(async () => {
  dir = await mkdtemp(join(tmpdir(), "askgate-report-"));
  await mkdir(join(dir, "sessions", "proj", "sessA"), { recursive: true });
  await writeFile(join(dir, "sessions", "proj", "sessA.jsonl"), sessionA);
  await writeFile(join(dir, "sessions", "proj", "sessA", "sub.jsonl"), sessionB);
  await writeFile(join(dir, "sessions", "proj", "notes.txt"), gate({ id: "x" }));
  records = await readSessions(join(dir, "sessions"), Date.parse(SINCE));
});

afterAll(async () => {
  await rm(dir, { recursive: true, force: true });
});

describe("parseSessionText", () => {
  test("skips bad lines, keeps gate entries and joins the call text", () => {
    const parsed = parseSessionText(sessionA, "s");
    expect(parsed).toHaveLength(8);
    const a1 = parsed.find((r) => r.toolCallId === "a1");
    expect(a1?.text).toBe("dispatch ask --issue ACME-1 --question 'Ship it?'");
    expect(parsed.find((r) => r.toolCallId === "a3")?.text).toBeNull();
  });

  test("a non-bash call's text is its arguments as JSON", () => {
    const b1 = parseSessionText(sessionB, "s").find((r) => r.toolCallId === "b1");
    expect(b1?.text).toBe(JSON.stringify({ issue: "ACME-3", body: "hello" }));
  });

  test("an entry's reviewedText is the sample's text; an older entry's falls back to the command, marked so", () => {
    const reviewed = JSON.stringify({ issue: "ACME-1", content: "# The brief\n" }, null, 2);
    const text = [
      call("r1", "bash", { command: "dispatch artifact --issue ACME-1 --name b.md --path b.md" }),
      gate({ id: "r1", decision: "revise", reviewed }),
      call("r2", "bash", { command: "dispatch ask --issue ACME-2 --question q" }),
      gate({ id: "r2", decision: "revise" }),
    ].join("\n");
    const [r1, r2] = parseSessionText(text, "s");
    expect(r1).toMatchObject({ text: reviewed, textSource: "reviewed" });
    expect(r2).toMatchObject({
      text: "dispatch ask --issue ACME-2 --question q",
      textSource: "command",
    });
    expect(JSON.parse(sampleLine(r1 as GateRecord)).textSource).toBe("reviewed");
    expect(JSON.parse(sampleLine(r2 as GateRecord)).textSource).toBe("command");
  });
});

describe("readSessions and computeStats", () => {
  test("walks subdirectories, reads only .jsonl and applies --since", () => {
    expect(records).toHaveLength(10);
    expect(records.some((r) => r.toolCallId === "old")).toBe(false);
    expect(records.some((r) => r.toolCallId === "x")).toBe(false);
    expect(new Set(records.map((r) => r.session)).size).toBe(2);
  });

  test("counts, latency, cost, refusals and per-tool shares", () => {
    const s = computeStats(records);
    expect(s.gatedCalls).toBe(10);
    expect(s.reviews).toBe(8);
    expect(s.sentBack).toBe(4);
    // Review latencies 100..800: nearest-rank median is the 4th, p90 the 8th.
    expect(s.latencyMedian).toBe(400);
    expect(s.latencyP90).toBe(800);
    // Costs 0.01, 0.02, 0.03, 0.04: nearest-rank median is the 2nd.
    expect(s.costMedian).toBe(0.02);
    // a1 (block) and a6 (unreadable).
    expect(s.refusals).toBe(2);
    expect(s.perTool).toEqual([
      { tool: "dispatch_ask", reviews: 5, sentBack: 2 },
      { tool: "dispatch_comment", reviews: 3, sentBack: 2 },
    ]);
    const lines = formatReport(s, 42);
    expect(lines).toContain("sent back: 50.0% (4 of 8 reviews)");
    expect(lines).toContain("review latency median: 400 ms");
    expect(lines).toContain("review latency p90: 800 ms");
    expect(lines).toContain("review cost median: $0.0200 (4 reviews with a cost)");
    expect(lines).toContain("tool dispatch_ask: 5 reviews, sent back 40.0% (2)");
    expect(lines.at(-1)).toBe("seed: 42");
  });

  test("no reviews gives n/a", () => {
    const lines = formatReport(computeStats([]), 1);
    expect(lines).toContain("review latency median: n/a");
    expect(lines).toContain("review latency p90: n/a");
    expect(lines).toContain("review cost median: n/a");
    expect(lines).toContain("sent back: n/a (0 of 0 reviews)");
  });
});

describe("sampleSentBack", () => {
  test("a fixed seed gives the same sample; another seed differs", () => {
    const many: GateRecord[] = Array.from({ length: 40 }, (_, i) => ({
      ...records[0],
      toolCallId: `m${i}`,
      decision: "revise",
      outcome: "verdict",
    }));
    const one = sampleSentBack(many, 5, 7).map((r) => r.toolCallId);
    const two = sampleSentBack(many, 5, 7).map((r) => r.toolCallId);
    const other = sampleSentBack(many, 5, 8).map((r) => r.toolCallId);
    expect(one).toHaveLength(5);
    expect(new Set(one).size).toBe(5);
    expect(two).toEqual(one);
    expect(other).not.toEqual(one);
  });

  test("fewer sent back than asked gives all of them", () => {
    const picks = sampleSentBack(records, 10, 3);
    expect(picks.map((r) => r.toolCallId).sort()).toEqual(["a1", "a2", "b1", "b2"]);
  });
});

describe("parseArgs", () => {
  test("refuses a missing or malformed flag, naming it", () => {
    expect(() => parseArgs(["--since", SINCE, "--sample", "1", "--out", "o"])).toThrow(
      "--sessions"
    );
    expect(() =>
      parseArgs(["--sessions", "d", "--since", "yesterday", "--sample", "1", "--out", "o"])
    ).toThrow("--since");
    expect(() =>
      parseArgs(["--sessions", "d", "--since", SINCE, "--sample", "-1", "--out", "o"])
    ).toThrow("--sample");
  });
});

describe("isSentBack and isRefusal", () => {
  test("a gate that did not load sent the write back and refused it, in warn mode too", () => {
    const [record] = parseSessionText(
      gate({ id: "lf", outcome: "load-failed", decision: "revise", mode: "warn" }),
      "s"
    );
    expect(record && isSentBack(record)).toBe(true);
    expect(record && isRefusal(record)).toBe(true);
  });
});

describe("CLI", () => {
  test("prints the report and writes sample.jsonl", async () => {
    const out = join(dir, "out", "nested");
    const run = () =>
      Bun.spawnSync({
        cmd: [
          process.execPath,
          join(import.meta.dir, "askgate-report.ts"),
          "--sessions",
          join(dir, "sessions"),
          "--since",
          SINCE,
          "--sample",
          "3",
          "--out",
          out,
          "--seed",
          "11",
        ],
      });
    const first = run();
    expect(first.exitCode).toBe(0);
    const stdout = first.stdout.toString();
    expect(stdout).toContain("gated calls: 10");
    expect(stdout).toContain("reviews: 8");
    expect(stdout).toContain("refusals: 2");
    expect(stdout).toContain("seed: 11");
    const sample = await readFile(join(out, "sample.jsonl"), "utf8");
    const rows = sample
      .trim()
      .split("\n")
      .map((l) => JSON.parse(l));
    expect(rows).toHaveLength(3);
    for (const row of rows) {
      expect(Object.keys(row).sort()).toEqual(
        [
          "reason",
          "session",
          "text",
          "textSource",
          "timestamp",
          "tool",
          "toolCallId",
          "verdictMode",
        ].sort()
      );
    }
    const a2 = rows.find((r) => r.toolCallId === "a2");
    if (a2) expect(a2.reason).toBe("d2");
    expect(run().exitCode).toBe(0);
    expect(await readFile(join(out, "sample.jsonl"), "utf8")).toBe(sample);
  });

  test("exits 2 on a bad flag", () => {
    const r = Bun.spawnSync({
      cmd: [process.execPath, join(import.meta.dir, "askgate-report.ts"), "--sessions", "d"],
    });
    expect(r.exitCode).toBe(2);
    expect(r.stderr.toString().trim().split("\n")).toHaveLength(1);
  });
});
