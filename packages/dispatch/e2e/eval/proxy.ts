#!/usr/bin/env bun
/**
 * The Dispatch evaluation proxy: what an agent under evaluation gets as its DISPATCH_URL, so it
 * reads a real Dispatch and writes nothing to it.
 *
 * - A GET on the allow-list below (the routes `DispatchClient` in
 *   `packages/envoy-client/src/dispatch-http.ts` calls, which is every route the Dispatch tools
 *   and the open-ask snapshot read) is forwarded to `--upstream` with the `--token-file` bearer.
 *   The agent's own `Authorization` is never forwarded.
 * - Every other GET, and any other method not listed next, is refused with `403
 *   EVAL_PROXY_REFUSED` and recorded.
 * - Every POST, PATCH, PUT and DELETE is recorded to the `--writes` JSONL file and never sent. The
 *   proxy answers it with a body in the route's response shape (`@legion/contracts`), and serves
 *   what it recorded back to later reads: a created issue, comment, ask, message or document is
 *   readable by its key or id, is listed under its issue, and appears in the issue's events; a
 *   patched issue reads with the patch applied. A write to a route the proxy does not model is
 *   recorded and answered `501 EVAL_PROXY_UNMODELLED_WRITE`, never a guessed success.
 * - `--exclude-key <KEY>` (repeatable) hides an issue: every route addressed by that key, or by an
 *   ask, comment, message or document it owns, answers `404 NOT_FOUND`, and every list element it
 *   owns is dropped from every response (list rows, children, search hits, open asks, reference
 *   edges and members); a `parent` naming it reads null. Mentions of the key inside other items'
 *   text are left as they are.
 *
 * Dispatch has no read-only token, so this allow-list is the only barrier between the agent and
 * a production write. The e2e spec beside this file proves both halves against a real Dispatch
 * server: reads match the server's, and a write through the proxy leaves the server unchanged.
 *
 * Usage: bun proxy.ts --upstream <dispatch base url> --token-file <file> --writes <jsonl>
 *                     [--port <n>] [--exclude-key <KEY>]...
 * Prints `listening on http://127.0.0.1:<port>` once the socket is open. `--port 0` (the default)
 * picks a free port.
 *
 * Each JSONL line is one request: `{seq, at, kind: "read" | "write" | "refused", method, path,
 * query, status}`, and a write adds `body` (the parsed request body; an uploaded file as
 * `{name, size, type}`) and `response` (what the agent was answered).
 */
import { randomUUID } from "node:crypto";
import { appendFile } from "node:fs/promises";
import type {
  Actor,
  Advised,
  Artifact,
  ArtifactUploadResponse,
  Ask,
  AskRead,
  Comment,
  CommentRead,
  EditArtifactResponse,
  Event,
  Issue,
  IssueDetails,
  IssueSummary,
  Message,
  MessageRead,
  Version,
} from "@legion/contracts";

export interface EvalProxyOptions {
  /** Dispatch base URL, e.g. `https://dispatch.example`; never a literal in this repository. */
  readonly upstream: string;
  /** The bearer sent upstream. */
  readonly token: string;
  /** The JSONL file every request is appended to. */
  readonly writesFile: string;
  /** Issue keys hidden from the agent. */
  readonly excludeKeys?: readonly string[];
  readonly port?: number;
  readonly fetch?: typeof fetch;
}

export interface EvalProxy {
  readonly url: string;
  readonly port: number;
  stop(): Promise<void>;
}

type Params = Readonly<Record<string, string>>;

interface ReadRoute {
  readonly name: string;
  readonly path: readonly string[];
}

/**
 * The GET allow-list, as path templates (`:name` captures one segment). Order matters where two
 * templates match one path: `issues/resolve` precedes `issues/:key`.
 */
export const READ_ROUTES: readonly ReadRoute[] = [
  { name: "whoami", path: ["whoami"] },
  { name: "agents", path: ["agents"] },
  { name: "search", path: ["search"] },
  { name: "references", path: ["references"] },
  { name: "open-asks", path: ["asks", "open"] },
  { name: "ask", path: ["asks", ":ask"] },
  { name: "comment", path: ["comments", ":comment"] },
  { name: "message-thread", path: ["messages", ":message"] },
  { name: "issue-list", path: ["issues"] },
  { name: "issue-resolve", path: ["issues", "resolve"] },
  { name: "issue", path: ["issues", ":key"] },
  { name: "issue-events", path: ["issues", ":key", "events"] },
  { name: "issue-asks", path: ["issues", ":key", "asks"] },
  { name: "issue-comments", path: ["issues", ":key", "comments"] },
  { name: "issue-references", path: ["issues", ":key", "references"] },
  { name: "issue-message", path: ["issues", ":key", "messages", ":message"] },
  { name: "artifact", path: ["artifacts", ":artifact"] },
  { name: "artifact-text", path: ["artifacts", ":artifact", "text"] },
  { name: "artifact-version", path: ["artifacts", ":artifact", "versions", ":version"] },
  { name: "artifact-blocks", path: ["artifacts", ":artifact", "blocks"] },
  { name: "artifact-events", path: ["artifacts", ":artifact", "events"] },
  { name: "artifact-references", path: ["artifacts", ":artifact", "references"] },
  { name: "artifact-asks", path: ["artifacts", ":artifact", "asks"] },
  { name: "artifact-comments", path: ["artifacts", ":artifact", "comments"] },
  { name: "project-artifacts", path: ["projects", ":project", "artifacts"] },
  { name: "project-artifact", path: ["projects", ":project", "artifacts", ":slug"] },
  { name: "architecture-source", path: ["projects", ":project", "architecture-source"] },
];

