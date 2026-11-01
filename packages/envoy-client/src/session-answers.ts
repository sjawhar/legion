import { mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import type { SessionAnswer, SessionAnswerRelation } from "@legion/contracts";
import type { DispatchConfigResolution } from "./dispatch-config";
import { DispatchClient, DispatchNoRouteError, isNoRoute } from "./dispatch-http";
import { ownerTitle } from "./dispatch-owner";
import { sessionDirectory, writeAtomically } from "./dispatch-session-directory";
import { messageFor } from "./errors";

/**
 * People's answers on every model request: what people wrote in the last 72
 * hours that Dispatch delivered to a session (`GET /api/v1/agents/{session_id}/answers`), rendered
 * as one message under a cap, and read on each request within a deadline, carrying the last list
 * read when Dispatch does not answer in time. pi-envoy calls `readSessionAnswers` in process, its
 * subagents sharing one in-flight read and one cache; claude-envoy's hook calls it as a short-lived
 * process, where the disk cache is what carries a list across calls; `dispatch answers` prints the
 * whole list with `renderSessionAnswersList`.
 */

/** How far back the list reaches, at the read and again at each render. */
export const SESSION_ANSWERS_WINDOW_MS = 72 * 3_600_000;

/** The cap on the whole rendered message, in UTF-16 units as `DISPATCH_BODY_MAX` counts. */
export const SESSION_ANSWERS_MAX_CHARS = 8_000;

/** How long one model request waits on the read before it carries the last list read. */
export const SESSION_ANSWERS_DEADLINE_MS = 800;

/** How long a read may run in the background, past the deadline, to fill the cache. */
const READ_TIMEOUT_MS = 10_000;

/** How long a Dispatch that has no answers route is not asked again, process-wide. */
const NO_ROUTE_BACKOFF_MS = 5 * 60_000;

/** How much of an ask's question an item quotes: the session's own words, not the person's. */
const QUESTION_MAX = 200;

/** The rendered list's first line, and the marker a host checks to find a list it already added.
 *  A list carried from the cache adds its read time on the line after, so this line never varies. */
export const SESSION_ANSWERS_MARKER =
  "[Dispatch: what people wrote to this session in the last 72 hours, newest first, word for word. Reference, not a new request.]";

/** How the line standing in for a list that could not be read begins. */
const FAILURE_MARKER = "[Dispatch: people's answers from the last 72 hours could not be read (";

/** The reason a read gives when Dispatch has no answers route (a build older than the route). */
export const NO_ANSWERS_ROUTE_REASON = "this Dispatch build has no answers route";

const RELATION_PHRASES: Readonly<Record<SessionAnswerRelation, string>> = {
  answered_your_ask: "answered your ask",
  replied_on_your_ask: "replied on your ask",
  replied_in_a_comment_thread: "replied in a comment thread",
  commented_on_a_document: "commented on a document",
  commented_on_an_issue: "commented on an issue",
  mentioned_you: "mentioned you",
  sent_you_a_message: "sent you a message",
  replied_to_your_message: "replied to your message",
};

/** Whether `text` is a message `renderSessionAnswers` or the failure line wrote: what a host checks
 *  so a second copy of its extension adds no second list. */
export function isSessionAnswersText(text: string): boolean {
  return text.startsWith(SESSION_ANSWERS_MARKER) || text.startsWith(FAILURE_MARKER);
}

/** Whether `error` is Dispatch's own answer that it has no answers route: the `404 NOT_FOUND`
 *  "no route for …" its router answers for a path it has no route for, as a build older than the
 *  route answers it. Both shapes of that answer count: the raw one (`isNoRoute`), as the read on
 *  the request path sees it from a client built with `retryNoRoute: false`, and
 *  `DispatchNoRouteError`, which a client that asks again (the `dispatch answers` command and the
 *  `dispatch_answers` tool) reports once every attempt met no route. A 404 that something in
 *  Dispatch's place wrote (a `DispatchGatewayError`: a proxy's page, an empty body) says nothing
 *  about Dispatch's routes. */
export function isNoAnswersRoute(error: unknown): boolean {
  return isNoRoute(error) || error instanceof DispatchNoRouteError;
}

/**
 * One item as both the per-request list and `dispatch answers` print it, in this order: the time
 * the person wrote it, the person and what it is to the session (with `replaces an earlier answer`
 * on an answer that replaced one, and `edited <time>` on an edited comment); its `dispatch://`
 * reference and its issue or document, or that it is a direct message; the ask's question, cut to
 * 200 characters, on an answer and a reply in an ask's thread; on an answer, `Selected:` with the
 * options chosen or `none`; then the person's words in full.
 */
export function renderSessionAnswerItem(item: SessionAnswer): string {
  const relation = `${RELATION_PHRASES[item.relation]}${item.replaced === true ? " (replaces an earlier answer)" : ""}`;
  const head = [item.at, `user ${item.author.id}`, relation];
  if (item.edited_at !== undefined) head.push(`edited ${item.edited_at}`);
  const owner = item.owner === null ? undefined : ownerTitle(item.owner);
  const where =
    item.ref === null
      ? `Direct message ${item.id}, on no issue`
      : owner === undefined
        ? item.ref
        : `${item.ref} · ${owner}`;
  const lines = [head.join(" · "), where];
  if (item.ask !== undefined) {
    const question = item.ask.question.replace(/\s+/g, " ").trim();
    lines.push(
      `Question: ${question.length > QUESTION_MAX ? `${question.slice(0, QUESTION_MAX)}…` : question}`
    );
  }
  if (item.kind === "answer") {
    const selected = item.selected ?? [];
    lines.push(`Selected: ${selected.length === 0 ? "none" : selected.join(", ")}`);
  }
  if (item.text !== "") lines.push(item.text);
  return lines.join("\n");
}

/** The items written inside the 72 hours before `now`, in the order given. */
function withinWindow(items: readonly SessionAnswer[], now: number): SessionAnswer[] {
  const start = now - SESSION_ANSWERS_WINDOW_MS;
  return items.filter((item) => Date.parse(item.at) >= start);
}

/** `SESSION_ANSWERS_MAX_CHARS` as the left-out line prints it. */
const MAX_CHARS_TEXT = SESSION_ANSWERS_MAX_CHARS.toLocaleString("en-US");

/** The last line of a list that left `count` items out: it names `dispatch answers`, which reads
 *  every one, unless `namesCommand` is false because that command cannot read the Dispatch. */
function leftOutLine(count: number, namesCommand: boolean): string {
  const counted = `${count} older ${count === 1 ? "item" : "items"} left out to stay under ${MAX_CHARS_TEXT} characters`;
  return namesCommand
    ? `${counted}: run \`dispatch answers\` to read every one in full.`
    : `${counted}.`;
}

export interface RenderSessionAnswersOptions {
  /** The render's time: an item written more than 72 hours before it is not shown or counted. */
  readonly now: number | Date;
  /** How many items the window held when the list was read (`SessionAnswersResponse.total`),
   *  when the server cut the list short with `max_chars`, so the left-out count covers the items
   *  it did not send; the items' own count when unset. */
  readonly total?: number | undefined;
  /** Set when a cached list stands in for a read that failed: when that list was read and why
   *  this read failed, which the header names on its second line. */
  readonly cached?: { readonly readAt: string; readonly reason: string } | undefined;
  /** Set when `dispatch answers` cannot read the Dispatch the list came from, since it has no
   *  answers route: the left-out line then counts the items left out and names no command. */
  readonly withoutCommand?: boolean | undefined;
}

/**
 * The per-request message: `SESSION_ANSWERS_MARKER` (and, for a cached list, `As read at <time>;
 * this read failed: <reason>.` on the next line), then each item newest first, blank-line
 * separated, as a strict prefix under the cap: the whole message, header and left-out line
 * included, is shorter than `SESSION_ANSWERS_MAX_CHARS` (at most 7,999 characters, as the
 * left-out line's "under 8,000" says); the first item that does not fit is left
 * out with every item after it, so an item over the cap on its own is left out whole, never cut.
 * When any item is left out, the last line counts them and names `dispatch answers` (no command
 * with `withoutCommand`). Items are
 * filtered by the 72-hour window at `now` first. Undefined when the window holds nothing.
 */
export function renderSessionAnswers(
  items: readonly SessionAnswer[],
  options: RenderSessionAnswersOptions
): string | undefined {
  const shown = withinWindow(items, Number(options.now));
  // A list the server cut short with max_chars left out only items older than every one it sent,
  // so once any item it sent has left the window, every one it left out has too.
  const unsent =
    shown.length === items.length ? Math.max(0, (options.total ?? items.length) - items.length) : 0;
  const count = shown.length + unsent;
  if (count === 0) return undefined;
  const header =
    options.cached === undefined
      ? SESSION_ANSWERS_MARKER
      : `${SESSION_ANSWERS_MARKER}\nAs read at ${options.cached.readAt}; this read failed: ${failureSentence(options.cached.reason)}`;
  const namesCommand = options.withoutCommand !== true;
  const blocks: string[] = [];
  let length = header.length;
  for (const item of shown) {
    const block = renderSessionAnswerItem(item);
    const after = count - blocks.length - 1;
    const needed =
      length + 2 + block.length + (after === 0 ? 0 : 2 + leftOutLine(after, namesCommand).length);
    if (needed >= SESSION_ANSWERS_MAX_CHARS) break;
    blocks.push(block);
    length += 2 + block.length;
  }
  const leftOut = count - blocks.length;
  return [header, ...blocks, ...(leftOut === 0 ? [] : [leftOutLine(leftOut, namesCommand)])].join(
    "\n\n"
  );
}

/** What `dispatch answers` prints: every item of the 72 hours before `now`, newest first, in the
 *  per-request list's item format and with no cap, or a line saying there is none. */
export function renderSessionAnswersList(
  items: readonly SessionAnswer[],
  options: { readonly now: number | Date }
): string {
  const shown = withinWindow(items, Number(options.now));
  if (shown.length === 0) return "No person wrote to this session in the last 72 hours.";
  return [
    `${shown.length} ${shown.length === 1 ? "item" : "items"} people wrote to this session in the last 72 hours, newest first, word for word:`,
    ...shown.map(renderSessionAnswerItem),
  ].join("\n\n");
}

/** A read's failure reason as the end of a sentence: whitespace collapsed, and a full stop added
 *  only when the reason does not already end in `.`, `?` or `!`. */
function failureSentence(reason: string): string {
  const text = reason.replace(/\s+/g, " ").trim();
  return /[.?!]$/.test(text) ? text : `${text}.`;
}

/** The line a request carries in place of the list when no list it could carry renders. */
export function sessionAnswersFailureLine(reason: string): string {
  return `${FAILURE_MARKER}${reason.replace(/\s+/g, " ").trim()}); run \`dispatch answers\`.]`;
}

/** The last list read for a session from one Dispatch, as memory and `answers.json` keep it. */
export interface SessionAnswersCache {
  /** The Dispatch URL the list was read from: only a read of that URL carries it. */
  readonly dispatch_url: string;
  /** When the read answered (ISO 8601 UTC). */
  readonly read_at: string;
  /** `SessionAnswersResponse.total` of that read. */
  readonly total: number;
  /** `SessionAnswersResponse.items` of that read, newest first. */
  readonly items: readonly SessionAnswer[];
}

type ReadOutcome =
  | { readonly cache: SessionAnswersCache }
  | { readonly failure: string }
  /** Dispatch answered that it has no answers route (`isNoAnswersRoute`). */
  | { readonly noRoute: true };

/** One session's read state for one Dispatch URL. */
interface SessionEntry {
  /** The last list read from that URL, from a read in this process or `answers.json`. */
  cache: SessionAnswersCache | undefined;
  /** The read in flight to that URL, which every request of the session to it joins; it never
   *  rejects. */
  inFlight: Promise<ReadOutcome> | undefined;
  /** Whether a request already waited the whole deadline on `inFlight`, so no other waits on it. */
  overdue: boolean;
  /** Whether a read already gave the session the notice that the URL has no answers route, since
   *  the last read of it that answered with a list. */
  noRouteReported: boolean;
}

interface SessionAnswersState {
  /** Each session's read state, by session id and then by Dispatch URL. */
  readonly sessions: Map<string, Map<string, SessionEntry>>;
  /** Until when (epoch ms) each Dispatch URL that answered it has no answers route is not asked
   *  again. */
  readonly noRouteUntil: Map<string, number>;
}

/** The process-wide read state. It lives on `globalThis` under a registered symbol, as
 *  `envoy-config.ts`'s kept command outcomes do, because Oh My Pi loads each extension instance
 *  (a subagent's included) as a fresh module, and the parent and every subagent share one read
 *  and one cache per session and Dispatch URL. Its shape (`SessionAnswersState`) is its contract
 *  across bundles: a change to it takes a new key (`.v4`). */
const STATE_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers.v3");
const processGlobal = globalThis as typeof globalThis & {
  [STATE_KEY]?: SessionAnswersState;
};
const state: SessionAnswersState = processGlobal[STATE_KEY] ?? {
  sessions: new Map(),
  noRouteUntil: new Map(),
};
processGlobal[STATE_KEY] = state;

const CACHE_FILE = "answers.json";

/** `answers.json` under `directory` when it holds a list read from `url`, else undefined: a
 *  missing or broken cache, or one another Dispatch answered, only means the request has no list
 *  to carry. */
function readCacheFile(directory: string, url: string): SessionAnswersCache | undefined {
  try {
    const value: unknown = JSON.parse(readFileSync(join(directory, CACHE_FILE), "utf-8"));
    if (
      typeof value === "object" &&
      value !== null &&
      "dispatch_url" in value &&
      typeof value.dispatch_url === "string" &&
      value.dispatch_url === url &&
      "read_at" in value &&
      typeof value.read_at === "string" &&
      "total" in value &&
      typeof value.total === "number" &&
      "items" in value &&
      Array.isArray(value.items)
    ) {
      // Only `readSessionAnswers` writes this file, owner-only in the session's private
      // directory, with the items a Dispatch read answered.
      return {
        dispatch_url: value.dispatch_url,
        read_at: value.read_at,
        total: value.total,
        items: value.items as SessionAnswer[],
      };
    }
  } catch {
    // Missing, unreadable or not JSON: no cache.
  }
  return undefined;
}

export interface ReadSessionAnswersOptions {
  /** The session whose list is read: a subagent passes its parent's. */
  readonly session: string;
  /** Dispatch as `sessionDispatchSettings(…, { commands: "kept" })` resolves it, so no `!` token
   *  command runs on the request path. Off with a notice (nothing configures Dispatch) reads
   *  nothing; off with an error is a failed read with that error as its reason. */
  readonly dispatch: DispatchConfigResolution;
  /** The Dispatch URL the session is configured with (`sessionDispatchUrl`), for `dispatch` off
   *  with an error, which names none: only a list read from this URL stands in. Unset, such a read
   *  carries the failure line. Not read while `dispatch` is on: its own URL is the one. */
  readonly dispatchUrl?: string | undefined;
  /** The environment the session's state directory is worked out from
   *  (`sessionDirectory(env, session)`), which holds `answers.json`. A session id that is not one
   *  plain path segment keeps the cache in memory only. */
  readonly env: Readonly<Record<string, string | undefined>>;
  readonly fetchImpl?: typeof fetch | undefined;
  /** The request's own abort: it stops this call's wait, never the shared read. */
  readonly signal?: AbortSignal | undefined;
  /** The clock (epoch ms): the window, the read time and the 404 back-off read it. */
  readonly now?: (() => number) | undefined;
  /** How long this call waits on the read (`SESSION_ANSWERS_DEADLINE_MS` by default). */
  readonly deadlineMs?: number | undefined;
}

export interface SessionAnswersRead {
  /** The message to carry: the list, the list as last read, or the failure line; undefined when
   *  there is nothing to show, Dispatch has no answers route, or Dispatch is not configured. */
  readonly text: string | undefined;
  /** `fresh`: this request's read answered. `cache`: it did not, and the last list read stands
   *  in. `failure`: it did not and no list read has an item inside the 72 hours, so `text` is the
   *  failure line. `unavailable`: Dispatch answered that it has no answers route
   *  (`isNoAnswersRoute`, or that answer's 5-minute back-off) and no list read has an item inside
   *  the 72 hours, so the request carries nothing: the failure line would send the agent to
   *  `dispatch answers`, which that Dispatch refuses too. `off`: nothing configures Dispatch. */
  readonly source: "fresh" | "cache" | "failure" | "unavailable" | "off";
  /** When the list shown was read (`fresh` and `cache`). */
  readonly readAt?: string;
  /** Why the read gave no list (`cache`, `failure`, `unavailable`), or the notice that turns
   *  Dispatch off. */
  readonly reason?: string;
  /** For the host to report once, never to the model: set on the first read for this session
   *  and Dispatch URL that found the URL has no answers route, and again only after a read of it
   *  answered with a list. It names the URL. */
  readonly notice?: string;
}

/** `promise`'s value, or which came first of `ms` passing and `signal` aborting. */
async function settleWithin<T>(
  promise: Promise<T>,
  ms: number,
  signal: AbortSignal | undefined
): Promise<T | "deadline" | "aborted"> {
  if (signal?.aborted === true) return "aborted";
  let timer: NodeJS.Timeout | undefined;
  let onAbort: (() => void) | undefined;
  const stop = new Promise<"deadline" | "aborted">((resolve) => {
    timer = setTimeout(() => resolve("deadline"), ms);
    onAbort = () => resolve("aborted");
    signal?.addEventListener("abort", onAbort, { once: true });
  });
  try {
    return await Promise.race([promise, stop]);
  } finally {
    clearTimeout(timer);
    if (onAbort !== undefined) signal?.removeEventListener("abort", onAbort);
  }
}

/**
 * The list for one model request, never throwing. Every request reads it; one read is in flight
 * per session and Dispatch URL for the whole process, and every request of the session (the
 * parent's and each subagent's) to that URL joins it. A request waits at most `deadlineMs`
 * (800 ms); past that it carries the last list read, and the read goes on in the background (for
 * at most 10 s) and fills the cache when it answers. While a read that a request already waited
 * out is still in flight, a new request does not wait on it: it carries the cache at once. The
 * cache is in memory, keyed by session and Dispatch URL, and in `answers.json` under the
 * session's state directory (`sessionDirectory(env, session)`) with the URL it was read from,
 * loaded when memory has none for that URL; a list read from another Dispatch is never carried.
 * A cached list is rendered with its read time and the reason this read failed, and filtered by
 * the 72 hours again. When no cached list renders (none was read, it was empty, or every item is
 * now past 72 hours), a failed read carries the failure line naming why. Dispatch off with an
 * error names no URL: only a list read from `dispatchUrl` stands in, and with none given the
 * failure line does. A Dispatch that answers it has no answers route (`isNoAnswersRoute`) is not
 * asked again for 5 minutes, process-wide; that answer, and a request inside those 5 minutes,
 * carry the list read before from that URL, its left-out line naming no command, or else nothing:
 * the failure line would send the agent to `dispatch answers`, which that Dispatch refuses too.
 * The first such read for a session and URL, until a read of it answers with a list again, gives
 * the host a `notice` to report. Every other failure, a gateway's 404 or a 200 that is no
 * answers list included, is a failed read as above.
 */
export async function readSessionAnswers(
  options: ReadSessionAnswersOptions
): Promise<SessionAnswersRead> {
  const now = options.now ?? Date.now;
  const { dispatch, session } = options;
  if (!dispatch.enabled && dispatch.error === null) {
    return { text: undefined, source: "off", reason: dispatch.notice };
  }
  let directory: string | undefined;
  try {
    directory = sessionDirectory(options.env, session);
  } catch {
    // An id that is not one plain path segment keeps no answers.json; the read still runs.
  }
  const entries = state.sessions.get(session) ?? new Map<string, SessionEntry>();
  state.sessions.set(session, entries);
  const carry = (
    cache: SessionAnswersCache | undefined,
    reason: string,
    noRoute = false
  ): SessionAnswersRead => {
    const text =
      cache === undefined
        ? undefined
        : renderSessionAnswers(cache.items, {
            now: now(),
            total: cache.total,
            cached: { readAt: cache.read_at, reason },
            withoutCommand: noRoute,
          });
    if (cache === undefined || text === undefined) {
      if (noRoute) return { text: undefined, source: "unavailable", reason };
      // A cache that renders nothing is no list to show: the failure line keeps a read that failed
      // from looking like no person wrote.
      return { text: sessionAnswersFailureLine(reason), source: "failure", reason };
    }
    return { text, source: "cache", readAt: cache.read_at, reason };
  };
  if (!dispatch.enabled) {
    const url = options.dispatchUrl;
    if (url === undefined) return carry(undefined, dispatch.error);
    const cache =
      entries.get(url)?.cache ??
      (directory === undefined ? undefined : readCacheFile(directory, url));
    return carry(cache, dispatch.error);
  }
  const { url } = dispatch;
  const current: SessionEntry = entries.get(url) ?? {
    cache: undefined,
    inFlight: undefined,
    overdue: false,
    noRouteReported: false,
  };
  entries.set(url, current);
  if (current.cache === undefined && directory !== undefined) {
    current.cache = readCacheFile(directory, url);
  }
  const noRoute = (): SessionAnswersRead => {
    const read = carry(current.cache, NO_ANSWERS_ROUTE_REASON, true);
    if (current.noRouteReported) return read;
    current.noRouteReported = true;
    return {
      ...read,
      notice: `Dispatch at ${url} has no answers route (its build is older than the route), so people's answers cannot be read from it`,
    };
  };
  if ((state.noRouteUntil.get(url) ?? 0) > now()) return noRoute();
  if (current.inFlight !== undefined && current.overdue) {
    return carry(current.cache, "an earlier read is still waiting on Dispatch");
  }
  let read = current.inFlight;
  if (read === undefined) {
    // The client runs the read once: an answer of no route is this read's own (`isNoAnswersRoute`,
    // `NO_ROUTE_BACKOFF_MS`), and that 5-minute back-off, not the client's waits, is what carries
    // the request past a release rolling out, within this call's deadline.
    const client = new DispatchClient(url, dispatch.token, options.fetchImpl ?? fetch, {
      retryNoRoute: false,
    });
    const started = client
      .sessionAnswers(session, {
        maxChars: SESSION_ANSWERS_MAX_CHARS,
        signal: AbortSignal.timeout(READ_TIMEOUT_MS),
      })
      .then(
        (response): ReadOutcome => {
          // A 200 whose body is no answers list (a proxy's page, another service on the URL) is a
          // failed read, never a cache: rendering it would throw on the request path.
          if (!Array.isArray(response?.items) || typeof response.total !== "number") {
            return { failure: "Dispatch answered with something that is not an answers list" };
          }
          const cache: SessionAnswersCache = {
            dispatch_url: url,
            read_at: new Date(now()).toISOString(),
            total: response.total,
            items: response.items,
          };
          current.cache = cache;
          current.noRouteReported = false;
          if (directory !== undefined) {
            try {
              mkdirSync(directory, { recursive: true, mode: 0o700 });
              writeAtomically(join(directory, CACHE_FILE), JSON.stringify(cache));
            } catch {
              // The memory cache still holds it; the next read writes the file again.
            }
          }
          return { cache };
        },
        (error: unknown): ReadOutcome => {
          if (isNoAnswersRoute(error)) {
            state.noRouteUntil.set(url, now() + NO_ROUTE_BACKOFF_MS);
            return { noRoute: true };
          }
          return { failure: messageFor(error) };
        }
      );
    read = started.finally(() => {
      if (current.inFlight === read) {
        current.inFlight = undefined;
        current.overdue = false;
      }
    });
    current.inFlight = read;
    current.overdue = false;
  }
  const deadlineMs = options.deadlineMs ?? SESSION_ANSWERS_DEADLINE_MS;
  const outcome = await settleWithin(read, deadlineMs, options.signal);
  if (outcome === "deadline") {
    if (current.inFlight === read) current.overdue = true;
    return carry(current.cache, `Dispatch did not answer within ${deadlineMs} ms`);
  }
  if (outcome === "aborted") {
    return carry(current.cache, "the request was cancelled before Dispatch answered");
  }
  if ("noRoute" in outcome) return noRoute();
  if ("failure" in outcome) return carry(current.cache, outcome.failure);
  return {
    text: renderSessionAnswers(outcome.cache.items, {
      now: now(),
      total: outcome.cache.total,
    }),
    source: "fresh",
    readAt: outcome.cache.read_at,
  };
}

/** Settles once every read in flight for `session`, to any Dispatch, has filled its cache or
 *  failed. A short-lived host (claude-envoy's hook) awaits it after printing, within a budget of
 *  its own, so a read that answered past the deadline still writes `answers.json` for the next
 *  process. */
export async function sessionAnswersSettled(session: string): Promise<void> {
  await Promise.all(
    Array.from(state.sessions.get(session)?.values() ?? [], (entry) => entry.inFlight)
  );
}
