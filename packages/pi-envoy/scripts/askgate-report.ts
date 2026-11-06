// Reports on AskGate's advisor-gate session entries, its rules check included, and samples the
// reviews it sent back. With --dispatch-project it reads instead the decision documents AskGate
// publishes to a Dispatch project, which is all a standing session holding a Dispatch token and
// nothing else — no access to the machine a gated session ran on — can read; that reader is
// `askgate-report-dispatch.ts`, and what both count with is `askgate-report-common.ts`. This file
// holds the session-file report and the command line of both.
// Usage: bun askgate-report.ts --sessions <dir> --since <RFC 3339> --sample <n> --out <dir>
//   [--seed <n>]
//   bun askgate-report.ts --dispatch-project <KEY> --since <RFC 3339> [--json]
//
// What the counts mean, as `isSentBack`, `isRefusal` and `isUnreviewed` read an entry:
//   - sent back: the gate did not pass the write as sent. Every entry whose decision is
//     `revise` — a revise verdict, a rules refusal, `no-model-role`, and, in block mode,
//     `abandoned` and `shutdown` (the two causes of a call given up with no verdict) and
//     `unmarked` (a gated write the dispatch CLI found no marker for) — and every `unreadable`,
//     `unmarked` or `load-failed` entry, whatever its mode.
//   - refusal: a write that did not post. Every sent-back entry of a session in block mode,
//     plus `no-model-role` and `load-failed` in warn mode too, since neither gate can review
//     anything and a gate that cannot review is no off switch, and every entry with a `rule`,
//     since the rules check refuses in either mode. A warn-mode `unreadable` or `unmarked` is
//     sent back and no refusal: its write posted.
//   - unreviewed: a write that posted with no review behind it. Every `allow` whose outcome is
//     not `verdict`: timeout, error, no-verdict, halted, skipped, rebuttal, breaker, and in
//     warn mode `unreadable`, `unmarked`, `abandoned` and `shutdown`. A warn-mode `abandoned` or
//     `shutdown` is that alone: its decision is `allow`, so it is not sent back.
//   - decision records not confirmed: the session could not confirm that the gated call's
//     decision reached Dispatch. Every entry carrying a `recordError`, which rides on the
//     `advisor-gate-record` entry written after the call — the gate's own, named by the tool call
//     id, and an unmarked write's, named by that record's own id — so the day's `--sessions`
//     count names what the `--dispatch-project` count over the same day cannot hold. It is an
//     upper bound on what Dispatch lacks: a write given up on this side may still have landed
//     (`extensions/askgate/decision-record.ts`).
import { mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs as parseArgv } from "node:util";
import { RULE_NAMES, RULE_OUTCOMES } from "../extensions/askgate/rules-check";
import {
  type Count,
  isObject,
  num,
  overrides,
  percentile,
  share,
  str,
  strOrNull,
  tally,
} from "./askgate-report-common";
import {
  computeDispatchReport,
  dispatchReportLines,
  readDecisionDocuments,
  sessionDispatch,
} from "./askgate-report-dispatch";

/**
 * One advisor-gate entry, with the write's text: the arguments the model reviewed (`reviewedText`),
 * or, for an entry written before the gate recorded them, the text of its tool call.
 */
