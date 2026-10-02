import type {
  AcceptedMessageDelivery,
  Actor,
  Advised,
  Agent,
  ArchitectureSource,
  Artifact,
  ArtifactApproval,
  ArtifactBlock,
  ArtifactReferences,
  ArtifactText,
  ArtifactUploadResponse,
  ArtifactVersionText,
  Ask,
  AskRead,
  Comment,
  CommentRead,
  CreateArtifactInput,
  CreateAskInput,
  CreateCommentInput,
  CreateIssueInput,
  CreateMessageInput,
  CreateVersionInput,
  DispatchServiceErrorShape,
  EditArtifactInput,
  EditArtifactResponse,
  EditAskInput,
  Event,
  GraphEdgeKind,
  GraphReferences,
  Issue,
  IssueDetails,
  IssuePriority,
  IssueRead,
  IssueReferences,
  IssueRouteStatus,
  IssueSummaryPage,
  Message,
  MessageRead,
  MessageReplyInput,
  MessageReplyResult,
  OpenAsksResponse,
  ResolveAskInput,
  SearchResponse,
  UpdateIssueInput,
  Version,
  WhoamiResponse,
} from "@legion/contracts";

import { textHead } from "./ask-answer";

export type { DispatchServiceErrorShape } from "@legion/contracts";

export class DispatchServiceError extends Error {
  override readonly name = "DispatchServiceError";
  /**
   * Whether Dispatch itself wrote this refusal, so its status and code say what Dispatch decided;
   * false for an answer something in Dispatch's place wrote (`DispatchGatewayError`).
   */
  readonly fromDispatch: boolean = true;

  constructor(
    readonly code: string,
    readonly status: number,
    message: string,
    readonly candidates?: DispatchServiceErrorShape["candidates"],
    readonly current?: DispatchServiceErrorShape["current"],
    readonly mismatches?: DispatchServiceErrorShape["mismatches"]
  ) {
    super(message);
  }
}

/**
 * A non-2xx answer that is not Dispatch's JSON, so whatever answers in its place (a proxy, a load
 * balancer, an auth gateway) wrote it. `answer` says what came back and `advice` what asking again
 * can do, judged from the method and the status alone; the message joins them. A caller that knows
 * more about what its own request did gives `answer` advice of its own instead. `code` is
 * `HTTP_<status>`, as for any refusal that names no code.
 */
export class DispatchGatewayError extends DispatchServiceError {
  override readonly fromDispatch = false;
  /** Whether the status can clear on its own: a 5xx, a timeout (408) or a rate limit (429). */
  readonly transient: boolean;
  /**
   * Whether the request may have reached Dispatch before the gateway answered, so a write may
   * have taken effect (`reachesDispatch`).
   */
  readonly mayHaveReachedDispatch: boolean;

  constructor(
    status: number,
    /**
     * `<METHOD> <url> answered <status>[ <reason>] with <body>, which looks like a proxy or
     * gateway page rather than Dispatch's own answer`.
     */
    readonly answer: string,
    /** What asking again can do, as a clause that follows "so", without a final stop. */
    readonly advice: string,
    message = `${answer}, so ${advice}.`
  ) {
    super(`HTTP_${status}`, status, message);
    this.transient = transientStatus(status);
    this.mayHaveReachedDispatch = reachesDispatch(status);
  }
}

function asErrorShape(value: unknown): DispatchServiceErrorShape {
  return typeof value === "object" && value !== null ? (value as DispatchServiceErrorShape) : {};
}

function isJson(response: Response): boolean {
  return response.headers.get("content-type")?.includes("application/json") ?? false;
}

export interface ListIssuesOptions {
  readonly project?: string;
  readonly status?: string;
  readonly parent?: string;
  readonly label?: string;
  /** Each value repeats as `priority=`; `"none"` matches an issue with no priority. */
  readonly priority?: readonly (IssuePriority | "none")[];
  readonly updated_since?: string;
  /** Only open issues whose route is in this state. */
  readonly route_status?: IssueRouteStatus;
}

/** The page `listIssuePage` asks for: `limit` issues from position `offset` of the listing. */
export interface IssuePageRequest {
  readonly limit: number;
  readonly offset: number;
}

