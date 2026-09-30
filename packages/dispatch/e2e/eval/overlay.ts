/**
 * What an agent under evaluation wrote through the evaluation proxy, and how a read shows it:
 * the `Overlay` store, and the views `reads.ts` and `writes.ts` share (an issue read with the
 * agent's writes in it, an ask's derived fields, a log with the agent's events on it). Every
 * view follows the Go server's read of the same thing (`packages/envoy/internal/dispatch/api`).
 */
import { randomUUID } from "node:crypto";
import {
  type Actor,
  type Artifact,
  type ArtifactApproval,
  type Ask,
  type AskEdit,
  type AskFollower,
  type AskLastReply,
  type AskRead,
  type Comment,
  type CommentEventPayload,
  type CommentRead,
  type Event,
  ISSUE_STATUSES,
  type Issue,
  type IssueChild,
  type IssueDetails,
  type IssueSummary,
  type Message,
  type MessageRead,
  type OpenAsk,
  type OpenAsksResponse,
  type Version,
} from "@legion/contracts";

export type Params = Readonly<Record<string, string>>;

/** A status and a JSON body (none for 204). */
export interface Answer {
  readonly status: number;
  readonly body?: unknown;
}

/** An answer a handler reaches early: a refusal it shares with every other handler. */
export class Refusal extends Error {
  constructor(readonly answer: Answer) {
    super(`refused with ${answer.status}`);
  }
}

/** The server behind the proxy, for the reads the handlers make themselves. */
export interface Upstream {
  /** The server's answer to a GET of `path`, or undefined when it answers anything but 2xx. */
  json<T>(path: string, query?: string): Promise<T | undefined>;
}

export const ok = (body: unknown): Answer => ({ status: 200, body });
export const created = (body: unknown): Answer => ({ status: 201, body });
export const refused = (status: number, code: string, error: string): Answer => ({
  status,
  body: { code, error },
});
export const notFound = (): Answer => refused(404, "NOT_FOUND", "not found");
/** Answers a write the proxy recorded but cannot answer as the server would. */
export const unmodelledWrite = (what: string): Answer =>
  refused(501, "EVAL_PROXY_UNMODELLED_WRITE", `the evaluation proxy ${what}`);
/** Answers a read of something the agent created that the proxy cannot derive. */
export const unmodelledRead = (what: string): Answer =>
  refused(501, "EVAL_PROXY_UNMODELLED_READ", `the evaluation proxy does not model ${what}`);
export const closedIssue = () => new Refusal(refused(409, "ISSUE_CLOSED", "issue is closed"));

/** Ids the proxy gives the events it appends: past any real event id. */
const CREATED_EVENT_ID_BASE = 9_000_000_000;
export const api = (path: string) => `/api/v1${path}`;
export const enc = encodeURIComponent;
export const now = () => new Date().toISOString();
export const projectOf = (key: string) => key.replace(/-\d+$/, "");
/** Oldest first, as every server list of asks, comments and messages is ordered. */
export const byCreated = (left: { created_at: string; id: string }, right: typeof left) =>
  left.created_at === right.created_at
    ? left.id.localeCompare(right.id)
    : left.created_at.localeCompare(right.created_at);
interface StoredDocument {
  artifact: Artifact;
  markdown: string;
}

/** The document an approval ask is about (the server's `AskApproval`, which every ask read
 *  carries and the contract's `Ask` does not spell). */
interface AskApproval {
  readonly artifact_id: string;
  readonly name: string;
  readonly version: number;
}

export type StoredAsk = Ask & { readonly approval?: AskApproval };

/** Who owns an event log: an issue, or a project document (one with no issue). */
export type EventOwner = { readonly issue: string } | { readonly document: Artifact };

const logKey = (owner: EventOwner) =>
  "issue" in owner ? owner.issue : `artifact:${owner.document.id}`;

/** One event's type and payload, as the contract pairs them. */
type EventBody = {
  [Type in Event["type"]]: {
    readonly type: Type;
    readonly payload: Extract<Event, { type: Type }>["payload"];
  };
}[Event["type"]];

/** The comment event payload the server writes, with the delivery fields the contract's
 *  `CommentEventPayload` leaves out. */
