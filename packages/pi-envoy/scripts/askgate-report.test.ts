import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  computeStats,
  formatReport,
  type GateRecord,
  isRefusal,
  parseArgs,
  parseSessionText,
  readSessions,
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
  /** Fields of the rules check and the target, written into the entry as given. */
  fields?: Record<string, unknown>;
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
  Object.assign(data, e.fields);
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

const ASK_1 = "dispatch_ask issue=ACME-1";
const EDIT_2 = "dispatch_doc_edit issue=ACME-2 artifact=spec";
const SPEC = "dispatch_issue project=ACME";
const COMMENT_7 = "dispatch_comment issue=ACME-7";
const at = (second: number) => `2026-10-22T10:00:${String(second).padStart(2, "0")}.000Z`;

// Session C: the rules check. c2 comes first in the file but is stamped after c1, so only a join
// in time order finds c1's rebuttal.
const sessionC = [
  gate({ id: "c2", ts: at(3), outcome: "rebuttal", fields: { target: ASK_1 } }),
  gate({
    id: "c1",
    ts: at(1),
    decision: "revise",
    fields: {
      target: ASK_1,
      rule: "no_change",
      ruleAsk: 1,
      ruleQuote: "keep the 02:00 export",
      ruleOutcome: "match",
      ruleLatencyMs: 100,
      ruleModel: "p/m1",
    },
  }),
  gate({
    id: "c9",
    ts: at(2),
    tool: "dispatch_ask",
    fields: {
      target: "dispatch_ask issue=ACME-4",
      ruleOutcome: "none",
      ruleLatencyMs: 200,
      ruleModel: "p/m1",
      ruleAsksCut: 0,
    },
  }),
  gate({
    id: "c3",
    ts: at(4),
    tool: "dispatch_doc_edit",
    decision: "revise",
    fields: {
      target: EDIT_2,
      rule: "untried_access",
      ruleOutcome: "match",
      ruleLatencyMs: 300,
      ruleModel: "p/m2",
      ruleTextMissing: true,
      ruleAsksCut: 2,
    },
  }),
  gate({
    id: "c4",
    ts: at(5),
    tool: "dispatch_doc_edit",
    decision: "revise",
    fields: {
      target: EDIT_2,
      rule: "no_change",
      ruleOutcome: "match",
      ruleLatencyMs: 400,
      ruleModel: "p/m1",
    },
  }),
  gate({
    id: "c5",
    ts: at(6),
    tool: "dispatch_doc_edit",
    outcome: "breaker",
    fields: { target: EDIT_2 },
  }),
  gate({
    id: "c6",
    ts: at(7),
    tool: "dispatch_issue",
    mode: "block",
    decision: "revise",
    fields: {
      target: SPEC,
      rule: "no_change",
      ruleOutcome: "match",
      ruleLatencyMs: 500,
      ruleModel: "p/m1",
    },
  }),
  gate({
    id: "c7",
    ts: at(8),
    fields: {
      target: "dispatch_ask issue=ACME-3",
      ruleOutcome: "error",
      ruleError: "529 overloaded",
      ruleLatencyMs: 50,
      ruleModel: "p/m1",
    },
  }),
  gate({
    id: "c8",
    ts: at(9),
    fields: {
      target: "dispatch_ask issue=ACME-3",
      ruleOutcome: "timeout",
      ruleError: "timeout",
      ruleLatencyMs: 600,
      ruleModel: "p/m1",
      ruleAsksCut: 3,
    },
  }),
  gate({
    id: "c10",
    ts: at(10),
    fields: {
      target: "dispatch_ask issue=ACME-5",
      ruleOutcome: "no-verdict",
      ruleError: "no-verdict",
      ruleLatencyMs: 700,
      ruleModel: "p/m1",
    },
  }),
  gate({
    id: "c11",
    ts: at(11),
    fields: {
      target: "dispatch_ask issue=ACME-6",
      ruleOutcome: "error",
      ruleError: "529 overloaded",
      ruleLatencyMs: 60,
      ruleModel: "p/m2",
    },
  }),
  // A charter revise in warn mode: sent back, not refused, and its rebuttal overrides no rule.
  gate({
    id: "c12",
    ts: at(12),
    tool: "dispatch_comment",
    decision: "revise",
    fields: { target: COMMENT_7 },
  }),
  gate({
    id: "c13",
    ts: at(13),
    tool: "dispatch_comment",
    outcome: "rebuttal",
    fields: { target: COMMENT_7 },
  }),
].join("\n");

// Session D: a rebuttal on c6's target in another session does not override c6.
const sessionD = gate({
  id: "d1",
  ts: at(20),
  tool: "dispatch_issue",
  outcome: "rebuttal",
  fields: { target: SPEC },
});

const SINCE = "2026-10-20T00:00:00Z";
let dir = "";
let records: GateRecord[] = [];