export interface SearchOptions {
  readonly project?: string;
  readonly limit?: number;
}

/** One node's edges: `to` reads what points at it, `from` what it points to; exactly one. */
export type GraphReferencesQuery = ({ readonly to: string } | { readonly from: string }) & {
  readonly kind?: readonly GraphEdgeKind[];
  /** events.id; keeps mentions introduced after it and excludes structural edges. */
  readonly since?: number;
};

export const DISPATCH_TOOL_DEADLINE_MS = 60_000;

function requestSignal(signal: AbortSignal | undefined): AbortSignal {
  const deadline = AbortSignal.timeout(DISPATCH_TOOL_DEADLINE_MS);
  return signal === undefined ? deadline : AbortSignal.any([signal, deadline]);
}

/**
 * Why `answer` is not the page `listIssuePage` asked for, and whether asking again can change
 * that; undefined when it is one. A JSON answer that is not a page is Dispatch's own and comes back
 * the same; text (`#response` returns a body it could not read as JSON as its text) looks like a
 * proxy or gateway in the way, which a retry can get past.
 */
function whyNotAPage(answer: unknown): string | undefined {
  if (typeof answer === "string") {
    return `text that is not a JSON page, ${GATEWAY_PAGE}, so a retry may succeed.`;
  }
  let what: string;
  if (Array.isArray(answer)) {
    what =
      `a bare array of ${answer.length} entries, the unpaged listing, which means that Dispatch ` +
      "is older than sjawhar/legion#1612 or that change has regressed";
  } else if (answer === null || typeof answer !== "object") {
    what = answer === null ? "null" : `a ${typeof answer}`;
  } else {
    const fields = answer as Partial<Record<keyof IssueSummaryPage, unknown>>;
    const lacking = [
      ...(Array.isArray(fields.issues) ? [] : ["an issues array"]),
      ...(["total", "limit", "offset"] as const)
        .filter((name) => typeof fields[name] !== "number")
        .map((name) => `a numeric ${name}`),
    ];
    if (lacking.length === 0) return undefined;
    what = `an object without ${lacking.join(" or ")}`;
  }
  return (
    `${what}. Retrying will not help: the same request gets the same answer until that ` +
    "Dispatch is upgraded or fixed."
  );
}

/**
 * What a body that is not Dispatch's JSON is. Dispatch writes every answer as JSON, its errors and
 * its `/api` 404 included, so anything else in its place came from a proxy or gateway in the way.
 */
const GATEWAY_PAGE = "which looks like a proxy or gateway page rather than Dispatch's own answer";

/**
 * What asking again can do once a gateway answered `method` with `status`. A status that cannot
 * clear answers the same request the same way. One that can (`transientStatus`) may clear for a
 * read. A write answered so may already have taken effect only when the request reached Dispatch
 * (`reachesDispatch`), where a blind retry could apply it twice; otherwise it did not, and a retry
 * may succeed as for a read.
 */
function gatewayAdvice(method: string, status: number): string {
  if (!transientStatus(status)) {
    return "a retry gets the same answer until the Dispatch URL, or whatever answers in its place, is fixed";
  }
  if (method === "GET") return "a retry may succeed";
  return reachesDispatch(status)
    ? "the write may or may not have reached Dispatch: check whether it took effect before retrying it"
    : "the write did not reach Dispatch, and a retry may succeed";
}

/**
 * Whether a status that did not come from Dispatch can clear on its own: a gateway's 5xx, a
 * timeout or a rate limit can; any other status answers the same request the same way.
 */
function transientStatus(status: number): boolean {
  return status >= 500 || status === 408 || status === 429;
}

/**
 * Whether a gateway that answered with `status` may have forwarded the request to Dispatch first:
 * only after a 5xx, which a gateway sends when the upstream failed, timed out or dropped the
 * connection, possibly once Dispatch had the request. A 408 or 429 is the gateway's own timeout on
 * the client's request or its own rate limit, and any other status its own refusal, each sent
 * before it forwards anything.
 */