export interface GateRecord {
  session: string;
  timestamp: string;
  tool: string;
  toolCallId: string;
  decision: string;
  outcome: string;
  verdictMode: string;
  reason: string | null;
  latencyMs: number | null;
  cost: number | null;
  text: string | null;
  /** The rule the first matching new ask broke: set exactly when the rules check refused the call. */
  rule: string | null;
  /** How the rules check ended: `match`, `none`, `error`, `timeout`, `no-verdict`, `unjudged`, … */
  ruleOutcome: string | null;
  /** The provider's error text, `timeout` or `no-verdict`. */
  ruleError: string | null;
  /** An ask's text could not be read, so it was judged without it. */
  ruleTextMissing: boolean;
  /** How many asks the rules request left out or cut. */
  ruleAsksCut: number | null;
  ruleLatencyMs: number | null;
  ruleModel: string | null;
  /** The call's breaker target, as `dispatch_ask issue=ACME-1`. */
  target: string | null;
  /** Why no marker was minted though the decision let the write out: `call-ended`, or null. */
  mintSkipped: string | null;
  /**
   * On an unmarked write whose Dispatch record the host started: that record's own id,
   * `<toolCallId>#<the CLI ledger line's place in the call>`, which is what its
   * `advisor-gate-record` follow-up names. Null on every other entry.
   */
  recordId: string | null;
  /**
   * Why this call's decision record was never confirmed: the `recordError` of the
   * `advisor-gate-record` entry written after it — the gate's own, named by the tool call id, or
   * an unmarked write's, named by its `recordId`. Null where the record landed, or where the call
   * had none to write. The write may have landed on Dispatch all the same (`decision-record.ts`).
   */
  recordError: string | null;
  /** `reviewed`: the text the model judged. `command`: the tool call's own text, which for a
   *  file-backed write names the file and not its content. */
  textSource: "reviewed" | "command";
}

export interface ToolShare {
  tool: string;
  /** Every gated call of the tool, reviewed or not. */
  calls: number;
  reviews: number;
  sentBack: number;
}

export interface RuleStats {
  /** Entries with a ruleOutcome: the gated calls whose rules call ran. */
  calls: number;
  outcomes: Count[];
  latencyMedian: number | null;
  latencyP90: number | null;
  models: Count[];
  /** Rules refusals by rule. */
  matches: Count[];
  /** Entries with a ruleError. */
  errors: number;
  /** The commonest error texts, most first. */
  errorTexts: Count[];
  textMissing: number;
  /** Entries that left out or cut an ask, and how many asks they left out or cut. */
  cutEntries: number;
  cutAsks: number;
  /** Entries with a rule. */
  refusals: number;
  /** Rules refusals whose next entry on the same target was a rebuttal or a breaker. */
  overridden: { rebuttal: number; breaker: number };
}

export interface SessionOutcomes {
  session: string;
  /** Each unreviewed outcome of that session and how many times it happened, by outcome name. */
  counts: Array<[string, number]>;
}

export interface Stats {
  gatedCalls: number;
  reviews: number;
  /** Every gated call the gate did not pass as sent, reviewed or not (`isSentBack`). */
  sentBack: number;
  /** Every write that posted with no review behind it (`isUnreviewed`). */
  unreviewed: number;
  latencyMedian: number | null;
  latencyP90: number | null;
  costMedian: number | null;
  costCount: number;
  refusals: number;
  /**
   * Entries carrying `mintSkipped`: the decision let the write out, and the call's own tool had
   * ended while the gate still held it, so no marker was minted and no command of it can post.
   */
  mintsSkipped: number;
  /**
   * Gated calls carrying a `recordError`: the session could not confirm that the decision's
   * record reached Dispatch, because the write failed or was given up. An upper bound on what
   * Dispatch lacks, never a count of it — a write given up on this side may have landed all the
   * same (`decision-record.ts`) — so the Dispatch report over the same day counts at most that
   * many decisions fewer than the machine made.
   */
  recordsUnconfirmed: number;
  perTool: ToolShare[];
  rules: RuleStats;
  /** Every outcome seen and how many entries carried it, most first. */
  byOutcome: Array<[string, number]>;
  /** The sessions where the gate reviewed nothing at least once, named with their counts. */
  unreviewedSessions: SessionOutcomes[];
}

/** The report over the session files on disk. */
export interface SessionsArgs {
  mode: "sessions";
  sessions: string;
  since: number;
  sample: number;
  out: string;
  seed: number | null;
}

/** The report over one Dispatch project's decision documents. */
export interface DispatchArgs {
  mode: "dispatch";
  project: string;
  since: number;
  /** `--since` as given, which the report prints so a day's numbers name their day. */
  sinceText: string;
  json: boolean;
}

export type Args = SessionsArgs | DispatchArgs;

/** One session entry's JSON object, as the reader walks its fields. */
type Json = Record<string, unknown>;

function callText(call: Json): string {
  const args = call.arguments;
  if (call.name === "bash" && isObject(args) && typeof args.command === "string") {
    return args.command;
  }
  return JSON.stringify(args ?? null);
}

