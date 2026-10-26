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
  ClaimedIssue,
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
  IssueSummary,
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
  answerNotKept,
  DEFAULT_ISSUE_READ_EVENTS,
  IDEMPOTENCY_ANSWER_NOT_KEPT,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_HEADER,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_REPLAYED_HEADER,
  type IdempotencyLandedCode,
  retriesUnderKey,
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
 * A write that ended with no definitive answer while an attempt of it may have reached Dispatch,
 * so it may or may not have taken effect. Either the last attempt's refusal alone would say the
 * write never took effect (a gateway's 408 or 429, a rate limit of Dispatch's own) or names an
 * earlier attempt Dispatch was still running (`409 KEY_IN_FLIGHT`), while an earlier attempt failed
 * in transport, got a 5xx, was cut off unanswered or was that running one; or the write ended with
 * no answer at all, after an attempt that may have reached Dispatch: its last attempt was cut off
 * for silence, or the call's deadline ended it. This is deliberately no `DispatchServiceError`,
 * which a caller reads as a refusal Dispatch decided. `last` (also the `cause`) is that last
 * refusal, the silence cut or the deadline's timeout.
 */
export class DispatchWriteUnsettledError extends Error {
  override readonly name = "DispatchWriteUnsettledError";

  constructor(
    readonly last: Error,
    readonly attempts: number,
    /**
     * What the write got, as the message's first clause, where `last` is no refusal that says it:
     * `<METHOD> <url>` and how it ended. `last`'s own message otherwise.
     */
    unanswered?: string
  ) {
    let answer: string;
    let why = "an earlier attempt of the write may have reached Dispatch";
    if (last instanceof DispatchGatewayError) {
      answer = last.answer;
    } else if (last instanceof DispatchServiceError) {
      answer = `${last.code}: ${last.message}`;
      if (last.code === IDEMPOTENCY_KEY_IN_FLIGHT) {
        why = "Dispatch was still running an earlier attempt of the write";
      }
    } else {
      answer = unanswered ?? last.message;
      why = "an attempt of the write may have reached Dispatch";
    }
    const tried = attempts === 1 ? "its only attempt" : `the last of ${attempts} attempts`;
    super(
      `${answer} (${tried}), and ${why}, so it may or may not have taken effect: check whether ` +
        "it did before retrying it.",
      { cause: last }
    );
  }
}

/**
 * A write Dispatch made whose answer is gone, so the caller cannot be shown what it made: a repeat
 * under its key answered `409 IDEMPOTENCY_KEY_COMMITTED` (the write landed before its answer was
 * lost, and Dispatch will not run it again) or a replayed 2xx reading
 * `IDEMPOTENCY_ANSWER_NOT_KEPT` (its first answer was larger than Dispatch keeps). The write took
 * effect, so this is no refusal and deliberately no `DispatchServiceError`; it is never asked
 * again. Its message is the one line a session reads: what was made, that its answer was lost,
 * and to read it back.
 */
export class DispatchWriteLandedError extends Error {
  override readonly name = "DispatchWriteLandedError";