beforeAll(async () => {
  dir = await mkdtemp(join(tmpdir(), "askgate-report-"));
  await mkdir(join(dir, "sessions", "proj", "sessA"), { recursive: true });
  await writeFile(join(dir, "sessions", "proj", "sessA.jsonl"), sessionA);
  await writeFile(join(dir, "sessions", "proj", "sessA", "sub.jsonl"), sessionB);
  await writeFile(join(dir, "sessions", "proj", "notes.txt"), gate({ id: "x" }));
  await mkdir(join(dir, "rules", "proj"), { recursive: true });
  await writeFile(join(dir, "rules", "proj", "sessC.jsonl"), sessionC);
  await writeFile(join(dir, "rules", "proj", "sessD.jsonl"), sessionD);
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
    expect(lines).toContain("rules calls: 0");
    expect(lines).toContain(
      "rules outcomes: match 0, none 0, error 0, timeout 0, no-verdict 0, abandoned 0, shutdown 0"
    );
    expect(lines).toContain("rules latency median: n/a");
    expect(lines).toContain("rules matches: no_change 0, untried_access 0");
    expect(lines).toContain("rules errors: 0");
    expect(lines).toContain("rules asks cut: 0 asks in 0 entries");
    expect(lines).toContain("rules refusals overridden: 0 of 0 (rebuttal 0, breaker 0)");
  });
});

