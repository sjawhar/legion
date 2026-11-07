import {
  AUTH_NO_IDENTITY,
  answerNotKept,
  DISPATCH_LOGIN_HEADER,
  IDEMPOTENCY_ANSWER_NOT_KEPT,
  IDEMPOTENCY_KEY_COMMITTED,
  IDEMPOTENCY_KEY_HEADER,
  IDEMPOTENCY_KEY_IN_FLIGHT,
  IDEMPOTENCY_REPLAYED_HEADER,
  type IdempotencyLandedCode,
} from "@legion/contracts";

import {
  type Answer,
  type Body,
  type FormBody,
  type Json200,
  type Op,
  type PathParams,
  type PathsOf,
  type Query,
  type QueryParams,
  type RequestBody,
  repoParams,
  routeUrl,
} from "./paths";
import { reachable, routeOf, unreachable } from "./reachability";

import type {
  ArtifactVersionContent,
  ArtifactVersionText,
  AuthenticatedUser,
  BlockSchema,
  CreateArtifactInput,
  DispatchEvent,
  DispatchUser,
  DuplicateCandidate,
  IDCandidate,
  IssueAskThreadRow,
  TargetCandidate,
  UserIssueState,
} from "./types";

export type FetchImplementation = (
  input: RequestInfo | URL,
  init?: RequestInit
) => Promise<Response>;

/**
 * How an upload reaches Dispatch: `init` (its body a multipart form) sent to `path`, answered with
 * Dispatch's response, and `progress` called as each piece of the body goes up - with how much of
 * it has gone (`sent`), when the transport knows - and as the answer comes back. The browser's
 * `fetch` reports no upload progress, so a large file on a slow line would look silent for as long
 * as it uploads; `xhrUpload` is the one that reports it.
 */
export type UploadImplementation = (
  path: string,
  init: RequestInit,
  progress: (sent?: UploadProgress) => void
) => Promise<Response>;

/** How much of an upload's body has gone up, in bytes, out of how many. */
export interface UploadProgress {
  readonly loaded: number;
  readonly total: number;
}

/** The largest body a write the page lets finish as it goes (`keepalive`) may carry: the browser
 *  caps every such request's body together at 64 KiB. */
const maxKeepaliveBodyBytes = 30 * 1024;
const utf8 = new TextEncoder();

/** One item a refusal lists for the caller to choose between: the items an ambiguous id prefix
 *  names (`ID_AMBIGUOUS`), the issues a duplicate check found, or the positions an anchored
 *  quote resolved at. Each refusal's own `code` says which of the three its list holds. */
export type ErrorCandidate = IDCandidate | DuplicateCandidate | TargetCandidate;

interface ErrorPayload {
  code?: string;
  error?: string;
  state?: UserIssueState;
  candidates?: readonly ErrorCandidate[];
}

export class ApiError extends Error {
  readonly code: string | undefined;
  readonly status: number;
  readonly state: UserIssueState | undefined;
  /** What the refusal listed for the caller to choose between, read by the page that shows the
   *  choice; the shape of each entry follows `code` (`ErrorCandidate`). Undefined for a refusal
   *  that listed nothing, which is almost all of them. */
  readonly candidates: readonly ErrorCandidate[] | undefined;
  /** How long the server asked the caller to wait before trying again (`Retry-After` in
   *  delta-seconds, as Dispatch sends with a 503 refusing excess load), in milliseconds;
   *  undefined when the response carried none. */
  readonly retryAfterMs: number | undefined;

  constructor(status: number, payload: ErrorPayload, retryAfterMs?: number) {
    super(payload.error ?? `Dispatch request failed (${status})`);
    this.name = "ApiError";
    this.code = payload.code;
    this.status = status;
    this.state = payload.state;
    this.candidates = payload.candidates;
    this.retryAfterMs = retryAfterMs;
  }
}

/** A request the browser could not get an answer to at all: the connection failed or dropped
 *  (`fetch`'s and XHR's own error). It is no refusal - the request may have reached Dispatch - so
 *  a read is asked again by the queries' policy and a write again under its key. */
export class NetworkError extends Error {
  readonly path: string;

  constructor(path: string, cause: unknown) {
    super("Dispatch could not be reached", { cause });
    this.name = "NetworkError";
    this.path = path;
  }
}

/** A 2xx whose body is not JSON - a proxy's page, a body cut short - so it says nothing of what
 *  Dispatch did. The write is asked again under its key, and Dispatch answers the repeat with the
 *  first's answer. */
export class UnreadableAnswerError extends Error {
  readonly status: number;

  constructor(status: number, cause: unknown) {
    super(`Dispatch answered ${status} with a body that is not JSON`, { cause });
    this.name = "UnreadableAnswerError";
    this.status = status;
  }
}

/** The kinds `GET /api/v1/references/questions` reads a reference source as, from the route's
 *  own query schema: a document version, an ask, a comment or an issue message. */
export type ReferenceSourceKind = NonNullable<
  QueryParams<Op<"/api/v1/references/questions", "get">>["kind"]
>;

/** How long the page lets Dispatch go silent on a request - no response headers yet, no piece of
 *  its body, and for an upload no piece of the request body sent either - before it gives the
 *  request up (`RequestTimeoutError`). A read Dispatch never answers, or stops answering halfway,
 *  would otherwise hold its page loading for good; given up, it is retried by the queries' shared
 *  policy, as a dropped connection is. A body that keeps moving, either way, goes on however long
 *  it takes: the deadline is on the silence, not the transfer. */
export const REQUEST_DEADLINE_MS = 30_000;

/** A request Dispatch went silent on for its deadline, which the page then aborted. It is no
 *  refusal: the request may still have reached Dispatch, so only a read, or a write made again
 *  under its key (`writeUnderKey`), is asked again. */
export class RequestTimeoutError extends Error {
  readonly path: string;

  constructor(path: string, deadlineMs: number) {
    super(`Dispatch did not answer within ${deadlineMs / 1000} seconds`);
    this.name = "RequestTimeoutError";
    this.path = path;
  }
}

/** How a caller bounds one attempt of a request: how long Dispatch may go silent on it (`null` for
 *  no limit; left out, `REQUEST_DEADLINE_MS` for a GET, and no limit for any other method). */
export interface RequestBounds {
  readonly deadlineMs?: number | null;
}

/** Every method a write goes as: the one list `WriteRequest.method` and the check of a kept write
 *  (`write-storage.ts`) are both read from. */
export const WRITE_METHODS = ["DELETE", "PATCH", "POST", "PUT"] as const;

export type WriteMethod = (typeof WRITE_METHODS)[number];

/** What a request is answered with, for the type checker alone: no request carries it. */
declare const answeredWith: unique symbol;

/** One write as it goes to Dispatch: its method, its path and its body - JSON, a multipart form
 *  (an upload), or none - and, for the type checker, what Dispatch answers it with (`T`): a
 *  request's builder names it (`issuePatchRequest` answers an `Issue`), so a write made from its
 *  request alone (`WriteSpec.request`) is answered as its client method's is. A kept write is
 *  this, made again after a reload (`send`). */
export interface WriteRequest<T = unknown> {
  readonly method: WriteMethod;
  readonly path: string;
  readonly body?: unknown;
  readonly [answeredWith]?: T;
}

/** One attempt of a write, as the one write path (`Writes.run`) hands it over from the press it
 *  belongs to: the key that names the person's action - minted when the person acts, the same on
 *  every attempt of it, and what the store keeps it under; sent as `Idempotency-Key` on a `POST`
 *  and a `PATCH`, and never on a `PUT` or a `DELETE`, which are the same however often they are
 *  made - the login of the person who pressed it (`Dispatch-Login`, sent on every write, so
 *  Dispatch refuses it under anyone else's session), the attempt's silence deadline, `sent`, told
 *  the request as it goes out, for a caller that keeps it to make again, and `progress`, told how
 *  much of an upload's body has gone up. Every write method takes these, so a write made outside
 *  the store does not type-check. */
export interface WriteBounds extends RequestBounds {
  readonly key: string;
  readonly login?: string;
  readonly sent?: (request: WriteRequest) => void;
  readonly progress?: (loaded: number, total: number) => void;
}

