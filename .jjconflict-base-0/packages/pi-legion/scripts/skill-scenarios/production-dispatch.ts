/**
 * The one reader of production Dispatch that label-comments.ts and fetch-spec-sources.ts use. It
 * finds Dispatch as the calling session does, through @legion/envoy-client: the envoy.json that
 * `envoyConfigPath("command")` chooses, or DISPATCH_URL with DISPATCH_TOKEN or DISPATCH_TOKEN_FILE.
 * It sends GET requests only. HTTP and the wait between two tries are injected, so a test passes
 * fakes.
 *
 * It does not use DispatchClient (@legion/envoy-client/dispatch-http): that client gives all its
 * requests one 60-second deadline from its construction, has no unpaged issue list and no issue
 * message list, and does not try a read again. A labelling run reads for many minutes.
 */
import { existsSync, realpathSync } from "node:fs";
import path from "node:path";
import type {
  Artifact,
  ArtifactText,
  Ask,
  Comment,
  IssueDetails,
  IssueSummary,
  MessageRead,
} from "@legion/contracts";
import { type DispatchEnvironment, dispatchSettings } from "@legion/envoy-client/dispatch-config";
import { envoyConfigPath, readEnvoyConfig } from "@legion/envoy-client/envoy-config";
import { messageFor } from "@legion/envoy-client/errors";

/** The HTTP call this module and the tools make. The global `fetch` is one. */
export type Fetch = (url: string, init: RequestInit) => Promise<Response>;

/** The wait between two tries of one call. */
export type Sleep = (ms: number) => Promise<void>;

/** What a tool's `main` reads and writes, so that a test passes fakes. */
export interface CliIo {
  readonly env: DispatchEnvironment;
  readonly cwd: string;
  readonly fetch: Fetch;
  readonly sleep: Sleep;
  readonly out: (line: string) => void;
  readonly err: (line: string) => void;
}

/** The io of a real run. */
export function processIo(): CliIo {
  return {
    env: process.env,
    cwd: process.cwd(),
    fetch: (url, init) => fetch(url, init),
    sleep: (ms) => Bun.sleep(ms),
    out: (line) => console.log(line),
    err: (line) => console.error(line),
  };
}

/** Dispatch's address and the bearer for it. */
export interface DispatchCredentials {
  readonly url: string;
  readonly token: string;
}

/**
 * Dispatch as the calling session finds it (`dispatchSettings` over the envoy.json that
 * `envoyConfigPath("command", env)` chooses, and the checkout's `.envoy/envoy.json` under `cwd`).
 * Throws `Dispatch is not configured: <why>` when no server or no token resolves. The message
 * names the file or the variable that is missing, never a token.
 */
export function resolveDispatch(env: DispatchEnvironment, cwd: string): DispatchCredentials {
  let file: string;
  try {
    file = envoyConfigPath("command", env);
  } catch (error) {
    throw new Error(`Dispatch is not configured: ${messageFor(error)}`);
  }
  const resolution = dispatchSettings(env, readEnvoyConfig(file, env, { cwd }));
  if (!resolution.enabled) {
    throw new Error(`Dispatch is not configured: ${resolution.error ?? resolution.notice}`);
  }
  return { url: resolution.url, token: resolution.token };
}

/** How many times one read is tried before it fails. */
const READ_TRIES = 6;
/** The longest one request may take. */
const REQUEST_TIMEOUT_MS = 60_000;

/** The wait before try `n + 1` (n from 1): 1 s, 2 s, 4 s, and so on, at most 30 s. */
export function backoffMs(n: number): number {
  return Math.min(30_000, 1000 * 2 ** (n - 1));
}

/** True for a status that can change when the same request goes again: a rate limit or a 5xx. */
export function transient(status: number): boolean {
  return status === 429 || status >= 500;
}

/** A GET-only client of production Dispatch's API. */
export class ProductionDispatch {
  readonly #base: string;
  readonly #token: string;
  readonly #fetch: Fetch;
  readonly #sleep: Sleep;

  constructor(credentials: DispatchCredentials, fetchImpl: Fetch, sleep: Sleep) {
    this.#base = credentials.url.replace(/\/+$/, "");
    this.#token = credentials.token;
    this.#fetch = fetchImpl;
    this.#sleep = sleep;
  }

