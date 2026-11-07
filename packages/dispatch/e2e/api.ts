import type { DispatchEvent, paths } from "@legion/contracts";
import type { Cookie } from "@playwright/test";
import {
  type Body,
  type Op,
  type PathParams,
  type PathsOf,
  type Query,
  type QueryParams,
  type RequestBody,
  type Result,
  repoParams,
  routeUrl,
} from "../web/src/api/paths";
import type { Actor } from "../web/src/api/types";
import { harnessPorts } from "./harness-ports";
import { type DispatchProcess, pinnedTo } from "./harness-processes";

/** The Dispatch server a process of `env`'s lane addresses: the deployed server `env` names, else
 *  the harness server of that lane, whose port e2e/run.ts exports from the server's report before
 *  anything imports this module (e2e/harness-ports.ts). `e2e/preflight.ts` resolves another
 *  lane's the same way this one resolves its own. */
export function baseUrlOf(env: NodeJS.ProcessEnv = process.env): string {
  return env.PLAYWRIGHT_BASE_URL || `http://127.0.0.1:${harnessPorts(env).dispatch.port}`;
}

export const baseUrl = baseUrlOf();
/** The origin the server compares every cookie-authenticated write against (enforceCookieOrigin). */
export const dashboardOrigin = new URL(baseUrl).origin;
// A deployed server has its own agent token; the local harness pins `e2e-token` in
// e2e/run-server.sh, so an E2E_AGENT_TOKEN left in the shell from a deployed run would only
// make every bearer-seeded call 401 against it.
export const e2eAgentToken = process.env.PLAYWRIGHT_BASE_URL
  ? (process.env.E2E_AGENT_TOKEN ?? "e2e-token")
  : "e2e-token";
const seedActor: Actor = { kind: "session", id: "e2e-seed" };

interface ApiOptions {
  actor?: Actor;
  as?: "agent" | "user";
  /** The Dispatch server the call goes to; this module's `baseUrl`, the lane of the process that
   *  imported it, when absent. Only e2e/preflight.ts names another: it checks every lane. */
  baseUrl?: string;
  /** The `Idempotency-Key` header the write carries, naming one intent: a repeat under it is
   *  answered what the first was answered. None when absent, which Dispatch handles unkeyed. */
  idempotencyKey?: string;
  login?: string;
  /** The bearer an `as: "agent"` call sends; the shared `e2eAgentToken` when absent. */
  token?: string;
  /** In two-process mode, the process the round-robin proxy sends the call to; absent, whichever
   *  takes its turn (e2e/round-robin-proxy.ts). */
  process?: DispatchProcess;
}

/** The name of the session cookie the server issues at sign-in, dev or GitHub. */
export const sessionCookieName = "dsession";

/** The server's dev sign-in route for `login`, relative to the dashboard origin; it answers a
 *  sign-in with a 302 that sets the session cookie. */
export function devSignInPath(login: string): string {
  return `/auth/_dev/signin?login=${encodeURIComponent(login)}`;
}

/** The whole `Set-Cookie` the server set the session cookie with, or undefined when it set
 *  none. */
function sessionSetCookie(response: Response): string | undefined {
  return response.headers
    .getSetCookie()
    .find((header) => header.startsWith(`${sessionCookieName}=`));
}

/** The `name=value` of the session cookie `response` sets, or undefined when it sets none. */
export function sessionCookieFrom(response: Response): string | undefined {
  return sessionSetCookie(response)?.split(";")[0];
}

/** The SameSite values a `Set-Cookie` carries, by their lowercased spelling, as Playwright spells
 *  them. */
const sameSiteValues: Record<string, Cookie["sameSite"]> = {
  lax: "Lax",
  none: "None",
  strict: "Strict",
};

/** The session cookie `response` sets, as `BrowserContext.addCookies` takes one, or undefined
 *  when it sets none: the attributes the server itself set, never invented ones. The session
 *  cookie is `HttpOnly`, `Path=/`, `SameSite=Strict`, `Max-Age=2592000`, and `Secure` only where
 *  the server is not under DISPATCH_INSECURE_COOKIE (auth.IssueSessionCookie). It names no
 *  `Domain`, which makes it host-only: the host of `from`, the URL that set it, is then its
 *  domain. Playwright dates a cookie in whole seconds, and -1 is one that lasts the context's
 *  life, which a header with no `Max-Age` gets. */
function sessionCookieOf(response: Response, from: URL): Cookie | undefined {
  const setCookie = sessionSetCookie(response);
  if (setCookie === undefined) {
    return undefined;
  }
  const [pair, ...attributes] = setCookie.split(";");
  const assigned = pair.indexOf("=");
  const cookie: Cookie = {
    domain: from.hostname,
    expires: -1,
    httpOnly: false,
    name: pair.slice(0, assigned),
    path: "/",
    sameSite: "Lax",
    secure: false,
    value: pair.slice(assigned + 1),
  };
  for (const attribute of attributes) {
    const equals = attribute.indexOf("=");
    const name = (equals === -1 ? attribute : attribute.slice(0, equals)).trim().toLowerCase();
    const value = equals === -1 ? "" : attribute.slice(equals + 1).trim();
    if (name === "domain") cookie.domain = value;
    else if (name === "path") cookie.path = value;
    else if (name === "max-age") cookie.expires = Math.floor(Date.now() / 1000) + Number(value);
    else if (name === "httponly") cookie.httpOnly = true;
    else if (name === "secure") cookie.secure = true;
    else if (name === "samesite") cookie.sameSite = sameSiteValues[value.toLowerCase()] ?? "Lax";
  }
  return cookie;
}