/** Every write route the Dispatch tools call, keyed `METHOD path-template`. */
const WRITE_ROUTES = [
  ["POST", ["issues"], "create-issue"],
  ["PATCH", ["issues", ":key"], "update-issue"],
  ["POST", ["issues", ":key", "claim"], "claim"],
  ["DELETE", ["issues", ":key", "claim"], "release"],
  ["POST", ["issues", ":key", "asks"], "issue-ask"],
  ["POST", ["issues", ":key", "comments"], "issue-comment"],
  ["POST", ["issues", ":key", "messages"], "issue-message"],
  ["POST", ["issues", ":key", "artifacts"], "issue-artifact"],
  ["POST", ["projects", ":project", "artifacts"], "project-artifact"],
  ["POST", ["projects", ":project", "architecture-source", "sync"], "architecture-sync"],
  ["POST", ["messages", ":message", "reply"], "message-reply"],
  ["POST", ["asks", ":ask", "resolve"], "resolve-ask"],
  ["PATCH", ["asks", ":ask"], "edit-ask"],
  ["PUT", ["asks", ":ask", "followers", ":session"], "follow"],
  ["DELETE", ["asks", ":ask", "followers", ":session"], "unfollow"],
  ["POST", ["artifacts", ":artifact", "asks"], "artifact-ask"],
  ["POST", ["artifacts", ":artifact", "comments"], "artifact-comment"],
  ["POST", ["artifacts", ":artifact", "edits"], "artifact-edit"],
  ["POST", ["artifacts", ":artifact", "versions"], "artifact-version"],
  ["POST", ["artifacts", ":artifact", "approval-requests"], "approval-request"],
  ["POST", ["comments", ":comment", "resolve"], "resolve-comment"],
] as const satisfies readonly (readonly [string, readonly string[], string])[];

const WRITE_METHODS: Readonly<Record<string, true>> = {
  POST: true,
  PATCH: true,
  PUT: true,
  DELETE: true,
};
/** A Dispatch issue key; its characters need no escaping inside a regular expression. */
const ISSUE_KEY = /^[A-Z][A-Z0-9]*-\d+$/;
/** Keys the proxy gives the issues it fakes: past any real issue number. */
const FAKE_ISSUE_NUMBER_BASE = 900_000;
/** Event ids the proxy gives the events it fakes: past any real event id. */
const FAKE_EVENT_ID_BASE = 9_000_000_000;

function match(template: readonly string[], segments: readonly string[]): Params | undefined {
  if (template.length !== segments.length) return undefined;
  const params: Record<string, string> = {};
  for (const [index, part] of template.entries()) {
    const segment = segments[index] ?? "";
    if (part.startsWith(":")) params[part.slice(1)] = segment;
    else if (part !== segment) return undefined;
  }
  return params;
}

function errorResponse(status: number, code: string, error: string): Response {
  return Response.json({ code, error }, { status });
}

const notFoundBody = { code: "NOT_FOUND", error: "not found" };
const notFound = () => Response.json(notFoundBody, { status: 404 });

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function stringField(value: unknown, key: string): string | undefined {
  if (!isRecord(value)) return undefined;
  const field = value[key];
  return typeof field === "string" ? field : undefined;
}

/** `dispatch://KEY`, `dispatch://KEY/...`, `/issues/KEY`, `/issues/KEY/...` or `...?...`. */
function refNamesKey(ref: string, key: string): boolean {
  return new RegExp(`(^|dispatch://|/issues/)${key}($|[/?#])`).test(ref);
}

/**
 * Whether one list element belongs to an excluded issue, by the fields Dispatch's list shapes
 * carry: an issue row or child (`key`), anything issue-owned (`issue_key`), a search hit or
 * open ask (`owner`), a legacy search hit or ask (`issue`), a reference edge (`node`), a
 * reference member or outgoing reference (`artifact`), and any deep link (`ref`, `href`).
 */
