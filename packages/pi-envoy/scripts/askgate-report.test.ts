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
  /** Fields of the rules check, the target and the mint, written into the entry as given. */
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
  // An unreadable call in each mode: warn ran it and returned the reason, block refused it.
  gate({ id: "a6", outcome: "unreadable", decision: "allow", mode: "warn", latency: 5 }),
  gate({ id: "a8", outcome: "unreadable", decision: "revise", mode: "block", latency: 6 }),
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
    expect(parsed).toHaveLength(9);
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

  // #2161, reviewer round 16: a decision the gate could not record on Dispatch is in no count of
  // the Dispatch report, so the session files are where it is counted. Round 17: an unmarked
  // write's loss rides on a follow-up of its own too, since the host appends that write's entry
  // before it starts the record.
  const SHUT_DOWN = "the session shut down before Dispatch answered the record";
  const REFUSED = "dispatch: 503 Service Unavailable";
  /** An `advisor-gate-record` follow-up: the gate's names a tool call, the host's a record id. */
  const lost = (data: Record<string, unknown>) =>
    JSON.stringify({
      type: "custom",
      customType: "advisor-gate-record",
      id: `r-${String(data.toolCallId)}`,
      parentId: "p",
      timestamp: "2026-10-21T12:00:01.000Z",
      data: { tool: "dispatch_ask", ...data },
    });
  test("a record the session could not confirm is read onto its call, from either follow-up", () => {
    const text = [
      gate({ id: "k1", fields: { ruleOutcome: "none" } }),
      lost({ toolCallId: "k1", recordError: SHUT_DOWN }),
      gate({ id: "k2", fields: { ruleOutcome: "none" } }),
      // An unmarked write: its entry is written before the record starts, so the reason rides on
      // a follow-up named by the record's own id.
      gate({ id: "k3", outcome: "unmarked", fields: { asksCarried: 1, recordId: "k3#0" } }),
      lost({ toolCallId: "k3", recordId: "k3#0", recordError: REFUSED }),
    ].join("\n");
    const parsed = parseSessionText(text, "s");
    expect(parsed.map((record) => [record.toolCallId, record.recordError])).toEqual([
      ["k1", SHUT_DOWN],
      ["k2", null],
      ["k3", REFUSED],
    ]);
    // The follow-up entry is no gated call of its own: it is counted as a record the session
    // could not confirm, and in nothing else.
    const stats = computeStats(parsed);
    expect(stats.gatedCalls).toBe(3);
    expect(stats.recordsUnconfirmed).toBe(2);
    expect(formatReport(stats, 1)).toContain("decision records not confirmed: 2");
  });

  // The quality review's probe (#2161, round 17): an unmarked entry takes the tool call id of the
  // `tool_result` that read the CLI's ledger line, so one tool call can carry a gated call of its
  // own and several unmarked writes. Joining the gated call's follow-up by that id copied its one
  // loss onto each of them, and counted one lost record three times.
  test("a gated call's unconfirmed record is counted once, not again on each unmarked entry of its tool call", () => {
    const text = [
      gate({ id: "c1", fields: { ruleOutcome: "none" } }),
      gate({ id: "c1", outcome: "unmarked", fields: { asksCarried: 1, recordId: "c1#0" } }),
      gate({ id: "c1", outcome: "unmarked", fields: { asksCarried: 1, recordId: "c1#1" } }),
      lost({ toolCallId: "c1", recordError: SHUT_DOWN }),
    ].join("\n");
    const parsed = parseSessionText(text, "s");
    expect(parsed.map((record) => record.recordError)).toEqual([SHUT_DOWN, null, null]);
    const stats = computeStats(parsed);
    expect(stats.gatedCalls).toBe(3);
    expect(stats.recordsUnconfirmed).toBe(1);
  });

  test("an unmarked write's unconfirmed record lands on its own entry alone, never on the tool call's others", () => {
    const text = [
      gate({ id: "c1", fields: { ruleOutcome: "none" } }),
      gate({ id: "c1", outcome: "unmarked", fields: { asksCarried: 1, recordId: "c1#0" } }),
      gate({ id: "c1", outcome: "unmarked", fields: { asksCarried: 1, recordId: "c1#1" } }),
      lost({ toolCallId: "c1", recordId: "c1#1", recordError: REFUSED }),
    ].join("\n");
    const parsed = parseSessionText(text, "s");
    expect(parsed.map((record) => record.recordError)).toEqual([null, null, REFUSED]);
    expect(computeStats(parsed).recordsUnconfirmed).toBe(1);
  });
});