export interface CommentPayload extends CommentEventPayload {
  readonly thread_root_id?: string;
  readonly suppress_route: boolean;
  readonly suppressed_authors: string[] | null;
}

/** What the agent wrote, served back to it. */
export class Overlay {
  /** Issues the agent created or changed, each as it stands now. */
  readonly issues = new Map<string, Issue>();
  /** Keys and ids of what the agent created, as opposed to changed. */
  readonly created = new Set<string>();
  /** Asks the agent created or changed, as rows (no read-only fields). */
  readonly asks = new Map<string, StoredAsk>();
  /** An upstream ask's read as it was when the agent first changed it. */
  readonly upstreamAsks = new Map<string, StoredAsk>();
  readonly askEdits = new Map<string, AskEdit[]>();
  /** Followers the agent's writes added, by ask. */
  readonly followers = new Map<string, AskFollower[]>();
  /** Comments the agent created or changed. */
  readonly comments = new Map<string, Comment>();
  /** Messages the agent created. */
  readonly messages = new Map<string, Message>();
  /** Upstream delivery attempts the agent answered: message id, then attempt, to reply id. */
  readonly answered = new Map<string, Map<number, string>>();
  readonly documents = new Map<string, StoredDocument>();
  /** A document's approval once the agent requested it, and what it was before. */
  readonly approvals = new Map<string, { now: ArtifactApproval; before: ArtifactApproval }>();
  readonly events = new Map<string, Event[]>();
  /** The upstream `last_seq` of each log the agent appended to, read at its first append. */
  readonly baseSeq = new Map<string, number>();
  issueCount = 0;
  eventCount = 0;

  lastSeq(owner: string): number {
    return this.events.get(owner)?.at(-1)?.seq ?? this.baseSeq.get(owner) ?? 0;
  }

  /** The agent's replies to an ask, oldest first. */
  repliesTo(askId: string): Comment[] {
    return [...this.comments.values()]
      .filter((comment) => comment.ask_id === askId && this.created.has(comment.id))
      .sort(byCreated);
  }

  documentsOf(filter: (artifact: Artifact) => boolean): Artifact[] {
    return [...this.documents.values()].map((stored) => stored.artifact).filter(filter);
  }
}

interface Base {
  readonly overlay: Overlay;
  readonly upstream: Upstream;
}

export interface ReadContext extends Base {
  readonly params: Params;
  readonly query: URLSearchParams;
}

export interface WriteContext extends ReadContext {
  /** The request body: a JSON object, or a multipart form's fields and files. */
  readonly input: Readonly<Record<string, unknown>>;
  /** The session writing, as the server records it. */
  readonly actor: Actor;
}

export interface ReadRoute {
  /** The path under `/api/v1`, `:name` capturing one segment. */
  readonly path: string;
  /** Answers from the overlay alone: a read of something the agent created. */
  readonly own?: (context: ReadContext) => Promise<Answer | undefined>;
  /** Folds what the agent wrote into the server's answer. */
  readonly merge?: (context: ReadContext, body: unknown) => Promise<unknown>;
  /** The issue an answer belongs to when its path does not name it. */
  readonly owner?: (body: unknown) => string | null | undefined;
  /** Recomputes what the answer derives from rows the exclusion filter dropped. */
  readonly recount?: (body: unknown) => unknown;
}

export interface WriteRoute {
  readonly method: "POST" | "PATCH" | "PUT" | "DELETE";
  /** The paths under `/api/v1`, `:name` capturing one segment. */
  readonly paths: readonly string[];
  readonly handle: (context: WriteContext) => Promise<Answer>;
}

// ---------------------------------------------------------------------------------------------
// Issues

/** The fields an issue read adds to the issue. */
function issueOf(details: IssueDetails): Issue {
  const {
    artifacts: _artifacts,
    open_asks: _openAsks,
    children: _children,
    referenced_by_count: _referencedBy,
    route_status: _routeStatus,
    route_holder: _routeHolder,
    ...issue
  } = details;
  return issue;
}

/** Everything the agent appended to an issue's log shows on the issue: its `last_seq`, and an
 *  `updated_at` no older than the newest event (the server's event append does both). */