/**
 * Reads every advisor-gate entry of one session file's text, each carrying what the session could
 * not confirm of its record on Dispatch: the `recordError` of the `advisor-gate-record` entry
 * written after it, which the gate names by the tool call id and the host, for an unmarked write,
 * by that record's own id. Lines that do not parse are skipped.
 */
export function parseSessionText(text: string, session: string): GateRecord[] {
  const calls = new Map<string, string>();
  /** Each gated call whose decision record failed, by tool call id, and what stopped it. */
  const gatedLost = new Map<string, string>();
  /**
   * The same for an unmarked write, by its record's own id: one tool call can log several
   * unmarked writes and be a gated call itself, since an unmarked entry takes the id of the tool
   * result that read the CLI's ledger line, so a tool call id names none of them on its own
   * (#2161, reviewer round 17).
   */
  const unmarkedLost = new Map<string, string>();
  const records: GateRecord[] = [];
  for (const line of text.split("\n")) {
    if (line.trim() === "") continue;
    let entry: unknown;
    try {
      entry = JSON.parse(line);
    } catch {
      continue;
    }
    if (!isObject(entry)) continue;
    if (entry.type === "message" && isObject(entry.message)) {
      const content = entry.message.content;
      if (!Array.isArray(content)) continue;
      for (const part of content) {
        if (isObject(part) && part.type === "toolCall" && typeof part.id === "string") {
          calls.set(part.id, callText(part));
        }
      }
      continue;
    }
    if (entry.type === "custom" && entry.customType === "advisor-gate-record") {
      const lost = isObject(entry.data) ? entry.data : {};
      const why = strOrNull(lost.recordError);
      if (why === null) continue;
      const recordId = strOrNull(lost.recordId);
      if (recordId === null) gatedLost.set(str(lost.toolCallId), why);
      else unmarkedLost.set(recordId, why);
      continue;
    }
    if (entry.type !== "custom" || entry.customType !== "advisor-gate") continue;
    const data = isObject(entry.data) ? entry.data : {};
    const usage = isObject(data.usage) ? data.usage : {};
    const reason = data.deliveredReason ?? data.reason;
    const reviewed = typeof data.reviewedText === "string" ? data.reviewedText : null;
    records.push({
      session,
      timestamp: str(entry.timestamp),
      tool: str(data.tool),
      toolCallId: str(data.toolCallId),
      decision: str(data.decision),
      outcome: str(data.outcome),
      verdictMode: str(data.verdictMode),
      reason: strOrNull(reason),
      latencyMs: num(data.latencyMs),
      cost: num(usage.cost),
      text: reviewed,
      textSource: reviewed === null ? "command" : "reviewed",
      rule: strOrNull(data.rule),
      ruleOutcome: strOrNull(data.ruleOutcome),
      ruleError: strOrNull(data.ruleError),
      ruleTextMissing: data.ruleTextMissing === true,
      ruleAsksCut: num(data.ruleAsksCut),
      ruleLatencyMs: num(data.ruleLatencyMs),
      ruleModel: strOrNull(data.ruleModel),
      target: strOrNull(data.target),
      mintSkipped: strOrNull(data.mintSkipped),
      recordId: strOrNull(data.recordId),
      // Joined below, from the follow-up entry the record's failure wrote.
      recordError: null,
    });
  }
  // The call comes before its entry, and the record's follow-up entry after it, but both joins
  // wait for the whole file.
  for (const record of records) {
    if (record.textSource === "command") record.text = calls.get(record.toolCallId) ?? null;
    // Each side joins on its own key, so one lost record is counted once: a gated call's loss is
    // not copied onto the unmarked entries that share its tool call id, nor an unmarked write's
    // onto the gated call's entry.
    record.recordError =
      record.outcome === "unmarked"
        ? record.recordId === null
          ? null
          : (unmarkedLost.get(record.recordId) ?? null)
        : (gatedLost.get(record.toolCallId) ?? null);
  }
  return records;
}

async function listJsonl(dir: string): Promise<string[]> {
  const found: string[] = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) found.push(...(await listJsonl(path)));
    else if (entry.isFile() && entry.name.endsWith(".jsonl")) found.push(path);
  }
  return found;
}