export function devSignInError(login: string, status: number, responseText: string): Error {
  return new Error(`dev sign-in as ${login} failed: ${status} ${responseText}`);
}

/** The people the reset re-creates, each with a `user_sessions` row at generation 0
 *  (`resetDatabaseOnce`, e2e/seed.ts): the only logins whose session cookie outlives a reset. */
export const resetLogins: readonly string[] = ["alice", "bob"];

/** One login's cached session cookie. */
interface CachedSession {
  cookie: Promise<Cookie>;
  /** Whether the cookie names generation 0, which is where the reset puts `user_sessions` back
   *  (e2e/seed.ts). A cookie minted after a sign-out names a later generation, and the server
   *  admits only the login's current one (identity.CookieIdentity.Login), so the reset would
   *  leave such a cookie refused rather than usable. */
  atGenerationZero: boolean;
}

const sessionCookies = new Map<string, CachedSession>();

/** The session cookie the server issues `login` at its dev sign-in route, minted once per
 *  spelling of a login and kept until something revokes it: the generation the cookie names lives
 *  in `user_sessions`, which `resetDatabase` empties and refills for `resetLogins` alone (it
 *  calls `forgetSessions`), and a browser context's Sign out as the same login advances it. A
 *  sign-in that fails, by its answer or by the request itself, is not kept, so the next call asks
 *  again. */
export function sessionCookie(login: string): Promise<Cookie> {
  const cached = sessionCookies.get(login);
  if (cached !== undefined) {
    return cached.cookie;
  }
  const mint = new URL(devSignInPath(login), baseUrl);
  const session: CachedSession = {
    atGenerationZero: false,
    cookie: fetch(mint, { redirect: "manual" }).then(async (response) => {
      const cookie = sessionCookieOf(response, mint);
      if (response.status !== 302 || cookie === undefined) {
        throw devSignInError(login, response.status, await response.text());
      }
      // The cookie's value is `2.<base64url email>.<generation>.<expiry>.<mac>`
      // (auth.sessionPayload), whose generation strconv.FormatInt writes, so "0" is the only
      // spelling of the generation the reset puts back.
      session.atGenerationZero = cookie.value.split(".")[2] === "0";
      return cookie;
    }),
  };
  sessionCookies.set(login, session);
  session.cookie.catch(() => {
    if (sessionCookies.get(login) === session) {
      sessionCookies.delete(login);
    }
  });
  return session.cookie;
}

/** The logins a browser context has signed in as since the last reset by carrying the cached
 *  cookie rather than minting one (`signIn`, e2e/users.ts). */
const reusedSignIns = new Set<string>();

/** The cookie a context signing in as `login` can carry instead of minting one, or undefined when
 *  that sign-in must mint, which is the sign-in every row paid before this cache.
 *
 *  Only a `resetLogins` login reuses: the reset re-creates no other person, so no other login's
 *  cookie outlives it. Only the first sign-in of that login since the reset reuses: a row that
 *  signs the same login in twice has almost always signed it out in between, which advances the
 *  login's generation, and the cached cookie, which names the generation before it, would then be
 *  refused. */
export function reusableSessionCookie(login: string): Promise<Cookie> | undefined {
  if (!resetLogins.includes(login) || reusedSignIns.has(login)) {
    return undefined;
  }
  reusedSignIns.add(login);
  return sessionCookie(login);
}

/** Forgets the cached session cookies the reset leaves unusable, and the sign-ins it undoes;
 *  `resetDatabase` calls it after emptying the tables.
 *
 *  The cookies of `resetLogins`, alice and bob, are kept: the reset re-inserts their people rows
 *  and their `user_sessions` rows at generation 0 (e2e/seed.ts), the generation a cookie minted
 *  after the previous reset names, so those two cookies authenticate again and the next row pays
 *  no sign-in for them. Every other login's cookie is forgotten, since the reset re-creates no
 *  other person, and so is a kept login's cookie minted at a later generation, which a sign-out
 *  in the row just ended leaves behind. A kept cookie carries no state of its row into the next
 *  one: it names a login and a generation, both of which the emptied database holds again. */
export function forgetSessions(): void {
  for (const [login, session] of sessionCookies) {
    if (!resetLogins.includes(login) || !session.atGenerationZero) {
      sessionCookies.delete(login);
    }
  }
  reusedSignIns.clear();
}

/** Headers that make a plain fetch act as `login`: the session cookie, and the dashboard origin
 *  the server requires on every cookie-authenticated write (enforceCookieOrigin admits a write
 *  whose `Origin` is the dashboard's). */
export async function userHeaders(login = "alice"): Promise<Record<string, string>> {
  const cookie = await sessionCookie(login);
  return { Cookie: `${cookie.name}=${cookie.value}`, Origin: dashboardOrigin };
}

