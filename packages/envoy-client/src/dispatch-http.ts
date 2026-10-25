import type {
  AcceptedMessageDelivery,
  Actor,
  Advised,
  Agent,
  ApprovalRequestResponse,
  ArchitectureSource,
  Artifact,
  ArtifactBlock,
  ArtifactReferences,
  ArtifactText,
  ArtifactUploadResponse,
  ArtifactVersionText,
  Ask,
  AskRead,
  Comment,
  CommentRead,
  CommentWriteResponse,
  CreateArtifactInput,
  CreateAskInput,
  CreateCommentInput,
  CreateIssueInput,
  CreateMessageInput,
  CreateProjectInput,
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
  Project,
  ResolveAskInput,
  SearchResponse,
  UpdateIssueInput,
  Version,
  WhoamiResponse,
} from "@legion/contracts";
import {
  IDEMPOTENCY_ANSWER_NOT_KEPT,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_HEADER,
  IDEMPOTENCY_KEY_IN_FLIGHT,
} from "@legion/contracts";

import { textHead } from "./ask-answer";

export type { DispatchServiceErrorShape } from "@legion/contracts";

/** The stored bytes of one version of an uploaded file, and the MIME type Dispatch serves them as. */
export interface ArtifactFileVersion {
  readonly mime: string;
  readonly bytes: Uint8Array;
}

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

/**
 * A write none of whose attempts got a definitive answer, where the last attempt's refusal alone
 * would say the write never took effect (a gateway's 408 or 429, a rate limit of Dispatch's own) or
 * names an earlier attempt Dispatch was still running (`409 KEY_IN_FLIGHT`), while an attempt may
 * have reached Dispatch: one that failed in transport or got a 5xx, or that running one. So the
 * write may or may not have taken effect, and this is deliberately no `DispatchServiceError`, which
 * a caller reads as a refusal Dispatch decided. `last` (also the `cause`) is that last refusal.
 */
export class DispatchWriteUnsettledError extends Error {
  override readonly name = "DispatchWriteUnsettledError";

  constructor(
    readonly last: DispatchServiceError,
    readonly attempts: number
  ) {
    const answer =
      last instanceof DispatchGatewayError ? last.answer : `${last.code}: ${last.message}`;
    const why =
      last.code === IDEMPOTENCY_KEY_IN_FLIGHT
        ? "Dispatch was still running an earlier attempt of the write"
        : "an earlier attempt of the write may have reached Dispatch";
    super(
      `${answer} (the last of ${attempts} attempts), and ${why}, so it may or may not have ` +
        "taken effect: check whether it did before retrying it.",
      { cause: last }
    );
  }
}

/**
 * A write Dispatch made whose answer is gone, so the caller cannot be shown what it made: a repeat
 * under its key answered `409 IDEMPOTENCY_KEY_COMMITTED` (the write landed before its answer was
 * lost, and Dispatch will not run it again) or a 2xx carrying `IDEMPOTENCY_ANSWER_NOT_KEPT` (its
 * first answer was larger than Dispatch keeps). The write took effect, so this is no refusal and
 * deliberately no `DispatchServiceError`; it is never asked again. Its message is the one line a
 * session reads: what was made, that its answer was lost, and to read it back.
 */
export class DispatchWriteLandedError extends Error {
  override readonly name = "DispatchWriteLandedError";