/** A write Dispatch had already made, with no answer it can give for it (`code`): the first
 *  answer was too large for it to keep, so the repeat came back with the first one's status and
 *  no resource (`IDEMPOTENCY_ANSWER_NOT_KEPT`), or the answer was lost before Dispatch stored it,
 *  so the repeat came back `409 KEY_COMMITTED` (`isKeyCommitted`). The write landed; its caller
 *  reads what it wrote again, and shows no refusal. */
export class AnswerNotKeptError extends Error {
  readonly code: IdempotencyLandedCode;
  readonly status: number;

  constructor(code: IdempotencyLandedCode, status: number, message: string) {
    super(message);
    this.name = "AnswerNotKeptError";
    this.code = code;
    this.status = status;
  }
}

/** Whether Dispatch is still running the first request under this write's key: nothing ran, and
 *  the write is asked again under the same key. */
export function isKeyInFlight(error: unknown): error is ApiError {
  return (
    error instanceof ApiError && error.status === 409 && error.code === IDEMPOTENCY_KEY_IN_FLIGHT
  );
}

/** Whether Dispatch made this write's key's write already, and lost the answer before it stored
 *  it (`409 KEY_COMMITTED`, even after a 5xx the first attempt was answered): the write landed, and
 *  Dispatch will not run it again. */
export function isKeyCommitted(error: unknown): error is ApiError {
  return (
    error instanceof ApiError && error.status === 409 && error.code === IDEMPOTENCY_KEY_COMMITTED
  );
}

// A 401 means "not signed in" and should never be retried or confused with a transient
// network/5xx failure, which the caller should retry and eventually surface with a Retry
// affordance instead of bouncing the user to the sign-in page.
export function isUnauthorized(error: unknown): boolean {
  return error instanceof ApiError && error.status === 401;
}

/** Whether `error` is the sign-in check's `503 EVENT_HEAD_UNAVAILABLE`: Dispatch has no event id
 *  it can vouch for as final yet, which a process just restarted while an event write from before
 *  it is still open answers until that write ends. The page asks again until it answers
 *  (`queryRetry`); nothing about the session is wrong. */
export function isEventHeadUnavailable(error: unknown): boolean {
  return (
    error instanceof ApiError && error.status === 503 && error.code === "EVENT_HEAD_UNAVAILABLE"
  );
}

/** Whether `error` is Dispatch's own answer that the caller is signed in to nobody
 *  (`401 AUTH_NO_IDENTITY`), rather than a 401 from something in front of Dispatch - an
 *  authenticating proxy refusing the request, which says nothing about the Dispatch session. A
 *  sign-out's landing turns on the difference: Dispatch's 401 means the session it was ending is
 *  already gone, and a proxy's means the sign-out never reached Dispatch at all.
 */
export function isNoIdentity(error: unknown): boolean {
  return error instanceof ApiError && error.status === 401 && error.code === AUTH_NO_IDENTITY;
}

// The architecture TREE read answers this 404 for a project with no source — definitive like an
// auth outcome, since retrying changes nothing. The project's source list answers `[]` instead:
// the question it asks has "no source" as an ordinary answer, and a 404 made every reader of an
// ordinary project page log a failed request.
export function isSourceNotFound(error: unknown): boolean {
  return error instanceof ApiError && error.status === 404 && error.code === "SOURCE_NOT_FOUND";
}

// A document that no longer exists cannot be restored, so its browser-held edits are discarded
// when the admission read confirms this exact server response. Other 404s remain retryable.
export function isArtifactNotFound(error: unknown): boolean {
  return error instanceof ApiError && error.status === 404 && error.code === "ARTIFACT_NOT_FOUND";
}

// A Dispatch with no secrets broker configured (no DISPATCH_AGENT_SECRETS_URL) answers every
// credential route but the pending list with this 404 — the same class as the architecture-source
// 404 above it: retrying changes nothing. The pending list, which every page reads, answers `null`
// instead (`getCredentialPending`).
export function isCredentialFeatureOff(error: unknown): boolean {
  return error instanceof ApiError && error.status === 404 && error.code === "FEATURE_OFF";
}

// The delivery timeline answers this 404 until someone sets its configuration: definitive like the
// two above, since retrying changes nothing until the settings form is saved.
export function isDeliveryNotConfigured(error: unknown): boolean {
  return (
    error instanceof ApiError && error.status === 404 && error.code === "DELIVERY_NOT_CONFIGURED"
  );
}

// A stored document outside the Proof schema is a repairable state, not a transient failure:
// retrying the read cannot repair it, while opening its live editor could let the browser rewrite it.
export function isDocumentSchemaError(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409 && error.code === "DOC_SCHEMA";
}

// The server refusing a send because the document does not hold the mark it names
// (`docs.VerifyMark`): the margin answers it by writing the mark again over the same words.
export function isAnchorMissing(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409 && error.code === "ANCHOR_MISSING";
}

// The server refusing a write because its text passes a bound Dispatch holds it to: one block
// too long, too many blocks, or an ask's question and options too long together. The refusal
// names what passed which bound, so a page shows those words rather than wording of its own.
export function isCapExceeded(error: unknown): error is ApiError {
  return error instanceof ApiError && error.code === "CAP_EXCEEDED";
}

// A document whose stored history cannot load at all: the state a rebuild from its latest saved
// version repairs, and retrying the read cannot.
export function isDocumentUnloadable(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409 && error.code === "DOCUMENT_UNLOADABLE";
}

/** What every view says while Envoy's agent list refills: the server's own `ENVOY_REFILLING`
 *  message, shown wherever a view would otherwise say an agent is absent. */
export const AGENT_LIST_REFILLING = "The agent list is refilling. Try again in a moment.";

// The agents list and the subscriber reads answer this 503 while the Envoy listener refills its
// copy of the session registry: the list is not known yet, so nothing on it may be read as gone.
// It is transient, so the shared retry policy keeps retrying it like any other 5xx.
export function isAgentListRefilling(error: unknown): boolean {
  return error instanceof ApiError && error.status === 503 && error.code === "ENVOY_REFILLING";
}

// The retry policy every query in the app shares: an auth outcome (401), a missing architecture
// source, an unconfigured credential broker or delivery timeline, a stored document outside
// Proof's schema, or a stored history that cannot load is definitive and retrying it changes
// nothing; any other failure (dropped connection, a read past its deadline, 5xx) is worth a couple
// of automatic attempts before surfacing a Retry affordance to the user.
export function isRetryableQueryError(error: unknown): boolean {
  return (
    !isUnauthorized(error) &&
    !isSourceNotFound(error) &&
    !isCredentialFeatureOff(error) &&
    !isDeliveryNotConfigured(error) &&
    !isDocumentSchemaError(error) &&
    !isDocumentUnloadable(error)
  );
}

/** The codes of the two `503`s Dispatch answers with a `Retry-After` it means: its database is
 *  too busy to take the request now (`DATABASE_BUSY`), or the request ran past the time Dispatch
 *  gives one (`REQUEST_TIMEOUT`). Any other `503` - a gateway's, or Dispatch's own without one of
 *  these codes - names no wait the page can trust, and keeps the page's own backoff. */
const BUSY_CODES: ReadonlySet<string> = new Set(["DATABASE_BUSY", "REQUEST_TIMEOUT"]);

/** How long Dispatch asked to be left before this request is asked again, when it was too busy
 *  for it (`BUSY_CODES`) and said so with a `Retry-After`; `undefined` for anything else. A
 *  write's retry waits it out (`features/writes/keyed-write.ts`). */
export function busyRetryAfterMs(error: unknown): number | undefined {
  if (!(error instanceof ApiError) || error.status !== 503) return undefined;
  return error.code !== undefined && BUSY_CODES.has(error.code) ? error.retryAfterMs : undefined;
}

// The reason to show a reader for a failed write: the server's own message when it answered
// with one, that it did not answer in time when it did not, otherwise the caller's fallback line
// (the request never reached the server, or died in transit).
export function apiErrorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiError || error instanceof RequestTimeoutError
    ? error.message
    : fallback;
}

// Whether the server refused with one of `codes`: a refusal the same request meets every time,
// which its caller shows without a Retry.
export function apiErrorCodeIn(error: unknown, codes: ReadonlySet<string>): error is ApiError {
  return error instanceof ApiError && error.code !== undefined && codes.has(error.code);
}

/** Who an uploaded file belongs to: an issue, a project (its documents), or an agent's
 *  conversation, named by the agent's session id (direct messages on the Agents page). */