async function request<T>(
  path: string,
  method: string,
  body?: object,
  options: ApiOptions = {}
): Promise<T> {
  const as = options.as ?? "user";
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const payload =
    body === undefined
      ? undefined
      : as === "agent"
        ? { ...body, actor: options.actor ?? seedActor }
        : body;

  if (as === "agent") {
    headers.Authorization = `Bearer ${options.token ?? e2eAgentToken}`;
  } else {
    Object.assign(headers, await userHeaders(options.login));
  }
  if (options.idempotencyKey !== undefined) {
    headers["Idempotency-Key"] = options.idempotencyKey;
  }
  if (options.process !== undefined) Object.assign(headers, pinnedTo(options.process));

  const response = await fetch(new URL(path, options.baseUrl ?? baseUrl), {
    body: payload === undefined ? undefined : JSON.stringify(payload),
    headers,
    method,
  });

  if (!response.ok) {
    throw new Error(`${method} ${path} failed: ${response.status} ${await response.text()}`);
  }

  return (response.status === 204 ? undefined : await response.json()) as T;
}

// Every wrapper below names its route's own path pattern, and reads that route's path
// parameters, its query parameters, its request body and its answer from the generated document
// (`paths`, rendered from the Go route table): nothing here spells a shape of its own. A route
// the document no longer holds, a renamed path parameter and a changed query parameter are then
// errors at the wrapper, and a changed response body an error at the row that reads it. The
// helpers that read the document are the dashboard client's own (web/src/api/paths.ts), so the
// two clients cannot drift apart.

/** One call's own arguments. A route with no path parameter takes no `params`, and one the
 *  document gives no request body takes no `body`; `& object` keeps each an object type, which
 *  is what `request` takes. */
type CallArgs<P extends keyof paths, M extends string> = {
  body?: RequestBody<Op<P, M>> & object;
  query?: QueryParams<Op<P, M>> & object;
} & ([keyof PathParams<Op<P, M>>] extends [never]
  ? { params?: Record<never, never> }
  : { params: PathParams<Op<P, M>> & object });

/** The one place a route's path pattern becomes a request: `request` sends it, with the harness
 *  half's sign-in, bearer, `Origin`, `Idempotency-Key` and process pinning. */
function send<T>(
  pattern: string,
  method: string,
  args: { body?: object; params?: object; query?: object },
  options: ApiOptions
): Promise<T> {
  return request<T>(
    routeUrl(pattern, args.params ?? {}, args.query ?? {}),
    method,
    args.body,
    options
  );
}

function get<P extends PathsOf<"get">>(
  pattern: P,
  args: CallArgs<P, "get">,
  options: ApiOptions = {}
): Promise<Result<P, "get">> {
  return send(pattern, "GET", args, options);
}

/** A read the document cannot type, taken under the type `@legion/contracts` holds for it. Its
 *  path pattern, path parameters and query parameters are still the document's, and only the
 *  answer is the caller's, which has to name both type arguments. */
function getAnswering<T, P extends PathsOf<"get">>(
  pattern: P,
  args: CallArgs<P, "get">,
  options: ApiOptions = {}
): Promise<T> {
  return send(pattern, "GET", args, options);
}

function post<P extends PathsOf<"post">>(
  pattern: P,
  args: CallArgs<P, "post">,
  options: ApiOptions = {}
): Promise<Result<P, "post">> {
  return send(pattern, "POST", args, options);
}

function put<P extends PathsOf<"put">>(
  pattern: P,
  args: CallArgs<P, "put">,
  options: ApiOptions = {}
): Promise<Result<P, "put">> {
  return send(pattern, "PUT", args, options);
}

function patch<P extends PathsOf<"patch">>(
  pattern: P,
  args: CallArgs<P, "patch">,
  options: ApiOptions = {}
): Promise<Result<P, "patch">> {
  return send(pattern, "PATCH", args, options);
}

function del<P extends PathsOf<"delete">>(
  pattern: P,
  args: CallArgs<P, "delete">,
  options: ApiOptions = {}
): Promise<Result<P, "delete">> {
  return send(pattern, "DELETE", args, options);
}

export function createProject(input: Body<"/api/v1/projects", "post">, login = "alice") {
  return post("/api/v1/projects", { body: input }, { login });
}

/** Maps `repo` (`owner/name`) to a project, as the Settings page does; `default: true` makes it the
 *  project's default repository. */
export function putRepoProject(
  repo: string,
  input: Body<"/api/v1/settings/repo-projects/{owner}/{repo}", "put">,
  login = "alice"
) {
  return put(
    "/api/v1/settings/repo-projects/{owner}/{repo}",
    { body: input, params: repoParams(repo) },
    { login }
  );
}

/** Removes `repo`'s mapping, as the Settings page's Delete does. */
export function deleteRepoProject(repo: string, login = "alice") {
  return del(
    "/api/v1/settings/repo-projects/{owner}/{repo}",
    { body: {}, params: repoParams(repo) },
    { login }
  );
}

/** Mints a personal agent token for `login`; the returned bearer acts for that human. */
export async function mintAgentToken(name: string, login: string): Promise<string> {
  const minted = await post("/api/v1/me/agent-tokens", { body: { name } }, { login });
  return minted.token;
}