export function withLog(overlay: Overlay, issue: Issue): Issue {
  const newest = overlay.events.get(issue.key)?.at(-1)?.created_at;
  return {
    ...issue,
    last_seq: Math.max(issue.last_seq, overlay.lastSeq(issue.key)),
    updated_at: newest !== undefined && newest > issue.updated_at ? newest : issue.updated_at,
  };
}

/** The issue as it stands now: created, changed, or the server's; undefined when none exists. */
export async function currentIssue(context: Base, key: string): Promise<Issue | undefined> {
  const stored = context.overlay.issues.get(key);
  if (stored) return withLog(context.overlay, stored);
  const upstream = await context.upstream.json<IssueDetails>(api(`/issues/${enc(key)}`));
  return upstream && withLog(context.overlay, issueOf(upstream));
}

export async function requireOpenIssue(context: Base, key: string): Promise<Issue> {
  const issue = await currentIssue(context, key);
  if (!issue) throw new Refusal(notFound());
  if (issue.closed_at !== null) throw closedIssue();
  return issue;
}

/** Every key the agent's writes touched: created, changed, or given an event. */
export function touchedIssues(overlay: Overlay): Set<string> {
  return new Set([
    ...overlay.issues.keys(),
    ...[...overlay.events.keys()].filter((key) => !key.startsWith("artifact:")),
  ]);
}

/** The issue read (`GET /issues/{key}`) with the agent's writes in it. */
export async function issueRead(context: Base, key: string): Promise<IssueDetails | undefined> {
  const { overlay } = context;
  let base: IssueDetails;
  if (overlay.created.has(key)) {
    const issue = overlay.issues.get(key);
    if (!issue) return undefined;
    base = {
      ...issue,
      route_status: null,
      route_holder: null,
      artifacts: [],
      open_asks: [],
      children: [],
      referenced_by_count: 0,
    };
  } else {
    const upstream = await context.upstream.json<IssueDetails>(api(`/issues/${enc(key)}`));
    if (!upstream) return undefined;
    base = upstream;
  }
  return mergeIssueRead(context, key, base);
}

export function mergeIssueRead(context: Base, key: string, base: IssueDetails): IssueDetails {
  const { overlay } = context;
  const issue = withLog(overlay, { ...issueOf(base), ...overlay.issues.get(key) });
  const createdChildren = [...overlay.issues.values()].filter(
    (child) => child.parent === key && overlay.created.has(child.key)
  );
  // Every document the proxy stores is one the agent created.
  const createdDocuments = overlay.documentsOf((artifact) => artifact.issue_key === key);
  return {
    ...base,
    ...issue,
    artifacts: [
      ...base.artifacts.map((artifact) => withApproval(overlay, artifact)),
      ...createdDocuments.map((artifact) => withApproval(overlay, artifact)),
    ],
    open_asks: askList(overlay, base.open_asks, (ask) => ask.issue_key === key, "open", true),
    children: childrenOf(overlay, key, base.children),
    // Structural edges point at the issue: each child's `child_of` and each of its documents'
    // `attached_to` (`graph_edges`).
    referenced_by_count:
      base.referenced_by_count + createdChildren.length + createdDocuments.length,
  };
}

/** Every issue under `key` the agent created, at any depth. */
function createdDescendants(overlay: Overlay, key: string): Issue[] {
  const found: Issue[] = [];
  const frontier = [key];
  for (let parent = frontier.pop(); parent !== undefined; parent = frontier.pop()) {
    for (const issue of overlay.issues.values()) {
      if (issue.parent === parent && overlay.created.has(issue.key)) {
        found.push(withLog(overlay, issue));
        frontier.push(issue.key);
      }
    }
  }
  return found;
}