  constructor(
    /** `IDEMPOTENCY_KEY_COMMITTED` or `IDEMPOTENCY_ANSWER_NOT_KEPT`. */
    readonly code: string,
    /** The status Dispatch answered with: the 409, or the first answer's own 2xx. */
    readonly status: number,
    method: string,
    url: string
  ) {
    const lost =
      code === IDEMPOTENCY_ANSWER_NOT_KEPT
        ? "Dispatch kept no copy of its answer, which was too large"
        : "its answer was lost on the way back";
    super(
      `${method} ${url} was made, but ${lost} (${code}), so this command cannot print what it ` +
        "made: read it back with `dispatch read`, or with the matching list for something it created."
    );
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
  readonly offset?: number;
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
 * How a write asks Dispatch again under its key while its attempts get no definitive answer:
 * `pausesMs` is the wait before each retry in turn, so the write makes at most one attempt more than
 * it has entries, and no retry starts once `withinMs` has passed since the first attempt began. An
 * answer's `Retry-After` longer than the scheduled pause is waited instead. `pause` and `now` are
 * the timer and the monotonic clock, which a test replaces.
 */
export interface DispatchWriteRetry {
  readonly pausesMs: readonly number[];
  readonly withinMs: number;
  /** Waits `ms`, or rejects with the signal's reason once it aborts. */
  readonly pause?: (ms: number, signal: AbortSignal) => Promise<void>;
  readonly now?: () => number;
}

/**
 * At most five attempts, 1, 2, 4 and 8 s apart, none starting more than 30 s after the first: a
 * write whose every attempt fails at once settles in about 15 s, a `Retry-After` can stretch that
 * to the 30 s bound, and all of it sits inside the call's 60 s deadline, so a Dispatch that is down
 * holds the `dispatch` command, and the session waiting on it, for no longer.
 */
export const DISPATCH_WRITE_RETRY: DispatchWriteRetry = {
  pausesMs: [1_000, 2_000, 4_000, 8_000],
  withinMs: 30_000,
};

function pauseFor(ms: number, signal: AbortSignal): Promise<void> {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  if (signal.aborted) {
    reject(signal.reason);
    return promise;
  }
  const stop = (): void => {
    clearTimeout(timer);
    reject(signal.reason);
  };
  const timer = setTimeout(() => {
    signal.removeEventListener("abort", stop);
    resolve();
  }, ms);
  signal.addEventListener("abort", stop, { once: true });
  return promise;
}

/** The wait an answer's `Retry-After` asks for, in delta-seconds or as an HTTP date; 0 for none. */
function retryAfterMs(response: Response): number {
  const value = response.headers.get("Retry-After")?.trim() ?? "";
  if (/^\d+$/.test(value)) return Number(value) * 1_000;
  const at = Date.parse(value);
  return Number.isNaN(at) ? 0 : Math.max(0, at - Date.now());
}

/**
 * The Dispatch origins this process has seen run the keyed-write layer: one answered a write with
 * the key it sent (`keyEchoed`), or its `/healthz` said `idempotency_keys`. Only such a Dispatch is
 * asked again under a key; against any other a repeat would be a second write.
 */
const KEY_ECHOING_ORIGINS = new Set<string>();

/** Each origin's `/healthz` probe, asked at most once per process (`DispatchClient.#runsKeyedLayer`). */
const KEY_LAYER_PROBES = new Map<string, Promise<boolean>>();

/** How long the `/healthz` probe waits; it runs only after a write has already failed. */
const KEY_LAYER_PROBE_MS = 5_000;

/**
 * Forgets what this process learned about which Dispatch runs the keyed layer. A test seam: the
 * memory is per process and per origin, so a test that drives one origin as a Dispatch of its own
 * starts from nothing.
 */
export function forgetKeyedLayerMemory(): void {
  KEY_ECHOING_ORIGINS.clear();
  KEY_LAYER_PROBES.clear();
}

/**
 * Whether `response` carries back the key its request sent (`Idempotency-Key`), which says the
 * answer came through Dispatch's keyed layer. Dispatch echoes it bare; a key sent in double quotes
 * comes back without them, so both forms are read.
 */
function keyEchoed(response: Response, key: string): boolean {
  const echoed = response.headers.get(IDEMPOTENCY_KEY_HEADER)?.trim() ?? "";
  return echoed.replace(/^"(.*)"$/, "$1") === key;
}

/** A request body encoded once, so every attempt of a write sends the bytes the first one did. */
interface EncodedBody {
  readonly content: string | Uint8Array<ArrayBuffer>;
  readonly type: string;
}

/** One attempt's answer to a write, as the retry loop reads it. */
interface WriteAnswer {
  /** The answer to hand on, its body still unread. */
  readonly response: Response;
  /** Whether it is no definitive answer, so the write asks again under its key. */
  readonly again: boolean;
  /** Whether this attempt, or one Dispatch was still running, may have reached Dispatch. */
  readonly reached: boolean;
}

/**
 * Whether one attempt's answer to a write is definitive. It is not after a 5xx (a gateway's, or
 * Dispatch's own, which Dispatch never stores under the key), a 408 or 429 (a gateway's timeout,
 * or a rate limit), or Dispatch's `409 KEY_IN_FLIGHT` (an earlier attempt still runs under the
 * key); any other answer, a 2xx or a refusal, is. Only a 409's body is read here, for its code, so
 * that answer is handed on rebuilt from what was read.
 */
async function writeAnswer(response: Response): Promise<WriteAnswer> {
  const { status } = response;
  if (status >= 500) return { response, again: true, reached: true };
  if (status === 408 || status === 429) return { response, again: true, reached: false };
  if (status !== 409) return { response, again: false, reached: false };
  const text = await response.text();
  let code: unknown;
  if (isJson(response)) {
    try {
      code = asErrorShape(JSON.parse(text)).code;
    } catch {
      code = undefined;
    }
  }
  const running = code === IDEMPOTENCY_KEY_IN_FLIGHT;
  return {
    response: new Response(text, {
      status,
      statusText: response.statusText,
      headers: response.headers,
    }),
    again: running,
    reached: running,
  };
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

/**
 * JSON HTTP client for Dispatch's native-tool API. Every request goes through `#send`, where each
 * write (POST or PATCH) carries an `Idempotency-Key` of its own and asks again under it as
 * `writeRetry` paces it.
 */
export class DispatchClient {
  readonly #baseUrl: string;
  readonly #resolvedIssues = new Map<string, Promise<string>>();
  readonly #signal: AbortSignal;

  constructor(
    baseUrl: string,
    readonly token: string,
    readonly fetchImpl: typeof fetch = fetch,
    signal?: AbortSignal,
    readonly writeRetry: DispatchWriteRetry = DISPATCH_WRITE_RETRY
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
    return this.#upload(["api", "v1", "projects", project, "artifacts"], input);
  }

  async getProjectArtifact(project: string, slug: string): Promise<Artifact> {
    return this.#json("GET", ["api", "v1", "projects", project, "artifacts", slug]);
  }

  /** `POST /api/v1/agents/{session_id}/artifacts`: an upload owned by an agent's conversation on
   *  the Agents page, addressed `dispatch://agent/<session id>/artifact/<slug>`. That owner takes
   *  files and pictures only; Dispatch refuses a Markdown document there. */
  async agentArtifact(
    sessionId: string,
    input: CreateArtifactInput
  ): Promise<ArtifactUploadResponse> {
    return this.#upload(["api", "v1", "agents", sessionId, "artifacts"], input);
  }

  async getAgentArtifact(sessionId: string, slug: string): Promise<Artifact> {
    return this.#json("GET", ["api", "v1", "agents", sessionId, "artifacts", slug]);
  }

  async getIssueArtifact(issue: string, slug: string): Promise<Artifact> {
    return this.#json("GET", [
      "api",
      "v1",
      "issues",
      await this.#resolveIssue(issue),
      "artifacts",
      slug,
    ]);
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

  /** `POST /api/v1/projects`: create a project. A key that already exists is 409
   *  PROJECT_EXISTS, and a key that is not a project key or an empty name 400 INVALID_PROJECT. */
  async createProject(input: CreateProjectInput): Promise<Project> {
    return this.#json("POST", ["api", "v1", "projects"], input);
  }

  /** `GET /api/v1/projects`: every project, ordered by key. */
  async listProjects(): Promise<Project[]> {
    return this.#json("GET", ["api", "v1", "projects"]);
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
  ): Promise<ApprovalRequestResponse & { recorded: boolean }> {
    const answer = await this.#jsonAnswer<ApprovalRequestResponse>(
      "POST",
      ["api", "v1", "artifacts", artifactID, "approval-requests"],
      input
    );
    return { ...answer.payload, recorded: answer.status === 201 };
  }

  async editAsk(id: string, input: EditAskInput): Promise<Ask> {
    return this.#json("PATCH", ["api", "v1", "asks", id], input);
  }

  async comment(issue: string, input: CreateCommentInput): Promise<Advised<CommentWriteResponse>> {
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

  async artifactComment(
    id: string,
    input: CreateCommentInput
  ): Promise<Advised<CommentWriteResponse>> {
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

  /**
   * `POST /api/v1/{messages|comments}/{id}/reply` as the session a targeted delivery reached: its
   * reply to that delivery attempt, or why it could not honour it (`postDeliveryReply`, whose
   * body names the session as `actor` and the `attempt`).
   */
  async deliveryReply(
    resource: "message" | "comment",
    id: string,
    input: Readonly<Record<string, unknown>>
  ): Promise<void> {
    const collection = resource === "message" ? "messages" : "comments";
    await this.#json("POST", ["api", "v1", collection, id, "reply"], input);
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
    return this.#upload(
      ["api", "v1", "issues", await this.#resolveIssue(issue), "artifacts"],
      input
    );
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

  /** One version of an uploaded file as Dispatch stores it: `GET /artifacts/{id}/versions/{n}`
   * answers a file with its bytes under its own MIME type, never a JSON envelope. */
  async fileVersion(id: string, version: number): Promise<ArtifactFileVersion> {
    const url = this.#url(["api", "v1", "artifacts", id, "versions", String(version)]);
    const response = await this.#send("GET", url, undefined, "*/*");
    if (!response.ok) return this.#response<never>("GET", url, response);
    return {
      mime: response.headers.get("Content-Type") ?? "application/octet-stream",
      bytes: new Uint8Array(await response.arrayBuffer()),
    };
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
    const url = this.#url(path, query);
    const response = await this.#send(
      method,
      url,
      body === undefined ? undefined : { content: JSON.stringify(body), type: "application/json" }
    );
    return { status: response.status, payload: await this.#response<T>(method, url, response) };
  }

  /** An upload to an owner's artifact route: inline content as JSON, a file as multipart. */
  async #upload<T>(path: readonly string[], input: CreateArtifactInput): Promise<T> {
    if ("content" in input) return this.#json("POST", path, input);
    const form = new FormData();
    form.set("name", input.name);
    if (input.summary !== undefined) form.set("summary", input.summary);
    if (input.actor !== undefined) form.set("actor", JSON.stringify(input.actor));
    form.set("file", input.file, input.name);
    return this.#form("POST", path, form);
  }

  /**
   * A multipart write. The form is encoded here, once: every encoding of a `FormData` draws a new
   * boundary, and a retry whose bytes differ from its first attempt's is a different request to
   * Dispatch, which refuses a key reused for one (`422 KEY_REUSED`).
   */
  async #form<T>(method: string, path: readonly string[], form: FormData): Promise<T> {
    const url = this.#url(path);
    const encoded = new Response(form);
    const type = encoded.headers.get("Content-Type") ?? "multipart/form-data";
    const content = new Uint8Array(await encoded.arrayBuffer());
    const response = await this.#send(method, url, { content, type });
    return this.#response<T>(method, url, response);
  }

  /**
   * The one request function: every call the client makes goes through here. A read, a PUT or a
   * DELETE goes once. A write (POST or PATCH) carries an `Idempotency-Key` minted here for this
   * call alone, so all its attempts are one intent to Dispatch, which runs it once and answers a
   * repeat with the answer it stored, while two calls are two writes. Each attempt sends the same
   * encoded body. While an attempt gets no definitive answer (`writeAnswer`, or a transport
   * failure) the write asks again under its key, paced by `writeRetry`, and the first definitive
   * answer is the call's: a 2xx, a replay of one included, or a refusal. The call's deadline and
   * its caller's cancellation end the attempts at once, whether a fetch or a pause is under way.
   *
   * It asks again only against a Dispatch this process has seen run the keyed layer, which an
   * answer says by echoing the request's key (`keyEchoed`): a Dispatch without the layer would run
   * a repeat as a second write, so its 5xx — which can come after it committed — is reported as it
   * stands rather than resent. The failing answer's own echo is that knowledge, so one of
   * Dispatch's own 5xx, 429 or `409 KEY_IN_FLIGHT` is asked again at once; a transport failure, and
   * an answer something in Dispatch's place wrote (which carries no echo), ask `/healthz` first
   * (`#runsKeyedLayer`), once per process per origin.
   */
  async #send(
    method: string,
    url: string,
    body?: EncodedBody,
    accept = "application/json"
  ): Promise<Response> {
    const headers: Record<string, string> = {
      Accept: accept,
      Authorization: `Bearer ${this.token}`,
    };
    if (body !== undefined) headers["Content-Type"] = body.type;
    const init: RequestInit = {
      method,
      headers,
      signal: this.#signal,
      ...(body === undefined ? {} : { body: body.content }),
    };
    if (method !== "POST" && method !== "PATCH") return this.fetchImpl(url, init);
    const key = crypto.randomUUID();
    headers[IDEMPOTENCY_KEY_HEADER] = key;
    const { pausesMs, withinMs, pause = pauseFor, now = () => performance.now() } = this.writeRetry;
    const started = now();
    let reached = false;
    for (let attempt = 1; ; attempt++) {
      let answer: WriteAnswer | undefined;
      let thrown: unknown;
      try {
        const response = await this.fetchImpl(url, init);
        if (keyEchoed(response, key)) KEY_ECHOING_ORIGINS.add(this.#baseUrl);
        answer = await writeAnswer(response);
        if (!answer.again) return answer.response;
        reached ||= answer.reached;
      } catch (error) {
        // A deadline or a cancellation is the caller's limit, never a reason to ask again.
        const name = error instanceof Error || error instanceof DOMException ? error.name : "";
        if (this.#signal.aborted || name === "AbortError" || name === "TimeoutError") throw error;
        thrown = error;
        reached = true;
      }
      const scheduled = pausesMs[attempt - 1];
      const wait = Math.max(
        scheduled ?? 0,
        answer === undefined ? 0 : retryAfterMs(answer.response)
      );
      // Only a Dispatch that runs the keyed layer may be asked again: against any other a repeat
      // would be a second write. The attempt's own answer says so when it echoed the key; a
      // transport failure, and an answer something in Dispatch's place wrote, say nothing, so the
      // probe asks before a write that already failed is resent.
      const inTime = scheduled !== undefined && now() - started + wait <= withinMs;
      if (!inTime || !(await this.#runsKeyedLayer())) {
        if (answer === undefined) throw thrown;
        return this.#unsettled(method, url, answer.response, reached, attempt);
      }
      await answer?.response.body?.cancel().catch(() => undefined);
      await pause(wait, this.#signal);
    }
  }

  /**
   * Whether this Dispatch runs the keyed layer, so a failed write may be asked again under its
   * key. True once an answer echoed the key; otherwise `GET /healthz`, whose `idempotency_keys`
   * says it, asked at most once per process per origin and bounded at `KEY_LAYER_PROBE_MS`. A
   * probe that fails, times out, answers no JSON or leaves the field out is false: the write is
   * reported as it stands rather than resent, since a Dispatch without the layer would run a
   * repeat as a second write. It runs only after a write has already failed, so the normal path
   * costs nothing.
   */
  async #runsKeyedLayer(): Promise<boolean> {
    if (KEY_ECHOING_ORIGINS.has(this.#baseUrl)) return true;
    let probe = KEY_LAYER_PROBES.get(this.#baseUrl);
    if (probe === undefined) {
      probe = this.#probeKeyedLayer();
      KEY_LAYER_PROBES.set(this.#baseUrl, probe);
    }
    const runs = await probe;
    if (runs) KEY_ECHOING_ORIGINS.add(this.#baseUrl);
    return runs;
  }

  async #probeKeyedLayer(): Promise<boolean> {
    try {
      const response = await this.fetchImpl(`${this.#baseUrl}/healthz`, {
        method: "GET",
        headers: { Accept: "application/json", Authorization: `Bearer ${this.token}` },
        signal: AbortSignal.any([this.#signal, AbortSignal.timeout(KEY_LAYER_PROBE_MS)]),
      });
      if (!response.ok || !isJson(response)) return false;
      const health: unknown = await response.json();
      return (
        typeof health === "object" &&
        health !== null &&
        Reflect.get(health, "idempotency_keys") === true
      );
    } catch {
      return false;
    }
  }

  /**
   * What a write throws once it stops asking with no definitive answer: the last attempt's refusal
   * as `#response` reads it, unless that refusal alone would say the write never took effect while
   * an attempt may have reached Dispatch (`reached`), when it is a `DispatchWriteUnsettledError`.
   * `#response` throws for every status `writeAnswer` asks again after.
   */
  async #unsettled(
    method: string,
    url: string,
    response: Response,
    reached: boolean,
    attempts: number
  ): Promise<never> {
    const refusal: unknown = await this.#response(method, url, response).catch(
      (error: unknown) => error
    );
    if (reached && refusal instanceof DispatchServiceError) {
      const landedAlone =
        refusal instanceof DispatchGatewayError
          ? refusal.mayHaveReachedDispatch
          : refusal.status >= 500;
      if (!landedAlone) throw new DispatchWriteUnsettledError(refusal, attempts);
    }
    throw refusal;
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
   * size), and what asking again can do for that method and status. A write Dispatch made whose
   * answer is gone — `409 IDEMPOTENCY_KEY_COMMITTED`, or a 2xx carrying
   * `IDEMPOTENCY_ANSWER_NOT_KEPT` — is a `DispatchWriteLandedError`, neither a refusal nor a payload.
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
        if (response.status === 409 && error.code === IDEMPOTENCY_KEY_COMMITTED) {
          throw new DispatchWriteLandedError(error.code, response.status, method, url);
        }
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
    const replay = asErrorShape(payload);
    if (replay.code === IDEMPOTENCY_ANSWER_NOT_KEPT && typeof replay.error === "string") {
      throw new DispatchWriteLandedError(replay.code, response.status, method, url);
    }
    return payload as T;
  }
}