describe("readSessions and computeStats", () => {
  test("walks subdirectories, reads only .jsonl and applies --since", () => {
    expect(records).toHaveLength(11);
    expect(records.some((r) => r.toolCallId === "old")).toBe(false);
    expect(records.some((r) => r.toolCallId === "x")).toBe(false);
    expect(new Set(records.map((r) => r.session)).size).toBe(2);
  });

  test("counts, latency, cost, refusals and per-tool shares", () => {
    const s = computeStats(records);
    expect(s.gatedCalls).toBe(11);
    expect(s.reviews).toBe(8);
    // Every call the gate did not pass as sent: four revises, and the two it could not read,
    // which never reached a review (a6 ran in warn mode, a8 was refused in block mode).
    expect(s.sentBack).toBe(6);
    // Every write that posted with no review behind it: the warn-mode unreadable call and the
    // timeout. A revise in warn mode posted, but a review stands behind it.
    expect(s.unreviewed).toBe(2);
    // Review latencies 100..800: nearest-rank median is the 4th, p90 the 8th.
    expect(s.latencyMedian).toBe(400);
    expect(s.latencyP90).toBe(800);
    // Costs 0.01, 0.02, 0.03, 0.04: nearest-rank median is the 2nd.
    expect(s.costMedian).toBe(0.02);
    // a1 (a revise in block mode) and a8 (unreadable in block mode); a6, unreadable in warn, ran.
    expect(s.refusals).toBe(2);
    expect(s.perTool).toEqual([
      { tool: "dispatch_ask", calls: 8, reviews: 5, sentBack: 4 },
      { tool: "dispatch_comment", calls: 3, reviews: 3, sentBack: 2 },
    ]);
    const lines = formatReport(s, 42);
    expect(lines).toContain("sent back: 54.5% (6 of 11 gated calls)");
    expect(lines).toContain("unreviewed sends: 2");
    expect(lines).toContain("review latency median: 400 ms");
    expect(lines).toContain("review latency p90: 800 ms");
    expect(lines).toContain("review cost median: $0.0200 (4 reviews with a cost)");
    expect(lines).toContain("tool dispatch_ask: 8 gated calls, 5 reviews, sent back 50.0% (4)");
    expect(lines.at(-1)).toBe("seed: 42");
  });

  test("no reviews gives n/a", () => {
    const lines = formatReport(computeStats([]), 1);
    expect(lines).toContain("review latency median: n/a");
    expect(lines).toContain("review latency p90: n/a");
    expect(lines).toContain("review cost median: n/a");
    expect(lines).toContain("sent back: n/a (0 of 0 gated calls)");
    expect(lines).toContain("rules calls: 0");
    expect(lines).toContain(
      "rules outcomes: match 0, none 0, error 0, timeout 0, no-verdict 0, abandoned 0, shutdown 0, unjudged 0"
    );
    expect(lines).toContain("rules latency median: n/a");
    expect(lines).toContain("rules matches: no_change 0, untried_access 0");
    expect(lines).toContain("rules errors: 0");
    expect(lines).toContain("rules asks cut: 0 asks in 0 entries");
    expect(lines).toContain("rules refusals overridden: 0 of 0 (rebuttal 0, breaker 0)");
    expect(lines).toContain("mints skipped: 0");
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
      mintSkipped: null,
    });
  });

  test("a rules refusal in warn mode is a refusal; a charter revise in warn mode is not", async () => {
    const parsed = await rulesRecords();
    const c1 = parsed.find((r) => r.toolCallId === "c1") as GateRecord;
    expect(c1).toMatchObject({ decision: "revise", verdictMode: "warn", outcome: "verdict" });
    expect(isRefusal(c1)).toBe(true);
    expect(isRefusal({ ...c1, rule: null })).toBe(false);
    expect(isRefusal({ ...c1, rule: null, outcome: "no-model-role" })).toBe(true);
    // A call the gate could not read is one call's form: warn mode runs it, so it is no refusal.
    expect(isRefusal({ ...c1, rule: null, outcome: "unreadable" })).toBe(false);
    expect(isRefusal({ ...c1, rule: null, outcome: "unreadable", verdictMode: "block" })).toBe(
      true
    );
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
      { name: "unjudged", count: 0 },
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
      "rules outcomes: match 4, none 1, error 2, timeout 1, no-verdict 1, abandoned 0, shutdown 0, unjudged 0",
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

  test("an entry whose rules check judged none of its asks is a rules call, outcome unjudged, with no error", () => {
    // An upload whose stored version could not be read, every opener carrying an id: no model was
    // asked, so the entry has no model and no error, and it is a rules call all the same.
    const [unjudged] = parseSessionText(
      gate({
        id: "u1",
        tool: "dispatch_artifact",
        fields: {
          target: "dispatch_artifact issue=ACME-4",
          ruleOutcome: "unjudged",
          ruleLatencyMs: 40,
          ruleAsksPartial: true,
        },
      }),
      "s"
    );
    const rules = computeStats([unjudged as GateRecord]).rules;
    expect(rules.calls).toBe(1);
    expect(rules.outcomes.find((count) => count.name === "unjudged")).toEqual({
      name: "unjudged",
      count: 1,
    });
    expect(rules.errors).toBe(0);
    expect(rules.models).toEqual([]);
    expect(formatReport(computeStats([unjudged as GateRecord]), 1)).toContain(
      "rules outcomes: match 0, none 0, error 0, timeout 0, no-verdict 0, abandoned 0, shutdown 0, unjudged 1"
    );
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

describe("the markers the gate did not mint", () => {
  test("an entry whose call had ended is counted as a mint skipped, and no other entry is", () => {
    // The call's own tool ended while the gate still held it, so nothing of the call can run and
    // the gate minted no marker (`mintSkipped`). Its outcome and decision are the review's own:
    // the count is of the markers, not of the verdicts, and it is printed on its own line.
    const parsed = parseSessionText(
      [
        gate({ id: "m1", decision: "allow", fields: { mintSkipped: "call-ended" } }),
        gate({ id: "m2", decision: "allow" }),
        gate({ id: "m3", outcome: "rebuttal", fields: { mintSkipped: "call-ended" } }),
      ].join("\n"),
      "sessM.jsonl"
    );
    expect(parsed.map((r) => r.mintSkipped)).toEqual(["call-ended", null, "call-ended"]);
    const stats = computeStats(parsed);
    expect(stats.mintsSkipped).toBe(2);
    // The skipped mint changes no other count: three gated calls, one of them unreviewed.
    expect(stats.gatedCalls).toBe(3);
    expect(stats.refusals).toBe(0);
    expect(stats.unreviewed).toBe(1);
    const lines = formatReport(stats, 1);
    expect(lines).toContain("mints skipped: 2");
    expect(lines.slice(0, lines.indexOf("gated calls: 3")).join(" ")).toContain(
      "no marker was minted"
    );
  });
});

describe("outcome counts and the sessions whose gate did not review", () => {
  test("every outcome is counted, in a fixed order", () => {
    const lines = formatReport(computeStats(records), 1);
    expect(lines).toContain("outcome verdict: 8");
    expect(lines).toContain("outcome unreadable: 2");
    expect(lines).toContain("outcome timeout: 1");
  });

  test("a session with an unreviewed write is named with its counts", () => {
    const lines = formatReport(computeStats(records), 1);
    const named = lines.filter((line) => line.startsWith("session "));
    // Session A holds one timeout and one unreadable call warn mode ran; session B's entries are
    // all reviews or revises, each of which a review stands behind.
    expect(named).toHaveLength(1);
    expect(named[0]).toContain("sessA.jsonl");
    expect(named[0]).toContain("timeout 1");
    expect(named[0]).toContain("unreadable 1");
  });

  test("a rebuttal and a breaker trip name their session too", () => {
    const unreviewed = parseSessionText(
      [
        gate({ id: "rb", outcome: "rebuttal", decision: "allow", mode: "block" }),
        gate({ id: "bk", outcome: "breaker", decision: "allow", mode: "block" }),
      ].join("\n"),
      "sessC.jsonl"
    );
    const stats = computeStats(unreviewed);
    expect(stats.unreviewed).toBe(2);
    expect(formatReport(stats, 1)).toContain("session sessC.jsonl: breaker 1, rebuttal 1");
  });

  test("no unreviewed write names no session", () => {
    const reviewed = records.filter((r) => r.outcome === "verdict");
    expect(formatReport(computeStats(reviewed), 1).some((l) => l.startsWith("session "))).toBe(
      false
    );
  });
});

describe("sampleSentBack", () => {
  test("a fixed seed gives the same sample; another seed differs", () => {
    const many: GateRecord[] = Array.from({ length: 40 }, (_, i) => ({
      ...(records[0] as GateRecord),
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

  test("refuses an empty --dispatch-project by name, rather than reading a project with no key", () => {
    expect(() => parseArgs(["--dispatch-project", "", "--since", SINCE])).toThrow(
      "--dispatch-project requires a project key"
    );
  });

  test("refuses an empty --sessions by name, rather than failing later on another flag", () => {
    expect(() => parseArgs(["--sessions", "", "--since", SINCE])).toThrow(
      "--sessions requires a directory"
    );
    expect(() =>
      parseArgs(["--sessions", "", "--since", SINCE, "--sample", "1", "--out", "o"])
    ).toThrow("--sessions requires a directory");
  });

  test("a flag given twice is never read as its last value alone", () => {
    // Live, tester round 6 at a3fc5c94: `--sessions <run 1> … --sessions <run 22>` printed
    // `gated calls: 1`, the last directory's, where the 22 directories held 35, with no error.
    // Every flag is refused by name when it is given twice.
    const valid: Record<string, string> = {
      "--sessions": "d",
      "--since": SINCE,
      "--sample": "1",
      "--out": "o",
    };
    for (const [flag, first, second] of [
      ["--sessions", "/runs/one", "/runs/two"],
      ["--since", SINCE, "2026-10-01T00:00:00Z"],
      ["--sample", "1", "5"],
      ["--out", "o", "p"],
      ["--seed", "1", "2"],
    ] as const) {
      const others = Object.entries(valid)
        .filter(([name]) => name !== flag)
        .flat();
      expect(() => parseArgs([...others, flag, first, flag, second])).toThrow(flag);
    }
    expect(() =>
      parseArgs(["--dispatch-project", "ACME", "--dispatch-project", "WIDGETS", "--since", SINCE])
    ).toThrow("--dispatch-project");
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

  test("a warn-mode unreadable call was sent back and not refused; a block-mode one was refused", () => {
    const [warned] = parseSessionText(
      gate({ id: "uw", outcome: "unreadable", decision: "allow", mode: "warn" }),
      "s"
    );
    expect(warned && isSentBack(warned)).toBe(true);
    expect(warned && isRefusal(warned)).toBe(false);
    const [blocked] = parseSessionText(
      gate({ id: "ub", outcome: "unreadable", decision: "revise", mode: "block" }),
      "s"
    );
    expect(blocked && isRefusal(blocked)).toBe(true);
  });

  // Final review of #2159, confirm at 0aa47421c, N2: the counts the header and the report's own
  // lines name, one fixture each.
  test("no-model-role is sent back and refused in warn mode too", () => {
    const [record] = parseSessionText(
      gate({ id: "nm", outcome: "no-model-role", decision: "revise", mode: "warn" }),
      "s"
    );
    expect(record && isSentBack(record)).toBe(true);
    expect(record && isRefusal(record)).toBe(true);
  });

  test("a block-mode abandoned call is sent back and refused; a warn-mode one is neither, and counts as unreviewed", () => {
    const [blocked] = parseSessionText(
      gate({ id: "ab", outcome: "abandoned", decision: "revise", mode: "block" }),
      "s"
    );
    expect(blocked && isSentBack(blocked)).toBe(true);
    expect(blocked && isRefusal(blocked)).toBe(true);
    const [warned] = parseSessionText(
      gate({ id: "aw", outcome: "abandoned", decision: "allow", mode: "warn" }),
      "s"
    );
    expect(warned && isSentBack(warned)).toBe(false);
    expect(warned && isRefusal(warned)).toBe(false);
    expect(warned ? computeStats([warned]).unreviewed : 0).toBe(1);
  });

  test("an unmarked write the CLI refused is sent back and refused; a warn-mode one posted, sent back and unreviewed", () => {
    const [blocked] = parseSessionText(
      gate({ id: "mb", outcome: "unmarked", decision: "revise", mode: "block" }),
      "s"
    );
    expect(blocked && isRefusal(blocked)).toBe(true);
    const [warned] = parseSessionText(
      gate({ id: "mw", outcome: "unmarked", decision: "allow", mode: "warn" }),
      "s"
    );
    expect(warned && isSentBack(warned)).toBe(true);
    expect(warned && isRefusal(warned)).toBe(false);
    expect(warned ? computeStats([warned]).unreviewed : 0).toBe(1);
  });

  test("the printed header names the rules isSentBack, isRefusal and isUnreviewed count by", () => {
    const header = formatReport(computeStats([]), 1).slice(0, 9).join(" ");
    for (const name of ["no-model-role", "abandoned", "shutdown", "unmarked", "load-failed"])
      expect(header).toContain(name);
    expect(header).toContain("warn mode's unreadable, unmarked, abandoned and shutdown");
    expect(header).toContain("a rules refusal, which refuses in either mode");
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
    expect(stdout).toContain("gated calls: 11");
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

  test("exits 2 naming --sessions when it is empty, and prints nothing", () => {
    const r = Bun.spawnSync({
      cmd: [
        process.execPath,
        join(import.meta.dir, "askgate-report.ts"),
        "--sessions",
        "",
        "--since",
        SINCE,
      ],
    });
    expect(r.exitCode).toBe(2);
    expect(r.stdout.toString()).toBe("");
    expect(r.stderr.toString()).toContain("--sessions requires a directory");
  });
});