function ownedByExcluded(element: unknown, excluded: ReadonlySet<string>): boolean {
  if (!isRecord(element)) return false;
  const owns = (key: string | undefined) => key !== undefined && excluded.has(key);
  if (owns(stringField(element, "key")) || owns(stringField(element, "issue_key"))) return true;
  const owner = element.owner;
  if (isRecord(owner)) {
    if (owner.kind === "issue" && owns(stringField(owner, "key"))) return true;
    if (owns(stringField(owner.issue, "key"))) return true;
  }
  if (owns(stringField(element.issue, "key"))) return true;
  const node = element.node;
  if (isRecord(node)) {
    if (owns(stringField(node, "issue_key"))) return true;
    if (node.kind === "issue" && owns(stringField(node, "id"))) return true;
  }
  if (owns(stringField(element.artifact, "issue_key"))) return true;
  for (const link of [stringField(element, "ref"), stringField(element, "href")]) {
    if (link !== undefined && [...excluded].some((key) => refNamesKey(link, key))) return true;
  }
  return false;
}

/** Drops every array element an excluded issue owns, at any depth, and nulls a `parent` naming one. */
export function withoutExcluded(value: unknown, excluded: ReadonlySet<string>): unknown {
  if (excluded.size === 0) return value;
  if (Array.isArray(value)) {
    return value
      .filter((element) => !ownedByExcluded(element, excluded))
      .map((element) => withoutExcluded(element, excluded));
  }
  if (!isRecord(value)) return value;
  const result: Record<string, unknown> = {};
  for (const [field, inner] of Object.entries(value)) {
    result[field] =
      field === "parent" && typeof inner === "string" && excluded.has(inner)
        ? null
        : withoutExcluded(inner, excluded);
  }
  return result;
}

interface StoredArtifact {
  artifact: Artifact;
  markdown: string;
}

/** What the agent wrote, served back to it. */
class Overlay {
  readonly issues = new Map<string, IssueDetails>();
  readonly patches = new Map<string, Record<string, unknown>>();
  readonly comments = new Map<string, Comment>();
  readonly asks = new Map<string, Ask>();
  readonly messages = new Map<string, Message>();
  readonly artifacts = new Map<string, StoredArtifact>();
  readonly events = new Map<string, Event[]>();
  /** The upstream `last_seq` of each issue the agent wrote to, read once at its first write. */
  readonly baseSeq = new Map<string, number>();
  issueCount = 0;
  eventCount = 0;

  commentsOn(key: string): Comment[] {
    return [...this.comments.values()].filter((comment) => comment.issue_key === key);
  }

  /** An issue's asks as a read shows them: only an open ask's read carries `waiting_on`, and a
   * create answers without it, as the server's do. */
  asksOn(key: string): Ask[] {
    return [...this.asks.values()]
      .filter((ask) => ask.issue_key === key)
      .map((ask) => (ask.state === "open" ? { ...ask, waiting_on: "human" } : ask));
  }

  artifactsOn(key: string): Artifact[] {
    return [...this.artifacts.values()]
      .map((stored) => stored.artifact)
      .filter((artifact) => artifact.issue_key === key);
  }

  lastSeq(key: string): number {
    const events = this.events.get(key) ?? [];
    return events.at(-1)?.seq ?? this.baseSeq.get(key) ?? 0;
  }
}

function sessionActor(body: unknown): Actor {
  const actor = isRecord(body) ? body.actor : undefined;
  if (isRecord(actor) && typeof actor.kind === "string" && typeof actor.id === "string") {
    return actor as unknown as Actor;
  }
  return { kind: "session", id: "eval-proxy" };
}

function now(): string {
  return new Date().toISOString();
}

function version(number: number, author: Actor, summary: string | null): Version {
  return { number, named: false, summary, authors: [author], created_at: now() };
}

/** The fields an issue read adds to the issue; a create, update or claim answers without them. */
type IssueReadOnlyField =
  | "artifacts"
  | "open_asks"
  | "children"
  | "referenced_by_count"
  | "route_status"
  | "route_holder";

function issueOf<T extends Partial<Record<IssueReadOnlyField, unknown>>>(
  details: T
): Omit<T, IssueReadOnlyField> {
  const {
    artifacts: _a,
    open_asks: _o,
    children: _c,
    referenced_by_count: _r,
    route_status: _s,
    route_holder: _h,
    ...issue
  } = details;
  return issue;
}

async function readBody(request: Request): Promise<unknown> {
  const type = request.headers.get("content-type") ?? "";
  if (type.includes("multipart/form-data")) {
    const form = await request.formData();
    const fields: Record<string, unknown> = {};
    for (const [name, value] of form.entries()) {
      if (typeof value === "string") {
        fields[name] = name === "actor" ? parseJson(value) : value;
      } else {
        const file = value as unknown as File;
        fields[name] = { name: file.name, size: file.size, type: file.type };
      }
    }
    return fields;
  }
  const text = await request.text();
  return text === "" ? undefined : parseJson(text);
}

