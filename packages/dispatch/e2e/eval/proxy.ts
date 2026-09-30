#!/usr/bin/env bun
/**
 * The Dispatch evaluation proxy: what an agent under evaluation gets as its DISPATCH_URL, so it
 * reads a real Dispatch and writes nothing to it.
 *
 * - A GET on the allow-list (`READS` in reads.ts: every route `DispatchClient` in
 *   `packages/envoy-client/src/dispatch-http.ts` reads, and the route index, block schema,
 *   project architecture and document blocks the Dispatch skill tells an agent to read) is
 *   forwarded to `--upstream` with the `--token-file` bearer. The agent's own `Authorization` is
 *   never forwarded.
 * - Every other GET, and any method that is neither a GET nor a write, is refused with `403
 *   EVAL_PROXY_REFUSED` and recorded.
 * - Every POST, PATCH, PUT and DELETE is recorded to the `--writes` JSONL file and never sent.
 *   A write `WRITES` in writes.ts models is answered as the server answers it, and later reads
 *   show it as the server would. Any other write, and any input a modelled write cannot follow
 *   (the duplicate check, a claim held by another session, a delivery, a document edit), is
 *   recorded and answered `501 EVAL_PROXY_UNMODELLED_WRITE`, never a guessed success, with one
 *   exception: the server's strict JSON decoder refuses an unknown field or a non-JSON content
 *   type, and the proxy answers such a body from the fields it reads. Answers carry no
 *   `advice`; events carry no `references_changed`; a mention in what the agent writes adds no
 *   search hit, reference edge or backlink count; a document the agent uploads reads back as
 *   uploaded with the canonical final line feed (`canonicalText` in overlay.ts), and its text
 *   `token` is not the server's. Issues the agent creates are numbered from 900001, past any
 *   real issue's number, so a list ordered by key places them by that number.
 * - `--exclude-key <KEY>` (repeatable) hides an issue: every route addressed by that key, or by
 *   an ask, comment, message or document it owns, answers `404 NOT_FOUND`; every list element it
 *   owns is dropped from every answer (list rows, children, child events, search hits, open asks,
 *   reference edges and members, component rows), and a `parent` or `inherited_from` naming it
 *   reads null. Mentions inside longer text are left as they are, and aggregate counts
 *   (subtree totals, backlink counts, architecture totals) still count it. An answer that still
 *   names it anywhere else, an upstream refusal naming it included, is withheld with `502
 *   EVAL_PROXY_WITHHELD`: the filter fails closed.
 * - An upstream that fails answers `502 EVAL_PROXY_UPSTREAM_FAILED` naming the failure.
 *
 * Dispatch has no read-only token, so this allow-list is the only barrier between the agent and
 * a production write. The e2e spec beside this file proves it against a real Dispatch server:
 * a write through the proxy leaves the server unchanged, and each modelled answer, and each read
 * after it, is what the server gives once the same writes really land.
 *
 * Usage: bun proxy.ts --upstream <dispatch base url> --token-file <file> --writes <jsonl>
 *                     [--port <n>] [--exclude-key <KEY>]...
 * Prints `listening on http://127.0.0.1:<port>` once the socket is open. `--port 0` (the default)
 * picks a free port.
 *
 * Each JSONL line is one request: `{seq, at, kind: "read" | "write" | "refused", route, method,
 * path, query, status}`, where `route` names the `READS` or `WRITES` entry that served it; a
 * write adds `body` (the parsed request body; an uploaded file as `{name, size, type}`) and
 * `response` (what the agent was answered), and a failure adds `error`.
 */
import { appendFile } from "node:fs/promises";
import type { Actor, ActorOrigin, WhoamiResponse } from "@legion/contracts";