export function getIssue(key: string, options: ApiOptions = {}) {
  return get("/api/v1/issues/{key}", { params: { key } }, options);
}

/** `GET /api/v1/issues/{key}/events`: the issue's events in the server's order.
 *
 *  The answer is typed `DispatchEvent[]`, not the document's `Event[]`: Go's `model.Event.Payload`
 *  is bytes, so the document carries `payload` as `unknown` and cannot say which payload each
 *  `type` brings, while `DispatchEvent` is exactly that union. The route, its path parameter and
 *  its query parameters are still the document's. */
export function getIssueEvents(
  key: string,
  page: Query<"/api/v1/issues/{key}/events", "get"> = {},
  options: ApiOptions = {}
): Promise<DispatchEvent[]> {
  return getAnswering<DispatchEvent[], "/api/v1/issues/{key}/events">(
    "/api/v1/issues/{key}/events",
    { params: { key }, query: page },
    options
  );
}

export function patchIssue(
  key: string,
  input: Body<"/api/v1/issues/{key}", "patch">,
  options: ApiOptions = {}
) {
  return patch("/api/v1/issues/{key}", { body: input, params: { key } }, options);
}

/** `POST /api/v1/issues/{key}/results`: one result of one check, recorded on `issue`, which
 *  need not be the issue whose Verification document holds the check. The answer carries the
 *  stored result and the pull-request body line it implies. */
export function recordCheckResult(
  issue: string,
  input: Body<"/api/v1/issues/{key}/results", "post">,
  options: ApiOptions = { as: "agent" }
) {
  return post("/api/v1/issues/{key}/results", { body: input, params: { key: issue } }, options);
}

/** `GET /api/v1/issues/{key}/coverage`: what each requirement of the issue's spec lacks. */
export function getIssueCoverage(issue: string, options: ApiOptions = {}) {
  return get("/api/v1/issues/{key}/coverage", { params: { key: issue } }, options);
}

/** The project list in the server's order: lifecycle status, then rank.
 *
 *  The route answers the page object where the call asks for one with `limit` or `offset`, and
 *  the whole list otherwise; this read asks for neither. */
export async function listIssues(project: string, options: ApiOptions = {}) {
  const answered = await get("/api/v1/issues", { query: { project } }, options);
  if (!Array.isArray(answered)) {
    throw new Error("GET /api/v1/issues answered a page where the whole list was asked for");
  }
  return answered;
}

/** The viewer's Inbox rows, as the SPA's own `["inbox"]` query reads them. */
export function getInbox(options: ApiOptions = {}) {
  return get("/api/v1/inbox", {}, options);
}

/** `GET /api/v1/search`: the palette's own read, so a row's `href` here is the path a reader
 *  lands on by clicking that hit. */
export function search(query: string, options: ApiOptions = {}) {
  return get("/api/v1/search", { query: { q: query } }, options);
}

/** `GET /api/v1/me/answers`: the caller's own answers and ask replies, newest first, one page. */
export function listMyAnswers(
  page: Query<"/api/v1/me/answers", "get"> = {},
  options: ApiOptions = {}
) {
  return get("/api/v1/me/answers", { query: page }, options);
}

/** `GET /api/v1/me/replies`: the threads the caller wrote in where someone else has written since
 *  they last read it, newest reply first, one page. */
export function listMyReplies(
  page: Query<"/api/v1/me/replies", "get"> = {},
  options: ApiOptions = {}
) {
  return get("/api/v1/me/replies", { query: page }, options);
}

/** `PUT /api/v1/me/threads/{id}/read`: move the caller's read mark on one thread forward. */
export function markThreadRead(threadId: string, readThrough: string, options: ApiOptions = {}) {
  return put(
    "/api/v1/me/threads/{id}/read",
    { body: { read_through: readThrough }, params: { id: threadId } },
    options
  );
}

/** `PUT /api/v1/me/asks/{id}/snooze`: the caller's own snooze on an Inbox row, until
 *  `snoozedUntil` (RFC 3339, in the future). */
export function snoozeAsk(id: string, snoozedUntil: string, options: ApiOptions = {}) {
  return put(
    "/api/v1/me/asks/{id}/snooze",
    { body: { snoozed_until: snoozedUntil }, params: { id } },
    options
  );
}

/** The Agents page's rows: every live Envoy session merged with its Dispatch activity. */
export function listAgents(options: ApiOptions = {}) {
  return get("/api/v1/agents", {}, options);
}

/** A session's conversation as the Agents page reads it: its exchanges in the server's order. */
export function listAgentMessages(sessionID: string, options: ApiOptions = {}) {
  return get(
    "/api/v1/agents/{session_id}/messages",
    { params: { session_id: sessionID } },
    options
  );
}

export function createIssue(input: Body<"/api/v1/issues", "post">, options: ApiOptions = {}) {
  return post("/api/v1/issues", { body: input }, options);
}

export function createAsk(
  issue: string,
  input: Body<"/api/v1/issues/{key}/asks", "post">,
  options: ApiOptions = {}
) {
  return post("/api/v1/issues/{key}/asks", { body: input, params: { key: issue } }, options);
}

