// Reports on AskGate's advisor-gate session entries, its rules check included, and samples the
// reviews it sent back. With --dispatch-project it reads instead the decision documents AskGate
// publishes to a Dispatch project, which is all a standing session holding a Dispatch token and
// nothing else — no access to the machine a gated session ran on — can read.
// Usage: bun askgate-report.ts --sessions <dir> --since <RFC 3339> --sample <n> --out <dir>
//   [--seed <n>]
//   bun askgate-report.ts --dispatch-project <KEY> --since <RFC 3339> [--json]
import { mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { activeDispatchSettings } from "@legion/envoy-client/dispatch-config";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { sessionConfigFile, sessionDispatchSettings } from "@legion/envoy-client/session-settings";
import type { DecisionRecord } from "../extensions/askgate/decision-record";
import type { RuleName, RuleOutcome } from "../extensions/askgate/rules-check";

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
  /** How the rules call ended: `match`, `none`, `error`, `timeout`, `no-verdict`, … */
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
  /** `reviewed`: the text the model judged. `command`: the tool call's own text, which for a
   *  file-backed write names the file and not its content. */
  textSource: "reviewed" | "command";
}

export interface ToolShare {
  tool: string;
  reviews: number;
  sentBack: number;
}

/** A value and how many entries hold it. */
export interface Count {
  name: string;
  count: number;
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

export interface Stats {
  gatedCalls: number;
  reviews: number;
  sentBack: number;
  latencyMedian: number | null;
  latencyP90: number | null;
  costMedian: number | null;
  costCount: number;
  refusals: number;
  perTool: ToolShare[];
  rules: RuleStats;
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

type Json = Record<string, unknown>;

function isObject(value: unknown): value is Json {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function str(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function strOrNull(value: unknown): string | null {
  return typeof value === "string" ? value : null;
}

function num(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function callText(call: Json): string {
  const args = call.arguments;
  if (call.name === "bash" && isObject(args) && typeof args.command === "string") {
    return args.command;
  }
  return JSON.stringify(args ?? null);
}

/** Reads every advisor-gate entry of one session file's text. Lines that do not parse are skipped. */
export function parseSessionText(text: string, session: string): GateRecord[] {
  const calls = new Map<string, string>();
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
    });
  }
  // The call comes before its entry, but the join waits for the whole file.
  for (const record of records)
    if (record.textSource === "command") record.text = calls.get(record.toolCallId) ?? null;
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

/** The gate did not pass the write as sent: a revise, a refusal, or a gate that did not load. */
export function isSentBack(record: GateRecord): boolean {
  return record.decision === "revise" || record.outcome === "load-failed";
}

/** A write that did not post: refused in block mode, or refused in every mode (a rules refusal is one in warn mode too). */
export function isRefusal(record: GateRecord): boolean {
  return (
    isSentBack(record) &&
    (record.verdictMode === "block" ||
      record.outcome === "unreadable" ||
      record.outcome === "no-model-role" ||
      record.outcome === "load-failed" ||
      record.rule !== null)
  );
}

/** Nearest-rank percentile; null when there are no values. */
export function percentile(values: number[], p: number): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const rank = Math.max(1, Math.ceil((p / 100) * sorted.length));
  return sorted[rank - 1] ?? null;
}

const RULES: readonly RuleName[] = ["no_change", "untried_access"];
const RULE_OUTCOMES: readonly RuleOutcome[] = [
  "match",
  "none",
  "error",
  "timeout",
  "no-verdict",
  "abandoned",
  "shutdown",
];
/** How many of the commonest rules error texts the report prints. */
const ERROR_TEXTS_SHOWN = 5;

/**
 * Counts each value. The names in `first` come first, in that order, even at zero; every other
 * value follows, most first, then by name.
 */
function tally(values: string[], first: readonly string[] = []): Count[] {
  const counts = new Map<string, number>(first.map((name) => [name, 0]));
  for (const value of values) counts.set(value, (counts.get(value) ?? 0) + 1);
  const rest = [...counts]
    .filter(([name]) => !first.includes(name))
    .sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  return [
    ...first.map((name) => ({ name, count: counts.get(name) ?? 0 })),
    ...rest.map(([name, count]) => ({ name, count })),
  ];
}

/** What the override walk reads: a gate entry and a decision record alike. */
interface Overridable {
  session: string;
  target: string | null;
  rule: string | null;
  outcome: string;
}

/**
 * The rules refusals (records with a rule) that the agent overrode, each with what overrode it:
 * in each session, in time order, the next record with the refusal's target is a rebuttal or a
 * breaker. The time is read through `stamp`, since a gate entry carries it as `timestamp` and a
 * decision record as `time`.
 */
function overrides<T extends Overridable>(
  records: readonly T[],
  stamp: (record: T) => string
): { refusal: T; by: "rebuttal" | "breaker" }[] {
  const found: { refusal: T; by: "rebuttal" | "breaker" }[] = [];
  const sessions = new Map<string, T[]>();
  for (const record of records) {
    const list = sessions.get(record.session) ?? [];
    list.push(record);
    sessions.set(record.session, list);
  }
  for (const list of sessions.values()) {
    // A stable sort by time: entries stamped in the same millisecond keep their file order.
    const ordered = list
      .map((record, index) => {
        const at = Date.parse(stamp(record));
        return { record, index, at: Number.isFinite(at) ? at : Number.NEGATIVE_INFINITY };
      })
      .sort((a, b) => a.at - b.at || a.index - b.index);
    // Walk back from the end, so each target's next outcome is known when its refusal is reached.
    const next = new Map<string, string>();
    for (let i = ordered.length - 1; i >= 0; i--) {
      const { record } = ordered[i];
      if (record.target === null) continue;
      if (record.rule !== null) {
        const after = next.get(record.target);
        if (after === "rebuttal" || after === "breaker") found.push({ refusal: record, by: after });
      }
      next.set(record.target, record.outcome);
    }
  }
  return found;
}

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
      RULES
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

export function computeStats(records: GateRecord[]): Stats {
  const reviews = records.filter(isReview);
  const latencies = reviews.flatMap((r) => (r.latencyMs === null ? [] : [r.latencyMs]));
  const costs = reviews.flatMap((r) => (r.cost === null ? [] : [r.cost]));
  const byTool = new Map<string, ToolShare>();
  for (const review of reviews) {
    const share = byTool.get(review.tool) ?? { tool: review.tool, reviews: 0, sentBack: 0 };
    share.reviews += 1;
    if (isSentBack(review)) share.sentBack += 1;
    byTool.set(review.tool, share);
  }
  return {
    gatedCalls: records.length,
    reviews: reviews.length,
    sentBack: reviews.filter(isSentBack).length,
    latencyMedian: percentile(latencies, 50),
    latencyP90: percentile(latencies, 90),
    costMedian: percentile(costs, 50),
    costCount: costs.length,
    refusals: records.filter(isRefusal).length,
    perTool: [...byTool.values()].sort((a, b) => (a.tool < b.tool ? -1 : a.tool > b.tool ? 1 : 0)),
    rules: computeRuleStats(records),
  };
}

function share(part: number, whole: number): string {
  return whole === 0 ? "n/a" : `${((part / whole) * 100).toFixed(1)}%`;
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
    "Sent back means the decision was revise.",
    "A rules call is an entry with a ruleOutcome. A rules refusal is an entry with a rule.",
    "A rules refusal was overridden when the next entry on its target, in its session, is a rebuttal or a breaker.",
    `The rules error lines show the ${ERROR_TEXTS_SHOWN} commonest error texts.`,
    `gated calls: ${stats.gatedCalls}`,
    `reviews: ${stats.reviews}`,
    `sent back: ${share(stats.sentBack, stats.reviews)} (${stats.sentBack} of ${stats.reviews} reviews)`,
    `review latency median: ${ms(stats.latencyMedian)}`,
    `review latency p90: ${ms(stats.latencyP90)}`,
    `review cost median: ${cost}`,
    `refusals: ${stats.refusals}`,
    ...stats.perTool.map(
      (t) =>
        `tool ${t.tool}: ${t.reviews} reviews, sent back ${share(t.sentBack, t.reviews)} (${t.sentBack})`
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
    const picked = pool[j];
    pool[j] = pool[i];
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

/** The name and slug every decision document begins with, as the gate's recorder posts them. */
const DECISION_DOCUMENT_PREFIX = "askgate-decisions-";
/** A rules call that answered nothing usable, named by its outcome rather than by an error text. */
const ERROR_OUTCOMES = ["error", "timeout", "no-verdict"];

/**
 * The fields of one decision record the report counts: a subset of the record
 * `extensions/askgate/decision-record.ts` writes, taken from its own type, so a change to that
 * wire shape is a type error here rather than a silently empty count.
 */
export type ReportRecord = Pick<
  DecisionRecord,
  | "time"
  | "session"
  | "source"
  | "target"
  | "rule"
  | "ruleOutcome"
  | "ruleError"
  | "ruleAsksCut"
  | "ruleAsksPartial"
  | "outcome"
>;

/** What the reader reads of a project's artifact: the two names it matches, and the id it reads. */
export interface ReportArtifact {
  readonly id: string;
  readonly name: string;
  readonly slug: string;
}

/**
 * What the reader needs of a Dispatch client, which is the two calls it makes. Declared
 * structurally, over the fields it reads, so the host's own `DispatchClient` satisfies it and a
 * test drives it with a plain object.
 */
export interface ReportClient {
  listProjectArtifacts(project: string): Promise<readonly ReportArtifact[]>;
  docRead(id: string): Promise<{ readonly markdown: string }>;
}

/** One fenced line as a decision record, or null when it is not one. */
export function parseRecordLine(line: string): ReportRecord | null {
  let value: unknown;
  try {
    value = JSON.parse(line);
  } catch {
    return null;
  }
  if (!isObject(value)) return null;
  // The four fields every count rests on. A line missing one of them belongs in no count at all,
  // so it is reported as a line that would not parse rather than counted as a decision.
  const time = str(value.time);
  const session = str(value.session);
  const source = value.source;
  const ruleOutcome = value.ruleOutcome;
  if (!Number.isFinite(Date.parse(time)) || session === "") return null;
  if (source !== "pod" && source !== "machine") return null;
  // Null on an override — the record of a call the agent sent anyway, which no rules check judged
  // — and a string on a rules call; a line holding anything else there is no record at all.
  if (ruleOutcome !== null && typeof ruleOutcome !== "string") return null;
  return {
    time,
    session,
    source,
    target: strOrNull(value.target),
    rule: strOrNull(value.rule),
    ruleOutcome,
    ruleError: strOrNull(value.ruleError),
    // A record written before the gate counted the asks holds none, which is no ask cut.
    ruleAsksCut: num(value.ruleAsksCut) ?? 0,
    // A record written before the gate told the two apart holds nothing here, which is a call
    // judged on all of its asks.
    ruleAsksPartial: value.ruleAsksPartial === true,
    outcome: str(value.outcome),
  };
}

/** The records of one decision document, and how many of its fenced lines were not records. */
export function parseDecisionDocument(markdown: string): {
  records: ReportRecord[];
  unparsed: number;
} {
  const records: ReportRecord[] = [];
  let unparsed = 0;
  let inJson = false;
  for (const line of markdown.split("\n")) {
    const text = line.trim();
    if (text.startsWith("```")) {
      // A fence either closes the block it is in or opens one, and only a json block holds records.
      inJson = !inJson && text.slice(3).trim() === "json";
      continue;
    }
    if (!inJson || text === "") continue;
    const record = parseRecordLine(text);
    if (record === null) unparsed += 1;
    else records.push(record);
  }
  return { records, unparsed };
}

/** What a project's decision documents held, with the losses the read leaves visible. */
export interface DocumentRead {
  records: ReportRecord[];
  /** Documents whose markdown was read. */
  documents: number;
  /** Documents the read refused: their decisions are missing from every count below. */
  unreadable: number;
  /** Fenced lines that were not records: a decision half-written, or something else entirely. */
  unparsedLines: number;
}

/** How many decision documents are read at once, as `scripts/dispatch/ask-census.ts` reads issues. */
const DOCUMENT_CONCURRENCY = 8;

/**
 * Reads every decision document of the project, keeping the records at or after since (epoch ms),
 * `DOCUMENT_CONCURRENCY` of them in flight: a fortnight holds one document per session, each one
 * round trip, and read one at a time they are the whole run. A document that cannot be read is
 * counted rather than thrown: one session's loss leaves the rest of the day's counts standing,
 * and the count says how much of the day is missing.
 */
export async function readDecisionDocuments(
  client: ReportClient,
  project: string,
  since: number
): Promise<DocumentRead> {
  const read: DocumentRead = { records: [], documents: 0, unreadable: 0, unparsedLines: 0 };
  const artifacts = (await client.listProjectArtifacts(project)).filter(
    (artifact) =>
      artifact.name.startsWith(DECISION_DOCUMENT_PREFIX) ||
      artifact.slug.startsWith(DECISION_DOCUMENT_PREFIX)
  );
  // Each document's text, or undefined when its read failed, kept in the artifacts' own order, so
  // the counts and the records below read as one serial pass does.
  const markdowns: (string | undefined)[] = [];
  let next = 0;
  const reader = async (): Promise<void> => {
    while (next < artifacts.length) {
      const index = next++;
      try {
        markdowns[index] = (await client.docRead(artifacts[index].id)).markdown;
      } catch {
        markdowns[index] = undefined;
      }
    }
  };
  await Promise.all(
    Array.from({ length: Math.min(DOCUMENT_CONCURRENCY, artifacts.length) }, reader)
  );
  for (const markdown of markdowns) {
    if (markdown === undefined) {
      read.unreadable += 1;
      continue;
    }
    read.documents += 1;
    const { records, unparsed } = parseDecisionDocument(markdown);
    read.unparsedLines += unparsed;
    for (const record of records) if (Date.parse(record.time) >= since) read.records.push(record);
  }
  return read;
}

/** The day's counts over one set of records: one source's, or every source together. */
export interface DecisionCounts {
  /**
   * The rules calls: the records a rules check judged, which is the error rate's denominator. An
   * override is a record of its own and none of them, so overriding a refusal never moves it.
   */
  calls: number;
  /** Records with a rule: the calls the rules check refused. */
  refusals: number;
  /** The refusals by rule, the rules the gate ships named even at zero. */
  byRule: Record<string, number>;
  /** Refusals a later record on the same target, in the same session, overrode. */
  overrides: number;
  /** Rules calls that carried an error text, timed out, or gave no verdict. */
  errors: number;
  /**
   * Rules calls whose request did not carry every new ask whole (`ruleAsksCut`): the check judged
   * clipped text, so what it did not read could match no rule.
   */
  cut: number;
  /**
   * Rules calls judged on part of their asks (`ruleAsksPartial`): an upload whose stored version
   * the gate could not read, so the asks that version may already hold went unjudged.
   */
  partial: number;
}

/** One day's decisions over one Dispatch project, as both output modes print them. */
export interface DispatchReport {
  project: string;
  /** `--since`, as given. */
  since: string;
  documents: number;
  unreadableDocuments: number;
  /** Sessions the kept records name: the sessions that made a decision in the day. */
  sessions: number;
  unparsedLines: number;
  total: DecisionCounts;
  pod: DecisionCounts;
  machine: DecisionCounts;
}

/** The counts of one source's records; `overridden` holds the day's overridden refusals. */
function countDecisions(
  records: readonly ReportRecord[],
  overridden: ReadonlySet<ReportRecord>
): DecisionCounts {
  const refusals = records.filter((record) => record.rule !== null);
  const calls = records.filter((record) => record.ruleOutcome !== null);
  return {
    calls: calls.length,
    refusals: refusals.length,
    byRule: Object.fromEntries(
      tally(
        refusals.map((record) => record.rule as string),
        RULES
      ).map((count) => [count.name, count.count])
    ),
    overrides: refusals.filter((record) => overridden.has(record)).length,
    errors: calls.filter(
      (record) => record.ruleError !== null || ERROR_OUTCOMES.includes(record.ruleOutcome as string)
    ).length,
    cut: calls.filter((record) => record.ruleAsksCut > 0).length,
    partial: calls.filter((record) => record.ruleAsksPartial).length,
  };
}

export function computeDispatchReport(
  read: DocumentRead,
  project: string,
  since: string
): DispatchReport {
  // The walk runs over the whole day, so a refusal and the rebuttal that overrode it meet even
  // where one source's records are counted on their own; each override lands on its refusal's.
  const overridden = new Set(
    overrides(read.records, (record) => record.time).map(({ refusal }) => refusal)
  );
  const of = (source: ReportRecord["source"]): readonly ReportRecord[] =>
    read.records.filter((record) => record.source === source);
  return {
    project,
    since,
    documents: read.documents,
    unreadableDocuments: read.unreadable,
    sessions: new Set(read.records.map((record) => record.session)).size,
    unparsedLines: read.unparsedLines,
    total: countDecisions(read.records, overridden),
    pod: countDecisions(of("pod"), overridden),
    machine: countDecisions(of("machine"), overridden),
  };
}

function bySource(report: DispatchReport, count: (counts: DecisionCounts) => number): string {
  return `${count(report.total)} (pod ${count(report.pod)}, machine ${count(report.machine)})`;
}

export function formatDispatchReport(report: DispatchReport): string[] {
  return [
    "A rules call is one decision record whose rules check ran; every count below but the overrides is over those alone.",
    "An override is the other record a document holds: a refused call the agent sent again, which no rules check judged and which no rules call count holds.",
    "A rules refusal is a rules call with a rule. It was overridden when the next record on its target, in its session, is a rebuttal or a breaker.",
    "A document that could not be read, and a line that is not a record, are counted below: the decisions they held are missing from every other count.",
    "A rules call with asks cut is one whose request did not carry every new ask of the call whole: the check judged clipped text.",
    "A rules call judged on part of its asks is an upload whose stored version the gate could not read: it judged the asks no stored version can hold and left the rest alone.",
    `project: ${report.project}`,
    `since: ${report.since}`,
    `documents: ${report.documents}`,
    `unreadable documents: ${report.unreadableDocuments}`,
    `sessions: ${report.sessions}`,
    `unparsed lines: ${report.unparsedLines}`,
    `rules calls: ${bySource(report, (c) => c.calls)}`,
    `refusals: ${bySource(report, (c) => c.refusals)}`,
    ...Object.keys(report.total.byRule).map(
      (rule) =>
        `rule ${rule}: ${report.total.byRule[rule] ?? 0} (pod ${report.pod.byRule[rule] ?? 0}, ` +
        `machine ${report.machine.byRule[rule] ?? 0})`
    ),
    `overrides: ${bySource(report, (c) => c.overrides)}`,
    `rules errors: ${bySource(report, (c) => c.errors)}`,
    `rules calls with asks cut: ${bySource(report, (c) => c.cut)}`,
    `rules calls judged on part of their asks: ${bySource(report, (c) => c.partial)}`,
    `rules error rate: ${share(report.total.errors, report.total.calls)} (pod ` +
      `${share(report.pod.errors, report.pod.calls)}, machine ` +
      `${share(report.machine.errors, report.machine.calls)})`,
  ];
}

/** What the Dispatch mode prints: with --json one object and nothing else, else the plain text. */
export function dispatchReportLines(report: DispatchReport, json: boolean): string[] {
  return json ? [JSON.stringify(report)] : formatDispatchReport(report);
}

/** The session's own Dispatch, as a standing session holding a token and nothing else resolves it. */
function sessionDispatch(): DispatchClient {
  const { dispatch, warnings } = sessionDispatchSettings(
    sessionConfigFile("command", process.env),
    process.env,
    { cwd: process.cwd() }
  );
  // Said on stderr, so --json keeps stdout to the one object.
  for (const warning of warnings) console.error(`askgate-report: ${warning}`);
  const config = activeDispatchSettings(dispatch);
  if (config === null) {
    throw new Error(`Dispatch is not configured: ${dispatch.enabled ? "" : dispatch.notice}`);
  }
  return new DispatchClient(config.url, config.token, fetch);
}

function nonNegativeInt(flag: string, value: string): number {
  if (!/^\d+$/.test(value)) throw new Error(`${flag} must be a non-negative integer`);
  return Number(value);
}

/** Parses the command line, in either mode. Throws an error naming the flag at fault. */
export function parseArgs(argv: string[]): Args {
  const values: Record<string, string> = {};
  let json = false;
  const known = ["--sessions", "--since", "--sample", "--out", "--seed", "--dispatch-project"];
  for (let i = 0; i < argv.length; i++) {
    const flag = argv[i] as string;
    if (flag === "--json") {
      json = true;
      continue;
    }
    if (!known.includes(flag)) throw new Error(`unknown flag ${flag}`);
    const value = argv[i + 1];
    if (value === undefined || value.startsWith("--")) throw new Error(`${flag} needs a value`);
    values[flag] = value;
    i++;
  }
  const project = values["--dispatch-project"];
  // An empty key would be read as a project name and die on a 404 from Dispatch, so it is refused
  // here by name, as `scripts/dispatch/ask-census.ts` refuses an empty --project.
  if (project === "") throw new Error("--dispatch-project requires a project key");
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