/** An issue's direct children, each with its subtree's done and total counts. */
function childrenOf(overlay: Overlay, key: string, upstream: IssueChild[]): IssueChild[] {
  const rollup = (child: IssueChild, self: Issue | undefined): IssueChild => {
    const below = createdDescendants(overlay, child.key);
    const wasDone = child.status === "done";
    const isDone = (self?.status ?? child.status) === "done";
    const active = [child.active_at, self?.updated_at, ...below.map((issue) => issue.updated_at)]
      .filter((at): at is string => at !== undefined)
      .sort()
      .at(-1);
    return {
      ...child,
      title: self?.title ?? child.title,
      status: self?.status ?? child.status,
      subtree_total: child.subtree_total + below.length,
      subtree_done:
        child.subtree_done +
        Number(isDone) -
        Number(wasDone) +
        below.filter((issue) => issue.status === "done").length,
      active_at: active ?? child.active_at,
    };
  };
  const rows = upstream.map((child) => {
    const stored = overlay.issues.get(child.key);
    return rollup(child, stored && withLog(overlay, stored));
  });
  for (const issue of overlay.issues.values()) {
    if (issue.parent !== key || !overlay.created.has(issue.key)) continue;
    const self = withLog(overlay, issue);
    rows.push(
      rollup(
        {
          key: issue.key,
          title: issue.title,
          status: issue.status,
          subtree_done: Number(issue.status === "done"),
          subtree_total: 1,
          active_at: self.updated_at,
          external_links: [],
        },
        undefined
      )
    );
  }
  return rows.sort((left, right) => (left.key < right.key ? -1 : left.key > right.key ? 1 : 0));
}

export function summaryOf(details: IssueDetails): IssueSummary {
  return {
    key: details.key,
    title: details.title,
    status: details.status,
    priority: details.priority,
    rank: details.rank,
    labels: details.labels,
    parent: details.parent,
    assignee: details.assignee,
    claim: details.claim,
    components: details.components,
    route: details.route,
    route_status: details.route_status,
    route_holder: details.route_holder,
    updated_at: details.updated_at,
    last_seq: details.last_seq,
    open_asks: details.closed_at === null ? details.open_asks.length : 0,
  };
}

/** Whether an issue passes the issue list's filters (`listIssues`). */
export function listed(details: IssueDetails, query: URLSearchParams): boolean {
  const equals = (name: string, value: string | null) => {
    const wanted = query.get(name)?.trim() ?? "";
    return wanted === "" || wanted === value;
  };
  if (!equals("project", details.project) || !equals("status", details.status)) return false;
  if (!equals("parent", details.parent)) return false;
  const labels = new Set(details.labels.map((label) => label.toLowerCase()));
  if (!query.getAll("label").every((label) => labels.has(label.trim().toLowerCase()))) return false;
  const since = query.get("updated_since");
  if (since !== null && Date.parse(details.updated_at) < Date.parse(since.trim())) return false;
  const routeStatus = query.get("route_status")?.trim() ?? "";
  if ((query.get("open") === "true" || routeStatus !== "") && details.closed_at !== null) {
    return false;
  }
  if (routeStatus !== "" && details.route_status !== routeStatus) return false;
  const priorities = query.getAll("priority").map((value) => value.trim());
  if (priorities.length === 0) return true;
  return details.priority === null
    ? priorities.includes("none")
    : priorities.includes(String(details.priority));
}

export function byLifecycle(left: IssueSummary, right: IssueSummary): number {
  const order = (status: string) => ISSUE_STATUSES.findIndex((known) => known === status);
  return (
    order(left.status) - order(right.status) ||
    (left.rank < right.rank ? -1 : left.rank > right.rank ? 1 : 0)
  );
}

// ---------------------------------------------------------------------------------------------
// Asks and comments

/** An ask as a read shows it: the reply count the server batches, and for an open ask whose
 *  turn it is (the turn of its newest reply, `human` when nobody replied). */
export function askRead(overlay: Overlay, ask: Ask, upstream: Ask | undefined): Ask {
  const {
    waiting_on: _waitingOn,
    referenced_by_count: _count,
    last_reply: _lastReply,
    ...row
  } = ask;
  const before = upstream ?? overlay.upstreamAsks.get(ask.id);
  const newest = overlay.repliesTo(ask.id).at(-1);
  const read: Ask = { ...row, referenced_by_count: before?.referenced_by_count ?? 0 };
  if (ask.state !== "open") return read;
  return {
    ...read,
    waiting_on: newest ? (newest.turn ?? "human") : (before?.waiting_on ?? "human"),
  };
}

function lastReply(overlay: Overlay, ask: Ask, upstream: Ask | undefined): AskLastReply | null {
  const newest = overlay.repliesTo(ask.id).at(-1);
  if (newest) return { author: newest.author, created_at: newest.created_at };
  return upstream?.last_reply ?? null;
}

type AskState = "all" | "open" | "answered";

