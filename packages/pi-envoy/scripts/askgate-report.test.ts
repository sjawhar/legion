import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  computeDispatchReport,
  computeStats,
  type DispatchReport,
  dispatchReportLines,
  formatReport,
  type GateRecord,
  isRefusal,
  isSentBack,
  parseArgs,
  parseSessionText,
  type ReportClient,
  readDecisionDocuments,
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
  /** Fields of the rules check and the target, written into the entry as given. */
  fields?: Record<string, unknown>;
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
  Object.assign(data, e.fields);
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

  test("parses each mode, and refuses --json outside the Dispatch mode", () => {
    expect(parseArgs(["--sessions", "d", "--since", SINCE, "--sample", "2", "--out", "o"])).toEqual(
      { mode: "sessions", sessions: "d", since: Date.parse(SINCE), sample: 2, out: "o", seed: null }
    );
    expect(parseArgs(["--dispatch-project", "ACME", "--since", SINCE, "--json"])).toEqual({
      mode: "dispatch",
      project: "ACME",
      since: Date.parse(SINCE),
      sinceText: SINCE,
      json: true,
    });
    expect(() =>
      parseArgs(["--sessions", "d", "--since", SINCE, "--sample", "1", "--out", "o", "--json"])
    ).toThrow("--json is allowed only with --dispatch-project");
  });

  test("refuses the two modes together, neither of them, and --sample or --out with the project", () => {
    expect(() =>
      parseArgs(["--sessions", "d", "--dispatch-project", "ACME", "--since", SINCE])
    ).toThrow("--sessions and --dispatch-project cannot be given together");
    expect(() => parseArgs(["--since", SINCE])).toThrow(
      "one of --sessions and --dispatch-project is required"
    );
    expect(() =>
      parseArgs(["--dispatch-project", "ACME", "--since", SINCE, "--sample", "1"])
    ).toThrow("--sample is not allowed with --dispatch-project");
    expect(() => parseArgs(["--dispatch-project", "ACME", "--since", SINCE, "--out", "o"])).toThrow(
      "--out is not allowed with --dispatch-project"
    );
    expect(() => parseArgs(["--dispatch-project", "ACME", "--since", "yesterday"])).toThrow(
      "--since"
    );
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

const PROJECT = "ACME";
const POD_SESSION = "ses-pod";
const MACHINE_SESSION = "ses-machine";

/** One decision record's line, in the wire shape `extensions/askgate/decision-record.ts` writes. */
function decision(e: {
  time: string;
  session: string;
  source: "pod" | "machine";
  target?: string;
  rule?: string;
  outcome?: string;
  ruleOutcome?: string;
  ruleError?: string;
}): string {
  return JSON.stringify({
    time: e.time,
    session: e.session,
    source: e.source,
    project: PROJECT,
    tool: "bash",
    target: e.target ?? null,
    rule: e.rule ?? null,
    ruleOutcome: e.ruleOutcome ?? (e.rule === undefined ? "none" : "match"),
    ruleError: e.ruleError ?? null,
    ruleTextMissing: false,
    ruleAsksCut: 0,
    decision: e.rule === undefined ? "allow" : "revise",
    outcome: e.outcome ?? "verdict",
    verdictMode: "block",
    ruleLatencyMs: 1200,
    charterLatencyMs: null,
    plugin: "@sjawhar/pi-envoy@0.0.0",
  });
}

/** A decision document as the gate uploads one: its header paragraph, then one block per record. */
function decisionDocument(session: string, lines: readonly string[]): string {
  const blocks = lines.map((line) => `\`\`\`json\n${line}\n\`\`\``).join("\n\n");
  return `AskGate rule decisions of session ${session}. One block per decision.\n\n${blocks}\n`;
}

/** A document of the project: its markdown, or null for one whose read Dispatch refuses. */
interface ProjectDocument {
  name: string;
  markdown: string | null;
}

/** A Dispatch project holding documents, as the reader's two calls see it. */
function dispatchProject(documents: readonly ProjectDocument[]): ReportClient {
  return {
    listProjectArtifacts: async (key: string) => {
      expect(key).toBe(PROJECT);
      return documents.map((held, index) => ({
        id: `art-${index}`,
        name: held.name,
        slug: held.name.replace(/\.md$/, ""),
      }));
    },
    docRead: async (id: string) => {
      const held = documents[Number(id.slice("art-".length))];
      if (held === undefined || held.markdown === null) {
        throw new Error(`artifact ${id} cannot be read`);
      }
      return { markdown: held.markdown };
    },
  };
}

// The pod session: a refusal a rebuttal on the same target overrode, a refusal nothing overrode,
// and a rules call that errored.
const POD_DOCUMENT = decisionDocument(POD_SESSION, [
  decision({ time: at(1), session: POD_SESSION, source: "pod", rule: "no_change", target: ASK_1 }),
  decision({
    time: at(2),
    session: POD_SESSION,
    source: "pod",
    target: ASK_1,
    outcome: "rebuttal",
    ruleOutcome: "none",
  }),
  decision({
    time: at(3),
    session: POD_SESSION,
    source: "pod",
    rule: "untried_access",
    target: EDIT_2,
  }),
  decision({
    time: at(4),
    session: POD_SESSION,
    source: "pod",
    target: SPEC,
    ruleOutcome: "error",
    ruleError: "502 from the provider",
  }),
]);

// The machine session: one refusal, and a rules call that timed out with no error text of its own.
const MACHINE_DOCUMENT = decisionDocument(MACHINE_SESSION, [
  decision({
    time: at(1),
    session: MACHINE_SESSION,
    source: "machine",
    rule: "no_change",
    target: COMMENT_7,
  }),
  decision({
    time: at(2),
    session: MACHINE_SESSION,
    source: "machine",
    target: SPEC,
    ruleOutcome: "timeout",
  }),
  decision({ time: at(3), session: MACHINE_SESSION, source: "machine", target: SPEC }),
]);

/** The day's project: the two sessions' documents, and one document that is not a decision one. */
const DAY: ProjectDocument[] = [
  { name: `askgate-decisions-${POD_SESSION}.md`, markdown: POD_DOCUMENT },
  { name: "spec.md", markdown: "The project's specification, which holds no decision." },
  { name: `askgate-decisions-${MACHINE_SESSION}.md`, markdown: MACHINE_DOCUMENT },
];

const DAY_REPORT: DispatchReport = {
  project: PROJECT,
  since: SINCE,
  documents: 2,
  unreadableDocuments: 0,
  sessions: 2,
  unparsedLines: 0,
  total: {
    calls: 7,
    refusals: 3,
    byRule: { no_change: 2, untried_access: 1 },
    overrides: 1,
    errors: 2,
  },
  pod: {
    calls: 4,
    refusals: 2,
    byRule: { no_change: 1, untried_access: 1 },
    overrides: 1,
    errors: 1,
  },
  machine: {
    calls: 3,
    refusals: 1,
    byRule: { no_change: 1, untried_access: 0 },
    overrides: 0,
    errors: 1,
  },
};

async function reportOf(documents: readonly ProjectDocument[], since: string) {
  const read = await readDecisionDocuments(dispatchProject(documents), PROJECT, Date.parse(since));
  return { read, report: computeDispatchReport(read, PROJECT, since) };
}

describe("the Dispatch reader", () => {
  test("two sessions' documents, one pod and one machine, are read into one report", async () => {
    const { report } = await reportOf(DAY, SINCE);
    expect(report).toEqual(DAY_REPORT);
  });

  test("a record before --since is left out, and one at --since is counted", async () => {
    const documents = [
      {
        name: `askgate-decisions-${POD_SESSION}.md`,
        markdown: decisionDocument(POD_SESSION, [
          decision({
            time: at(0),
            session: POD_SESSION,
            source: "pod",
            rule: "no_change",
            target: ASK_1,
          }),
          decision({
            time: at(1),
            session: POD_SESSION,
            source: "pod",
            rule: "untried_access",
            target: EDIT_2,
          }),
        ]),
      },
    ];
    const { read, report } = await reportOf(documents, at(1));
    expect(read.records.map((record) => record.time)).toEqual([at(1)]);
    expect(report.total).toEqual({
      calls: 1,
      refusals: 1,
      byRule: { no_change: 0, untried_access: 1 },
      overrides: 0,
      errors: 0,
    });
  });

  test("a line that is not a record and a document the read refuses are counted, not fatal", async () => {
    const documents: ProjectDocument[] = [
      { name: `askgate-decisions-${POD_SESSION}.md`, markdown: POD_DOCUMENT },
      {
        name: `askgate-decisions-${MACHINE_SESSION}.md`,
        markdown: decisionDocument(MACHINE_SESSION, [
          '{"time": "2026-10-22T10:00:01.000Z", "session": "ses-machine"',
          decision({
            time: at(1),
            session: MACHINE_SESSION,
            source: "machine",
            rule: "no_change",
            target: COMMENT_7,
          }),
        ]),
      },
      { name: "askgate-decisions-ses-killed.md", markdown: null },
    ];
    const { report } = await reportOf(documents, SINCE);
    expect({
      documents: report.documents,
      unreadableDocuments: report.unreadableDocuments,
      unparsedLines: report.unparsedLines,
      sessions: report.sessions,
    }).toEqual({ documents: 2, unreadableDocuments: 1, unparsedLines: 1, sessions: 2 });
    expect(report.pod).toEqual(DAY_REPORT.pod);
    expect(report.machine).toEqual({
      calls: 1,
      refusals: 1,
      byRule: { no_change: 1, untried_access: 0 },
      overrides: 0,
      errors: 0,
    });
  });

  test("--json prints one object carrying the numbers the plain text prints", async () => {
    const { report } = await reportOf(DAY, SINCE);
    const json = dispatchReportLines(report, true);
    expect(json).toHaveLength(1);
    expect(JSON.parse(json[0] as string)).toEqual(DAY_REPORT);
    const text = dispatchReportLines(report, false);
    expect(text).toContain(`project: ${PROJECT}`);
    expect(text).toContain(`since: ${SINCE}`);
    expect(text).toContain("documents: 2");
    expect(text).toContain("unreadable documents: 0");
    expect(text).toContain("sessions: 2");
    expect(text).toContain("unparsed lines: 0");
    expect(text).toContain("rules calls: 7 (pod 4, machine 3)");
    expect(text).toContain("refusals: 3 (pod 2, machine 1)");
    expect(text).toContain("rule no_change: 2 (pod 1, machine 1)");
    expect(text).toContain("rule untried_access: 1 (pod 1, machine 0)");
    expect(text).toContain("overrides: 1 (pod 1, machine 0)");
    expect(text).toContain("rules errors: 2 (pod 1, machine 1)");
    expect(text).toContain("rules error rate: 28.6% (pod 25.0%, machine 33.3%)");
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
          "textSource",
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

  test("exits 2 naming the flag at fault when both modes are given", () => {
    const r = Bun.spawnSync({
      cmd: [
        process.execPath,
        join(import.meta.dir, "askgate-report.ts"),
        "--sessions",
        "d",
        "--dispatch-project",
        "ACME",
        "--since",
        SINCE,
      ],
    });
    expect(r.exitCode).toBe(2);
    expect(r.stderr.toString()).toContain(
      "--sessions and --dispatch-project cannot be given together"
    );
  });
});