import {
  assertNothingExcluded,
  ExcludedIssueLeak,
  ISSUE_KEY,
  isRecord,
  refNamesKey,
  textNamesKey,
  withoutExcluded,
} from "./exclude";
import {
  type Answer,
  currentArtifact,
  currentAsk,
  currentComment,
  notFound,
  Overlay,
  type Params,
  type ReadContext,
  type ReadRoute,
  Refusal,
  refused,
  type Upstream,
  unmodelledWrite,
  type WriteRoute,
} from "./overlay";
import { READS, type ReadName } from "./reads";
import { WRITES, type WriteName } from "./writes";

interface Options {
  /** Dispatch base URL, e.g. `https://dispatch.example`; never a literal in this repository. */
  readonly upstream: string;
  /** The bearer sent upstream. */
  readonly token: string;
  /** The JSONL file every request is appended to. */
  readonly writesFile: string;
  /** Issue keys hidden from the agent. */
  readonly excludeKeys: readonly string[];
  readonly port: number;
}

/** The upstream did not answer at all. */
class UpstreamFailed extends Error {}

interface Template {
  readonly segments: readonly string[];
  /** How many segments are literal: where two templates match, the more literal one wins, as
   *  the server's mux prefers the more specific pattern (`issues/resolve` over `issues/:key`). */
  readonly literals: number;
}

function template(path: string): Template {
  const segments = path === "" ? [] : path.split("/");
  return { segments, literals: segments.filter((segment) => !segment.startsWith(":")).length };
}

function match(path: Template, segments: readonly string[]): Params | undefined {
  if (path.segments.length !== segments.length) return undefined;
  const params: Record<string, string> = {};
  for (const [index, part] of path.segments.entries()) {
    const segment = segments[index] ?? "";
    if (part.startsWith(":")) params[part.slice(1)] = segment;
    else if (part !== segment) return undefined;
  }
  return params;
}

interface Entry<Name, Route> {
  readonly name: Name;
  readonly route: Route;
  readonly path: Template;
}

function find<Name, Route>(
  table: readonly Entry<Name, Route>[],
  segments: readonly string[]
): (Entry<Name, Route> & { params: Params }) | undefined {
  let best: (Entry<Name, Route> & { params: Params }) | undefined;
  for (const entry of table) {
    const params = match(entry.path, segments);
    if (params && (!best || entry.path.literals > best.path.literals)) best = { ...entry, params };
  }
  return best;
}

/** A table's entries with their names typed: `Object.entries` widens a key to `string`. */
function entries<Name extends string, Route>(table: Record<Name, Route>): [Name, Route][] {
  return Object.entries(table) as [Name, Route][];
}

const readTable: Entry<ReadName, ReadRoute>[] = entries<ReadName, ReadRoute>(READS).map(
  ([name, route]) => ({ name, route, path: template(route.path) })
);
const writeTable: Entry<WriteName, WriteRoute>[] = entries<WriteName, WriteRoute>(WRITES).flatMap(
  ([name, route]) => route.paths.map((path) => ({ name, route, path: template(path) }))
);

const WRITE_METHODS = ["POST", "PATCH", "PUT", "DELETE"];
const ORIGIN_FIELDS = ["host", "machine", "cwd", "tmux", "pane", "session_title"] as const;

/** A request body as the handlers read it, and as the JSONL records it (a file by its
 *  name, size and type). */
async function readBody(
  request: Request
): Promise<{ input: Record<string, unknown>; recorded: unknown }> {
  const type = request.headers.get("content-type") ?? "";
  if (type.includes("multipart/form-data")) {
    const input: Record<string, unknown> = {};
    const recorded: Record<string, unknown> = {};
    // Bun's `FormData` types `entries()` as strings only; `forEach` carries files too.
    (await request.formData()).forEach((value, name) => {
      if (typeof value === "string") {
        input[name] = name === "actor" ? parseJson(value) : value;
        recorded[name] = input[name];
      } else {
        input[name] = value;
        recorded[name] = { name: value.name, size: value.size, type: value.type };
      }
    });
    return { input, recorded };
  }
  const text = await request.text();
  const body = text === "" ? undefined : parseJson(text);
  return { input: isRecord(body) ? body : {}, recorded: body };
}

function parseJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

function startEvalProxy(options: Options): string {
  const excluded = new Set(options.excludeKeys);
  const overlay = new Overlay();
  const base = options.upstream.replace(/\/+$/, "");
  let seq = 0;

  const record = async (line: Record<string, unknown>) => {
    seq += 1;
    await appendFile(
      options.writesFile,
      `${JSON.stringify({ seq, at: new Date().toISOString(), ...line })}\n`
    );
  };

  const fetchUpstream = async (path: string, query: string): Promise<Response> => {
    try {
      return await fetch(`${base}${path}${query}`, {
        headers: { Accept: "application/json", Authorization: `Bearer ${options.token}` },
      });
    } catch (error) {
      throw new UpstreamFailed(
        `the evaluation proxy could not reach Dispatch at ${base}: ${error instanceof Error ? error.message : String(error)}`
      );
    }
  };

  const upstream: Upstream = {
    json: async <T>(path: string, query = "") => {
      const response = await fetchUpstream(path, query);
      // The upstream is Dispatch itself: a 2xx answer is the route's contract type.
      return response.ok ? ((await response.json()) as T) : undefined;
    },
  };

  let whoami: Promise<WhoamiResponse | undefined> | undefined;
  /** The session a write names, as the server records it: the body's session with what the
   *  bearer proved (a personal token's owner, a service token's subject) attached. */
  const sessionActor = async (input: Readonly<Record<string, unknown>>): Promise<Actor> => {
    const supplied = input.actor;
    if (
      !isRecord(supplied) ||
      supplied.kind !== "session" ||
      typeof supplied.id !== "string" ||
      supplied.id.trim() === ""
    ) {
      throw new Refusal(refused(400, "ACTOR_KIND", "bearer callers require actor.kind session"));
    }
    whoami ??= upstream.json<WhoamiResponse>("/api/v1/whoami");
    const who = await whoami;
    const origin: ActorOrigin = Object.fromEntries(
      ORIGIN_FIELDS.flatMap((field) => {
        const value = isRecord(supplied.origin) ? supplied.origin[field] : undefined;
        return typeof value === "string" && value !== "" ? [[field, value]] : [];
      })
    );
    return {
      kind: "session",
      id: supplied.id,
      ...(Object.keys(origin).length > 0 ? { origin } : {}),
      ...(who?.kind === "agent" && who.owner !== null ? { owner: who.owner } : {}),
      ...(who?.kind === "agent" && who.service !== null ? { service: who.service } : {}),
    };
  };

  const isExcluded = (key: string | null | undefined) => key != null && excluded.has(key);

  /** Whether a route's address names an excluded issue, or something it owns. */
  const addressesExcluded = async (context: ReadContext): Promise<boolean> => {
    if (excluded.size === 0) return false;
    const { params, query } = context;
    if (isExcluded(params.key)) return true;
    for (const name of ["to", "from", "ref", "parent"]) {
      const ref = query.get(name);
      if (ref !== null && [...excluded].some((key) => refNamesKey(ref, key))) return true;
    }
    if (params.artifact !== undefined) {
      return isExcluded((await currentArtifact(context, params.artifact))?.issue_key);
    }
    if (params.ask !== undefined)
      return isExcluded((await currentAsk(context, params.ask))?.issue_key);
    if (params.comment !== undefined) {
      return isExcluded((await currentComment(context, params.comment))?.issue_key);
    }
    if (params.message !== undefined) {
      const thread = await upstream.json<{ message: { issue_key: string | null } }>(
        `/api/v1/messages/${encodeURIComponent(params.message)}`
      );
      return isExcluded(thread?.message.issue_key);
    }
    return false;
  };

  const withheld = (leak: ExcludedIssueLeak): Answer =>
    refused(
      502,
      "EVAL_PROXY_WITHHELD",
      `the evaluation proxy withheld this answer: ${leak.message}`
    );

  /** Filters an answer for the excluded issues, records it, and serves it. */
  const answer = async (
    line: Record<string, unknown>,
    work: () => Promise<Answer | Response>,
    route?: ReadRoute
  ): Promise<Response> => {
    let result: Answer | Response;
    try {
      result = await work();
    } catch (error) {
      if (error instanceof Refusal) result = error.answer;
      else if (error instanceof ExcludedIssueLeak) result = withheld(error);
      else if (error instanceof UpstreamFailed) {
        result = refused(502, "EVAL_PROXY_UPSTREAM_FAILED", error.message);
      } else {
        const message = error instanceof Error ? error.message : String(error);
        console.error(`eval-proxy: ${line.method} ${line.path}: ${message}`);
        result = refused(500, "EVAL_PROXY_FAILED", `the evaluation proxy failed: ${message}`);
      }
    }
    if (result instanceof Response) {
      await record({ ...line, status: result.status });
      return result;
    }
    let body = withoutExcluded(result.body, excluded);
    if (excluded.size > 0 && route?.recount) body = route.recount(body);
    let status = result.status;
    try {
      assertNothingExcluded(body, excluded);
    } catch (error) {
      if (!(error instanceof ExcludedIssueLeak)) throw error;
      ({ status, body } = withheld(error));
    }
    const failed = status >= 500 && isRecord(body) ? { error: body.error } : {};
    await record({
      ...line,
      status,
      ...(line.kind === "write" ? { response: body } : {}),
      ...failed,
    });
    return status === 204 ? new Response(null, { status }) : Response.json(body, { status });
  };

  const refuse = (method: string, url: URL, status: number, code: string, error: string) =>
    answer({ kind: "refused", method, path: url.pathname, query: url.search }, async () =>
      refused(status, code, error)
    );

  const read = (url: URL, segments: readonly string[]): Promise<Response> => {
    const found = find(readTable, segments);
    if (!found) {
      return refuse(
        "GET",
        url,
        403,
        "EVAL_PROXY_REFUSED",
        `the evaluation proxy forwards only its GET allow-list; GET ${url.pathname} is not on it`
      );
    }
    const { route, params } = found;
    const context: ReadContext = { overlay, upstream, params, query: url.searchParams };
    const line = {
      kind: "read",
      route: found.name,
      method: "GET",
      path: url.pathname,
      query: url.search,
    };
    return answer(
      line,
      async () => {
        if (await addressesExcluded(context)) return notFound();
        const own = await route.own?.(context);
        if (own) return own;
        const response = await fetchUpstream(url.pathname, url.search);
        const type = response.headers.get("content-type") ?? "";
        if (!response.ok || !type.includes("application/json")) {
          const text = await response.text();
          // A refusal is free text, and the server's name the issues they concern.
          if ([...excluded].some((key) => textNamesKey(text, key))) {
            throw new ExcludedIssueLeak("an upstream refusal");
          }
          return new Response(text, { status: response.status, headers: { "content-type": type } });
        }
        const body: unknown = await response.json();
        if (isExcluded(route.owner?.(body))) return notFound();
        return {
          status: response.status,
          body: route.merge ? await route.merge(context, body) : body,
        };
      },
      route
    );
  };

  const write = async (
    request: Request,
    url: URL,
    segments: readonly string[]
  ): Promise<Response> => {
    const { input, recorded } = await readBody(request);
    const found = find(
      writeTable.filter((entry) => entry.route.method === request.method),
      segments
    );
    const line = {
      kind: "write",
      route: found?.name ?? null,
      method: request.method,
      path: url.pathname,
      query: url.search,
      body: recorded,
    };
    return answer(line, async () => {
      if (!found) {
        return unmodelledWrite(
          `recorded ${request.method} ${url.pathname} but does not model its answer`
        );
      }
      const context = { overlay, upstream, params: found.params, query: url.searchParams };
      if (await addressesExcluded(context)) return notFound();
      return found.route.handle({ ...context, input, actor: await sessionActor(input) });
    });
  };

  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: options.port,
    async fetch(request) {
      const url = new URL(request.url);
      if (url.pathname !== "/api/v1" && !url.pathname.startsWith("/api/v1/")) {
        return refuse(
          request.method,
          url,
          403,
          "EVAL_PROXY_REFUSED",
          `${url.pathname} is not a Dispatch API route`
        );
      }
      let segments: string[];
      try {
        segments = url.pathname
          .slice("/api/v1".length)
          .split("/")
          .filter((segment) => segment !== "")
          .map((segment) => decodeURIComponent(segment));
      } catch {
        return refuse(
          request.method,
          url,
          400,
          "EVAL_PROXY_BAD_PATH",
          `${url.pathname} has a malformed escape`
        );
      }
      if (request.method === "GET") return read(url, segments);
      if (WRITE_METHODS.includes(request.method)) return write(request, url, segments);
      return refuse(
        request.method,
        url,
        403,
        "EVAL_PROXY_REFUSED",
        `the evaluation proxy refuses ${request.method}`
      );
    },
    // Nothing above lets an error escape; if one does, the agent still gets JSON, not Bun's
    // development page with the proxy's source in it.
    error(error) {
      return Response.json({ code: "EVAL_PROXY_FAILED", error: error.message }, { status: 500 });
    },
  });
  return `http://127.0.0.1:${server.port}`;
}

