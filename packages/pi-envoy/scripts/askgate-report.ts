// Reports on AskGate's advisor-gate session entries and samples the reviews it sent back.
// Usage: bun askgate-report.ts --sessions <dir> --since <RFC 3339> --sample <n> --out <dir>
//   [--seed <n>]
import { mkdir, readdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";

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
  perTool: ToolShare[];
  /** Every outcome seen and how many entries carried it, most first. */
  byOutcome: Array<[string, number]>;
  /** The sessions where the gate reviewed nothing at least once, named with their counts. */
  unreviewedSessions: SessionOutcomes[];
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
      reason: typeof reason === "string" ? reason : null,
      latencyMs: num(data.latencyMs),
      cost: num(usage.cost),
      text: reviewed,
      textSource: reviewed === null ? "command" : "reviewed",
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

/**
 * The gate did not pass the write as sent: a revise, a call the gate could not read (which in warn
 * mode runs with the reason returned, so its decision is `allow`), or a gate that did not load.
 */
export function isSentBack(record: GateRecord): boolean {
  return (
    record.decision === "revise" ||
    record.outcome === "unreadable" ||
    record.outcome === "load-failed"
  );
}

/**
 * A write that did not post: sent back in block mode, or refused in every mode. `no-model-role`
 * and `load-failed` mean the gate can review nothing, so each refuses in warn mode too; an
 * `unreadable` call is one call's form, which warn mode lets through.
 */
export function isRefusal(record: GateRecord): boolean {
  return (
    isSentBack(record) &&
    (record.verdictMode === "block" ||
      record.outcome === "no-model-role" ||
      record.outcome === "load-failed")
  );
}

/** Nearest-rank percentile; null when there are no values. */
export function percentile(values: number[], p: number): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const rank = Math.max(1, Math.ceil((p / 100) * sorted.length));
  return sorted[rank - 1] ?? null;
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
    const share = byTool.get(record.tool) ?? { tool: record.tool, calls: 0, reviews: 0, sentBack: 0 };
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
    perTool: [...byTool.values()].sort((a, b) => (a.tool < b.tool ? -1 : a.tool > b.tool ? 1 : 0)),
    byOutcome: [...byOutcome.entries()].sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1)),
    unreviewedSessions: [...bySession.entries()]
      .map(([session, counts]) => ({
        session,
        counts: [...counts.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1)),
      }))
      .sort((a, b) => (a.session < b.session ? -1 : 1)),
  };
}

function share(part: number, whole: number): string {
  return whole === 0 ? "n/a" : `${((part / whole) * 100).toFixed(1)}%`;
}

function ms(value: number | null): string {
  return value === null ? "n/a" : `${value} ms`;
}

export function formatReport(stats: Stats, seed: number): string[] {
  const cost =
    stats.costMedian === null
      ? "n/a"
      : `$${stats.costMedian.toFixed(4)} (${stats.costCount} reviews with a cost)`;
  return [
    "A gated call is any advisor-gate entry. A review is one where the model gave a verdict.",
    "Sent back means the gate did not pass the write as sent: a revise verdict, a call it could",
    "not read, or a gate that did not load. It is counted over every gated call, since two of",
    "those three never reach a review. A refusal is a write that did not post: one sent back",
    "in block mode, and `no-model-role` and `load-failed`, which refuse in warn mode too.",
    "Unreviewed is a write that posted with no review behind it: every allow whose outcome is no",
    "verdict, a rebuttal, a breaker trip and a warn-mode unreadable call among them.",
    "A session named below had at least one such write, with each outcome's count.",
    `gated calls: ${stats.gatedCalls}`,
    `reviews: ${stats.reviews}`,
    `sent back: ${share(stats.sentBack, stats.gatedCalls)} (${stats.sentBack} of ${stats.gatedCalls} gated calls)`,
    `review latency median: ${ms(stats.latencyMedian)}`,
    `review latency p90: ${ms(stats.latencyP90)}`,
    `review cost median: ${cost}`,
    `refusals: ${stats.refusals}`,
    `unreviewed sends: ${stats.unreviewed}`,
    ...stats.byOutcome.map(([outcome, count]) => `outcome ${outcome}: ${count}`),
    ...stats.perTool.map(
      (t) =>
        `tool ${t.tool}: ${t.calls} gated calls, ${t.reviews} reviews, sent back ${share(t.sentBack, t.calls)} (${t.sentBack})`
    ),
    ...stats.unreviewedSessions.map(
      ({ session, counts }) =>
        `session ${session}: ${counts.map(([outcome, count]) => `${outcome} ${count}`).join(", ")}`
    ),
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
    reason: record.reason,
    textSource: record.textSource,
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