describe("the rules check", () => {
  const rulesRecords = () => readSessions(join(dir, "rules"), Date.parse(SINCE));

  test("reads the rules fields and the target; an entry without them reads as unset", () => {
    const parsed = parseSessionText(sessionC, "s");
    const c3 = parsed.find((r) => r.toolCallId === "c3");
    expect(c3).toMatchObject({
      rule: "untried_access",
      ruleOutcome: "match",
      ruleError: null,
      ruleTextMissing: true,
      ruleAsksCut: 2,
      ruleLatencyMs: 300,
      ruleModel: "p/m2",
      target: EDIT_2,
    });
    expect(parsed.find((r) => r.toolCallId === "c8")?.ruleError).toBe("timeout");
    const a1 = parseSessionText(sessionA, "s").find((r) => r.toolCallId === "a1");
    expect(a1).toMatchObject({
      rule: null,
      ruleOutcome: null,
      ruleError: null,
      ruleTextMissing: false,
      ruleAsksCut: null,
      ruleLatencyMs: null,
      ruleModel: null,
      target: null,
    });
  });

  test("a rules refusal in warn mode is a refusal; a charter revise in warn mode is not", async () => {
    const parsed = await rulesRecords();
    const c1 = parsed.find((r) => r.toolCallId === "c1") as GateRecord;
    expect(c1).toMatchObject({ decision: "revise", verdictMode: "warn", outcome: "verdict" });
    expect(isRefusal(c1)).toBe(true);
    expect(isRefusal({ ...c1, rule: null })).toBe(false);
    expect(isRefusal({ ...c1, rule: null, outcome: "no-model-role" })).toBe(true);
    expect(isRefusal({ ...c1, rule: null, outcome: "unreadable" })).toBe(true);
    expect(isRefusal({ ...c1, rule: null, verdictMode: "block" })).toBe(true);
    expect(isRefusal({ ...c1, decision: "allow" })).toBe(false);
    // c1, c3 and c4 (warn, with a rule) and c6 (block); c12, a charter revise in warn, is not.
    expect(computeStats(parsed).refusals).toBe(4);
  });

  test("counts matches by rule, outcomes, latency, models, errors, missing text and cut asks", async () => {
    const rules = computeStats(await rulesRecords()).rules;
    expect(rules.calls).toBe(9);
    expect(rules.outcomes).toEqual([
      { name: "match", count: 4 },
      { name: "none", count: 1 },
      { name: "error", count: 2 },
      { name: "timeout", count: 1 },
      { name: "no-verdict", count: 1 },
      { name: "abandoned", count: 0 },
      { name: "shutdown", count: 0 },
    ]);
    expect(rules.matches).toEqual([
      { name: "no_change", count: 3 },
      { name: "untried_access", count: 1 },
    ]);
    // Rules latencies 50, 60, 100 … 700: nearest-rank median is the 5th, p90 the 9th.
    expect(rules.latencyMedian).toBe(300);
    expect(rules.latencyP90).toBe(700);
    expect(rules.models).toEqual([
      { name: "p/m1", count: 7 },
      { name: "p/m2", count: 2 },
    ]);
    expect(rules.errors).toBe(4);
    expect(rules.errorTexts).toEqual([
      { name: "529 overloaded", count: 2 },
      { name: "no-verdict", count: 1 },
      { name: "timeout", count: 1 },
    ]);
    expect(rules.textMissing).toBe(1);
    // c3 cut 2 and c8 cut 3; c9's 0 cut nothing.
    expect(rules.cutEntries).toBe(2);
    expect(rules.cutAsks).toBe(5);
  });

  test("a rules refusal is overridden only by the next entry on its target, in time order, in its session", async () => {
    const rules = computeStats(await rulesRecords()).rules;
    // c1 → c2 (rebuttal, earlier in the file but later in time); c3 → c4 (a refusal, so c3 stands);
    // c4 → c5 (breaker); c6 → nothing in its session (d1 is another session's rebuttal).
    // c12 → c13 is a rebuttal of a charter revise, not of a rules refusal.
    expect(rules.refusals).toBe(4);
    expect(rules.overridden).toEqual({ rebuttal: 1, breaker: 1 });
  });

  test("the report prints the rules lines before the seed", async () => {
    const lines = formatReport(computeStats(await rulesRecords()), 5);
    for (const line of [
      "refusals: 4",
      "rules calls: 9",
      "rules outcomes: match 4, none 1, error 2, timeout 1, no-verdict 1, abandoned 0, shutdown 0",
      "rules latency median: 300 ms",
      "rules latency p90: 700 ms",
      "rules model p/m1: 7",
      "rules model p/m2: 2",
      "rules matches: no_change 3, untried_access 1",
      "rules errors: 4",
      'rules error "529 overloaded": 2',
      'rules error "no-verdict": 1',
      'rules error "timeout": 1',
      "rules text missing: 1",
      "rules asks cut: 5 asks in 2 entries",
      "rules refusals overridden: 2 of 4 (rebuttal 1, breaker 1)",
    ]) {
      expect(lines).toContain(line);
    }
    expect(lines.indexOf('rules error "529 overloaded": 2')).toBeLessThan(
      lines.indexOf('rules error "no-verdict": 1')
    );
    expect(lines.at(-1)).toBe("seed: 5");
  });

  test("prints at most five error texts, each cut to 200 code points", () => {
    const base = parseSessionText(sessionC, "s").find((r) => r.toolCallId === "c7") as GateRecord;
    const long = "é".repeat(300);
    const errors = ["a", "b", "c", "d", "e", "f", long, long].map((ruleError, i) => ({
      ...base,
      toolCallId: `e${i}`,
      ruleError,
    }));
    const lines = formatReport(computeStats(errors), 1).filter((l) => l.startsWith("rules error "));
    expect(lines).toHaveLength(5);
    expect(lines[0]).toBe(`rules error ${JSON.stringify(`${"é".repeat(199)}…`)}: 2`);
    expect(lines.slice(1)).toEqual([
      'rules error "a": 1',
      'rules error "b": 1',
      'rules error "c": 1',
      'rules error "d": 1',
    ]);
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
          "rule",
          "session",
          "text",
          "timestamp",
          "tool",
          "toolCallId",
          "verdictMode",
        ].sort()
      );
      expect(row.rule).toBeNull();
    }
    const a2 = rows.find((r) => r.toolCallId === "a2");
    if (a2) expect(a2.reason).toBe("d2");
    expect(run().exitCode).toBe(0);
    expect(await readFile(join(out, "sample.jsonl"), "utf8")).toBe(sample);
  });

  test("prints the rules check's counts, and a sampled rules refusal names its rule", async () => {
    const out = join(dir, "out", "rules");
    const r = Bun.spawnSync({
      cmd: [
        process.execPath,
        join(import.meta.dir, "askgate-report.ts"),
        "--sessions",
        join(dir, "rules"),
        "--since",
        SINCE,
        "--sample",
        "10",
        "--out",
        out,
        "--seed",
        "1",
      ],
    });
    expect(r.exitCode).toBe(0);
    const stdout = r.stdout.toString();
    expect(stdout).toContain("gated calls: 14\n");
    expect(stdout).toContain("refusals: 4\n");
    expect(stdout).toContain("rules matches: no_change 3, untried_access 1\n");
    expect(stdout).toContain("rules refusals overridden: 2 of 4 (rebuttal 1, breaker 1)\n");
    const rows = (await readFile(join(out, "sample.jsonl"), "utf8"))
      .trim()
      .split("\n")
      .map((l) => JSON.parse(l));
    // The reviews sent back: the four rules refusals and c12, a charter revise.
    expect(Object.fromEntries(rows.map((row) => [row.toolCallId, row.rule]))).toEqual({
      c1: "no_change",
      c3: "untried_access",
      c4: "no_change",
      c6: "no_change",
      c12: null,
    });
  });

  test("exits 2 on a bad flag", () => {
    const r = Bun.spawnSync({
      cmd: [process.execPath, join(import.meta.dir, "askgate-report.ts"), "--sessions", "d"],
    });
    expect(r.exitCode).toBe(2);
    expect(r.stderr.toString().trim().split("\n")).toHaveLength(1);
  });
});