/** An owner's asks in a state, the server's rows with the agent's asks in them, oldest first. */
export function askList(
  overlay: Overlay,
  upstream: Ask[],
  belongs: (ask: Ask) => boolean,
  state: AskState,
  withLastReply = false
): Ask[] {
  const rows = new Map(upstream.map((ask) => [ask.id, ask]));
  const asks = [
    ...upstream.map((ask) => overlay.asks.get(ask.id) ?? ask),
    ...[...overlay.asks.values()].filter((ask) => !rows.has(ask.id) && belongs(ask)),
  ];
  return asks
    .filter((ask) => state === "all" || ask.state === state)
    .map((ask) => {
      const read = askRead(overlay, ask, rows.get(ask.id));
      return withLastReply
        ? { ...read, last_reply: lastReply(overlay, ask, rows.get(ask.id)) }
        : read;
    })
    .sort(byCreated);
}

export function askState(query: URLSearchParams): AskState {
  const state = query.get("state");
  return state === "open" || state === "answered" ? state : "all";
}

export function commentList(
  overlay: Overlay,
  upstream: Comment[],
  belongs: (comment: Comment) => boolean
): Comment[] {
  const ids = new Set(upstream.map((comment) => comment.id));
  return [
    ...upstream.map((comment) => overlay.comments.get(comment.id) ?? comment),
    ...[...overlay.comments.values()].filter(
      (comment) => !ids.has(comment.id) && overlay.created.has(comment.id) && belongs(comment)
    ),
  ].sort(byCreated);
}

export function followerList(upstream: AskFollower[], added: AskFollower[] = []): AskFollower[] {
  const sessions = new Set(upstream.map((follower) => follower.session_id));
  return [...upstream, ...added.filter((follower) => !sessions.has(follower.session_id))].sort(
    (left, right) => left.since.localeCompare(right.since)
  );
}

export function follow(overlay: Overlay, askId: string, actor: Actor): void {
  if (actor.kind !== "session") return;
  const followers = overlay.followers.get(askId) ?? [];
  if (followers.some((follower) => follower.session_id === actor.id)) return;
  overlay.followers.set(askId, [...followers, { session_id: actor.id, since: now() }]);
}

/** The server's ask row as the agent last left it, or its row as a read gives it. */
export async function currentAsk(context: Base, id: string): Promise<StoredAsk | undefined> {
  const stored = context.overlay.asks.get(id);
  if (stored) return stored;
  const read = await context.upstream.json<AskRead & { ask: StoredAsk }>(api(`/asks/${enc(id)}`));
  if (!read) return undefined;
  context.overlay.upstreamAsks.set(id, read.ask);
  const { waiting_on: _waitingOn, referenced_by_count: _count, ...row } = read.ask;
  return row;
}

export async function currentComment(context: Base, id: string): Promise<Comment | undefined> {
  const stored = context.overlay.comments.get(id);
  if (stored) return stored;
  return (await context.upstream.json<CommentRead>(api(`/comments/${enc(id)}`)))?.comment;
}

// ---------------------------------------------------------------------------------------------
// Documents

export async function currentArtifact(context: Base, id: string): Promise<Artifact | undefined> {
  const stored = context.overlay.documents.get(id);
  if (stored) return stored.artifact;
  return context.upstream.json<Artifact>(api(`/artifacts/${enc(id)}`));
}

/** A document's approval as reads show it: the agent's request, or the server's. */
export function withApproval(overlay: Overlay, artifact: Artifact): Artifact {
  if (artifact.kind !== "doc") return artifact;
  const latest = artifact.versions.at(-1)?.number ?? 0;
  const approval = overlay.approvals.get(artifact.id)?.now ??
    artifact.approval ?? { state: "draft", latest_version: latest };
  return { ...artifact, approval };
}

export function version(number: number, author: Actor, summary: string): Version {
  return {
    number,
    named: summary !== "",
    summary: summary === "" ? null : summary,
    authors: [author],
    created_at: now(),
  };
}

/** A document's text as the server keeps it: canonical markdown ends in exactly one line feed.
 *  The rest of the server's canonical serialization is not modelled; an upload it would rewrite
 *  (emphasis markers, list bullets, spacing) reads back as uploaded. */
export const canonicalText = (markdown: string) => markdown.replace(/\n*$/, "\n");