/** Reads every session file under dir, keeping entries at or after since (epoch ms). */
export async function readSessions(dir: string, since: number): Promise<GateRecord[]> {
  const files = (await listJsonl(dir)).sort();
  const records: GateRecord[] = [];
  for (const file of files) {
    for (const record of parseSessionText(await readFile(file, "utf8"), file)) {
      const at = Date.parse(record.timestamp);
      if (Number.isFinite(at) && at >= since) records.push(record);
    }
  }
  return records;
}

export function isReview(record: GateRecord): boolean {
  return record.outcome === "verdict";
}

/**
 * The gate did not pass the write as sent: a revise (a charter revise or a rules refusal), a
 * literal call whose file was missing or not a regular file when the gate read it, or a gated
 * write the CLI found no matching marker for (each of which warn mode lets post, so its decision
 * is `allow`), or a gate that did not load.
 */
export function isSentBack(record: GateRecord): boolean {
  return (
    record.decision === "revise" ||
    record.outcome === "unreadable" ||
    record.outcome === "unmarked" ||
    record.outcome === "load-failed"
  );
}

/**
 * A write that did not post: sent back in block mode, or refused in every mode. `no-model-role`
 * and `load-failed` mean the gate can review nothing, so each refuses in warn mode too, and a
 * rules refusal (an entry with a `rule`) refuses in either mode; an `unreadable` call is one
 * call's form, which warn mode lets through, as it lets an `unmarked` write post.
 */
export function isRefusal(record: GateRecord): boolean {
  return (
    isSentBack(record) &&
    (record.verdictMode === "block" ||
      record.outcome === "no-model-role" ||
      record.outcome === "load-failed" ||
      record.rule !== null)
  );
}

/** How many of the commonest rules error texts the report prints. */
const ERROR_TEXTS_SHOWN = 5;

/** Counts the rules refusals the agent overrode, by what overrode each. */
export function overriddenRefusals(records: GateRecord[]): RuleStats["overridden"] {
  const overridden = { rebuttal: 0, breaker: 0 };
  for (const { by } of overrides(records, (record) => record.timestamp)) overridden[by] += 1;
  return overridden;
}

export function computeRuleStats(records: GateRecord[]): RuleStats {
  const calls = records.filter((r) => r.ruleOutcome !== null);
  const refusals = records.filter((r) => r.rule !== null);
  const errors = records.filter((r) => r.ruleError !== null);
  const cut = records.filter((r) => r.ruleAsksCut !== null && r.ruleAsksCut > 0);
  const latencies = records.flatMap((r) => (r.ruleLatencyMs === null ? [] : [r.ruleLatencyMs]));
  return {
    calls: calls.length,
    outcomes: tally(
      calls.map((r) => r.ruleOutcome as string),
      RULE_OUTCOMES
    ),
    latencyMedian: percentile(latencies, 50),
    latencyP90: percentile(latencies, 90),
    models: tally(records.flatMap((r) => (r.ruleModel === null ? [] : [r.ruleModel]))),
    matches: tally(
      refusals.map((r) => r.rule as string),
      RULE_NAMES
    ),
    errors: errors.length,
    errorTexts: tally(errors.map((r) => r.ruleError as string)).slice(0, ERROR_TEXTS_SHOWN),
    textMissing: records.filter((r) => r.ruleTextMissing).length,
    cutEntries: cut.length,
    cutAsks: cut.reduce((sum, r) => sum + (r.ruleAsksCut as number), 0),
    refusals: refusals.length,
    overridden: overriddenRefusals(records),
  };
}

/**
 * A write that posted with no review behind it: the gate failed, gave up, was paused, had no
 * model for the session's provider, took a rebuttal or tripped its breaker, or could not read the
 * call and warn mode ran it. Every one of them is an `allow` whose outcome is not a verdict, and
 * only a reader who counts them can tell "no writes" from "no gate".
 */
export function isUnreviewed(record: GateRecord): boolean {
  return record.decision === "allow" && record.outcome !== "verdict";
}