export type ArtifactOwner = { issue: string } | { project: string } | { session: string };

export type CreateArtifactReviewInput = Body<"/api/v1/artifacts/{id}/reviews", "post">;

/** Every write verb, as the document names a route's operation; the request carries the method
 *  it uppercases to, which is `WRITE_METHODS`' member of the same name. */
type WriteVerb = Lowercase<WriteMethod>;

/**
 * One write as it goes to Dispatch, named by the route pattern the document holds it under: the
 * URL built from `params` (`routeUrl`, which throws for a parameter the pattern does not hold or
 * has no value for), the body the document gives that route, and - for the type checker alone -
 * the body the document says it is answered with. Nothing here restates a wire shape.
 */
export function writeRequest<M extends WriteVerb, P extends PathsOf<M>>(
  verb: M,
  pattern: P,
  params: PathParams<Op<P, M>>,
  body?: RequestBody<Op<P, M>> | FormBody<Op<P, M>>
): WriteRequest<Answer<Op<P, M>>> {
  return {
    ...(body === undefined ? {} : { body }),
    method: verb.toUpperCase() as WriteMethod,
    path: routeUrl(pattern, params),
  };
}

/** The same-origin URL of one GET route, for a reader that makes no request of its own (an
 *  `<img>` loading a stored version's bytes). */
function readUrl<P extends PathsOf<"get">>(pattern: P, params: PathParams<Op<P, "get">>): string {
  return routeUrl(pattern, params);
}

// The requests of the writes that wait their turn on a line (`WriteSpec.request`), each the exact
// request its client method sends, answered as that method is: a surface names it at the press,
// and the write is made from it alone (`api.send`), so a write waiting behind another is kept as
// the very request that goes out, and nothing of it runs before its turn.

/** `PATCH /api/v1/issues/{key}`: one change of an issue's fields (`patchIssue`). */
export function issuePatchRequest(key: string, input: Body<"/api/v1/issues/{key}", "patch">) {
  return writeRequest("patch", "/api/v1/issues/{key}", { key }, input);
}

/** `POST /api/v1/agents/{session_id}/messages`: a person's message to one session
 *  (`createAgentMessage`). */
export function agentMessageRequest(
  sessionID: string,
  input: Body<"/api/v1/agents/{session_id}/messages", "post">
) {
  return writeRequest(
    "post",
    "/api/v1/agents/{session_id}/messages",
    { session_id: sessionID },
    input
  );
}

/** `PUT /api/v1/me/asks/{id}/snooze` (`snoozeAsk`). */
export function askSnoozeRequest(id: string, snoozedUntil: string) {
  return writeRequest(
    "put",
    "/api/v1/me/asks/{id}/snooze",
    { id },
    { snoozed_until: snoozedUntil }
  );
}

/** `PUT /api/v1/me/threads/{id}/read`: a thread's read mark (`markThreadRead`). */
export function threadReadRequest(threadId: string, readThrough: string) {
  return writeRequest(
    "put",
    "/api/v1/me/threads/{id}/read",
    { id: threadId },
    { read_through: readThrough }
  );
}

/** `PUT /api/v1/me/documents/{id}/read`: the person's last read version of a document
 *  (`putDocumentRead`). */
export function documentReadRequest(id: string, version: number) {
  return writeRequest("put", "/api/v1/me/documents/{id}/read", { id }, { version });
}

/** `PUT /api/v1/me/issues/{key}/state`: the person's state of an issue (`putIssueState`). */
export function issueStateRequest(
  key: string,
  input: Body<"/api/v1/me/issues/{key}/state", "put">
) {
  return writeRequest("put", "/api/v1/me/issues/{key}/state", { key }, input);
}

/** `PUT /api/v1/me/agents/{session_id}/state`: the person's state of a session (`putAgentState`). */
export function agentStateRequest(
  sessionID: string,
  input: Body<"/api/v1/me/agents/{session_id}/state", "put">
) {
  return writeRequest(
    "put",
    "/api/v1/me/agents/{session_id}/state",
    { session_id: sessionID },
    input
  );
}

/** The same-origin route of one stored version's bytes, addressed by the owner and the artifact's
 *  slug as a picture's `dispatch://` reference names it, so an `<img>` loads it with the reader's
 *  own cookie. A version never changes, so the server lets the browser keep it. */
export function artifactVersionPath(owner: ArtifactOwner, slug: string, version: number): string {
  const number = String(version);
  if ("issue" in owner) {
    return readUrl("/api/v1/issues/{key}/artifacts/{slug}/versions/{number}", {
      key: owner.issue,
      number,
      slug,
    });
  }
  if ("project" in owner) {
    return readUrl("/api/v1/projects/{key}/artifacts/{slug}/versions/{number}", {
      key: owner.project,
      number,
      slug,
    });
  }
  return readUrl("/api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}", {
    number,
    session_id: owner.session,
    slug,
  });
}

/** What an upload carries: the JSON body the document gives the three artifact routes for text
 *  written in the browser, and a multipart form - the body those same routes take as
 *  `multipart/form-data` - for a file picked, pasted or dropped. */
function artifactUploadBody(
  input: CreateArtifactInput
): Body<"/api/v1/issues/{key}/artifacts", "post"> | FormData {
  if ("content" in input) {
    return { actor: input.actor, content: input.content, name: input.name, summary: input.summary };
  }
  const form = new FormData();
  form.set("name", input.name);
  if (input.summary !== undefined) {
    form.set("summary", input.summary);
  }
  if (input.actor !== undefined) {
    form.set("actor", JSON.stringify(input.actor));
  }
  form.set("file", input.file);
  return form;
}

/** The wait `Retry-After` names, in milliseconds: its delta-seconds form, which is the one
 *  Dispatch sends. An HTTP-date (Dispatch never sends one) or any other value is no delay. */
function retryAfterMs(response: Response): number | undefined {
  const retryAfter = response.headers.get("Retry-After")?.trim();
  return retryAfter !== undefined && /^\d+$/.test(retryAfter)
    ? Number(retryAfter) * 1_000
    : undefined;
}

async function errorFrom(response: Response): Promise<ApiError> {
  const payload = (await response.json().catch(() => ({}))) as ErrorPayload;
  return new ApiError(response.status, payload, retryAfterMs(response));
}

/** The statuses whose response carries no body, which `new Response` refuses one for. */
const NULL_BODY_STATUSES: ReadonlySet<number> = new Set([101, 204, 205, 304]);

/** `XMLHttpRequest`'s response headers (`getAllResponseHeaders`), as `Headers`. */
function headersOf(xhr: XMLHttpRequest): Headers {
  const headers = new Headers();
  for (const line of xhr
    .getAllResponseHeaders()
    .trim()
    .split(/[\r\n]+/u)) {
    const colon = line.indexOf(":");
    if (colon > 0) headers.append(line.slice(0, colon).trim(), line.slice(colon + 1).trim());
  }
  return headers;
}

/**
 * An upload through `XMLHttpRequest`, the one transport whose upload progress every engine the
 * suite runs reports (`upload.onprogress`), so a file on a slow line is never mistaken for a
 * silent Dispatch: `progress` runs as each piece of the body goes up, as the response's headers
 * arrive, and as each piece of the answer does. The request fails as `fetch` does - a `TypeError`
 * for a connection that failed, the signal's reason once `init.signal` aborts.
 */
export function xhrUpload(
  makeXhr: () => XMLHttpRequest = () => new XMLHttpRequest()
): UploadImplementation {
  return (path, init, progress) => {
    const { promise, reject, resolve } = Promise.withResolvers<Response>();
    const xhr = makeXhr();
    const signal = init.signal ?? undefined;
    const aborted = () => xhr.abort();
    xhr.open(init.method ?? "POST", path);
    xhr.withCredentials = init.credentials === "include";
    for (const [name, value] of new Headers(init.headers)) xhr.setRequestHeader(name, value);
    xhr.upload.onprogress = (event) =>
      progress(
        event?.lengthComputable === true ? { loaded: event.loaded, total: event.total } : undefined
      );
    xhr.onprogress = () => progress();
    xhr.onreadystatechange = () => {
      if (xhr.readyState === xhr.HEADERS_RECEIVED) progress();
    };
    xhr.onload = () => {
      signal?.removeEventListener("abort", aborted);
      resolve(
        new Response(NULL_BODY_STATUSES.has(xhr.status) ? null : xhr.responseText, {
          headers: headersOf(xhr),
          status: xhr.status,
          statusText: xhr.statusText,
        })
      );
    };
    xhr.onerror = () => {
      signal?.removeEventListener("abort", aborted);
      reject(new TypeError("Failed to upload"));
    };
    xhr.onabort = () => reject(signal?.reason ?? new DOMException("Aborted", "AbortError"));
    if (signal?.aborted === true) {
      reject(signal.reason);
      return promise;
    }
    signal?.addEventListener("abort", aborted, { once: true });
    xhr.send(init.body as XMLHttpRequestBodyInit | null | undefined);
    return promise;
  };
}