/** Every ask on the issue, whatever its state (the route's default `state=all`). */
export function listIssueAsks(issue: string, options: ApiOptions = {}) {
  return get("/api/v1/issues/{key}/asks", { params: { key: issue } }, options);
}

export function createProjectDocument(
  project: string,
  input: Body<"/api/v1/projects/{key}/artifacts", "post">,
  options: ApiOptions = {}
) {
  return post(
    "/api/v1/projects/{key}/artifacts",
    { body: input, params: { key: project } },
    options
  );
}

export function createIssueArtifact(
  issue: string,
  input: Body<"/api/v1/issues/{key}/artifacts", "post">,
  options: ApiOptions = {}
) {
  return post("/api/v1/issues/{key}/artifacts", { body: input, params: { key: issue } }, options);
}

export function getProjectArtifact(project: string, slug: string, options: ApiOptions = {}) {
  return get(
    "/api/v1/projects/{key}/artifacts/{slug}",
    { params: { key: project, slug } },
    options
  );
}

export function createArtifactAsk(
  artifactID: string,
  input: Body<"/api/v1/artifacts/{id}/asks", "post">,
  options: ApiOptions = {}
) {
  return post("/api/v1/artifacts/{id}/asks", { body: input, params: { id: artifactID } }, options);
}

export function createArtifactComment(
  artifactID: string,
  input: Body<"/api/v1/artifacts/{id}/comments", "post">,
  options: ApiOptions = {}
) {
  return post(
    "/api/v1/artifacts/{id}/comments",
    { body: input, params: { id: artifactID } },
    options
  );
}

export function listArtifactComments(artifactID: string, options: ApiOptions = {}) {
  return get("/api/v1/artifacts/{id}/comments", { params: { id: artifactID } }, options);
}

export function editAsk(
  id: string,
  input: Body<"/api/v1/asks/{id}", "patch">,
  options: ApiOptions = {}
) {
  return patch("/api/v1/asks/{id}", { body: input, params: { id } }, options);
}

export function answerAsk(
  id: string,
  input: Body<"/api/v1/asks/{id}/answer", "post">,
  options: ApiOptions = {}
) {
  return post("/api/v1/asks/{id}/answer", { body: input, params: { id } }, options);
}

export function resolveAsk(
  id: string,
  input: Body<"/api/v1/asks/{id}/resolve", "post">,
  options: ApiOptions = {}
) {
  return post("/api/v1/asks/{id}/resolve", { body: input, params: { id } }, options);
}

export function createComment(
  issue: string,
  input: Body<"/api/v1/issues/{key}/comments", "post">,
  options: ApiOptions = {}
) {
  return post("/api/v1/issues/{key}/comments", { body: input, params: { key: issue } }, options);
}

export function acceptSuggestion(id: string, options: ApiOptions = {}) {
  return post("/api/v1/comments/{id}/accept", { body: {}, params: { id } }, options);
}

export function rejectSuggestion(id: string, options: ApiOptions = {}) {
  return post("/api/v1/comments/{id}/reject", { body: {}, params: { id } }, options);
}

export function resolveComment(id: string, options: ApiOptions = {}) {
  return post("/api/v1/comments/{id}/resolve", { body: {}, params: { id } }, options);
}

export function editComment(
  id: string,
  input: Body<"/api/v1/comments/{id}", "patch">,
  options: ApiOptions = {}
) {
  return patch("/api/v1/comments/{id}", { body: input, params: { id } }, options);
}

export function listComments(
  issue: string,
  artifact: string | undefined = undefined,
  options: ApiOptions = {}
) {
  return get(
    "/api/v1/issues/{key}/comments",
    { params: { key: issue }, query: { artifact } },
    options
  );
}

/** One comment and its reply chain, as the server holds them now: its anchor's quote included. */
export function getComment(id: string, options: ApiOptions = {}) {
  return get("/api/v1/comments/{id}", { params: { id } }, options);
}

/** `GET /api/v1/comments/{id}/blocks`: the comment's current blocks in body order, each with its
 *  span in the stored body, its revision, and whether the blocks are indexed from that body. */
export function getCommentBlocks(id: string, options: ApiOptions = {}) {
  return get("/api/v1/comments/{id}/blocks", { params: { id } }, options);
}

/** A reply to comment `comment` of `issue` that answers its block `block` (`b2`): `reply_to`
 *  with `reply_to_block`. */
export function replyToCommentBlock(
  issue: string,
  comment: string,
  block: string,
  input: Omit<Body<"/api/v1/issues/{key}/comments", "post">, "reply_to" | "reply_to_block">,
  options: ApiOptions = {}
) {
  return createComment(issue, { ...input, reply_to: comment, reply_to_block: block }, options);
}

export function getAsk(id: string, options: ApiOptions = {}) {
  return get("/api/v1/asks/{id}", { params: { id } }, options);
}

export function listAskFollowers(id: string, options: ApiOptions = {}) {
  return get("/api/v1/asks/{id}/followers", { params: { id } }, options);
}