interface Arguments {
  upstream?: string;
  tokenFile?: string;
  writesFile?: string;
  excludeKeys: string[];
  port: number;
}

function parseArguments(argv: readonly string[]): Arguments {
  const parsed: Arguments = { port: 0, excludeKeys: [] };
  for (let index = 0; index < argv.length; index += 2) {
    const flag = argv[index];
    const value = argv[index + 1];
    if (value === undefined || value.startsWith("--")) throw new Error(`${flag} needs a value`);
    switch (flag) {
      case "--upstream":
        parsed.upstream = value;
        break;
      case "--token-file":
        parsed.tokenFile = value;
        break;
      case "--writes":
        parsed.writesFile = value;
        break;
      case "--port":
        if (!/^\d+$/.test(value) || Number(value) > 65535)
          throw new Error(`--port ${value} is not a port`);
        parsed.port = Number(value);
        break;
      case "--exclude-key":
        if (!ISSUE_KEY.test(value)) throw new Error(`--exclude-key ${value} is not an issue key`);
        parsed.excludeKeys.push(value);
        break;
      default:
        throw new Error(`unknown flag ${flag}`);
    }
  }
  return parsed;
}

if (import.meta.main) {
  try {
    const cli = parseArguments(Bun.argv.slice(2));
    if (cli.upstream === undefined) {
      throw new Error("--upstream is required: the Dispatch base URL reads go to");
    }
    if (!/^https?:\/\/[^/]/.test(cli.upstream)) {
      throw new Error(`--upstream ${cli.upstream} is not an http(s) URL`);
    }
    if (cli.tokenFile === undefined) {
      throw new Error("--token-file is required: the file holding the upstream bearer");
    }
    if (cli.writesFile === undefined) {
      throw new Error("--writes is required: the JSONL file every request is recorded to");
    }
    const token = (await Bun.file(cli.tokenFile).text()).trim();
    if (token === "") throw new Error(`--token-file ${cli.tokenFile} is empty`);
    const url = startEvalProxy({
      upstream: cli.upstream,
      token,
      writesFile: cli.writesFile,
      excludeKeys: cli.excludeKeys,
      port: cli.port,
    });
    console.log(`listening on ${url}`);
  } catch (error) {
    console.error(`eval-proxy: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(2);
  }
}