  /** `GET /api/v1/issues` with no query: every issue of every project, closed issues too, in one
   * unpaged read (`parseIssuePage`, packages/envoy/internal/dispatch/api/issue_page.go:18-30). */
  async issues(): Promise<IssueSummary[]> {
    const answer = await this.#get<unknown>(["api", "v1", "issues"]);
    if (!Array.isArray(answer)) {
      throw new Error("GET /api/v1/issues answered something other than a JSON array of issues");
    }
    return answer as IssueSummary[];
  }

  issue(key: string): Promise<IssueDetails> {
    return this.#get(["api", "v1", "issues", key]);
  }

  /** Every comment on the issue, oldest first. */
  comments(key: string): Promise<Comment[]> {
    return this.#list(["api", "v1", "issues", key, "comments"]);
  }

  /** Every root message of the issue, oldest first, each with its replies. */
  messages(key: string): Promise<MessageRead[]> {
    return this.#list(["api", "v1", "issues", key, "messages"]);
  }

  /** Every ask on the issue, in every state: no `state` query, whose default is `all`
   * (`parseAskListState`, packages/envoy/internal/dispatch/api/asks.go), as `listIssueAsks` of
   * packages/dispatch/e2e/api.ts reads it. */
  asks(key: string): Promise<Ask[]> {
    return this.#list(["api", "v1", "issues", key, "asks"]);
  }

  artifactText(id: string): Promise<ArtifactText> {
    return this.#get(["api", "v1", "artifacts", id, "text"]);
  }

  projectArtifact(project: string, slug: string): Promise<Artifact> {
    return this.#get(["api", "v1", "projects", project, "artifacts", slug]);
  }

  projectArtifactText(project: string, slug: string): Promise<ArtifactText> {
    return this.#get(["api", "v1", "projects", project, "artifacts", slug, "text"]);
  }

  async #list<T>(segments: readonly string[], query?: Record<string, string>): Promise<T[]> {
    const answer = await this.#get<unknown>(segments, query);
    if (!Array.isArray(answer)) {
      throw new Error(
        `GET ${describe(segments, query)} answered something other than a JSON array`
      );
    }
    return answer as T[];
  }

  /** One GET, tried again after a 429, a 5xx or a failed connection, the body's arrival included:
   * a connection that fails after the headers is a failed connection too. Its errors name the
   * path and the query, never the server or the token. */
  async #get<T>(segments: readonly string[], query?: Record<string, string>): Promise<T> {
    const shown = describe(segments, query);
    const url = new URL(`${this.#base}${shown}`);
    let last = "";
    for (let attempt = 1; attempt <= READ_TRIES; attempt++) {
      if (attempt > 1) await this.#sleep(backoffMs(attempt - 1));
      let response: Response;
      let text: string;
      try {
        response = await this.#fetch(url.toString(), {
          method: "GET",
          headers: { Accept: "application/json", Authorization: `Bearer ${this.#token}` },
          signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
        });
        text = await response.text();
      } catch (error) {
        last = `failed: ${messageFor(error)}`;
        continue;
      }
      if (response.ok) {
        try {
          return JSON.parse(text) as T;
        } catch {
          throw new Error(`GET ${shown} answered ${response.status} with a body that is not JSON`);
        }
      }
      if (transient(response.status)) {
        last = `answered ${response.status}`;
        continue;
      }
      throw new Error(`GET ${shown} answered ${response.status}${dispatchError(text)}`);
    }
    throw new Error(`GET ${shown} ${last} on each of ${READ_TRIES} tries`);
  }
}

/** The path and the query of a read, each path segment encoded. */
function describe(segments: readonly string[], query?: Record<string, string>): string {
  const pathname = `/${segments.map((segment) => encodeURIComponent(segment)).join("/")}`;
  const search = query === undefined ? "" : `?${new URLSearchParams(query).toString()}`;
  return `${pathname}${search}`;
}

/** ` <code>: <error>` from Dispatch's error JSON, or a note that the body is not that JSON. */
function dispatchError(text: string): string {
  try {
    const shape = JSON.parse(text) as { error?: unknown; code?: unknown };
    if (typeof shape.error === "string") {
      return ` ${typeof shape.code === "string" ? shape.code : "ERROR"}: ${shape.error}`;
    }
  } catch {
    // The body is not JSON. The line below says so.
  }
  return " with a body that is not Dispatch's error JSON";
}

/**
 * The git or jj checkout that holds `target`, or undefined. The search starts at `target` and goes
 * up to the root. A directory holds a checkout when it holds `.git` (a directory, or the file of a
 * git worktree) or `.jj`. The nearest existing ancestor of `target` is resolved through its
 * symbolic links first, as rig.sh resolves SKILL_SCENARIOS_WORK with `realpath -m`.
 */