/** A session follows an ask: the bearer body names the session, which must equal the path. */
export function followAsk(id: string, sessionID: string, actor: Actor) {
  return put(
    "/api/v1/asks/{id}/followers/{session_id}",
    { body: {}, params: { id, session_id: sessionID } },
    { actor, as: "agent" }
  );
}

/** The document's current canonical markdown, as the server's live room holds it. */
export function getArtifactText(id: string, options: ApiOptions = {}) {
  return get("/api/v1/artifacts/{id}/text", { params: { id } }, options);
}

/** The blocks of the document's current version, each with the span of markdown it covers. */
export function getArtifactBlocks(id: string, options: ApiOptions = {}) {
  return get("/api/v1/artifacts/{id}/blocks", { params: { id } }, options);
}

/** The same markdown by the slug route, `GET /api/v1/issues/{key}/artifacts/{slug}/text`. */
export function getIssueArtifactText(issue: string, slug: string, options: ApiOptions = {}) {
  return get(
    "/api/v1/issues/{key}/artifacts/{slug}/text",
    { params: { key: issue, slug } },
    options
  );
}

export function getArtifact(id: string, options: ApiOptions = {}) {
  return get("/api/v1/artifacts/{id}", { params: { id } }, options);
}

/**
 * `POST /api/v1/artifacts/{id}/approval-requests`: the request that asks a person to approve the
 * document's latest settled version, with an optional summary of what they are approving.
 *
 * `include_verification` asks for the issue's Verification document beside that document, each at
 * its own current settled version, so one card names both. The route takes it only on the issue's
 * primary document - its spec - and refuses it on any other with `409 NOT_SPEC_DOCUMENT`, and only
 * where that issue holds a `verification.md` with a settled version, refusing an issue that holds
 * none with `409 NO_VERIFICATION_DOCUMENT`.
 */
export function requestApproval(
  id: string,
  input: Body<"/api/v1/artifacts/{id}/approval-requests", "post"> = {},
  options: ApiOptions = {}
) {
  return post("/api/v1/artifacts/{id}/approval-requests", { body: input, params: { id } }, options);
}

/** What `POST /api/v1/artifacts/{id}/approval-requests` answers. */
export type ApprovalRequested = Result<"/api/v1/artifacts/{id}/approval-requests", "post">;

/** That answer with the ask the request opened, for a row that reads the ask: the route answers
 *  `ask: null` where the version it names is already approved, so it opened none, and a row that
 *  goes on to read the ask would otherwise read null. */
export function withOpenedAsk(
  requested: ApprovalRequested
): ApprovalRequested & { ask: NonNullable<ApprovalRequested["ask"]> } {
  const ask = requested.ask;
  if (ask === null) {
    throw new Error(
      `the approval request on ${requested.artifact_id} version ${requested.version} opened no ask`
    );
  }
  return { ...requested, ask };
}

/** `POST /api/v1/artifacts/{id}/plan-reviews`: the plan review's approval of one plan version,
 *  which is the only way a plan is approved. */
export function createPlanReview(
  id: string,
  version: number,
  options: ApiOptions = { as: "agent" }
) {
  return post(
    "/api/v1/artifacts/{id}/plan-reviews",
    { body: { version }, params: { id } },
    options
  );
}

/** Reviews the document's latest settled version as a person does from its header, with no
 *  approval request needed: Approve, or Request changes with a reason. */
export function reviewArtifact(
  id: string,
  input: Body<"/api/v1/artifacts/{id}/reviews", "post">,
  options: ApiOptions = {}
) {
  return post("/api/v1/artifacts/{id}/reviews", { body: input, params: { id } }, options);
}

export function createNamedVersion(id: string, summary: string, options: ApiOptions = {}) {
  return post("/api/v1/artifacts/{id}/versions", { body: { summary }, params: { id } }, options);
}

export function getArtifactVersion(id: string, version: number, options: ApiOptions = {}) {
  return get(
    "/api/v1/artifacts/{id}/versions/{number}",
    { params: { id, number: String(version) } },
    options
  );
}

export function editArtifact(
  id: string,
  input: Body<"/api/v1/artifacts/{id}/edits", "post">,
  options: ApiOptions = {}
) {
  return post("/api/v1/artifacts/{id}/edits", { body: input, params: { id } }, options);
}

/** `POST /api/v1/issues/{key}/claim`: the claimant is the caller itself, so `options.actor`
 *  (the session a bearer names, as on every other write) is the holder recorded. */
export function claimIssue(issue: string, options: ApiOptions = {}) {
  return post("/api/v1/issues/{key}/claim", { body: {}, params: { key: issue } }, options);
}

/** `DELETE /api/v1/issues/{key}/claim`: the holder, `options.actor` with the agent bearer, gives
 *  the issue up. */
export function releaseClaim(issue: string, options: ApiOptions = {}) {
  return del("/api/v1/issues/{key}/claim", { body: {}, params: { key: issue } }, options);
}

export function createMessage(
  issue: string,
  input: Body<"/api/v1/issues/{key}/messages", "post">,
  options: ApiOptions = {}
) {
  return post("/api/v1/issues/{key}/messages", { body: input, params: { key: issue } }, options);
}

