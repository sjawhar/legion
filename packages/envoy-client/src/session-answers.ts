import { randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { SessionAnswer, SessionAnswerRelation } from "@legion/contracts";
import type { DispatchConfigResolution } from "./dispatch-config";
import { DispatchClient, DispatchServiceError } from "./dispatch-http";
import { messageFor } from "./errors";

/**
 * People's answers on every model request (dispatch://INFRA-148): what people wrote in the last 72
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

/** The reason a read gives when Dispatch has no answers route (a build older than INFRA-148). */
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

/** Whether `error` is Dispatch answering that it has no answers route: any 404, since the route
 *  itself never answers one (a bad session id is 400). */
export function isNoAnswersRoute(error: unknown): boolean {
  return error instanceof DispatchServiceError && error.status === 404;
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
  const owner =
    item.owner === null
      ? undefined
      : "issue" in item.owner
        ? `${item.owner.issue.key}: ${item.owner.issue.title}`
        : `${item.owner.document.project} / ${item.owner.document.name}`;
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

function leftOutLine(count: number, maxChars: number): string {
  return `${count} older ${count === 1 ? "item" : "items"} left out to stay under ${maxChars.toLocaleString("en-US")} characters: run \`dispatch answers\` to read every one in full.`;
}

export interface RenderSessionAnswersOptions {
  /** The render's time: an item written more than 72 hours before it is not shown or counted. */
  readonly now: number | Date;
  /** The cap on the whole message (`SESSION_ANSWERS_MAX_CHARS` by default). */
  readonly maxChars?: number | undefined;
  /** How many items the window held when the list was read (`SessionAnswersResponse.total`),
   *  when the server cut the list short with `max_chars`, so the left-out count covers the items
   *  it did not send; the items' own count when unset. */
  readonly total?: number | undefined;
  /** When the list was read, set when a cached list stands in for a read that failed: the header
   *  says so on its second line. */
  readonly cachedAt?: string | undefined;
}

/**
 * The per-request message: `SESSION_ANSWERS_MARKER` (and, for a cached list, `As read at <time>;
 * Dispatch did not answer now.` on the next line), then each item newest first, blank-line
 * separated, as a strict prefix under the cap: the whole message, header and left-out line
 * included, is at most `maxChars` long; the first item that does not fit is left out with every
 * item after it, so an item over the cap on its own is left out whole, never cut. When any item is
 * left out, the last line counts them and names `dispatch answers`. Items are filtered by the
 * 72-hour window at `now` first. Undefined when the window holds nothing.
 */
export function renderSessionAnswers(
  items: readonly SessionAnswer[],
  options: RenderSessionAnswersOptions
): string | undefined {
  const maxChars = options.maxChars ?? SESSION_ANSWERS_MAX_CHARS;
  const shown = withinWindow(items, Number(options.now));
  // A list the server cut short with max_chars left out only items older than every one it sent,
  // so once any item it sent has left the window, every one it left out has too.
  const unsent =
    shown.length === items.length ? Math.max(0, (options.total ?? items.length) - items.length) : 0;
  const count = shown.length + unsent;
  if (count === 0) return undefined;
  const header =
    options.cachedAt === undefined
      ? SESSION_ANSWERS_MARKER
      : `${SESSION_ANSWERS_MARKER}\nAs read at ${options.cachedAt}; Dispatch did not answer now.`;
  const blocks: string[] = [];
  let length = header.length;
  for (const item of shown) {
    const block = renderSessionAnswerItem(item);
    const after = count - blocks.length - 1;
    const needed =
      length + 2 + block.length + (after === 0 ? 0 : 2 + leftOutLine(after, maxChars).length);
    if (needed > maxChars) break;
    blocks.push(block);
    length += 2 + block.length;
  }
  const leftOut = count - blocks.length;
  return [header, ...blocks, ...(leftOut === 0 ? [] : [leftOutLine(leftOut, maxChars)])].join(
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

/** The line a request carries in place of the list when no list has ever been read. */
export function sessionAnswersFailureLine(reason: string): string {
  return `${FAILURE_MARKER}${reason.replace(/\s+/g, " ").trim()}); run \`dispatch answers\`.]`;
}

/** The last list read for a session, as memory and `answers.json` keep it. */
export interface SessionAnswersCache {
  /** When the read answered (ISO 8601 UTC). */
  readonly read_at: string;
  /** `SessionAnswersResponse.total` of that read. */
  readonly total: number;
  /** `SessionAnswersResponse.items` of that read, newest first. */
  readonly items: readonly SessionAnswer[];
}

type ReadOutcome = { readonly cache: SessionAnswersCache } | { readonly failure: string };

/** One session's read state. */
interface SessionEntry {
  /** The last list read, from a read in this process or `answers.json`. */
  cache: SessionAnswersCache | undefined;
  /** The read in flight, which every request of the session joins; it never rejects. */
  inFlight: Promise<ReadOutcome> | undefined;
  /** Whether a request already waited the whole deadline on `inFlight`, so no other waits on it. */
  overdue: boolean;
}

interface SessionAnswersState {
  readonly sessions: Map<string, SessionEntry>;
  /** Until when (epoch ms) each Dispatch URL that answered 404 is not asked again. */
  readonly noRouteUntil: Map<string, number>;
}

/** The process-wide read state. It lives on `globalThis` under a registered symbol, as
 *  `envoy-config.ts`'s kept command outcomes do, because Oh My Pi loads each extension instance
 *  (a subagent's included) as a fresh module, and the parent and every subagent share one read
 *  and one cache per session. Its shape (`SessionAnswersState`) is its contract across bundles: a
 *  change to it takes a new key (`.v2`). */
const STATE_KEY: unique symbol = Symbol.for("legion.pi-envoy.session-answers.v1");
const processGlobal = globalThis as typeof globalThis & {
  [STATE_KEY]?: SessionAnswersState;
};
const state: SessionAnswersState = processGlobal[STATE_KEY] ?? {
  sessions: new Map(),
  noRouteUntil: new Map(),
};
processGlobal[STATE_KEY] = state;

const CACHE_FILE = "answers.json";

/** `answers.json` under `directory`, or undefined when it cannot be read or is not a cache: a
 *  missing or broken cache only means the request has no list to carry. */
function readCacheFile(directory: string): SessionAnswersCache | undefined {
  let text: string;
  try {
    text = readFileSync(join(directory, CACHE_FILE), "utf-8");
  } catch {
    return undefined;
  }
  try {
    const value: unknown = JSON.parse(text);
    if (
      typeof value === "object" &&
      value !== null &&
      "read_at" in value &&
      typeof value.read_at === "string" &&
      "total" in value &&
      typeof value.total === "number" &&
      "items" in value &&
      Array.isArray(value.items)
    ) {
      // Only `writeCacheFile` writes this file, owner-only in the session's private directory,
      // with the items a Dispatch read answered.
      return { read_at: value.read_at, total: value.total, items: value.items as SessionAnswer[] };
    }
  } catch {
    // Not JSON: no cache.
  }
  return undefined;
}

/** Writes `cache` to `answers.json` under `directory` through a temporary file and `rename`, so a
 *  reader in another process sees the old list or the new one, never part of one. The session
 *  state's own `writeAtomically` is not imported: that module imports `dispatch-execute`, which
 *  imports this one. */
function writeCacheFile(directory: string, cache: SessionAnswersCache): void {
  mkdirSync(directory, { recursive: true, mode: 0o700 });
  const temporary = join(directory, `${CACHE_FILE}.${process.pid}.${randomUUID()}.tmp`);
  try {
    writeFileSync(temporary, JSON.stringify(cache), { mode: 0o600 });
    renameSync(temporary, join(directory, CACHE_FILE));
  } catch (error) {
    rmSync(temporary, { force: true });
    throw error;
  }
}

export interface ReadSessionAnswersOptions {
  /** The session whose list is read: a subagent passes its parent's. */
  readonly session: string;
  /** Dispatch as `sessionDispatchSettings(…, { commands: "kept" })` resolves it, so no `!` token
   *  command runs on the request path. Off with a notice (nothing configures Dispatch) reads
   *  nothing; off with an error is a failed read with that error as its reason. */
  readonly dispatch: DispatchConfigResolution;
  /** The session's state directory (`sessionDirectory(env, session)`), which holds `answers.json`;
   *  unset keeps the cache in memory only. */
  readonly stateDirectory?: string | undefined;
  readonly fetchImpl?: typeof fetch | undefined;
  /** The request's own abort: it stops this call's wait, never the shared read. */
  readonly signal?: AbortSignal | undefined;
  /** The clock (epoch ms): the window, the read time and the 404 back-off read it. */
  readonly now?: (() => number) | undefined;
  /** How long this call waits on the read (`SESSION_ANSWERS_DEADLINE_MS` by default). */
  readonly deadlineMs?: number | undefined;
  /** How long the read itself may run, in the background past the deadline (10 s by default). */
  readonly readTimeoutMs?: number | undefined;
  /** The message's cap (`SESSION_ANSWERS_MAX_CHARS` by default), also sent as `max_chars`. */
  readonly maxChars?: number | undefined;
}

export interface SessionAnswersRead {
  /** The message to carry: the list, the list as last read, or the failure line; undefined when
   *  there is nothing to show or Dispatch is not configured. */
  readonly text: string | undefined;
  /** `fresh`: this request's read answered. `cache`: it did not, and the last list read stands
   *  in. `failure`: it did not and no list was ever read, so `text` is the failure line. `off`:
   *  nothing configures Dispatch. */
  readonly source: "fresh" | "cache" | "failure" | "off";
  /** When the list shown was read (`fresh` and `cache`). */
  readonly readAt?: string;
  /** Why the read gave no list (`cache`, `failure`), or the notice that turns Dispatch off. */
  readonly reason?: string;
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
 * per session for the whole process, and every request of the session (the parent's and each
 * subagent's) joins it. A request waits at most `deadlineMs` (800 ms); past that it carries the last
 * list read, and the read goes on in the background (for at most `readTimeoutMs`) and fills the
 * cache when it answers. While a read that a request already waited out is still in flight, a new
 * request does not wait on it: it carries the cache at once. The cache is in memory, keyed by
 * session, and in `answers.json` under `stateDirectory`, loaded when memory has none; a cached
 * list is rendered with its read time and filtered by the 72 hours again. With no list ever read,
 * a failed read carries the failure line naming why. A Dispatch that answers 404 has no answers
 * route: no request goes to its URL for 5 minutes, process-wide.
 */
export async function readSessionAnswers(
  options: ReadSessionAnswersOptions
): Promise<SessionAnswersRead> {
  const now = options.now ?? Date.now;
  const maxChars = options.maxChars ?? SESSION_ANSWERS_MAX_CHARS;
  const { dispatch, session, stateDirectory } = options;
  if (!dispatch.enabled && dispatch.error === null) {
    return { text: undefined, source: "off", reason: dispatch.notice };
  }
  let entry = state.sessions.get(session);
  if (entry === undefined) {
    entry = { cache: undefined, inFlight: undefined, overdue: false };
    state.sessions.set(session, entry);
  }
  if (entry.cache === undefined && stateDirectory !== undefined) {
    entry.cache = readCacheFile(stateDirectory);
  }
  const current = entry;
  const carry = (reason: string): SessionAnswersRead => {
    const { cache } = current;
    if (cache === undefined) {
      return { text: sessionAnswersFailureLine(reason), source: "failure", reason };
    }
    return {
      text: renderSessionAnswers(cache.items, {
        now: now(),
        maxChars,
        total: cache.total,
        cachedAt: cache.read_at,
      }),
      source: "cache",
      readAt: cache.read_at,
      reason,
    };
  };
  if (!dispatch.enabled) return carry(dispatch.error);
  if ((state.noRouteUntil.get(dispatch.url) ?? 0) > now()) return carry(NO_ANSWERS_ROUTE_REASON);
  if (current.inFlight !== undefined && current.overdue) {
    return carry("an earlier read is still waiting on Dispatch");
  }
  let read = current.inFlight;
  if (read === undefined) {
    const client = new DispatchClient(dispatch.url, dispatch.token, options.fetchImpl ?? fetch);
    const started = client
      .sessionAnswers(session, {
        maxChars,
        signal: AbortSignal.timeout(options.readTimeoutMs ?? READ_TIMEOUT_MS),
      })
      .then(
        (response): ReadOutcome => {
          const cache = {
            read_at: new Date(now()).toISOString(),
            total: response.total,
            items: response.items,
          };
          current.cache = cache;
          if (stateDirectory !== undefined) {
            try {
              writeCacheFile(stateDirectory, cache);
            } catch {
              // The memory cache still holds it; the next read writes the file again.
            }
          }
          return { cache };
        },
        (error: unknown): ReadOutcome => {
          if (isNoAnswersRoute(error)) {
            state.noRouteUntil.set(dispatch.url, now() + NO_ROUTE_BACKOFF_MS);
            return { failure: NO_ANSWERS_ROUTE_REASON };
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
    return carry(`Dispatch did not answer within ${deadlineMs} ms`);
  }
  if (outcome === "aborted") return carry("the request was cancelled before Dispatch answered");
  if ("failure" in outcome) return carry(outcome.failure);
  return {
    text: renderSessionAnswers(outcome.cache.items, {
      now: now(),
      maxChars,
      total: outcome.cache.total,
    }),
    source: "fresh",
    readAt: outcome.cache.read_at,
  };
}

/** Settles once the read in flight for `session`, if any, has filled the cache or failed. A
 *  short-lived host (claude-envoy's hook) awaits it after printing, within a budget of its own, so
 *  a read that answered past the deadline still writes `answers.json` for the next process. */
export async function sessionAnswersSettled(session: string): Promise<void> {
  await state.sessions.get(session)?.inFlight;
}