export function computeStats(records: GateRecord[]): Stats {
  const reviews = records.filter(isReview);
  const latencies = reviews.flatMap((r) => (r.latencyMs === null ? [] : [r.latencyMs]));
  const costs = reviews.flatMap((r) => (r.cost === null ? [] : [r.cost]));
  const byTool = new Map<string, ToolShare>();
  // Every gated call of the tool, not its reviews alone: a call the gate could not read, and one
  // a gate that did not load refused, was sent back without any review.
  for (const record of records) {
    const share = byTool.get(record.tool) ?? {
      tool: record.tool,
      calls: 0,
      reviews: 0,
      sentBack: 0,
    };
    share.calls += 1;
    if (isReview(record)) share.reviews += 1;
    if (isSentBack(record)) share.sentBack += 1;
    byTool.set(record.tool, share);
  }
  const byOutcome = new Map<string, number>();
  const bySession = new Map<string, Map<string, number>>();
  for (const record of records) {
    byOutcome.set(record.outcome, (byOutcome.get(record.outcome) ?? 0) + 1);
    if (!isUnreviewed(record)) continue;
    const counts = bySession.get(record.session) ?? new Map<string, number>();
    counts.set(record.outcome, (counts.get(record.outcome) ?? 0) + 1);
    bySession.set(record.session, counts);
  }
  return {
    gatedCalls: records.length,
    reviews: reviews.length,
    sentBack: records.filter(isSentBack).length,
    unreviewed: records.filter(isUnreviewed).length,
    latencyMedian: percentile(latencies, 50),
    latencyP90: percentile(latencies, 90),
    costMedian: percentile(costs, 50),
    costCount: costs.length,
    refusals: records.filter(isRefusal).length,
    mintsSkipped: records.filter((r) => r.mintSkipped !== null).length,
    recordsUnconfirmed: records.filter((r) => r.recordError !== null).length,
    perTool: [...byTool.values()].sort((a, b) => (a.tool < b.tool ? -1 : a.tool > b.tool ? 1 : 0)),
    rules: computeRuleStats(records),
    byOutcome: [...byOutcome.entries()].sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1)),
    unreviewedSessions: [...bySession.entries()]
      .map(([session, counts]) => ({
        session,
        counts: [...counts.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1)),
      }))
      .sort((a, b) => (a.session < b.session ? -1 : 1)),
  };
}

function ms(value: number | null): string {
  return value === null ? "n/a" : `${value} ms`;
}

/** How many code points of a rules error text the report prints. */
const ERROR_TEXT_POINTS = 200;