/** Sends one message to a session as a human; `options.idempotencyKey` keys it. */
export function createAgentMessage(
  sessionID: string,
  input: Body<"/api/v1/agents/{session_id}/messages", "post">,
  options: ApiOptions = {}
) {
  return post(
    "/api/v1/agents/{session_id}/messages",
    { body: input, params: { session_id: sessionID } },
    options
  );
}

/** Sends one broadcast as a human; `options.idempotencyKey` keys it, as a row that repeats a
 *  request the page made does. */
export function createBroadcast(
  input: Body<"/api/v1/broadcasts", "post">,
  options: ApiOptions = {}
) {
  return post("/api/v1/broadcasts", { body: input }, options);
}

export function listBroadcasts(options: ApiOptions = {}) {
  return get("/api/v1/broadcasts", {}, options);
}

export function getBroadcast(id: string, options: ApiOptions = {}) {
  return get("/api/v1/broadcasts/{id}", { params: { id } }, options);
}

/** `GET /api/v1/issues/{key}/messages`: every root message of the issue, oldest first, each with
 *  its replies, as the Conversation reads them. */
export function listIssueMessages(issue: string, options: ApiOptions = {}) {
  return get("/api/v1/issues/{key}/messages", { params: { key: issue } }, options);
}

export function getMessage(issue: string, messageID: string, options: ApiOptions = {}) {
  return get(
    "/api/v1/issues/{key}/messages/{id}",
    { params: { id: messageID, key: issue } },
    options
  );
}

/** `GET /api/v1/messages/{id}/blocks`: the message's blocks in body order, each with its span in
 *  the stored body, and whether they are indexed from that body. */
export function getMessageBlocks(id: string, options: ApiOptions = {}) {
  return get("/api/v1/messages/{id}/blocks", { params: { id } }, options);
}

/** A reply to message `message` of `issue` that answers its block `block` (`b2`): `in_reply_to`
 *  with `in_reply_to_block`. */
export function replyToMessageBlock(
  issue: string,
  message: string,
  block: string,
  input: Omit<Body<"/api/v1/issues/{key}/messages", "post">, "in_reply_to" | "in_reply_to_block">,
  options: ApiOptions = {}
) {
  return createMessage(
    issue,
    { ...input, in_reply_to: message, in_reply_to_block: block },
    options
  );
}

/** A session answers a targeted message's delivery. `followUp` is `dispatch message`'s
 *  `?follow_up=true`: once the attempt is answered, other text posts as a follow-up instead of
 *  handing back the stored reply. */
export function replyToMessageDelivery(
  messageID: string,
  input: Body<"/api/v1/messages/{id}/reply", "post">,
  actor: Actor,
  { followUp = false }: { followUp?: boolean } = {}
) {
  return post(
    "/api/v1/messages/{id}/reply",
    { body: input, params: { id: messageID }, query: followUp ? { follow_up: true } : {} },
    { actor, as: "agent" }
  );
}

/** A session answers a mention delivered to it, the way the callback path does. */
export function replyToCommentDelivery(
  commentID: string,
  input: Body<"/api/v1/comments/{id}/reply", "post">,
  actor: Actor
) {
  return post(
    "/api/v1/comments/{id}/reply",
    { body: input, params: { id: commentID } },
    { actor, as: "agent" }
  );
}

/** A session records the next version of `phase`'s handoff on the issue, as `dispatch
 *  handoff-write` does: under the agent bearer, naming itself in `actor`. `body` is sent as the
 *  one JSON object's text the server stores verbatim. */
export function createHandoff(
  issue: string,
  phase: string,
  body: object,
  actor: Actor = seedActor
) {
  return post(
    "/api/v1/issues/{key}/handoffs/{phase}",
    { body: { body: JSON.stringify(body) }, params: { key: issue, phase } },
    { actor, as: "agent" }
  );
}

/** The signed-in human's own per-agent conversation state, as another of their devices sets it. */
export function putAgentState(
  sessionID: string,
  input: Body<"/api/v1/me/agents/{session_id}/state", "put">,
  options: ApiOptions = {}
) {
  return put(
    "/api/v1/me/agents/{session_id}/state",
    { body: input, params: { session_id: sessionID } },
    options
  );
}

/** The signed-in human's own conversation state with every session, unread counts included. */
export function getAgentStates(options: ApiOptions = {}) {
  return get("/api/v1/me/agents/state", {}, options);
}

export function putIssueState(
  key: string,
  input: Body<"/api/v1/me/issues/{key}/state", "put">,
  options: ApiOptions = {}
) {
  return put("/api/v1/me/issues/{key}/state", { body: input, params: { key } }, options);
}

/** `login`'s own state on every issue, `GET /api/v1/me/state`: each issue's pin and its pinned
 *  items (`dismissed`), as the page reads them. */
export function getMyState(login = "alice") {
  return get("/api/v1/me/state", {}, { login });
}

/**
 * Forces every open live socket on the server closed, as if it had restarted.
 * Test-only endpoint (mounted when DISPATCH_TEST_HOOKS=1, see run-server.sh) that
 * proves the page resumes every subscription after an unplanned gap.
 */
export function disconnectLiveSockets(options: ApiOptions = {}) {
  return post("/api/v1/live/_test/disconnect", {}, { as: "agent", ...options });
}

