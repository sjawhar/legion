import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";

const DEFAULT_PROJECTS = ["AGENTC", "LEGION", "OPS"] as const;
const CODES = ["to-do", "design", "may-I-proceed", "operations"] as const;

type Code = (typeof CODES)[number];

type UnknownRecord = Record<string, unknown>;

interface CensusAuthor {
  readonly kind?: string;
  readonly id?: string;
  readonly origin?: {
    readonly machine?: string;
    readonly session_title?: string;
  };
}

export interface CensusAsk {
  readonly id: string;
  readonly created_at: string;
  readonly kind: string;
  readonly block_id?: string | null;
  readonly question?: string;
  readonly author?: CensusAuthor;
}

export interface CensusEvent {
  readonly id: number | string;
  readonly type: string;
  readonly actor?: CensusAuthor;
  readonly created_at?: string;
  readonly payload: unknown;
}

interface ApprovalRound {
  readonly artifactId: string;
  readonly inboxRows: number;
  readonly handbacks: number;
  readonly humanTurns: number;
  readonly exceedsHumanTurnBudget: boolean;
}

interface SessionSummary {
  readonly sessionId: string;
  readonly machine: string;
  readonly title: string;
  readonly firstAsk: string;
  readonly lastAsk: string;
  readonly asks: number;
}

interface CensusOptions {
  readonly from: string;
  readonly to: string;
  readonly projects: readonly string[];
  readonly excludedSessionIds: ReadonlySet<string>;
  readonly codesPath?: string;
}

interface DispatchConfig {
  readonly url: string;
  readonly token: string;
}

interface IssueSummary {
  readonly key: string;
  readonly title?: string;
}

interface MutableApprovalRound {
  inboxRows: number;
  handbacks: number;
  humanTurnKeys: Set<string>;
}

function record(value: unknown): UnknownRecord | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as UnknownRecord)
    : undefined;
}