export function formatReport(stats: Stats, seed: number): string[] {
  const cost =
    stats.costMedian === null
      ? "n/a"
      : `$${stats.costMedian.toFixed(4)} (${stats.costCount} reviews with a cost)`;
  const rules = stats.rules;
  const overridden = rules.overridden.rebuttal + rules.overridden.breaker;
  return [
    "A gated call is any advisor-gate entry. A review is one where the model gave a verdict.",
    "Sent back means the gate did not pass the write as sent: every revise (a revise verdict,",
    "a rules refusal, no-model-role, and in block mode abandoned, shutdown and unmarked), and",
    "every unreadable, unmarked or load-failed entry in any mode. It is counted over every",
    "gated call, since most of those never reach a review. A refusal is a write that did not",
    "post: one sent back in block mode, no-model-role and load-failed, which refuse in warn",
    "mode too, and a rules refusal, which refuses in either mode. Unreviewed is a write that",
    "posted with no review behind it: every allow whose outcome is no verdict, a rebuttal, a",
    "breaker trip, and warn mode's unreadable, unmarked, abandoned and shutdown among them.",
    "A session named below had at least one such write, with each outcome's count.",
    "A rules call is an entry with a ruleOutcome. A rules refusal is an entry with a rule.",
    "A rules refusal was overridden when the next entry on its target, in its session, is a rebuttal or a breaker.",
    `The rules error lines show the ${ERROR_TEXTS_SHOWN} commonest error texts.`,
    "A mint skipped is a call whose decision let the write out and whose own tool ended while the",
    "gate still held it: no marker was minted, since no command of that call can run any more.",
    "Decision records not confirmed are the calls whose decision record failed or was given up:",
    "an upper bound on what Dispatch lacks, since a write given up here may have landed, so the",
    "Dispatch report over the same day counts at most that many decisions fewer than this one.",
    `gated calls: ${stats.gatedCalls}`,
    `reviews: ${stats.reviews}`,
    `sent back: ${share(stats.sentBack, stats.gatedCalls)} (${stats.sentBack} of ${stats.gatedCalls} gated calls)`,
    `review latency median: ${ms(stats.latencyMedian)}`,
    `review latency p90: ${ms(stats.latencyP90)}`,
    `review cost median: ${cost}`,
    `refusals: ${stats.refusals}`,
    `unreviewed sends: ${stats.unreviewed}`,
    `mints skipped: ${stats.mintsSkipped}`,
    `decision records not confirmed: ${stats.recordsUnconfirmed}`,
    ...stats.byOutcome.map(([outcome, count]) => `outcome ${outcome}: ${count}`),
    ...stats.perTool.map(
      (t) =>
        `tool ${t.tool}: ${t.calls} gated calls, ${t.reviews} reviews, sent back ${share(t.sentBack, t.calls)} (${t.sentBack})`
    ),
    ...stats.unreviewedSessions.map(
      ({ session, counts }) =>
        `session ${session}: ${counts.map(([outcome, count]) => `${outcome} ${count}`).join(", ")}`
    ),
    `rules calls: ${rules.calls}`,
    `rules outcomes: ${rules.outcomes.map((c) => `${c.name} ${c.count}`).join(", ")}`,
    `rules latency median: ${ms(rules.latencyMedian)}`,
    `rules latency p90: ${ms(rules.latencyP90)}`,
    ...rules.models.map((m) => `rules model ${m.name}: ${m.count}`),
    `rules matches: ${rules.matches.map((c) => `${c.name} ${c.count}`).join(", ")}`,
    `rules errors: ${rules.errors}`,
    ...rules.errorTexts.map((e) => {
      const points = Array.from(e.name);
      const text =
        points.length > ERROR_TEXT_POINTS
          ? `${points.slice(0, ERROR_TEXT_POINTS - 1).join("")}…`
          : e.name;
      return `rules error ${JSON.stringify(text)}: ${e.count}`;
    }),
    `rules text missing: ${rules.textMissing}`,
    `rules asks cut: ${rules.cutAsks} asks in ${rules.cutEntries} entries`,
    `rules refusals overridden: ${overridden} of ${rules.refusals} (rebuttal ${rules.overridden.rebuttal}, breaker ${rules.overridden.breaker})`,
    `seed: ${seed}`,
  ];
}