  constructor(
    readonly code: IdempotencyLandedCode,
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

/** Whether `error` is a server's answer that it has no route for the request (`404 NOT_FOUND`,
 *  "no route for …"), as a server older than a route answers it; a `404 NOT_FOUND` for a
 *  resource the server does not know says "not found" instead. */
function isNoRoute(error: unknown): boolean {
  return (
    error instanceof DispatchServiceError &&
    error.status === 404 &&
    error.code === "NOT_FOUND" &&
    error.message.startsWith("no route for ")
  );
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

/** Which of an issue's events `read` returns, oldest first: the newest `events`, or every one. */
export interface IssueReadOptions {
  /** `DEFAULT_ISSUE_READ_EVENTS` when absent or undefined. */
  readonly events?: number | "all" | undefined;
}

/** The most events one `GET /api/v1/issues/{key}/events` page holds: Dispatch refuses a larger
 *  `limit` (`parseEventLimit` in packages/envoy/internal/dispatch/api/events.go). */
const ISSUE_EVENTS_PAGE_MAX = 200;

export const DISPATCH_TOOL_DEADLINE_MS = 60_000;

function requestSignal(signal: AbortSignal | undefined): AbortSignal {
  const deadline = AbortSignal.timeout(DISPATCH_TOOL_DEADLINE_MS);
  return signal === undefined ? deadline : AbortSignal.any([signal, deadline]);
}

/**
 * How a write asks Dispatch again under its key while its attempts get no definitive answer:
 * `pausesMs` is the wait before each retry in turn, so the write makes at most one attempt more than
 * it has entries, and no retry starts once `withinMs` has passed since the first attempt began. An
 * answer's `Retry-After` longer than the scheduled pause is waited instead. An attempt that goes
 * `silenceMs` without progress before its answer begins - none of its body taken by the connection
 * in that time - is cut off and asked again like one that failed in transport, so a write Dispatch
 * is slow to finish is asked again under its key (and answered `409 KEY_IN_FLIGHT`, then replayed)
 * rather than waited on for the call's whole deadline, while an upload still going out is never
 * cut. `pause` and `now` are the timer and the monotonic clock, which a test replaces; `now` also
 * dates what the process learns about which Dispatch runs the keyed layer
 * (`KEYED_LAYER_MEMORY_MS`).
 */
export interface DispatchWriteRetry {
  readonly pausesMs: readonly number[];
  readonly withinMs: number;
  readonly silenceMs: number;
  /** Waits `ms`, or rejects with the signal's reason once it aborts. */
  readonly pause?: (ms: number, signal: AbortSignal) => Promise<void>;
  readonly now?: () => number;
}

/**
 * At most five attempts, 1, 2, 4 and 8 s apart, none starting more than 30 s after the first, each
 * cut off after 20 s without progress: a write whose every attempt fails at once settles in about
 * 15 s, a `Retry-After` can stretch that to the 30 s bound, and an attempt started then that hears
 * nothing is cut off by 50 s, all inside the call's 60 s deadline, so a Dispatch that is down holds
 * the `dispatch` command, and the session waiting on it, for no longer.
 */
export const DISPATCH_WRITE_RETRY: DispatchWriteRetry = {
  pausesMs: [1_000, 2_000, 4_000, 8_000],
  withinMs: 30_000,
  silenceMs: 20_000,
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
 * How long what this process learned about a Dispatch's keyed layer holds: five minutes from the
 * answer that showed it. A Dispatch rolled back to a release without the layer is still believed to
 * run it for at most that long after it last showed it, which bounds the time in which a failed
 * write could be resent to it and run twice.
 */
export const KEYED_LAYER_MEMORY_MS = 5 * 60_000;

/**
 * The Dispatch origins this process has lately seen run the keyed-write layer, each with when it
 * last saw it (on `DispatchWriteRetry.now`'s clock): an answer that carried back the key its request
 * sent (`keyEchoed`), or a `/healthz` that said `idempotency_keys: true`. Only such a Dispatch is
 * asked again under a key; against any other a repeat would be a second write. An entry older than
 * `KEYED_LAYER_MEMORY_MS` says nothing, and only a yes is kept: a probe that found no layer is asked
 * again by the next write that fails.
 */
const KEYED_LAYER_SEEN = new Map<string, number>();

/**
 * Each origin's `/healthz` probe while it runs, so writes that fail together ask once; dropped once
 * it answers (`DispatchClient.#runsKeyedLayer`).
 */
const KEYED_LAYER_PROBES = new Map<string, Promise<boolean>>();

/** How long the `/healthz` probe waits; it runs only after a write has already failed. */
const KEY_LAYER_PROBE_MS = 5_000;

/**
 * Forgets what this process learned about which Dispatch runs the keyed layer. A test seam: the
 * memory is per process and per origin, so a test that drives one origin as a Dispatch of its own
 * starts from nothing.
 */
export function forgetKeyedLayerMemory(): void {
  KEYED_LAYER_SEEN.clear();
  KEYED_LAYER_PROBES.clear();
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

/**
 * The size from which an attempt's body goes out as a stream read a chunk at a time
 * (`STREAMED_BODY_CHUNK`), so the connection taking each chunk counts as the attempt's progress.
 * A smaller body leaves in one write to the connection, so there is no progress of it to see.
 */
const STREAMED_BODY_MIN = 1 << 20;
const STREAMED_BODY_CHUNK = 64 << 10;

/**
 * What one attempt sends for `body`: its bytes as they are when they are smaller than
 * `STREAMED_BODY_MIN`, or else a stream of them that calls `progress` each time the connection reads
 * a chunk (fetch reads a stream body only as the connection takes it), with the `Content-Length`
 * they add up to, so the request on the wire is the one the bytes make.
 */
function attemptBody(
  body: EncodedBody,
  headers: Record<string, string>,
  progress: () => void
): Pick<RequestInit, "body" | "headers"> {
  const bytes =
    typeof body.content === "string" ? new TextEncoder().encode(body.content) : body.content;
  if (bytes.byteLength < STREAMED_BODY_MIN) return { body: body.content, headers };
  let sent = 0;
  const stream = new ReadableStream<Uint8Array>({
    pull(controller) {
      progress();
      if (sent >= bytes.byteLength) {
        controller.close();
        return;
      }
      const end = Math.min(sent + STREAMED_BODY_CHUNK, bytes.byteLength);
      controller.enqueue(bytes.subarray(sent, end));
      sent = end;
    },
  });
  return { body: stream, headers: { ...headers, "Content-Length": String(bytes.byteLength) } };
}

/** An attempt cut off after `DispatchWriteRetry.silenceMs` without progress before its answer. */
class WriteAttemptSilent extends Error {
  override readonly name = "WriteAttemptSilent";

  constructor(silenceMs: number) {
    super(`made no progress for ${silenceMs / 1_000} s, so it was cut off`);
  }
}

/** One attempt's answer to a write, as the retry loop reads it. */
interface WriteAnswer {
  /** The answer to hand on, its body still unread. */
  readonly response: Response;
  /** Whether it is no definitive answer, so the write asks again under its key. */
  readonly again: boolean;
  /** Whether this attempt, or one Dispatch was still running, may have reached Dispatch. */
  readonly reached: boolean;
  /** Whether it carried back the request's key, so Dispatch's keyed layer judged the request. */
  readonly echoed: boolean;
  /**
   * Whether it is Dispatch's own refusal (its error JSON, with a string `error`) that carried no key
   * back: Dispatch answered, and its keyed layer did not judge the request, so what the process
   * remembers about the origin is not trusted for it (`DispatchClient.#runsKeyedLayer`).
   */
  readonly unjudged: boolean;
}

/**
 * How the retry loop reads one attempt's answer to a write sent under `key`. A 2xx is definitive
 * and handed on unread. Any other answer is read here, for its code and to tell Dispatch's own
 * refusal from a page something in its place wrote, and handed on rebuilt from what was read; it is
 * no definitive answer where `retriesUnderKey` says so: a 5xx (a gateway's, or Dispatch's own, which
 * Dispatch never stores under the key), a 408 or 429 (a timeout or a rate limit), or Dispatch's
 * `409 KEY_IN_FLIGHT` (an earlier attempt still runs under the key).
 */
async function writeAnswer(response: Response, key: string): Promise<WriteAnswer> {
  const echoed = keyEchoed(response, key);
  if (response.ok) return { response, again: false, reached: false, echoed, unjudged: false };
  const { status } = response;
  const text = await response.text();
  let shape: DispatchServiceErrorShape = {};
  if (isJson(response)) {
    try {
      shape = asErrorShape(JSON.parse(text));
    } catch {
      shape = {};
    }
  }
  const again = retriesUnderKey(status, shape.code);
  return {
    response: new Response(text, {
      status,
      statusText: response.statusText,
      headers: response.headers,
    }),
    again,
    reached: status >= 500 || (again && status === 409),
    echoed,
    unjudged: !echoed && typeof shape.error === "string",
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
 * Why `answer` is not the bare array `listAllIssues` asked for. A page here is the telling case:
 * it means Dispatch paged a request that carried neither `limit` nor `offset`, so the caller did
 * not get the exact set it needs. Text looks like a proxy in the way, which a retry can get past.
 */
function whyNotAnArray(answer: unknown): string {
  if (typeof answer === "string")
    return `text that is not JSON, ${GATEWAY_PAGE}, so a retry may succeed`;
  if (answer === null) return "null";
  if (typeof answer !== "object") return `a ${typeof answer}`;
  if ("issues" in answer && Array.isArray(answer.issues)) {
    return "a page, so this Dispatch pages a request that carries no limit and no offset";
  }
  return "an object that is not the listing";
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

  /**
   * Every issue the filters match, in one read: `GET /api/v1/issues` with neither `limit` nor
   * `offset`, which Dispatch answers as a bare array rather than a page (`parseIssuePage` returns
   * no page when the query carries neither). A caller whose own writes move issues out of what it
   * filtered on cannot walk offsets safely, because a row that shifts across a page boundary is
   * served twice or never; this read is the exact set at one moment. Any answer that is not an
   * array is refused, naming what arrived: a page here means a Dispatch that still pages a request
   * carrying no paging parameters.
   */
  async listAllIssues(options: ListIssuesOptions): Promise<IssueSummary[]> {
    const path = ["api", "v1", "issues"];
    const query = { ...options };
    const answer: unknown = await this.#json("GET", path, undefined, query);
    if (Array.isArray(answer)) return answer as IssueSummary[];
    throw new Error(
      `GET ${this.#url(path, query)} asked for the unpaged listing and got ${whyNotAnArray(answer)}.`
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
   *  while the request ran, so nothing was applied. The answer's `legion_managed_status` says
   *  whether the Legion daemon owns this issue's status — true when the issue carries the
   *  `legion` label in any case or sits under one that does, where a status anyone else writes
   *  is undone. */
  async claimIssue(issue: string, input: { readonly actor?: Actor } = {}): Promise<ClaimedIssue> {
    return this.#json(
      "POST",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "claim"],
      input
    );
  }

  /** `DELETE /api/v1/issues/{key}/claim`: give up the claim, or clear one whose session is
   *  gone. The status does not move. It answers the same two 409s, has no force, and carries
   *  `legion_managed_status` as `claimIssue` does. */
  async releaseIssueClaim(
    issue: string,
    input: { readonly actor?: Actor } = {}
  ): Promise<ClaimedIssue> {
    return this.#json(
      "DELETE",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "claim"],
      input
    );
  }

  async getIssueEvents(issue: string, after = 0, limit = ISSUE_EVENTS_PAGE_MAX): Promise<Event[]> {
    return this.#json(
      "GET",
      ["api", "v1", "issues", await this.#resolveIssue(issue), "events"],
      undefined,
      { after, limit }
    );
  }

  /** The issue and its newest `options.events` events (every one for `"all"`), oldest first, up
   *  to the issue's `last_seq` as read. An issue's events are numbered 1 to `last_seq`, so the
   *  newest N are the N after `last_seq - N`, read a page of at most `ISSUE_EVENTS_PAGE_MAX` at a
   *  time, and an event written after the issue was read is left to the next read. */
  async read(issueReference: string, options: IssueReadOptions = {}): Promise<IssueRead> {
    const issueKey = await this.#resolveIssue(issueReference);
    const issue = await this.getIssue(issueKey);
    const requested = options.events;
    const count =
      requested === undefined
        ? DEFAULT_ISSUE_READ_EVENTS
        : requested === "all"
          ? issue.last_seq
          : Math.min(requested, issue.last_seq);
    const events: Event[] = [];
    let after = Math.max(0, issue.last_seq - count);
    while (events.length < count) {
      const limit = Math.min(ISSUE_EVENTS_PAGE_MAX, count - events.length);
      const page = await this.getIssueEvents(issueKey, after, limit);
      events.push(...page);
      const last = page.at(-1);
      // A short page is the end of the issue's events; a full one ends the read once it holds
      // `count`. Only a page the read goes past must have moved past `after`.
      if (page.length < limit || last === undefined || events.length >= count) break;
      if (last.seq <= after) {
        throw new Error(
          `GET /api/v1/issues/${issueKey}/events?after=${after} answered no event after seq ${after}`
        );
      }
      after = last.seq;
    }
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

  /** `GET /api/v1/projects/{key}/architecture-sources`: the project's architecture sources, one
   *  per repository, in the order the project added them; `[]` when it has none.
   *
   *  The plugin and the server ship separately, so a rollout mixes versions (#1522). A server
   *  older than this route has no route for it (`404 NOT_FOUND`, "no route for …"), so the client
   *  asks its `GET /api/v1/projects/{key}/architecture-source` instead: `200 null` (or a pre-#1522
   *  `404 SOURCE_NOT_FOUND`) is `[]`, and a row is that one source. Any other answer throws, so a
   *  project the server does not know (`404 NOT_FOUND`, "not found") is still an error. */
  async listArchitectureSources(project: string): Promise<ArchitectureSource[]> {
    try {
      return await this.#json("GET", ["api", "v1", "projects", project, "architecture-sources"]);
    } catch (error) {
      if (!isNoRoute(error)) throw error;
    }
    try {
      const source: ArchitectureSource | null = await this.#json("GET", [
        "api",
        "v1",
        "projects",
        project,
        "architecture-source",
      ]);
      return source === null ? [] : [source];
    } catch (error) {
      if (
        error instanceof DispatchServiceError &&
        error.status === 404 &&
        error.code === "SOURCE_NOT_FOUND"
      ) {
        return [];
      }
      throw error;
    }
  }

  /** `POST /api/v1/projects/{key}/architecture-sources/sync`: import every source of the
   *  project now. Every source is attempted, and each row's `last_error` says whether its
   *  import replaced its components or its previous ones stay up. 404 SOURCE_NOT_FOUND when the
   *  project has no source.
   *
   *  The plugin and the server ship separately, so a rollout mixes versions (#1522). A server
   *  older than this route has no route for it (`404 NOT_FOUND`, "no route for …"), so the client
   *  asks its `POST /api/v1/projects/{key}/architecture-source/sync` instead, which imports that
   *  server's one source, and returns the row as a one-element array. Any other answer of either
   *  call throws as is, so that server's `404 SOURCE_NOT_FOUND` for a project with no source is
   *  still an error. */
  async syncArchitectureSources(project: string): Promise<ArchitectureSource[]> {
    try {
      return await this.#json("POST", [
        "api",
        "v1",
        "projects",
        project,
        "architecture-sources",
        "sync",
      ]);
    } catch (error) {
      if (!isNoRoute(error)) throw error;
    }
    const source: ArchitectureSource = await this.#json("POST", [
      "api",
      "v1",
      "projects",
      project,
      "architecture-source",
      "sync",
    ]);
    return [source];
  }