export function storeDocument(
  overlay: Overlay,
  owner: { issue: string | null; project: string },
  name: string,
  slug: string,
  markdown: string,
  actor: Actor,
  primary: boolean
): StoredDocument {
  const stored: StoredDocument = {
    markdown: canonicalText(markdown),
    artifact: {
      id: randomUUID(),
      issue_key: owner.issue,
      project: owner.project,
      ref_key: `${owner.issue ?? owner.project}/${slug}`,
      slug,
      name,
      kind: "doc",
      primary,
      created_by: actor,
      created_at: now(),
      versions: [version(1, actor, "")],
    },
  };
  overlay.documents.set(stored.artifact.id, stored);
  overlay.created.add(stored.artifact.id);
  return stored;
}

/** The artifact-scoped routes serve only a project document; an issue's is read through it. */
export function requireProjectDocument(artifact: Artifact): void {
  if (artifact.issue_key !== null) {
    throw new Refusal(
      refused(
        400,
        "ARTIFACT_LINKED",
        `artifact belongs to issue ${artifact.issue_key}; use /api/v1/issues/${artifact.issue_key}/...`
      )
    );
  }
}

// ---------------------------------------------------------------------------------------------
// Events

/** Reads a log's upstream `last_seq` before the agent's first append to it. */
async function openLog(context: Base, owner: EventOwner): Promise<void> {
  const key = logKey(owner);
  const { overlay } = context;
  if (overlay.baseSeq.has(key)) return;
  if ("issue" in owner) {
    const issue = overlay.created.has(owner.issue)
      ? undefined
      : await context.upstream.json<IssueDetails>(api(`/issues/${enc(owner.issue)}`));
    overlay.baseSeq.set(key, issue?.last_seq ?? 0);
    return;
  }
  const newest = overlay.created.has(owner.document.id)
    ? undefined
    : await context.upstream.json<Event[]>(
        api(`/artifacts/${enc(owner.document.id)}/events`),
        "?order=desc&limit=1"
      );
  overlay.baseSeq.set(key, newest?.[0]?.seq ?? 0);
}

/** Appends one event to an owner's log. `build` gets the event's id and sequence number, which
 *  an `ask.opened` payload (its `opened_event_id`) and an `issue.*` payload (its `last_seq`)
 *  carry. */
export async function append(
  context: Base,
  owner: EventOwner,
  actor: Actor,
  build: (event: { readonly id: number; readonly seq: number }) => EventBody
): Promise<Event> {
  await openLog(context, owner);
  const { overlay } = context;
  const key = logKey(owner);
  overlay.eventCount += 1;
  const id = CREATED_EVENT_ID_BASE + overlay.eventCount;
  const seq = overlay.lastSeq(key) + 1;
  const body = build({ id, seq });
  const event: Event = {
    id,
    issue_key: "issue" in owner ? owner.issue : null,
    artifact_id: "issue" in owner ? null : owner.document.id,
    project: "issue" in owner ? projectOf(owner.issue) : owner.document.project,
    seq,
    actor,
    // A session's writes wake nobody, except a child's move, which its parent's watchers hear.
    notify: body.type.startsWith("child."),
    created_at: now(),
    ...body,
  };
  overlay.events.set(key, [...(overlay.events.get(key) ?? []), event]);
  return event;
}

interface EventPage {
  readonly after: number;
  readonly before: number | undefined;
  readonly descending: boolean;
  readonly ids: readonly number[] | undefined;
  readonly limit: number;
}