/** mulberry32: a small deterministic PRNG returning floats in [0, 1). */
export function mulberry32(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let t = state;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** Picks n of the reviews sent back (all of them when fewer), in a seeded order. */
export function sampleSentBack(records: GateRecord[], n: number, seed: number): GateRecord[] {
  const pool = records.filter((r) => isReview(r) && isSentBack(r));
  const random = mulberry32(seed);
  const count = Math.min(n, pool.length);
  // Partial Fisher-Yates shuffle.
  for (let i = 0; i < count; i++) {
    const j = i + Math.floor(random() * (pool.length - i));
    const picked = pool[j] as GateRecord;
    pool[j] = pool[i] as GateRecord;
    pool[i] = picked;
  }
  return pool.slice(0, count);
}

export function sampleLine(record: GateRecord): string {
  return JSON.stringify({
    session: record.session,
    timestamp: record.timestamp,
    tool: record.tool,
    toolCallId: record.toolCallId,
    verdictMode: record.verdictMode,
    rule: record.rule,
    reason: record.reason,
    textSource: record.textSource,
    text: record.text,
  });
}

function nonNegativeInt(flag: string, value: string): number {
  if (!/^\d+$/.test(value)) throw new Error(`${flag} must be a non-negative integer`);
  return Number(value);
}

/**
 * Every flag, each read as a list so that a flag given twice is refused by name rather than read
 * as its last value alone: `--sessions <run 1> … --sessions <run 22>` printed one directory's
 * counts as the whole (tester round 6 at a3fc5c94).
 */
const OPTIONS = {
  sessions: { type: "string", multiple: true },
  since: { type: "string", multiple: true },
  sample: { type: "string", multiple: true },
  out: { type: "string", multiple: true },
  seed: { type: "string", multiple: true },
  "dispatch-project": { type: "string", multiple: true },
  json: { type: "boolean", multiple: true },
} as const;

/** Parses the command line, in either mode. Throws an error naming the flag at fault. */
export function parseArgs(argv: string[]): Args {
  const parsed = parseArgv({ args: argv, options: OPTIONS, strict: true, allowPositionals: false });
  const values: Record<string, string> = {};
  for (const [name, given] of Object.entries(parsed.values)) {
    const flag = `--${name}`;
    if (!Array.isArray(given)) continue;
    if (given.length > 1) throw new Error(`${flag} is given more than once; give it once`);
    const value = given[0];
    // A value that begins with `--` is the flag the agent meant to give next, taken as this
    // flag's value only when it was written as `--sessions=--since`.
    if (typeof value === "string") {
      if (value.startsWith("--")) throw new Error(`${flag} needs a value`);
      values[flag] = value;
    }
  }
  const json = parsed.values.json !== undefined;
  const project = values["--dispatch-project"];
  // An empty key would be read as a project name and die on a 404 from Dispatch, so it is refused
  // here by name, as `scripts/dispatch/ask-census.ts` refuses an empty --project.
  if (project === "") throw new Error("--dispatch-project requires a project key");
  // An empty directory would pass as given and fail later on another flag, so it is refused by
  // name too.
  if (values["--sessions"] === "") throw new Error("--sessions requires a directory");
  if (project !== undefined && values["--sessions"] !== undefined) {
    throw new Error("--sessions and --dispatch-project cannot be given together");
  }
  if (values["--since"] === undefined) throw new Error("--since is required");
  const sinceText = values["--since"];
  const rfc3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/i;
  const since = Date.parse(sinceText);
  if (!rfc3339.test(sinceText) || !Number.isFinite(since)) {
    throw new Error("--since must be an RFC 3339 time");
  }
  if (project !== undefined) {
    // The Dispatch mode counts what the documents hold: it samples nothing and writes no file.
    for (const flag of ["--sample", "--out", "--seed"]) {
      if (values[flag] !== undefined) {
        throw new Error(`${flag} is not allowed with --dispatch-project`);
      }
    }
    return { mode: "dispatch", project, since, sinceText, json };
  }
  if (json) throw new Error("--json is allowed only with --dispatch-project");
  const sessions = values["--sessions"];
  if (sessions === undefined) {
    throw new Error("one of --sessions and --dispatch-project is required");
  }
  for (const flag of ["--sample", "--out"]) {
    if (values[flag] === undefined) throw new Error(`${flag} is required`);
  }
  const seedText = values["--seed"];
  return {
    mode: "sessions",
    sessions,
    since,
    sample: nonNegativeInt("--sample", values["--sample"] as string),
    out: values["--out"] as string,
    seed: seedText === undefined ? null : nonNegativeInt("--seed", seedText) >>> 0,
  };
}

async function main(): Promise<void> {
  let args: Args;
  try {
    args = parseArgs(process.argv.slice(2));
  } catch (error) {
    console.error(`askgate-report: ${(error as Error).message}`);
    process.exit(2);
  }
  if (args.mode === "dispatch") {
    const read = await readDecisionDocuments(sessionDispatch(), args.project, args.since);
    const report = computeDispatchReport(read, args.project, args.sinceText);
    for (const line of dispatchReportLines(report, args.json)) console.log(line);
    return;
  }
  const seed = args.seed ?? (crypto.getRandomValues(new Uint32Array(1))[0] as number);
  const records = await readSessions(args.sessions, args.since);
  for (const line of formatReport(computeStats(records), seed)) console.log(line);
  const picks = sampleSentBack(records, args.sample, seed);
  await mkdir(args.out, { recursive: true });
  const body = picks.map(sampleLine).join("\n");
  await writeFile(join(args.out, "sample.jsonl"), body === "" ? "" : `${body}\n`);
}

if (import.meta.main) await main();
