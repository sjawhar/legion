// A stand-in for the dispatch service's HTTP surface, backed by in-memory
// issues, so the built SPA runs in a real browser without GitHub. It speaks
// exactly the routes the SPA calls (`/auth/whoami`, `/api/installations`,
// `/api/view`, `/api/github/graphql`, `/api/github/rest/*`, `/api/events`)
// and adds control endpoints for drivers outside the test process:
// POST /__fixture/comment (a comment arriving from elsewhere),
// POST /__fixture/state (an issue closed or reopened elsewhere), and
// POST /__fixture/event (a GitHub SSE event). Tests in-process call the same
// operations on the returned FixtureServer. Plain node:http, because
// Playwright runs test files in Node workers, not Bun.

import { existsSync, readFileSync, statSync } from "node:fs";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import path from "node:path";
import type { FixtureComment, FixtureIssue } from "./threads";

export interface PostedComment {
  repo: string;
  number: number;
  body: string;
}

/** The SSE frame's JSON, as the Go service emits it. */
export interface FixtureEvent {
  subject: string;
  repo: string;
}

export interface FixtureServer {
  readonly url: string;
  /** Every comment the SPA posted through the REST proxy, oldest first. */
  readonly posted: PostedComment[];
  addComment(repo: string, number: number, comment: Omit<FixtureComment, "id">): FixtureComment;
  setState(repo: string, number: number, state: FixtureIssue["state"]): void;
  /** Delivers the frame to every connected SSE client, waiting for the first one to connect. */
  emit(event: FixtureEvent): Promise<void>;
  stop(): Promise<void>;
}

const CONTENT_TYPES: Record<string, string> = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".svg": "image/svg+xml",
  ".json": "application/json",
  ".map": "application/json",
};

const REST_ISSUE = /^\/api\/github\/rest\/repos\/([^/]+\/[^/]+)\/issues\/(\d+)(\/comments)?$/;

function readBody(request: IncomingMessage): Promise<string> {
  const body = Promise.withResolvers<string>();
  let data = "";
  request.setEncoding("utf8");
  request.on("data", (chunk: string) => {
    data += chunk;
  });
  request.on("end", () => body.resolve(data));
  request.on("error", body.reject);
  return body.promise;
}

async function readJson<T>(request: IncomingMessage): Promise<T> {
  return JSON.parse(await readBody(request)) as T;
}

function sendJson(response: ServerResponse, value: unknown, status = 200): void {
  response.writeHead(status, { "content-type": "application/json" });
  response.end(JSON.stringify(value));
}

function restIssue(issue: FixtureIssue) {
  return {
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    state_reason: issue.state === "closed" ? "completed" : null,
    created_at: issue.createdAt,
    updated_at: issue.createdAt,
    user: { login: issue.author },
  };
}

function restComment(comment: FixtureComment) {
  return {
    id: comment.id,
    body: comment.body,
    created_at: comment.createdAt,
    updated_at: comment.createdAt,
    user: { login: comment.author },
  };
}

// The search response for one issue, in the shape the SPA's SearchDispatchThreads
// query selects: the last 30 comments ride along so the sidebar can count open asks.
function searchNode(issue: FixtureIssue) {
  const [owner, name] = issue.repo.split("/") as [string, string];
  return {
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state.toUpperCase(),
    updatedAt: issue.createdAt,
    createdAt: issue.createdAt,
    author: { login: issue.author },
    comments: {
      totalCount: issue.comments.length,
      nodes: issue.comments.slice(-30).map((comment) => ({
        databaseId: comment.id,
        body: comment.body,
        createdAt: comment.createdAt,
        updatedAt: comment.createdAt,
        author: { login: comment.author },
      })),
    },
    parent: null,
    repository: { owner: { login: owner }, name },
  };
}