export function checkoutAbove(target: string): string | undefined {
  let current = resolveExisting(path.resolve(target));
  for (;;) {
    if (existsSync(path.join(current, ".git")) || existsSync(path.join(current, ".jj"))) {
      return current;
    }
    const parent = path.dirname(current);
    if (parent === current) return undefined;
    current = parent;
  }
}

/** `absolute` with its nearest existing ancestor resolved through symbolic links. */
export function resolveExisting(absolute: string): string {
  const rest: string[] = [];
  let current = absolute;
  while (!existsSync(current)) {
    const parent = path.dirname(current);
    if (parent === current) return absolute;
    rest.unshift(path.basename(current));
    current = parent;
  }
  return path.join(realpathSync(current), ...rest);
}

/**
 * The refusal of an `--out` inside a checkout, or undefined when `out` is outside every checkout.
 * Text from production Dispatch must never reach a commit, and jj records every file of its
 * working copy (the rule of rig.sh's SKILL_SCENARIOS_WORK check, extended to any checkout).
 */
export function outRefusal(out: string): string | undefined {
  const resolved = resolveExisting(path.resolve(out));
  const checkout = checkoutAbove(resolved);
  if (checkout === undefined) return undefined;
  return `--out ${resolved} is inside the checkout ${checkout}. Production text must stay out of a checkout, where git or jj can record it. Name a directory outside every checkout.`;
}

const RFC3339 =
  /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(?:([Zz])|([+-])(\d{2}):(\d{2}))$/;

/**
 * An RFC 3339 time as nanoseconds since the epoch, or undefined when `text` is not one. Dispatch
 * stores microseconds, which a JavaScript Date cuts to milliseconds, so the bounds of a window
 * are compared at full precision.
 */
export function parseInstant(text: string): bigint | undefined {
  const match = RFC3339.exec(text);
  if (match === null) return undefined;
  const [, year, month, day, hour, minute, second, fraction, zulu, sign, offsetH, offsetM] = match;
  const parts = [year, month, day, hour, minute, second].map(Number) as [
    number,
    number,
    number,
    number,
    number,
    number,
  ];
  const [y, mo, d, h, mi, s] = parts;
  if (mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 || mi > 59 || s > 60) return undefined;
  let seconds = BigInt(Date.UTC(y, mo - 1, d, h, mi, s) / 1000);
  if (zulu === undefined) {
    const offset = (Number(offsetH) * 60 + Number(offsetM)) * 60;
    seconds -= BigInt(sign === "-" ? -offset : offset);
  }
  const nanos = BigInt((fraction ?? "").padEnd(9, "0"));
  return seconds * 1_000_000_000n + nanos;
}

/** A window of time: `since` inclusive, `until` exclusive. */
export interface Window {
  readonly since: bigint;
  readonly until: bigint;
}

/** The window of `--since` and `--until`, or the refusal that names the bad one. */
export function parseWindow(since: string, until: string): Window | string {
  const from = parseInstant(since);
  if (from === undefined) {
    return `--since ${since} is not an RFC 3339 time, for example 2026-10-16T00:00:00Z`;
  }
  const to = parseInstant(until);
  if (to === undefined) {
    return `--until ${until} is not an RFC 3339 time, for example 2026-10-23T00:00:00Z`;
  }
  if (from >= to) return `--since ${since} is not before --until ${until}`;
  return { since: from, until: to };
}

/** True when `createdAt` is in `window`. Throws when Dispatch gave a time that does not parse. */
export function inWindow(window: Window, createdAt: string): boolean {
  const at = parseInstant(createdAt);
  if (at === undefined)
    throw new Error(`Dispatch gave created_at ${createdAt}, not an RFC 3339 time`);
  return at >= window.since && at < window.until;
}

/** Order by `created_at` at full precision. */
export function byCreatedAt(a: { created_at: string }, b: { created_at: string }): number {
  const left = parseInstant(a.created_at) ?? 0n;
  const right = parseInstant(b.created_at) ?? 0n;
  return left < right ? -1 : left > right ? 1 : 0;
}

/** `fn` over `items`, at most `limit` calls at once, the results in the order of `items`. */
export async function mapLimit<T, R>(
  items: readonly T[],
  limit: number,
  fn: (item: T, index: number) => Promise<R>
): Promise<R[]> {
  const results = new Array<R>(items.length);
  let next = 0;
  const worker = async (): Promise<void> => {
    while (next < items.length) {
      const index = next++;
      results[index] = await fn(items[index] as T, index);
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
  return results;
}
