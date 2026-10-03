import { readFile } from "node:fs/promises";
import { parseArgs } from "node:util";
import type {
  ArtifactReviewEventPayload,
  Ask,
  AskApproval,
  AskBlockArtifact,
  DispatchEvent,
  Issue,
} from "../../packages/contracts/src/dispatch-api";
import {
  type ActiveDispatchConfig,
  activeDispatchConfig,
} from "../../packages/envoy-client/src/dispatch-config";
import { DispatchClient } from "../../packages/envoy-client/src/dispatch-http";

const DEFAULT_PROJECTS = ["AGENTC", "LEGION", "OPS"] as const;
const CODES = ["to-do", "design", "may-I-proceed", "operations"] as const;
const EVENT_PAGE_SIZE = 200;

type Code = (typeof CODES)[number];

type DispatchConfig = Pick<ActiveDispatchConfig, "url" | "token">;

/** The fields of an ask the census reads. */
export type CensusAsk = Pick<
  Ask,
  "id" | "created_at" | "kind" | "block_id" | "block_artifact" | "question" | "author"
>;

/** The document an approval ask names, as every `ask.*` event of one records it. */
type RecordedApproval = Pick<AskApproval, "artifact_id">;

/** The document a decision block's ask lives in, as every `ask.*` event of one records it. */
type RecordedBlockArtifact = Pick<AskBlockArtifact, "id">;

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
    readonly block_artifact?: RecordedBlockArtifact;
  };
}

/** An event as `GET /api/v1/issues/{key}/events` returns it. */
type RecordedEvent = CensusEvent & Pick<DispatchEvent, "seq" | "created_at">;

interface ApprovalRound {
  readonly artifactId: string;
  readonly inboxRows: number;
  readonly arrivals: number;
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
  /** Where in the events the round's first approval event stands. */
  readonly start: number;
  inboxRows: number;
  arrivals: number;
  humanTurnKeys: Set<string>;
}

/** What one issue adds to each table. */
interface IssueCensus {
  readonly windowAsks: readonly CensusAsk[];
  /** `windowAsks` without the excluded sessions' asks. */
  readonly asks: readonly CensusAsk[];
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
  artifactId: string,
  start: number
): MutableApprovalRound {
  const existing = rounds.get(artifactId);
  if (existing !== undefined) return existing;
  const created = { start, inboxRows: 0, arrivals: 0, humanTurnKeys: new Set<string>() };
  rounds.set(artifactId, created);
  return created;
}

/**
 * Counts, per document, every time an approval request reached the human's Inbox (`arrivals`):
 * each `ask.opened` and each `ask.handed_back`. An `ask.edited` only rewords a request, by moving
 * it to a new version or giving it a new summary, so it never arrives; a hand-back with a new
 * summary is an `ask.edited` followed by its `ask.handed_back`, and arrives once. Before #1671,
 * when an approval request began following its document's versions, a request made again opened
 * a new row, a new `ask.opened`, so the same rule counts it.
 *
 * A human's turn is an answer to the request or a reply in its thread, and, once the round has
 * begun, an answer or a reply on a decision block in the document the request names: a choice a
 * request's thread raises becomes such a block, and the hand-back after its answer responds to
 * that turn. `asks` names each block's document for a reply on a block with no `ask.*` event among
 * `events`. A round with more arrivals than human turns plus one is flagged.
 */