/** The server's `parseEventListOptions`. */
export function eventPage(query: URLSearchParams): EventPage {
  const invalid = (message: string) => new Refusal(refused(400, "INVALID_QUERY", message));
  const count = (name: string) => {
    const raw = query.get(name)?.trim() ?? "";
    if (raw === "") return 0;
    if (!/^\d+$/.test(raw)) throw invalid(`${name} must be a non-negative integer`);
    return Number(raw);
  };
  if (query.has("before") && query.get("before")?.trim() === "") {
    throw invalid("before must be a non-negative integer");
  }
  const after = count("after");
  const before = count("before");
  const rawIds = query.get("ids")?.trim() ?? "";
  const ids = rawIds === "" ? undefined : rawIds.split(",").map((part) => Number(part.trim()));
  if (ids && ids.length > 50) throw invalid("ids must include at most 50 values");
  if (ids?.some((id) => !Number.isInteger(id) || id < 1))
    throw invalid("ids must be positive integers");
  const order = query.get("order")?.trim() ?? "";
  if (order !== "" && order !== "asc" && order !== "desc")
    throw invalid("order must be asc or desc");
  const rawLimit = query.get("limit")?.trim() ?? "";
  const limit = rawLimit === "" ? 200 : Number(rawLimit);
  if (!Number.isInteger(limit) || limit < 1 || limit > 200) {
    throw invalid("limit must be between 1 and 200");
  }
  if (query.has("after") && (query.has("before") || order === "desc")) {
    throw invalid("after cannot be combined with before or order=desc");
  }
  return {
    after,
    before: query.has("before") ? before : undefined,
    descending: order === "desc" || query.has("before"),
    ids,
    limit,
  };
}

/** One page of a log: the server's page with the agent's events in it. */
export function eventsPage(upstream: Event[], mine: Event[], page: EventPage): Event[] {
  const { after, before, descending, ids, limit } = page;
  const selected = mine.filter((event) =>
    ids
      ? ids.includes(event.id)
      : descending
        ? before === undefined || event.seq < before
        : event.seq > after
  );
  const events = [...upstream, ...selected].sort((left, right) =>
    descending && !ids ? right.seq - left.seq : left.seq - right.seq
  );
  return ids ? events : events.slice(0, limit);
}

// ---------------------------------------------------------------------------------------------
// Messages

/** The message thread under `message`, as `readMessage` gives it: the message with its
 *  deliveries (the agent's answers applied) and every message transitively replying to it. */
export function messageRead(
  overlay: Overlay,
  message: Message,
  upstreamReplies: Message[]
): MessageRead {
  const answered = overlay.answered.get(message.id);
  const deliveries = message.deliveries.map((delivery) => {
    const reply = answered?.get(delivery.attempt);
    return reply === undefined
      ? delivery
      : { ...delivery, reply_id: reply, state: "sent" as const, error: null };
  });
  const thread = new Set([message.id, ...upstreamReplies.map((reply) => reply.id)]);
  const mine: Message[] = [];
  for (let grew = true; grew; ) {
    grew = false;
    for (const reply of overlay.messages.values()) {
      if (!thread.has(reply.id) && reply.in_reply_to !== null && thread.has(reply.in_reply_to)) {
        thread.add(reply.id);
        mine.push(reply);
        grew = true;
      }
    }
  }
  return {
    message: { ...message, deliveries },
    replies: [...upstreamReplies, ...mine].sort(byCreated),
  };
}

/** The root of a created message's thread: a created root, or the upstream message the
 *  agent's replies hang under. */
function createdThreadRoot(overlay: Overlay, id: string): { created?: Message; upstream?: string } {
  let current = overlay.messages.get(id);
  while (current?.in_reply_to) {
    const parent = overlay.messages.get(current.in_reply_to);
    if (!parent) return { upstream: current.in_reply_to };
    current = parent;
  }
  return { created: current };
}

export async function messageThread(
  context: Base,
  id: string,
  query: string
): Promise<MessageRead | undefined> {
  const { overlay } = context;
  if (overlay.created.has(id)) {
    const root = createdThreadRoot(overlay, id);
    if (root.created) return messageRead(overlay, root.created, []);
    if (root.upstream === undefined) return undefined;
    const upstream = await context.upstream.json<MessageRead>(
      api(`/messages/${enc(root.upstream)}`),
      query
    );
    return upstream && messageRead(overlay, upstream.message, upstream.replies);
  }
  const upstream = await context.upstream.json<MessageRead>(api(`/messages/${enc(id)}`), query);
  return upstream && messageRead(overlay, upstream.message, upstream.replies);
}

/** The target of a thread's root, walking the agent's messages and then the server's. */
export async function threadTarget(context: Base, message: Message): Promise<string> {
  if (message.in_reply_to === null) return message.target ?? "";
  const root = createdThreadRoot(context.overlay, message.id);
  if (root.created) return root.created.target ?? "";
  const thread = await context.upstream.json<MessageRead>(
    api(`/messages/${enc(root.upstream ?? message.in_reply_to)}`)
  );
  return thread?.message.target ?? "";
}