function parseJson(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

export async function startEvalProxy(options: EvalProxyOptions): Promise<EvalProxy> {
  const upstream = options.upstream.replace(/\/+$/, "");
  const excluded = new Set(options.excludeKeys ?? []);
  for (const key of excluded) {
    if (!ISSUE_KEY.test(key)) throw new Error(`excluded key ${key} is not an issue key`);
  }
  const fetchImpl = options.fetch ?? fetch;
  const overlay = new Overlay();
  const artifactOwners = new Map<string, string | null>();
  let seq = 0;

  const record = async (line: Record<string, unknown>) => {
    seq += 1;
    await appendFile(options.writesFile, `${JSON.stringify({ seq, at: now(), ...line })}\n`);
  };

  const refuse = async (method: string, url: URL, error: string): Promise<Response> => {
    await record({ kind: "refused", method, path: url.pathname, query: url.search, status: 403 });
    return errorResponse(403, "EVAL_PROXY_REFUSED", error);
  };

  const upstreamGet = async (path: string, query = ""): Promise<Response> =>
    fetchImpl(`${upstream}${path}${query}`, {
      headers: { Accept: "application/json", Authorization: `Bearer ${options.token}` },
    });

  const upstreamJson = async (path: string): Promise<unknown | undefined> => {
    const response = await upstreamGet(path);
    return response.ok ? response.json() : undefined;
  };

  /** The issue a document belongs to, read once from upstream (null for a project document). */
  const artifactOwner = async (id: string): Promise<string | null | undefined> => {
    const stored = overlay.artifacts.get(id);
    if (stored) return stored.artifact.issue_key;
    if (artifactOwners.has(id)) return artifactOwners.get(id);
    const artifact = await upstreamJson(`/api/v1/artifacts/${encodeURIComponent(id)}`);
    const owner = isRecord(artifact) ? ((artifact.issue_key as string | null) ?? null) : undefined;
    if (owner !== undefined) artifactOwners.set(id, owner);
    return owner;
  };

  const isExcluded = (key: string | null | undefined) =>
    key !== undefined && key !== null && excluded.has(key);

  /** Whether a route addressed by these params names an excluded issue or something it owns. */
  const addressesExcluded = async (params: Params, url: URL): Promise<boolean> => {
    if (isExcluded(params.key)) return true;
    if (params.artifact !== undefined && isExcluded(await artifactOwner(params.artifact))) {
      return true;
    }
    for (const name of ["to", "from", "ref"]) {
      const ref = url.searchParams.get(name);
      if (ref !== null && [...excluded].some((key) => refNamesKey(ref, key))) return true;
    }
    return false;
  };

  const appendEvent = (
    key: string,
    type: string,
    actor: Actor,
    payload: unknown,
    artifactId: string | null = null
  ) => {
    overlay.eventCount += 1;
    const events = overlay.events.get(key) ?? [];
    events.push({
      id: FAKE_EVENT_ID_BASE + overlay.eventCount,
      issue_key: key,
      artifact_id: artifactId,
      project: key.replace(/-\d+$/, ""),
      seq: overlay.lastSeq(key) + 1,
      type,
      actor,
      notify: false,
      created_at: now(),
      payload,
    } as unknown as Event);
    overlay.events.set(key, events);
  };

  /** Reads an issue's upstream `last_seq` before the first write to it, so faked events follow. */
  const noteIssue = async (key: string): Promise<void> => {
    if (overlay.baseSeq.has(key) || overlay.issues.has(key)) return;
    const issue = await upstreamJson(`/api/v1/issues/${encodeURIComponent(key)}`);
    overlay.baseSeq.set(
      key,
      isRecord(issue) && typeof issue.last_seq === "number" ? issue.last_seq : 0
    );
  };

  const withOverlay = (name: string, params: Params, url: URL, body: unknown): unknown => {
    const key = params.key;
    switch (name) {
      case "issue": {
        if (key === undefined || !isRecord(body)) return body;
        const patch = overlay.patches.get(key) ?? {};
        const children = [...overlay.issues.values()]
          .filter((issue) => issue.parent === key)
          .map((issue) => ({
            key: issue.key,
            title: issue.title,
            status: issue.status,
            subtree_done: 0,
            subtree_total: 1,
            active_at: issue.updated_at,
            external_links: [],
          }));
        return {
          ...body,
          ...patch,
          artifacts: [...((body.artifacts as unknown[]) ?? []), ...overlay.artifactsOn(key)],
          open_asks: [
            ...((body.open_asks as unknown[]) ?? []),
            ...overlay.asksOn(key).filter((ask) => ask.state === "open"),
          ],
          children: [...((body.children as unknown[]) ?? []), ...children],
          last_seq: Math.max(Number(body.last_seq ?? 0), overlay.lastSeq(key)),
        };
      }
      case "issue-events": {
        if (key === undefined || !Array.isArray(body)) return body;
        const after = Number(url.searchParams.get("after") ?? 0);
        const limit = Number(url.searchParams.get("limit") ?? 200);
        const faked = (overlay.events.get(key) ?? []).filter((event) => event.seq > after);
        return [...body, ...faked].sort((a, b) => a.seq - b.seq).slice(0, limit);
      }
      case "issue-comments": {
        if (key === undefined || !Array.isArray(body)) return body;
        const artifact = url.searchParams.get("artifact");
        const own = overlay
          .commentsOn(key)
          .filter((comment) => artifact === null || comment.artifact_id === artifact);
        return [...body, ...own];
      }
      case "issue-asks":
        return key === undefined || !Array.isArray(body) ? body : [...body, ...overlay.asksOn(key)];
      case "issue-list": {
        if (!Array.isArray(body)) return body;
        const project = url.searchParams.get("project");
        const own: IssueSummary[] = [...overlay.issues.values()]
          .filter((issue) => project === null || issue.project === project)
          .map((issue) => ({
            key: issue.key,
            title: issue.title,
            status: issue.status,
            priority: issue.priority,
            rank: issue.rank,
            parent: issue.parent,
            assignee: issue.assignee,
            claim: issue.claim,
            components: issue.components,
            route: issue.route,
            updated_at: issue.updated_at,
            last_seq: overlay.lastSeq(issue.key),
            route_status: null,
            route_holder: null,
            labels: issue.labels,
            open_asks: 0,
          }));
        return [...body, ...own];
      }
      default:
        return body;
    }
  };

  /** A read the overlay answers alone: something the agent created. */
  const overlayRead = (name: string, params: Params): unknown | undefined => {
    switch (name) {
      case "issue": {
        const issue = params.key === undefined ? undefined : overlay.issues.get(params.key);
        if (!issue) return undefined;
        return withOverlay("issue", params, new URL("http://overlay/"), {
          ...issue,
          artifacts: [],
          open_asks: [],
          children: [],
        });
      }
      case "issue-events":
      case "issue-comments":
      case "issue-asks":
        if (params.key === undefined || !overlay.issues.has(params.key)) return undefined;
        return withOverlay(name, params, new URL("http://overlay/"), []);
      case "comment": {
        const comment = overlay.comments.get(params.comment ?? "");
        if (!comment) return undefined;
        const replies = [...overlay.comments.values()].filter((c) => c.reply_to === comment.id);
        return { comment, replies } satisfies CommentRead;
      }
      case "ask": {
        const ask = overlay.asks.get(params.ask ?? "");
        if (!ask) return undefined;
        const replies = [...overlay.comments.values()].filter((c) => c.ask_id === ask.id);
        const read: Ask = ask.state === "open" ? { ...ask, waiting_on: "human" } : ask;
        return { ask: read, replies, edits: [], followers: [] } satisfies AskRead;
      }
      case "message-thread":
      case "issue-message": {
        const message = overlay.messages.get(params.message ?? "");
        if (!message) return undefined;
        const replies = [...overlay.messages.values()].filter((m) => m.in_reply_to === message.id);
        return { message, replies } satisfies MessageRead;
      }
      case "artifact":
        return overlay.artifacts.get(params.artifact ?? "")?.artifact;
      case "artifact-text": {
        const stored = overlay.artifacts.get(params.artifact ?? "");
        if (!stored) return undefined;
        return { markdown: stored.markdown, version: stored.artifact.versions.at(-1)?.number ?? 1 };
      }
      default:
        return undefined;
    }
  };

  const read = async (url: URL, segments: readonly string[]): Promise<Response> => {
    const found = READ_ROUTES.map((route) => ({ route, params: match(route.path, segments) })).find(
      (candidate) => candidate.params !== undefined
    );
    if (!found?.params) {
      return refuse(
        "GET",
        url,
        `the evaluation proxy forwards only its GET allow-list; GET ${url.pathname} is not on it`
      );
    }
    const { route, params } = found;
    const answer = async (response: Response) => {
      await record({
        kind: "read",
        method: "GET",
        path: url.pathname,
        query: url.search,
        status: response.status,
      });
      return response;
    };
    if (await addressesExcluded(params, url)) return answer(notFound());
    const own = overlayRead(route.name, params);
    if (own !== undefined) return answer(Response.json(own));

    const response = await upstreamGet(url.pathname, url.search);
    const type = response.headers.get("content-type") ?? "";
    if (!response.ok || !type.includes("application/json")) {
      return answer(
        new Response(await response.arrayBuffer(), {
          status: response.status,
          headers: { "content-type": type },
        })
      );
    }
    const body: unknown = await response.json();
    // A route addressed by an ask, comment or message id: the owner is in the body.
    const entity = isRecord(body) ? (body.ask ?? body.comment ?? body.message ?? body) : undefined;
    if (isExcluded(stringField(entity, "issue_key"))) return answer(notFound());
    if (route.name === "issue-resolve" && isExcluded(stringField(body, "key"))) {
      return answer(notFound());
    }
    return answer(
      Response.json(withoutExcluded(withOverlay(route.name, params, url, body), excluded))
    );
  };

  const write = async (
    request: Request,
    url: URL,
    segments: readonly string[]
  ): Promise<Response> => {
    const body = await readBody(request);
    const found = WRITE_ROUTES.map(([method, path, name]) => ({
      name,
      params: method === request.method ? match(path, segments) : undefined,
    })).find((candidate) => candidate.params !== undefined);
    let status: number;
    let response: unknown;
    if (!found?.params) {
      status = 501;
      response = {
        code: "EVAL_PROXY_UNMODELLED_WRITE",
        error: `the evaluation proxy recorded ${request.method} ${url.pathname} but does not model its response`,
      };
    } else if (await addressesExcluded(found.params, url)) {
      status = 404;
      response = notFoundBody;
    } else {
      [status, response] = await fake(found.name, found.params, body, url);
    }
    await record({
      kind: "write",
      method: request.method,
      path: url.pathname,
      query: url.search,
      status,
      body,
      response,
    });
    return status === 204 ? new Response(null, { status }) : Response.json(response, { status });
  };

  const fake = async (
    name: (typeof WRITE_ROUTES)[number][2],
    params: Params,
    body: unknown,
    url: URL
  ): Promise<[number, unknown]> => {
    const actor = sessionActor(body);
    const input = isRecord(body) ? body : {};
    const key = params.key ?? "";
    switch (name) {
      case "create-issue": {
        overlay.issueCount += 1;
        const project = String(input.project ?? "EVAL");
        const number = FAKE_ISSUE_NUMBER_BASE + overlay.issueCount;
        const issueKey = `${project}-${number}`;
        const spec: StoredArtifact = newDocument(
          issueKey,
          project,
          "spec.md",
          String(input.spec ?? ""),
          actor,
          true
        );
        overlay.artifacts.set(spec.artifact.id, spec);
        const issue: IssueDetails = {
          key: issueKey,
          project,
          number,
          title: String(input.title ?? ""),
          status: "triage",
          priority: (input.priority as Issue["priority"]) ?? null,
          rank: "0|zzzzzz:",
          labels: Array.isArray(input.labels) ? (input.labels as string[]) : [],
          parent: typeof input.parent === "string" ? input.parent : null,
          assignee: typeof input.assignee === "string" ? input.assignee : null,
          claim: null,
          components: { mode: "inherit", ids: [], unknown: [] } as unknown as Issue["components"],
          external_links: [],
          route: null,
          created_by: actor,
          created_at: now(),
          updated_at: now(),
          closed_at: null,
          primary_artifact_id: spec.artifact.id,
          last_seq: 0,
          route_status: null,
          route_holder: null,
          artifacts: [],
          open_asks: [],
          children: [],
          referenced_by_count: 0,
        };
        overlay.issues.set(issueKey, issue);
        overlay.baseSeq.set(issueKey, 0);
        appendEvent(issueKey, "issue.created", actor, issue);
        return [
          201,
          { ...issueOf(issue), last_seq: overlay.lastSeq(issueKey) } satisfies Advised<Issue>,
        ];
      }
      case "update-issue": {
        await noteIssue(key);
        const { actor: _actor, rank: _rank, ...fields } = input;
        const created = overlay.issues.get(key);
        if (created) {
          overlay.issues.set(key, { ...created, ...fields, updated_at: now() } as IssueDetails);
        } else {
          overlay.patches.set(key, { ...overlay.patches.get(key), ...fields, updated_at: now() });
        }
        const current = await currentIssue(key);
        appendEvent(key, "issue.updated", actor, current);
        return [200, current];
      }
      case "claim":
      case "release": {
        await noteIssue(key);
        const claim = name === "claim" ? { actor, at: now() } : null;
        overlay.patches.set(key, { ...overlay.patches.get(key), claim });
        return [200, await currentIssue(key)];
      }
      case "issue-ask":
      case "artifact-ask": {
        const owner = name === "issue-ask" ? key : await artifactOwner(params.artifact ?? "");
        if (owner) await noteIssue(owner);
        const ask: Ask = {
          id: randomUUID(),
          issue_key: owner ?? null,
          artifact_id: params.artifact ?? null,
          block_id: null,
          author: actor,
          kind: "question",
          question: String(input.question ?? ""),
          options: Array.isArray(input.options) ? (input.options as Ask["options"]) : [],
          multiple: input.multiple === true,
          urgency: (input.urgency as Ask["urgency"]) ?? "normal",
          anchor: null,
          state: "open",
          answer: null,
          opened_event_id: FAKE_EVENT_ID_BASE + overlay.eventCount + 1,
          created_at: now(),
          edited_at: null,
        };
        overlay.asks.set(ask.id, ask);
        if (owner) appendEvent(owner, "ask.opened", actor, ask, ask.artifact_id ?? null);
        return [201, ask satisfies Advised<Ask>];
      }
      case "issue-comment":
      case "artifact-comment": {
        const owner = name === "issue-comment" ? key : await artifactOwner(params.artifact ?? "");
        if (owner) await noteIssue(owner);
        const suggestion = isRecord(input.suggestion)
          ? { replace_with: String(input.suggestion.replace_with ?? ""), accepted: null }
          : null;
        const comment: Comment = {
          id: randomUUID(),
          issue_key: owner ?? null,
          artifact_id: params.artifact ?? null,
          author: actor,
          body: String(input.body ?? ""),
          anchor: null,
          reply_to: typeof input.reply_to === "string" ? input.reply_to : null,
          ask_id: typeof input.ask_id === "string" ? input.ask_id : null,
          turn:
            typeof input.ask_id === "string" ? ((input.turn as Comment["turn"]) ?? "human") : null,
          resolved: false,
          resolved_by: null,
          resolved_at: null,
          edited_at: null,
          suggestion,
          created_at: now(),
          mentions: [],
          deliveries: [],
        };
        overlay.comments.set(comment.id, comment);
        if (owner)
          appendEvent(owner, "comment.created", actor, comment, comment.artifact_id ?? null);
        return [201, comment satisfies Advised<Comment>];
      }
      case "issue-message":
      case "message-reply": {
        const parent =
          params.message === undefined ? undefined : overlay.messages.get(params.message);
        const issueKey = name === "issue-message" ? key : (parent?.issue_key ?? null);
        if (issueKey) await noteIssue(issueKey);
        const message: Message = {
          id: randomUUID(),
          issue_key: issueKey,
          author: actor,
          body: String(input.body ?? ""),
          target: typeof input.target === "string" ? input.target : null,
          in_reply_to:
            name === "message-reply"
              ? (params.message ?? null)
              : typeof input.in_reply_to === "string"
                ? input.in_reply_to
                : null,
          deliveries: [],
          created_at: now(),
        };
        overlay.messages.set(message.id, message);
        if (issueKey) appendEvent(issueKey, "message.created", actor, message);
        return [201, message satisfies Advised<Message>];
      }
      case "issue-artifact":
      case "project-artifact": {
        const issueKey = name === "issue-artifact" ? key : null;
        if (issueKey) await noteIssue(issueKey);
        const project = issueKey === null ? (params.project ?? "") : issueKey.replace(/-\d+$/, "");
        const content = typeof input.content === "string" ? input.content : "";
        const stored = newDocument(
          issueKey,
          project,
          String(input.name ?? "document"),
          content,
          actor,
          false
        );
        overlay.artifacts.set(stored.artifact.id, stored);
        if (issueKey)
          appendEvent(
            issueKey,
            "artifact.created",
            actor,
            { artifact: stored.artifact },
            stored.artifact.id
          );
        const created = stored.artifact.versions[0] as Version;
        return [
          201,
          { artifact: stored.artifact, version: created } satisfies ArtifactUploadResponse,
        ];
      }
      case "artifact-edit":
      case "artifact-version": {
        const id = params.artifact ?? "";
        const stored = overlay.artifacts.get(id);
        const upstreamArtifact = stored
          ? undefined
          : await upstreamJson(`/api/v1/artifacts/${encodeURIComponent(id)}`);
        const versions =
          stored?.artifact.versions ??
          (isRecord(upstreamArtifact) ? (upstreamArtifact.versions as Version[]) : []);
        const next = version(
          (versions.at(-1)?.number ?? 0) + 1,
          actor,
          typeof input.summary === "string" ? input.summary : null
        );
        if (stored) stored.artifact = { ...stored.artifact, versions: [...versions, next] };
        if (name === "artifact-version") return [201, { ...next, named: true } satisfies Version];
        const ops = Array.isArray(input.ops) ? input.ops.length : 0;
        return [
          200,
          { applied: ops, version: next, changed: ops > 0 } satisfies EditArtifactResponse,
        ];
      }
      case "resolve-ask":
      case "edit-ask": {
        const id = params.ask ?? "";
        const stored = overlay.asks.get(id);
        const upstreamAsk = stored
          ? undefined
          : await upstreamJson(`/api/v1/asks/${encodeURIComponent(id)}`);
        const ask = stored ?? (isRecord(upstreamAsk) ? (upstreamAsk.ask as Ask) : undefined);
        if (!ask) return [404, notFoundBody];
        const { actor: _actor, ...fields } = input;
        const updated: Ask =
          name === "resolve-ask"
            ? {
                ...ask,
                state: "resolved",
                resolution: {
                  kind: input.kind === "retracted" ? "retracted" : "resolved",
                  reason: String(input.reason ?? ""),
                  actor,
                  at: now(),
                } as Ask["resolution"],
              }
            : ({ ...ask, ...fields, edited_at: now() } as Ask);
        overlay.asks.set(id, updated);
        return [200, updated];
      }
      case "follow":
      case "unfollow":
        return [204, undefined];
      case "resolve-comment": {
        const id = params.comment ?? "";
        const stored = overlay.comments.get(id);
        const upstreamComment = stored
          ? undefined
          : await upstreamJson(`/api/v1/comments/${encodeURIComponent(id)}`);
        const comment =
          stored ?? (isRecord(upstreamComment) ? (upstreamComment.comment as Comment) : undefined);
        if (!comment) return [404, notFoundBody];
        const resolved: Comment = {
          ...comment,
          resolved: true,
          resolved_by: actor,
          resolved_at: now(),
        };
        overlay.comments.set(id, resolved);
        return [200, resolved];
      }
      case "approval-request": {
        const id = params.artifact ?? "";
        const owner = await artifactOwner(id);
        const [, ask] = await fake(
          "artifact-ask",
          params,
          { question: "Approve this document?", actor },
          url
        );
        const latest = overlay.artifacts.get(id)?.artifact.versions.at(-1)?.number ?? 1;
        return [
          201,
          {
            ask: { ...(ask as Ask), kind: "approval", issue_key: owner ?? null },
            artifact_id: id,
            version: latest,
            approval: { state: "awaiting", latest_version: latest, requested_by: actor },
          },
        ];
      }
      case "architecture-sync":
        return [
          501,
          {
            code: "EVAL_PROXY_UNMODELLED_WRITE",
            error: `the evaluation proxy recorded the sync of ${params.project} but does not model its response`,
          },
        ];
    }
  };

  const currentIssue = async (key: string): Promise<Issue> => {
    const created = overlay.issues.get(key);
    if (created) return { ...issueOf(created), last_seq: overlay.lastSeq(key) };
    const upstreamIssue = await upstreamJson(`/api/v1/issues/${encodeURIComponent(key)}`);
    const base = isRecord(upstreamIssue) ? upstreamIssue : { key };
    return {
      ...issueOf(base),
      ...overlay.patches.get(key),
      last_seq: overlay.lastSeq(key),
    } as unknown as Issue;
  };

  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: options.port ?? 0,
    async fetch(request) {
      const url = new URL(request.url);
      if (!url.pathname.startsWith("/api/v1/")) {
        return refuse(request.method, url, `${url.pathname} is not a Dispatch API route`);
      }
      const segments = url.pathname
        .slice("/api/v1/".length)
        .split("/")
        .filter((segment) => segment !== "")
        .map((segment) => decodeURIComponent(segment));
      if (request.method === "GET") return read(url, segments);
      if (Object.hasOwn(WRITE_METHODS, request.method)) return write(request, url, segments);
      return refuse(request.method, url, `the evaluation proxy refuses ${request.method}`);
    },
  });
  const port = server.port ?? 0;
  return {
    url: `http://127.0.0.1:${port}`,
    port,
    stop: async () => {
      await server.stop(true);
    },
  };
}