export function summarizeApprovalRounds(
  events: readonly CensusEvent[],
  asks: readonly Pick<CensusAsk, "id" | "block_artifact">[] = []
): ApprovalRound[] {
  const rounds = new Map<string, MutableApprovalRound>();
  const approvalAskArtifacts = new Map<string, string>();
  const blockAskArtifacts = new Map<string, string>();
  for (const ask of asks) {
    if (ask.block_artifact !== undefined) blockAskArtifacts.set(ask.id, ask.block_artifact.id);
  }

  for (const [index, { type, payload }] of events.entries()) {
    if (payload.id === undefined) continue;
    if (payload.kind === "question" && payload.block_artifact !== undefined) {
      blockAskArtifacts.set(payload.id, payload.block_artifact.id);
    }
    if (payload.kind !== "approval" || payload.approval === undefined) continue;
    const artifactId = payload.approval.artifact_id;
    approvalAskArtifacts.set(payload.id, artifactId);
    const round = approvalRound(rounds, artifactId, index);
    if (type === "ask.opened") round.inboxRows += 1;
    if (type === "ask.opened" || type === "ask.handed_back") round.arrivals += 1;
  }

  for (const [index, event] of events.entries()) {
    if (event.actor?.kind !== "user") continue;
    const askId = event.payload.ask_id ?? event.payload.id;
    if (askId === undefined) continue;
    const approvalArtifactId = approvalAskArtifacts.get(askId);
    const artifactId = approvalArtifactId ?? blockAskArtifacts.get(askId);
    const round = artifactId === undefined ? undefined : rounds.get(artifactId);
    if (round === undefined) continue;
    // A block answered before the round's first request was part of writing the document, not a
    // turn in the round.
    if (approvalArtifactId === undefined && index < round.start) continue;
    if (event.type === "comment.created") round.humanTurnKeys.add(`comment:${event.id}`);
    if (
      event.type === "ask.answered" ||
      event.type === "artifact.approved" ||
      event.type === "artifact.changes_requested"
    ) {
      round.humanTurnKeys.add(`answer:${askId}`);
    }
  }

  return [...rounds]
    .map(([artifactId, round]) => ({
      artifactId,
      inboxRows: round.inboxRows,
      arrivals: round.arrivals,
      humanTurns: round.humanTurnKeys.size,
      exceedsHumanTurnBudget: round.arrivals > round.humanTurnKeys.size + 1,
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

async function get<T>(config: DispatchConfig, path: string): Promise<T> {
  const response = await fetch(`${config.url}${path}`, {
    headers: { Authorization: `Bearer ${config.token}` },
  });
  if (!response.ok) throw new Error(`GET ${path}: ${response.status} ${await response.text()}`);
  return (await response.json()) as T;
}

export async function fetchIssueEvents(
  client: DispatchClient,
  issueKey: string
): Promise<RecordedEvent[]> {
  const events: RecordedEvent[] = [];
  let after = 0;
  while (true) {
    // The client types an event by today's contract, and the history the census reads predates
    // parts of it, so each event is read as the census's own all-optional shape.
    const page = (await client.getIssueEvents(
      issueKey,
      after,
      EVENT_PAGE_SIZE
    )) as readonly RecordedEvent[];
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
  client: DispatchClient,
  issue: string,
  options: CensusOptions
): Promise<IssueCensus> {
  const fromTime = date(options.from, "from");
  const toTime = date(options.to, "to");
  const issueAsks = await client.listIssueAsks(issue);
  const windowAsks = filterAsksInWindow(issueAsks, options.from, options.to);
  const asks = excludeSessionAsks(windowAsks, options.excludedSessionIds);
  // Since #1671 one approval row follows its document from the first request until a human
  // answers it, so a request opened before the window can be handed back inside it. Approval
  // rounds come from the in-window events of every issue that carries an approval ask, whenever
  // it was opened.
  const events = issueAsks.some((ask) => ask.kind === "approval")
    ? (await fetchIssueEvents(client, issue)).filter(
        (event) =>
          inWindow(event.created_at, fromTime, toTime) &&
          !fromExcludedSession(event.actor, options.excludedSessionIds)
      )
    : [];
  return {
    windowAsks,
    asks,
    issueRow: asks.length === 0 ? undefined : { issue, ...summarizeAsks(asks) },
    approvalRounds: summarizeApprovalRounds(events, issueAsks).map((round) => ({
      issue,
      ...round,
    })),
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
  const client = new DispatchClient(config.url, config.token);
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
    projectIssues.flat().map((issue) => censusIssue(client, issue.key, options))
  );
  const issueRows = census.flatMap((issue) =>
    issue.issueRow === undefined ? [] : [issue.issueRow]
  );
  const approvalRounds = census.flatMap((issue) => issue.approvalRounds);
  const standaloneQuestions = census.flatMap((issue) => issue.standaloneQuestions);

  const totals = summarizeAsks(census.flatMap((issue) => issue.asks));
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