/** `response` with its body watched: `arrived` runs as each piece of it does, for a reader that
 *  bounds the silence between them (`exchange`'s idle deadline) rather than the download. A
 *  response with no body - a 204, a 304 - is handed back as it is: Chromium and WebKit give one
 *  an empty stream all the same, and refuse a new 204 Response built around it. */
function watchBody(response: Response, arrived: () => void): Response {
  const body = response.body;
  if (body === null || NULL_BODY_STATUSES.has(response.status)) return response;
  const pieces = body.getReader();
  const watched = new ReadableStream<Uint8Array>({
    async pull(toReader) {
      const piece = await pieces.read();
      if (piece.done) {
        toReader.close();
        return;
      }
      arrived();
      toReader.enqueue(piece.value);
    },
    cancel: (reason) => pieces.cancel(reason),
  });
  return new Response(watched, {
    headers: response.headers,
    status: response.status,
    statusText: response.statusText,
  });
}

export class DispatchApiClient {
  /** `upload` is the transport of a multipart form; left out, it goes by `fetchImpl`, which
   *  reports no upload progress, so an upload's deadline then runs from its send. */
  constructor(
    private readonly fetchImpl: FetchImplementation,
    private readonly upload?: UploadImplementation
  ) {}

  /** One request to Dispatch, answered with what `read` makes of its response. It is held to a
   *  deadline on Dispatch's silence - `deadlineMs`, or `REQUEST_DEADLINE_MS` for a GET that names
   *  none, and none for any other method that names none - that starts over as the request's body
   *  goes up (an upload's), as the response's headers arrive and as each piece of its body does,
   *  `read` included: a transfer that keeps moving goes on however slowly, either way, and one
   *  that stops fails as headers that never come do. A caller's `init.signal` still aborts the
   *  request, as itself. */
  private async exchange<T>(
    path: string,
    init: RequestInit,
    deadlineMs: number | null | undefined,
    read: (response: Response) => Promise<T>,
    uploaded?: (loaded: number, total: number) => void
  ): Promise<T> {
    const limit =
      deadlineMs !== undefined
        ? deadlineMs
        : (init.method ?? "GET").toUpperCase() === "GET"
          ? REQUEST_DEADLINE_MS
          : null;
    if (limit === null) return read(await this.answered(path, init, undefined, uploaded));
    const deadline = new AbortController();
    let timer = setTimeout(() => deadline.abort(), limit);
    const caller = init.signal ?? undefined;
    const arrived = () => {
      clearTimeout(timer);
      timer = setTimeout(() => deadline.abort(), limit);
    };
    try {
      const signal =
        caller === undefined ? deadline.signal : AbortSignal.any([caller, deadline.signal]);
      return await read(await this.answered(path, { ...init, signal }, arrived, uploaded));
    } catch (error) {
      // An answer Dispatch gave stays its answer and the caller's abort stays the caller's: only
      // what the deadline cut short is a timeout.
      if (deadline.signal.aborted && !(error instanceof ApiError) && caller?.aborted !== true) {
        // Dispatch went silent on it: its route cannot be reached, as far as the page knows.
        unreachable(routeOf(init.method, path));
        throw new RequestTimeoutError(path, limit);
      }
      throw error;
    } finally {
      clearTimeout(timer);
    }
  }

  /** Dispatch's response to one request, or its refusal as an `ApiError`; a connection that
   *  failed is a `NetworkError`, and says Dispatch cannot be reached on its route - its method
   *  and path (`unreachable`). `arrived`, for a request held to a deadline, runs as the request's
   *  body goes up (an upload's), as the response's headers arrive and as each piece of its body
   *  does, the body of a refusal included; `uploaded` is told how much of an upload's body has gone
   *  up. Any answer of Dispatch's own - one below 500, which no gateway gives for a Dispatch that
   *  is down - says it can be reached on that route (`reachable`), which matters only once it
   *  could not be. */
  private async answered(
    path: string,
    init: RequestInit,
    arrived?: () => void,
    uploaded?: (loaded: number, total: number) => void
  ): Promise<Response> {
    const sent = { credentials: "same-origin", ...init } satisfies RequestInit;
    const progress = (part?: UploadProgress) => {
      arrived?.();
      if (part !== undefined) uploaded?.(part.loaded, part.total);
    };
    let answer: Response;
    try {
      answer =
        sent.body instanceof FormData && this.upload !== undefined
          ? await this.upload(path, sent, progress)
          : await this.fetchImpl(path, sent);
    } catch (error) {
      // An abort is the deadline's or the caller's (`exchange`), never the network's.
      if (init.signal?.aborted === true) throw error;
      unreachable(routeOf(init.method, path));
      throw new NetworkError(path, error);
    }
    if (answer.status < 500) reachable("answer", routeOf(init.method, path));
    arrived?.();
    const response = arrived === undefined ? answer : watchBody(answer, arrived);
    if (!response.ok) throw await errorFrom(response);
    return response;
  }

  /** The response itself, for a caller that reads its body: the deadline holds until the
   *  response arrives, and the caller reads the body on its own time. */
  private response(
    path: string,
    init: RequestInit = {},
    deadlineMs?: number | null
  ): Promise<Response> {
    return this.exchange(path, init, deadlineMs, async (response) => response);
  }

  private json<T>(path: string, init: RequestInit = {}, deadlineMs?: number | null): Promise<T> {
    return this.exchange(path, init, deadlineMs, (response) => response.json() as Promise<T>);
  }

  /**
   * The one path every write to Dispatch takes. A `POST` or a `PATCH` goes under `bounds.key`, the
   * `Idempotency-Key` that names the person's action, so a repeat of it - a retry after a dropped
   * answer, a reload resuming it - is answered with what the first did rather than done twice; a
   * `PUT` or a `DELETE` is the same however often it is made, so it carries none, and one with a
   * small body is let finish as the page goes (`keepalive`). The key is the caller's, minted when
   * the person acts (`Writes.run`), never here; so is `bounds.login` (`Dispatch-Login`), the
   * person who pressed it. A JSON body goes as JSON and a multipart form as itself, through the
   * upload transport; `bounds.sent` is told the request as it goes out. Answers what Dispatch
   * answered (nothing for an empty body), as the request's builder names it (`WriteRequest`), or
   * rejects with its refusal; a 2xx whose body is not JSON rejects with `UnreadableAnswerError`,
   * and a replay of an answer Dispatch did not keep with `AnswerNotKeptError`.
   */
  send<T>(
    request: WriteRequest<T>,
    { deadlineMs, key, login, progress, sent }: WriteBounds
  ): Promise<T> {
    const { body, method, path } = request;
    const idempotent = method === "PUT" || method === "DELETE";
    const form = body instanceof FormData ? body : undefined;
    const json = body === undefined || form !== undefined ? undefined : JSON.stringify(body);
    const keepalive =
      idempotent && (json === undefined || utf8.encode(json).byteLength < maxKeepaliveBodyBytes);
    const init: RequestInit = {
      ...(form !== undefined ? { body: form } : json !== undefined ? { body: json } : {}),
      headers: {
        ...(idempotent ? {} : { [IDEMPOTENCY_KEY_HEADER]: key }),
        ...(login === undefined ? {} : { [DISPATCH_LOGIN_HEADER]: login }),
        ...(json === undefined ? {} : { "Content-Type": "application/json" }),
      },
      ...(keepalive ? { keepalive: true } : {}),
      method,
    };
    sent?.(request);
    const read = async (response: Response): Promise<T> => {
      const text = await response.text();
      let answer: unknown;
      try {
        answer = text === "" ? undefined : JSON.parse(text);
      } catch (error) {
        throw new UnreadableAnswerError(response.status, error);
      }
      if (
        answerNotKept(response.status, response.headers.has(IDEMPOTENCY_REPLAYED_HEADER), answer)
      ) {
        const reason = Reflect.get(answer as object, "error");
        throw new AnswerNotKeptError(
          IDEMPOTENCY_ANSWER_NOT_KEPT,
          response.status,
          typeof reason === "string" ? reason : "Dispatch did not keep this write's answer"
        );
      }
      return answer as T;
    };
    return this.exchange(path, init, deadlineMs, read, progress);
  }