// ---------------------------------------------------------------------------------------------
// Open asks

async function openAskRow(
  context: Base,
  ask: Ask,
  upstream: OpenAsk | undefined
): Promise<OpenAsk | undefined> {
  const { overlay } = context;
  if (ask.state !== "open") return undefined;
  let owner: OpenAsk["owner"];
  let priority: OpenAsk["priority"] = null;
  let ref: string;
  if (ask.issue_key !== null) {
    const issue = await currentIssue(context, ask.issue_key);
    if (!issue || issue.closed_at !== null) return undefined;
    // The server's owner also carries the assignee, which the contract's `OpenAskOwner` omits.
    const row = { key: issue.key, title: issue.title, assignee: issue.assignee };
    owner = { issue: row };
    priority = issue.priority;
    ref = `/issues/${issue.key}?ask=${ask.id}`;
  } else {
    const document = await currentArtifact(context, ask.artifact_id ?? "");
    if (!document) return undefined;
    owner = { document: { project: document.project, slug: document.slug, name: document.name } };
    ref = `/projects/${document.project}/documents/${document.slug}?ask=${ask.id}`;
  }
  const read = askRead(overlay, ask, overlay.upstreamAsks.get(ask.id));
  const newest = overlay.repliesTo(ask.id).at(-1);
  return {
    id: ask.id,
    ref,
    question: ask.question,
    kind: ask.kind,
    urgency: ask.urgency,
    created_at: ask.created_at,
    age_seconds: Math.max(0, Math.floor((Date.now() - Date.parse(ask.created_at)) / 1000)),
    priority,
    owner,
    human_replied: upstream?.human_replied ?? false,
    last_reply: newest
      ? { author: newest.author, created_at: newest.created_at }
      : (upstream?.last_reply ?? null),
    waiting_on: read.waiting_on ?? "human",
  };
}

export function recountOpenAsks(response: OpenAsksResponse): OpenAsksResponse {
  return {
    ...response,
    count: response.asks.length,
    waiting_on_human: response.asks.filter((ask) => ask.waiting_on === "human").length,
    waiting_on_agent: response.asks.filter((ask) => ask.waiting_on === "agent").length,
  };
}

export async function mergeOpenAsks(
  context: ReadContext,
  body: OpenAsksResponse
): Promise<OpenAsksResponse> {
  const { overlay, query } = context;
  const session = query.get("author_session")?.trim() ?? "";
  const project = query.get("project")?.trim() ?? "";
  const touched = touchedIssues(overlay);
  const inScope = async (ask: Ask) => {
    if (session !== "") return ask.author.kind === "session" && ask.author.id === session;
    if (ask.issue_key !== null) return projectOf(ask.issue_key) === project;
    return (await currentArtifact(context, ask.artifact_id ?? ""))?.project === project;
  };
  const rows: OpenAsk[] = [];
  const listed = new Set<string>();
  for (const row of body.asks) {
    listed.add(row.id);
    const issueKey = "issue" in row.owner ? row.owner.issue.key : null;
    const changed = overlay.asks.get(row.id);
    const repliedTo = overlay.repliesTo(row.id).length > 0;
    if (!changed && !repliedTo && (issueKey === null || !touched.has(issueKey))) {
      rows.push(row);
      continue;
    }
    const ask = changed ?? (await currentAsk(context, row.id));
    const rebuilt = ask && (await openAskRow(context, ask, row));
    if (rebuilt) rows.push(rebuilt);
  }
  for (const ask of overlay.asks.values()) {
    if (listed.has(ask.id) || !(await inScope(ask))) continue;
    const row = await openAskRow(context, ask, undefined);
    if (row) rows.push(row);
  }
  rows.sort(
    (left, right) =>
      (left.priority ?? 4) - (right.priority ?? 4) ||
      left.created_at.localeCompare(right.created_at) ||
      left.id.localeCompare(right.id)
  );
  const since = query.get("since");
  const openedSince =
    body.opened_since ||
    (since !== null &&
      [...overlay.asks.values()].some(
        (ask) => overlay.created.has(ask.id) && ask.author.id === session && ask.created_at >= since
      ));
  return recountOpenAsks({ ...body, opened_since: openedSince, asks: rows });
}
