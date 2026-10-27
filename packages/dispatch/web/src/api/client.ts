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

import { reachable, routeOf, unreachable } from "./reachability";

import type {
  Agent,
  AgentToken,
  AnswerAskInput,
  ArchitectureSource,
  ArchitectureTree,
  Artifact,
  ArtifactBlock,
  ArtifactRebuildReport,
  ArtifactReview,
  ArtifactReviewState,
  ArtifactText,
  ArtifactUploadResponse,
  ArtifactVersionContent,
  ArtifactVersionText,
  Ask,
  AskRead,
  AskSnooze,
  AuthenticatedUser,
  BlockSchema,
  BroadcastCreated,
  BroadcastRead,
  BroadcastSummary,
  Comment,
  CommentDelivery,
  CommentRead,
  CreateAgentMessageInput,
  CreateArtifactInput,
  CreateAskInput,
  CreateBroadcastInput,
  CreateCommentInput,
  CreateDeliverySessionGroupInput,
  CreatedAgentToken,
  CreateIssueInput,
  CreateMessageInput,
  CreateProjectInput,
  CreateVersionInput,
  CredentialDecisionResponse,
  CredentialGrantsResponse,
  CredentialPendingResponse,
  CredentialRecord,
  DeliveryRunDetail,
  DeliverySessionGroup,
  DeliverySettings,
  DeliverySettingsInput,
  DispatchUser,
  EditCommentInput,
  Event,
  GraphReferences,
  InboxRow,
  Issue,
  IssueAskRow,
  IssueAskThreadRow,
  IssueDetails,
  IssueReferences,
  IssueSummary,
  ListUsersResponse,
  MachineLoginsResponse,
  Message,
  MessageDelivery,
  MessageRead,
  MyAnswersResponse,
  MyRepliesResponse,
  Project,
  ProjectDefaultAssignee,
  ProjectDefaultAssigneeInput,
  RepoProject,
  SearchResponse,
  Subscriber,
  UpdateDeliverySessionGroupInput,
  UpdateIssueInput,
  UserAgentState,
  UserAgentStateInput,
  UserAgentStates,
  UserIssueState,
  UserState,
  Version,
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

interface ErrorPayload {
  code?: string;
  error?: string;
  state?: UserIssueState;
}

export class ApiError extends Error {
  readonly code: string | undefined;
  readonly status: number;
  readonly state: UserIssueState | undefined;
  /** How long Dispatch asked the client to wait before it asks again (`Retry-After`), if it said. */
  readonly retryAfterMs: number | undefined;

  constructor(status: number, payload: ErrorPayload, retryAfterMs?: number) {
    super(payload.error ?? `Dispatch request failed (${status})`);
    this.name = "ApiError";
    this.code = payload.code;
    this.status = status;
    this.state = payload.state;
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

// A document whose stored history cannot load at all: the state a rebuild from its latest saved
// version repairs, and retrying the read cannot.
export function isDocumentUnloadable(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409 && error.code === "DOCUMENT_UNLOADABLE";
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

export interface ListIssuesOptions {
  pinned?: boolean;
  project?: string;
  open?: boolean;
  status?: string;
  parent?: string;
  updated_since?: string;
  labels?: readonly string[];
}

export interface ListEventsOptions {
  after?: number;
  before?: number;
  ids?: string[];
  limit?: number;
  order?: "desc";
}

/** Who an uploaded file belongs to: an issue, a project (its documents), or an agent's
 *  conversation, named by the agent's session id (direct messages on the Agents page). */
export type ArtifactOwner = { issue: string } | { project: string } | { session: string };

export interface CreateArtifactReviewInput {
  state: ArtifactReviewState;
  reason?: string;
}

function pathSegment(value: string): string {
  return encodeURIComponent(value);
}

// The requests of the writes that wait their turn on a line (`WriteSpec.request`), each the exact
// request its client method sends, answered as that method is: a surface names it at the press,
// and the write is made from it alone (`api.send`), so a write waiting behind another is kept as
// the very request that goes out, and nothing of it runs before its turn.

/** `PATCH /api/v1/issues/{key}`: one change of an issue's fields (`patchIssue`). */
export function issuePatchRequest(key: string, input: UpdateIssueInput): WriteRequest<Issue> {
  return { body: input, method: "PATCH", path: `/api/v1/issues/${pathSegment(key)}` };
}

/** `POST /api/v1/agents/{session}/messages`: a person's message to one session
 *  (`createAgentMessage`). */
export function agentMessageRequest(
  sessionID: string,
  input: CreateAgentMessageInput
): WriteRequest<Message> {
  return { body: input, method: "POST", path: `/api/v1/agents/${pathSegment(sessionID)}/messages` };
}

/** `PUT /api/v1/me/asks/{id}/snooze` (`snoozeAsk`). */
export function askSnoozeRequest(id: string, snoozedUntil: string): WriteRequest<AskSnooze> {
  return {
    body: { snoozed_until: snoozedUntil },
    method: "PUT",
    path: `/api/v1/me/asks/${pathSegment(id)}/snooze`,
  };
}

/** `PUT /api/v1/me/threads/{id}/read`: a thread's read mark (`markThreadRead`). */
export function threadReadRequest(threadId: string, readThrough: string): WriteRequest {
  return {
    body: { read_through: readThrough },
    method: "PUT",
    path: `/api/v1/me/threads/${pathSegment(threadId)}/read`,
  };
}

/** `PUT /api/v1/me/issues/{key}/state`: the person's state of an issue (`putIssueState`). */
export function issueStateRequest(
  key: string,
  input: Partial<UserIssueState>
): WriteRequest<UserIssueState> {
  return { body: input, method: "PUT", path: `/api/v1/me/issues/${pathSegment(key)}/state` };
}

/** `PUT /api/v1/me/agents/{session}/state`: the person's state of a session (`putAgentState`). */
export function agentStateRequest(
  sessionID: string,
  input: UserAgentStateInput
): WriteRequest<UserAgentState> {
  return { body: input, method: "PUT", path: `/api/v1/me/agents/${pathSegment(sessionID)}/state` };
}

/** The owner's artifact list, the route an upload posts to. */
function artifactsPath(owner: ArtifactOwner): string {
  if ("issue" in owner) return `/api/v1/issues/${pathSegment(owner.issue)}/artifacts`;
  if ("project" in owner) return `/api/v1/projects/${pathSegment(owner.project)}/artifacts`;
  return `/api/v1/agents/${pathSegment(owner.session)}/artifacts`;
}

/** The same-origin route of one stored version's bytes, addressed by the owner and the artifact's
 *  slug as a picture's `dispatch://` reference names it, so an `<img>` loads it with the reader's
 *  own cookie. A version never changes, so the server lets the browser keep it. */
export function artifactVersionPath(owner: ArtifactOwner, slug: string, version: number): string {
  return `${artifactsPath(owner)}/${pathSegment(slug)}/versions/${version}`;
}

function repoPath(repo: string): string {
  const [owner, name, ...rest] = repo.split("/");
  if (owner === "" || name === "" || rest.length !== 0) {
    throw new Error("repository must be formatted as owner/repo");
  }
  return `${pathSegment(owner)}/${pathSegment(name)}`;
}

function pathWithQuery(path: string, values: object): string {
  const query = new URLSearchParams();

  for (const [key, value] of Object.entries(values)) {
    if (Array.isArray(value)) {
      for (const item of value) {
        query.append(key, item);
      }
    } else if (
      typeof value === "string" ||
      typeof value === "number" ||
      typeof value === "boolean"
    ) {
      query.set(key, String(value));
    }
  }

  const search = query.toString();
  return search ? `${path}?${search}` : path;
}

/** The wait `Retry-After` names, in milliseconds: its delay-seconds form, which is the one
 *  Dispatch sends. */
function retryAfterMs(response: Response): number | undefined {
  const seconds = Number(response.headers.get("Retry-After") ?? Number.NaN);
  return Number.isFinite(seconds) && seconds >= 0 ? seconds * 1000 : undefined;
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

  private post<T>(path: string, body: unknown, bounds: WriteBounds): Promise<T> {
    return this.send<T>({ body, method: "POST", path }, bounds);
  }

  private put<T>(path: string, body: unknown, bounds: WriteBounds): Promise<T> {
    return this.send<T>({ body, method: "PUT", path }, bounds);
  }

  private remove<T>(path: string, bounds: WriteBounds): Promise<T> {
    return this.send<T>({ method: "DELETE", path }, bounds);
  }

  getBlockSchema(): Promise<BlockSchema> {
    return this.json<BlockSchema>("/api/v1/schema/blocks");
  }

  listProjects(): Promise<Project[]> {
    return this.json<Project[]>("/api/v1/projects");
  }

  createProject(input: CreateProjectInput, bounds: WriteBounds): Promise<Project> {
    return this.post<Project>("/api/v1/projects", input, bounds);
  }

  listRepoProjects(): Promise<RepoProject[]> {
    return this.json<RepoProject[]>("/api/v1/settings/repo-projects");
  }

  putRepoProject(
    repo: string,
    input: { project: string; default?: boolean },
    bounds: WriteBounds
  ): Promise<RepoProject> {
    return this.put<RepoProject>(`/api/v1/settings/repo-projects/${repoPath(repo)}`, input, bounds);
  }

  async deleteRepoProject(repo: string, bounds: WriteBounds): Promise<void> {
    await this.remove(`/api/v1/settings/repo-projects/${repoPath(repo)}`, bounds);
  }

  /** Only the projects that name a default; a project left out names nobody. Human-only. */
  listProjectDefaultAssignees(): Promise<ProjectDefaultAssignee[]> {
    return this.json<ProjectDefaultAssignee[]>("/api/v1/settings/project-default-assignees");
  }

  /** Sets the person a project's unowned work lands on, or clears it with `{assignee: null}`.
   *  Human-only, and a login nobody has signed in as is refused `400 ASSIGNEE_NOT_ALLOWED`. */
  putProjectDefaultAssignee(
    project: string,
    input: ProjectDefaultAssigneeInput,
    bounds: WriteBounds
  ): Promise<ProjectDefaultAssignee> {
    return this.put<ProjectDefaultAssignee>(
      `/api/v1/projects/${pathSegment(project)}/default-assignee`,
      input,
      bounds
    );
  }

  /** Every project's architecture sources, by project and then in the order each was added. */
  listArchitectureSources(): Promise<ArchitectureSource[]> {
    return this.json<ArchitectureSource[]>("/api/v1/settings/architecture-sources");
  }

  /** The project's sources, one per repository, in the order they were added; `[]` when it has
   *  none: having none is the common answer, not a failure. */
  listProjectArchitectureSources(project: string): Promise<ArchitectureSource[]> {
    return this.json<ArchitectureSource[]>(
      `/api/v1/projects/${pathSegment(project)}/architecture-sources`
    );
  }

  /** Adds `repo` (`owner/name`) as a source of the project, or changes the branch of the source
   *  it already is. */
  putArchitectureSource(
    project: string,
    repo: string,
    input: { branch: string },
    bounds: WriteBounds
  ): Promise<ArchitectureSource> {
    return this.put<ArchitectureSource>(
      `/api/v1/projects/${pathSegment(project)}/architecture-sources/${repoPath(repo)}`,
      input,
      bounds
    );
  }

  /** Imports one source's model now; the row's `last_error` says whether the import failed. */
  syncArchitectureSource(
    project: string,
    repo: string,
    bounds: WriteBounds
  ): Promise<ArchitectureSource> {
    return this.send<ArchitectureSource>(
      {
        method: "POST",
        path: `/api/v1/projects/${pathSegment(project)}/architecture-sources/${repoPath(repo)}/sync`,
      },
      bounds
    );
  }

  async deleteArchitectureSource(
    project: string,
    repo: string,
    bounds: WriteBounds
  ): Promise<void> {
    await this.remove(
      `/api/v1/projects/${pathSegment(project)}/architecture-sources/${repoPath(repo)}`,
      bounds
    );
  }

  /** The project's component tree with the work attached; 404 SOURCE_NOT_FOUND without a source. */
  getArchitecture(project: string): Promise<ArchitectureTree> {
    return this.json<ArchitectureTree>(`/api/v1/projects/${pathSegment(project)}/architecture`);
  }

  listAgentTokens(): Promise<AgentToken[]> {
    return this.json<AgentToken[]>("/api/v1/me/agent-tokens");
  }

  createAgentToken(input: { name: string }, bounds: WriteBounds): Promise<CreatedAgentToken> {
    return this.post<CreatedAgentToken>("/api/v1/me/agent-tokens", input, bounds);
  }

  async revokeAgentToken(id: string, bounds: WriteBounds): Promise<void> {
    await this.remove(`/api/v1/me/agent-tokens/${pathSegment(id)}`, bounds);
  }

  /** Everyone who has signed in, sorted by email: the assignee picker's options. */
  async listUsers(): Promise<DispatchUser[]> {
    return (await this.json<ListUsersResponse>("/api/v1/users")).users;
  }

  listIssues(options: ListIssuesOptions = {}): Promise<IssueSummary[]> {
    const { labels, ...query } = options;
    return this.json<IssueSummary[]>(pathWithQuery("/api/v1/issues", { ...query, label: labels }));
  }
  search(
    query: string,
    options: { project?: string; limit?: number } = {}
  ): Promise<SearchResponse> {
    return this.json<SearchResponse>(pathWithQuery("/api/v1/search", { q: query, ...options }));
  }

  createIssue(input: CreateIssueInput, bounds: WriteBounds): Promise<Issue> {
    return this.post<Issue>("/api/v1/issues", input, bounds);
  }

  getIssue(key: string): Promise<IssueDetails> {
    return this.json<IssueDetails>(`/api/v1/issues/${pathSegment(key)}`);
  }

  patchIssue(key: string, input: UpdateIssueInput, bounds: WriteBounds): Promise<Issue> {
    return this.send<Issue>(issuePatchRequest(key, input), bounds);
  }

  /** `DELETE /api/v1/issues/{key}/claim`: release the claim. A human may release any claim;
   *  the issue's status does not move. */
  releaseIssueClaim(key: string, bounds: WriteBounds): Promise<Issue> {
    return this.remove<Issue>(`/api/v1/issues/${pathSegment(key)}/claim`, bounds);
  }

  getIssueEvents(key: string, options: ListEventsOptions = {}): Promise<Event[]> {
    const { ids, ...query } = options;
    return this.json<Event[]>(
      pathWithQuery(`/api/v1/issues/${pathSegment(key)}/events`, { ...query, ids: ids?.join(",") })
    );
  }

  getInbox(project?: string): Promise<InboxRow[]> {
    return this.json<InboxRow[]>(pathWithQuery("/api/v1/inbox", { project }));
  }

  /** `PUT /api/v1/me/asks/{id}/snooze`: fold this inbox row away for the caller until
   *  `snoozedUntil` (RFC3339). Human-only, like the inbox itself. */
  snoozeAsk(id: string, snoozedUntil: string, bounds: WriteBounds): Promise<AskSnooze> {
    return this.send<AskSnooze>(askSnoozeRequest(id, snoozedUntil), bounds);
  }

  /** `DELETE /api/v1/me/asks/{id}/snooze`: bring the row back now. A row that is not snoozed
   *  answers the same 204. */
  async unsnoozeAsk(id: string, bounds: WriteBounds): Promise<void> {
    await this.remove(`/api/v1/me/asks/${pathSegment(id)}/snooze`, bounds);
  }

  listAgents(): Promise<Agent[]> {
    return this.json<Agent[]>("/api/v1/agents");
  }

  createAsk(key: string, input: CreateAskInput, bounds: WriteBounds): Promise<Ask> {
    return this.post<Ask>(`/api/v1/issues/${pathSegment(key)}/asks`, input, bounds);
  }

  /** `GET /api/v1/issues/{key}/asks`: the issue's asks with their newest reply and no thread,
   *  for a reader that renders the ask itself. */
  listIssueAsks(key: string, state: "all" | "open" | "answered" = "all"): Promise<IssueAskRow[]> {
    return this.json<IssueAskRow[]>(
      pathWithQuery(`/api/v1/issues/${pathSegment(key)}/asks`, { state })
    );
  }

  /** `GET /api/v1/issues/{key}/asks?thread=1`: the same rows carrying each ask's thread. The
   *  list is unpaged, so only a reader that draws the threads asks for them. */
  listIssueAskThreads(
    key: string,
    state: "all" | "open" | "answered" = "all"
  ): Promise<IssueAskThreadRow[]> {
    return this.json<IssueAskThreadRow[]>(
      pathWithQuery(`/api/v1/issues/${pathSegment(key)}/asks`, { state, thread: 1 })
    );
  }

  answerAsk(id: string, input: AnswerAskInput, bounds: WriteBounds): Promise<Ask> {
    return this.post<Ask>(`/api/v1/asks/${pathSegment(id)}/answer`, input, bounds);
  }

  getAsk(id: string): Promise<AskRead> {
    return this.json<AskRead>(`/api/v1/asks/${pathSegment(id)}`);
  }

  /** `GET /api/v1/me/answers`: the signed-in person's answers and ask replies, newest first,
   *  one page at a time. Human-only. */
  listMyAnswers(page: { limit: number; offset: number }): Promise<MyAnswersResponse> {
    return this.json<MyAnswersResponse>(pathWithQuery("/api/v1/me/answers", page));
  }

  /** `GET /api/v1/me/replies`: the threads the signed-in person wrote in where someone else has
   *  written since they last read it, newest reply first; `issue` or `artifact` narrows to one
   *  owner. Human-only. */
  listMyReplies(page: {
    artifact?: string;
    issue?: string;
    limit: number;
    offset?: number;
  }): Promise<MyRepliesResponse> {
    return this.json<MyRepliesResponse>(pathWithQuery("/api/v1/me/replies", page));
  }

  /** `PUT /api/v1/me/threads/{id}/read`: move the signed-in person's read mark on one thread
   *  forward to `readThrough` (RFC3339, the newest reply shown). Answers 204 whether or not it
   *  moved. */
  async markThreadRead(threadId: string, readThrough: string, bounds: WriteBounds): Promise<void> {
    await this.send(threadReadRequest(threadId, readThrough), bounds);
  }

  async removeAskFollower(id: string, sessionId: string, bounds: WriteBounds): Promise<void> {
    await this.remove(
      `/api/v1/asks/${pathSegment(id)}/followers/${pathSegment(sessionId)}`,
      bounds
    );
  }

  listComments(key: string, artifact?: string): Promise<Comment[]> {
    return this.json<Comment[]>(
      pathWithQuery(`/api/v1/issues/${pathSegment(key)}/comments`, { artifact })
    );
  }

  createComment(key: string, input: CreateCommentInput, bounds: WriteBounds): Promise<Comment> {
    return this.post<Comment>(`/api/v1/issues/${pathSegment(key)}/comments`, input, bounds);
  }

  getComment(id: string): Promise<CommentRead> {
    return this.json<CommentRead>(`/api/v1/comments/${pathSegment(id)}`);
  }

  resolveComment(id: string, bounds: WriteBounds): Promise<Comment> {
    return this.post<Comment>(`/api/v1/comments/${pathSegment(id)}/resolve`, {}, bounds);
  }

  reopenComment(id: string, bounds: WriteBounds): Promise<Comment> {
    return this.post<Comment>(`/api/v1/comments/${pathSegment(id)}/reopen`, {}, bounds);
  }

  editComment(id: string, input: EditCommentInput, bounds: WriteBounds): Promise<Comment> {
    return this.send<Comment>(
      { body: input, method: "PATCH", path: `/api/v1/comments/${pathSegment(id)}` },
      bounds
    );
  }

  acceptComment(id: string, bounds: WriteBounds): Promise<Comment> {
    return this.post<Comment>(`/api/v1/comments/${pathSegment(id)}/accept`, {}, bounds);
  }

  rejectComment(id: string, bounds: WriteBounds): Promise<Comment> {
    return this.post<Comment>(`/api/v1/comments/${pathSegment(id)}/reject`, {}, bounds);
  }

  createCommentDelivery(
    id: string,
    target: string | undefined,
    delivery: "btw" | "aside" | "steer",
    bounds: WriteBounds
  ): Promise<CommentDelivery> {
    return this.post<CommentDelivery>(
      `/api/v1/comments/${pathSegment(id)}/deliveries`,
      { delivery, ...(target === undefined ? {} : { target }) },
      bounds
    );
  }

  createMessage(key: string, input: CreateMessageInput, bounds: WriteBounds): Promise<Message> {
    return this.post<Message>(`/api/v1/issues/${pathSegment(key)}/messages`, input, bounds);
  }

  /** A person's message to one session. */
  createAgentMessage(
    sessionID: string,
    input: CreateAgentMessageInput,
    bounds: WriteBounds
  ): Promise<Message> {
    return this.send<Message>(agentMessageRequest(sessionID, input), bounds);
  }

  listAgentMessages(sessionID: string): Promise<MessageRead[]> {
    return this.json<MessageRead[]>(`/api/v1/agents/${pathSegment(sessionID)}/messages`);
  }

  /** A person's message to many sessions. */
  createBroadcast(input: CreateBroadcastInput, bounds: WriteBounds): Promise<BroadcastCreated> {
    return this.post<BroadcastCreated>("/api/v1/broadcasts", input, bounds);
  }

  listBroadcasts(): Promise<BroadcastSummary[]> {
    return this.json<BroadcastSummary[]>("/api/v1/broadcasts");
  }

  getBroadcast(id: string): Promise<BroadcastRead> {
    return this.json<BroadcastRead>(`/api/v1/broadcasts/${pathSegment(id)}`);
  }

  createMessageDelivery(
    id: string,
    delivery: "btw" | "aside" | "steer",
    bounds: WriteBounds
  ): Promise<MessageDelivery> {
    return this.post<MessageDelivery>(
      `/api/v1/messages/${pathSegment(id)}/deliveries`,
      { delivery },
      bounds
    );
  }

  getMessage(key: string, id: string): Promise<MessageRead> {
    return this.json<MessageRead>(`/api/v1/issues/${pathSegment(key)}/messages/${pathSegment(id)}`);
  }

  /** `GET /api/v1/issues/{key}/messages`: every root message of the issue, oldest first, each
   *  with its deliveries and its replies. */
  listIssueMessages(key: string): Promise<MessageRead[]> {
    return this.json<MessageRead[]>(`/api/v1/issues/${pathSegment(key)}/messages`);
  }

  listArtifacts(key: string): Promise<Artifact[]> {
    return this.json<Artifact[]>(`/api/v1/issues/${pathSegment(key)}/artifacts`);
  }

  listProjectArtifacts(key: string, unlinked?: boolean): Promise<Artifact[]> {
    return this.json<Artifact[]>(
      pathWithQuery(`/api/v1/projects/${pathSegment(key)}/artifacts`, { unlinked })
    );
  }

  uploadArtifact(
    owner: ArtifactOwner,
    input: CreateArtifactInput,
    bounds: WriteBounds
  ): Promise<ArtifactUploadResponse> {
    const path = artifactsPath(owner);
    if ("content" in input) {
      return this.post<ArtifactUploadResponse>(
        path,
        { actor: input.actor, content: input.content, name: input.name, summary: input.summary },
        bounds
      );
    }
    const body = new FormData();
    body.set("name", input.name);
    if (input.summary !== undefined) {
      body.set("summary", input.summary);
    }
    if (input.actor !== undefined) {
      body.set("actor", JSON.stringify(input.actor));
    }
    body.set("file", input.file);

    return this.post<ArtifactUploadResponse>(path, body, bounds);
  }

  getArtifact(id: string): Promise<Artifact> {
    return this.json<Artifact>(`/api/v1/artifacts/${pathSegment(id)}`);
  }

  getProjectArtifact(key: string, slug: string): Promise<Artifact> {
    return this.json<Artifact>(
      `/api/v1/projects/${pathSegment(key)}/artifacts/${pathSegment(slug)}`
    );
  }

  /** An artifact of an agent's conversation, with its versions. */
  getAgentArtifact(sessionId: string, slug: string): Promise<Artifact> {
    return this.json<Artifact>(
      `/api/v1/agents/${pathSegment(sessionId)}/artifacts/${pathSegment(slug)}`
    );
  }

  getArtifactText(id: string): Promise<ArtifactText> {
    return this.json<ArtifactText>(`/api/v1/artifacts/${pathSegment(id)}/text`);
  }

  rebuildArtifact(
    id: string,
    markdown: string | undefined,
    bounds: WriteBounds
  ): Promise<ArtifactRebuildReport> {
    return this.post<ArtifactRebuildReport>(
      `/api/v1/artifacts/${pathSegment(id)}/rebuild`,
      markdown === undefined ? {} : { markdown },
      bounds
    );
  }

  getArtifactBlocks(id: string): Promise<ArtifactBlock[]> {
    return this.json<ArtifactBlock[]>(`/api/v1/artifacts/${pathSegment(id)}/blocks`);
  }

  /** One version's bytes. A large file on a slow line can take longer than any read deadline, so
   *  this GET carries none. */
  async getArtifactVersion(id: string, version: number): Promise<ArtifactVersionContent> {
    const response = await this.response(
      `/api/v1/artifacts/${pathSegment(id)}/versions/${pathSegment(String(version))}`,
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
    input: CreateVersionInput,
    bounds: WriteBounds
  ): Promise<Version> {
    return this.post<Version>(`/api/v1/artifacts/${pathSegment(id)}/versions`, input, bounds);
  }

  listArtifactReviews(id: string): Promise<ArtifactReview[]> {
    return this.json<ArtifactReview[]>(`/api/v1/artifacts/${pathSegment(id)}/reviews`);
  }

  createArtifactReview(
    id: string,
    input: CreateArtifactReviewInput,
    bounds: WriteBounds
  ): Promise<ArtifactReview> {
    return this.post<ArtifactReview>(`/api/v1/artifacts/${pathSegment(id)}/reviews`, input, bounds);
  }

  listArtifactAsks(id: string, state: "all" | "open" | "answered" = "all"): Promise<Ask[]> {
    return this.json<Ask[]>(pathWithQuery(`/api/v1/artifacts/${pathSegment(id)}/asks`, { state }));
  }

  createArtifactAsk(id: string, input: CreateAskInput, bounds: WriteBounds): Promise<Ask> {
    return this.post<Ask>(`/api/v1/artifacts/${pathSegment(id)}/asks`, input, bounds);
  }

  listArtifactComments(id: string): Promise<Comment[]> {
    return this.json<Comment[]>(`/api/v1/artifacts/${pathSegment(id)}/comments`);
  }

  createArtifactComment(
    id: string,
    input: CreateCommentInput,
    bounds: WriteBounds
  ): Promise<Comment> {
    return this.post<Comment>(`/api/v1/artifacts/${pathSegment(id)}/comments`, input, bounds);
  }

  getArtifactSubscribers(id: string): Promise<Subscriber[]> {
    return this.json<Subscriber[]>(`/api/v1/artifacts/${pathSegment(id)}/subscribers`);
  }

  async unsubscribeArtifactSession(
    id: string,
    sessionId: string,
    bounds: WriteBounds
  ): Promise<void> {
    await this.remove(
      `/api/v1/artifacts/${pathSegment(id)}/subscribers/${pathSegment(sessionId)}`,
      bounds
    );
  }

  getIssueReferences(key: string): Promise<IssueReferences> {
    return this.json<IssueReferences>(`/api/v1/issues/${pathSegment(key)}/references`);
  }
  getReferences(reference: string): Promise<GraphReferences> {
    return this.json<GraphReferences>(pathWithQuery("/api/v1/references", { to: reference }));
  }

  /** One deploy-repository run with every job it ran, for the drill-down's job lists. */
  getDeliveryRun(id: number): Promise<DeliveryRunDetail> {
    return this.json<DeliveryRunDetail>(`/api/v1/delivery/runs/${id}`);
  }

  /** The delivery timeline's configuration record, or `null` until someone sets it. */
  getDeliverySettings(): Promise<DeliverySettings | null> {
    return this.json<DeliverySettings | null>("/api/v1/settings/delivery");
  }

  /** Replaces the delivery timeline's configuration (human callers only). The server first proves
   *  the GitHub App can read `deploy_repo`, answering `409 DELIVERY_SETTINGS_ACCESS` when not. */
  putDeliverySettings(
    input: DeliverySettingsInput,
    bounds: WriteBounds
  ): Promise<DeliverySettings> {
    return this.put<DeliverySettings>("/api/v1/settings/delivery", input, bounds);
  }

  /** Creates a Delivery session group: its sessions' pull requests take it as their Parent agent.
   *  A session another group holds is `409 SESSION_IN_GROUP`; a name another group has, or that is
   *  the title of a session outside the group, is `409` as well, the message naming which. */
  createDeliverySessionGroup(
    input: CreateDeliverySessionGroupInput,
    bounds: WriteBounds
  ): Promise<DeliverySessionGroup> {
    return this.post<DeliverySessionGroup>("/api/v1/delivery/session-groups", input, bounds);
  }

  /** Renames a session group, adds sessions to it or removes sessions from it. */
  updateDeliverySessionGroup(
    id: string,
    input: UpdateDeliverySessionGroupInput,
    bounds: WriteBounds
  ): Promise<DeliverySessionGroup> {
    return this.send<DeliverySessionGroup>(
      {
        body: input,
        method: "PATCH",
        path: `/api/v1/delivery/session-groups/${pathSegment(id)}`,
      },
      bounds
    );
  }

  /** Deletes a session group; its sessions show as themselves again. */
  async deleteDeliverySessionGroup(id: string, bounds: WriteBounds): Promise<void> {
    await this.remove(`/api/v1/delivery/session-groups/${pathSegment(id)}`, bounds);
  }

  getIssueSubscribers(key: string): Promise<Subscriber[]> {
    return this.json<Subscriber[]>(`/api/v1/issues/${pathSegment(key)}/subscribers`);
  }

  async unsubscribeIssueSession(
    key: string,
    sessionId: string,
    bounds: WriteBounds
  ): Promise<void> {
    await this.remove(
      `/api/v1/issues/${pathSegment(key)}/subscribers/${pathSegment(sessionId)}`,
      bounds
    );
  }

  getMyState(): Promise<UserState> {
    return this.json<UserState>("/api/v1/me/state");
  }

  putIssueState(
    key: string,
    input: Partial<UserIssueState>,
    bounds: WriteBounds
  ): Promise<UserIssueState> {
    return this.send<UserIssueState>(issueStateRequest(key, input), bounds);
  }

  getMyAgentState(): Promise<UserAgentStates> {
    return this.json<UserAgentStates>("/api/v1/me/agents/state");
  }

  putAgentState(
    sessionID: string,
    input: UserAgentStateInput,
    bounds: WriteBounds
  ): Promise<UserAgentState> {
    return this.send<UserAgentState>(agentStateRequest(sessionID, input), bounds);
  }

  whoAmI(): Promise<AuthenticatedUser> {
    return this.json<AuthenticatedUser>("/auth/whoami");
  }

  /** Ends this browser's session. `/auth/logout` sits outside Dispatch's keyed layer and is the
   *  same however often it is made, so it goes with no key. */
  logout(): Promise<{ ok: true }> {
    return this.json<{ ok: true }>("/auth/logout", { method: "POST" });
  }

  /** A read through Dispatch's GitHub proxy; it makes no write. */
  githubRest(path: string, init: { readonly signal?: AbortSignal } = {}): Promise<Response> {
    return this.response(`/api/github/rest/${path.replace(/^\/+/, "")}`, init);
  }

  /** `GET /api/v1/credential-requests?approver=me`: every request the viewer may decide - the
   *  ones naming the viewer, and the ones any signed-in person may decide - as the Inbox's
   *  credential-requests section lists them, or `null` when this Dispatch has no secrets broker. */
  getCredentialPending(): Promise<CredentialPendingResponse | null> {
    return this.json<CredentialPendingResponse | null>(
      pathWithQuery("/api/v1/credential-requests", { approver: "me" })
    );
  }

  getCredentialRecord(recordId: string): Promise<CredentialRecord> {
    return this.json<CredentialRecord>(`/api/v1/credential-requests/${pathSegment(recordId)}`);
  }

  /** Approve a credential request as the signed-in viewer: Dispatch names the viewer's own login
   *  as the approver, and the broker decides whether that login may. A machine login also carries
   *  the code the viewer typed. */
  approveCredentialRecord(
    recordId: string,
    body: { code?: string },
    bounds: WriteBounds
  ): Promise<CredentialDecisionResponse> {
    return this.post<CredentialDecisionResponse>(
      `/api/v1/credential-requests/${pathSegment(recordId)}/approve`,
      body,
      bounds
    );
  }

  /** Deny a credential request as the signed-in viewer; a machine login also carries its code. */
  denyCredentialRecord(
    recordId: string,
    body: { code?: string },
    bounds: WriteBounds
  ): Promise<CredentialDecisionResponse> {
    return this.post<CredentialDecisionResponse>(
      `/api/v1/credential-requests/${pathSegment(recordId)}/deny`,
      body,
      bounds
    );
  }

  /** A machine (`launcher_credential`) record is only ever selected through this route: the code
   *  the operator types in is what proves which pending login they mean, and deciding it takes
   *  the same code again. */
  lookupMachineCredential(code: string, bounds: WriteBounds): Promise<CredentialRecord> {
    return this.post<CredentialRecord>(
      "/api/v1/credential-requests/machine-lookup",
      { code },
      bounds
    );
  }

  /** `GET /api/v1/credential-grants?approver=me`: every live grant of the viewer's sessions,
   *  automatic or approved, and every grant the viewer approved. */
  getCredentialGrants(): Promise<CredentialGrantsResponse> {
    return this.json<CredentialGrantsResponse>(
      pathWithQuery("/api/v1/credential-grants", { approver: "me" })
    );
  }

  /** Revoke a grant as the signed-in viewer, its approver or its enrollment's operator. The
   *  operator's revoke of an automatic grant also ends that session's other automatic grants of
   *  those secrets and makes it ask before it gets them again. */
  async revokeCredentialGrant(grantId: string, bounds: WriteBounds): Promise<void> {
    await this.post(`/api/v1/credential-grants/${pathSegment(grantId)}/revoke`, {}, bounds);
  }

  /** `GET /api/v1/machine-logins`: the machine logins the viewer may revoke, their own machines'
   *  and every service's, whoever approved it, that are unexpired or expired with a session still
   *  running. */
  getMachineLogins(): Promise<MachineLoginsResponse> {
    return this.json<MachineLoginsResponse>("/api/v1/machine-logins");
  }

  /** Revoke a machine login, expired or not: a service's, or one of the viewer's machines'.
   *  Dispatch names the viewer as the person revoking, and every session the login enrolled ends
   *  with it. */
  async revokeMachineLogin(credentialId: string, bounds: WriteBounds): Promise<void> {
    await this.post(`/api/v1/machine-logins/${pathSegment(credentialId)}/revoke`, {}, bounds);
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
