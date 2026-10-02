import { readFile } from "node:fs/promises";
import { parseArgs } from "node:util";
import type {
  ArtifactReviewEventPayload,
  Ask,
  AskApproval,
  DispatchEvent,
  Issue,
} from "../../packages/contracts/src/dispatch-api";
import {
  type ActiveDispatchConfig,
  activeDispatchConfig,
} from "../../packages/envoy-client/src/dispatch-config";

const DEFAULT_PROJECTS = ["AGENTC", "LEGION", "OPS"] as const;
const CODES = ["to-do", "design", "may-I-proceed", "operations"] as const;
const EVENT_PAGE_SIZE = 200;

type Code = (typeof CODES)[number];

type DispatchConfig = Pick<ActiveDispatchConfig, "url" | "token">;

/** The fields of an ask the census reads. */
export type CensusAsk = Pick<
  Ask,
  "id" | "created_at" | "kind" | "block_id" | "question" | "author"
>;

/** An approval as an `ask.*` event recorded it; events written before F1 carry no
 * `requested_version`. */
type RecordedApproval = Pick<AskApproval, "artifact_id" | "version"> &
  Partial<Pick<AskApproval, "requested_version">>;

/**
 * The fields of a recorded Dispatch event the approval count reads. The history it reads predates
 * parts of today's contract, so each payload field is optional and checked before use.
 */
export interface CensusEvent {
  readonly id: DispatchEvent["id"];
  readonly type: string;
  readonly actor?: { readonly kind: string; readonly id: string };
  readonly payload: {
    /** The ask an `ask.*` event carries. */
    readonly id?: Ask["id"];
    readonly kind?: Ask["kind"];
    /** The ask a comment replied to or a review answered. */
    readonly ask_id?: ArtifactReviewEventPayload["ask_id"];
    readonly approval?: RecordedApproval;
  };
}

/** An event as `GET /api/v1/issues/{key}/events` returns it. */
type RecordedEvent = CensusEvent & Pick<DispatchEvent, "seq" | "created_at">;

interface ApprovalRound {
  readonly artifactId: string;
  readonly inboxRows: number;
  readonly handbacks: number;
  readonly humanTurns: number;
  readonly exceedsHumanTurnBudget: boolean;
}