  /** One read of a route the document holds, named by its own path pattern: `params` fills the
   *  pattern's `{name}`s and `query` its query parameters, and what it answers is the document's
   *  `200` body for that route. No read states a response shape of its own. */
  private read<P extends PathsOf<"get">>(
    pattern: P,
    params: PathParams<Op<P, "get">>,
    query?: QueryParams<Op<P, "get">>,
    init: RequestInit = {},
    deadlineMs?: number | null
  ): Promise<Json200<Op<P, "get">>> {
    return this.json<Json200<Op<P, "get">>>(routeUrl(pattern, params, query), init, deadlineMs);
  }

  private post<P extends PathsOf<"post">>(
    pattern: P,
    params: PathParams<Op<P, "post">>,
    body: RequestBody<Op<P, "post">> | FormBody<Op<P, "post">>,
    bounds: WriteBounds
  ): Promise<Answer<Op<P, "post">>> {
    return this.send<Answer<Op<P, "post">>>(writeRequest("post", pattern, params, body), bounds);
  }

  private put<P extends PathsOf<"put">>(
    pattern: P,
    params: PathParams<Op<P, "put">>,
    body: RequestBody<Op<P, "put">>,
    bounds: WriteBounds
  ): Promise<Answer<Op<P, "put">>> {
    return this.send<Answer<Op<P, "put">>>(writeRequest("put", pattern, params, body), bounds);
  }

  private patch<P extends PathsOf<"patch">>(
    pattern: P,
    params: PathParams<Op<P, "patch">>,
    body: RequestBody<Op<P, "patch">>,
    bounds: WriteBounds
  ): Promise<Answer<Op<P, "patch">>> {
    return this.send<Answer<Op<P, "patch">>>(writeRequest("patch", pattern, params, body), bounds);
  }

  private remove<P extends PathsOf<"delete">>(
    pattern: P,
    params: PathParams<Op<P, "delete">>,
    bounds: WriteBounds
  ): Promise<Answer<Op<P, "delete">>> {
    return this.send<Answer<Op<P, "delete">>>(writeRequest("delete", pattern, params), bounds);
  }

  /** The block schema. The route answers pre-rendered JSON bytes (`pmdoc.SchemaJSON`), so the
   *  document gives its body no schema at all; `BlockSchema` is generated from the same source
   *  those bytes are rendered from, so this one read names what it is answered with. */
  getBlockSchema(): Promise<BlockSchema> {
    return this.json<BlockSchema>(routeUrl("/api/v1/schema/blocks", {}));
  }

  listProjects() {
    return this.read("/api/v1/projects", {});
  }

  createProject(input: Body<"/api/v1/projects", "post">, bounds: WriteBounds) {
    return this.post("/api/v1/projects", {}, input, bounds);
  }

  listRepoProjects() {
    return this.read("/api/v1/settings/repo-projects", {});
  }

  putRepoProject(
    repo: string,
    input: Body<"/api/v1/settings/repo-projects/{owner}/{repo}", "put">,
    bounds: WriteBounds
  ) {
    return this.put(
      "/api/v1/settings/repo-projects/{owner}/{repo}",
      repoParams(repo),
      input,
      bounds
    );
  }

  async deleteRepoProject(repo: string, bounds: WriteBounds): Promise<void> {
    await this.remove("/api/v1/settings/repo-projects/{owner}/{repo}", repoParams(repo), bounds);
  }

  /** Only the projects that name a default; a project left out names nobody. Needs the
   *  `person` scope. */
  listProjectDefaultAssignees() {
    return this.read("/api/v1/settings/project-default-assignees", {});
  }

  /** Sets the person a project's unowned work lands on, or clears it with `{assignee: null}`.
   *  Needs the `person` scope, and a login nobody has signed in as is refused
   *  `400 ASSIGNEE_NOT_ALLOWED`. */
  putProjectDefaultAssignee(
    project: string,
    input: Body<"/api/v1/projects/{key}/default-assignee", "put">,
    bounds: WriteBounds
  ) {
    return this.put("/api/v1/projects/{key}/default-assignee", { key: project }, input, bounds);
  }

  /** Every project's architecture sources, by project and then in the order each was added. */
  listArchitectureSources() {
    return this.read("/api/v1/settings/architecture-sources", {});
  }

  /** The project's sources, one per repository, in the order they were added; `[]` when it has
   *  none: having none is the common answer, not a failure. */
  listProjectArchitectureSources(project: string) {
    return this.read("/api/v1/projects/{key}/architecture-sources", { key: project });
  }

  /** Adds `repo` (`owner/name`) as a source of the project, or changes the branch of the source
   *  it already is. */
  putArchitectureSource(
    project: string,
    repo: string,
    input: Body<"/api/v1/projects/{key}/architecture-sources/{owner}/{repo}", "put">,
    bounds: WriteBounds
  ) {
    return this.put(
      "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}",
      { key: project, ...repoParams(repo) },
      input,
      bounds
    );
  }

  /** Imports one source's model now; the row's `last_error` says whether the import failed. */
  syncArchitectureSource(project: string, repo: string, bounds: WriteBounds) {
    return this.post(
      "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync",
      { key: project, ...repoParams(repo) },
      undefined,
      bounds
    );
  }

  async deleteArchitectureSource(
    project: string,
    repo: string,
    bounds: WriteBounds
  ): Promise<void> {
    await this.remove(
      "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}",
      { key: project, ...repoParams(repo) },
      bounds
    );
  }

  /** The project's component tree with the work attached; 404 SOURCE_NOT_FOUND without a source. */
  getArchitecture(project: string) {
    return this.read("/api/v1/projects/{key}/architecture", { key: project });
  }

  listAgentTokens() {
    return this.read("/api/v1/me/agent-tokens", {});
  }

  createAgentToken(input: Body<"/api/v1/me/agent-tokens", "post">, bounds: WriteBounds) {
    return this.post("/api/v1/me/agent-tokens", {}, input, bounds);
  }

  /** `PATCH /api/v1/me/agent-tokens/{id}`: the whole set of scopes the token is to hold, which
   *  replaces the set it holds now, answering the updated token. A scope the caller does not hold
   *  itself is refused, as it is at the mint. */
  setAgentTokenScopes(
    id: string,
    scopes: NonNullable<Body<"/api/v1/me/agent-tokens/{id}", "patch">>["scopes"],
    bounds: WriteBounds
  ) {
    return this.patch("/api/v1/me/agent-tokens/{id}", { id }, { scopes }, bounds);
  }

  async revokeAgentToken(id: string, bounds: WriteBounds): Promise<void> {
    await this.remove("/api/v1/me/agent-tokens/{id}", { id }, bounds);
  }

  /** Everyone who has signed in, sorted by email: the assignee picker's options. Dispatch
   *  answers `null` for nobody, which is the empty list. */
  async listUsers(): Promise<DispatchUser[]> {
    return (await this.read("/api/v1/users", {})).users ?? [];
  }

  /** The issue list's rows. The route answers the plain list, or a page object when the caller
   *  asks for one (`limit`/`offset`); either arm is read here, so a caller sees rows whichever
   *  it was answered with. */
  async listIssues(query: Query<"/api/v1/issues", "get"> = {}) {
    const answer = await this.read("/api/v1/issues", {}, query);
    return Array.isArray(answer) ? answer : (answer.issues ?? []);
  }

  search(query: string, options: Omit<Query<"/api/v1/search", "get">, "q"> = {}) {
    return this.read("/api/v1/search", {}, { q: query, ...options });
  }