function string(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

function date(value: string, name: string): number {
  const parsed = Date.parse(value);
  if (Number.isNaN(parsed)) throw new Error(`${name} must be an ISO timestamp: ${value}`);
  return parsed;
}

function inWindow(timestamp: string, from: number, to: number): boolean {
  const value = date(timestamp, "timestamp");
  return value >= from && value < to;
}

export function filterAsksInWindow<T extends CensusAsk>(
  asks: readonly T[],
  from: string,
  to: string
): T[] {
  const fromTime = date(from, "from");
  const toTime = date(to, "to");
  return asks.filter((ask) => inWindow(ask.created_at, fromTime, toTime));
}

export function excludeSessionAsks<T extends CensusAsk>(
  asks: readonly T[],
  excludedSessionIds: ReadonlySet<string>
): T[] {
  return asks.filter(
    (ask) =>
      ask.author?.kind !== "session" ||
      ask.author.id === undefined ||
      !excludedSessionIds.has(ask.author.id)
  );
}

export function summarizeAsks(asks: readonly CensusAsk[]): {
  asks: number;
  decisionBlocks: number;
  standaloneQuestions: number;
  approvalRequests: number;
} {
  return {
    asks: asks.length,
    decisionBlocks: asks.filter((ask) => ask.block_id !== null && ask.block_id !== undefined)
      .length,
    standaloneQuestions: asks.filter((ask) => ask.kind === "question" && ask.block_id === null)
      .length,
    approvalRequests: asks.filter((ask) => ask.kind === "approval").length,
  };
}

function approval(payload: unknown):
  | {
      artifactId: string;
      version?: number;
      requestedVersion?: number;
      askId?: string;
      kind?: string;
    }
  | undefined {
  const payloadRecord = record(payload);
  const approvalRecord = payloadRecord === undefined ? undefined : record(payloadRecord.approval);
  const artifactId = approvalRecord === undefined ? undefined : string(approvalRecord.artifact_id);
  if (artifactId === undefined) return undefined;
  return {
    artifactId,
    version: typeof approvalRecord.version === "number" ? approvalRecord.version : undefined,
    requestedVersion:
      typeof approvalRecord.requested_version === "number"
        ? approvalRecord.requested_version
        : undefined,
    askId: payloadRecord === undefined ? undefined : string(payloadRecord.id),
    kind: payloadRecord === undefined ? undefined : string(payloadRecord.kind),
  };
}

function approvalRound(
  rounds: Record<string, MutableApprovalRound>,
  artifactId: string
): MutableApprovalRound {
  const existing = rounds[artifactId];
  if (existing !== undefined) return existing;
  const created = { inboxRows: 0, handbacks: 0, humanTurnKeys: new Set<string>() };
  rounds[artifactId] = created;
  return created;
}

export function summarizeApprovalRounds(events: readonly CensusEvent[]): ApprovalRound[] {
  const rounds: Record<string, MutableApprovalRound> = {};
  const approvalAskArtifacts: Record<string, string> = {};

  for (const event of events) {
    const details = approval(event.payload);
    if (details?.kind !== "approval" || details.askId === undefined) continue;
    approvalAskArtifacts[details.askId] = details.artifactId;
    const round = approvalRound(rounds, details.artifactId);
    if (event.type === "ask.opened") {
      round.inboxRows += 1;
      if (details.requestedVersion === undefined) round.handbacks += 1;
    }
    if (
      event.type === "ask.edited" &&
      details.requestedVersion !== undefined &&
      details.version === details.requestedVersion
    ) {
      round.handbacks += 1;
    }
  }

  for (const event of events) {
    if (event.actor?.kind !== "user") continue;
    const payload = record(event.payload);
    if (payload === undefined) continue;
    const askId = string(payload.ask_id) ?? string(payload.id);
    if (askId === undefined) continue;
    const artifactId = approvalAskArtifacts[askId];
    if (artifactId === undefined) continue;
    if (event.type === "comment.created") {
      approvalRound(rounds, artifactId).humanTurnKeys.add(`comment:${event.id}`);
    }
    if (
      event.type === "ask.answered" ||
      event.type === "artifact.approved" ||
      event.type === "artifact.changes_requested"
    ) {
      approvalRound(rounds, artifactId).humanTurnKeys.add(`answer:${askId}`);
    }
  }

  return Object.entries(rounds)
    .map(([artifactId, round]) => ({
      artifactId,
      inboxRows: round.inboxRows,
      handbacks: round.handbacks,
      humanTurns: round.humanTurnKeys.size,
      exceedsHumanTurnBudget: round.handbacks > round.humanTurnKeys.size + 1,
    }))
    .sort((left, right) => left.artifactId.localeCompare(right.artifactId));
}

export function applyCodes(
  asks: readonly CensusAsk[],
  codes: Readonly<Record<string, Code>>
): Record<Code | "uncoded", number> {
  const totals: Record<Code | "uncoded", number> = {
    "to-do": 0,
    design: 0,
    "may-I-proceed": 0,
    operations: 0,
    uncoded: 0,
  };
  for (const ask of asks) {
    if (ask.kind !== "question" || ask.block_id !== null) continue;
    totals[codes[ask.id] ?? "uncoded"] += 1;
  }
  return totals;
}

export function summarizeSessions(
  asks: readonly CensusAsk[],
  excludedSessionIds: ReadonlySet<string>
): { dropped: number; sessions: SessionSummary[] } {
  const sessions: Record<string, SessionSummary> = {};
  let dropped = 0;
  for (const ask of asks) {
    if (ask.author?.kind !== "session" || ask.author.id === undefined) continue;
    if (excludedSessionIds.has(ask.author.id)) {
      dropped += 1;
      continue;
    }
    const current = sessions[ask.author.id];
    const machine = ask.author.origin?.machine ?? "unknown";
    const title = ask.author.origin?.session_title ?? "unknown";
    if (current === undefined) {
      sessions[ask.author.id] = {
        sessionId: ask.author.id,
        machine,
        title,
        firstAsk: ask.created_at,
        lastAsk: ask.created_at,
        asks: 1,
      };
      continue;
    }
    sessions[ask.author.id] = {
      ...current,
      firstAsk: current.firstAsk < ask.created_at ? current.firstAsk : ask.created_at,
      lastAsk: current.lastAsk > ask.created_at ? current.lastAsk : ask.created_at,
      asks: current.asks + 1,
    };
  }
  return {
    dropped,
    sessions: Object.values(sessions).sort(
      (left, right) => right.asks - left.asks || left.sessionId.localeCompare(right.sessionId)
    ),
  };
}

export function parseCodes(source: string): Record<string, Code> {
  const parsed: Record<string, Code> = {};
  for (const [index, line] of source.split("\n").entries()) {
    const trimmed = line.trim();
    if (trimmed === "" || trimmed.startsWith("#")) continue;
    const [askId, code, extra] = trimmed.split(",").map((part) => part.trim());
    if (
      askId === "" ||
      code === undefined ||
      extra !== undefined ||
      !CODES.includes(code as Code)
    ) {
      throw new Error(`invalid code at line ${index + 1}: ${line}`);
    }
    parsed[askId] = code as Code;
  }
  return parsed;
}

function parseArguments(argv: readonly string[]): CensusOptions {
  let from: string | undefined;
  let to: string | undefined;
  const projects: string[] = [];
  const excludedSessionIds = new Set<string>();
  let codesPath: string | undefined;

  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    const value = argv[index + 1];
    if (argument === "--from") from = value;
    else if (argument === "--to") to = value;
    else if (argument === "--project") projects.push(value ?? "");
    else if (argument === "--exclude-session") excludedSessionIds.add(value ?? "");
    else if (argument === "--codes") codesPath = value;
    else throw new Error(`unknown argument: ${argument}`);
    index += 1;
  }

  if (from === undefined || to === undefined) throw new Error("--from and --to are required");
  if (date(from, "from") >= date(to, "to")) throw new Error("--from must be before --to");
  if (projects.some((project) => project === ""))
    throw new Error("--project requires a project key");
  if (excludedSessionIds.has("")) throw new Error("--exclude-session requires a session id");
  if (codesPath === "") throw new Error("--codes requires a file path");
  return {
    from,
    to,
    projects: projects.length === 0 ? DEFAULT_PROJECTS : projects,
    excludedSessionIds,
    ...(codesPath === undefined ? {} : { codesPath }),
  };
}

