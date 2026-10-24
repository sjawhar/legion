// Reports on AskGate's advisor-gate session entries, its rules check included, and samples the
// reviews it sent back.
// Usage: bun askgate-report.ts --sessions <dir> --since <RFC 3339> --sample <n> --out <dir>
//   [--seed <n>]
import { mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";

/** One advisor-gate entry, with the write's text joined from its tool call. */
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

export interface Args {
  sessions: string;
  since: number;
  sample: number;
  out: string;
  seed: number | null;
}

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
      text: null,
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
  for (const record of records) record.text = calls.get(record.toolCallId) ?? null;
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

export function isSentBack(record: GateRecord): boolean {
  return record.decision === "revise";
}

/** A refused call: AskGate did not send it. A rules refusal is one in warn mode too. */
export function isRefusal(record: GateRecord): boolean {
  return (
    record.decision === "revise" &&
    (record.verdictMode === "block" ||
      record.outcome === "unreadable" ||
      record.outcome === "no-model-role" ||
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

const RULES = ["no_change", "untried_access"];
const RULE_OUTCOMES = ["match", "none", "error", "timeout", "no-verdict", "abandoned", "shutdown"];
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

/**
 * Counts the rules refusals (entries with a rule) that the agent overrode: in each session, in time
 * order, the next entry with the refusal's target is a rebuttal or a breaker.
 */
export function overriddenRefusals(records: GateRecord[]): RuleStats["overridden"] {
  const overridden = { rebuttal: 0, breaker: 0 };
  const sessions = new Map<string, GateRecord[]>();
  for (const record of records) {
    const list = sessions.get(record.session) ?? [];
    list.push(record);
    sessions.set(record.session, list);
  }
  for (const list of sessions.values()) {
    // A stable sort by time: entries stamped in the same millisecond keep their file order.
    const ordered = list
      .map((record, index) => {
        const at = Date.parse(record.timestamp);
        return { record, index, at: Number.isFinite(at) ? at : Number.NEGATIVE_INFINITY };
      })
      .sort((a, b) => a.at - b.at || a.index - b.index);
    // Walk back from the end, so each target's next outcome is known when its refusal is reached.
    const next = new Map<string, string>();
    for (let i = ordered.length - 1; i >= 0; i--) {
      const { record } = ordered[i] as (typeof ordered)[number];
      if (record.target === null) continue;
      if (record.rule !== null) {
        const after = next.get(record.target);
        if (after === "rebuttal") overridden.rebuttal += 1;
        else if (after === "breaker") overridden.breaker += 1;
      }
      next.set(record.target, record.outcome);
    }
  }
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
    text: record.text,
  });
}

function nonNegativeInt(flag: string, value: string): number {
  if (!/^\d+$/.test(value)) throw new Error(`${flag} must be a non-negative integer`);
  return Number(value);
}

/** Parses the command line. Throws an error naming the flag at fault. */
export function parseArgs(argv: string[]): Args {
  const values: Record<string, string> = {};
  const known = ["--sessions", "--since", "--sample", "--out", "--seed"];
  for (let i = 0; i < argv.length; i++) {
    const flag = argv[i] as string;
    if (!known.includes(flag)) throw new Error(`unknown flag ${flag}`);
    const value = argv[i + 1];
    if (value === undefined || value.startsWith("--")) throw new Error(`${flag} needs a value`);
    values[flag] = value;
    i++;
  }
  for (const flag of ["--sessions", "--since", "--sample", "--out"]) {
    if (values[flag] === undefined) throw new Error(`${flag} is required`);
  }
  const sinceText = values["--since"] as string;
  const rfc3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/i;
  const since = Date.parse(sinceText);
  if (!rfc3339.test(sinceText) || !Number.isFinite(since)) {
    throw new Error("--since must be an RFC 3339 time");
  }
  const seedText = values["--seed"];
  return {
    sessions: values["--sessions"] as string,
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
  const seed = args.seed ?? (crypto.getRandomValues(new Uint32Array(1))[0] as number);
  const records = await readSessions(args.sessions, args.since);
  for (const line of formatReport(computeStats(records), seed)) console.log(line);
  const picks = sampleSentBack(records, args.sample, seed);
  await mkdir(args.out, { recursive: true });
  const body = picks.map(sampleLine).join("\n");
  await writeFile(join(args.out, "sample.jsonl"), body === "" ? "" : `${body}\n`);
}

if (import.meta.main) await main();