  /**
   * `GET /api/v1/projects/{key}/related`: the related-item check over a title, recording nothing.
   * The create dialog asks while a person types, so a caller passes the `signal` of the check it
   * is still interested in and a superseded one is aborted rather than raced.
   */
  relatedItems(project: string, title: string, options: { signal?: AbortSignal } = {}) {
    return this.read(
      "/api/v1/projects/{key}/related",
      { key: project },
      { title },
      { signal: options.signal }
    );
  }

  createIssue(input: Body<"/api/v1/issues", "post">, bounds: WriteBounds) {
    return this.post("/api/v1/issues", {}, input, bounds);
  }

  getIssue(key: string) {
    return this.read("/api/v1/issues/{key}", { key });
  }

  /** What a reference to the issue or one of its documents shows: title, status and each
   *  document's name, kind and latest version, without the full issue read. */
  getIssueSummary(key: string) {
    return this.read("/api/v1/issues/{key}/summary", { key });
  }

  patchIssue(key: string, input: Body<"/api/v1/issues/{key}", "patch">, bounds: WriteBounds) {
    return this.send(issuePatchRequest(key, input), bounds);
  }

  /** `DELETE /api/v1/issues/{key}/claim`: release the claim. A human may release any claim;
   *  the issue's status does not move. */
  releaseIssueClaim(key: string, bounds: WriteBounds) {
    return this.remove("/api/v1/issues/{key}/claim", { key }, bounds);
  }

  /** `GET /api/v1/issues/{key}/events`: a page of the issue's event log. The document types every
   *  event's `payload` as `unknown`, because Go carries it as the bytes it was written as and no
   *  OpenAPI schema can say which payload belongs to which `type`; `DispatchEvent` is that union
   *  over the same bytes, so this one response shape is named here rather than read. */
  getIssueEvents(
    key: string,
    query: Query<"/api/v1/issues/{key}/events", "get"> = {}
  ): Promise<DispatchEvent[]> {
    return this.json<DispatchEvent[]>(routeUrl("/api/v1/issues/{key}/events", { key }, query));
  }

  /** `GET /api/v1/issues/{key}/results`: the checks of the issue's Verification document in
   *  document order, each with every result recorded against it from any issue, newest first,
   *  and each result's currency as the server judged it against the repository. */
  getIssueResults(key: string) {
    return this.read("/api/v1/issues/{key}/results", { key });
  }

  /** `GET /api/v1/issues/{key}/coverage`: what each requirement of the issue's spec lacks. The
   *  server decides the gaps - a requirement with no plan task is no gap until the issue has a
   *  plan document - so a reader draws its answer rather than judging again. */
  getIssueCoverage(key: string) {
    return this.read("/api/v1/issues/{key}/coverage", { key });
  }

  getInbox(project?: string) {
    return this.read("/api/v1/inbox", {}, { project });
  }

  /** `PUT /api/v1/me/asks/{id}/snooze`: fold this inbox row away for the caller until
   *  `snoozedUntil` (RFC3339). Needs the `person` scope, like the inbox itself. */
  snoozeAsk(id: string, snoozedUntil: string, bounds: WriteBounds) {
    return this.send(askSnoozeRequest(id, snoozedUntil), bounds);
  }

  /** `DELETE /api/v1/me/asks/{id}/snooze`: bring the row back now. A row that is not snoozed
   *  answers the same 204. */
  async unsnoozeAsk(id: string, bounds: WriteBounds): Promise<void> {
    await this.remove("/api/v1/me/asks/{id}/snooze", { id }, bounds);
  }

  listAgents() {
    return this.read("/api/v1/agents", {});
  }

  createAsk(key: string, input: Body<"/api/v1/issues/{key}/asks", "post">, bounds: WriteBounds) {
    return this.post("/api/v1/issues/{key}/asks", { key }, input, bounds);
  }

  /** `GET /api/v1/issues/{key}/asks`: the issue's asks with their newest reply and no thread,
   *  for a reader that renders the ask itself. */
  listIssueAsks(key: string, state: "all" | "open" | "answered" = "all") {
    return this.read("/api/v1/issues/{key}/asks", { key }, { state });
  }

  /** `GET /api/v1/issues/{key}/asks?thread=1`: the same rows carrying each ask's thread. The
   *  list is unpaged, so only a reader that draws the threads asks for them. The response the
   *  document gives the route leaves `thread` out, since asking for it is what makes it present,
   *  so this one read names what it is answered with. */
  listIssueAskThreads(
    key: string,
    state: "all" | "open" | "answered" = "all"
  ): Promise<IssueAskThreadRow[]> {
    return this.json<IssueAskThreadRow[]>(
      routeUrl("/api/v1/issues/{key}/asks", { key }, { state, thread: "1" })
    );
  }

  answerAsk(id: string, input: Body<"/api/v1/asks/{id}/answer", "post">, bounds: WriteBounds) {
    return this.post("/api/v1/asks/{id}/answer", { id }, input, bounds);
  }

  getAsk(id: string) {
    return this.read("/api/v1/asks/{id}", { id });
  }

  /** `GET /api/v1/me/answers`: the signed-in person's answers and ask replies, newest first,
   *  one page at a time. Needs the `person` scope. */
  listMyAnswers(page: Query<"/api/v1/me/answers", "get">) {
    return this.read("/api/v1/me/answers", {}, page);
  }

  /** `GET /api/v1/me/replies`: the threads the signed-in person wrote in where someone else has
   *  written since they last read it, newest reply first; `issue` or `artifact` narrows to one
   *  owner. Needs the `person` scope. */
  listMyReplies(page: Query<"/api/v1/me/replies", "get">) {
    return this.read("/api/v1/me/replies", {}, page);
  }

  /** `PUT /api/v1/me/threads/{id}/read`: move the signed-in person's read mark on one thread
   *  forward to `readThrough` (RFC3339, the newest reply shown). Answers 204 whether or not it
   *  moved. */
  async markThreadRead(threadId: string, readThrough: string, bounds: WriteBounds): Promise<void> {
    await this.send(threadReadRequest(threadId, readThrough), bounds);
  }

  async removeAskFollower(id: string, sessionId: string, bounds: WriteBounds): Promise<void> {
    await this.remove(
      "/api/v1/asks/{id}/followers/{session_id}",
      { id, session_id: sessionId },
      bounds
    );
  }

  listComments(key: string, artifact?: string) {
    return this.read("/api/v1/issues/{key}/comments", { key }, { artifact });
  }

  createComment(
    key: string,
    input: Body<"/api/v1/issues/{key}/comments", "post">,
    bounds: WriteBounds
  ) {
    return this.post("/api/v1/issues/{key}/comments", { key }, input, bounds);
  }

  getComment(id: string) {
    return this.read("/api/v1/comments/{id}", { id });
  }

  resolveComment(id: string, bounds: WriteBounds) {
    return this.post("/api/v1/comments/{id}/resolve", { id }, {}, bounds);
  }

  reopenComment(id: string, bounds: WriteBounds) {
    return this.post("/api/v1/comments/{id}/reopen", { id }, {}, bounds);
  }

  editComment(id: string, input: Body<"/api/v1/comments/{id}", "patch">, bounds: WriteBounds) {
    return this.patch("/api/v1/comments/{id}", { id }, input, bounds);
  }

  /** The comment's current blocks, each with its span in the stored body, and its revision: what
   *  the editor carries block ids from (`editComment`'s `blocks` and `base_revision`). */
  commentBlocks(id: string) {
    return this.read("/api/v1/comments/{id}/blocks", { id });
  }

  acceptComment(id: string, bounds: WriteBounds) {
    return this.post("/api/v1/comments/{id}/accept", { id }, {}, bounds);
  }

  rejectComment(id: string, bounds: WriteBounds) {
    return this.post("/api/v1/comments/{id}/reject", { id }, {}, bounds);
  }

  createCommentDelivery(
    id: string,
    target: string | undefined,
    delivery: "btw" | "aside" | "steer",
    bounds: WriteBounds
  ) {
    return this.post(
      "/api/v1/comments/{id}/deliveries",
      { id },
      { delivery, ...(target === undefined ? {} : { target }) },
      bounds
    );
  }

  createMessage(
    key: string,
    input: Body<"/api/v1/issues/{key}/messages", "post">,
    bounds: WriteBounds
  ) {
    return this.post("/api/v1/issues/{key}/messages", { key }, input, bounds);
  }