  /** `POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync`: import the one
   *  source `repo` (`owner/name`) now; the row's `last_error` says whether the import replaced
   *  its components or its previous ones stay up. 404 SOURCE_NOT_FOUND when the project has no
   *  such source. */
  async syncArchitectureSource(project: string, repo: string): Promise<ArchitectureSource> {
    const [owner, name, ...rest] = repo.split("/");
    if (!owner || !name || rest.length > 0) {
      throw new Error(`repository must be owner/name, got ${JSON.stringify(repo)}`);
    }
    return this.#json("POST", [
      "api",
      "v1",
      "projects",
      project,
      "architecture-sources",
      owner,
      name,
      "sync",
    ]);
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
   * encoded body. While an attempt gets no definitive answer (`writeAnswer`, a transport failure,
   * or `writeRetry.silenceMs` without progress, `#attempt`) the write asks again under its key,
   * paced by `writeRetry`, and the first definitive answer is the call's: a 2xx, a replay of one
   * included, or a refusal. The call's deadline and its caller's cancellation end the attempts at
   * once, whether a fetch or a pause is under way (`#ended`).
   *
   * It asks again only against a Dispatch this process has lately seen run the keyed layer: a
   * Dispatch without the layer would run a repeat as a second write, so its 5xx — which can come
   * after it committed — is reported as it stands rather than resent. A failing answer that echoed
   * the request's key (`keyEchoed`) is that knowledge itself, so one of Dispatch's own 5xx, 429 or
   * `409 KEY_IN_FLIGHT` is asked again at once. Dispatch's own refusal with no echo says its layer
   * did not judge this request, so the write asks `/healthz` afresh whatever the process
   * remembers; a transport failure, a silence cut and an answer something in Dispatch's place wrote
   * say nothing, so they rely on what an answer or `/healthz` showed within
   * `KEYED_LAYER_MEMORY_MS`, and ask `/healthz` when nothing did (`#runsKeyedLayer`).
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
    if (method !== "POST" && method !== "PATCH") {
      return this.fetchImpl(url, {
        method,
        headers,
        signal: this.#signal,
        ...(body === undefined ? {} : { body: body.content }),
      });
    }
    const key = crypto.randomUUID();
    headers[IDEMPOTENCY_KEY_HEADER] = key;
    const { pausesMs, withinMs, silenceMs, pause = pauseFor } = this.writeRetry;
    const request = `${method} ${url}`;
    const started = this.#now();
    let reached = false;
    for (let attempt = 1; ; attempt++) {
      let answer: WriteAnswer | undefined;
      let thrown: unknown;
      try {
        answer = await writeAnswer(await this.#attempt(url, method, headers, body, silenceMs), key);
        if (answer.echoed) KEYED_LAYER_SEEN.set(this.#baseUrl, this.#now());
        if (!answer.again) return answer.response;
        reached ||= answer.reached;
      } catch (error) {
        // A deadline or a cancellation is the caller's limit, never a reason to ask again; the
        // attempt's own silence cut is such a reason, as a transport failure is.
        const name = error instanceof Error || error instanceof DOMException ? error.name : "";
        if (
          !(error instanceof WriteAttemptSilent) &&
          (this.#signal.aborted || name === "AbortError" || name === "TimeoutError")
        ) {
          throw this.#ended(error, request, attempt, true);
        }
        thrown = error;
        reached = true;
      }
      const scheduled = pausesMs[attempt - 1];
      const wait = Math.max(
        scheduled ?? 0,
        answer === undefined ? 0 : retryAfterMs(answer.response)
      );
      // Only a Dispatch that runs the keyed layer may be asked again: against any other a repeat
      // would be a second write.
      const inTime = scheduled !== undefined && this.#now() - started + wait <= withinMs;
      if (!inTime || !(answer?.echoed || (await this.#runsKeyedLayer(answer?.unjudged ?? false)))) {
        if (answer !== undefined) {
          return this.#unsettled(method, url, answer.response, reached, attempt);
        }
        if (thrown instanceof WriteAttemptSilent) {
          throw new DispatchWriteUnsettledError(thrown, attempt, `${request} ${thrown.message}`);
        }
        throw thrown;
      }
      await answer?.response.body?.cancel().catch(() => undefined);
      try {
        await pause(wait, this.#signal);
      } catch (error) {
        throw this.#ended(error, request, attempt, reached);
      }
    }
  }

  /** The write clock: `writeRetry.now`, or the process's monotonic one. */
  #now(): number {
    return this.writeRetry.now?.() ?? performance.now();
  }

  /**
   * One attempt of a write, under the call's signal and a silence bound of its own: once
   * `silenceMs` pass before its answer begins with none of its body taken by the connection in
   * that time, it is cut off and fails as `WriteAttemptSilent`. The bound ends with the answer's
   * headers; its body is read under the call's deadline alone.
   */
  async #attempt(
    url: string,
    method: string,
    headers: Record<string, string>,
    body: EncodedBody | undefined,
    silenceMs: number
  ): Promise<Response> {
    const cut = new AbortController();
    const silent = (): void => cut.abort(new WriteAttemptSilent(silenceMs));
    let timer = setTimeout(silent, silenceMs);
    let answered = false;
    // A stream body read after the answer came (an early refusal) starts no timer of its own.
    const progress = (): void => {
      if (answered) return;
      clearTimeout(timer);
      timer = setTimeout(silent, silenceMs);
    };
    try {
      return await this.fetchImpl(url, {
        method,
        signal: AbortSignal.any([this.#signal, cut.signal]),
        ...(body === undefined ? { headers } : attemptBody(body, headers, progress)),
      });
    } catch (error) {
      if (cut.signal.aborted && !this.#signal.aborted) throw cut.signal.reason;
      throw error;
    } finally {
      answered = true;
      clearTimeout(timer);
    }
  }

  /**
   * What a write throws when its deadline or its caller's cancellation ends it, with `error` the
   * fetch's or the pause's: once an attempt may have reached Dispatch (`mayHaveReached`), the
   * deadline's timeout is a `DispatchWriteUnsettledError` saying the write may or may not have
   * taken effect; a cancellation, the caller's own, and a timeout before anything reached Dispatch
   * are thrown as they are.
   */
  #ended(error: unknown, request: string, attempts: number, mayHaveReached: boolean): unknown {
    const reason: unknown = this.#signal.aborted ? this.#signal.reason : error;
    const timedOut =
      (reason instanceof Error || reason instanceof DOMException) && reason.name === "TimeoutError";
    if (!timedOut || !mayHaveReached || !(error instanceof Error)) return error;
    return new DispatchWriteUnsettledError(
      error,
      attempts,
      `${request} had no definitive answer when its deadline ended it`
    );
  }

  /**
   * Whether this Dispatch runs the keyed layer, so a failed write may be asked again under its
   * key: true while an answer that echoed a key, or a `/healthz` saying `idempotency_keys: true`,
   * is younger than `KEYED_LAYER_MEMORY_MS`, and otherwise what `/healthz` says now, which writes
   * failing together ask once. `unjudged` (Dispatch's own refusal that echoed no key) sets aside
   * what the process remembers, since that answer itself says the layer did not judge the
   * request: the probe decides. Only a yes is kept. It runs only after a write has already
   * failed, so the normal path costs nothing.
   */
  async #runsKeyedLayer(unjudged: boolean): Promise<boolean> {
    const origin = this.#baseUrl;
    if (unjudged) KEYED_LAYER_SEEN.delete(origin);
    const seen = KEYED_LAYER_SEEN.get(origin);
    if (seen !== undefined && this.#now() - seen < KEYED_LAYER_MEMORY_MS) return true;
    let probe = KEYED_LAYER_PROBES.get(origin);
    if (probe === undefined) {
      probe = this.#probeKeyedLayer().finally(() => KEYED_LAYER_PROBES.delete(origin));
      KEYED_LAYER_PROBES.set(origin, probe);
    }
    const runs = await probe;
    if (runs) KEYED_LAYER_SEEN.set(origin, this.#now());
    return runs;
  }

  /**
   * `GET /healthz`'s `idempotency_keys`, bounded at `KEY_LAYER_PROBE_MS`. Its status is not read:
   * Dispatch answers 503 whenever its database or NATS is down, the outage a failed write most
   * likely met, and still says `idempotency_keys: true`, a fact of its binary. A probe that fails,
   * times out, answers no JSON or leaves the field out is false: the write is reported as it
   * stands rather than resent, since a Dispatch without the layer would run a repeat as a second
   * write.
   */
  async #probeKeyedLayer(): Promise<boolean> {
    try {
      const response = await this.fetchImpl(`${this.#baseUrl}/healthz`, {
        method: "GET",
        headers: { Accept: "application/json", Authorization: `Bearer ${this.token}` },
        signal: AbortSignal.any([this.#signal, AbortSignal.timeout(KEY_LAYER_PROBE_MS)]),
      });
      if (!isJson(response)) return false;
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
   * answer is gone — `409 IDEMPOTENCY_KEY_COMMITTED`, or a replayed 2xx reading
   * `IDEMPOTENCY_ANSWER_NOT_KEPT` (`answerNotKept`) — is a `DispatchWriteLandedError`, neither a
   * refusal nor a payload.
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
          throw new DispatchWriteLandedError(
            IDEMPOTENCY_KEY_COMMITTED,
            response.status,
            method,
            url
          );
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
    const replayed = response.headers.get(IDEMPOTENCY_REPLAYED_HEADER)?.trim() === "?1";
    if (answerNotKept(response.status, replayed, payload)) {
      throw new DispatchWriteLandedError(IDEMPOTENCY_ANSWER_NOT_KEPT, response.status, method, url);
    }
    return payload as T;
  }
}