export function startFixtureServer(options: {
  distDir: string;
  issues: FixtureIssue[];
}): Promise<FixtureServer> {
  const distDir = path.resolve(options.distDir);
  const issues = structuredClone(options.issues);
  const posted: PostedComment[] = [];
  const streams = new Set<ServerResponse>();
  // The SPA opens /api/events only after its first paint, so a spec that emits
  // right after the page shows content would otherwise race the connection.
  let subscribed = Promise.withResolvers<void>();
  let addressed: Record<string, string> = {};
  let nextCommentId = 900;

  function find(repo: string, number: number): FixtureIssue {
    const issue = issues.find(
      (candidate) => candidate.repo === repo && candidate.number === number
    );
    if (!issue) throw new Error(`fixture has no ${repo}#${number}`);
    return issue;
  }

  function addComment(
    repo: string,
    number: number,
    comment: Omit<FixtureComment, "id">
  ): FixtureComment {
    const created = { id: ++nextCommentId, ...comment };
    find(repo, number).comments.push(created);
    return created;
  }

  function setState(repo: string, number: number, state: FixtureIssue["state"]): void {
    find(repo, number).state = state;
  }

  async function emit(event: FixtureEvent): Promise<void> {
    await subscribed.promise;
    const frame = `event: github_event\ndata: ${JSON.stringify({ ...event, payload: {} })}\n\n`;
    for (const stream of streams) stream.write(frame);
  }

  function serveStatic(pathname: string, response: ServerResponse): void {
    const requested = path.resolve(distDir, `.${pathname}`);
    const inDist = requested.startsWith(`${distDir}${path.sep}`);
    const target =
      inDist && existsSync(requested) && statSync(requested).isFile()
        ? requested
        : path.join(distDir, "index.html");
    response.writeHead(200, {
      "content-type": CONTENT_TYPES[path.extname(target)] ?? "application/octet-stream",
    });
    response.end(readFileSync(target));
  }

  const server = createServer(async (request, response) => {
    const url = new URL(request.url ?? "/", "http://fixture");
    const pathname = url.pathname;
    if (pathname === "/auth/whoami") return sendJson(response, { login: "fixture" });
    if (pathname === "/api/installations") {
      return sendJson(response, { installations: [{ account: { login: "acme-org" } }] });
    }
    if (pathname === "/api/view") {
      if (request.method === "PATCH") {
        addressed =
          (await readJson<{ addressed?: Record<string, string> }>(request)).addressed ?? {};
      }
      return sendJson(response, { addressed });
    }
    if (pathname === "/api/events") {
      response.writeHead(200, {
        "content-type": "text/event-stream",
        "cache-control": "no-cache",
        connection: "keep-alive",
      });
      response.write(": connected\n\n");
      streams.add(response);
      subscribed.resolve();
      request.on("close", () => {
        streams.delete(response);
        if (streams.size === 0) subscribed = Promise.withResolvers<void>();
      });
      return;
    }
    if (pathname === "/api/github/graphql") {
      // The SPA searches `is:issue is:open label:dispatch-thread`; the fixture
      // treats "carries a marker" as the label, so `referencedPr` never appears
      // in the sidebar and a closed thread drops out of it.
      const nodes = issues
        .filter(
          (issue) =>
            issue.state === "open" &&
            (issue.body.startsWith("<!-- dispatch:") || issue.body.startsWith("---\n"))
        )
        .map(searchNode);
      return sendJson(response, { data: { search: { nodes } } });
    }
    const rest = pathname.match(REST_ISSUE);
    if (rest) {
      const [, repo, numberText, comments] = rest as [string, string, string, string | undefined];
      const issue = issues.find(
        (candidate) => candidate.repo === repo && candidate.number === Number(numberText)
      );
      if (!issue) return sendJson(response, { message: "Not Found" }, 404);
      if (comments && request.method === "POST") {
        const { body } = await readJson<{ body: string }>(request);
        posted.push({ repo: issue.repo, number: issue.number, body });
        const created = addComment(issue.repo, issue.number, {
          body,
          author: "fixture",
          createdAt: new Date().toISOString(),
        });
        return sendJson(response, restComment(created));
      }
      if (comments) return sendJson(response, issue.comments.map(restComment));
      if (request.method === "PATCH") {
        const patch = await readJson<{ state?: string; state_reason?: string | null }>(request);
        if (patch.state === "closed" || patch.state === "open") issue.state = patch.state;
        return sendJson(response, {
          ...restIssue(issue),
          state_reason: patch.state_reason ?? restIssue(issue).state_reason,
        });
      }
      return sendJson(response, restIssue(issue));
    }
    if (pathname === "/__fixture/comment" && request.method === "POST") {
      const { repo, number, body, author } = await readJson<{
        repo: string;
        number: number;
        body: string;
        author: string;
      }>(request);
      return sendJson(
        response,
        addComment(repo, number, { body, author, createdAt: new Date().toISOString() })
      );
    }
    if (pathname === "/__fixture/state" && request.method === "POST") {
      const { repo, number, state } = await readJson<{
        repo: string;
        number: number;
        state: FixtureIssue["state"];
      }>(request);
      setState(repo, number, state);
      return sendJson(response, { ok: true });
    }
    if (pathname === "/__fixture/event" && request.method === "POST") {
      await emit(await readJson<FixtureEvent>(request));
      return sendJson(response, { ok: true });
    }
    if (pathname.startsWith("/api/") || pathname.startsWith("/__fixture/")) {
      return sendJson(response, { message: `fixture does not serve ${pathname}` }, 404);
    }
    serveStatic(pathname === "/" ? "/index.html" : pathname, response);
  });

  const listening = Promise.withResolvers<FixtureServer>();
  server.listen(0, "127.0.0.1", () => {
    const { port } = server.address() as AddressInfo;
    listening.resolve({
      url: `http://127.0.0.1:${port}`,
      posted,
      addComment,
      setState,
      emit,
      stop: () => {
        const closed = Promise.withResolvers<void>();
        for (const stream of streams) stream.end();
        server.close(() => closed.resolve());
        server.closeAllConnections();
        return closed.promise;
      },
    });
  });
  return listening.promise;
}