/** Forces every open live socket closed with 4001, as its bounded age does. */
export function rotateLiveSockets(options: ApiOptions = {}) {
  return post("/api/v1/live/_test/rotate", {}, { as: "agent", ...options });
}

/**
 * Closes every live document on the server and waits for the settlements in flight, leaving it
 * able to load documents again. Test-only endpoint (mounted when DISPATCH_TEST_HOOKS=1, see
 * run-server.sh) that resetDatabase uses so its TRUNCATE cannot cross lock order with a
 * settlement still running from the scenario before it.
 */
export function quiesceDocuments(options: ApiOptions = {}) {
  return post("/api/v1/artifacts/_test/quiesce", {}, { as: "agent", ...options });
}

/**
 * Makes every keyed `method` request to `path` (the exact request path) take `delayMs` longer on
 * the server: once Dispatch has claimed its key, the run waits that long on the handler's own
 * context before the handler runs, so a write Dispatch is slow to finish is slow server-side
 * rather than held in the browser; a context that ends during the wait answers
 * `503 SLOW_WRITE_CUT_OFF` and the handler never runs. Each call replaces the delay for that
 * method and path, and `delayMs` 0 removes it. Test-only endpoint (mounted when
 * DISPATCH_TEST_HOOKS=1, see run-server.sh), called as a person: its body takes no `actor`.
 */
export function slowWrites(
  method: "PATCH" | "POST",
  path: string,
  delayMs: number,
  options: ApiOptions = {}
) {
  return post("/api/v1/writes/_test/slow", { body: { delay_ms: delayMs, method, path } }, options);
}

/** Adds `repo`'s `.dispatch/architecture` on `branch` as one of `project`'s sources, or moves that
 *  source to `branch` (the fake GitHub serves it). */
export function putArchitectureSource(
  project: string,
  input: { branch: string; repo: string },
  options: ApiOptions = {}
) {
  return put(
    "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}",
    { body: { branch: input.branch }, params: { key: project, ...repoParams(input.repo) } },
    options
  );
}

/** Names the person a project's unowned new issues fall to, or clears it with `assignee: null`. */
export function putProjectDefaultAssignee(
  project: string,
  input: Body<"/api/v1/projects/{key}/default-assignee", "put">,
  options: ApiOptions = {}
) {
  return put(
    "/api/v1/projects/{key}/default-assignee",
    { body: input, params: { key: project } },
    options
  );
}

/** Every project that names a default assignee, as the Settings page reads them. */
export function listProjectDefaultAssignees(options: ApiOptions = {}) {
  return get("/api/v1/settings/project-default-assignees", {}, options);
}

/** Imports `project`'s source `repo` now; a rejected model answers 200 with the reason in
 *  `last_error`. */
export function syncArchitectureSource(project: string, repo: string, options: ApiOptions = {}) {
  return post(
    "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync",
    { params: { key: project, ...repoParams(repo) } },
    options
  );
}

export function getArchitecture(project: string, options: ApiOptions = {}) {
  return get("/api/v1/projects/{key}/architecture", { params: { key: project } }, options);
}

/** Retry one comment mention's delivery, the endpoint the "Mention deliveries" Retry posts to. */
export function retryCommentDelivery(
  comment: string,
  input: Body<"/api/v1/comments/{id}/deliveries", "post">,
  options: ApiOptions = {}
) {
  return post(
    "/api/v1/comments/{id}/deliveries",
    { body: input, params: { id: comment } },
    options
  );
}

/**
 * Test-only endpoint (mounted when DISPATCH_TEST_HOOKS=1, see run-server.sh): publishes one
 * frame to whoever is watching a session's conversation, or, with `as: "replay"`, sets what
 * that session answers a viewer's replay request with. The harness runs Dispatch with NATS
 * disabled, so there is no bus and no live session for a browser test of the conversation view
 * to take frames from.
 */
/** Whether a session answers the relay's replay request at all, with no history behind it: a
 *  live session that has produced nothing (true), or one whose plugin predates the live view or
 *  is no longer running (false). */
export function setAgentStreamResponder(
  sessionID: string,
  responding: boolean,
  options: ApiOptions = {}
) {
  return post(
    "/api/v1/agents/{session_id}/stream/_test/publish",
    { body: { responding }, params: { session_id: sessionID }, query: { as: "responder" } },
    options
  );
}

export function publishAgentStreamFrame(
  sessionID: string,
  frame: Record<string, unknown>,
  as: "frame" | "replay" | "drop" = "frame",
  options: ApiOptions = {}
) {
  return post(
    "/api/v1/agents/{session_id}/stream/_test/publish",
    { body: frame, params: { session_id: sessionID }, query: as === "frame" ? {} : { as } },
    options
  );
}

/** Ends every open agent viewer for a session. The page resubscribes its dropped view on the live
 * socket, which asks the session for its replay again and rebuilds the transcript: this is how a
 * test gets a second replay into a page that is already open, which a published frame alone
 * cannot do. */
export function dropAgentStreamViewers(sessionID: string, options: ApiOptions = {}) {
  return publishAgentStreamFrame(sessionID, {}, "drop", options);
}