interface AskSummary {
  readonly asks: number;
  readonly decisionBlocks: number;
  readonly standaloneQuestions: number;
  readonly approvalRequests: number;
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

interface MutableApprovalRound {
  inboxRows: number;
  handbacks: number;
  humanTurnKeys: Set<string>;
}

/** What one issue adds to each table. */
interface IssueCensus {
  readonly windowAsks: readonly CensusAsk[];
  readonly issueRow: (AskSummary & { issue: string }) | undefined;
  readonly approvalRounds: Array<ApprovalRound & { issue: string }>;
  readonly standaloneQuestions: Array<CensusAsk & { issue: string }>;
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

/** Text a session or the server chose, with control characters escaped, so a printed table
 * carries no terminal escape sequence. */
function printable(text: string): string {
  return text.replace(
    /\p{Cc}/gu,
    (character) => `\\u${(character.codePointAt(0) ?? 0).toString(16).padStart(4, "0")}`
  );
}

function fromExcludedSession(
  actor: { readonly kind: string; readonly id: string } | undefined,
  excludedSessionIds: ReadonlySet<string>
): boolean {
  return actor?.kind === "session" && excludedSessionIds.has(actor.id);
}

export function filterAsksInWindow<T extends Pick<CensusAsk, "created_at">>(
  asks: readonly T[],
  from: string,
  to: string
): T[] {
  const fromTime = date(from, "from");
  const toTime = date(to, "to");
  return asks.filter((ask) => inWindow(ask.created_at, fromTime, toTime));
}

export function excludeSessionAsks<T extends Pick<CensusAsk, "author">>(
  asks: readonly T[],
  excludedSessionIds: ReadonlySet<string>
): T[] {
  return asks.filter((ask) => !fromExcludedSession(ask.author, excludedSessionIds));
}

function isStandaloneQuestion(ask: Pick<CensusAsk, "kind" | "block_id">): boolean {
  return ask.kind === "question" && ask.block_id === null;
}

export function summarizeAsks(asks: readonly Pick<CensusAsk, "kind" | "block_id">[]): AskSummary {
  let decisionBlocks = 0;
  let standaloneQuestions = 0;
  let approvalRequests = 0;
  for (const ask of asks) {
    if (ask.block_id !== null && ask.block_id !== undefined) decisionBlocks += 1;
    if (isStandaloneQuestion(ask)) standaloneQuestions += 1;
    if (ask.kind === "approval") approvalRequests += 1;
  }
  return { asks: asks.length, decisionBlocks, standaloneQuestions, approvalRequests };
}

function approvalRound(
  rounds: Map<string, MutableApprovalRound>,
  artifactId: string
): MutableApprovalRound {
  const existing = rounds.get(artifactId);
  if (existing !== undefined) return existing;
  const created = { inboxRows: 0, handbacks: 0, humanTurnKeys: new Set<string>() };
  rounds.set(artifactId, created);
  return created;
}

/**
 * Counts, per document, every time an approval request reached the human's Inbox (`handbacks`):
 * the request's opening `ask.opened`, and each hand-back, an `ask.edited` that sets
 * `requested_version` to `version`. A move, the `ask.edited` a new version writes, leaves
 * `requested_version` below `version` and reaches nobody. Before F1 every request opened its own
 * row, so the same rule counts each one. A round with more arrivals than human turns plus one is
 * flagged.
 */
export function summarizeApprovalRounds(events: readonly CensusEvent[]): ApprovalRound[] {
  const rounds = new Map<string, MutableApprovalRound>();
  const approvalAskArtifacts = new Map<string, string>();

  for (const { type, payload } of events) {
    if (payload.kind !== "approval" || payload.id === undefined || payload.approval === undefined) {
      continue;
    }
    const { artifact_id: artifactId, version, requested_version } = payload.approval;
    approvalAskArtifacts.set(payload.id, artifactId);
    const round = approvalRound(rounds, artifactId);
    if (type === "ask.opened") round.inboxRows += 1;
    if (type === "ask.opened" || (type === "ask.edited" && requested_version === version)) {
      round.handbacks += 1;
    }
  }

  for (const event of events) {
    if (event.actor?.kind !== "user") continue;
    const askId = event.payload.ask_id ?? event.payload.id;
    if (askId === undefined) continue;
    const artifactId = approvalAskArtifacts.get(askId);
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

  return [...rounds]
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
  asks: readonly Pick<CensusAsk, "id" | "kind" | "block_id">[],
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
    if (!isStandaloneQuestion(ask)) continue;
    totals[codes[ask.id] ?? "uncoded"] += 1;
  }
  return totals;
}

export function summarizeSessions(
  asks: readonly Pick<CensusAsk, "created_at" | "author">[],
  excludedSessionIds: ReadonlySet<string>
): { dropped: number; sessions: SessionSummary[] } {
  const sessions = new Map<string, SessionSummary>();
  let dropped = 0;
  for (const { author, created_at } of asks) {
    if (author.kind !== "session") continue;
    if (fromExcludedSession(author, excludedSessionIds)) {
      dropped += 1;
      continue;
    }
    const current = sessions.get(author.id);
    sessions.set(
      author.id,
      current === undefined
        ? {
            sessionId: author.id,
            machine: author.origin?.machine ?? "unknown",
            title: author.origin?.session_title ?? "unknown",
            firstAsk: created_at,
            lastAsk: created_at,
            asks: 1,
          }
        : {
            ...current,
            firstAsk: current.firstAsk < created_at ? current.firstAsk : created_at,
            lastAsk: current.lastAsk > created_at ? current.lastAsk : created_at,
            asks: current.asks + 1,
          }
    );
  }
  return {
    dropped,
    sessions: [...sessions.values()].sort(
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
  const { values } = parseArgs({
    args: [...argv],
    options: {
      from: { type: "string" },
      to: { type: "string" },
      project: { type: "string", multiple: true },
      "exclude-session": { type: "string", multiple: true },
      codes: { type: "string" },
    },
    strict: true,
  });
  const { from, to, codes: codesPath } = values;
  const projects = values.project ?? [];
  const excludedSessionIds = new Set(values["exclude-session"]);
  if (from === undefined || to === undefined) throw new Error("--from and --to are required");
  if (date(from, "from") >= date(to, "to")) throw new Error("--from must be before --to");
  if (projects.includes("")) throw new Error("--project requires a project key");
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

async function get<T>(
  config: DispatchConfig,
  path: string,
  fetchImpl: typeof fetch = fetch
): Promise<T> {
  const response = await fetchImpl(`${config.url}${path}`, {
    headers: { Authorization: `Bearer ${config.token}` },
  });
  if (!response.ok) throw new Error(`GET ${path}: ${response.status} ${await response.text()}`);
  return (await response.json()) as T;
}

export async function fetchIssueEvents(
  config: DispatchConfig,
  issueKey: string,
  fetchImpl: typeof fetch = fetch
): Promise<RecordedEvent[]> {
  const events: RecordedEvent[] = [];
  let after = 0;
  while (true) {
    const page = await get<RecordedEvent[]>(
      config,
      `/api/v1/issues/${encodeURIComponent(issueKey)}/events?limit=${EVENT_PAGE_SIZE}&after=${after}`,
      fetchImpl
    );
    events.push(...page);
    if (page.length < EVENT_PAGE_SIZE) return events;
    const last = page.at(-1);
    if (last === undefined || last.seq <= after) {
      throw new Error(`GET /api/v1/issues/${issueKey}/events returned no seq after ${after}`);
    }
    after = last.seq;
  }
}

async function censusIssue(
  config: DispatchConfig,
  issue: string,
  options: CensusOptions
): Promise<IssueCensus> {
  const fromTime = date(options.from, "from");
  const toTime = date(options.to, "to");
  const issueAsks = await get<CensusAsk[]>(
    config,
    `/api/v1/issues/${encodeURIComponent(issue)}/asks`
  );
  const windowAsks = filterAsksInWindow(issueAsks, options.from, options.to);
  const asks = excludeSessionAsks(windowAsks, options.excludedSessionIds);
  // One approval row follows its document from the first request until a human answers it (F1),
  // so a request opened before the window can be handed back inside it. Approval rounds come from
  // the in-window events of every issue that carries an approval ask, whenever it was opened.
  const events = issueAsks.some((ask) => ask.kind === "approval")
    ? (await fetchIssueEvents(config, issue)).filter(
        (event) =>
          inWindow(event.created_at, fromTime, toTime) &&
          !fromExcludedSession(event.actor, options.excludedSessionIds)
      )
    : [];
  return {
    windowAsks,
    issueRow: asks.length === 0 ? undefined : { issue, ...summarizeAsks(asks) },
    approvalRounds: summarizeApprovalRounds(events).map((round) => ({ issue, ...round })),
    standaloneQuestions: asks.filter(isStandaloneQuestion).map((ask) => ({ ...ask, issue })),
  };
}

async function run(options: CensusOptions): Promise<void> {
  const config = activeDispatchConfig(process.env);
  if (config === null) {
    throw new Error(
      "Dispatch is not configured: set DISPATCH_URL with DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or enable dispatch in ~/.config/opencode/envoy.json"
    );
  }
  const codes =
    options.codesPath === undefined ? {} : parseCodes(await readFile(options.codesPath, "utf8"));
  // Without limit or offset the issues route answers every matching issue in one array, so this
  // read is deliberately unpaged. Every event bumps an issue's updated_at, so the list holds every
  // issue with activity in the window.
  const projectIssues = await Promise.all(
    options.projects.map((project) =>
      get<Pick<Issue, "key">[]>(
        config,
        `/api/v1/issues?project=${encodeURIComponent(project)}&updated_since=${encodeURIComponent(options.from)}`
      )
    )
  );
  // Each issue is read concurrently; the results keep project and issue order, which the approval
  // rounds table prints in.
  const census = await Promise.all(
    projectIssues.flat().map((issue) => censusIssue(config, issue.key, options))
  );
  const issueRows = census.flatMap((issue) =>
    issue.issueRow === undefined ? [] : [issue.issueRow]
  );
  const approvalRounds = census.flatMap((issue) => issue.approvalRounds);
  const standaloneQuestions = census.flatMap((issue) => issue.standaloneQuestions);

  const totals = issueRows.reduce(
    (sum, row) => ({
      asks: sum.asks + row.asks,
      decisionBlocks: sum.decisionBlocks + row.decisionBlocks,
      standaloneQuestions: sum.standaloneQuestions + row.standaloneQuestions,
      approvalRequests: sum.approvalRequests + row.approvalRequests,
    }),
    { asks: 0, decisionBlocks: 0, standaloneQuestions: 0, approvalRequests: 0 }
  );
  const sessions = summarizeSessions(
    census.flatMap((issue) => issue.windowAsks),
    options.excludedSessionIds
  );
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
        question: printable(ask.question),
      }))
  );
  console.log("Coding totals");
  console.table([applyCodes(standaloneQuestions, codes)]);
  console.log(`Excluded asks from old-plugin sessions: ${sessions.dropped}`);
  console.log("Sessions");
  console.table(
    sessions.sessions.map((session) => ({
      ...session,
      sessionId: printable(session.sessionId),
      machine: printable(session.machine),
      title: printable(session.title),
    }))
  );
  const flagged = approvalRounds.filter((round) => round.exceedsHumanTurnBudget);
  if (flagged.length > 0) {
    console.log("Approval rounds above the human-turn budget");
    console.table(flagged);
  }
}

if (import.meta.main) await run(parseArguments(Bun.argv.slice(2)));