function reachesDispatch(status: number): boolean {
  return status >= 500;
}

const REDACTED = "[redacted]";

/**
 * The most of a body an excerpt scans. A gateway page's inline styles and scripts can run to tens
 * of kilobytes before its first words, so this reaches well past them, and it bounds the work a
 * body of any size can cost.
 */
const EXCERPT_SCAN_LIMIT = 64 * 1024;

/**
 * The shortest run of the client's bearer's characters an excerpt redacts wherever it stands. A
 * gateway that echoes the header can cut it short, split it with markup, or write it twice with the
 * copies overlapping (a bearer can end in its own first characters), so redacting only whole copies
 * leaves most of one behind; a bearer shorter than this is redacted only whole.
 */
const BEARER_PIECE = 8;

/**
 * The first `EXCERPT_SCAN_LIMIT` characters of `text`, each run of `bearer`'s characters at least
 * `BEARER_PIECE` long replaced with one `REDACTED`. Every piece of that length starting inside the
 * limit is found (one that runs past it reads up to `BEARER_PIECE - 1` characters beyond), so a
 * run is redacted whole however the limit or another copy cuts it. Redaction changes the length,
 * so the text is cut to the limit only afterwards, and nothing from past the limit moves into it.
 * Each distinct piece is one native scan of that slice, and no two distinct pieces of one length
 * match at the same position, so the marking is bounded by the slice and the scans by the limit and
 * the bearer's length, whatever the size of the body. Searching again for a repeated piece would
 * mark the same matches again: a bearer of one character repeated would mark every position of the
 * slice once per piece.
 */
function redactBearer(text: string, bearer: string): string {
  if (bearer === "") return text.slice(0, EXCERPT_SCAN_LIMIT);
  const length = Math.min(BEARER_PIECE, bearer.length);
  const scanned = text.slice(0, EXCERPT_SCAN_LIMIT + length - 1);
  const covered = new Uint8Array(scanned.length);
  const pieces = new Set<string>();
  for (let at = 0; at + length <= bearer.length; at++) pieces.add(bearer.slice(at, at + length));
  for (const piece of pieces) {
    let found = scanned.indexOf(piece);
    while (found !== -1) {
      covered.fill(1, found, found + length);
      found = scanned.indexOf(piece, found + 1);
    }
  }
  let redacted = "";
  let kept = 0;
  for (let start = covered.indexOf(1); start !== -1; start = covered.indexOf(1, kept)) {
    redacted += `${scanned.slice(kept, start)}${REDACTED}`;
    const end = covered.indexOf(0, start);
    kept = end === -1 ? scanned.length : end;
  }
  return `${redacted}${scanned.slice(kept, EXCERPT_SCAN_LIMIT)}`.slice(0, EXCERPT_SCAN_LIMIT);
}

/**
 * Text a gateway chose (a body, or the reason phrase) as one line of plain text safe to quote. A
 * misconfigured gateway echoes the request back, so two kinds of credential are redacted: first
 * every piece of the client's own bearer, trimmed as fetch sends it (`redactBearer`); then, after
 * scripts, styles and tags are dropped, the value after `Authorization:` (its scheme kept) or
 * `Bearer`. All of it runs before the cut to one line, so a cut never leaves a credential's first
 * characters behind. The text is the answerer's, and the excerpt is built on the host's event loop,
 * so only the first `EXCERPT_SCAN_LIMIT` characters are read and every pattern runs in linear time:
 * a tag holds no `<`, and a script or style block left open runs to the end.
 */