async function dispatchConfig(): Promise<DispatchConfig> {
  const path = join(homedir(), ".config", "opencode", "envoy.json");
  const configured = record(JSON.parse(await readFile(path, "utf8")));
  const dispatch = configured === undefined ? undefined : record(configured.dispatch);
  const url =
    process.env.DISPATCH_URL ?? (dispatch === undefined ? undefined : string(dispatch.serverUrl));
  const token =
    process.env.DISPATCH_TOKEN ?? (dispatch === undefined ? undefined : string(dispatch.token));
  if (url === undefined || token === undefined) {
    throw new Error(
      "set DISPATCH_URL and DISPATCH_TOKEN, or configure dispatch.serverUrl and dispatch.token"
    );
  }
  return { url: url.replace(/\/+$/, ""), token };
}

async function get<T>(config: DispatchConfig, path: string): Promise<T> {
  const response = await fetch(`${config.url}${path}`, {
    headers: { Authorization: `Bearer ${config.token}` },
  });
  if (!response.ok) throw new Error(`GET ${path}: ${response.status} ${await response.text()}`);
  return (await response.json()) as T;
}

async function run(options: CensusOptions): Promise<void> {
  const config = await dispatchConfig();
  const fromTime = date(options.from, "from");
  const toTime = date(options.to, "to");
  const codes =
    options.codesPath === undefined ? {} : parseCodes(await readFile(options.codesPath, "utf8"));
  const issueRows: {
    issue: string;
    asks: number;
    decisionBlocks: number;
    standaloneQuestions: number;
    approvalRequests: number;
  }[] = [];
  const approvalRounds: Array<ApprovalRound & { issue: string }> = [];
  const standaloneQuestions: Array<CensusAsk & { issue: string }> = [];
  const allAsks: CensusAsk[] = [];
  const allWindowAsks: CensusAsk[] = [];

  for (const project of options.projects) {
    const issues = await get<IssueSummary[]>(
      config,
      `/api/v1/issues?project=${encodeURIComponent(project)}&updated_since=${encodeURIComponent(options.from)}`
    );
    for (const issue of issues) {
      const windowAsks = filterAsksInWindow(
        await get<CensusAsk[]>(config, `/api/v1/issues/${encodeURIComponent(issue.key)}/asks`),
        options.from,
        options.to
      );
      allWindowAsks.push(...windowAsks);
      const asks = excludeSessionAsks(windowAsks, options.excludedSessionIds);
      if (asks.length === 0) continue;
      const events = (
        await get<CensusEvent[]>(config, `/api/v1/issues/${encodeURIComponent(issue.key)}/events`)
      ).filter(
        (event) =>
          (event.created_at === undefined || inWindow(event.created_at, fromTime, toTime)) &&
          (event.actor?.kind !== "session" ||
            event.actor.id === undefined ||
            !options.excludedSessionIds.has(event.actor.id))
      );
      issueRows.push({ issue: issue.key, ...summarizeAsks(asks) });
      approvalRounds.push(
        ...summarizeApprovalRounds(events).map((round) => ({ issue: issue.key, ...round }))
      );
      standaloneQuestions.push(
        ...asks
          .filter((ask) => ask.kind === "question" && ask.block_id === null)
          .map((ask) => ({ ...ask, issue: issue.key }))
      );
      allAsks.push(...asks);
    }
  }

  const totals = summarizeAsks(allAsks);
  const sessions = summarizeSessions(allWindowAsks, options.excludedSessionIds);
  console.log("Ask totals");
  console.table([totals]);
  console.log("Issue counts");
  console.table(
    issueRows.sort((left, right) => right.asks - left.asks || left.issue.localeCompare(right.issue))
  );
  console.log("Approval rounds");
  console.table(approvalRounds);
  console.log("Standalone question asks");
  console.table(
    standaloneQuestions
      .sort((left, right) => left.created_at.localeCompare(right.created_at))
      .map((ask) => ({
        issue: ask.issue,
        ask: ask.id,
        code: codes[ask.id] ?? "",
        question: ask.question ?? "",
      }))
  );
  console.log("Coding totals");
  console.table([applyCodes(allAsks, codes)]);
  console.log(`Excluded asks from old-plugin sessions: ${sessions.dropped}`);
  console.log("Sessions");
  console.table(sessions.sessions);
  const flagged = approvalRounds.filter((round) => round.exceedsHumanTurnBudget);
  if (flagged.length > 0) {
    console.log("Approval rounds above the human-turn budget");
    console.table(flagged);
  }
}

if (import.meta.main) await run(parseArguments(Bun.argv.slice(2)));