  /** A person's message to one session. */
  createAgentMessage(
    sessionID: string,
    input: Body<"/api/v1/agents/{session_id}/messages", "post">,
    bounds: WriteBounds
  ) {
    return this.send(agentMessageRequest(sessionID, input), bounds);
  }

  listAgentMessages(sessionID: string) {
    return this.read("/api/v1/agents/{session_id}/messages", { session_id: sessionID });
  }

  /** A person's message to many sessions. */
  createBroadcast(input: Body<"/api/v1/broadcasts", "post">, bounds: WriteBounds) {
    return this.post("/api/v1/broadcasts", {}, input, bounds);
  }

  listBroadcasts() {
    return this.read("/api/v1/broadcasts", {});
  }

  getBroadcast(id: string) {
    return this.read("/api/v1/broadcasts/{id}", { id });
  }

  createMessageDelivery(id: string, delivery: "btw" | "aside" | "steer", bounds: WriteBounds) {
    return this.post("/api/v1/messages/{id}/deliveries", { id }, { delivery }, bounds);
  }

  getMessage(key: string, id: string) {
    return this.read("/api/v1/issues/{key}/messages/{id}", { id, key });
  }

  /** `GET /api/v1/issues/{key}/messages`: every root message of the issue, oldest first, each
   *  with its deliveries and its replies. */
  listIssueMessages(key: string) {
    return this.read("/api/v1/issues/{key}/messages", { key });
  }

  /** `GET /api/v1/issues/{key}/handoffs`: every phase handoff of the issue, newest first, without
   *  its body. */
  listHandoffs(key: string) {
    return this.read("/api/v1/issues/{key}/handoffs", { key });
  }

  /** `GET /api/v1/handoffs/{id}`: one handoff with the body its session wrote. */
  getHandoff(id: string) {
    return this.read("/api/v1/handoffs/{id}", { id });
  }

  listArtifacts(key: string) {
    return this.read("/api/v1/issues/{key}/artifacts", { key });
  }

  listProjectArtifacts(key: string, unlinked?: boolean) {
    return this.read("/api/v1/projects/{key}/artifacts", { key }, { unlinked });
  }

  uploadArtifact(owner: ArtifactOwner, input: CreateArtifactInput, bounds: WriteBounds) {
    const body = artifactUploadBody(input);
    if ("issue" in owner) {
      return this.post("/api/v1/issues/{key}/artifacts", { key: owner.issue }, body, bounds);
    }
    if ("project" in owner) {
      return this.post("/api/v1/projects/{key}/artifacts", { key: owner.project }, body, bounds);
    }
    return this.post(
      "/api/v1/agents/{session_id}/artifacts",
      { session_id: owner.session },
      body,
      bounds
    );
  }

  getArtifact(id: string) {
    return this.read("/api/v1/artifacts/{id}", { id });
  }

  getProjectArtifact(key: string, slug: string) {
    return this.read("/api/v1/projects/{key}/artifacts/{slug}", { key, slug });
  }

  /** What a reference to a project document shows: name, kind and latest version, without its
   *  versions. */
  getProjectDocumentSummary(key: string, slug: string) {
    return this.read("/api/v1/projects/{key}/artifacts/{slug}/summary", { key, slug });
  }

  /** An artifact of an agent's conversation, with its versions. */
  getAgentArtifact(sessionId: string, slug: string) {
    return this.read("/api/v1/agents/{session_id}/artifacts/{slug}", {
      session_id: sessionId,
      slug,
    });
  }

  getArtifactText(id: string) {
    return this.read("/api/v1/artifacts/{id}/text", { id });
  }

  rebuildArtifact(id: string, markdown: string | undefined, bounds: WriteBounds) {
    return this.post(
      "/api/v1/artifacts/{id}/rebuild",
      { id },
      markdown === undefined ? {} : { markdown },
      bounds
    );
  }

  getArtifactBlocks(id: string) {
    return this.read("/api/v1/artifacts/{id}/blocks", { id });
  }

  /** An issue's decision records: its decisions.md, the open questions its spec holds, and each
   *  decision with the spec blocks it shaped. */
  getIssueDecisions(key: string) {
    return this.read("/api/v1/issues/{key}/decisions", { key });
  }

  /** Record that the caller agrees with the decision that heading block opens, as the text they
   *  read: `textSha256` is the decision's own `text_sha256`, and a decision that changed since
   *  is `409 DECISION_CHANGED`. */
  keepDecision(id: string, blockId: string, textSha256: string, bounds: WriteBounds) {
    return this.post(
      "/api/v1/artifacts/{id}/blocks/{block_id}/keep",
      { block_id: blockId, id },
      { text_sha256: textSha256 },
      bounds
    );
  }

  /** The caller's last read version of a document; its version is null when they have read none. */
  getDocumentRead(id: string) {
    return this.read("/api/v1/me/documents/{id}/read", { id });
  }

  /** Record the caller's last read version of a document: the Spec tab marks each block that
   *  changed since it. */
  putDocumentRead(id: string, version: number, bounds: WriteBounds) {
    return this.send(documentReadRequest(id, version), bounds);
  }

  /** One version's bytes. A large file on a slow line can take longer than any read deadline, so
   *  this GET carries none. */
  async getArtifactVersion(id: string, version: number): Promise<ArtifactVersionContent> {
    const response = await this.response(
      routeUrl("/api/v1/artifacts/{id}/versions/{number}", { id, number: String(version) }),
      {},
      null
    );
    const mime = response.headers.get("Content-Type") ?? "application/octet-stream";

    if (mime.includes("application/json")) {
      return (await response.json()) as ArtifactVersionText;
    }

    return { blob: await response.blob(), mime, version };
  }

  createArtifactVersion(
    id: string,
    input: Body<"/api/v1/artifacts/{id}/versions", "post">,
    bounds: WriteBounds
  ) {
    return this.post("/api/v1/artifacts/{id}/versions", { id }, input, bounds);
  }

  listArtifactReviews(id: string) {
    return this.read("/api/v1/artifacts/{id}/reviews", { id });
  }

  createArtifactReview(id: string, input: CreateArtifactReviewInput, bounds: WriteBounds) {
    return this.post("/api/v1/artifacts/{id}/reviews", { id }, input, bounds);
  }

  listArtifactAsks(id: string, state: "all" | "open" | "answered" = "all") {
    return this.read("/api/v1/artifacts/{id}/asks", { id }, { state });
  }

  createArtifactAsk(
    id: string,
    input: Body<"/api/v1/artifacts/{id}/asks", "post">,
    bounds: WriteBounds
  ) {
    return this.post("/api/v1/artifacts/{id}/asks", { id }, input, bounds);
  }

  listArtifactComments(id: string) {
    return this.read("/api/v1/artifacts/{id}/comments", { id });
  }

  createArtifactComment(
    id: string,
    input: Body<"/api/v1/artifacts/{id}/comments", "post">,
    bounds: WriteBounds
  ) {
    return this.post("/api/v1/artifacts/{id}/comments", { id }, input, bounds);
  }

  getArtifactSubscribers(id: string) {
    return this.read("/api/v1/artifacts/{id}/subscribers", { id });
  }

  async unsubscribeArtifactSession(
    id: string,
    sessionId: string,
    bounds: WriteBounds
  ): Promise<void> {
    await this.remove(
      "/api/v1/artifacts/{id}/subscribers/{session_id}",
      { id, session_id: sessionId },
      bounds
    );
  }

  getIssueReferences(key: string) {
    return this.read("/api/v1/issues/{key}/references", { key });
  }
  /** The edges at a node; `kind` keeps those of one kind - `part_of` lists a document's blocks,
   *  which a document's read otherwise leaves out. */
  getReferences(reference: string, options: { kind?: string } = {}) {
    return this.read("/api/v1/references", {}, { kind: options.kind, to: reference });
  }

  /** The question map of the document one reference source's text belongs to: every question
   *  block that document's latest version declares, with the ask settled for it (for a copied
   *  block, the ask it was copied from). `kind` is the source's own kind - artifact, ask,
   *  comment or message - and `id` its id. A source whose text belongs to no document is 404. */
  getReferenceQuestions(kind: ReferenceSourceKind, id: string) {
    return this.read("/api/v1/references/questions", {}, { id, kind });
  }