function excerpt(text: string, token: string): string {
  const plain = redactBearer(text, token.trim())
    .replace(/<(script|style)\b[^<>]*>(?:[\s\S]*?<\/\1\s*>|[\s\S]*$)/gi, " ")
    .replace(/<[^<>]*>/g, " ");
  return textHead(
    plain
      .replace(
        /(\bauthorization["']?\s*[:=]\s*["']?(?:(?:bearer|basic|digest|token)\s+)?)[^\s"'<>,;]+/gi,
        `$1${REDACTED}`
      )
      .replace(/(\bbearer\s+)[^\s"'<>,;]+/gi, `$1${REDACTED}`)
  );
}

/** JSON HTTP client for Dispatch's native-tool API. */
export class DispatchClient {
  readonly #baseUrl: string;
  readonly #resolvedIssues = new Map<string, Promise<string>>();
  readonly #signal: AbortSignal;

  constructor(
    baseUrl: string,
    readonly token: string,
    readonly fetchImpl: typeof fetch = fetch,
    signal?: AbortSignal
  ) {
    this.#baseUrl = baseUrl.replace(/\/+$/, "");
    this.#signal = requestSignal(signal);
  }

  async issue(input: CreateIssueInput): Promise<Advised<Issue>> {
    return this.#json("POST", ["api", "v1", "issues"], input);
  }

  /** Resolves an existing native key or external reference without creating an issue. */
  async resolveIssue(issueReference: string): Promise<string> {
    return this.#resolveIssue(issueReference);
  }

  /**
   * One page of `GET /api/v1/issues?limit=&offset=`, used as Dispatch served it: `IssueSummaryPage`
   * (sjawhar/legion#1612). Any other answer is refused, naming the request sent, what arrived and
   * whether a retry can help; a bare array answered to this paged request means a Dispatch older
   * than that change or a regression of it.
   */
  async listIssuePage(
    options: ListIssuesOptions,
    page: IssuePageRequest
  ): Promise<IssueSummaryPage> {
    const path = ["api", "v1", "issues"];
    const query = { ...options, limit: page.limit, offset: page.offset };
    const answer: unknown = await this.#json("GET", path, undefined, query);
    const refusal = whyNotAPage(answer);
    if (refusal === undefined) return answer as IssueSummaryPage;
    throw new Error(
      `GET ${this.#url(path, query)} asked for a page ({issues, total, limit, offset}) and got ` +
        refusal
    );
  }

  async listProjectArtifacts(project: string, unlinked = false): Promise<Artifact[]> {
    return this.#json(
      "GET",
      ["api", "v1", "projects", project, "artifacts"],
      undefined,
      unlinked ? { unlinked: "true" } : undefined
    );
  }

  async projectArtifact(
    project: string,
    input: CreateArtifactInput
  ): Promise<ArtifactUploadResponse> {
    const artifactPath = ["api", "v1", "projects", project, "artifacts"];
    if ("content" in input) return this.#json("POST", artifactPath, input);

    const form = new FormData();
    form.set("name", input.name);
    if (input.summary !== undefined) form.set("summary", input.summary);
    if (input.actor !== undefined) form.set("actor", JSON.stringify(input.actor));
    form.set("file", input.file, input.name);
    return this.#form("POST", artifactPath, form);
  }

  async getProjectArtifact(project: string, slug: string): Promise<Artifact> {
    return this.#json("GET", ["api", "v1", "projects", project, "artifacts", slug]);
  }

  async search(query: string, options: SearchOptions = {}): Promise<SearchResponse> {
    return this.#json("GET", ["api", "v1", "search"], undefined, { q: query, ...options });
  }

  async getIssue(issue: string): Promise<IssueDetails> {
    return this.#json("GET", ["api", "v1", "issues", await this.#resolveIssue(issue)]);
  }

  /** `PATCH /api/v1/issues/{key}`: the server replaces every field the body names. */
  async updateIssue(issue: string, input: UpdateIssueInput): Promise<Advised<Issue>> {
    return this.#json("PATCH", ["api", "v1", "issues", await this.#resolveIssue(issue)], input);
  }

  /** `POST /api/v1/issues/{key}/claim`: this session takes the issue. 409 ISSUE_CLAIMED when a
   *  running session, or any human, holds it; 409 CLAIM_CONTENDED when the holder changed twice
   *  while the request ran, so nothing was applied. */
  async claimIssue(issue: string, input: { readonly actor?: Actor } = {}): Promise<Issue> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "claim"],
      input
    );
  }

  /** `DELETE /api/v1/issues/{key}/claim`: give up the claim, or clear one whose session is
   *  gone. The status does not move. It answers the same two 409s, and has no force. */
  async releaseIssueClaim(issue: string, input: { readonly actor?: Actor } = {}): Promise<Issue> {
    return this.#json(
      "DELETE",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "claim"],
      input
    );
  }

  async getIssueEvents(issue: string, after = 0, limit = 200): Promise<Event[]> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "events"],
      undefined,
      { after, limit }
    );
  }
  async read(issueReference: string): Promise<IssueRead> {
    const issueKey = await this.#resolveIssue(issueReference);
    const issue = await this.getIssue(issueKey);
    const events = await this.getIssueEvents(issueKey, Math.max(0, issue.last_seq - 10), 10);
    return { issue, events };
  }

  async ask(issue: string, input: CreateAskInput): Promise<Advised<Ask>> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "asks"],
      input
    );
  }

  async openAsks(sessionID: string, since?: string): Promise<OpenAsksResponse> {
    return this.#json("GET", ["api", "v1", "asks", "open"], undefined, {
      author_session: sessionID,
      ...(since === undefined ? {} : { since }),
    });
  }

  /** Every open ask on a project's issues and documents (issue- or document-owned), oldest first. */
  async openAsksForProject(project: string): Promise<OpenAsksResponse> {
    return this.#json("GET", ["api", "v1", "asks", "open"], undefined, { project });
  }

  /** `GET /api/v1/agents`: the Envoy listener's live sessions, for naming a session by the
   *  title it is running under rather than the one it stamped on an old write. */
  async listAgents(): Promise<Agent[]> {
    return this.#json("GET", ["api", "v1", "agents"]);
  }

  async whoami(): Promise<WhoamiResponse> {
    return this.#json("GET", ["api", "v1", "whoami"]);
  }

  /** `POST /api/v1/projects/{key}/architecture-source/sync`: import the
   *  project's architecture model now; the row's `last_error` says whether the
   *  import replaced the model or the previous one stays up. */
  async syncArchitectureSource(project: string): Promise<ArchitectureSource> {
    return this.#json("POST", ["api", "v1", "projects", project, "architecture-source", "sync"]);
  }

  /** `GET /api/v1/projects/{key}/architecture-source`: the configured source row, or `null`
   *  when the project has none. Having none is an answer, not a failure: a current server says
   *  `200 null`, an older one `404 SOURCE_NOT_FOUND`, and a client meets both while a rollout
   *  mixes versions. Any other failure, a different 404 included, throws. */
  async getArchitectureSource(project: string): Promise<ArchitectureSource | null> {
    try {
      return await this.#json("GET", ["api", "v1", "projects", project, "architecture-source"]);
    } catch (error) {
      if (
        error instanceof DispatchServiceError &&
        error.status === 404 &&
        error.code === "SOURCE_NOT_FOUND"
      ) {
        return null;
      }
      throw error;
    }
  }

  async resolveAsk(id: string, input: ResolveAskInput): Promise<Ask> {
    return this.#json("POST", ["api", "v1", "asks", id, "resolve"], input);
  }

  /**
   * Requests approval of a document at its latest settled version, its question the document, the
   * version and `summary`. A document holds one open request: this opens it, rewords it with a new
   * summary, or hands it back to the human when a new version or a thread reply left it waiting on
   * the agent. `recorded` is whether this call did any of that (Dispatch's 201); false (its 200)
   * means the open request already stood as asked, waiting on the human, or the version is already
   * approved, when `ask` is null and `approval` carries the standing approval.
   */
  async requestApproval(
    artifactID: string,
    input: { actor: Actor; summary: string }
  ): Promise<{
    ask: Ask | null;
    artifact_id: string;
    version: number;
    approval: ArtifactApproval;
    recorded: boolean;
  }> {
    const answer = await this.#jsonAnswer<{
      ask: Ask | null;
      artifact_id: string;
      version: number;
      approval: ArtifactApproval;
    }>("POST", ["api", "v1", "artifacts", artifactID, "approval-requests"], input);
    return { ...answer.payload, recorded: answer.status === 201 };
  }

  async editAsk(id: string, input: EditAskInput): Promise<Ask> {
    return this.#json("PATCH", ["api", "v1", "asks", id], input);
  }

  async comment(issue: string, input: CreateCommentInput): Promise<Advised<Comment>> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"],
      input
    );
  }

  /** Every ask on an issue (all states unless narrowed). */
  async listIssueAsks(issue: string, state?: "all" | "open" | "answered"): Promise<Ask[]> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "asks"],
      undefined,
      state === undefined ? undefined : { state }
    );
  }

  async getArtifactAsks(id: string, state?: "all" | "open" | "answered"): Promise<Ask[]> {
    return this.#json(
      "GET",
      ["api", "v1", "artifacts", id, "asks"],
      undefined,
      state === undefined ? undefined : { state }
    );
  }

  async artifactAsk(id: string, input: CreateAskInput): Promise<Advised<Ask>> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "asks"], input);
  }

  async suggest(
    issue: string,
    input: Omit<CreateCommentInput, "body" | "suggestion"> & {
      readonly body?: string;
      readonly replace_with: string;
    }
  ): Promise<Advised<Comment>> {
    const { replace_with, ...comment } = input;
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"],
      {
        ...comment,
        suggestion: { replace_with },
      }
    );
  }

  async getArtifactComments(id: string): Promise<Comment[]> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "comments"]);
  }

  async artifactComment(id: string, input: CreateCommentInput): Promise<Advised<Comment>> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "comments"], input);
  }

  async artifactSuggest(
    id: string,
    input: Omit<CreateCommentInput, "body" | "suggestion"> & {
      readonly body?: string;
      readonly replace_with: string;
    }
  ): Promise<Advised<Comment>> {
    const { replace_with, ...comment } = input;
    return this.#json("POST", ["api", "v1", "artifacts", id, "comments"], {
      ...comment,
      suggestion: { replace_with },
    });
  }

  async message(issue: string, input: CreateMessageInput): Promise<Advised<Message>> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "messages"],
      input
    );
  }

  /**
   * `POST /api/v1/messages/{id}/reply`: the targeted session's reply to the delivery it
   * received. It takes no issue, so it is the route that answers a human's direct message -
   * a conversation Dispatch keeps without one - and the server settles the delivery attempt
   * with the reply it inserts. `followUp` asks to post more once the attempt is answered
   * (`?follow_up=true`); without it an answered attempt hands back its stored reply and posts
   * nothing. The result's `duplicate` says the route posted nothing.
   */
  async messageReply(
    id: string,
    input: MessageReplyInput,
    options: { readonly followUp?: boolean } = {}
  ): Promise<MessageReplyResult> {
    return this.#json(
      "POST",
      ["api", "v1", "messages", id, "reply"],
      input,
      options.followUp === true ? { follow_up: "true" } : undefined
    );
  }

  async getMessage(issue: string, id: string): Promise<MessageRead> {
    return this.#json("GET", [
      "api",
      "v1",
      "issues",
      await this.#resolveIssue(issue),
      "messages",
      id,
    ]);
  }

  /** `GET /api/v1/messages/{id}`: the conversation a message belongs to, by any message id in
   *  it - the root and every reply. It takes no issue, so it reads a human's direct message to
   *  a session and the replies to it, which belong to none. It reads as `session`; which
   *  threads a session may read is the route's rule (`GET /api/v1` describes it). */
  async getMessageThread(id: string, session: string): Promise<MessageRead> {
    return this.#json("GET", ["api", "v1", "messages", id], undefined, { session });
  }

  /** `POST /api/v1/messages/{id}/deliveries/{attempt}/accept`: this session records that it took
   *  that attempt of the message as its user's own turn, and gets back the attempt with the
   *  message's stored body, which is what it injects. Dispatch allows one acceptance per message,
   *  of a person's own direct Send or Aside to this session (no issue, no broadcast), of its
   *  latest attempt, which a person asked for within the last minute and which did not fail; any
   *  refusal throws a `DispatchServiceError` naming the check. */
  async acceptMessageDelivery(
    id: string,
    attempt: number,
    input: { readonly actor: Actor }
  ): Promise<AcceptedMessageDelivery> {
    return this.#json(
      "POST",
      ["api", "v1", "messages", id, "deliveries", String(attempt), "accept"],
      input
    );
  }

  async artifact(
    issue: string,
    input: CreateArtifactInput
  ): Promise<
    ArtifactUploadResponse & { readonly artifact: Artifact & { readonly issue_key: string } }
  > {
    const artifactPath = ["api", "v1", "issues", await this.#resolveIssue(issue), "artifacts"];
    if ("content" in input) return this.#json("POST", artifactPath, input);

    const form = new FormData();
    form.set("name", input.name);
    if (input.summary !== undefined) form.set("summary", input.summary);
    if (input.actor !== undefined) form.set("actor", JSON.stringify(input.actor));
    form.set("file", input.file, input.name);
    return this.#form("POST", artifactPath, form);
  }

  async getArtifact(id: string): Promise<Artifact> {
    return this.#json("GET", ["api", "v1", "artifacts", id]);
  }

  async docRead(id: string): Promise<ArtifactText>;
  async docRead(id: string, version: number): Promise<ArtifactVersionText>;
  async docRead(id: string, version?: number): Promise<ArtifactText | ArtifactVersionText>;
  async docRead(id: string, version?: number): Promise<ArtifactText | ArtifactVersionText> {
    return version === undefined
      ? this.#json("GET", ["api", "v1", "artifacts", id, "text"])
      : this.#json("GET", ["api", "v1", "artifacts", id, "versions", String(version)]);
  }

  async artifactBlocks(id: string): Promise<ArtifactBlock[]> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "blocks"]);
  }

  async docEdit(id: string, input: EditArtifactInput): Promise<EditArtifactResponse> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "edits"], input);
  }

  async nameArtifactVersion(id: string, input: CreateVersionInput): Promise<Version> {
    return this.#json("POST", ["api", "v1", "artifacts", id, "versions"], input);
  }

  async getArtifactEvents(id: string, after = 0, limit = 200): Promise<Event[]> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "events"], undefined, { after, limit });
  }

  async getArtifactReferences(id: string): Promise<ArtifactReferences> {
    return this.#json("GET", ["api", "v1", "artifacts", id, "references"]);
  }

  async getAsk(id: string): Promise<AskRead> {
    return this.#json("GET", ["api", "v1", "asks", id]);
  }

  /** A bearer follows only its own session: the body names it and the path repeats it. */
  async followAsk(id: string, sessionId: string, actor: Actor): Promise<void> {
    await this.#json("PUT", ["api", "v1", "asks", id, "followers", sessionId], { actor });
  }

  async unfollowAsk(id: string, sessionId: string, actor: Actor): Promise<void> {
    await this.#json("DELETE", ["api", "v1", "asks", id, "followers", sessionId], { actor });
  }

  async getComment(id: string): Promise<CommentRead> {
    return this.#json("GET", ["api", "v1", "comments", id]);
  }

  /** Resolves a comment thread as `actor`; the server takes no reason and any actor may resolve. */
  async resolveComment(id: string, actor: Actor): Promise<Comment> {
    return this.#json("POST", ["api", "v1", "comments", id, "resolve"], { actor });
  }

  async getComments(issue: string, artifact?: string): Promise<Comment[]> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "comments"],
      undefined,
      artifact ? { artifact } : undefined
    );
  }

  async getIssueReferences(issue: string): Promise<IssueReferences> {
    return this.#json("GET", [
      "api",
      "v1",
      "issues",
      await this.#resolveIssue(issue),
      "references",
    ]);
  }

  async getReferences(query: GraphReferencesQuery): Promise<GraphReferences> {
    return this.#json("GET", ["api", "v1", "references"], undefined, {
      ...("to" in query ? { to: query.to } : { from: query.from }),
      ...(query.kind === undefined || query.kind.length === 0
        ? {}
        : { kind: query.kind.join(",") }),
      ...(query.since === undefined ? {} : { since: query.since }),
    });
  }

  async #resolveIssue(issueReference: string): Promise<string> {
    if (!issueReference.includes("#")) return issueReference;
    let resolved = this.#resolvedIssues.get(issueReference);
    if (!resolved) {
      resolved = this.#json<{ key: string }>("GET", ["api", "v1", "issues", "resolve"], undefined, {
        ref: issueReference,
      }).then((resolution) => resolution.key);
      this.#resolvedIssues.set(issueReference, resolved);
    }
    try {
      return await resolved;
    } catch (error) {
      this.#resolvedIssues.delete(issueReference);
      throw error;
    }
  }
  async #json<T>(
    method: string,
    path: readonly string[],
    body?: unknown,
    query?: object
  ): Promise<T> {
    return (await this.#jsonAnswer<T>(method, path, body, query)).payload;
  }

  /** `#json` with the status Dispatch answered, for a route whose status says what it did. */
  async #jsonAnswer<T>(
    method: string,
    path: readonly string[],
    body?: unknown,
    query?: object
  ): Promise<{ readonly status: number; readonly payload: T }> {
    const headers: Record<string, string> = {
      Accept: "application/json",
      Authorization: `Bearer ${this.token}`,
    };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const url = this.#url(path, query);
    const response = await this.fetchImpl(url, {
      method,
      headers,
      signal: this.#signal,
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    return { status: response.status, payload: await this.#response<T>(method, url, response) };
  }

  async #form<T>(method: string, path: readonly string[], body: FormData): Promise<T> {
    const url = this.#url(path);
    const response = await this.fetchImpl(url, {
      method,
      headers: { Accept: "application/json", Authorization: `Bearer ${this.token}` },
      body,
      signal: this.#signal,
    });
    return this.#response<T>(method, url, response);
  }

  #url(path: readonly string[], query?: object): string {
    const url = new URL(
      `${path.map((segment) => encodeURIComponent(segment)).join("/")}`,
      `${this.#baseUrl}/`
    );
    if (query) {
      for (const [name, value] of Object.entries(query)) {
        if (typeof value === "string" || typeof value === "number") {
          url.searchParams.set(name, String(value));
        } else if (Array.isArray(value)) {
          for (const item of value) url.searchParams.append(name, String(item));
        }
      }
    }
    return url.toString();
  }

  /**
   * The answer's payload, or the refusal it carries. Dispatch's own refusal is JSON with a string
   * `error` (`writeError` in the server), and it renders as that text with its `code`. Any other
   * non-2xx body — a gateway's HTML page, an empty body, JSON of another shape — did not come
   * from Dispatch, so it is a `DispatchGatewayError` naming the request (method, URL and query;
   * the token travels in a header, never the URL), the status and reason phrase, a one-line
   * plain-text excerpt of the body (or, when none of what the excerpt reads is text, the body's
   * size), and what asking again can do for that method and status.
   */
  async #response<T>(method: string, url: string, response: Response): Promise<T> {
    const text = await response.text();
    let payload: unknown = text;
    if (text && isJson(response)) {
      try {
        payload = JSON.parse(text);
      } catch {
        payload = text;
      }
    }
    if (!response.ok) {
      const error = asErrorShape(payload);
      if (typeof error.error === "string") {
        throw new DispatchServiceError(
          error.code ?? `HTTP_${response.status}`,
          response.status,
          error.error,
          error.candidates,
          error.current,
          error.mismatches
        );
      }
      const quoted = excerpt(text, this.token);
      let body: string;
      if (quoted !== "") {
        body = `a body that is not Dispatch's error JSON (${JSON.stringify(quoted)})`;
      } else if (text === "") {
        body = "an empty body";
      } else {
        const within =
          text.length > EXCERPT_SCAN_LIMIT ? ` in its first ${EXCERPT_SCAN_LIMIT / 1024} KiB` : "";
        const size = Buffer.byteLength(text).toLocaleString("en-US");
        body = `a body of ${size} bytes and no readable text${within}`;
      }
      const reasonPhrase = excerpt(response.statusText, this.token);
      const reason = reasonPhrase === "" ? "" : ` ${reasonPhrase}`;
      throw new DispatchGatewayError(
        response.status,
        `${method} ${url} answered ${response.status}${reason} with ${body}, ${GATEWAY_PAGE}`,
        gatewayAdvice(method, response.status)
      );
    }
    return payload as T;
  }
}