/** A document as the server's create answers it: an issue's primary document is `spec`, every
 * other slug is its name made URL-safe, `ref_key` is `<owner>/<slug>`, and only the primary
 * document carries an approval. */
function newDocument(
  issueKey: string | null,
  project: string,
  name: string,
  markdown: string,
  actor: Actor,
  primary: boolean
): StoredArtifact {
  const slug = primary
    ? "spec"
    : name
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/(^-|-$)/g, "");
  return {
    markdown,
    artifact: {
      id: randomUUID(),
      issue_key: issueKey,
      project,
      ref_key: `${issueKey ?? project}/${slug}`,
      slug,
      name,
      kind: "doc",
      primary,
      created_by: actor,
      created_at: now(),
      versions: [version(1, actor, null)],
      ...(primary ? { approval: { state: "draft", latest_version: 1 } as const } : {}),
    },
  };
}

interface CliOptions {
  upstream?: string;
  tokenFile?: string;
  writes?: string;
  port: number;
  excludeKeys: string[];
}

function parseArguments(argv: readonly string[]): CliOptions {
  const parsed: CliOptions = { port: 0, excludeKeys: [] };
  for (let index = 0; index < argv.length; index += 1) {
    const flag = argv[index];
    const value = argv[index + 1];
    if (value === undefined || value.startsWith("--")) throw new Error(`${flag} needs a value`);
    index += 1;
    switch (flag) {
      case "--upstream":
        parsed.upstream = value;
        break;
      case "--token-file":
        parsed.tokenFile = value;
        break;
      case "--writes":
        parsed.writes = value;
        break;
      case "--port":
        if (!/^\d+$/.test(value) || Number(value) > 65535)
          throw new Error(`--port ${value} is not a port`);
        parsed.port = Number(value);
        break;
      case "--exclude-key":
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
    if (cli.upstream === undefined)
      throw new Error("--upstream is required: the Dispatch base URL reads go to");
    if (!/^https?:\/\/[^/]/.test(cli.upstream))
      throw new Error(`--upstream ${cli.upstream} is not an http(s) URL`);
    if (cli.tokenFile === undefined)
      throw new Error("--token-file is required: the file holding the upstream bearer");
    if (cli.writes === undefined)
      throw new Error("--writes is required: the JSONL file every request is recorded to");
    const token = (await Bun.file(cli.tokenFile).text()).trim();
    if (token === "") throw new Error(`--token-file ${cli.tokenFile} is empty`);
    const proxy = await startEvalProxy({
      upstream: cli.upstream,
      token,
      writesFile: cli.writes,
      excludeKeys: cli.excludeKeys,
      port: cli.port,
    });
    console.log(`listening on ${proxy.url}`);
  } catch (error) {
    console.error(`eval-proxy: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(2);
  }
}