  /** One deploy-repository run with every job it ran, for the drill-down's job lists. */
  getDeliveryRun(id: number) {
    return this.read("/api/v1/delivery/runs/{id}", { id: String(id) });
  }

  /** The delivery timeline's configuration record, or `null` until someone sets it. */
  getDeliverySettings() {
    return this.read("/api/v1/settings/delivery", {});
  }

  /** Replaces the delivery timeline's configuration (human callers only). The server first proves
   *  the GitHub App can read `deploy_repo`, answering `409 DELIVERY_SETTINGS_ACCESS` when not. */
  putDeliverySettings(input: Body<"/api/v1/settings/delivery", "put">, bounds: WriteBounds) {
    return this.put("/api/v1/settings/delivery", {}, input, bounds);
  }

  /** Creates a Delivery session group: its sessions' pull requests take it as their Parent agent.
   *  A session another group holds is `409 SESSION_IN_GROUP`; a name another group has, or that is
   *  the title of a session outside the group, is `409` as well, the message naming which. */
  createDeliverySessionGroup(
    input: Body<"/api/v1/delivery/session-groups", "post">,
    bounds: WriteBounds
  ) {
    return this.post("/api/v1/delivery/session-groups", {}, input, bounds);
  }

  /** Renames a session group, adds sessions to it or removes sessions from it. */
  updateDeliverySessionGroup(
    id: string,
    input: Body<"/api/v1/delivery/session-groups/{id}", "patch">,
    bounds: WriteBounds
  ) {
    return this.patch("/api/v1/delivery/session-groups/{id}", { id }, input, bounds);
  }

  /** Deletes a session group; its sessions show as themselves again. */
  async deleteDeliverySessionGroup(id: string, bounds: WriteBounds): Promise<void> {
    await this.remove("/api/v1/delivery/session-groups/{id}", { id }, bounds);
  }

  getIssueSubscribers(key: string) {
    return this.read("/api/v1/issues/{key}/subscribers", { key });
  }

  async unsubscribeIssueSession(
    key: string,
    sessionId: string,
    bounds: WriteBounds
  ): Promise<void> {
    await this.remove(
      "/api/v1/issues/{key}/subscribers/{session_id}",
      { key, session_id: sessionId },
      bounds
    );
  }

  getMyState() {
    return this.read("/api/v1/me/state", {});
  }

  putIssueState(
    key: string,
    input: Body<"/api/v1/me/issues/{key}/state", "put">,
    bounds: WriteBounds
  ) {
    return this.send(issueStateRequest(key, input), bounds);
  }

  getMyAgentState() {
    return this.read("/api/v1/me/agents/state", {});
  }

  putAgentState(
    sessionID: string,
    input: Body<"/api/v1/me/agents/{session_id}/state", "put">,
    bounds: WriteBounds
  ) {
    return this.send(agentStateRequest(sessionID, input), bounds);
  }

  /** The signed-in person. `/auth/*` sits outside the `/api/v1` route table the document is
   *  rendered from, so this one read names its path and its answer itself. */
  whoAmI(): Promise<AuthenticatedUser> {
    return this.json<AuthenticatedUser>("/auth/whoami");
  }

  /** Ends this browser's session. `/auth/logout` sits outside Dispatch's keyed layer and is the
   *  same however often it is made, so it goes with no key. */
  logout(): Promise<{ ok: true }> {
    return this.json<{ ok: true }>("/auth/logout", { method: "POST" });
  }

  /** A read through Dispatch's GitHub proxy; it makes no write. The proxy carries any GitHub path
   *  through, so the route table holds no route for what it answers. */
  githubRest(path: string, init: { readonly signal?: AbortSignal } = {}): Promise<Response> {
    return this.response(`/api/github/rest/${path.replace(/^\/+/, "")}`, init);
  }

  /** `GET /api/v1/credential-requests?approver=me`: every request the viewer may decide - the
   *  ones naming the viewer, and the ones any signed-in person may decide - as the Inbox's
   *  credential-requests section lists them, each secret request with the secrets broker's page
   *  where it is decided, or `null` when this Dispatch has no secrets broker. */
  getCredentialPending() {
    return this.read("/api/v1/credential-requests", {}, { approver: "me" });
  }

  /** Approve a machine login as the signed-in viewer with the code the viewer typed: Dispatch
   *  names the viewer's own login as the approver, and the broker decides whether that login may.
   *  A secret request is decided only on the secrets broker's own page, so the broker refuses it
   *  here with 403 `DECIDE_ON_BROKER_PAGE`. */
  approveCredentialRecord(
    recordId: string,
    body: Body<"/api/v1/credential-requests/{id}/approve", "post">,
    bounds: WriteBounds
  ) {
    return this.post("/api/v1/credential-requests/{id}/approve", { id: recordId }, body, bounds);
  }

  /** Deny a machine login as the signed-in viewer with the code the viewer typed. */
  denyCredentialRecord(
    recordId: string,
    body: Body<"/api/v1/credential-requests/{id}/deny", "post">,
    bounds: WriteBounds
  ) {
    return this.post("/api/v1/credential-requests/{id}/deny", { id: recordId }, body, bounds);
  }

  /** A machine (`launcher_credential`) record is only ever selected through this route: the code
   *  the operator types in is what proves which pending login they mean, and deciding it takes
   *  the same code again. */
  lookupMachineCredential(code: string, bounds: WriteBounds) {
    return this.post("/api/v1/credential-requests/machine-lookup", {}, { code }, bounds);
  }

  /** `GET /api/v1/credential-grants?approver=me`: every live grant of the viewer's sessions,
   *  automatic or approved, and every grant the viewer approved. */
  getCredentialGrants() {
    return this.read("/api/v1/credential-grants", {}, { approver: "me" });
  }

  /** Revoke a grant as the signed-in viewer, its approver or its enrollment's operator. The
   *  operator's revoke of an automatic grant also ends that session's other automatic grants of
   *  those secrets and makes it ask before it gets them again. */
  async revokeCredentialGrant(grantId: string, bounds: WriteBounds): Promise<void> {
    await this.post("/api/v1/credential-grants/{id}/revoke", { id: grantId }, undefined, bounds);
  }

  /** `GET /api/v1/machine-logins`: the machine logins the viewer may revoke, their own machines'
   *  and every service's, whoever approved it, that are unexpired or expired with a session still
   *  running. */
  getMachineLogins() {
    return this.read("/api/v1/machine-logins", {});
  }

  /** Revoke a machine login, expired or not: a service's, or one of the viewer's machines'.
   *  Dispatch names the viewer as the person revoking, and every session the login enrolled ends
   *  with it. */
  async revokeMachineLogin(credentialId: string, bounds: WriteBounds): Promise<void> {
    await this.post("/api/v1/machine-logins/{id}/revoke", { id: credentialId }, undefined, bounds);
  }

  /** `GET /api/v1/signins`: the viewer's own sign-ins on their machines (`agent-secrets login`),
   *  one per box key they have not revoked. Dispatch names the viewer, so nobody reads another
   *  person's. */
  getSignIns() {
    return this.read("/api/v1/signins", {});
  }

  /** End one of the viewer's own sign-ins: that machine trades no token for them again and the
   *  access tokens already minted under it end with it, while their sign-ins on other machines
   *  keep working. */
  async revokeSignIn(signInId: string, bounds: WriteBounds): Promise<void> {
    await this.post("/api/v1/signins/{id}/revoke", { id: signInId }, undefined, bounds);
  }

  /** `GET /api/v1/credentials/capabilities`: what the credential pages may offer against this
   *  deployment's secrets broker, which Dispatch relays from the broker's own RFC 8414 metadata
   *  because a browser reaches no broker. */
  getCredentialCapabilities() {
    return this.read("/api/v1/credentials/capabilities", {});
  }
}

/** A client over `fetchImpl`. The page's own (`api`) sends its uploads through `XMLHttpRequest`
 *  (`xhrUpload`), so their progress counts against the silence deadline; one given a `fetchImpl`
 *  of its own sends them through it unless it is given an `upload` too. */
export function createApiClient(
  fetchImpl?: FetchImplementation,
  upload?: UploadImplementation
): DispatchApiClient {
  return fetchImpl === undefined
    ? new DispatchApiClient((...args) => fetch(...args), upload ?? xhrUpload())
    : new DispatchApiClient(fetchImpl, upload);
}

export const api = createApiClient();
