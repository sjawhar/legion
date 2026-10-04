import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  type Artifact,
  type BlockPath,
  dispatchToolSpecs,
  type IssueComponents,
  SEARCH_QUERY_MAX,
  type TablePosition,
  zodSchemaApi,
} from "@legion/contracts";
import { z } from "zod";
import type { ExecFn } from "../dispatch-cwd";
import {
  type DispatchToolResult,
  executeDispatchTool,
  positionText,
  resolveIssueDocumentId,
} from "../dispatch-execute";
import { DispatchClient } from "../dispatch-http";
import { dispatchFollowNotice } from "../dispatch-subscribe";
import { ToolInputError } from "../tool-input-errors";

function response(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}
function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}
/** GET /api/v1/references for a node nothing cites and that cites nothing. */
function emptyGraph(kind: string): { node: { kind: string; id: string }; edges: never[] } {
  return { node: { kind, id: "node" }, edges: [] };
}

function repoExec(repo: string): ExecFn {
  return async (file, args) => {
    if (file === "jj" && args.join(" ") === "git remote list") {
      return { stdout: `origin https://github.com/${repo}.git\n` };
    }
    throw new Error(`unexpected command: ${file} ${args.join(" ")}`);
  };
}

const config = { enabled: true, url: "http://dispatch.test", token: "secret", error: null };
const releaseQuestion = "The release is ready. How should we proceed?";
const changedReleaseQuestion =
  "The change is ready but needs a release decision. How should we proceed?";
const firstReleaseQuestion = "The first release needs sequencing. How should we proceed?";
const secondReleaseQuestion = "The second release needs sequencing. How should we proceed?";
const runbookQuestion = "The runbook is ready for readers. How should we publish it?";
const revisedPlanQuestion =
  "The revised plan changes the release, but it has not been reviewed. How should we proceed?";
const documentReviewQuestion = "The document needs review before approval. How should we proceed?";
const reopenQuestion = "The closed issue may need further work. How should we proceed?";

function executeAsk(args: Record<string, unknown>, fetchImpl: typeof fetch) {
  return executeDispatchTool({
    tool: "dispatch_ask",
    args,
    cwd: "/workspace",
    host: "omp",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl,
  });
}

const architectureComponentsGuidance =
  'Project LEGION has an architecture model, but this issue is not linked to any of its current components. Review the `dispatch` skill, "Architecture components", to attach it to the parts it changes or mark it as non-architectural with a reason.';

const createdIssueLine =
  "Created LEGION-216: Architecture work (not subscribed to LEGION-216; envoy_subscribe notifications.dispatch.issue.LEGION-216.> for every event on it)";

const architectureSourceUnavailableGuidance =
  'Could not check whether project LEGION has an architecture model: architecture source network error. Review the `dispatch` skill, "Architecture components", to attach it to the parts it changes or mark it as non-architectural with a reason.';

/** A project with no architecture source, as a current server answers it: `200 null`. */
function sourceNull(): Response {
  return new Response("null", { headers: { "Content-Type": "application/json" } });
}

/** The same project as an older server answers it, which a client still meets mid-rollout. */
function sourceNotFound(): Response {
  return new Response(
    JSON.stringify({
      code: "SOURCE_NOT_FOUND",
      error: "no architecture source configured for LEGION",
    }),
    { status: 404, headers: { "Content-Type": "application/json" } }
  );
}

/** A 404 that is not the no-source answer: a project the server does not know. */
function projectNotFound(): Response {
  return new Response(JSON.stringify({ code: "NOT_FOUND", error: "project LEGION not found" }), {
    status: 404,
    headers: { "Content-Type": "application/json" },
  });
}

function architectureSource(project: string) {
  return {
    project,
    repo: "sjawhar/legion",
    branch: "main",
    enabled: true,
    created_by: { kind: "user", login: "sjawhar" },
    created_at: "2026-09-23T00:00:00Z",
    last_sync_at: null,
    last_commit: null,
    last_error: null,
  };
}
function issueComponents(
  mode: IssueComponents["mode"],
  ids: string[],
  unknown: string[] = [],
  reason: string | null = null,
  inheritedFrom: string | null = null
): IssueComponents {
  return { mode, ids, unknown, reason, inherited_from: inheritedFrom };
}

type ArchitectureSourceState =
  | "exists"
  | "null"
  | "source-not-found"
  | "project-not-found"
  | "fails";

async function createIssueWithComponents(
  components: IssueComponents,
  source: ArchitectureSourceState = "exists"
): Promise<{ readonly result: DispatchToolResult; readonly requests: string[] }> {
  const requests: string[] = [];
  const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const target = new URL(String(url));
    requests.push(`${init?.method ?? "GET"} ${target.pathname}`);
    if (target.pathname === "/api/v1/issues") {
      return response({
        key: "LEGION-216",
        title: "Architecture work",
        project: "LEGION",
        components,
      });
    }
    if (target.pathname === "/api/v1/projects/LEGION/architecture-source") {
      if (source === "exists") {
        return response(architectureSource("LEGION"));
      }
      if (source === "null") return sourceNull();
      if (source === "source-not-found") return sourceNotFound();
      if (source === "project-not-found") return projectNotFound();
      throw new Error("architecture source network error");
    }
    throw new Error(`unexpected request: ${target.pathname}`);
  };

  const result = await executeDispatchTool({
    tool: "dispatch_issue",
    args: { project: "LEGION", title: "Architecture work" },
    cwd: "/workspace",
    host: "omp",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl: fetchImpl as typeof fetch,
  });
  return { result, requests };
}

describe("executeDispatchTool", () => {
  test("prefills an omitted issue from LEGION_ISSUE using the cwd repository", async () => {
    const requests: Array<{ url: string; init: RequestInit }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const request = { url: String(url), init: init ?? {} };
      requests.push(request);
      if (new URL(request.url).pathname === "/api/v1/issues/resolve") {
        return response({ key: "DSP-41" });
      }
      return response({
        id: "ask-1",
        issue_key: "DSP-41",
        author: { kind: "session", id: "session-1" },
        question: releaseQuestion,
        options: [],
        multiple: false,
        urgency: "med",
        anchor: null,
        state: "open",
        answer: null,
        created_at: "2026-09-09T00:00:00Z",
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_ask",
      args: { question: releaseQuestion },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-1",
      sessionTitle: "Implement B2",
      config,
      env: { LEGION_ISSUE: "41" },
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual(["/api/v1/issues/resolve?ref=owner%2Frepo%2341", "/api/v1/issues/DSP-41/asks"]);
    expect(JSON.parse(requests[1]?.init.body as string)).toMatchObject({
      question: releaseQuestion,
      actor: {
        kind: "session",
        id: "session-1",
        origin: { host: "omp", cwd: "/workspace", session_title: "Implement B2" },
      },
    });
    expect(result.details).toEqual({
      issue: "DSP-41",
      ask: "ask-1",
      follows: { ask: "ask-1" },
    });
    expect(result.details).not.toHaveProperty("topic");
    expect(result.text).toBe(
      `Asked ask-1 on DSP-41 (urgency med): ${releaseQuestion}\n` +
        "You follow this ask: its answer and replies reach you directly. " +
        "For every event on DSP-41: envoy_subscribe notifications.dispatch.issue.DSP-41.>"
    );
  });

  test("appends an ask ref to the question sent to Dispatch", async () => {
    const requests: unknown[] = [];
    const ref = "dispatch://DSP-41/message/message-1";
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      expect(new URL(String(url)).pathname).toBe("/api/v1/issues/DSP-41/asks");
      const body = JSON.parse(String(init?.body));
      requests.push(body);
      return response({
        id: "ask-1",
        issue_key: "DSP-41",
        urgency: "med",
        question: body.question,
      });
    };

    const result = await executeAsk(
      { issue: "DSP-41", question: changedReleaseQuestion, ref },
      fetchImpl as typeof fetch
    );

    expect(requests).toEqual([
      expect.objectContaining({ question: `${changedReleaseQuestion}\n\nRef: ${ref}` }),
    ]);
    expect(result.text).toStartWith(
      `Asked ask-1 on DSP-41 (urgency med): ${changedReleaseQuestion}\n\nRef: ${ref}\n`
    );
  });

  // A gateway in front of Dispatch answers a non-2xx with its own page. The tool result is the
  // thrown message, so through the executor the agent must get the request, the status and a
  // plain-text excerpt rather than the page's markup, for a write and for a read alike, and advice
  // that fits the method: a read may succeed on a retry, a write may already have landed. The
  // message's wording is pinned in dispatch-http.test.ts.
  test("hands the agent the request and status, not a gateway's HTML, when Dispatch's host answers non-2xx", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      requests.push(`${init?.method ?? "GET"} ${new URL(String(url)).pathname}`);
      return new Response(
        "<html><head><title>502 Bad Gateway</title></head><body>nginx</body></html>",
        {
          status: 502,
          statusText: "Bad Gateway",
          headers: { "Content-Type": "text/html" },
        }
      );
    };
    const run = (tool: string, args: Record<string, unknown>) =>
      executeDispatchTool({
        tool,
        args,
        cwd: "/workspace",
        host: "omp",
        sessionId: "session-1",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      }).then(
        () => "",
        (error: Error) => error.message
      );

    const write = await run("dispatch_message", { issue: "DSP-1", body: "Shipped." });
    expect(write).toStartWith(
      "POST http://dispatch.test/api/v1/issues/DSP-1/messages answered 502 Bad Gateway with a body that is not Dispatch's error JSON"
    );
    expect(write).not.toContain("<");
    expect(write).toEndWith(
      "so the write may or may not have reached Dispatch: check whether it took effect before retrying it."
    );
    expect(write).not.toContain("a retry may succeed");
    expect(requests).toEqual(["POST /api/v1/issues/DSP-1/messages"]);

    const read = await run("dispatch_read", { issue: "DSP-1" });
    expect(read).toStartWith(
      "GET http://dispatch.test/api/v1/issues/DSP-1 answered 502 Bad Gateway with a body that is not Dispatch's error JSON"
    );
    expect(read).not.toContain("<");
    expect(read).toEndWith("so a retry may succeed.");
  });

  // Whether a write a gateway answered may have reached Dispatch decides what the agent does next.
  // A 5xx can come back after Dispatch applied it, so the agent checks before posting again; a 408
  // or 429 is the gateway's own timeout or rate limit, sent before it forwards the request, so the
  // message was not posted and a retry may succeed.
  test("tells dispatch_message whether a gateway's answer may have reached Dispatch", async () => {
    const gateway = ", which looks like a proxy or gateway page rather than Dispatch's own answer";
    for (const [status, statusText, advice] of [
      [408, "Request Timeout", "the write did not reach Dispatch, and a retry may succeed"],
      [429, "Too Many Requests", "the write did not reach Dispatch, and a retry may succeed"],
      [
        502,
        "Bad Gateway",
        "the write may or may not have reached Dispatch: check whether it took effect before retrying it",
      ],
    ] as const) {
      const fetchImpl = async () =>
        new Response(`<html><body><h1>${status} ${statusText}</h1></body></html>`, {
          status,
          statusText,
          headers: { "Content-Type": "text/html" },
        });

      const failure = await executeDispatchTool({
        tool: "dispatch_message",
        args: { issue: "DSP-1", body: "Shipped." },
        cwd: "/workspace",
        host: "omp",
        sessionId: "session-1",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as unknown as typeof fetch,
      }).then(
        () => "",
        (error: Error) => error.message
      );

      expect(failure).toBe(
        `POST http://dispatch.test/api/v1/issues/DSP-1/messages answered ${status} ${statusText} ` +
          `with a body that is not Dispatch's error JSON ("${status} ${statusText}")${gateway}, so ` +
          `${advice}.`
      );
    }
  });

  // An issue given as an external reference is resolved first (GET /api/v1/issues/resolve). A
  // gateway's 404 page there (a wrong host, a proxy that does not route /api) is not Dispatch
  // saying no issue is linked, so the agent must get the gateway's answer, not "create it first".
  test("does not read a gateway's 404 page on the resolve route as no linked issue", async () => {
    const fetchImpl = (async () =>
      new Response("<html><body><h1>404 Not Found</h1><hr>nginx</body></html>", {
        status: 404,
        statusText: "Not Found",
        headers: { "Content-Type": "text/html" },
      })) as unknown as typeof fetch;

    const message = await executeDispatchTool({
      tool: "dispatch_read",
      args: { issue: "owner/repo#12" },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-1",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl,
    }).then(
      () => "",
      (error: Error) => error.message
    );

    expect(message).toStartWith(
      "GET http://dispatch.test/api/v1/issues/resolve?ref=owner%2Frepo%2312 answered 404 Not Found"
    );
    expect(message).not.toContain("create it first");
  });

  test("names the configured Dispatch URL when its transport is unreachable", async () => {
    const fetchImpl = (() => {
      throw new TypeError("Unable to connect. Is the computer able to access the url?");
    }) as unknown as typeof fetch;

    await expect(
      executeAsk({ issue: "DSP-41", question: releaseQuestion }, fetchImpl)
    ).rejects.toThrow(
      "If the Dispatch URL changed, restart this agent process so it picks up the new configuration."
    );
  });

  test("does not duplicate an ask ref already in the question", async () => {
    const requests: unknown[] = [];
    const ref = "dispatch://DSP-41/message/message-1";
    const question = `${changedReleaseQuestion}\n\nRef: ${ref}`;
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const body = JSON.parse(String(init?.body));
      requests.push(body);
      return response({ id: "ask-1", issue_key: "DSP-41", question: body.question });
    };

    await executeAsk({ issue: "DSP-41", question, ref }, fetchImpl as typeof fetch);

    expect(requests).toEqual([expect.objectContaining({ question })]);
  });

  test("rejects an ask ref that would exceed Dispatch's question limit, naming the number to trim", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    // 790 + "\n\nRef: " (7) + 24 = 821 UTF-16 units.
    await expect(
      executeAsk(
        {
          issue: "DSP-41",
          question: "x".repeat(790),
          ref: "dispatch://DSP-41/ask/ab",
        },
        fetchImpl
      )
    ).rejects.toThrow(
      "question plus ref is 21 characters over the 800-character limit (821/800); shorten the question or drop the ref"
    );
  });

  test("rejects an ask ref outside the dispatch scheme", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeAsk(
        {
          issue: "DSP-41",
          question: changedReleaseQuestion,
          ref: "https://dispatch.test/issues/DSP-41",
        },
        fetchImpl
      )
    ).rejects.toThrow("ref must be a dispatch:// reference");
  });

  test("refuses a call once, listing the owner, schema, and hand-check problems together", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;
    const refuse = (args: Record<string, unknown>) =>
      executeDispatchTool({
        tool: "dispatch_message",
        args,
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      }).then(
        () => undefined,
        (error: unknown) => error
      );

    // `in_reply_to` is what makes `issue` optional, so a call that names one is never refused
    // for a missing issue - only for the reply target it could not read.
    const replying = await refuse({ message: "x", in_reply_to: "nope" });
    expect(replying).toBeInstanceOf(ToolInputError);
    if (!(replying instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(replying.problems).toEqual([
      "body is required (string)",
      'unknown field "message"; allowed: issue, body, in_reply_to',
      "in_reply_to must be a full message id (uuid) or a dispatch://KEY/message/<id> reference",
    ]);
    expect(replying.message).toBe(
      [
        "dispatch_message was not called: 3 problems",
        "- body is required (string)",
        '- unknown field "message"; allowed: issue, body, in_reply_to',
        "- in_reply_to must be a full message id (uuid) or a dispatch://KEY/message/<id> reference",
        "- Allowed keys: issue, body, in_reply_to",
        '- Example: dispatch_message({"issue":"DSP-1","body":"Implementation started."})',
      ].join("\n")
    );

    // Without one, the owner problem leads and the schema's own restatement of it is dropped.
    const posting = await refuse({ message: "x" });
    expect(posting).toBeInstanceOf(ToolInputError);
    if (!(posting instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(posting.problems).toEqual([
      "issue is required; supply issue or set LEGION_ISSUE",
      "body is required (string)",
      'unknown field "message"; allowed: issue, body, in_reply_to',
    ]);
  });

  test("names every bad option element and the unknown field in one refusal", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    const failure = await executeAsk(
      { issue: "DSP-41", question: "Pick", options: ["a", "b"], custom: true },
      fetchImpl
    ).then(
      () => undefined,
      (error: unknown) => error
    );
    if (!(failure instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(failure.problems).toEqual([
      "options.0 must be an object {label, description?}, not a string",
      "options.1 must be an object {label, description?}, not a string",
      'unknown field "custom"; allowed: issue, project, artifact, ref, question, options, multiple, urgency, anchor',
    ]);
  });

  test("reports the comment hand checks alongside the schema problems", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    const failure = await executeDispatchTool({
      tool: "dispatch_comment",
      args: {
        issue: "DSP-41",
        quote: "some text",
        reply_to: "c1",
        reply_to_ask: "a1",
        body: "x".repeat(2001),
      },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl,
    }).then(
      () => undefined,
      (error: unknown) => error
    );
    if (!(failure instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(failure.problems).toEqual([
      "body is 1 characters over the 2000-character limit (2001/2000)",
      "artifact is required when quote is supplied",
      "reply_to and reply_to_ask cannot both be set",
    ]);
  });

  test("rejects a numbered reply_to_ask with the issue's open asks", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const pathname = new URL(String(url)).pathname;
      requests.push(pathname);
      if (pathname === "/api/v1/issues/DSP-41") {
        return response({
          key: "DSP-41",
          open_asks: [
            { id: "01234567-0000-4000-8000-000000000001", question: firstReleaseQuestion },
            { id: "89abcdef-0000-4000-8000-000000000002", question: secondReleaseQuestion },
          ],
        });
      }
      throw new Error(`unexpected request: ${pathname}`);
    };

    const failure = await executeDispatchTool({
      tool: "dispatch_comment",
      args: { issue: "DSP-41", body: "Ship the first.", reply_to_ask: "12" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    }).then(
      () => undefined,
      (error: unknown) => error
    );

    expect(failure).toBeInstanceOf(ToolInputError);
    if (!(failure instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(failure.problems).toEqual([
      `ask IDs are UUIDs; use the full ask ID; this issue's open asks: 01234567… ${firstReleaseQuestion}, 89abcdef… ${secondReleaseQuestion}`,
    ]);
    expect(requests).toEqual(["/api/v1/issues/DSP-41"]);
  });

  test("consumes every declared dispatch_ask argument", async () => {
    const fields = [
      "issue",
      "project",
      "artifact",
      "ref",
      "question",
      "options",
      "multiple",
      "urgency",
      "anchor",
    ] as const;
    type Field = (typeof fields)[number];
    interface ArgumentCase {
      readonly args: Record<string, unknown>;
      readonly assert: (request: { body: Record<string, unknown>; path: string }) => void;
    }
    const cases: Record<Field, ArgumentCase> = {
      issue: {
        args: { issue: "DSP-41", question: "Question" },
        assert: ({ path }) => expect(path).toBe("/api/v1/issues/DSP-41/asks"),
      },
      project: {
        args: { project: "CORE", artifact: "notes", question: "Question" },
        assert: ({ path }) => expect(path).toBe("/api/v1/artifacts/artifact-CORE-notes/asks"),
      },
      artifact: {
        args: { project: "CORE", artifact: "architecture", question: "Question" },
        assert: ({ path }) =>
          expect(path).toBe("/api/v1/artifacts/artifact-CORE-architecture/asks"),
      },
      ref: {
        args: {
          issue: "DSP-41",
          question: "Question",
          ref: "dispatch://DSP-41/message/message-1",
        },
        assert: ({ body }) =>
          expect(body.question).toBe("Question\n\nRef: dispatch://DSP-41/message/message-1"),
      },
      question: {
        args: { issue: "DSP-41", question: "A distinct question" },
        assert: ({ body }) => expect(body.question).toBe("A distinct question"),
      },
      options: {
        args: {
          issue: "DSP-41",
          question: "Choose",
          options: [{ label: "Ship", description: "Deploy it." }],
        },
        assert: ({ body }) =>
          expect(body.options).toEqual([{ label: "Ship", description: "Deploy it." }]),
      },
      multiple: {
        args: { issue: "DSP-41", question: "Choose", multiple: true },
        assert: ({ body }) => expect(body.multiple).toBe(true),
      },
      urgency: {
        args: { issue: "DSP-41", question: "Choose", urgency: "high" },
        assert: ({ body }) => expect(body.urgency).toBe("high"),
      },
      anchor: {
        args: {
          issue: "DSP-41",
          question: "Review this",
          anchor: { artifact: "spec", occurrence: 1, quote: "The passage" },
        },
        assert: ({ body }) =>
          expect(body.anchor).toEqual({
            artifact: "artifact-spec",
            occurrence: 1,
            quote: "The passage",
          }),
      },
    };
    const askSpec = dispatchToolSpecs.find((spec) => spec.name === "dispatch_ask");
    if (askSpec === undefined) throw new Error("dispatch_ask spec is missing");
    expect(Object.keys(askSpec.arguments(zodSchemaApi(z))).sort()).toEqual([...fields].sort());

    for (const [field, testCase] of Object.entries(cases) as Array<[Field, ArgumentCase]>) {
      let request: { body: Record<string, unknown>; path: string } | undefined;
      const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const target = new URL(String(url));
        if (target.pathname === "/api/v1/issues/DSP-41") {
          return response({
            artifacts: [{ id: "artifact-spec", primary: true }],
            key: "DSP-41",
            primary_artifact_id: "artifact-spec",
          });
        }
        const document = target.pathname.match(
          /^\/api\/v1\/projects\/([^/]+)\/artifacts\/([^/]+)$/
        );
        if (document !== null) {
          const [, project, artifact] = document;
          return response({
            id: `artifact-${project}-${artifact}`,
            project,
            slug: artifact,
          });
        }
        if (/^\/api\/v1\/projects\/[^/]+\/artifacts$/.test(target.pathname)) return response([]);
        if (target.pathname.endsWith("/asks")) {
          const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
          request = { body, path: target.pathname };
          return response({
            artifact_id: target.pathname.startsWith("/api/v1/artifacts/") ? "artifact-1" : null,
            id: "ask-1",
            issue_key: target.pathname.startsWith("/api/v1/issues/") ? "DSP-41" : null,
            question: body.question,
          });
        }
        throw new Error(`unexpected request for ${field}: ${target.pathname}`);
      };

      await executeAsk(testCase.args, fetchImpl as typeof fetch);
      if (request === undefined) throw new Error(`dispatch_ask did not create an ask for ${field}`);
      testCase.assert(request);
    }
  });
  test("prefills an omitted issue from a native LEGION_ISSUE without resolving cwd repo", async () => {
    const requests: string[] = [];
    let execCalls = 0;
    const exec: ExecFn = async () => {
      execCalls += 1;
      throw new Error("cwd repository lookup must not run");
    };
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      if (target.pathname === "/api/v1/issues/LEGION-3/messages") {
        return response({ id: "message-3", issue_key: "LEGION-3" });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_message",
      args: { body: "Native issue" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: { LEGION_ISSUE: "LEGION-3" },
      exec,
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.details).toMatchObject({ issue: "LEGION-3" });
    expect(requests).toEqual(["/api/v1/issues/LEGION-3/messages"]);
    expect(execCalls).toBe(0);
  });

  test("posts a message in_reply_to as a full id or a dispatch://.../message/<id> reference, and cites the result", async () => {
    const parent = "6f1d2c3b-4a5e-4f60-8b7c-9d0e1f2a3b4c";
    const bodies: unknown[] = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42/messages") {
        bodies.push(JSON.parse(init?.body as string));
        return response({ id: "message-2", issue_key: "DSP-42" });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const bareIDResult = await executeDispatchTool({
      tool: "dispatch_message",
      args: { issue: "DSP-42", body: "Sounds good", in_reply_to: parent },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });
    const refResult = await executeDispatchTool({
      tool: "dispatch_message",
      args: {
        issue: "DSP-42",
        body: "Sounds good",
        in_reply_to: `dispatch://DSP-42/message/${parent}`,
      },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(bodies).toMatchObject([
      { body: "Sounds good", in_reply_to: parent },
      { body: "Sounds good", in_reply_to: parent },
    ]);
    const posted =
      "Posted message message-2 (dispatch://DSP-42/message/message-2) " +
      "(not subscribed to DSP-42; envoy_subscribe notifications.dispatch.issue.DSP-42.> for every event on it)";
    expect(bareIDResult.text).toBe(posted);
    expect(refResult.text).toBe(posted);
  });

  // A human's direct message to a session (the Agents page, POST /api/v1/agents/{id}/messages)
  // belongs to no issue, and only POST /api/v1/messages/{id}/reply can carry an answer into that
  // conversation. The route the tool picks is decided by what the caller named, not by what the
  // environment could supply: a Legion pane has LEGION_ISSUE set, and filing the answer on that
  // issue would put it on unrelated work.
  test("routes only a bare in_reply_to with no issue to the message reply route", async () => {
    const parent = "6f1d2c3b-4a5e-4f60-8b7c-9d0e1f2a3b4c";
    const calls: Array<{ method: string; path: string; body: unknown }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      calls.push({
        method: init?.method ?? "GET",
        path: `${target.pathname}${target.search}`,
        body: JSON.parse(init?.body as string),
      });
      if (target.pathname === `/api/v1/messages/${parent}/reply`) {
        return response({
          id: "reply-1",
          issue_key: null,
          body: JSON.parse(init?.body as string).body,
        });
      }
      if (target.pathname === "/api/v1/issues/LEGION-3/messages") {
        return response({ id: "message-9", issue_key: "LEGION-3" });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };
    const call = (args: Record<string, unknown>) =>
      executeDispatchTool({
        tool: "dispatch_message",
        args,
        cwd: "/workspace",
        host: "omp",
        sessionId: "ses_reader",
        config,
        env: { LEGION_ISSUE: "LEGION-3" },
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      });

    const reply = await call({ body: "On it.", in_reply_to: parent });
    const onIssue = await call({ issue: "LEGION-3", body: "On it.", in_reply_to: parent });
    // A dispatch://KEY/message/<id> names the issue its message lives on, so it is not a direct
    // message: it keeps the issue path even though the call names no issue of its own.
    const byRef = await call({
      body: "On it.",
      in_reply_to: `dispatch://LEGION-3/message/${parent}`,
    });

    expect(calls).toMatchObject([
      {
        method: "POST",
        // The model means to post, so it asks to follow up once the attempt is answered.
        path: `/api/v1/messages/${parent}/reply?follow_up=true`,
        body: { body: "On it.", attempt: 1, actor: { kind: "session", id: "ses_reader" } },
      },
      {
        method: "POST",
        path: "/api/v1/issues/LEGION-3/messages",
        body: { body: "On it.", in_reply_to: parent },
      },
      {
        method: "POST",
        path: "/api/v1/issues/LEGION-3/messages",
        body: { body: "On it.", in_reply_to: parent },
      },
    ]);
    expect(reply.details).toMatchObject({ message: "reply-1", in_reply_to: parent, posted: true });
    expect(reply.text).toBe(
      `Replied to message ${parent} with message reply-1. ` +
        `dispatch_read({message: "${parent}"}) reads the conversation back.`
    );
    expect(onIssue.details).toMatchObject({ issue: "LEGION-3", message: "message-9" });
    expect(byRef.details).toMatchObject({ issue: "LEGION-3", message: "message-9" });
  });

  // Once the session has answered, other text is its follow-up: Dispatch threads it under the
  // session's first reply and answers 201 with a message whose parent is that reply, not the
  // human's message.
  test("reports a follow-up threaded under the session's first reply", async () => {
    const parent = "6f1d2c3b-4a5e-4f60-8b7c-9d0e1f2a3b4c";
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      if (new URL(String(url)).pathname !== `/api/v1/messages/${parent}/reply`) {
        throw new Error("unexpected request");
      }
      return response({
        id: "reply-2",
        issue_key: null,
        body: JSON.parse(init?.body as string).body,
        in_reply_to: "reply-1",
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_message",
      args: { body: "Done: the dashboard is at /dash.", in_reply_to: parent },
      cwd: "/workspace",
      host: "omp",
      sessionId: "ses_reader",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.details).toMatchObject({
      message: "reply-2",
      in_reply_to: parent,
      posted: true,
      follows: "reply-1",
    });
    expect(result.text).toBe(
      `Replied to message ${parent} with message reply-2, a follow-up threaded under your ` +
        `reply reply-1. dispatch_read({message: "${parent}"}) reads the conversation back.`
    );
  });

  // Text the session already posted in the conversation (its first reply or an earlier
  // follow-up) posts nothing; Dispatch hands back that message marked duplicate, and the session
  // is told nothing new went out rather than that it replied.
  test("says nothing new was posted when Dispatch already has the exact text", async () => {
    const parent = "6f1d2c3b-4a5e-4f60-8b7c-9d0e1f2a3b4c";
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      if (new URL(String(url)).pathname !== `/api/v1/messages/${parent}/reply`) {
        throw new Error("unexpected request");
      }
      return response({
        id: "reply-1",
        issue_key: null,
        body: JSON.parse(init?.body as string).body,
        in_reply_to: parent,
        duplicate: true,
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_message",
      args: { body: "Still running.", in_reply_to: parent },
      cwd: "/workspace",
      host: "omp",
      sessionId: "ses_reader",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.details).toMatchObject({ message: "reply-1", posted: false, duplicate: true });
    expect(result.text).toBe(
      "Dispatch already has this exact text in the conversation (message reply-1); nothing new " +
        "was posted. Send different text if you have more to say."
    );
  });

  // A human's direct message belongs to no issue, so a session reads that conversation back by
  // the message id alone - even in a Legion pane, where LEGION_ISSUE names unrelated work - as
  // itself, since Dispatch lets a bearer read only a conversation its session is in.
  test("dispatch_read reads a direct-message conversation by message id", async () => {
    const parent = "6f1d2c3b-4a5e-4f60-8b7c-9d0e1f2a3b4c";
    const paths: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      paths.push(`${target.pathname}${target.search}`);
      if (target.pathname !== `/api/v1/messages/${parent}`) throw new Error("unexpected request");
      return response({
        message: {
          id: parent,
          issue_key: null,
          author: { kind: "user", id: "sami" },
          body: "Where is the dashboard?",
          deliveries: [],
        },
        replies: [
          {
            id: "reply-1",
            issue_key: null,
            author: { kind: "session", id: "ses_reader" },
            body: "On it.",
            in_reply_to: parent,
            deliveries: [],
          },
          {
            id: "reply-2",
            issue_key: null,
            author: { kind: "session", id: "ses_reader" },
            body: "Done.",
            in_reply_to: "reply-1",
            deliveries: [],
          },
        ],
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { message: parent },
      cwd: "/workspace",
      host: "omp",
      sessionId: "ses_reader",
      config,
      env: { LEGION_ISSUE: "LEGION-3" },
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(paths).toEqual([`/api/v1/messages/${parent}?session=ses_reader`]);
    expect(result.details).toMatchObject({ message: parent });
    expect(result.text).toContain(`${parent} · user sami`);
    expect(result.text).toContain("Body: Where is the dashboard?");
    expect(result.text).toContain("reply-2 · session ses_reader");
    expect(result.text).toContain("Body: Done.");
  });

  // A Dispatch that keeps one reply per delivery attempt answers a second call with the stored
  // reply, at 200, and posts nothing. Reporting that as a send would tell a session it answered
  // a human it never answered - most often over the host's own automatic BTW reply.
  test("says nothing was posted when the delivery was already answered", async () => {
    const parent = "6f1d2c3b-4a5e-4f60-8b7c-9d0e1f2a3b4c";
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      if (new URL(String(url)).pathname !== `/api/v1/messages/${parent}/reply`) {
        throw new Error("unexpected request");
      }
      return response({ id: "reply-0", issue_key: null, body: "The host already answered." });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_message",
      args: { body: "On it.", in_reply_to: parent },
      cwd: "/workspace",
      host: "omp",
      sessionId: "ses_reader",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.details).toMatchObject({ message: "reply-0", posted: false });
    expect(result.text).toBe(
      `Message ${parent} was already answered by message reply-0; Dispatch kept that reply and ` +
        "posted nothing. Wait for their next message rather than answering this one again."
    );
  });

  test("rejects a message in_reply_to referencing a non-message dispatch reference", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_message",
        args: {
          issue: "DSP-42",
          body: "Sounds good",
          in_reply_to: "dispatch://DSP-42/comment/comment-1",
        },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(
      "in_reply_to must be a full message id (uuid) or a dispatch://KEY/message/<id> reference"
    );
  });

  test("uses a full LEGION_ISSUE external reference without resolving cwd repo", async () => {
    const requests: string[] = [];
    let execCalls = 0;
    const exec: ExecFn = async () => {
      execCalls += 1;
      throw new Error("cwd repository lookup must not run");
    };
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      if (target.pathname === "/api/v1/issues/resolve") return response({ key: "LEGION-42" });
      if (target.pathname === "/api/v1/issues/LEGION-42/messages") {
        return response({ id: "message-42", issue_key: "LEGION-42" });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_message",
      args: { body: "External issue" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: { LEGION_ISSUE: "Owner/Repo.git#42" },
      exec,
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.details).toMatchObject({ issue: "LEGION-42" });
    expect(requests).toEqual([
      "/api/v1/issues/resolve?ref=owner%2Frepo%2342",
      "/api/v1/issues/LEGION-42/messages",
    ]);
    expect(execCalls).toBe(0);
  });

  test("rejects a LEGION_ISSUE outside the native, external, or bare-number forms", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_message",
        args: { body: "Invalid issue" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: { LEGION_ISSUE: "owner/repo#0" },
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(/native issue key.*owner\/repo#n.*bare positive issue number/);
  });

  test("rejects a dispatch operation that has neither an issue nor LEGION_ISSUE", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_message",
        args: { body: "Progress update" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(/issue/);
  });

  test("dispatch_search renders results with absolute links", async () => {
    const results = [
      {
        kind: "document",
        owner: { kind: "issue", key: "LEGION-2", title: "Astrolabe", status: "triage" },
        artifact: { slug: "spec", name: "spec.md" },
        id: "artifact-2",
        snippet: "…the <mark>astrolabe</mark> measures…",
        rank: 1,
        href: "/issues/LEGION-2/spec?q=astrolabe",
      },
      {
        kind: "comment",
        owner: { kind: "issue", key: "LEGION-2", title: "Astrolabe", status: "triage" },
        id: "comment-2",
        snippet: "Comment about <mark>astrolabe</mark>",
        rank: 0.5,
        href: "/issues/LEGION-2/spec#comment-2",
      },
      {
        kind: "comment",
        artifact: { slug: "navigation-design", name: "Navigation design" },
        owner: {
          artifact_id: "document-2",
          kind: "document",
          name: "Navigation design",
          project: "CORE",
          slug: "navigation-design",
        },
        id: "comment-3",
        snippet: "Comment on <mark>astrolabe</mark>",
        rank: 0.25,
        href: "/projects/CORE/documents/navigation-design?comment=comment-3",
      },
    ];
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      if (target.pathname !== "/api/v1/search")
        throw new Error(`unexpected request: ${target.pathname}`);
      return response({ results, took_ms: 12 });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_search",
      args: { query: "astrolabe" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe(
      [
        '3 results for "astrolabe" (12 ms)',
        "LEGION-2 [triage] Astrolabe - document spec.md: …the **astrolabe** measures… -> http://dispatch.test/issues/LEGION-2/spec?q=astrolabe",
        "LEGION-2 [triage] Astrolabe - comment: Comment about **astrolabe** -> http://dispatch.test/issues/LEGION-2/spec#comment-2",
        "dispatch://CORE/artifact/navigation-design [document] Navigation design - comment: Comment on **astrolabe** -> http://dispatch.test/projects/CORE/documents/navigation-design?comment=comment-3",
      ].join("\n")
    );
    expect(result.details).toEqual({ query: "astrolabe", results });
    expect(dispatchFollowNotice(result.details)).toBeNull();
    expect(requests).toEqual(["/api/v1/search?q=astrolabe"]);
  });

  test("dispatch_search reports no results for an accepted stop-word query", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      return response({ results: [], took_ms: 0 });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_search",
      args: { query: "the" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe('No results for "the".');
    expect(result.details).toEqual({ query: "the", results: [] });
    expect(requests).toEqual(["/api/v1/search?q=the"]);
  });

  test("dispatch_search rejects a one-character query before any request", async () => {
    let requests = 0;
    const fetchImpl = (() => {
      requests += 1;
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_search",
        args: { query: "a" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(/2 characters/);
    expect(requests).toBe(0);
  });

  test("dispatch_search refuses a spec-sized query by name before any request", async () => {
    let requests = 0;
    const fetchImpl = (() => {
      requests += 1;
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;
    const length = 26_637;

    await expect(
      executeDispatchTool({
        tool: "dispatch_search",
        args: { query: "x".repeat(length) },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(
      `- query is ${length - SEARCH_QUERY_MAX} characters over the ${SEARCH_QUERY_MAX}-character limit (${length}/${SEARCH_QUERY_MAX}); search with a short phrase of a few words, not a passage\n`
    );
    expect(requests).toBe(0);
  });

  test("dispatch_architecture_sync posts the sync route and reports the imported commit", async () => {
    const requests: string[] = [];
    const source = {
      project: "CORE",
      repo: "legion/arch",
      branch: "main",
      enabled: true,
      created_by: { kind: "user", id: "alice" },
      created_at: "2026-09-17T00:00:00Z",
      last_sync_at: "2026-09-17T00:05:00Z",
      last_commit: "c0ffee",
      last_error: null,
    };
    const fetchImpl = (async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(`${init?.method ?? "GET"} ${target.pathname}`);
      return response(source);
    }) as typeof fetch;

    const result = await executeDispatchTool({
      tool: "dispatch_architecture_sync",
      args: { project: "CORE" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl,
    });

    expect(requests).toEqual(["POST /api/v1/projects/CORE/architecture-source/sync"]);
    expect(result.text).toBe(
      "Synced CORE architecture from legion/arch@main: commit c0ffee (2026-09-17T00:05:00Z)."
    );
    expect(result.details).toEqual({
      project: "CORE",
      repo: "legion/arch",
      branch: "main",
      commit: "c0ffee",
      error: null,
    });
  });

  test("dispatch_architecture_sync reports a rejected model with the surviving commit", async () => {
    const fetchImpl = (async (): Promise<Response> =>
      response({
        project: "CORE",
        repo: "legion/arch",
        branch: "main",
        enabled: true,
        created_by: { kind: "user", id: "alice" },
        created_at: "2026-09-17T00:00:00Z",
        last_sync_at: "2026-09-17T00:10:00Z",
        last_commit: "c0ffee",
        last_error: "api.md: duplicate component id",
      })) as unknown as typeof fetch;

    const result = await executeDispatchTool({
      tool: "dispatch_architecture_sync",
      args: { project: "CORE" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl,
    });

    expect(result.text).toBe(
      [
        "Sync failed for CORE (legion/arch@main): api.md: duplicate component id",
        "The previous model stays up (commit c0ffee).",
      ].join("\n")
    );
    expect(result.details).toMatchObject({
      commit: "c0ffee",
      error: "api.md: duplicate component id",
    });
  });

  test("dispatch_open_asks renders active asks by whose reply is due", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      return response({
        session_id: "session-1",
        as_of: "2026-09-13T00:00:00Z",
        opened_since: false,
        count: 2,
        waiting_on_human: 1,
        waiting_on_agent: 1,
        asks: [
          {
            id: "ask-1",
            ref: "/issues/LEGION-1?ask=ask-1",
            question: releaseQuestion,
            kind: "question",
            urgency: "high",
            created_at: "2026-09-13T00:00:00Z",
            age_seconds: 65,
            priority: 0,
            owner: { issue: { key: "LEGION-1", title: "Reminder" } },
            human_replied: false,
            last_reply: null,
            waiting_on: "human",
          },
          {
            id: "ask-2",
            ref: "/projects/OPS/documents/runbook?ask=ask-2",
            question: "Which region?",
            kind: "question",
            urgency: "med",
            created_at: "2026-09-12T22:00:00Z",
            age_seconds: 7_200,
            priority: null,
            owner: { document: { project: "OPS", slug: "runbook", name: "Runbook" } },
            human_replied: true,
            last_reply: {
              author: { kind: "user", id: "alice" },
              created_at: "2026-09-13T00:00:00Z",
            },
            waiting_on: "agent",
          },
        ],
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_open_asks",
      args: {},
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-1",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result).toEqual({
      text: [
        "2 unanswered asks you authored on active issues and project documents.",
        "",
        "Waiting on human (1):",
        `- 1m 5s · P0 · LEGION-1: Reminder · ${releaseQuestion} · http://dispatch.test/issues/LEGION-1?ask=ask-1`,
        "",
        "Waiting on agent (1):",
        "- 2h · OPS / Runbook · Which region? · http://dispatch.test/projects/OPS/documents/runbook?ask=ask-2",
      ].join("\n"),
      details: {
        session_id: "session-1",
        as_of: "2026-09-13T00:00:00Z",
        opened_since: false,
        count: 2,
        waiting_on_human: 1,
        waiting_on_agent: 1,
        asks: expect.any(Array),
      },
    });
    expect(requests).toEqual(["/api/v1/asks/open?author_session=session-1"]);
    expect(dispatchFollowNotice(result.details)).toBeNull();
  });

  test("dispatch_open_asks reports its active-owner scope when no asks are open", async () => {
    const fetchImpl = async (): Promise<Response> =>
      response({
        session_id: "session-1",
        as_of: "2026-09-13T00:00:00Z",
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      });

    await expect(
      executeDispatchTool({
        tool: "dispatch_open_asks",
        args: {},
        cwd: "/workspace",
        host: "omp",
        sessionId: "session-1",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as unknown as typeof fetch,
      })
    ).resolves.toEqual({
      text: [
        "There are no unanswered asks for this session.",
        "Scope: active open asks you authored on open issues and project documents.",
      ].join("\n"),
      details: {
        session_id: "session-1",
        as_of: "2026-09-13T00:00:00Z",
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      },
    });
  });

  test("dispatch_open_asks with a project lists every open ask in that project without a host session", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      return response({
        session_id: "",
        as_of: "2026-09-13T00:00:00Z",
        opened_since: false,
        count: 1,
        waiting_on_human: 1,
        waiting_on_agent: 0,
        asks: [
          {
            id: "ask-3",
            ref: "/projects/CORE/documents/runbook?ask=ask-3",
            question: "Which region?",
            kind: "question",
            urgency: "med",
            created_at: "2026-09-12T22:00:00Z",
            age_seconds: 7_200,
            priority: null,
            owner: { document: { project: "CORE", slug: "runbook", name: "Runbook" } },
            human_replied: false,
            last_reply: null,
            waiting_on: "human",
          },
        ],
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_open_asks",
      args: { project: "CORE" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(requests).toEqual(["/api/v1/asks/open?project=CORE"]);
    expect(result.details).toMatchObject({ count: 1 });
  });

  test("dispatch_open_asks rejects a missing host session before it reads Dispatch", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_open_asks",
        args: {},
        cwd: "/workspace",
        host: "omp",
        sessionId: " ",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow("host session id is required");
  });

  test("dispatch_open_asks propagates Dispatch errors rather than treating them as no asks", async () => {
    const fetchImpl = async (): Promise<Response> =>
      new Response(
        JSON.stringify({ code: "SERVICE_UNAVAILABLE", error: "Dispatch is unavailable" }),
        {
          status: 503,
          headers: { "Content-Type": "application/json" },
        }
      );

    await expect(
      executeDispatchTool({
        tool: "dispatch_open_asks",
        args: {},
        cwd: "/workspace",
        host: "omp",
        sessionId: "session-1",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as unknown as typeof fetch,
      })
    ).rejects.toMatchObject({
      name: "DispatchServiceError",
      code: "SERVICE_UNAVAILABLE",
      status: 503,
    });
  });

  test("dispatch_claim claims for the calling session and releases with release: true", async () => {
    const calls: Array<{ method: string; pathname: string; body: unknown }> = [];
    const claim = {
      actor: { kind: "session", id: "session-one", origin: { session_title: "Implementer" } },
      at: "2026-09-24T06:00:00Z",
    };
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      const method = init?.method ?? "GET";
      calls.push({
        method,
        pathname: target.pathname,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
      });
      return response({
        key: "DSP-1",
        status: "todo",
        claim: method === "POST" ? claim : null,
      });
    };
    const run = (args: Record<string, unknown>) =>
      executeDispatchTool({
        tool: "dispatch_claim",
        args,
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        sessionId: "session-one",
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      });

    const claimed = await run({ issue: "DSP-1" });
    expect(calls[0]?.method).toBe("POST");
    expect(calls[0]?.pathname).toBe("/api/v1/issues/DSP-1/claim");
    // The claimant is this session and nothing else names it.
    expect(calls[0]?.body).toEqual({
      actor: expect.objectContaining({ kind: "session", id: "session-one" }),
    });
    expect(claimed.text).toContain("DSP-1: claimed by you since 2026-09-24T06:00:00Z");
    expect(claimed.text).toContain("move it to in_progress");
    expect(claimed.details).toMatchObject({ issue: "DSP-1", status: "todo", claim });

    const released = await run({ issue: "DSP-1", release: true });
    expect(calls[1]?.method).toBe("DELETE");
    expect(calls[1]?.pathname).toBe("/api/v1/issues/DSP-1/claim");
    expect(released.text).toContain("DSP-1: claim released");
    expect(released.details).toMatchObject({ issue: "DSP-1", claim: null });
  });

  test("a refused dispatch_claim reaches the caller with the code it must act on", async () => {
    const refusal =
      "session session-two (worker session-two) claimed this issue at 2026-09-24T06:00:00Z and is still running; ask that session to release it, or a human can force the claim";
    const fetchImpl = async (_url: RequestInfo | URL): Promise<Response> =>
      new Response(JSON.stringify({ code: "ISSUE_CLAIMED", error: refusal }), {
        status: 409,
        headers: { "Content-Type": "application/json" },
      });

    // ISSUE_CLAIMED and CLAIM_CONTENDED ask for different things, and the tool description
    // names both by code, so the code has to survive into what the host shows.
    await expect(
      executeDispatchTool({
        tool: "dispatch_claim",
        args: { issue: "DSP-1" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        sessionId: "session-one",
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).rejects.toThrow(`ISSUE_CLAIMED: ${refusal}`);
  });

  test("a re-coded refusal keeps every field the server sent beside its message", async () => {
    const current = {
      document: "sha256:current-document",
      blocks: [{ id: "block-1", token: "sha256:current-block" }],
    };
    const mismatches = [
      {
        scope: "block",
        block_id: "block-1",
        expected: "sha256:stale-block",
        current: "sha256:current-block",
      },
    ];
    const fetchImpl = async (_url: RequestInfo | URL): Promise<Response> =>
      new Response(
        JSON.stringify({ code: "PRECONDITION_FAILED", error: "stale", current, mismatches }),
        { status: 409, headers: { "Content-Type": "application/json" } }
      );

    // Putting the code into the message rebuilds the error, so every other field a caller
    // reads off it has to be carried over rather than dropped in the rebuild.
    await expect(
      executeDispatchTool({
        tool: "dispatch_claim",
        args: { issue: "DSP-1" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        sessionId: "session-one",
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).rejects.toEqual(
      expect.objectContaining({
        name: "DispatchServiceError",
        code: "PRECONDITION_FAILED",
        status: 409,
        message: "PRECONDITION_FAILED: stale",
        current,
        mismatches,
      })
    );
  });

  test("a claim reads by the holder's live title and says when its session is not running, or when nobody can tell", async () => {
    const agentCalls: string[] = [];
    let registryAnswers = true;
    const claimOf = (id: string, stamped: string) => ({
      actor: { kind: "session", id, origin: { session_title: stamped } },
      at: "2026-09-24T06:00:00Z",
    });
    const serve =
      (claim: unknown) =>
      async (url: RequestInfo | URL): Promise<Response> => {
        const target = new URL(String(url));
        if (target.pathname === "/api/v1/agents") {
          agentCalls.push(target.pathname);
          if (!registryAnswers) {
            return new Response(JSON.stringify({ code: "INTERNAL", error: "listener down" }), {
              status: 502,
              headers: { "Content-Type": "application/json" },
            });
          }
          return response([{ session_id: "session-one", title: "Live registry title" }]);
        }
        if (target.pathname === "/api/v1/issues/DSP-1") {
          return response({
            key: "DSP-1",
            title: "Claimed work",
            status: "todo",
            priority: null,
            assignee: null,
            claim,
            components: {
              mode: "inherit",
              ids: [],
              unknown: [],
              reason: null,
              inherited_from: null,
            },
            route: null,
            external_links: [],
            open_asks: [],
            last_seq: 0,
            labels: [],
          });
        }
        if (target.pathname === "/api/v1/issues/DSP-1/events") return response([]);
        if (target.pathname === "/api/v1/issues/DSP-1/references") {
          return response({ members: [], truncated: false });
        }
        if (target.pathname === "/api/v1/references") {
          return response(emptyGraph("issue"));
        }
        throw new Error(`unexpected request: ${target.pathname}`);
      };
    const read = (claim: unknown) =>
      executeDispatchTool({
        tool: "dispatch_read",
        args: { issue: "DSP-1" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: serve(claim) as typeof fetch,
      });

    // A session holds it: the live title wins over the one stamped on the claim, and a session
    // the registry lists carries no marker.
    const claimed = await read(claimOf("session-one", "Stamped title"));
    expect(claimed.text).toContain("Claimed by: Live registry title since 2026-09-24T06:00:00Z\n");
    expect(claimed.text).not.toContain("Stamped title");
    expect(agentCalls).toEqual(["/api/v1/agents"]);

    // A holder the loaded registry does not list is named by its stamped title and reads as not
    // running, as the dashboard's claim chip does: that claim is free to take.
    const unlisted = await read(claimOf("session-gone", "Stamped title"));
    expect(unlisted.text).toContain(
      "Claimed by: Stamped title since 2026-09-24T06:00:00Z · not running\n"
    );
    expect(agentCalls).toHaveLength(2);

    // A registry that cannot be read says nothing about any session: neither running nor not.
    registryAnswers = false;
    const blind = await read(claimOf("session-one", "Stamped title"));
    expect(blind.text).toContain(
      "Claimed by: Stamped title since 2026-09-24T06:00:00Z · liveness unknown\n"
    );
    expect(blind.text).not.toContain("not running");
    expect(agentCalls).toHaveLength(3);

    // Nothing a session holds: no registry request at all, and a person's claim never lapses.
    const unclaimed = await read(null);
    expect(unclaimed.text).toContain("Claimed by: nobody");
    const human = await read({ actor: { kind: "user", id: "alice" }, at: "2026-09-24T06:00:00Z" });
    expect(human.text).toContain("Claimed by: alice since 2026-09-24T06:00:00Z\n");
    expect(agentCalls).toHaveLength(3);
  });

  test("dispatch_issues passes each optional filter through, omits absent ones, and returns the documented row shape", async () => {
    const requests: URL[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target);
      // A session holds AGENTC-1, so the rows are labelled from the live registry.
      if (target.pathname === "/api/v1/agents") {
        return response([{ session_id: "s1", title: "Live registry title" }]);
      }
      return response({
        issues: [
          {
            key: "AGENTC-1",
            title: "First",
            status: "todo",
            priority: 1,
            rank: "a",
            labels: ["bug"],
            parent: null,
            assignee: "alice",
            updated_at: "2026-09-13T00:00:00Z",
            last_seq: 4,
            open_asks: 2,
            claim: {
              actor: { kind: "session", id: "s1", origin: { session_title: "Implementer" } },
              at: "2026-09-13T01:00:00Z",
            },
            route: "role:sre",
            route_status: "no_holder",
            route_holder: null,
          },
        ],
        total: 1,
        limit: 50,
        offset: 0,
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_issues",
      args: {
        project: "AGENTC",
        status: "todo",
        parent: "AGENTC-9",
        label: "bug",
        priority: [0, 1, null],
        updated_since: "2026-09-01T00:00:00Z",
        route_status: "no_holder",
      },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(requests.map((request) => request.pathname)).toEqual([
      "/api/v1/issues",
      "/api/v1/agents",
    ]);
    // Each priority repeats as its own parameter, and null asks for issues with no priority.
    expect([...(requests[0]?.searchParams ?? [])]).toEqual([
      ["project", "AGENTC"],
      ["status", "todo"],
      ["parent", "AGENTC-9"],
      ["label", "bug"],
      ["priority", "0"],
      ["priority", "1"],
      ["priority", "none"],
      ["updated_since", "2026-09-01T00:00:00Z"],
      ["route_status", "no_holder"],
      ["limit", "50"],
      ["offset", "0"],
    ]);
    // A route that reaches nobody is what the owner audit reads, so the row says so.
    expect(result.text).toContain("AGENTC-1 [todo] P1 First · 2 open asks · claimed by ");
    expect(result.text).toContain(" · route role:sre (nobody holds it right now)");
    expect(result.details).toEqual({
      issues: [
        {
          key: "AGENTC-1",
          title: "First",
          status: "todo",
          priority: 1,
          parent: null,
          claim: {
            actor: { kind: "session", id: "s1", origin: { session_title: "Implementer" } },
            at: "2026-09-13T01:00:00Z",
          },
          labels: ["bug"],
          open_asks: 2,
          route: "role:sre",
          route_status: "no_holder",
          route_holder: null,
          updated_at: "2026-09-13T00:00:00Z",
        },
      ],
      total: 1,
      offset: 0,
      limit: 50,
    });
  });

  test("dispatch_read says whether the issue's route reaches anyone", async () => {
    const read = async (reach: Record<string, unknown>) => {
      const agentCalls: string[] = [];
      const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
        const pathname = new URL(String(url)).pathname;
        if (pathname === "/api/v1/issues/DSP-42") {
          return response({
            key: "DSP-42",
            title: "Dispatch issue",
            status: "todo",
            priority: 2,
            assignee: null,
            claim: null,
            components: {
              mode: "inherit",
              ids: [],
              unknown: [],
              reason: null,
              inherited_from: null,
            },
            open_asks: [],
            last_seq: 0,
            external_links: [],
            labels: [],
            ...reach,
          });
        }
        if (pathname === "/api/v1/agents") {
          agentCalls.push(pathname);
          return response([{ session_id: "ses-sre", title: "SRE on call" }]);
        }
        if (pathname === "/api/v1/issues/DSP-42/events") return response([]);
        if (pathname === "/api/v1/issues/DSP-42/references" || pathname === "/api/v1/references") {
          return response({ node: { kind: "issue", id: "DSP-42" }, edges: [], members: [] });
        }
        throw new Error(`unexpected request: ${pathname}`);
      };
      const result = await executeDispatchTool({
        tool: "dispatch_read",
        args: { issue: "DSP-42" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      });
      return { text: result.text, agentCalls: agentCalls.length };
    };

    const unheld = await read({ route: "role:sre", route_status: "no_holder", route_holder: null });
    expect(unheld.text).toContain("Route: role:sre (nobody holds it right now)\n");
    expect(unheld.agentCalls).toBe(0);
    const gone = await read({
      route: "session:ses-gone",
      route_status: "no_holder",
      route_holder: null,
    });
    expect(gone.text).toContain(
      "Route: session:ses-gone (that session is not running right now)\n"
    );
    const blind = await read({ route: "role:sre", route_status: "unknown", route_holder: null });
    expect(blind.text).toContain(
      "Route: role:sre (the Envoy listener did not answer, so whether it reaches anyone is unknown)\n"
    );
    // A held role names its holder the way a claim does: by the live registry's title.
    const held = await read({ route: "role:sre", route_status: "live", route_holder: "ses-sre" });
    expect(held.text).toContain("Route: role:sre (held by SRE on call)\n");
    expect(held.agentCalls).toBe(1);
    const unrouted = await read({ route: null, route_status: null, route_holder: null });
    expect(unrouted.text).toContain("Route: none\n");
  });

  // Every Dispatch the hosts reach pages the listing (sjawhar/legion#1612), so an array answered to
  // dispatch_issues means an older server or a regression, and the agent gets a refusal naming both
  // rather than a page the client cut from every issue. The message itself is pinned in
  // dispatch-http.test.ts.
  test("dispatch_issues refuses Dispatch's unpaged array and names the change it lacks", async () => {
    const requests: URL[] = [];
    const issues = Array.from({ length: 5 }, (_, index) => ({
      key: `AGENTC-${index}`,
      title: `Issue ${index}`,
      status: "todo",
      priority: null,
      rank: "a",
      labels: [],
      parent: null,
      assignee: null,
      updated_at: "2026-09-13T00:00:00Z",
      last_seq: 1,
      open_asks: 0,
    }));
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      requests.push(new URL(String(url)));
      return response(issues);
    };

    await expect(
      executeDispatchTool({
        tool: "dispatch_issues",
        args: { project: "AGENTC", limit: 2 },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).rejects.toThrow("sjawhar/legion#1612");
    expect(requests).toHaveLength(1);
    expect(Object.fromEntries(requests[0]?.searchParams ?? [])).toEqual({
      project: "AGENTC",
      limit: "2",
      offset: "0",
    });
  });

  test("dispatch_issues returns the last 50 after offset 250 and renders the total", async () => {
    const issues = Array.from({ length: 300 }, (_, index) => ({
      key: `AGENTC-${index}`,
      title: `Issue ${index}`,
      status: "todo",
      priority: null,
      rank: "a",
      labels: [],
      parent: null,
      assignee: null,
      updated_at: "2026-09-13T00:00:00Z",
      last_seq: 1,
      open_asks: 0,
    }));
    const served: Array<{ limit: string | null; offset: string | null }> = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const query = new URL(String(url)).searchParams;
      served.push({ limit: query.get("limit"), offset: query.get("offset") });
      const limit = Number(query.get("limit"));
      const offset = Number(query.get("offset"));
      return response({
        issues: issues.slice(offset, offset + limit),
        total: issues.length,
        limit,
        offset,
      });
    };

    const defaultPage = await executeDispatchTool({
      tool: "dispatch_issues",
      args: { project: "AGENTC" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(defaultPage.text).toContain("50 issues in AGENTC (showing 1-50 of 300)");
    expect(defaultPage.details).toMatchObject({ total: 300, offset: 0, limit: 50 });

    const result = await executeDispatchTool({
      tool: "dispatch_issues",
      args: { project: "AGENTC", offset: 250 },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toContain("50 issues in AGENTC (showing 251-300 of 300)");
    expect(result.text).toContain("AGENTC-250 [todo] Issue 250");
    expect(result.text).toContain("AGENTC-299 [todo] Issue 299");
    expect(result.details).toMatchObject({ total: 300, offset: 250, limit: 50 });
    expect(result.details.issues).toHaveLength(50);
    // The page size and start reach Dispatch as the tool was given them, 50 when it names none.
    expect(served).toEqual([
      { limit: "50", offset: "0" },
      { limit: "50", offset: "250" },
    ]);
  });

  test("dispatch_issues names an empty page past the listing's end", async () => {
    const fetchImpl = async (_url: RequestInfo | URL): Promise<Response> =>
      response({ issues: [], total: 1, limit: 50, offset: 1 });

    const result = await executeDispatchTool({
      tool: "dispatch_issues",
      args: { project: "AGENTC", offset: 1 },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe("No issues in AGENTC. (showing 0-0 of 1)");
    expect(result.details).toMatchObject({ total: 1, offset: 1, limit: 50 });
    expect(result.details.issues).toEqual([]);
  });

  test("dispatch_issue returns duplicate candidates instead of throwing", async () => {
    const candidates = [
      {
        key: "LEGION-12",
        title: "Global search across issues and documents",
        status: "triage",
        snippet: "<mark>Global</mark> <mark>search</mark> …",
        shared_terms: 4,
        href: "/issues/LEGION-12",
      },
    ];
    const fetchImpl = async (_input: RequestInfo | URL, _init?: RequestInit): Promise<Response> =>
      new Response(
        JSON.stringify({
          error: "possible duplicate of LEGION-12: Global search across issues and documents",
          code: "POSSIBLE_DUPLICATE",
          candidates,
        }),
        { status: 409, headers: { "Content-Type": "application/json" } }
      );

    const result = await executeDispatchTool({
      tool: "dispatch_issue",
      args: { project: "LEGION", title: "Global search across issues and documents" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result).toEqual({
      text: [
        'Not created: "Global search across issues and documents" looks like a duplicate.',
        "LEGION-12 [triage] Global search across issues and documents → http://dispatch.test/issues/LEGION-12",
        "Reference the existing issue, or call dispatch_issue again with force: true after reading it.",
      ].join("\n"),
      details: { duplicates: candidates },
    });
  });

  test("dispatch_issue forwards force", async () => {
    const requests: Array<{ readonly body: unknown }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      if (new URL(String(url)).pathname === "/api/v1/projects/LEGION/architecture-source") {
        return sourceNull();
      }
      requests.push({ body: JSON.parse(String(init?.body)) });
      return response({
        key: "LEGION-13",
        title: "New global search work",
        project: "LEGION",
        components: issueComponents("inherit", []),
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_issue",
      args: { project: "LEGION", title: "New global search work", force: true },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe(
      "Created LEGION-13: New global search work (not subscribed to LEGION-13; envoy_subscribe notifications.dispatch.issue.LEGION-13.> for every event on it)"
    );
    expect(result.details).toEqual({ issue: "LEGION-13" });
    expect(dispatchFollowNotice(result.details)).toBeNull();
    expect(requests).toEqual([{ body: expect.objectContaining({ force: true }) }]);
  });

  test("guides an unassigned new issue to architecture components when its project has a source", async () => {
    const { result, requests } = await createIssueWithComponents(issueComponents("inherit", []));

    expect(result.text).toBe([createdIssueLine, architectureComponentsGuidance].join("\n"));
    expect(requests).toEqual([
      "POST /api/v1/issues",
      "GET /api/v1/projects/LEGION/architecture-source",
    ]);
  });

  test("checks the canonical project returned after Dispatch trims the requested project", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(`${init?.method ?? "GET"} ${target.pathname}`);
      if (target.pathname === "/api/v1/issues") {
        expect(JSON.parse(String(init?.body))).toMatchObject({ project: " LEGION " });
        return response({
          key: "LEGION-216",
          title: "Architecture work",
          project: "LEGION",
          components: issueComponents("inherit", []),
        });
      }
      if (target.pathname === "/api/v1/projects/LEGION/architecture-source") {
        return response(architectureSource("LEGION"));
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_issue",
      args: { project: " LEGION ", title: "Architecture work" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toContain(architectureComponentsGuidance);
    expect(requests).toEqual([
      "POST /api/v1/issues",
      "GET /api/v1/projects/LEGION/architecture-source",
    ]);
  });

  test("checks the canonical project returned after Dispatch resolves an external reference", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(`${init?.method ?? "GET"} ${target.pathname}`);
      if (target.pathname === "/api/v1/issues") {
        expect(JSON.parse(String(init?.body))).toMatchObject({
          project: "",
          external: "owner/repo#42",
        });
        return response({
          key: "LEGION-216",
          title: "Architecture work",
          project: "LEGION",
          components: issueComponents("inherit", []),
        });
      }
      if (target.pathname === "/api/v1/projects/LEGION/architecture-source") {
        return response(architectureSource("LEGION"));
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_issue",
      args: { project: "", title: "Architecture work", external: "owner/repo#42" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toContain(architectureComponentsGuidance);
    expect(requests).toEqual([
      "POST /api/v1/issues",
      "GET /api/v1/projects/LEGION/architecture-source",
    ]);
  });

  // A rollout mixes servers and clients, so both answers a server gives for a project with no
  // source mean no source: a current server's 200 null and an older one's 404 SOURCE_NOT_FOUND.
  test.each([
    ["200 null", "null"],
    ["404 SOURCE_NOT_FOUND", "source-not-found"],
  ] as const)("does not guide an unassigned new issue when the source read answers %s", async (_answer, source) => {
    const { result, requests } = await createIssueWithComponents(
      issueComponents("inherit", []),
      source
    );

    expect(result.text).toBe(createdIssueLine);
    expect(requests).toEqual([
      "POST /api/v1/issues",
      "GET /api/v1/projects/LEGION/architecture-source",
    ]);
  });

  test("reports a 404 other than SOURCE_NOT_FOUND as a source it could not check", async () => {
    const { result } = await createIssueWithComponents(
      issueComponents("inherit", []),
      "project-not-found"
    );

    expect(result.text).toBe(
      [
        createdIssueLine,
        'Could not check whether project LEGION has an architecture model: project LEGION not found. Review the `dispatch` skill, "Architecture components", to attach it to the parts it changes or mark it as non-architectural with a reason.',
      ].join("\n")
    );
  });

  test("does not guide a child whose live component attachment comes from its parent", async () => {
    const { result, requests } = await createIssueWithComponents(
      issueComponents("explicit", ["web"], [], null, "LEGION-200")
    );

    expect(result.text).toBe(createdIssueLine);
    expect(requests).toEqual(["POST /api/v1/issues"]);
  });

  test("does not guide a new issue deliberately classified as non-architectural", async () => {
    const { result, requests } = await createIssueWithComponents(
      issueComponents("none", [], [], "hiring, not code")
    );

    expect(result.text).toBe(createdIssueLine);
    expect(requests).toEqual(["POST /api/v1/issues"]);
  });

  test("guides a new issue whose only component attachment has been retired", async () => {
    const { result, requests } = await createIssueWithComponents(
      issueComponents("explicit", [], ["legacy-ui"])
    );

    expect(result.text).toBe([createdIssueLine, architectureComponentsGuidance].join("\n"));
    expect(requests).toEqual([
      "POST /api/v1/issues",
      "GET /api/v1/projects/LEGION/architecture-source",
    ]);
  });

  test("reports an unavailable architecture source check without undoing issue creation", async () => {
    const { result, requests } = await createIssueWithComponents(
      issueComponents("inherit", []),
      "fails"
    );

    expect(result.text).toBe([createdIssueLine, architectureSourceUnavailableGuidance].join("\n"));
    expect(requests).toEqual([
      "POST /api/v1/issues",
      "GET /api/v1/projects/LEGION/architecture-source",
    ]);
  });

  test("does not fetch an architecture source for a new issue with a live direct attachment", async () => {
    const { result, requests } = await createIssueWithComponents(
      issueComponents("explicit", ["web"])
    );

    expect(result.text).toBe(createdIssueLine);
    expect(requests).toEqual(["POST /api/v1/issues"]);
  });

  test("dispatch_issue forwards initial labels", async () => {
    const requests: Array<{ readonly body: unknown }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      if (new URL(String(url)).pathname === "/api/v1/projects/LEGION/architecture-source") {
        return sourceNull();
      }
      requests.push({ body: JSON.parse(String(init?.body)) });
      return response({
        key: "LEGION-13",
        title: "New global search work",
        project: "LEGION",
        components: issueComponents("inherit", []),
      });
    };

    await executeDispatchTool({
      tool: "dispatch_issue",
      args: { project: "LEGION", title: "New global search work", labels: ["frontend", "urgent"] },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(requests).toEqual([
      { body: expect.objectContaining({ labels: ["frontend", "urgent"] }) },
    ]);
  });

  test("dispatch_issue forwards an initial priority", async () => {
    const requests: Array<{ readonly body: unknown }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      if (new URL(String(url)).pathname === "/api/v1/projects/LEGION/architecture-source") {
        return sourceNull();
      }
      requests.push({ body: JSON.parse(String(init?.body)) });
      return response({
        key: "LEGION-13",
        title: "Priority work",
        project: "LEGION",
        components: issueComponents("inherit", []),
      });
    };

    await executeDispatchTool({
      tool: "dispatch_issue",
      args: { project: "LEGION", title: "Priority work", priority: 1 },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(requests).toEqual([{ body: expect.objectContaining({ priority: 1 }) }]);
  });

  test("dispatch_issue forwards an assignee login", async () => {
    const requests: Array<{ readonly body: unknown }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      if (new URL(String(url)).pathname === "/api/v1/projects/LEGION/architecture-source") {
        return sourceNull();
      }
      requests.push({ body: JSON.parse(String(init?.body)) });
      return response({
        key: "LEGION-14",
        title: "Assigned work",
        project: "LEGION",
        components: issueComponents("inherit", []),
      });
    };

    await executeDispatchTool({
      tool: "dispatch_issue",
      args: { project: "LEGION", title: "Assigned work", assignee: "alice" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(requests).toEqual([{ body: expect.objectContaining({ assignee: "alice" }) }]);
  });

  test("dispatch_whoami returns the session and the personal token's owner", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      requests.push(new URL(String(url)).pathname);
      return response({ kind: "agent", owner: "alice", service: null });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_whoami",
      args: {},
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-1",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(requests).toEqual(["/api/v1/whoami"]);
    expect(result.details).toEqual({ session: "session-1", owner: "alice", service: null });
    expect(result.text).toContain("alice");
  });

  test("dispatch_whoami reports a null owner under the shared token", async () => {
    const fetchImpl = async (): Promise<Response> =>
      response({ kind: "agent", owner: null, service: null });

    const result = await executeDispatchTool({
      tool: "dispatch_whoami",
      args: {},
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-1",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as unknown as typeof fetch,
    });

    expect(result.details).toEqual({ session: "session-1", owner: null, service: null });
    expect(result.text).toContain("shared token");
  });

  test("dispatch_whoami names the service subject a verified token authenticated", async () => {
    const fetchImpl = async (): Promise<Response> =>
      response({
        kind: "agent",
        owner: null,
        service: "system:serviceaccount:legion:legion-worker",
      });

    const result = await executeDispatchTool({
      tool: "dispatch_whoami",
      args: {},
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-1",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as unknown as typeof fetch,
    });

    // details mirrors the persisted value, so it keeps the subject whole; only the
    // sentence a human reads is labelled.
    expect(result.details).toEqual({
      session: "session-1",
      owner: null,
      service: "system:serviceaccount:legion:legion-worker",
    });
    expect(result.text).toContain("runs as service legion/legion-worker");
    expect(result.text).not.toContain("shared token");
  });

  test("dispatch_issue_update moves status and merges external links by URL as the session", async () => {
    const requests: Array<{ method: string; pathname: string; body?: unknown }> = [];
    const existingLink = { url: "https://ci.example/run/1", kind: "url" };
    const pullRequest = "https://github.com/owner/repo/pull/7";
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const pathname = new URL(String(url)).pathname;
      const method = init?.method ?? "GET";
      requests.push({
        method,
        pathname,
        ...(init?.body === undefined ? {} : { body: JSON.parse(String(init.body)) }),
      });
      if (pathname !== "/api/v1/issues/AGENTC-175") {
        throw new Error(`unexpected request: ${method} ${pathname}`);
      }
      if (method === "GET") {
        return response({
          key: "AGENTC-175",
          title: "Issue update tool",
          status: "in_progress",
          labels: [],
          route: null,
          external_links: [existingLink],
        });
      }
      return response({
        key: "AGENTC-175",
        title: "Issue update tool",
        status: "testing",
        labels: [],
        route: null,
        external_links: [existingLink, { url: pullRequest, kind: "url" }],
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_issue_update",
      args: {
        issue: "AGENTC-175",
        status: "testing",
        external_links: [pullRequest, existingLink.url, pullRequest],
      },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-42",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result).toEqual({
      text:
        `AGENTC-175: status in_progress -> testing; linked ${pullRequest} (2 links) ` +
        "(not subscribed to AGENTC-175; envoy_subscribe notifications.dispatch.issue.AGENTC-175.> for every event on it)",
      details: {
        issue: "AGENTC-175",
        status: "testing",
        external_links: [existingLink.url, pullRequest],
      },
    });
    expect(dispatchFollowNotice(result.details)).toBeNull();
    expect(requests).toEqual([
      { method: "GET", pathname: "/api/v1/issues/AGENTC-175" },
      {
        method: "PATCH",
        pathname: "/api/v1/issues/AGENTC-175",
        body: {
          status: "testing",
          external_links: [existingLink, { url: pullRequest }],
          actor: {
            kind: "session",
            id: "session-42",
            origin: expect.objectContaining({ host: "omp", cwd: "/workspace" }),
          },
        },
      },
    ]);
  });

  test("dispatch_issue_update surfaces the server's error code in the thrown message", async () => {
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> =>
      (init?.method ?? "GET") === "GET"
        ? response({
            key: "AGENTC-175",
            title: "x",
            status: "todo",
            labels: [],
            external_links: [],
          })
        : new Response(
            JSON.stringify({
              code: "INVALID_STATUS",
              error: "status is not in the Legion lifecycle",
            }),
            { status: 400, headers: { "Content-Type": "application/json" } }
          );

    await expect(
      executeDispatchTool({
        tool: "dispatch_issue_update",
        args: { issue: "AGENTC-175", status: "testing" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).rejects.toThrow("INVALID_STATUS: status is not in the Legion lifecycle");
  });

  test("dispatch_issue_update names the new URL when a pre-EXTERNAL_LINK_TAKEN server answers 500", async () => {
    const pullRequest = "https://github.com/owner/repo/pull/7";
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> =>
      (init?.method ?? "GET") === "GET"
        ? response({
            key: "AGENTC-175",
            title: "x",
            status: "todo",
            labels: [],
            external_links: [],
          })
        : new Response(JSON.stringify({ code: "INTERNAL", error: "internal server error" }), {
            status: 500,
            headers: { "Content-Type": "application/json" },
          });

    await expect(
      executeDispatchTool({
        tool: "dispatch_issue_update",
        args: { issue: "AGENTC-175", external_links: [pullRequest] },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).rejects.toThrow(
      `INTERNAL: internal server error; one of ${pullRequest} may already be linked from another issue (a URL links exactly one issue)`
    );
  });

  // The taken-link hint reads Dispatch's own 500 from before EXTERNAL_LINK_TAKEN. A gateway's 500
  // page in its place is not that answer: naming a link clash there gives the agent a second,
  // wrong diagnosis, so the refusal carries only the gateway's answer and the client's advice for
  // a write.
  test("dispatch_issue_update gives a gateway's 500 on external_links no link-clash hint", async () => {
    const pullRequest = "https://github.com/owner/repo/pull/7";
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> =>
      (init?.method ?? "GET") === "GET"
        ? response({
            key: "AGENTC-175",
            title: "x",
            status: "todo",
            labels: [],
            external_links: [],
          })
        : new Response("<html><body><h1>500 Internal Server Error</h1></body></html>", {
            status: 500,
            statusText: "Internal Server Error",
            headers: { "Content-Type": "text/html" },
          });

    const failure = await executeDispatchTool({
      tool: "dispatch_issue_update",
      args: { issue: "AGENTC-175", external_links: [pullRequest] },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    }).then(
      () => "",
      (error: Error) => error.message
    );

    expect(failure).toBe(
      "HTTP_500: PATCH http://dispatch.test/api/v1/issues/AGENTC-175 answered 500 Internal Server " +
        'Error with a body that is not Dispatch\'s error JSON ("500 Internal Server Error"), which ' +
        "looks like a proxy or gateway page rather than Dispatch's own answer, so the write may or " +
        "may not have reached Dispatch: check whether it took effect before retrying it."
    );
  });

  test("dispatch_issue_update refuses a call with nothing to change before any request", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_issue_update",
        args: { issue: "AGENTC-175" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(/Issue update requires at least one field besides issue/);
  });

  test("dispatch_issue_update refuses status done without a reason before any request", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    for (const args of [
      { issue: "AGENTC-175", status: "done" },
      { issue: "AGENTC-175", status: "done", reason: " " },
    ]) {
      await expect(
        executeDispatchTool({
          tool: "dispatch_issue_update",
          args,
          cwd: "/workspace",
          host: "omp",
          config,
          env: {},
          exec: repoExec("owner/repo"),
          fetchImpl,
        })
      ).rejects.toThrow(
        /status done requires reason, a non-empty note saying why the issue is closing, posted on the issue before it closes because a closed issue refuses messages, comments, and artifacts/
      );
    }
  });

  /** Records every request; the messages route and the PATCH answer with the given responses. */
  function closingServer(answers: { message: () => Response; patch: () => Response }) {
    const requests: Array<{ method: string; pathname: string; body?: unknown }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const pathname = new URL(String(url)).pathname;
      const method = init?.method ?? "GET";
      requests.push({
        method,
        pathname,
        ...(init?.body === undefined ? {} : { body: JSON.parse(String(init.body)) }),
      });
      if (pathname === "/api/v1/issues/AGENTC-175/messages") return answers.message();
      if (pathname !== "/api/v1/issues/AGENTC-175") {
        throw new Error(`unexpected request: ${method} ${pathname}`);
      }
      if (method === "GET") {
        return response({
          key: "AGENTC-175",
          title: "x",
          status: "retro",
          labels: [],
          route: null,
          external_links: [],
        });
      }
      return answers.patch();
    };
    return { requests, fetchImpl: fetchImpl as typeof fetch };
  }

  const refusal = (status: number, code: string, error: string) =>
    new Response(JSON.stringify({ code, error }), {
      status,
      headers: { "Content-Type": "application/json" },
    });

  const closeCall = (fetchImpl: typeof fetch) =>
    executeDispatchTool({
      tool: "dispatch_issue_update",
      args: { issue: "AGENTC-175", status: "done", reason: "Shipped in owner/repo#7." },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-42",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl,
    });

  test("dispatch_issue_update posts the reason as a message, then closes the issue", async () => {
    const server = closingServer({
      message: () => response({ id: "message-7", issue_key: "AGENTC-175" }),
      patch: () =>
        response({
          key: "AGENTC-175",
          title: "x",
          status: "done",
          labels: [],
          route: null,
          external_links: [],
        }),
    });

    const result = await closeCall(server.fetchImpl);

    expect(server.requests.map(({ method, pathname }) => `${method} ${pathname}`)).toEqual([
      "GET /api/v1/issues/AGENTC-175",
      "POST /api/v1/issues/AGENTC-175/messages",
      "PATCH /api/v1/issues/AGENTC-175",
    ]);
    expect(server.requests[1]?.body).toMatchObject({
      body: "Shipped in owner/repo#7.",
      actor: { kind: "session", id: "session-42" },
    });
    expect(server.requests[2]?.body).toMatchObject({ status: "done" });
    expect(server.requests[2]?.body).not.toHaveProperty("reason");
    expect(result.text).toStartWith(
      "AGENTC-175: reason posted as message message-7 (dispatch://AGENTC-175/message/message-7); status retro -> done"
    );
    expect(result.details).toMatchObject({
      issue: "AGENTC-175",
      status: "done",
      message: "message-7",
    });
  });

  test("dispatch_issue_update leaves the issue open when the reason cannot be posted", async () => {
    const server = closingServer({
      message: () => refusal(409, "ISSUE_CLOSED", "issue is closed"),
      patch: () => {
        throw new Error("the close must not be sent");
      },
    });

    await expect(closeCall(server.fetchImpl)).rejects.toThrow(
      "ISSUE_CLOSED: issue is closed; the reason was not posted, so the close was not sent"
    );
    expect(server.requests.map(({ method }) => method)).toEqual(["GET", "POST"]);
  });

  const gatewayPage = (status: number, statusText: string) =>
    new Response(`<html><body><h1>${status} ${statusText}</h1></body></html>`, {
      status,
      statusText,
      headers: { "Content-Type": "text/html" },
    });

  // A request that got no answer, the client's timeout and a transport error, each with the
  // message the client reports for it. The reason's post and the close's PATCH each add their own
  // account after it, as a sentence of its own, since both messages end one.
  const unanswered = [
    [
      (): Response => {
        throw new DOMException("The operation timed out.", "TimeoutError");
      },
      "The operation timed out.",
    ],
    [
      (): Response => {
        throw new TypeError("fetch failed");
      },
      "Dispatch at http://dispatch.test is unreachable: fetch failed. If the Dispatch URL " +
        "changed, restart this agent process so it picks up the new configuration.",
    ],
  ] as const;

  // Whether the reason was posted follows one rule with the close's PATCH: only an answer sent
  // before the reason could be stored proves it was not. That is Dispatch's own 4xx, or a
  // gateway's answer that never reached Dispatch: a 408 or 429 (its own timeout or rate limit,
  // where the client's advice that a retry may succeed stands) or a status that cannot clear. A
  // 5xx, Dispatch's own included (it can fail after it committed), a timeout or a transport error
  // leaves the reason possibly posted, and that account replaces the client's advice. After an
  // error's own message it joins a clause, or starts a sentence where the message ended one.
  test("dispatch_issue_update says whether a failed post of the reason leaves it posted", async () => {
    const answered = (status: string, page: string) =>
      `HTTP_${status.slice(0, 3)}: POST http://dispatch.test/api/v1/issues/AGENTC-175/messages ` +
      `answered ${status} with a body that is not Dispatch's error JSON ("${page}"), which looks ` +
      "like a proxy or gateway page rather than Dispatch's own answer";
    const unknown =
      "; the reason may or may not have been posted, and the close was not sent: read the " +
      "issue's messages before retrying, since retrying this call posts its reason again";
    const unknownSentence =
      " The reason may or may not have been posted, and the close was not sent: read the " +
      "issue's messages before retrying, since retrying this call posts its reason again";
    const notPosted = "; the reason was not posted, so the close was not sent";
    const retry = ", so the write did not reach Dispatch, and a retry may succeed";
    // Bun's messages for a refused connection and a reset one; the second ends with no stop.
    const refused = "Unable to connect. Is the computer able to access the url?";
    const reset =
      "The socket connection was closed unexpectedly. For more information, pass `verbose: true` " +
      "in the second argument to fetch()";
    for (const [message, expected] of [
      [
        () => gatewayPage(502, "Bad Gateway"),
        `${answered("502 Bad Gateway", "502 Bad Gateway")}${unknown}`,
      ],
      [
        () => refusal(500, "INTERNAL", "internal server error"),
        `INTERNAL: internal server error${unknown}`,
      ],
      ...unanswered.map(([fail, told]) => [fail, `${told}${unknownSentence}`] as const),
      [
        (): Response => {
          throw new Error(refused);
        },
        `${refused}${unknownSentence}`,
      ],
      [
        (): Response => {
          throw new Error(reset);
        },
        `${reset}${unknown}`,
      ],
      [
        () => gatewayPage(408, "Request Timeout"),
        `${answered("408 Request Timeout", "408 Request Timeout")}${retry}${notPosted}`,
      ],
      [
        () => gatewayPage(429, "Too Many Requests"),
        `${answered("429 Too Many Requests", "429 Too Many Requests")}${retry}${notPosted}`,
      ],
      [
        () => gatewayPage(404, "Not Found"),
        `${answered("404 Not Found", "404 Not Found")}, so a retry gets the same answer until the ` +
          `Dispatch URL, or whatever answers in its place, is fixed${notPosted}`,
      ],
    ] as const) {
      const server = closingServer({
        message,
        patch: () => {
          throw new Error("the close must not be sent");
        },
      });

      const failure = await closeCall(server.fetchImpl).then(
        () => "",
        (error: Error) => error.message
      );

      expect(failure).toBe(expected);
      expect(server.requests.map(({ method }) => method)).toEqual(["GET", "POST"]);
    }
  });

  test("dispatch_issue_update names the posted reason when the close fails after it", async () => {
    const server = closingServer({
      message: () => response({ id: "message-7", issue_key: "AGENTC-175" }),
      patch: () => refusal(409, "ISSUE_CLAIMED", "claimed by session other"),
    });

    await expect(closeCall(server.fetchImpl)).rejects.toThrow(
      "ISSUE_CLAIMED: claimed by session other; the reason already landed as message message-7 " +
        "(dispatch://AGENTC-175/message/message-7) but the issue did not close. Retrying this call " +
        "posts its reason again, so fix what refused the close, then retry with a reason that " +
        "points at message message-7"
    );
    expect(server.requests.map(({ method }) => method)).toEqual(["GET", "POST", "PATCH"]);

    const gateway = closingServer({
      message: () => response({ id: "message-7", issue_key: "AGENTC-175" }),
      patch: () => gatewayPage(404, "Not Found"),
    });
    await expect(closeCall(gateway.fetchImpl)).rejects.toThrow(
      "HTTP_404: PATCH http://dispatch.test/api/v1/issues/AGENTC-175 answered 404 Not Found with a " +
        'body that is not Dispatch\'s error JSON ("404 Not Found"), which looks like a proxy or ' +
        "gateway page rather than Dispatch's own answer, so a retry gets the same answer until the " +
        "Dispatch URL, or whatever answers in its place, is fixed; the reason already landed as " +
        "message message-7 (dispatch://AGENTC-175/message/message-7) but the issue did not close. " +
        "Retrying this call posts its reason again, so fix what refused the close, then retry with " +
        "a reason that points at message message-7"
    );
  });

  // A gateway's 408 or 429 on the close is its own timeout or rate limit, sent before it forwards
  // the PATCH, so the issue did not close, as after the reason's post. Nothing refused the close,
  // so the agent is not told to fix anything: the client's advice that a retry may succeed stands,
  // and the account says how to retry without posting the reason a second time.
  test("dispatch_issue_update says a gateway's 408 or 429 on the close left the issue open", async () => {
    for (const [status, statusText] of [
      [408, "Request Timeout"],
      [429, "Too Many Requests"],
    ] as const) {
      const server = closingServer({
        message: () => response({ id: "message-7", issue_key: "AGENTC-175" }),
        patch: () => gatewayPage(status, statusText),
      });

      const failure = await closeCall(server.fetchImpl).then(
        () => "",
        (error: Error) => error.message
      );

      expect(failure).toBe(
        `HTTP_${status}: PATCH http://dispatch.test/api/v1/issues/AGENTC-175 answered ${status} ` +
          `${statusText} with a body that is not Dispatch's error JSON ("${status} ${statusText}"), ` +
          "which looks like a proxy or gateway page rather than Dispatch's own answer, so the write " +
          "did not reach Dispatch, and a retry may succeed; the reason already landed as message " +
          "message-7 (dispatch://AGENTC-175/message/message-7) but the issue did not close. " +
          "Retrying this call posts its reason again, so retry with a reason that points at " +
          "message message-7"
      );
      expect(server.requests.map(({ method }) => method)).toEqual(["GET", "POST", "PATCH"]);
    }
  });

  // A timeout, a transport error, or a 5xx gives no proof the issue stayed open, so the error
  // must not say it did: an agent that believed it would retry and post its reason twice. A
  // gateway's 5xx page is the same case, and its account replaces the client's retry advice
  // rather than following it, so the message never says both that a retry may succeed and that
  // retrying posts the reason again.
  test("dispatch_issue_update says the close is unknown when the PATCH times out or 5xxes after the post", async () => {
    const unknown =
      "; the reason already landed as message message-7 (dispatch://AGENTC-175/message/message-7), " +
      "and the close may or may not have taken effect. Read the issue's status before retrying: " +
      "done means it closed; otherwise retry with a reason that points at message message-7, " +
      "since retrying this call posts its reason again";
    const unknownSentence =
      " The reason already landed as message message-7 (dispatch://AGENTC-175/message/message-7), " +
      "and the close may or may not have taken effect. Read the issue's status before retrying: " +
      "done means it closed; otherwise retry with a reason that points at message message-7, " +
      "since retrying this call posts its reason again";
    for (const [patch, expected] of [
      ...unanswered.map(([fail, told]) => [fail, `${told}${unknownSentence}`] as const),
      [() => refusal(502, "HTTP_502", "Bad Gateway"), `HTTP_502: Bad Gateway${unknown}`],
      [
        () => gatewayPage(502, "Bad Gateway"),
        "HTTP_502: PATCH http://dispatch.test/api/v1/issues/AGENTC-175 answered 502 Bad Gateway " +
          'with a body that is not Dispatch\'s error JSON ("502 Bad Gateway"), which looks like a ' +
          `proxy or gateway page rather than Dispatch's own answer${unknown}`,
      ],
    ] as const) {
      const server = closingServer({
        message: () => response({ id: "message-7", issue_key: "AGENTC-175" }),
        patch,
      });

      const failure = await closeCall(server.fetchImpl).catch((error: unknown) => error);

      if (!(failure instanceof Error)) throw new Error(`expected an Error, got ${String(failure)}`);
      expect(failure.message).toBe(expected);
      expect(failure.message).not.toContain("did not close");
      expect(failure.message).not.toContain("a retry may succeed");
      expect(server.requests.map(({ method }) => method)).toEqual(["GET", "POST", "PATCH"]);
    }
  });

  test("dispatch_issue_update sets the parent and reports the move", async () => {
    const patches: unknown[] = [];
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const method = init?.method ?? "GET";
      if (method === "GET") {
        return response({
          key: "AGENTC-175",
          title: "Issue update tool",
          status: "in_progress",
          labels: [],
          route: null,
          parent: null,
          external_links: [],
        });
      }
      patches.push(JSON.parse(String(init?.body)));
      return response({
        key: "AGENTC-175",
        title: "Issue update tool",
        status: "in_progress",
        labels: [],
        route: null,
        parent: "AGENTC-9",
        external_links: [],
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_issue_update",
      args: { issue: "AGENTC-175", parent: "AGENTC-9" },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-42",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toContain("parent -> AGENTC-9");
    expect(patches).toEqual([expect.objectContaining({ parent: "AGENTC-9" })]);
  });

  test("dispatch_issue_update passes components through and reports the attachment", async () => {
    const patches: unknown[] = [];
    const issue = {
      key: "AGENTC-175",
      title: "Issue update tool",
      status: "done",
      labels: [],
      route: null,
      parent: null,
      external_links: [],
    };
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      if ((init?.method ?? "GET") === "GET") return response(issue);
      const patch = JSON.parse(String(init?.body)) as { components: { mode: string } };
      patches.push(patch);
      const components =
        patch.components.mode === "explicit"
          ? { mode: "explicit", ids: ["dispatch-server", "web"], unknown: [], reason: null }
          : { mode: "none", ids: [], unknown: [], reason: "hiring, not code" };
      return response({ ...issue, components: { ...components, inherited_from: null } });
    };
    const run = (components: unknown) =>
      executeDispatchTool({
        tool: "dispatch_issue_update",
        args: { issue: "AGENTC-175", components },
        cwd: "/workspace",
        host: "omp",
        sessionId: "session-42",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      });

    const explicit = await run({ mode: "explicit", ids: ["web", "dispatch-server"] });
    expect(explicit.text).toContain("components -> explicit [dispatch-server, web]");
    const none = await run({ mode: "none", reason: "hiring, not code" });
    expect(none.text).toContain("components -> none (hiring, not code)");
    expect(patches).toEqual([
      expect.objectContaining({
        components: { mode: "explicit", ids: ["web", "dispatch-server"] },
      }),
      expect.objectContaining({ components: { mode: "none", reason: "hiring, not code" } }),
    ]);
  });

  test("dispatch_issue_update maps an empty parent to null and reports the clear", async () => {
    const patches: unknown[] = [];
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const method = init?.method ?? "GET";
      if (method === "GET") {
        return response({
          key: "AGENTC-175",
          title: "Issue update tool",
          status: "in_progress",
          labels: [],
          route: null,
          parent: "AGENTC-9",
          external_links: [],
        });
      }
      patches.push(JSON.parse(String(init?.body)));
      return response({
        key: "AGENTC-175",
        title: "Issue update tool",
        status: "in_progress",
        labels: [],
        route: null,
        parent: null,
        external_links: [],
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_issue_update",
      args: { issue: "AGENTC-175", parent: "" },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-42",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toContain("parent cleared");
    expect(patches).toEqual([expect.objectContaining({ parent: null })]);
  });

  test("dispatch_issue_update sends the priority in the patch and reports what it became", async () => {
    const patches: unknown[] = [];
    const issue = {
      key: "AGENTC-175",
      title: "Issue update tool",
      status: "in_progress",
      priority: null,
      labels: [],
      route: null,
      parent: null,
      external_links: [],
    };
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      if ((init?.method ?? "GET") === "GET") return response(issue);
      const patch = JSON.parse(String(init?.body)) as { priority: number | null };
      patches.push(patch);
      return response({ ...issue, priority: patch.priority });
    };
    const run = (priority: unknown) =>
      executeDispatchTool({
        tool: "dispatch_issue_update",
        args: { issue: "AGENTC-175", priority },
        cwd: "/workspace",
        host: "omp",
        sessionId: "session-42",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      });

    const set = await run(1);
    expect(set.text).toContain("priority -> P1");
    const cleared = await run(null);
    expect(cleared.text).toContain("priority cleared");
    expect(patches).toEqual([
      expect.objectContaining({ priority: 1 }),
      expect.objectContaining({ priority: null }),
    ]);
  });

  test("rejects tool arguments outside the shared schema before issuing a request", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_ask",
        args: { issue: "DSP-41", question: "x".repeat(801) },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow();
  });

  test("rejects a quoted comment without the artifact required to resolve its anchor", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_comment",
        args: { issue: "DSP-41", quote: "stale line", body: "Please change this" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(/artifact/);
  });
  test("rejects a malformed dispatch reference before using LEGION_ISSUE", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_read",
        args: { ref: "dispatch://DSP-42/not-a-reference" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: { LEGION_ISSUE: "42" },
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(/valid dispatch/);
  });

  test("reads a named artifact version from a singular dispatch URI", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [{ id: "artifact-42", slug: "spec", name: "spec.md", primary: true }],
          open_asks: [
            {
              id: "ask-1",
              state: "open",
              question: "Which API?",
              anchor: { artifact_id: "artifact-42" },
            },
          ],
        });
      }
      if (target.pathname === "/api/v1/artifacts/artifact-42/versions/3") {
        return response({ markdown: "Version three" });
      }
      if (target.pathname === "/api/v1/issues/DSP-42/comments") return response([]);
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_doc_read",
      args: { ref: "dispatch://DSP-42/artifact/spec@v3" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result).toEqual({
      text: "Version three\n\nOpen anchored asks/comments: ask ask-1",
      details: { issue: "DSP-42" },
    });
    expect(dispatchFollowNotice(result.details)).toBeNull();
    expect(requests).toEqual([
      "/api/v1/issues/DSP-42",
      "/api/v1/artifacts/artifact-42/versions/3",
      "/api/v1/issues/DSP-42/comments?artifact=artifact-42",
    ]);
  });

  // Dispatch serves a file's content only as the bytes of one of its versions; its /text route
  // refuses a file with 400 NOT_DOCUMENT, which every dispatch_doc_read of an uploaded .json, .yaml
  // or .py file used to end in.
  const fileIssue = {
    key: "DSP-42",
    primary_artifact_id: "artifact-42",
    artifacts: [
      {
        id: "artifact-42",
        slug: "spec",
        name: "spec.md",
        kind: "doc",
        primary: true,
        versions: [],
      },
      {
        id: "file-7",
        slug: "sweep-json",
        name: "sweep.json",
        kind: "file",
        primary: false,
        versions: [
          { number: 1, mime: "application/json", size: 10 },
          { number: 2, mime: "application/json", size: 17 },
        ],
      },
      {
        id: "image-8",
        slug: "chart-png",
        name: "chart.png",
        kind: "image",
        primary: false,
        versions: [{ number: 1, mime: "image/png", size: 6 }],
      },
    ],
    open_asks: [],
  };
  function fileServer(requests: string[]) {
    return (async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      if (target.pathname === "/api/v1/issues/DSP-42") return response(fileIssue);
      if (/^\/api\/v1\/artifacts\/(file-7|image-8)\/text$/.test(target.pathname)) {
        return response({ code: "NOT_DOCUMENT", error: "artifact is not a document" }, 400);
      }
      const served: Record<string, [string, Uint8Array<ArrayBuffer>]> = {
        "/api/v1/artifacts/file-7/versions/1": [
          "application/json",
          new TextEncoder().encode('{"rows":1}'),
        ],
        "/api/v1/artifacts/file-7/versions/2": [
          "application/json",
          new TextEncoder().encode('{"rows": [1, 2]}\n'),
        ],
        "/api/v1/artifacts/image-8/versions/1": [
          "image/png",
          new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a]),
        ],
      };
      const file = served[target.pathname];
      if (file !== undefined) {
        return new Response(file[1], { status: 200, headers: { "Content-Type": file[0] } });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    }) as typeof fetch;
  }

  test("dispatch_doc_read returns an uploaded file's text from its latest version or the one named", async () => {
    const requests: string[] = [];
    const read = (args: Record<string, unknown>) =>
      executeDispatchTool({
        tool: "dispatch_doc_read",
        args,
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fileServer(requests),
      });

    expect(await read({ issue: "DSP-42", artifact: "sweep-json" })).toEqual({
      text: 'File sweep.json: application/json, version 2, 17 bytes.\n\n{"rows": [1, 2]}\n',
      details: { issue: "DSP-42" },
    });
    expect(await read({ ref: "dispatch://DSP-42/artifact/sweep-json@v1" })).toEqual({
      text: 'File sweep.json: application/json, version 1 of 2, 10 bytes.\n\n{"rows":1}',
      details: { issue: "DSP-42" },
    });
    expect(requests).toEqual([
      "/api/v1/issues/DSP-42",
      "/api/v1/artifacts/file-7/versions/2",
      "/api/v1/issues/DSP-42",
      "/api/v1/artifacts/file-7/versions/1",
    ]);
  });

  test("dispatch_doc_read describes a binary upload instead of returning its bytes as text", async () => {
    const requests: string[] = [];
    const result = await executeDispatchTool({
      tool: "dispatch_doc_read",
      args: { issue: "DSP-42", artifact: "chart.png" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fileServer(requests),
    });

    expect(result).toEqual({
      text:
        "chart.png is an uploaded image/png file (version 1, 6 bytes) that is not UTF-8 text, so " +
        "dispatch_doc_read cannot show it. GET /api/v1/artifacts/image-8/versions/1 serves its bytes.",
      details: { issue: "DSP-42" },
    });
    expect(requests).toEqual(["/api/v1/issues/DSP-42", "/api/v1/artifacts/image-8/versions/1"]);
  });

  test("dispatch_doc_read starts project document and mark reads before any response resolves", async () => {
    const document = deferred<Response>();
    const documentRequested = deferred<void>();
    const asks = deferred<Response>();
    const comments = deferred<Response>();
    const started: string[] = [];
    const start = (name: string) => {
      started.push(name);
    };
    const notes = {
      id: "artifact-42",
      issue_key: null,
      project: "CORE",
      ref_key: "CORE/notes",
      slug: "notes",
      name: "notes.md",
      kind: "doc",
      primary: false,
      created_by: { kind: "session", id: "session-1" },
      created_at: "2026-09-18T00:00:00Z",
      versions: [],
    };
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/projects/CORE/artifacts/notes") return response(notes);
      if (target.pathname === "/api/v1/projects/CORE/artifacts") return response([notes]);
      if (target.pathname === "/api/v1/artifacts/artifact-42/text") {
        start("document");
        documentRequested.resolve();
        return document.promise;
      }
      if (target.pathname === "/api/v1/artifacts/artifact-42/asks") {
        start("asks");
        return asks.promise;
      }
      if (target.pathname === "/api/v1/artifacts/artifact-42/comments") {
        start("comments");
        return comments.promise;
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const run = executeDispatchTool({
      tool: "dispatch_doc_read",
      args: { project: "CORE", artifact: "notes" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    try {
      await documentRequested.promise;
      expect(started).toEqual(["document", "asks", "comments"]);
    } finally {
      document.resolve(response({ markdown: "# Notes", version: 1, token: "sha256:notes" }));
      asks.resolve(response([]));
      comments.resolve(response([]));
    }

    await expect(run).resolves.toEqual({
      text: "# Notes\n\nDocument token: sha256:notes",
      details: { project: "CORE", document: "CORE/notes" },
    });
  });
  test("rejects a plural artifact Dispatch URI", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_doc_read",
        args: { ref: "dispatch://DSP-42/artifacts/spec" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(/valid dispatch/);
  });

  test("lists the accepted dispatch:// reference grammar in the malformed-ref error", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_doc_read",
        args: { ref: "dispatch://DSP-42/not-a-reference" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(
      "ref must be a valid dispatch:// reference such as dispatch://KEY-1, " +
        "dispatch://KEY-1/ask/<uuid>, dispatch://KEY-1/comment/<uuid>, " +
        "dispatch://KEY-1/message/<uuid>, dispatch://KEY-1/artifact/<slug>, or " +
        "dispatch://PROJECT/artifact/<document-ref> (an artifact id, slug, or filename)"
    );
  });

  test("resolves an issue artifact by its filename, not only its id or slug", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [{ id: "artifact-42", slug: "spec", name: "garrett-reply-draft.md" }],
          open_asks: [],
        });
      }
      if (target.pathname === "/api/v1/artifacts/artifact-42/text") {
        return response({ markdown: "# Garrett reply", version: 1 });
      }
      if (target.pathname === "/api/v1/issues/DSP-42/comments") return response([]);
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_doc_read",
      args: { issue: "DSP-42", artifact: "garrett-reply-draft.md" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe("# Garrett reply");
  });

  test("resolves an issue document id before an earlier artifact's matching slug", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          artifacts: [
            { id: "artifact-first", slug: "target", name: "wrong.md" },
            { id: "target", slug: "right", name: "right.md" },
          ],
          open_asks: [],
        });
      }
      if (target.pathname === "/api/v1/artifacts/target/text") {
        return response({ markdown: "# Right document", version: 1 });
      }
      if (target.pathname === "/api/v1/issues/DSP-42/comments") return response([]);
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_doc_read",
      args: { issue: "DSP-42", artifact: "target" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(requests).toContain("/api/v1/artifacts/target/text");
    expect(result.text).toBe("# Right document");
  });

  test("dispatch_request_approval sends its summary and reports the question the Inbox shows", async () => {
    const posts: Array<{ path: string; body: unknown }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [
            {
              id: "artifact-42",
              slug: "spec",
              name: "spec.md",
              primary: true,
              approval: { state: "draft", latest_version: 3 },
            },
          ],
          open_asks: [],
        });
      }
      if (target.pathname === "/api/v1/artifacts/artifact-42/blocks") {
        return response([{ id: "p-1", type: "paragraph", from: 0, to: 12 }]);
      }
      if (target.pathname === "/api/v1/artifacts/artifact-42/approval-requests") {
        const body = JSON.parse(String(init?.body)) as { summary: string };
        posts.push({ path: target.pathname, body });
        return response(
          {
            ask: {
              id: "ask-9",
              issue_key: "DSP-42",
              artifact_id: null,
              kind: "approval",
              question: `Approve spec.md (version 3)? ${body.summary}`,
            },
            artifact_id: "artifact-42",
            version: 3,
          },
          201
        );
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_request_approval",
      args: { issue: "DSP-42", summary: "Proposes a live sync in place of the nightly export." },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(posts).toHaveLength(1);
    expect(posts[0]?.body).toMatchObject({
      actor: { kind: "session" },
      summary: "Proposes a live sync in place of the nightly export.",
    });
    // The architect copies the document id and version from this text into register_gate, so
    // both must be stated — the id in particular, since the slug it typed is not the id.
    expect(result.text).toStartWith(
      "Approval requested for spec.md (document id artifact-42) at version 3 (ask ask-9)."
    );
    expect(result.text).toContain(
      '"Approve spec.md (version 3)? Proposes a live sync in place of the nightly export."'
    );
    expect(result.details).toMatchObject({ issue: "DSP-42", ask: "ask-9", version: 3 });
    expect(result.details).toMatchObject({ follows: { ask: "ask-9" } });
    expect(result.details).not.toHaveProperty("topic");
  });

  // A call that finds the open request already waiting on the human (Dispatch's 200) changed
  // nothing, and says so rather than reporting a fresh request; the question it quotes is the open
  // request's own.
  test("dispatch_request_approval says a repeat changed nothing and quotes the open request's question", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [
            {
              id: "artifact-42",
              slug: "spec",
              name: "spec.md",
              primary: true,
              approval: { state: "awaiting", latest_version: 3 },
            },
          ],
          open_asks: [],
        });
      }
      if (target.pathname === "/api/v1/artifacts/artifact-42/blocks") {
        return response([]);
      }
      if (target.pathname === "/api/v1/artifacts/artifact-42/approval-requests") {
        return response({
          ask: {
            id: "ask-8",
            issue_key: "DSP-42",
            artifact_id: null,
            kind: "approval",
            question: "Approve spec.md (version 3)? Proposes a nightly export to the archive.",
          },
          artifact_id: "artifact-42",
          version: 3,
        });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_request_approval",
      args: { issue: "DSP-42", summary: "Proposes a nightly export to the archive." },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toStartWith(
      "The approval request for spec.md (document id artifact-42) at version 3 (ask ask-8) already waits on the human, so this call changed nothing: nothing since it last reached the human (a newer version, a human's reply in its thread, or your progress note) left it waiting on you."
    );
    expect(result.text).not.toContain("Approval requested");
    expect(result.text).toContain(
      '"Approve spec.md (version 3)? Proposes a nightly export to the archive."'
    );
  });

  test("dispatch_request_approval on a document approved at its current version opens nothing", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [
            {
              id: "artifact-42",
              slug: "spec",
              name: "spec.md",
              primary: true,
              approval: { state: "approved", latest_version: 3, version: 3 },
            },
          ],
          open_asks: [],
        });
      }
      if (target.pathname === "/api/v1/artifacts/artifact-42/approval-requests") {
        return response({
          ask: null,
          artifact_id: "artifact-42",
          version: 3,
          approval: {
            state: "approved",
            latest_version: 3,
            version: 3,
            by: { kind: "user", id: "sjawhar" },
          },
        });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_request_approval",
      args: { issue: "DSP-42", summary: "Proposes a live sync in place of the nightly export." },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toContain(
      "(document id artifact-42) is already approved at version 3 by sjawhar"
    );
    expect(result.text).not.toContain("ask ");
    expect(result.details).toMatchObject({ issue: "DSP-42", artifact: "artifact-42", version: 3 });
    expect(dispatchFollowNotice(result.details)).toBeNull();
  });

  // A request names the latest version, and a new version moves it there and back to its agent: a
  // request made over an open block leaves the human's turn the moment they answer it, and an
  // answer reaches a version only when the document settles or the agent folds it into the text.
  describe("dispatch_request_approval with decision blocks in the document", () => {
    const opening = (block: string, state: string) =>
      `:::ask{#${block} urgency="med" multiple="false" state="${state}"}\nQuestion of ${block}?\n:::`;
    const requestOver = async (blocks: string[], version4: string[], asks: unknown[]) => {
      const posts: string[] = [];
      const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
        const target = new URL(String(url));
        if (target.pathname === "/api/v1/issues/DSP-42") {
          return response({
            key: "DSP-42",
            primary_artifact_id: "artifact-42",
            artifacts: [
              {
                id: "artifact-42",
                slug: "spec",
                name: "spec.md",
                primary: true,
                approval: { state: "draft", latest_version: 4 },
              },
            ],
            open_asks: [],
          });
        }
        if (target.pathname === "/api/v1/artifacts/artifact-42/blocks") {
          return response([
            { id: "p-1", type: "paragraph", from: 0, to: 9 },
            ...blocks.map((id) => ({ id, type: "ask", from: 10, to: 90 })),
          ]);
        }
        if (target.pathname === "/api/v1/artifacts/artifact-42/versions/4") {
          return response({ number: 4, markdown: ["## Where", ...version4].join("\n\n") });
        }
        // An issue's document lists its asks under the issue: the artifact route refuses it.
        if (target.pathname === "/api/v1/issues/DSP-42/asks") return response(asks);
        if (target.pathname === "/api/v1/artifacts/artifact-42/approval-requests") {
          posts.push(target.pathname);
          return response(
            {
              ask: { id: "ask-9", kind: "approval", question: "Approve spec.md (version 4)? X." },
              artifact_id: "artifact-42",
              version: 4,
            },
            201
          );
        }
        throw new Error(`unexpected request: ${target.pathname}`);
      };
      const outcome = executeDispatchTool({
        tool: "dispatch_request_approval",
        args: { issue: "DSP-42", summary: "Proposes writing the export to S3." },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      });
      return { outcome, posts };
    };
    const blockAsk = (block: string, state: string) => ({
      id: `ask-${block}`,
      kind: "question",
      block_id: block,
      block_artifact: { id: "artifact-42" },
      state,
      question: `Question of ${block}?`,
    });

    test("refuses over every block the named version still holds open, sending nothing", async () => {
      const { outcome, posts } = await requestOver(
        ["b-1", "b-2", "b-3", "b-4"],
        [opening("b-1", "open"), opening("b-2", "open"), opening("b-4", "open")],
        [
          blockAsk("b-1", "open"),
          blockAsk("b-4", "answered"),
          { ...blockAsk("b-3", "answered"), block_artifact: { id: "another-document" } },
        ]
      );

      const refusal = await outcome.then(
        () => "",
        (error: Error) => error.message
      );
      expect(refusal.split("\n").slice(0, 5)).toEqual([
        "dispatch_request_approval was not called: spec.md (version 4) has 4 open decision blocks. Answering one writes a new version, which would move this request to that version and leave it waiting on you.",
        '- "Question of b-1?" (block b-1, ask ask-b-1)',
        "- block b-2, whose ask Dispatch has not opened yet",
        "- block b-3, which version 4 does not hold yet",
        '- "Question of b-4?" (block b-4, ask ask-b-4), answered but still open in version 4: fold the answer into the text with dispatch_doc_edit, which writes a version that carries it',
      ]);
      expect(refusal).toContain("even when a human asked for it");
      expect(refusal).toContain("ask them to answer it or to waive it");
      expect(posts).toEqual([]);
    });

    test("a resolved block the named version still holds open is named with the decision to write in", async () => {
      const { outcome, posts } = await requestOver(
        ["b-1"],
        [opening("b-1", "open")],
        [blockAsk("b-1", "resolved")]
      );

      const refusal = await outcome.then(
        () => "",
        (error: Error) => error.message
      );
      expect(refusal.split("\n")[1]).toBe(
        '- "Question of b-1?" (block b-1, ask ask-b-1), resolved but still open in version 4: write the decision into the text with dispatch_doc_edit, which writes a version that carries it'
      );
      expect(posts).toEqual([]);
    });

    test("a line quoting a block's opener cannot hide the open block below it", async () => {
      const quoted =
        'A settled block opens like `:::ask{#b-1 urgency="med" multiple="false" state="resolved"}`.';
      const { outcome, posts } = await requestOver(
        ["b-1"],
        [quoted, opening("b-1", "open")],
        [blockAsk("b-1", "open")]
      );

      const refusal = await outcome.then(
        () => "",
        (error: Error) => error.message
      );
      expect(refusal.split("\n").slice(0, 2)).toEqual([
        "dispatch_request_approval was not called: spec.md (version 4) has 1 open decision block. Answering one writes a new version, which would move this request to that version and leave it waiting on you.",
        '- "Question of b-1?" (block b-1, ask ask-b-1)',
      ]);
      expect(posts).toEqual([]);
    });

    test("requests approval once the named version holds every block answered or resolved", async () => {
      const { outcome, posts } = await requestOver(
        ["b-1", "b-2"],
        [opening("b-1", "answered"), opening("b-2", "resolved")],
        [blockAsk("b-1", "answered"), blockAsk("b-2", "resolved")]
      );

      expect((await outcome).text).toStartWith("Approval requested for spec.md");
      expect(posts).toEqual(["/api/v1/artifacts/artifact-42/approval-requests"]);
    });
  });

  describe("dispatch_doc_edit over a decision block", () => {
    // A callout holding the open block b-1, then b-2, whose ask a human has answered.
    const blocks = [
      { id: "p-1", type: "paragraph", from: 0, to: 9 },
      { id: "c-1", type: "callout", from: 10, to: 120 },
      { id: "b-1", type: "ask", from: 20, to: 110 },
      { id: "b-2", type: "ask", from: 130, to: 200 },
    ];
    const asks = [
      {
        id: "ask-b-1",
        kind: "question",
        block_id: "b-1",
        block_artifact: { id: "artifact-42" },
        state: "open",
        question: "Where should the nightly file be written?",
      },
      {
        id: "ask-b-2",
        kind: "question",
        block_id: "b-2",
        block_artifact: { id: "artifact-42" },
        state: "answered",
        question: "Which format?",
      },
    ];
    const edit = (ops: unknown[]) => {
      const reads: string[] = [];
      const edits: unknown[] = [];
      const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const target = new URL(String(url));
        if (target.pathname === "/api/v1/issues/DSP-42") {
          return response({
            key: "DSP-42",
            primary_artifact_id: "artifact-42",
            artifacts: [{ id: "artifact-42", slug: "spec", name: "spec.md", primary: true }],
            open_asks: [],
          });
        }
        if (target.pathname === "/api/v1/artifacts/artifact-42/blocks") {
          reads.push(target.pathname);
          return response(blocks);
        }
        if (target.pathname === "/api/v1/issues/DSP-42/asks") {
          reads.push(`${target.pathname}${target.search}`);
          return response(
            target.searchParams.get("state") === "open"
              ? asks.filter((ask) => ask.state === "open")
              : asks
          );
        }
        if (target.pathname === "/api/v1/artifacts/artifact-42/edits") {
          edits.push(JSON.parse(init?.body as string).ops);
          return response({ applied: ops.length, version: { number: 5 }, token: "sha256:t" });
        }
        throw new Error(`unexpected request: ${target.pathname}`);
      };
      const outcome = executeDispatchTool({
        tool: "dispatch_doc_edit",
        args: { issue: "DSP-42", artifact: "spec", ops },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      }).then(
        (result) => result.text,
        (error: Error) => error.message
      );
      return { outcome, reads, edits };
    };

    test("refuses to remove a block whose ask is open, sending nothing", async () => {
      // Removing the block writes a version at once and settlement retracts the ask without
      // another, so an approval request sent next would name a version with no open block.
      for (const ops of [
        [{ op: "delete", block: "b-1" }],
        [{ op: "retype", block: "b-1", type: "callout", attributes: { kind: "note" } }],
        [{ op: "delete", block: "c-1" }],
        // An insert carrying the block's id does not exempt it, whether it writes the block back
        // or only quotes its opener in a code fence: the executor cannot tell the two apart.
        [
          { op: "delete", block: "b-1" },
          {
            op: "insert",
            after: "block:p-1",
            markdown: ':::ask{#b-1 urgency="med"}\nWhere should the nightly file go?\n:::',
          },
        ],
        [
          { op: "delete", block: "b-1" },
          { op: "insert", after: "block:p-1", markdown: "```\n:::ask{#b-1 }\n```" },
        ],
      ]) {
        const { outcome, reads, edits } = edit(ops);
        expect((await outcome).split("\n")).toEqual([
          "dispatch_doc_edit was not called: it would remove a decision block whose ask is still open, and the human's question would leave their Inbox unanswered.",
          '- "Where should the nightly file be written?" (block b-1, ask ask-b-1)',
          "A decision block leaves the document once its ask is answered or resolved. Until then, reword it with replace, relocate it with move, or change its question, options, urgency or multiple with dispatch_edit_ask if you asked it; each keeps it.",
        ]);
        expect(reads).toEqual([
          "/api/v1/artifacts/artifact-42/blocks",
          "/api/v1/issues/DSP-42/asks?state=open",
        ]);
        expect(edits).toEqual([]);
      }
    });

    test("an opener that the inserted markdown holds only as code does not write the block back", async () => {
      // Code-only opener text does not restore a block or its ask.
      for (const markdown of [
        "```text\n:::ask{#b-1}\n```",
        '~~~\n:::ask{#b-1 urgency="med"}\n~~~',
        "The old question read:\n\n    :::ask{#b-1}",
      ]) {
        const { outcome, edits } = edit([
          { op: "delete", block: "b-1" },
          { op: "insert", after: "block:p-1", markdown },
        ]);
        expect((await outcome).split("\n")).toEqual([
          "dispatch_doc_edit was not called: it would remove a decision block whose ask is still open, and the human's question would leave their Inbox unanswered.",
          '- "Where should the nightly file be written?" (block b-1, ask ask-b-1)',
          "A decision block leaves the document once its ask is answered or resolved. Until then, reword it with replace, relocate it with move, or change its question, options, urgency or multiple with dispatch_edit_ask if you asked it; each keeps it.",
        ]);
        expect(edits).toEqual([]);
      }
    });

    test("names dispatch_edit_ask for the urgency, multiple and options replace and move cannot change", async () => {
      // Changing those attributes requires editing the ask; replace and move cannot change them.
      const { outcome, edits } = edit([
        { op: "delete", block: "b-1" },
        {
          op: "insert",
          after: "block:p-1",
          markdown: ':::ask{#b-1 urgency="high"}\nWhere should the nightly file be written?\n:::',
        },
      ]);
      const guidance = (await outcome).split("\n").at(-1);
      expect(guidance).toContain("dispatch_edit_ask");
      expect(edits).toEqual([]);
    });

    test("sends an edit that keeps every open block, reading nothing when no block is removed", async () => {
      for (const ops of [
        [{ op: "delete", block: "b-2" }],
        [{ op: "retype", block: "b-1", type: "ask", attributes: { urgency: "high" } }],
        [{ op: "delete", block: "p-1" }],
      ]) {
        const { outcome, edits } = edit(ops);
        expect(await outcome).toStartWith(`Applied ${ops.length} ops`);
        expect(edits).toEqual([ops]);
      }
      const moved = edit([{ op: "move", block: "b-1", after: "block:b-2" }]);
      expect(await moved.outcome).toStartWith("Applied 1 ops (version 5)");
      expect(moved.reads).toEqual([]);
    });
  });

  test("dispatch_doc_read tells the agent when the document's approval went stale", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [
            {
              id: "artifact-42",
              slug: "spec",
              name: "spec.md",
              primary: true,
              approval: {
                state: "stale",
                latest_version: 4,
                version: 2,
                by: { kind: "user", id: "sjawhar" },
              },
            },
          ],
          open_asks: [],
        });
      }
      if (target.pathname === "/api/v1/artifacts/artifact-42/text") {
        return response({ markdown: "# Spec", version: 4 });
      }
      if (target.pathname === "/api/v1/issues/DSP-42/comments") return response([]);
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_doc_read",
      args: { issue: "DSP-42" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe(
      "# Spec\n\nApproval: approved v2 by sjawhar, edited since (now v4) - request approval again once the human has agreed to every point in this version"
    );
  });

  // A version moves an open request back to its agent, the agent's own revision included, which
  // sends that agent no event, so the document's approval line says whom the request waits on.
  test("dispatch_doc_read says whom an awaiting approval request waits on", async () => {
    for (const waitingOn of ["agent", "human"] as const) {
      const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
        const target = new URL(String(url));
        if (target.pathname === "/api/v1/issues/DSP-42") {
          return response({
            key: "DSP-42",
            primary_artifact_id: "artifact-42",
            artifacts: [
              {
                id: "artifact-42",
                slug: "spec",
                name: "spec.md",
                primary: true,
                approval: {
                  state: "awaiting",
                  latest_version: 4,
                  ask_id: "ask-9",
                  requested_by: { kind: "session", id: "session-1" },
                  waiting_on: waitingOn,
                },
              },
            ],
            open_asks: [],
          });
        }
        if (target.pathname === "/api/v1/artifacts/artifact-42/text") {
          return response({ markdown: "# Spec", version: 4 });
        }
        if (target.pathname === "/api/v1/issues/DSP-42/comments") return response([]);
        throw new Error(`unexpected request: ${target.pathname}`);
      };

      const result = await executeDispatchTool({
        tool: "dispatch_doc_read",
        args: { issue: "DSP-42" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      });

      expect(result.text).toBe(
        `# Spec\n\nApproval: awaiting, waiting on ${waitingOn} (requested by session-1, ask ask-9)`
      );
    }
  });

  test("resolves a project artifact by its filename when the slug route 404s", async () => {
    const paths: string[] = [];
    const artifact = {
      id: "artifact-42",
      issue_key: null,
      project: "CORE",
      ref_key: "CORE/garrett-reply-draft-md",
      slug: "garrett-reply-draft-md",
      name: "garrett-reply-draft.md",
      kind: "doc",
      primary: false,
      created_by: { kind: "session", id: "session-42" },
      created_at: "2026-09-11T00:00:00Z",
      versions: [],
    };
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const request = new URL(String(url));
      paths.push(request.pathname + request.search);
      if (request.pathname === "/api/v1/projects/CORE/artifacts/garrett-reply-draft.md") {
        return new Response(JSON.stringify({ code: "ARTIFACT_NOT_FOUND", error: "not found" }), {
          status: 404,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (request.pathname === "/api/v1/projects/CORE/artifacts") return response([artifact]);
      if (request.pathname.endsWith("/text")) {
        return response({ markdown: "# Garrett reply", version: 1 });
      }
      if (request.pathname.endsWith("/asks") || request.pathname.endsWith("/comments")) {
        return response([]);
      }
      throw new Error(`unexpected request: ${request.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_doc_read",
      args: { project: "CORE", artifact: "garrett-reply-draft.md" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe("# Garrett reply");
    expect(paths).toContain("/api/v1/projects/CORE/artifacts?unlinked=true");
  });

  // Dispatch's own 404 on a project's document route sends the lookup to the project's unlinked
  // documents. A gateway's 404 page there says nothing about the document, so the agent gets the
  // gateway's answer rather than a document the project does not have.
  test("does not read a gateway's 404 page on a project's document route as no such document", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const { pathname } = new URL(String(url));
      if (pathname === "/api/v1/projects/CORE/artifacts/runbook-md") {
        return gatewayPage(404, "Not Found");
      }
      if (pathname === "/api/v1/projects/CORE/artifacts") return response([]);
      throw new Error(`unexpected request: ${pathname}`);
    };

    const failure = await executeDispatchTool({
      tool: "dispatch_doc_read",
      args: { project: "CORE", artifact: "runbook-md" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    }).then(
      () => "",
      (error: Error) => error.message
    );

    expect(failure).toStartWith(
      "GET http://dispatch.test/api/v1/projects/CORE/artifacts/runbook-md answered 404 Not Found " +
        "with a body that is not Dispatch's error JSON"
    );
  });

  // Dispatch's own 404 on a document's comments route (a server without it) reads as no comments.
  // A gateway's 404 page there is a failure like any other, so the read reports it instead of
  // showing the document as though nothing were anchored to it.
  test("does not read a gateway's 404 page on a document's comments route as no comments", async () => {
    const artifact = {
      id: "artifact-42",
      issue_key: null,
      project: "CORE",
      ref_key: "CORE/runbook-md",
      slug: "runbook-md",
      name: "Runbook.md",
      kind: "doc",
      primary: false,
      created_by: { kind: "session", id: "session-42" },
      created_at: "2026-09-12T00:00:00Z",
      versions: [],
    };
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const { pathname } = new URL(String(url));
      if (pathname === "/api/v1/projects/CORE/artifacts/runbook-md") return response(artifact);
      if (pathname === "/api/v1/projects/CORE/artifacts") return response([artifact]);
      if (pathname === "/api/v1/artifacts/artifact-42/text") {
        return response({ markdown: "# Runbook", version: 1 });
      }
      if (pathname === "/api/v1/artifacts/artifact-42/asks") return response([]);
      if (pathname === "/api/v1/artifacts/artifact-42/comments") {
        return gatewayPage(404, "Not Found");
      }
      throw new Error(`unexpected request: ${pathname}`);
    };

    const failure = await executeDispatchTool({
      tool: "dispatch_doc_read",
      args: { project: "CORE", artifact: "runbook-md" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    }).then(
      () => "",
      (error: Error) => error.message
    );

    expect(failure).toStartWith(
      "GET http://dispatch.test/api/v1/artifacts/artifact-42/comments answered 404 Not Found " +
        "with a body that is not Dispatch's error JSON"
    );
  });

  test("resolves a project document by filename, never an issue-owned artifact of the same name", async () => {
    const paths: string[] = [];
    const projectDoc = {
      id: "artifact-99",
      issue_key: null,
      project: "CORE",
      ref_key: "CORE/notes-md",
      slug: "notes-md",
      name: "notes.md",
      kind: "doc",
      primary: false,
      created_by: { kind: "user", id: "alice" },
      created_at: "2026-09-11T00:00:00Z",
      versions: [],
    };
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const request = new URL(String(url));
      paths.push(request.pathname + request.search);
      if (request.pathname === "/api/v1/projects/CORE/artifacts/notes.md") {
        return new Response(JSON.stringify({ code: "ARTIFACT_NOT_FOUND", error: "not found" }), {
          status: 404,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (request.pathname === "/api/v1/projects/CORE/artifacts") {
        // The unlinked-only route excludes CORE-1's same-named artifact; a caller
        // that forgot the `unlinked=true` flag would see it here and could match it.
        return request.search === "?unlinked=true"
          ? response([projectDoc])
          : response([
              {
                id: "artifact-1",
                issue_key: "CORE-1",
                project: "CORE",
                ref_key: "CORE-1/notes-md",
                slug: "notes-md-issue",
                name: "notes.md",
                kind: "doc",
                primary: false,
                created_by: { kind: "user", id: "alice" },
                created_at: "2026-09-11T00:00:00Z",
                versions: [],
              },
              projectDoc,
            ]);
      }
      if (request.pathname.endsWith("/text")) return response({ markdown: "# Notes", version: 1 });
      if (request.pathname.endsWith("/asks") || request.pathname.endsWith("/comments")) {
        return response([]);
      }
      throw new Error(`unexpected request: ${request.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_doc_read",
      args: { project: "CORE", artifact: "notes.md" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.details).toMatchObject({ project: "CORE", document: "CORE/notes-md" });
    expect(paths).toContain("/api/v1/projects/CORE/artifacts?unlinked=true");
  });

  test("rejects a filename shared by more than one unlinked project document", async () => {
    const duplicateDoc = (id: string) => ({
      id,
      issue_key: null,
      project: "CORE",
      ref_key: `CORE/${id}`,
      slug: id,
      name: "notes.md",
      kind: "doc",
      primary: false,
      created_by: { kind: "user", id: "alice" },
      created_at: "2026-09-11T00:00:00Z",
      versions: [],
    });
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const request = new URL(String(url));
      if (request.pathname === "/api/v1/projects/CORE/artifacts/notes.md") {
        return new Response(JSON.stringify({ code: "ARTIFACT_NOT_FOUND", error: "not found" }), {
          status: 404,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (request.pathname === "/api/v1/projects/CORE/artifacts") {
        return response([duplicateDoc("artifact-a"), duplicateDoc("artifact-b")]);
      }
      throw new Error(`unexpected request: ${request.pathname}`);
    };

    await expect(
      executeDispatchTool({
        tool: "dispatch_doc_read",
        args: { project: "CORE", artifact: "notes.md" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).rejects.toThrow(
      `"notes.md" names 2 documents on this project; use a slug: artifact-a (notes.md), artifact-b (notes.md)`
    );
  });

  // Dispatch suffixes a slug two documents would share: "plan v2" takes plan-v2, so a document
  // named "plan-v2" takes plan-v2-2, and plan-v2 is then one document's slug and the other's
  // filename.
  const projectDocument = (id: string, slug: string, name: string): Artifact => ({
    id,
    issue_key: null,
    project: "GREF",
    ref_key: `GREF/${slug}`,
    slug,
    name,
    kind: "doc",
    primary: false,
    created_by: { kind: "user", id: "alice" },
    created_at: "2026-09-27T00:00:00Z",
    versions: [],
  });
  /** Dispatch's project routes over `documents`: the slug route answers a slug, then a filename
   * no other document shares, and the unlinked list is every document. `requests` records each
   * call as its method and path. */
  const projectDispatch = (documents: readonly Artifact[], requests: string[]) =>
    (async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const request = new URL(String(url));
      requests.push(`${init?.method ?? "GET"} ${request.pathname}${request.search}`);
      if (request.pathname === "/api/v1/projects/GREF/artifacts") return response(documents);
      const routed = request.pathname.match(/^\/api\/v1\/projects\/GREF\/artifacts\/([^/]+)$/);
      if (routed?.[1] !== undefined) {
        const reference = decodeURIComponent(routed[1]);
        const named = documents.filter((document) => document.name === reference);
        const hit =
          documents.find((document) => document.slug === reference) ??
          (named.length === 1 ? named[0] : undefined);
        return hit === undefined
          ? new Response(JSON.stringify({ code: "ARTIFACT_NOT_FOUND", error: "not found" }), {
              status: 404,
              headers: { "Content-Type": "application/json" },
            })
          : response(hit);
      }
      const text = request.pathname.match(/^\/api\/v1\/artifacts\/([^/]+)\/text$/);
      if (text?.[1] !== undefined) return response({ markdown: `# ${text[1]}`, version: 1 });
      if (request.pathname.endsWith("/asks") || request.pathname.endsWith("/comments")) {
        return response([]);
      }
      throw new Error(`unexpected request: ${request.pathname}`);
    }) as typeof fetch;
  /** Dispatch's issue route for DSP-42, whose primary document is `spec` and which carries
   * `artifacts` besides, and the text and comment reads a document read makes. */
  const issueDispatch = (artifacts: readonly Pick<Artifact, "id" | "slug" | "name" | "kind">[]) =>
    (async (url: RequestInfo | URL): Promise<Response> => {
      const request = new URL(String(url));
      if (request.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-spec",
          artifacts: [
            { id: "artifact-spec", slug: "spec", name: "spec.md", kind: "doc", primary: true },
            ...artifacts.map((artifact) => ({ ...artifact, primary: false })),
          ],
        });
      }
      const text = request.pathname.match(/^\/api\/v1\/artifacts\/([^/]+)\/text$/);
      if (text?.[1] !== undefined) return response({ markdown: `# ${text[1]}`, version: 1 });
      if (request.pathname === "/api/v1/issues/DSP-42/comments") return response([]);
      throw new Error(`unexpected request: ${request.pathname}`);
    }) as typeof fetch;
  const readDocument = (args: Record<string, unknown>, fetchImpl: typeof fetch) =>
    executeDispatchTool({
      tool: "dispatch_doc_read",
      args,
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl,
    });

  test("a bare project reference that is one document's slug and another's filename is refused", async () => {
    const documents = [
      projectDocument("artifact-v2", "plan-v2", "plan v2"),
      projectDocument("artifact-v2-2", "plan-v2-2", "plan-v2"),
    ];
    const requests: string[] = [];
    const fetchImpl = projectDispatch(documents, requests);
    const refusal =
      '"plan-v2" names 2 documents on this project; use the id: artifact-v2 (plan-v2, plan v2), artifact-v2-2 (plan-v2-2, plan-v2)';
    await expect(readDocument({ project: "GREF", artifact: "plan-v2" }, fetchImpl)).rejects.toThrow(
      refusal
    );
    await expect(
      executeDispatchTool({
        tool: "dispatch_comment",
        args: { project: "GREF", artifact: "plan-v2", body: "Looks good." },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(refusal);
    expect(requests.filter((request) => !request.startsWith("GET "))).toEqual([]);
    for (const [reference, id] of [
      ["plan v2", "artifact-v2"],
      ["plan-v2-2", "artifact-v2-2"],
      ["artifact-v2", "artifact-v2"],
    ] as const) {
      const result = await readDocument({ project: "GREF", artifact: reference }, fetchImpl);
      expect(result.text).toBe(`# ${id}`);
    }
  });

  test("a bare project reference that is one document's id and another's slug resolves as the id", async () => {
    const documents = [
      projectDocument("shared-reference", "notes", "notes.md"),
      projectDocument("artifact-other", "shared-reference", "other.md"),
    ];
    const result = await readDocument(
      { project: "GREF", artifact: "shared-reference" },
      projectDispatch(documents, [])
    );
    expect(result.text).toBe("# shared-reference");
  });

  // Only documents answer to a filename (Dispatch's own filename fallback reads documents alone),
  // so an image or file whose filename is a document's slug names no second document.
  test("a document slug that an image's or file's filename repeats resolves to the document", async () => {
    const project = await readDocument(
      { project: "GREF", artifact: "diagram" },
      projectDispatch(
        [
          projectDocument("artifact-doc", "diagram", "Diagram notes"),
          { ...projectDocument("artifact-image", "diagram-2", "diagram"), kind: "image" },
          { ...projectDocument("artifact-file", "diagram-3", "diagram"), kind: "file" },
        ],
        []
      )
    );
    expect(project.text).toBe("# artifact-doc");

    const issue = await readDocument(
      { issue: "DSP-42", artifact: "diagram" },
      issueDispatch([
        { id: "artifact-doc", slug: "diagram", name: "Diagram notes", kind: "doc" },
        { id: "artifact-image", slug: "diagram-2", name: "diagram", kind: "image" },
      ])
    );
    expect(issue.text).toBe("# artifact-doc");
  });

  test("a bare project reference fails with the route read's error, and with the list read's after a route hit", async () => {
    const documents = [projectDocument("artifact-v2", "plan-v2", "plan v2")];
    const failing = (target: string) =>
      (async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const request = new URL(String(url));
        if (request.pathname + request.search === target) {
          return new Response(JSON.stringify({ code: "UNAVAILABLE", error: `${target} failed` }), {
            status: 503,
            headers: { "Content-Type": "application/json" },
          });
        }
        return projectDispatch(documents, [])(url, init);
      }) as typeof fetch;
    for (const target of [
      "/api/v1/projects/GREF/artifacts/plan-v2",
      "/api/v1/projects/GREF/artifacts?unlinked=true",
    ]) {
      await expect(
        readDocument({ project: "GREF", artifact: "plan-v2" }, failing(target))
      ).rejects.toThrow(`${target} failed`);
    }
  });

  // A dispatch:// reference's document part is a slug, the address the dashboard and Dispatch's
  // own routes use, so it names one document even where the same text is another's filename.
  test("a dispatch:// document reference resolves by slug, never refused for a filename clash", async () => {
    const requests: string[] = [];
    const documents = [
      projectDocument("artifact-v2", "plan-v2", "plan v2"),
      projectDocument("artifact-v2-2", "plan-v2-2", "plan-v2"),
    ];
    const project = await readDocument(
      { ref: "dispatch://GREF/artifact/plan-v2" },
      projectDispatch(documents, requests)
    );
    expect(project.text).toBe("# artifact-v2");
    expect(requests).not.toContain("GET /api/v1/projects/GREF/artifacts?unlinked=true");

    const clashing = issueDispatch([
      { id: "artifact-v2", slug: "spec-v2", name: "spec v2", kind: "doc" },
      { id: "artifact-v2-2", slug: "spec-v2-2", name: "spec-v2", kind: "doc" },
    ]);
    for (const ref of [
      "dispatch://DSP-42/artifact/spec-v2",
      "http://dispatch.test/issues/DSP-42/artifacts/spec-v2",
    ]) {
      const issue = await readDocument({ ref }, clashing);
      expect(issue.text).toBe("# artifact-v2");
    }
    await expect(readDocument({ issue: "DSP-42", artifact: "spec-v2" }, clashing)).rejects.toThrow(
      '"spec-v2" names 2 documents on this issue; use the id'
    );
  });

  test("renders a no-new-version document edit and forwards its summary and precondition", async () => {
    const requests: Array<{ url: string; init: RequestInit }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const request = { url: String(url), init: init ?? {} };
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [
            {
              id: "artifact-42",
              slug: "spec",
              name: "spec.md",
              primary: true,
            },
          ],
        });
      }
      if (path === "/api/v1/artifacts/artifact-42/edits") {
        return response({ applied: 2, version: null, token: "sha256:after-edit" });
      }
      throw new Error(`unexpected request: ${path}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_doc_edit",
      args: {
        issue: "DSP-42",
        artifact: "spec",
        ops: [{ op: "replace", find: "draft", with: "final" }],
        summary: "Record final wording",
        precondition: { document: "sha256:current-document" },
      },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-42",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result).toEqual({
      text:
        "Applied 2 ops (no new version) (not subscribed to DSP-42; envoy_subscribe notifications.dispatch.issue.DSP-42.> for every event on it)\n" +
        "Document token: sha256:after-edit",
      details: { issue: "DSP-42", applied: 2, token: "sha256:after-edit" },
    });
    expect(JSON.parse(requests[1]?.init.body as string)).toMatchObject({
      ops: [{ op: "replace", find: "draft", with: "final" }],
      summary: "Record final wording",
      precondition: { document: "sha256:current-document" },
    });
  });

  test("says nothing changed and names the operations that changed nothing", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const path = new URL(String(url)).pathname;
      if (path === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [{ id: "artifact-42", slug: "spec", name: "spec.md", primary: true }],
        });
      }
      if (path === "/api/v1/artifacts/artifact-42/edits") {
        return response({
          applied: 2,
          version: null,
          changed: false,
          unchanged_ops: [0, 1],
          token: "sha256:unchanged",
        });
      }
      throw new Error(`unexpected request: ${path}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_doc_edit",
      args: {
        issue: "DSP-42",
        artifact: "spec",
        ops: [
          { op: "replace", find: "draft", with: "draft" },
          { op: "replace", find: "final", with: "final" },
        ],
        summary: "Record final wording",
      },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-42",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe(
      "Applied 2 ops; nothing changed (no new version); operations 0, 1 changed nothing" +
        " (not subscribed to DSP-42; envoy_subscribe notifications.dispatch.issue.DSP-42.> for every event on it)\n" +
        "Document token: sha256:unchanged"
    );
    expect(result.details).toEqual({
      issue: "DSP-42",
      applied: 2,
      changed: false,
      token: "sha256:unchanged",
    });
  });

  // A concurrent browser deletion that lands after the version is written is past undoing, so the
  // edit reports it: the version records text the live document no longer has (LEGION-269). An
  // edit that survives reads exactly as it always did.
  test("names the operations whose text the live document no longer has", async () => {
    const editResponse = (body: Record<string, unknown>) => {
      return async (url: RequestInfo | URL): Promise<Response> => {
        const path = new URL(String(url)).pathname;
        if (path === "/api/v1/issues/DSP-42") {
          return response({
            key: "DSP-42",
            primary_artifact_id: "artifact-42",
            artifacts: [{ id: "artifact-42", slug: "spec", name: "spec.md", primary: true }],
          });
        }
        if (path === "/api/v1/artifacts/artifact-42/edits") return response(body);
        throw new Error(`unexpected request: ${path}`);
      };
    };
    const edit = async (body: Record<string, unknown>) =>
      await executeDispatchTool({
        tool: "dispatch_doc_edit",
        args: {
          issue: "DSP-42",
          artifact: "spec",
          ops: [{ op: "replace", find: "draft", with: "final" }],
        },
        cwd: "/workspace",
        host: "omp",
        sessionId: "session-42",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: editResponse(body) as typeof fetch,
      });

    const lost = await edit({
      applied: 1,
      version: { number: 7 },
      changed: true,
      unchanged_ops: [],
      lost_ops: [0],
    });
    expect(lost.text).toContain(
      "; version 7 carries text the live document no longer has: a concurrent change removed what operation 0 wrote — re-read the document"
    );
    expect(lost.details).toMatchObject({ lost_ops: [0] });

    const survived = await edit({
      applied: 1,
      version: { number: 7 },
      changed: true,
      unchanged_ops: [],
      lost_ops: [],
    });
    expect(survived.text).toContain("Applied 1 ops (version 7)");
    expect(survived.text).not.toContain("no longer has");

    const undetermined = await edit({
      applied: 1,
      version: { number: 7 },
      changed: true,
      unchanged_ops: [],
      lost_ops: null,
    });
    expect(undetermined.text).toContain("could not confirm this edit survived");
    expect(undetermined.text).not.toContain("no longer has");
    expect(undetermined.details).toMatchObject({ lost_ops: null });
  });

  // A Dispatch server predating the edit token returns none, and the result reads as it always
  // did: no trailer to mistake for a token, and nothing in details to pass as a precondition.
  test("omits the document token when the server returns none", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const path = new URL(String(url)).pathname;
      if (path === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [{ id: "artifact-42", slug: "spec", name: "spec.md", primary: true }],
        });
      }
      if (path === "/api/v1/artifacts/artifact-42/edits") {
        return response({ applied: 1, version: null });
      }
      throw new Error(`unexpected request: ${path}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_doc_edit",
      args: {
        issue: "DSP-42",
        artifact: "spec",
        ops: [{ op: "replace", find: "draft", with: "final" }],
      },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-42",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).not.toContain("Document token:");
    expect(result.details).toEqual({ issue: "DSP-42", applied: 1 });
  });

  test("names the issue's document slugs and display names when the requested one is missing", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const path = new URL(String(url)).pathname;
      if (path === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [
            { id: "artifact-42", slug: "spec", name: "spec.md", primary: true },
            { id: "artifact-43", slug: "notes", name: "notes.md", primary: false },
          ],
        });
      }
      throw new Error(`unexpected request: ${path}`);
    };

    await expect(
      executeDispatchTool({
        tool: "dispatch_doc_edit",
        args: {
          issue: "DSP-42",
          artifact: "primary",
          ops: [{ op: "replace", find: "draft", with: "final" }],
        },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).rejects.toThrow(
      `document "primary" not found by slug; this issue's documents: spec (spec.md), notes (notes.md)`
    );
  });

  test.each([
    [
      "dispatch_artifact",
      { project: "CORE", name: "runbook.md", content: "# Runbook\n" },
      "/api/v1/projects/CORE/artifacts",
      true,
    ],
    [
      "dispatch_ask",
      { project: "CORE", artifact: "runbook-md", question: runbookQuestion },
      "/api/v1/artifacts/artifact-42/asks",
      true,
    ],
    [
      "dispatch_comment",
      { project: "CORE", artifact: "runbook-md", body: "Looks good." },
      "/api/v1/artifacts/artifact-42/comments",
      true,
    ],
    [
      "dispatch_suggest",
      {
        project: "CORE",
        artifact: "runbook-md",
        quote: "draft",
        replace_with: "final",
      },
      "/api/v1/artifacts/artifact-42/comments",
      true,
    ],
    [
      "dispatch_doc_edit",
      {
        project: "CORE",
        artifact: "runbook-md",
        ops: [{ op: "replace", find: "draft", with: "final" }],
      },
      "/api/v1/artifacts/artifact-42/edits",
      true,
    ],
    [
      "dispatch_doc_read",
      { project: "CORE", artifact: "runbook-md" },
      "/api/v1/artifacts/artifact-42/text",
      false,
    ],
    [
      "dispatch_read",
      { project: "CORE", artifact: "runbook-md" },
      "/api/v1/projects/CORE/artifacts/runbook-md",
      false,
    ],
    [
      "dispatch_ask",
      { ref: "dispatch://CORE/artifact/runbook-md", question: runbookQuestion },
      "/api/v1/artifacts/artifact-42/asks",
      true,
    ],
    [
      "dispatch_comment",
      { ref: "dispatch://CORE/artifact/runbook-md", body: "Looks good." },
      "/api/v1/artifacts/artifact-42/comments",
      true,
    ],
    [
      "dispatch_suggest",
      {
        ref: "dispatch://CORE/artifact/runbook-md",
        quote: "draft",
        replace_with: "final",
      },
      "/api/v1/artifacts/artifact-42/comments",
      true,
    ],
    [
      "dispatch_doc_edit",
      {
        ref: "dispatch://CORE/artifact/runbook-md",
        ops: [{ op: "replace", find: "draft", with: "final" }],
      },
      "/api/v1/artifacts/artifact-42/edits",
      true,
    ],
  ])("executes %s with a project document owner", async (tool, args, expectedPath, writes) => {
    const paths: string[] = [];
    const artifact = {
      id: "artifact-42",
      issue_key: null,
      project: "CORE",
      ref_key: "CORE/runbook-md",
      slug: "runbook-md",
      name: "Runbook.md",
      kind: "doc",
      primary: false,
      created_by: { kind: "session", id: "session-42" },
      created_at: "2026-09-11T00:00:00Z",
      versions: [],
    };
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const request = new URL(String(url));
      paths.push(request.pathname);
      if (request.pathname === "/api/v1/projects/CORE/artifacts/runbook-md") {
        return response(artifact);
      }
      if (request.pathname === "/api/v1/projects/CORE/artifacts") {
        return request.search === "?unlinked=true"
          ? response([artifact])
          : response({ artifact, version: { number: 1 } });
      }
      if (request.pathname.endsWith("/text"))
        return response({ markdown: "# Runbook", version: 1 });
      if (request.pathname.endsWith("/edits"))
        return response({ applied: 1, version: { number: 2 } });
      if (request.pathname.endsWith("/events")) return response([]);
      if (request.pathname === "/api/v1/references") return response(emptyGraph("artifact"));
      if (request.pathname.endsWith("/references"))
        return response({ outgoing: [], referenced_by: [] });
      if (request.pathname.endsWith("/asks")) {
        return init?.method === "POST"
          ? response({
              id: "ask-42",
              issue_key: null,
              artifact_id: artifact.id,
              question: runbookQuestion,
            })
          : response([]);
      }
      if (request.pathname.endsWith("/comments")) {
        return init?.method === "POST"
          ? response({ id: "comment-42", issue_key: null, artifact_id: artifact.id })
          : response([]);
      }
      throw new Error(`unexpected request: ${request.pathname}`);
    };

    const result = await executeDispatchTool({
      tool,
      args,
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-42",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(paths).toContain(expectedPath);
    if (writes) {
      expect(result.details).not.toHaveProperty("topic");
      expect(result.details).toMatchObject({
        project: "CORE",
        document: "CORE/runbook-md",
      });
      expect(result.text).toContain(
        tool === "dispatch_ask"
          ? "For every event on CORE/runbook-md: envoy_subscribe notifications.dispatch.document.CORE.runbook-md.>"
          : "(not subscribed to CORE/runbook-md; envoy_subscribe notifications.dispatch.document.CORE.runbook-md.> for every event on it)"
      );
    } else {
      expect(dispatchFollowNotice(result.details)).toBeNull();
    }
  });

  test.each([
    ["slug", "runbook-md"],
    ["id", "artifact-42"],
    ["filename", "Runbook.md"],
  ])("resolves a project document by %s for every document-owning tool", async (_form, artifactReference) => {
    const paths: string[] = [];
    const artifact = {
      id: "artifact-42",
      issue_key: null,
      project: "CORE",
      ref_key: "CORE/runbook-md",
      slug: "runbook-md",
      name: "Runbook.md",
      kind: "doc",
      primary: false,
      created_by: { kind: "session", id: "session-42" },
      created_at: "2026-09-12T00:00:00Z",
      versions: [],
    };
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const request = new URL(String(url));
      paths.push(request.pathname + request.search);
      if (request.pathname.startsWith("/api/v1/projects/CORE/artifacts/")) {
        return request.pathname.endsWith("/runbook-md")
          ? response(artifact)
          : new Response(JSON.stringify({ code: "ARTIFACT_NOT_FOUND", error: "not found" }), {
              status: 404,
              headers: { "Content-Type": "application/json" },
            });
      }
      if (request.pathname === "/api/v1/projects/CORE/artifacts") return response([artifact]);
      if (request.pathname.endsWith("/text"))
        return response({ markdown: "# Runbook", version: 1 });
      if (request.pathname.endsWith("/asks")) {
        return init?.method === "POST"
          ? response({ id: "ask-42", issue_key: null, artifact_id: artifact.id })
          : response([]);
      }
      if (request.pathname.endsWith("/comments")) {
        return init?.method === "POST"
          ? response({ id: "comment-42", issue_key: null, artifact_id: artifact.id })
          : response([]);
      }
      if (request.pathname.endsWith("/edits")) return response({ applied: 1, version: null });
      throw new Error(`unexpected request: ${request.pathname}`);
    };
    const shared = {
      cwd: "/workspace",
      host: "omp" as const,
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    };

    for (const [tool, args] of [
      ["dispatch_ask", { project: "CORE", artifact: artifactReference, question: runbookQuestion }],
      ["dispatch_comment", { project: "CORE", artifact: artifactReference, body: "Looks good." }],
      [
        "dispatch_suggest",
        { project: "CORE", artifact: artifactReference, quote: "draft", replace_with: "final" },
      ],
      [
        "dispatch_doc_edit",
        {
          project: "CORE",
          artifact: artifactReference,
          ops: [{ op: "replace", find: "draft", with: "final" }],
        },
      ],
      ["dispatch_doc_read", { project: "CORE", artifact: artifactReference }],
      ["dispatch_read", { project: "CORE", artifact: artifactReference }],
    ] as const) {
      const result = await executeDispatchTool({ tool, args, ...shared });
      expect(result.details).toMatchObject({ document: "CORE/runbook-md", project: "CORE" });
    }

    expect(paths).toContain(
      artifactReference === "runbook-md"
        ? "/api/v1/projects/CORE/artifacts/runbook-md"
        : "/api/v1/projects/CORE/artifacts?unlinked=true"
    );
  });

  test("reads a project document from a dispatch project reference", async () => {
    const paths: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const request = new URL(String(url));
      paths.push(request.pathname);
      if (request.pathname === "/api/v1/projects/CORE/artifacts/runbook-md") {
        return response({
          id: "artifact-42",
          issue_key: null,
          project: "CORE",
          ref_key: "CORE/runbook-md",
          slug: "runbook-md",
          name: "Runbook.md",
          kind: "doc",
          primary: false,
          created_by: { kind: "session", id: "session-42" },
          created_at: "2026-09-11T00:00:00Z",
          versions: [],
        });
      }
      if (request.pathname.endsWith("/text"))
        return response({ markdown: "# Runbook", version: 1 });
      if (request.pathname.endsWith("/asks") || request.pathname.endsWith("/comments"))
        return response([]);
      throw new Error(`unexpected request: ${request.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_doc_read",
      args: { ref: "dispatch://CORE/artifact/runbook-md" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe("# Runbook");
    expect(result.details).toEqual({ project: "CORE", document: "CORE/runbook-md" });
    expect(paths).toEqual([
      "/api/v1/projects/CORE/artifacts/runbook-md",
      "/api/v1/artifacts/artifact-42/text",
      "/api/v1/artifacts/artifact-42/asks",
      "/api/v1/artifacts/artifact-42/comments",
    ]);
  });

  test("rejects conflicting project owners, missing document names, and invalid project keys", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;
    const shared = {
      cwd: "/workspace",
      host: "omp" as const,
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl,
    };

    await expect(
      executeDispatchTool({
        tool: "dispatch_doc_read",
        args: { issue: "CORE-1", project: "CORE", artifact: "runbook-md" },
        ...shared,
      })
    ).rejects.toThrow("exactly one of issue and project");
    await expect(
      executeDispatchTool({
        tool: "dispatch_doc_read",
        args: { project: "CORE" },
        ...shared,
      })
    ).rejects.toThrow("with project, artifact names the document");
    await expect(
      executeDispatchTool({
        tool: "dispatch_doc_read",
        args: { project: "not-a-project", artifact: "runbook-md" },
        ...shared,
      })
    ).rejects.toThrow("project must be a project key");
  });
  test("uploads a relative artifact path from the Dispatch process cwd", async () => {
    const cwd = mkdtempSync(path.join(os.tmpdir(), "dispatch-execute-"));
    writeFileSync(path.join(cwd, "artifact.txt"), "artifact from Dispatch cwd");
    const requests: Array<{ url: string; init: RequestInit }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const request = { url: String(url), init: init ?? {} };
      requests.push(request);
      if (new URL(request.url).pathname === "/api/v1/issues/DSP-42/artifacts") {
        return response({
          artifact: { id: "artifact-42", issue_key: "DSP-42", name: "artifact.txt" },
          version: { number: 1 },
        });
      }
      throw new Error(`unexpected request: ${new URL(request.url).pathname}`);
    };

    try {
      const result = await executeDispatchTool({
        tool: "dispatch_artifact",
        args: { issue: "DSP-42", name: "artifact.txt", path: "artifact.txt" },
        cwd,
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      });

      expect(result.details).toMatchObject({ issue: "DSP-42", artifact: "artifact-42" });
      expect(requests).toHaveLength(1);
      const form = requests[0]?.init.body as FormData;
      const file = form.get("file");
      expect(file).toBeInstanceOf(Blob);
      expect(await (file as Blob).text()).toBe("artifact from Dispatch cwd");
    } finally {
      rmSync(cwd, { recursive: true, force: true });
    }
  });

  test("uploads inline artifact content as JSON", async () => {
    const requests: Array<{ url: string; init: RequestInit }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const request = { url: String(url), init: init ?? {} };
      requests.push(request);
      return response({
        artifact: { id: "artifact-42", issue_key: "DSP-42", name: "spec.md" },
        version: { number: 1 },
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_artifact",
      args: { issue: "DSP-42", name: "spec.md", content: "# Spec\n" },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-42",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.details).toMatchObject({ issue: "DSP-42", artifact: "artifact-42", version: 1 });
    expect(requests).toHaveLength(1);
    expect(requests[0]?.init.headers).toMatchObject({ "Content-Type": "application/json" });
    const body = JSON.parse(requests[0]?.init.body as string);
    expect(body).toMatchObject({
      name: "spec.md",
      content: "# Spec\n",
      actor: { kind: "session", id: "session-42" },
    });
    expect(body).not.toHaveProperty("primary");
  });

  test("reports the artifact slug and dispatch:// reference in the upload result text", async () => {
    const fetchImpl = async (_url: RequestInfo | URL, _init?: RequestInit): Promise<Response> =>
      response({
        artifact: {
          id: "artifact-42",
          issue_key: "DSP-42",
          name: "garrett-reply-draft.md",
          slug: "garrett-reply-draft-md",
        },
        version: { number: 1 },
      });

    const result = await executeDispatchTool({
      tool: "dispatch_artifact",
      args: { issue: "DSP-42", name: "garrett-reply-draft.md", content: "# Reply\n" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe(
      "Uploaded garrett-reply-draft.md as version 1 " +
        "(artifact slug garrett-reply-draft-md; dispatch://DSP-42/artifact/garrett-reply-draft-md) " +
        "(not subscribed to DSP-42; envoy_subscribe notifications.dispatch.issue.DSP-42.> for every event on it)"
    );
  });

  test.each([
    ["neither path nor content", { issue: "DSP-42", name: "spec.md" }],
    [
      "both path and content",
      { issue: "DSP-42", name: "spec.md", path: "spec.md", content: "# Spec\n" },
    ],
  ])("rejects an artifact call with %s", async (_name, args) => {
    await expect(
      executeDispatchTool({
        tool: "dispatch_artifact",
        args,
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: (async () => response({})) as unknown as typeof fetch,
      })
    ).rejects.toThrow("Exactly one of path or content is required.");
  });

  for (const input of [
    {
      tool: "dispatch_message" as const,
      args: { issue: "owner/repo#999", body: "Proceed" },
    },
    {
      tool: "dispatch_read" as const,
      args: { issue: "owner/repo#999" },
    },
  ]) {
    test(`${input.tool} refuses an unlinked external reference without creating an issue`, async () => {
      const requests: Array<{ url: string; init: RequestInit }> = [];
      const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const request = { url: String(url), init: init ?? {} };
        requests.push(request);
        const target = new URL(request.url);
        if (target.pathname === "/api/v1/issues/resolve") {
          return new Response(JSON.stringify({ code: "NOT_FOUND", error: "issue not found" }), {
            status: 404,
            headers: { "Content-Type": "application/json" },
          });
        }
        if (target.pathname === "/api/v1/issues") return response({ key: "DSP-42" });
        if (target.pathname === "/api/v1/issues/DSP-42/messages") {
          return response({ id: "message-42", issue_key: "DSP-42" });
        }
        throw new Error(`unexpected request: ${target.pathname}`);
      };

      await expect(
        executeDispatchTool({
          tool: input.tool,
          args: input.args,
          cwd: "/workspace",
          host: "omp",
          config,
          env: {},
          exec: repoExec("owner/repo"),
          fetchImpl: fetchImpl as typeof fetch,
        })
      ).rejects.toThrow('dispatch_issue({ external: "owner/repo#999", ... })');
      expect(
        requests.filter(
          (request) =>
            request.init.method === "POST" && new URL(request.url).pathname === "/api/v1/issues"
        )
      ).toEqual([]);
    });
  }

  test("dispatch_issue creates an issue from an external reference", async () => {
    const requests: Array<{ url: string; init: RequestInit }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const request = { url: String(url), init: init ?? {} };
      requests.push(request);
      const target = new URL(request.url);
      if (target.pathname === "/api/v1/issues") {
        return response({
          key: "TEST-1",
          title: "External issue",
          project: "TEST",
          components: issueComponents("inherit", []),
        });
      }
      if (target.pathname === "/api/v1/projects/TEST/architecture-source") {
        return sourceNull();
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    await expect(
      executeDispatchTool({
        tool: "dispatch_issue",
        args: {
          project: "TEST",
          title: "External issue",
          external: "owner/repo#999",
        },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).resolves.toMatchObject({ details: { issue: "TEST-1" } });
    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual(["/api/v1/issues", "/api/v1/projects/TEST/architecture-source"]);
    expect(JSON.parse(requests[0]?.init.body as string)).toMatchObject({
      project: "TEST",
      title: "External issue",
      external: "owner/repo#999",
    });
  });

  test("does not send a blank body when posting a suggestion without a rationale", async () => {
    const requests: Array<{ url: string; init: RequestInit }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const request = { url: String(url), init: init ?? {} };
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [{ id: "artifact-42", slug: "spec", name: "spec.md", primary: true }],
        });
      }
      if (path === "/api/v1/issues/DSP-42/comments") {
        return response({ id: "comment-42", issue_key: "DSP-42" });
      }
      throw new Error(`unexpected request: ${path}`);
    };

    await executeDispatchTool({
      tool: "dispatch_suggest",
      args: {
        issue: "DSP-42",
        artifact: "spec",
        quote: "draft",
        replace_with: "final",
      },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(JSON.parse(requests[1]?.init.body as string)).toMatchObject({
      anchor: { artifact: "artifact-42", quote: "draft" },
      suggestion: { replace_with: "final" },
    });
    expect(JSON.parse(requests[1]?.init.body as string)).not.toHaveProperty("body");
  });

  test("reads the targeted ask, its reply thread, from a Dispatch ask reference", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      if (target.pathname === "/api/v1/asks/aaaaaaaa-0000-4000-8000-000000000042") {
        return response({
          ask: {
            id: "aaaaaaaa-0000-4000-8000-000000000042",
            issue_key: "DSP-42",
            author: { kind: "session", id: "author-1" },
            question: "The service needs a public API. Which approach should we use?",
            options: [
              { label: "JSON API", description: "Use the HTTP API." },
              { label: "MCP API" },
            ],
            multiple: false,
            urgency: "high",
            anchor: null,
            state: "answered",
            answer: {
              user: "sami",
              selected: ["JSON"],
              text: "Ship JSON.",
              at: "2026-09-09T00:00:00Z",
            },
            created_at: "2026-09-09T00:00:00Z",
          },
          replies: [
            {
              id: "comment-1",
              issue_key: "DSP-42",
              author: { kind: "user", id: "sami" },
              body: "JSON, please.",
              anchor: null,
              reply_to: null,
              ask_id: "aaaaaaaa-0000-4000-8000-000000000042",
              resolved: false,
              suggestion: null,
              created_at: "2026-09-08T23:59:00Z",
            },
          ],
        });
      }
      if (target.pathname === "/api/v1/references" && target.searchParams.has("to")) {
        return response({
          node: { kind: "ask", id: "aaaaaaaa-0000-4000-8000-000000000042" },
          edges: [
            {
              kind: "mentions",
              direction: "in",
              node: {
                kind: "message",
                id: "message-7",
                issue_key: "AGENTC-3",
                project: "AGENTC",
                ref: "dispatch://AGENTC-3/message/message-7",
              },
              excerpt: {
                text: "Decided in dispatch://DSP-42/ask/aaaaaaaa-0000-4000-8000-000000000042.\nShipping.",
              },
              created_at: "2026-09-10T08:00:00Z",
              source_seq: 918,
            },
          ],
        });
      }
      if (target.pathname === "/api/v1/references" && target.searchParams.has("from")) {
        return response({
          node: { kind: "ask", id: "aaaaaaaa-0000-4000-8000-000000000042" },
          edges: [
            {
              kind: "followed_by",
              direction: "out",
              node: { kind: "session", id: "author-1" },
              created_at: "2026-09-09T00:00:00Z",
              source_seq: null,
            },
          ],
        });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/ask/aaaaaaaa-0000-4000-8000-000000000042" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result).toEqual({
      text: [
        "Question: The service needs a public API. Which approach should we use?",
        "Options:",
        "- JSON API — Use the HTTP API.",
        "- MCP API",
        "State: answered",
        "Answer:",
        "- By: sami",
        "- Selected: JSON",
        "- Text: Ship JSON.",
        "Replies:",
        "comment-1 · user sami",
        "Body: JSON, please.",
        "Referenced by:",
        "- mentions message dispatch://AGENTC-3/message/message-7 (Decided in dispatch://DSP-42/ask/aaaaaaaa-0000-4000-8000-000000000042. Shipping. · 2026-09-10T08:00:00Z)",
        "Links:",
        "- followed_by session author-1 (2026-09-09T00:00:00Z)",
      ].join("\n"),
      details: { issue: "DSP-42" },
    });
    expect(dispatchFollowNotice(result.details)).toBeNull();
    expect(requests).toEqual([
      "/api/v1/asks/aaaaaaaa-0000-4000-8000-000000000042",
      "/api/v1/references?to=dispatch%3A%2F%2FDSP-42%2Fask%2Faaaaaaaa-0000-4000-8000-000000000042",
      "/api/v1/references?from=dispatch%3A%2F%2FDSP-42%2Fask%2Faaaaaaaa-0000-4000-8000-000000000042",
    ]);
  });

  test("posts a comment reply to an ask using reply_to_ask", async () => {
    const requests: Array<{ pathname: string; body: unknown }> = [];
    const askID = "01234567-0000-4000-8000-000000000042";
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      requests.push({ pathname: target.pathname, body: JSON.parse(String(init?.body)) });
      return response({
        id: "comment-1",
        issue_key: "DSP-42",
        author: { kind: "session", id: "session-1" },
        body: "I'd go with JSON.",
        anchor: null,
        reply_to: null,
        ask_id: askID,
        turn: "human",
        ask_waiting_on: "human",
        resolved: false,
        suggestion: null,
        created_at: "2026-09-09T00:00:00Z",
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_comment",
      args: { issue: "DSP-42", body: "I'd go with JSON.", reply_to_ask: askID },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe(
      `Replied on ask ${askID} (comment comment-1; ask now waiting on human). ` +
        "You follow this ask: its answer and replies reach you directly. " +
        "For every event on DSP-42: envoy_subscribe notifications.dispatch.issue.DSP-42.>"
    );
    expect(result.details).toEqual({
      issue: "DSP-42",
      comment: "comment-1",
      ask: askID,
      follows: { ask: askID },
      ask_waiting_on: "human",
    });
    expect(requests).toEqual([
      {
        pathname: "/api/v1/issues/DSP-42/comments",
        body: expect.objectContaining({ ask_id: askID, body: "I'd go with JSON." }),
      },
    ]);
  });

  test.each([
    ["uppercase", "a1b2c3d4-e5f6-4a7b-8c9d-e0f1a2b3c4d5".toUpperCase()],
    ["compact", "a1b2c3d4e5f64a7b8c9de0f1a2b3c4d5"],
    ["URN", "urn:uuid:a1b2c3d4-e5f6-4a7b-8c9d-e0f1a2b3c4d5"],
    ["braced", "{a1b2c3d4-e5f6-4a7b-8c9d-e0f1a2b3c4d5}"],
  ])("normalizes a %s UUID in reply_to_ask", async (_form, suppliedID) => {
    const askID = "a1b2c3d4-e5f6-4a7b-8c9d-e0f1a2b3c4d5";
    const requests: Array<{ pathname: string; body: unknown }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      requests.push({
        pathname: target.pathname,
        body: JSON.parse(String(init?.body)),
      });
      return response({
        id: "comment-1",
        issue_key: "DSP-42",
        ask_id: askID,
        turn: "human",
      });
    };

    await executeDispatchTool({
      tool: "dispatch_comment",
      args: { issue: "DSP-42", body: "Use the UUID.", reply_to_ask: suppliedID },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(requests).toEqual([
      {
        pathname: "/api/v1/issues/DSP-42/comments",
        body: expect.objectContaining({ ask_id: askID }),
      },
    ]);
  });

  test("posts an ask progress note with turn agent and reports the ask still waits on the agent", async () => {
    const requests: Array<{ pathname: string; body: unknown }> = [];
    const askID = "01234567-0000-4000-8000-000000000043";
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const target = new URL(String(url));
      requests.push({ pathname: target.pathname, body: JSON.parse(String(init?.body)) });
      return response({
        id: "comment-2",
        issue_key: "DSP-42",
        author: { kind: "session", id: "session-1" },
        body: "Dispatched two auditors, back with results.",
        anchor: null,
        reply_to: null,
        ask_id: askID,
        turn: "agent",
        ask_waiting_on: "agent",
        resolved: false,
        suggestion: null,
        created_at: "2026-09-09T00:00:00Z",
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_comment",
      args: {
        issue: "DSP-42",
        body: "Dispatched two auditors, back with results.",
        reply_to_ask: askID,
        turn: "agent",
      },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe(
      `Replied on ask ${askID} (comment comment-2; ask now waiting on agent). ` +
        "You follow this ask: its answer and replies reach you directly. " +
        "For every event on DSP-42: envoy_subscribe notifications.dispatch.issue.DSP-42.>"
    );
    expect(result.details).toMatchObject({ ask_waiting_on: "agent" });
    expect(requests).toEqual([
      {
        pathname: "/api/v1/issues/DSP-42/comments",
        body: expect.objectContaining({ ask_id: askID, turn: "agent" }),
      },
    ]);
  });

  test("reports a moved approval ask's derived turn instead of the replying agent's turn", async () => {
    const askID = "01234567-0000-4000-8000-000000000045";
    const result = await executeDispatchTool({
      tool: "dispatch_comment",
      args: { issue: "DSP-42", body: "The revision is ready.", reply_to_ask: askID },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: (async () =>
        response({
          id: "comment-3",
          issue_key: "DSP-42",
          ask_id: askID,
          turn: "human",
          ask_waiting_on: "agent",
        })) as unknown as typeof fetch,
    });

    expect(result.text).toContain("ask now waiting on agent");
    expect(result.details).toMatchObject({ ask_waiting_on: "agent" });
  });

  test("rejects a comment turn without reply_to_ask before calling the server", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_comment",
        args: { issue: "DSP-42", body: "Working on it.", turn: "agent" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(/turn requires reply_to_ask/);
  });

  test("reports no waiting state for a reply the server recorded without a turn (closed ask)", async () => {
    const askID = "01234567-0000-4000-8000-000000000044";
    const fetchImpl = async (_url: RequestInfo | URL, _init?: RequestInit): Promise<Response> =>
      response({
        id: "comment-3",
        issue_key: "DSP-42",
        author: { kind: "session", id: "session-1" },
        body: "Shipped in #42.",
        anchor: null,
        reply_to: null,
        ask_id: askID,
        turn: null,
        resolved: false,
        suggestion: null,
        created_at: "2026-09-09T00:00:00Z",
      });

    const result = await executeDispatchTool({
      tool: "dispatch_comment",
      args: { issue: "DSP-42", body: "Shipped in #42.", reply_to_ask: askID, turn: "agent" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe(
      `Replied on ask ${askID} (comment comment-3). ` +
        "You follow this ask: its answer and replies reach you directly. " +
        "For every event on DSP-42: envoy_subscribe notifications.dispatch.issue.DSP-42.>"
    );
    expect(result.details).not.toHaveProperty("ask_waiting_on");
  });

  test("a plain comment follows nothing and names the whole-issue opt-in", async () => {
    const fetchImpl = async (): Promise<Response> =>
      response({ id: "comment-2", issue_key: "DSP-42", ask_id: null, reply_to: null });

    const result = await executeDispatchTool({
      tool: "dispatch_comment",
      args: { issue: "DSP-42", body: "Looks good." },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as unknown as typeof fetch,
    });

    expect(result.text).toBe(
      "Posted comment comment-2 (not subscribed to DSP-42; envoy_subscribe notifications.dispatch.issue.DSP-42.> for every event on it)"
    );
    expect(result.details).toEqual({ issue: "DSP-42", comment: "comment-2" });
    expect(dispatchFollowNotice(result.details)).toBeNull();
  });

  test.each([
    [
      "follow",
      "PUT",
      [
        "/api/v1/asks/5a660655-04ad-4ce0-8a9b-93dd03c412b7",
        "/api/v1/asks/5a660655-04ad-4ce0-8a9b-93dd03c412b7/followers/session-7",
      ],
    ],
    [
      "unfollow",
      "DELETE",
      ["/api/v1/asks/5a660655-04ad-4ce0-8a9b-93dd03c412b7/followers/session-7"],
    ],
  ])("dispatch_follow %s names the calling session in the path and the body", async (action, method, paths) => {
    const askUuid = "5a660655-04ad-4ce0-8a9b-93dd03c412b7";
    const requests: Array<{ method: string; pathname: string; body: unknown }> = [];
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const pathname = new URL(String(url)).pathname;
      requests.push({
        method: init?.method ?? "GET",
        pathname,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
      });
      if (pathname === `/api/v1/asks/${askUuid}`) {
        return response({
          ask: { id: askUuid, issue_key: "DSP-42", question: releaseQuestion },
          replies: [],
          edits: [],
          followers: [{ session_id: "session-1", since: "2026-09-14T00:00:00Z" }],
        });
      }
      return new Response(null, { status: 204 });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_follow",
      args: { ask: `dispatch://DSP-42/ask/${askUuid}`, action },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-7",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(requests.map((request) => request.pathname)).toEqual(paths);
    const change = requests.at(-1);
    expect(change?.method).toBe(method);
    expect(change?.body).toEqual({
      actor: expect.objectContaining({ kind: "session", id: "session-7" }),
    });
    if (action === "follow") {
      expect(result.text).toBe(
        `Following ask ${askUuid}: its answer and replies reach this session directly.`
      );
      expect(result.details).toEqual({
        issue: "DSP-42",
        ask: askUuid,
        follows: { ask: askUuid },
      });
    } else {
      expect(result.text).toBe(`Unfollowed ask ${askUuid}.`);
      expect(result.details).toEqual({ ask: askUuid });
      expect(dispatchFollowNotice(result.details)).toBeNull();
    }
  });

  test("dispatch_follow needs the host session id", async () => {
    await expect(
      executeDispatchTool({
        tool: "dispatch_follow",
        args: { ask: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", action: "follow" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: (() => {
          throw new Error("network must not be called");
        }) as unknown as typeof fetch,
      })
    ).rejects.toThrow("host session id is required for dispatch_follow");
  });

  test("rejects a comment reply that names both reply_to and reply_to_ask", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_comment",
        args: {
          issue: "DSP-42",
          body: "Which one?",
          reply_to: "comment-1",
          reply_to_ask: "ask-42",
        },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(/reply_to and reply_to_ask/);
  });

  test("reads the targeted comment and quoted reply chain from a Dispatch comment reference", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      if (target.pathname === "/api/v1/comments/cccccccc-0000-4000-8000-000000000042") {
        return response({
          comment: {
            id: "cccccccc-0000-4000-8000-000000000042",
            issue_key: "DSP-42",
            author: { kind: "session", id: "reviewer-1" },
            body: "Please revise this.",
            anchor: {
              artifact_id: "artifact-42",
              mark_id: "m-1",
              version: 1,
              quote: "Initial wording",
              orphaned: false,
            },
            reply_to: null,
            resolved: false,
            suggestion: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          replies: [
            {
              id: "comment-43",
              issue_key: "DSP-42",
              author: { kind: "user", id: "sami" },
              body: "Revised.",
              anchor: {
                artifact_id: "artifact-42",
                mark_id: "m-2",
                version: 2,
                quote: "Revised wording",
                orphaned: false,
              },
              reply_to: "cccccccc-0000-4000-8000-000000000042",
              resolved: false,
              suggestion: null,
              created_at: "2026-09-09T00:01:00Z",
            },
          ],
        });
      }
      if (target.pathname === "/api/v1/references") return response(emptyGraph("comment"));
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/comment/cccccccc-0000-4000-8000-000000000042" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result).toEqual({
      text: [
        "Comment:",
        "cccccccc-0000-4000-8000-000000000042 · session reviewer-1",
        "> Initial wording",
        "Body: Please revise this.",
        "Reply chain:",
        "comment-43 · user sami",
        "> Revised wording",
        "Body: Revised.",
        "Referenced by:",
        "- none",
        "Links:",
        "- none",
      ].join("\n"),
      details: { issue: "DSP-42" },
    });
    expect(dispatchFollowNotice(result.details)).toBeNull();
    expect(requests).toEqual([
      "/api/v1/comments/cccccccc-0000-4000-8000-000000000042",
      "/api/v1/references?to=dispatch%3A%2F%2FDSP-42%2Fcomment%2Fcccccccc-0000-4000-8000-000000000042",
      "/api/v1/references?from=dispatch%3A%2F%2FDSP-42%2Fcomment%2Fcccccccc-0000-4000-8000-000000000042",
    ]);
  });
  test("prints where an anchored comment's block stands after its quote", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/comments/cccccccc-0000-4000-8000-000000000042") {
        return response({
          comment: {
            id: "cccccccc-0000-4000-8000-000000000042",
            issue_key: "DSP-42",
            author: { kind: "user", id: "sami" },
            body: "I want this done today",
            anchor: {
              artifact_id: "artifact-42",
              block_id: "p-5-2",
              mark_id: "m-1",
              version: 1,
              quote: "Today, Oct 1",
              orphaned: false,
            },
            anchor_block: anchoredCell,
            reply_to: null,
            resolved: false,
            suggestion: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          replies: [],
        });
      }
      if (target.pathname === "/api/v1/references") return response(emptyGraph("comment"));
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/comment/cccccccc-0000-4000-8000-000000000042" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe(
      [
        "Comment:",
        "cccccccc-0000-4000-8000-000000000042 · user sami",
        "> Today, Oct 1",
        "Position: table[3] › row 5 (Red-teamer loop), column Due",
        "Body: I want this done today",
        "Reply chain:",
        "- none",
        "Referenced by:",
        "- none",
        "Links:",
        "- none",
      ].join("\n")
    );
  });

  test("prints an anchored ask's quote and where its block stands after the question", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/asks/aaaaaaaa-0000-4000-8000-000000000042") {
        return response({
          ask: {
            id: "aaaaaaaa-0000-4000-8000-000000000042",
            issue_key: "DSP-42",
            author: { kind: "session", id: "author-1" },
            question: "Which day is meant?",
            options: [],
            multiple: false,
            urgency: "high",
            anchor: {
              artifact_id: "artifact-42",
              block_id: "p-5-2",
              mark_id: "m-1",
              version: 1,
              quote: "Today, Oct 1",
              orphaned: false,
            },
            anchor_block: anchoredCell,
            state: "open",
            answer: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          replies: [],
        });
      }
      if (target.pathname === "/api/v1/references") return response(emptyGraph("ask"));
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/ask/aaaaaaaa-0000-4000-8000-000000000042" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe(
      [
        "Question: Which day is meant?",
        "> Today, Oct 1",
        "Position: table[3] › row 5 (Red-teamer loop), column Due",
        "Options:",
        "- none",
        "State: open",
        "Answer:",
        "- none",
        "Replies:",
        "- none",
        "Referenced by:",
        "- none",
        "Links:",
        "- none",
      ].join("\n")
    );
  });

  test("says the position is unavailable, and why, when Dispatch could not read the document", async () => {
    const anchor = {
      artifact_id: "artifact-42",
      block_id: "p-5-2",
      mark_id: "m-1",
      version: 1,
      quote: "Today, Oct 1",
      orphaned: false,
    };
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/comments/cccccccc-0000-4000-8000-000000000042") {
        return response({
          comment: {
            id: "cccccccc-0000-4000-8000-000000000042",
            issue_key: "DSP-42",
            author: { kind: "user", id: "sami" },
            body: "I want this done today",
            anchor,
            anchor_block_error: "DOC_SERVICE_UNAVAILABLE",
            reply_to: null,
            resolved: false,
            suggestion: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          replies: [],
        });
      }
      if (target.pathname === "/api/v1/asks/aaaaaaaa-0000-4000-8000-000000000042") {
        return response({
          ask: {
            id: "aaaaaaaa-0000-4000-8000-000000000042",
            issue_key: "DSP-42",
            author: { kind: "session", id: "author-1" },
            question: "Which day is meant?",
            options: [],
            multiple: false,
            urgency: "high",
            anchor,
            anchor_block_error: "DOC_SCHEMA",
            state: "open",
            answer: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          replies: [],
        });
      }
      if (target.pathname === "/api/v1/references") return response(emptyGraph("node"));
      throw new Error(`unexpected request: ${target.pathname}`);
    };
    const tool = {
      tool: "dispatch_read",
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    } as const;

    const comment = await executeDispatchTool({
      ...tool,
      args: { ref: "dispatch://DSP-42/comment/cccccccc-0000-4000-8000-000000000042" },
    });
    expect(comment.text.split("\n").slice(0, 5)).toEqual([
      "Comment:",
      "cccccccc-0000-4000-8000-000000000042 · user sami",
      "> Today, Oct 1",
      "Position: unavailable (DOC_SERVICE_UNAVAILABLE)",
      "Body: I want this done today",
    ]);
    const ask = await executeDispatchTool({
      ...tool,
      args: { ref: "dispatch://DSP-42/ask/aaaaaaaa-0000-4000-8000-000000000042" },
    });
    expect(ask.text.split("\n").slice(0, 4)).toEqual([
      "Question: Which day is meant?",
      "> Today, Oct 1",
      "Position: unavailable (DOC_SCHEMA)",
      "Options:",
    ]);
  });

  test("reads the targeted message and its reply chain from a Dispatch message reference", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      if (target.pathname === "/api/v1/issues/DSP-42/messages/message-42") {
        return response({
          message: {
            id: "message-42",
            issue_key: "DSP-42",
            author: { kind: "session", id: "writer-1" },
            body: "Ship the build tonight.",
            in_reply_to: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          replies: [
            {
              id: "message-43",
              issue_key: "DSP-42",
              author: { kind: "user", id: "sami" },
              body: "Sounds good.",
              in_reply_to: "message-42",
              created_at: "2026-09-09T00:01:00Z",
            },
          ],
        });
      }
      if (target.pathname === "/api/v1/references") return response(emptyGraph("message"));
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/message/message-42" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result).toEqual({
      text: [
        "Message:",
        "message-42 · session writer-1",
        "Body: Ship the build tonight.",
        "Reply chain:",
        "message-43 · user sami",
        "Body: Sounds good.",
        "Referenced by:",
        "- none",
        "Links:",
        "- none",
      ].join("\n"),
      details: { issue: "DSP-42" },
    });
    expect(dispatchFollowNotice(result.details)).toBeNull();
    expect(requests).toEqual([
      "/api/v1/issues/DSP-42/messages/message-42",
      "/api/v1/references?to=dispatch%3A%2F%2FDSP-42%2Fmessage%2Fmessage-42",
      "/api/v1/references?from=dispatch%3A%2F%2FDSP-42%2Fmessage%2Fmessage-42",
    ]);
  });

  test("a comment thread a service token wrote names the service account on both actor lines", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/comments/cccccccc-0000-4000-8000-000000000042") {
        return response({
          comment: {
            id: "cccccccc-0000-4000-8000-000000000042",
            issue_key: "DSP-42",
            author: {
              kind: "session",
              id: "reviewer-1",
              service: "system:serviceaccount:legion:legion-worker",
            },
            body: "Please revise this.",
            anchor: null,
            reply_to: null,
            resolved: false,
            suggestion: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          replies: [
            {
              id: "comment-43",
              issue_key: "DSP-42",
              author: {
                kind: "session",
                id: "reviewer-2",
                service: "system:serviceaccount:legion:dispatch",
              },
              body: "Revised.",
              anchor: null,
              reply_to: "cccccccc-0000-4000-8000-000000000042",
              resolved: false,
              suggestion: null,
              created_at: "2026-09-09T00:01:00Z",
            },
          ],
        });
      }
      if (target.pathname === "/api/v1/references") return response(emptyGraph("comment"));
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/comment/cccccccc-0000-4000-8000-000000000042" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text.split("\n").slice(0, 5)).toEqual([
      "Comment:",
      "cccccccc-0000-4000-8000-000000000042 · session reviewer-1 (as legion/legion-worker)",
      "Body: Please revise this.",
      "Reply chain:",
      "comment-43 · session reviewer-2 (as legion/dispatch)",
    ]);
  });

  test("a message thread a service token wrote names the service account on both actor lines", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42/messages/message-42") {
        return response({
          message: {
            id: "message-42",
            issue_key: "DSP-42",
            author: {
              kind: "session",
              id: "writer-1",
              service: "system:serviceaccount:legion:legion-worker",
            },
            body: "Ship the build tonight.",
            in_reply_to: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          replies: [
            {
              id: "message-43",
              issue_key: "DSP-42",
              author: {
                kind: "session",
                id: "writer-2",
                service: "system:serviceaccount:legion:dispatch",
              },
              body: "Sounds good.",
              in_reply_to: "message-42",
              created_at: "2026-09-09T00:01:00Z",
            },
          ],
        });
      }
      if (target.pathname === "/api/v1/references") return response(emptyGraph("message"));
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/message/message-42" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text.split("\n").slice(0, 5)).toEqual([
      "Message:",
      "message-42 · session writer-1 (as legion/legion-worker)",
      "Body: Ship the build tonight.",
      "Reply chain:",
      "message-43 · session writer-2 (as legion/dispatch)",
    ]);
  });

  test("an ask reply from a service session names the service account", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/asks/aaaaaaaa-0000-4000-8000-000000000042") {
        return response({
          ask: {
            id: "aaaaaaaa-0000-4000-8000-000000000042",
            issue_key: "DSP-42",
            author: { kind: "session", id: "author-1" },
            question: "The service needs a public API. Which approach should we use?",
            options: [],
            multiple: false,
            urgency: "med",
            anchor: null,
            state: "open",
            answer: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          replies: [
            {
              id: "comment-1",
              issue_key: "DSP-42",
              author: {
                kind: "session",
                id: "reviewer-1",
                service: "system:serviceaccount:legion:legion-worker",
              },
              body: "JSON, please.",
              anchor: null,
              reply_to: null,
              ask_id: "aaaaaaaa-0000-4000-8000-000000000042",
              resolved: false,
              suggestion: null,
              created_at: "2026-09-08T23:59:00Z",
            },
          ],
        });
      }
      if (target.pathname === "/api/v1/references") return response(emptyGraph("ask"));
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/ask/aaaaaaaa-0000-4000-8000-000000000042" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text.split("\n")).toContain(
      "comment-1 · session reviewer-1 (as legion/legion-worker)"
    );
  });

  test("the event log names the service account of an event a service token wrote", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          title: "Dispatch issue",
          status: "open",
          route: null,
          open_asks: [],
          children: [],
          last_seq: 3,
        });
      }
      if (target.pathname === "/api/v1/issues/DSP-42/events") {
        return response([
          {
            seq: 3,
            type: "ask.opened",
            actor: {
              kind: "session",
              id: "s1",
              service: "system:serviceaccount:legion:legion-worker",
            },
            created_at: "2026-09-09T00:03:00Z",
            payload: { question: releaseQuestion },
          },
        ]);
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/log" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text.split("\n")).toContain(
      `- #3 ask.opened · session s1 (as legion/legion-worker) · 2026-09-09T00:03:00Z · ${releaseQuestion}`
    );
  });
  test("reading an issue summary does not subscribe the session to the issue", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          title: "Dispatch issue",
          status: "open",
          route: null,
          external_links: [
            { url: "https://github.com/owner/repo/pull/7", kind: "github_pr" },
            { url: "https://example.com/runs/3" },
          ],
          open_asks: [],
          last_seq: 0,
          labels: ["frontend", "urgent"],
          priority: 1,
          assignee: "alice",
          claim: {
            actor: { kind: "session", id: "s1", origin: { session_title: "Implementer" } },
            at: "2026-09-13T01:00:00Z",
          },
          components: {
            mode: "explicit",
            ids: ["web"],
            unknown: ["legacy-ui"],
            reason: null,
            inherited_from: "DSP-40",
          },
        });
      }
      if (target.pathname === "/api/v1/issues/DSP-42/events") return response([]);
      if (target.pathname === "/api/v1/issues/DSP-42/references") {
        return response({
          members: [
            {
              artifact: { project: "CORE", slug: "runbook-md", name: "Runbook.md" },
              depth: 1,
              via: { kind: "comment", id: "comment-1" },
            },
          ],
          truncated: false,
        });
      }
      if (
        target.pathname === "/api/v1/references" &&
        target.searchParams.get("to") === "dispatch://DSP-42"
      ) {
        return response({
          node: { kind: "issue", id: "DSP-42", ref: "dispatch://DSP-42" },
          edges: [
            {
              kind: "mentions",
              direction: "in",
              node: {
                kind: "artifact",
                id: "artifact-9",
                project: "OPS",
                ref: "dispatch://OPS/artifact/design-notes",
              },
              excerpt: {
                block_id: "b7",
                text: "The plan lives in dispatch://DSP-42 and nowhere else.",
              },
              created_at: "2026-09-11T10:00:00Z",
              source_seq: 77,
            },
            {
              kind: "child_of",
              direction: "in",
              node: {
                kind: "issue",
                id: "DSP-43",
                issue_key: "DSP-43",
                project: "DSP",
                ref: "dispatch://DSP-43",
              },
              excerpt: { text: "Child work" },
              created_at: "2026-09-10T10:00:00Z",
              source_seq: null,
            },
          ],
        });
      }
      if (
        target.pathname === "/api/v1/references" &&
        target.searchParams.get("from") === "dispatch://DSP-42"
      ) {
        return response({ node: { kind: "issue", id: "DSP-42" }, edges: [] });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { issue: "DSP-42" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.details).toEqual({ issue: "DSP-42" });
    expect(dispatchFollowNotice(result.details)).toBeNull();
    expect(result.text).toContain("References:\n- CORE/runbook-md · depth 1 via comment comment-1");
    expect(result.text).toContain("Labels: frontend, urgent");
    expect(result.text).toContain("Priority: P1");
    expect(result.text).toContain("Status: open\nAssignee: alice\n");
    expect(result.text).toContain("Claimed by: Implementer since 2026-09-13T01:00:00Z");
    expect(result.text).toContain(
      "Labels: frontend, urgent\nComponents: web (inherited from DSP-40) (retired: legacy-ui)\nRoute: none\n"
    );
    // The pull request a person linked is on the read, as on the issue page; the reference graph
    // at the end carries none of it.
    expect(result.text).toContain(
      "Route: none\nExternal links:\n- https://github.com/owner/repo/pull/7 (github_pr)\n- https://example.com/runs/3\nOpen asks:"
    );
    expect(
      result.text.endsWith(
        [
          "Referenced by:",
          "- mentions artifact dispatch://OPS/artifact/design-notes (The plan lives in dispatch://DSP-42 and nowhere else. · 2026-09-11T10:00:00Z)",
          "- child_of issue dispatch://DSP-43 (Child work · 2026-09-10T10:00:00Z)",
          "Links:",
          "- none",
        ].join("\n")
      )
    ).toBe(true);
  });

  test("keeps an issue summary readable when its references are unavailable", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const pathname = new URL(String(url)).pathname;
      if (pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          title: "Dispatch issue",
          status: "open",
          priority: null,
          assignee: null,
          claim: null,
          components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
          route: null,
          open_asks: [],
          last_seq: 0,
          external_links: [],
          labels: [],
        });
      }
      if (pathname === "/api/v1/issues/DSP-42/events") return response([]);
      if (pathname === "/api/v1/issues/DSP-42/references" || pathname === "/api/v1/references") {
        return new Response(JSON.stringify({ error: "missing", code: "NOT_FOUND" }), {
          status: 404,
          headers: { "Content-Type": "application/json" },
        });
      }
      throw new Error(`unexpected request: ${pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { issue: "DSP-42" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toContain("Title: Dispatch issue");
    expect(result.text).toContain("Assignee: unassigned");
    expect(result.text).toContain("Claimed by: nobody");
    expect(result.text).toContain("Components: unassigned");
    expect(result.text).toContain("References:\n- unavailable");
    expect(result.text).toContain("Referenced by:\n- unavailable\nLinks:\n- unavailable");
    expect(result.details).toEqual({ issue: "DSP-42" });
  });

  // A bare "unavailable" means Dispatch has no such section (a server without the route). A
  // gateway's 404 page in its place is a failure the agent can act on, so the section says what
  // answered, as it does for every other failure.
  test("names a gateway's 404 page on the reference routes rather than reading it as unavailable", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const { pathname } = new URL(String(url));
      if (pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          title: "Dispatch issue",
          status: "open",
          priority: null,
          assignee: null,
          claim: null,
          components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
          route: null,
          open_asks: [],
          last_seq: 0,
          external_links: [],
          labels: [],
        });
      }
      if (pathname === "/api/v1/issues/DSP-42/events") return response([]);
      if (pathname === "/api/v1/issues/DSP-42/references" || pathname === "/api/v1/references") {
        return gatewayPage(404, "Not Found");
      }
      throw new Error(`unexpected request: ${pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { issue: "DSP-42" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toContain(
      "References:\n- unavailable: GET http://dispatch.test/api/v1/issues/DSP-42/references " +
        "answered 404 Not Found with a body that is not Dispatch's error JSON"
    );
    expect(result.text).toContain(
      "Referenced by:\n- unavailable: GET http://dispatch.test/api/v1/references?to="
    );
  });

  test("keeps a fatal issue-read error when a 404 reference response arrives first", async () => {
    const issue = deferred<Response>();
    const issueRequested = deferred<void>();
    let referencesRequested = false;
    const referenceResponseRead = deferred<void>();
    const issueResponse = response({
      key: "DSP-42",
      title: "Dispatch issue",
      status: "open",
      priority: null,
      assignee: null,
      components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
      route: null,
      open_asks: [],
      last_seq: 0,
      labels: [],
    });
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42") {
        issueRequested.resolve();
        return issue.promise;
      }
      if (target.pathname === "/api/v1/issues/DSP-42/references") {
        referencesRequested = true;
        return {
          ok: false,
          status: 404,
          statusText: "Not Found",
          headers: new Headers({ "Content-Type": "application/json" }),
          text: async () => {
            referenceResponseRead.resolve();
            return JSON.stringify({ error: "missing reference", code: "NOT_FOUND" });
          },
        } as unknown as Response;
      }
      if (target.pathname === "/api/v1/issues/DSP-42/events") {
        return new Response(JSON.stringify({ error: "event read failed", code: "EVENTS_FAILED" }), {
          status: 500,
          headers: { "Content-Type": "application/json" },
        });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const run = executeDispatchTool({
      tool: "dispatch_read",
      args: { issue: "DSP-42" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });
    const outcome = run.then(
      () => ({ error: null }),
      (error: unknown) => ({ error })
    );

    try {
      await issueRequested.promise;
      expect(referencesRequested).toBe(true);
      await referenceResponseRead.promise;
      issue.resolve(issueResponse);

      const { error } = await outcome;
      expect(error).toMatchObject({
        name: "DispatchServiceError",
        code: "EVENTS_FAILED",
        status: 500,
        message: "event read failed",
      });
    } finally {
      issue.resolve(issueResponse);
    }
  });

  test("reads recent events from a Dispatch log reference, each with the head of its text", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          title: "Dispatch issue",
          status: "open",
          route: null,
          open_asks: [],
          children: [],
          last_seq: 8,
        });
      }
      if (target.pathname === "/api/v1/issues/DSP-42/events") {
        return response([
          {
            seq: 2,
            type: "comment.created",
            actor: { kind: "user", id: "sami" },
            created_at: "2026-09-09T00:02:00Z",
            payload: { body: `Looks good.\n${"x".repeat(200)}` },
          },
          {
            seq: 3,
            type: "ask.opened",
            actor: { kind: "session", id: "s1" },
            created_at: "2026-09-09T00:03:00Z",
            payload: { question: releaseQuestion },
          },
          {
            seq: 4,
            type: "ask.answered",
            actor: { kind: "user", id: "sami" },
            created_at: "2026-09-09T00:04:00Z",
            payload: {
              question: releaseQuestion,
              answer: { selected: ["Keep the limits"], text: "No, trim the asks." },
            },
          },
          {
            seq: 5,
            type: "artifact.version",
            actor: { kind: "session", id: "s1" },
            created_at: "2026-09-09T00:05:00Z",
            payload: { name: "spec.md", version: { number: 3, summary: "Record D1" } },
          },
          {
            seq: 6,
            type: "issue.updated",
            actor: { kind: "user", id: "sami" },
            created_at: "2026-09-09T00:06:00Z",
            payload: { key: "DSP-42", status: "in_progress", title: "Dispatch issue" },
          },
          {
            seq: 7,
            type: "comment.anchor_refreshed",
            actor: { kind: "user", id: "sami" },
            created_at: "2026-09-09T00:07:00Z",
            payload: { body: "Anchor moved after the document edit." },
          },
          {
            seq: 8,
            type: "ask.anchor_refreshed",
            actor: { kind: "user", id: "sami" },
            created_at: "2026-09-09T00:08:00Z",
            payload: { question: reopenQuestion },
          },
        ]);
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/log" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result).toEqual({
      text: [
        "Key: DSP-42",
        "Events:",
        `- #2 comment.created · user sami · 2026-09-09T00:02:00Z · Looks good. ${"x".repeat(108)}…`,
        `- #3 ask.opened · session s1 · 2026-09-09T00:03:00Z · ${releaseQuestion}`,
        `- #4 ask.answered · user sami · 2026-09-09T00:04:00Z · ${releaseQuestion} -> Keep the limits - No, trim the asks.`,
        "- #5 artifact.version · session s1 · 2026-09-09T00:05:00Z · spec.md v3: Record D1",
        "- #6 issue.updated · user sami · 2026-09-09T00:06:00Z · status in_progress",
        "- #7 comment.anchor_refreshed · user sami · 2026-09-09T00:07:00Z · Anchor moved after the document edit.",
        `- #8 ask.anchor_refreshed · user sami · 2026-09-09T00:08:00Z · ${reopenQuestion}`,
      ].join("\n"),
      details: { issue: "DSP-42" },
    });
  });

  test("reads an issue from its dashboard URL on the configured server", async () => {
    const requested: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requested.push(target.pathname);
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          title: "Dispatch issue",
          status: "open",
          route: null,
          open_asks: [],
          children: [],
          last_seq: 0,
        });
      }
      if (target.pathname === "/api/v1/issues/DSP-42/events") return response([]);
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "http://dispatch.test/issues/DSP-42/log" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe("Key: DSP-42\nEvents:\n- none");
    expect(requested).toEqual(["/api/v1/issues/DSP-42", "/api/v1/issues/DSP-42/events"]);
  });

  test("maps the SPA's version selector on dashboard URLs to a named-version read", async () => {
    for (const [ref, artifactPath, versionsPath] of [
      [
        "http://dispatch.test/issues/DSP-42/artifacts/notes?v=3",
        "/api/v1/issues/DSP-42",
        "/api/v1/artifacts/artifact-43/versions/3",
      ],
      [
        "http://dispatch.test/issues/DSP-42/spec?v=2",
        "/api/v1/issues/DSP-42",
        "/api/v1/artifacts/artifact-42/versions/2",
      ],
      [
        "http://dispatch.test/projects/CORE/documents/runbook?version=5",
        "/api/v1/projects/CORE/artifacts/runbook",
        "/api/v1/artifacts/doc-1/versions/5",
      ],
    ] as const) {
      const requested: string[] = [];
      const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
        const path = new URL(String(url)).pathname;
        requested.push(path);
        if (path === "/api/v1/issues/DSP-42") {
          return response({
            key: "DSP-42",
            primary_artifact_id: "artifact-42",
            open_asks: [],
            artifacts: [
              { id: "artifact-42", slug: "spec", name: "spec.md", primary: true },
              { id: "artifact-43", slug: "notes", name: "notes.md", primary: false },
            ],
          });
        }
        if (path === "/api/v1/projects/CORE/artifacts/runbook") {
          return response({ id: "doc-1", project: "CORE", slug: "runbook", name: "runbook.md" });
        }
        if (path === versionsPath) return response({ markdown: "# v", version: { number: 1 } });
        return response([]);
      };
      const result = await executeDispatchTool({
        tool: "dispatch_doc_read",
        args: { ref },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      });
      expect(result.text).toContain("# v");
      expect(requested.slice(0, 2)).toEqual([artifactPath, versionsPath]);
    }
  });

  test("maps the SPA's conversation, children, and message pages to their dispatch refs", async () => {
    const message = "6f1d2c3b-4a5e-4f60-8b7c-9d0e1f2a3b4c";
    const requested: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const path = new URL(String(url)).pathname;
      requested.push(path);
      if (path === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          title: "Dispatch issue",
          status: "open",
          route: null,
          open_asks: [],
          children: [{ key: "DSP-43", title: "Child", status: "todo" }],
          last_seq: 0,
        });
      }
      if (path === `/api/v1/issues/DSP-42/messages/${message}`) {
        return response({
          message: {
            id: message,
            issue_key: "DSP-42",
            author: { kind: "user", id: "sami" },
            body: "Hi",
          },
          replies: [],
        });
      }
      if (path === "/api/v1/references") return response(emptyGraph("message"));
      return response([]);
    };
    const read = (ref: string) =>
      executeDispatchTool({
        tool: "dispatch_read",
        args: { ref },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      });

    expect((await read("http://dispatch.test/issues/DSP-42/conversation")).text).toBe(
      "Key: DSP-42\nEvents:\n- none"
    );
    expect((await read("http://dispatch.test/issues/DSP-42/children")).text).toContain(
      "- DSP-43: Child (todo)"
    );
    expect((await read(`http://dispatch.test/issues/DSP-42/messages/${message}`)).text).toContain(
      "Body: Hi"
    );
    expect(requested).toContain(`/api/v1/issues/DSP-42/messages/${message}`);
  });

  test("resolves an 8-character ask id prefix against the issue's asks", async () => {
    const full = "7430fab3-1c2d-4e5f-8a9b-0c1d2e3f4a5b";
    const requested: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requested.push(target.pathname);
      if (target.pathname === "/api/v1/issues/DSP-42/asks") {
        return response([
          { id: full, question: releaseQuestion },
          { id: "9999aaaa-1c2d-4e5f-8a9b-0c1d2e3f4a5b", question: "Other" },
        ]);
      }
      if (target.pathname === `/api/v1/asks/${full}`) {
        return response({
          ask: {
            id: full,
            question: releaseQuestion,
            options: [],
            state: "open",
            answer: null,
          },
          replies: [],
          edits: [],
        });
      }
      if (target.pathname === "/api/v1/references") return response(emptyGraph("ask"));
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/ask/7430fab3" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toContain(`Question: ${releaseQuestion}`);
    expect(requested).toEqual([
      "/api/v1/issues/DSP-42/asks",
      `/api/v1/asks/${full}`,
      "/api/v1/references",
      "/api/v1/references",
    ]);

    const ambiguous = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/ask/7430fab3" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: (async (url: RequestInfo | URL) => {
        expect(new URL(String(url)).pathname).toBe("/api/v1/issues/DSP-42/asks");
        return response([
          { id: full, question: releaseQuestion },
          { id: "7430fab3-ffff-4e5f-8a9b-0c1d2e3f4a5b", question: "Other" },
        ]);
      }) as typeof fetch,
    }).then(
      () => undefined,
      (error: unknown) => error
    );
    if (!(ambiguous instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(ambiguous.problems).toEqual([
      "ask id 7430fab3 matches 2 asks on DSP-42; use the full id",
    ]);
  });

  test("reads the children listing from a Dispatch children reference", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          title: "Dispatch issue",
          status: "open",
          route: null,
          open_asks: [],
          children: [{ key: "DSP-43", title: "A child issue", status: "todo" }],
          last_seq: 0,
        });
      }
      if (target.pathname === "/api/v1/issues/DSP-42/events") return response([]);
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/children" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result).toEqual({
      text: ["Key: DSP-42", "Children:", "- DSP-43: A child issue (todo)"].join("\n"),
      details: { issue: "DSP-42" },
    });
  });
});

test("resolves an ask as the calling session", async () => {
  const requests: Array<{ readonly pathname: string; readonly body: unknown }> = [];
  const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const pathname = new URL(String(url)).pathname;
    requests.push({ pathname, body: JSON.parse(String(init?.body)) });
    if (pathname !== "/api/v1/asks/a5c42000-0000-4000-8000-000000000042/resolve") {
      throw new Error(`unexpected request: ${pathname}`);
    }
    return response({
      id: "a5c42000-0000-4000-8000-000000000042",
      issue_key: "DSP-42",
      resolution: {
        actor: { kind: "session", id: "session-42" },
        at: "2026-09-10T00:00:00Z",
        kind: "retracted",
        reason: "A newer question supersedes this one.",
      },
      state: "resolved",
    });
  };

  const result = await executeDispatchTool({
    tool: "dispatch_resolve_ask",
    args: {
      ask: "a5c42000-0000-4000-8000-000000000042",
      kind: "retracted",
      reason: "A newer question supersedes this one.",
    },
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-42",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl: fetchImpl as typeof fetch,
  });

  expect(result).toEqual({
    text: "Retracted ask a5c42000-0000-4000-8000-000000000042: A newer question supersedes this one.",
    details: { issue: "DSP-42", ask: "a5c42000-0000-4000-8000-000000000042" },
  });
  expect(requests).toHaveLength(1);
  expect(requests[0]).toEqual({
    pathname: "/api/v1/asks/a5c42000-0000-4000-8000-000000000042/resolve",
    body: {
      actor: {
        kind: "session",
        id: "session-42",
        origin: expect.objectContaining({ host: "omp", cwd: "/workspace" }),
      },
      kind: "retracted",
      reason: "A newer question supersedes this one.",
    },
  });
});

test("resolving a document ask reports the document it lives on", async () => {
  const requests: string[] = [];
  const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
    const pathname = new URL(String(url)).pathname;
    requests.push(pathname);
    if (pathname === "/api/v1/asks/a5cd0c00-0000-4000-8000-0000000000d0/resolve") {
      return response({
        id: "a5cd0c00-0000-4000-8000-0000000000d0",
        issue_key: null,
        artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
        resolution: {
          actor: { kind: "session", id: "session-42" },
          at: "2026-09-11T04:00:00Z",
          kind: "retracted",
          reason: "No longer needed.",
        },
        state: "resolved",
      });
    }
    if (pathname === "/api/v1/artifacts/a4cf7999-cab2-4326-939d-cb1e76733cc3") {
      return response({
        id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
        issue_key: null,
        project: "CORE",
        slug: "design-notes",
      });
    }
    throw new Error(`unexpected request: ${pathname}`);
  };

  const result = await executeDispatchTool({
    tool: "dispatch_resolve_ask",
    args: {
      ask: "a5cd0c00-0000-4000-8000-0000000000d0",
      kind: "retracted",
      reason: "No longer needed.",
    },
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-42",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl: fetchImpl as typeof fetch,
  });

  expect(result).toEqual({
    text: "Retracted ask a5cd0c00-0000-4000-8000-0000000000d0: No longer needed.",
    details: {
      project: "CORE",
      artifact: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      document: "CORE/design-notes",
      ask: "a5cd0c00-0000-4000-8000-0000000000d0",
    },
  });
  expect(requests).toEqual([
    "/api/v1/asks/a5cd0c00-0000-4000-8000-0000000000d0/resolve",
    "/api/v1/artifacts/a4cf7999-cab2-4326-939d-cb1e76733cc3",
  ]);
});

test("resolves a review comment as the calling session from a dispatch:// comment reference", async () => {
  const commentUuid = "cccccccc-0000-4000-8000-000000000042";
  const requests: Array<{
    readonly method: string;
    readonly pathname: string;
    readonly body: unknown;
  }> = [];
  const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const pathname = new URL(String(url)).pathname;
    requests.push({
      method: init?.method ?? "GET",
      pathname,
      body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
    });
    if (pathname !== `/api/v1/comments/${commentUuid}/resolve`) {
      throw new Error(`unexpected request: ${pathname}`);
    }
    return response({
      id: commentUuid,
      issue_key: "DSP-42",
      artifact_id: null,
      author: { kind: "session", id: "session-42" },
      body: "Expand DSN on first use.",
      anchor: null,
      reply_to: null,
      ask_id: null,
      turn: null,
      resolved: true,
      resolved_by: { kind: "session", id: "session-42" },
      resolved_at: "2026-09-15T00:00:00Z",
      edited_at: null,
      suggestion: null,
      created_at: "2026-09-14T00:00:00Z",
    });
  };

  const result = await executeDispatchTool({
    tool: "dispatch_resolve_comment",
    args: { comment: `dispatch://DSP-42/comment/${commentUuid}` },
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-42",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl: fetchImpl as typeof fetch,
  });

  expect(result).toEqual({
    text: `Resolved comment ${commentUuid} on DSP-42.`,
    details: { issue: "DSP-42", comment: commentUuid },
  });
  expect(requests).toEqual([
    {
      method: "POST",
      pathname: `/api/v1/comments/${commentUuid}/resolve`,
      body: {
        actor: {
          kind: "session",
          id: "session-42",
          origin: expect.objectContaining({ host: "omp", cwd: "/workspace" }),
        },
      },
    },
  ]);
});

test("resolving a document comment by bare id reports the document it lives on", async () => {
  const commentUuid = "cccccccc-0000-4000-8000-000000000043";
  const requests: string[] = [];
  const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
    const pathname = new URL(String(url)).pathname;
    requests.push(pathname);
    if (pathname === `/api/v1/comments/${commentUuid}/resolve`) {
      return response({
        id: commentUuid,
        issue_key: null,
        artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
        author: { kind: "session", id: "session-42" },
        body: "Name the fallback.",
        anchor: null,
        reply_to: null,
        ask_id: null,
        turn: null,
        resolved: true,
        resolved_by: { kind: "session", id: "session-42" },
        resolved_at: "2026-09-15T00:00:00Z",
        edited_at: null,
        suggestion: null,
        created_at: "2026-09-14T00:00:00Z",
      });
    }
    if (pathname === "/api/v1/artifacts/a4cf7999-cab2-4326-939d-cb1e76733cc3") {
      return response({
        id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
        issue_key: null,
        project: "CORE",
        slug: "design-notes",
      });
    }
    throw new Error(`unexpected request: ${pathname}`);
  };

  const result = await executeDispatchTool({
    tool: "dispatch_resolve_comment",
    args: { comment: commentUuid },
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-42",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl: fetchImpl as typeof fetch,
  });

  expect(result).toEqual({
    text: `Resolved comment ${commentUuid} on CORE/design-notes.`,
    details: {
      project: "CORE",
      artifact: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      document: "CORE/design-notes",
      comment: commentUuid,
    },
  });
  expect(requests).toEqual([
    `/api/v1/comments/${commentUuid}/resolve`,
    "/api/v1/artifacts/a4cf7999-cab2-4326-939d-cb1e76733cc3",
  ]);
});

test("resolving a comment through a project-document reference resolves a short id against the document", async () => {
  const commentUuid = "cccccccc-0000-4000-8000-000000000044";
  const requests: string[] = [];
  const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
    const pathname = new URL(String(url)).pathname;
    requests.push(pathname);
    if (pathname === "/api/v1/projects/CORE/artifacts/design-notes") {
      return response({
        id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
        project: "CORE",
        slug: "design-notes",
      });
    }
    if (pathname === "/api/v1/artifacts/a4cf7999-cab2-4326-939d-cb1e76733cc3/comments") {
      return response([{ id: commentUuid }, { id: "dddddddd-0000-4000-8000-000000000001" }]);
    }
    if (pathname === `/api/v1/comments/${commentUuid}/resolve`) {
      return response({
        id: commentUuid,
        issue_key: null,
        artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
        author: { kind: "session", id: "session-42" },
        body: "Name the fallback.",
        anchor: null,
        reply_to: null,
        ask_id: null,
        turn: null,
        resolved: true,
        resolved_by: { kind: "session", id: "session-42" },
        resolved_at: "2026-09-15T00:00:00Z",
        edited_at: null,
        suggestion: null,
        created_at: "2026-09-14T00:00:00Z",
      });
    }
    throw new Error(`unexpected request: ${pathname}`);
  };

  const result = await executeDispatchTool({
    tool: "dispatch_resolve_comment",
    args: { comment: "dispatch://CORE/artifact/design-notes/comment/cccccccc" },
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-42",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl: fetchImpl as typeof fetch,
  });

  expect(result.details).toEqual({
    project: "CORE",
    artifact: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
    document: "CORE/design-notes",
    comment: commentUuid,
  });
  expect(requests).toContain(`/api/v1/comments/${commentUuid}/resolve`);
});

test("refuses to resolve a comment named by an ask reference", async () => {
  let requests = 0;
  const fetchImpl = (() => {
    requests += 1;
    throw new Error("network must not be called");
  }) as unknown as typeof fetch;

  await expect(
    executeDispatchTool({
      tool: "dispatch_resolve_comment",
      args: { comment: "dispatch://DSP-42/ask/5a660655-04ad-4ce0-8a9b-93dd03c412b7" },
      cwd: "/workspace",
      host: "omp",
      sessionId: "session-42",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl,
    })
  ).rejects.toThrow("comment must be a bare comment id or a dispatch://.../comment/<id> reference");
  expect(requests).toBe(0);
});

test("edits an open ask with the calling session identity", async () => {
  const requests: Array<{
    readonly method: string;
    readonly pathname: string;
    readonly body: unknown;
  }> = [];
  const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const pathname = new URL(String(url)).pathname;
    requests.push({
      method: init?.method ?? "GET",
      pathname,
      body: JSON.parse(String(init?.body)),
    });
    if (pathname !== "/api/v1/asks/a5c42000-0000-4000-8000-000000000042") {
      throw new Error(`unexpected request: ${pathname}`);
    }
    return response({
      id: "a5c42000-0000-4000-8000-000000000042",
      issue_key: "DSP-42",
      question: revisedPlanQuestion,
      state: "open",
    });
  };

  const result = await executeDispatchTool({
    tool: "dispatch_edit_ask",
    args: {
      ask: "dispatch://DSP-42/ask/a5c42000-0000-4000-8000-000000000042",
      question: revisedPlanQuestion,
      options: [
        { label: "Review the revised plan", description: "Keeps the review gate in place." },
      ],
      multiple: true,
      urgency: "high",
    },
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-42",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl: fetchImpl as typeof fetch,
  });

  expect(result).toEqual({
    text: `Ask edited: ${revisedPlanQuestion}`,
    details: { issue: "DSP-42", ask: "a5c42000-0000-4000-8000-000000000042" },
  });
  expect(requests).toEqual([
    {
      method: "PATCH",
      pathname: "/api/v1/asks/a5c42000-0000-4000-8000-000000000042",
      body: {
        question: revisedPlanQuestion,
        options: [
          { label: "Review the revised plan", description: "Keeps the review gate in place." },
        ],
        multiple: true,
        urgency: "high",
        actor: {
          kind: "session",
          id: "session-42",
          origin: expect.objectContaining({ host: "omp", cwd: "/workspace" }),
        },
      },
    },
  ]);
});

test("reports why an answered ask cannot be edited", async () => {
  const fetchImpl = async (_url: RequestInfo | URL, _init?: RequestInit): Promise<Response> =>
    new Response(JSON.stringify({ error: "only open asks may be edited", code: "ASK_NOT_OPEN" }), {
      status: 409,
      headers: { "Content-Type": "application/json" },
    });

  await expect(
    executeDispatchTool({
      tool: "dispatch_edit_ask",
      args: {
        ask: "a5c42000-0000-4000-8000-000000000042",
        question: revisedPlanQuestion,
      },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    })
  ).rejects.toThrow("only open asks may be edited");
});

test("rejects an empty ask edit before issuing a request", async () => {
  let requests = 0;
  const fetchImpl = (() => {
    requests += 1;
    throw new Error("network must not be called");
  }) as unknown as typeof fetch;

  await expect(
    executeDispatchTool({
      tool: "dispatch_edit_ask",
      args: { ask: "ask-42" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl,
    })
  ).rejects.toThrow("at least one field");
  expect(requests).toBe(0);
});

test("rejects a non-ask Dispatch reference before issuing a request", async () => {
  let requests = 0;
  const fetchImpl = (() => {
    requests += 1;
    throw new Error("network must not be called");
  }) as unknown as typeof fetch;

  await expect(
    executeDispatchTool({
      tool: "dispatch_edit_ask",
      args: {
        ask: "dispatch://DSP-42/comment/comment-42",
        question: revisedPlanQuestion,
      },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl,
    })
  ).rejects.toThrow("ask must be a bare ask id or a dispatch://.../ask/<id> reference");
  expect(requests).toBe(0);
});

test("editing a document ask reports the document it lives on without claiming a follow", async () => {
  const requests: string[] = [];
  const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
    const pathname = new URL(String(url)).pathname;
    requests.push(pathname);
    if (pathname === "/api/v1/asks/a5cd0c00-0000-4000-8000-0000000000d0") {
      return response({
        id: "a5cd0c00-0000-4000-8000-0000000000d0",
        issue_key: null,
        artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
        question: documentReviewQuestion,
        state: "open",
      });
    }
    if (pathname === "/api/v1/artifacts/a4cf7999-cab2-4326-939d-cb1e76733cc3") {
      return response({
        id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
        issue_key: null,
        project: "CORE",
        slug: "design-notes",
      });
    }
    throw new Error(`unexpected request: ${pathname}`);
  };

  const result = await executeDispatchTool({
    tool: "dispatch_edit_ask",
    args: {
      ask: "a5cd0c00-0000-4000-8000-0000000000d0",
      question: documentReviewQuestion,
    },
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-42",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl: fetchImpl as typeof fetch,
  });

  expect(result).toEqual({
    text: `Ask edited: ${documentReviewQuestion}`,
    details: {
      project: "CORE",
      artifact: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      document: "CORE/design-notes",
      ask: "a5cd0c00-0000-4000-8000-0000000000d0",
    },
  });
  expect(requests).toEqual([
    "/api/v1/asks/a5cd0c00-0000-4000-8000-0000000000d0",
    "/api/v1/artifacts/a4cf7999-cab2-4326-939d-cb1e76733cc3",
  ]);
});

const askUUID = "7430fab3-1c2d-4e5f-8a9b-0c1d2e3f4a5b";

/** GET /api/v1/asks/open for the calling session. */
function openAsksBody(asks: readonly { readonly id: string; readonly question: string }[]) {
  return {
    session_id: "session-42",
    as_of: "2026-09-20T00:00:00Z",
    opened_since: false,
    count: asks.length,
    waiting_on_human: asks.length,
    waiting_on_agent: 0,
    asks: asks.map((ask) => ({
      ...ask,
      ref: `/issues/DSP-42/asks/${ask.id}`,
      kind: "question",
      urgency: "normal",
      created_at: "2026-09-19T00:00:00Z",
      age_seconds: 3600,
      priority: 1,
      owner: { issue: { key: "DSP-42", title: "Dispatch issue" } },
      human_replied: false,
      last_reply: null,
      waiting_on: "human",
    })),
  };
}

/** The three tools whose only owner is the ask id the caller supplies. */
const askWriteTools = [
  {
    tool: "dispatch_edit_ask",
    args: (ask: string) => ({ ask, question: revisedPlanQuestion }),
    writes: [`PATCH /api/v1/asks/${askUUID}`],
  },
  {
    tool: "dispatch_resolve_ask",
    args: (ask: string) => ({ ask, kind: "retracted", reason: "Superseded." }),
    writes: [`POST /api/v1/asks/${askUUID}/resolve`],
  },
  {
    tool: "dispatch_follow",
    args: (ask: string) => ({ ask, action: "follow" }),
    writes: [`GET /api/v1/asks/${askUUID}`, `PUT /api/v1/asks/${askUUID}/followers/session-42`],
  },
] as const;

/** Answers every ask route those tools reach, recording `METHOD /path` in call order. */
function askWriteFetch(requests: string[], openAsks: () => Response): typeof fetch {
  return (async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const target = new URL(String(url));
    const method = init?.method ?? "GET";
    requests.push(`${method} ${target.pathname}`);
    if (target.pathname === "/api/v1/asks/open") return openAsks();
    if (target.pathname === `/api/v1/asks/${askUUID}/followers/session-42`) return response({});
    if (target.pathname.startsWith(`/api/v1/asks/${askUUID}`)) {
      const ask = {
        id: askUUID,
        issue_key: "DSP-42",
        question: revisedPlanQuestion,
        state: "open",
        resolution: {
          actor: { kind: "session", id: "session-42" },
          at: "2026-09-20T00:00:00Z",
          kind: "retracted",
          reason: "Superseded.",
        },
      };
      return response(method === "GET" ? { ask, replies: [], edits: [] } : ask);
    }
    throw new Error(`unexpected request: ${method} ${target.pathname}`);
  }) as typeof fetch;
}

function executeAskWrite(
  tool: string,
  args: Record<string, unknown>,
  fetchImpl: typeof fetch
): Promise<unknown> {
  return executeDispatchTool({
    tool,
    args,
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-42",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl,
  });
}

async function refusal(promise: Promise<unknown>): Promise<ToolInputError> {
  const thrown = await promise.then(
    () => undefined,
    (error: unknown) => error
  );
  if (!(thrown instanceof ToolInputError))
    throw new Error(`expected ToolInputError, got ${thrown}`);
  return thrown;
}

for (const { tool, args, writes } of askWriteTools) {
  test(`${tool} refuses a short ask id with the uuid rule and this session's open asks`, async () => {
    const requests: string[] = [];
    const fetchImpl = askWriteFetch(requests, () =>
      response(openAsksBody([{ id: askUUID, question: revisedPlanQuestion }]))
    );

    const error = await refusal(executeAskWrite(tool, args("42"), fetchImpl));

    expect(error.problems).toEqual([
      "ask ids are uuids (a prefix of at least 8 hex characters works); " +
        `your open asks: 7430fab3 — ${revisedPlanQuestion}`,
    ]);
    expect(requests).toEqual(["GET /api/v1/asks/open"]);
  });

  test(`${tool} refuses a short ask id on the rule alone when its open asks cannot be read`, async () => {
    const requests: string[] = [];
    const fetchImpl = askWriteFetch(
      requests,
      () =>
        new Response(JSON.stringify({ code: "SERVICE_UNAVAILABLE", error: "Dispatch is down" }), {
          status: 503,
          headers: { "Content-Type": "application/json" },
        })
    );

    const error = await refusal(executeAskWrite(tool, args("42"), fetchImpl));

    expect(error.problems).toEqual([
      "ask ids are uuids (a prefix of at least 8 hex characters works)",
    ]);
    expect(requests).toEqual(["GET /api/v1/asks/open"]);
  });

  test(`${tool} resolves an 8-character prefix of one open ask to its full uuid`, async () => {
    const requests: string[] = [];
    const fetchImpl = askWriteFetch(requests, () =>
      response(
        openAsksBody([
          { id: askUUID, question: revisedPlanQuestion },
          { id: "9999aaaa-1c2d-4e5f-8a9b-0c1d2e3f4a5b", question: "A different question" },
        ])
      )
    );

    await executeAskWrite(tool, args("7430fab3"), fetchImpl);

    expect(requests).toEqual(["GET /api/v1/asks/open", ...writes]);
  });

  test(`${tool} sends a full uuid without reading the open-ask list`, async () => {
    const requests: string[] = [];
    const fetchImpl = askWriteFetch(requests, () => {
      throw new Error("a full uuid must not read the open-ask list");
    });

    await executeAskWrite(tool, args(askUUID), fetchImpl);

    expect(requests).toEqual([...writes]);
  });
}

// resolveIssueDocumentId takes the reference the Dispatch tools take for an issue's document: `spec`
// for its primary document, its id, its slug, or its filename; one that names no document throws.
test("an issue document reference resolves to the document's id as the Dispatch tools resolve it", async () => {
  const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
    const target = new URL(String(url));
    if (target.pathname !== "/api/v1/issues/DSP-42")
      throw new Error(`unexpected request: ${target.pathname}`);
    return response({
      key: "DSP-42",
      primary_artifact_id: "artifact-42",
      artifacts: [
        { id: "artifact-42", slug: "spec-md", name: "spec.md", primary: true },
        { id: "artifact-43", slug: "notes-md", name: "notes.md", primary: false },
      ],
    });
  };
  const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl as typeof fetch);
  for (const [reference, id] of [
    ["spec", "artifact-42"],
    ["spec-md", "artifact-42"],
    ["spec.md", "artifact-42"],
    ["artifact-43", "artifact-43"],
    ["notes-md", "artifact-43"],
    ["notes.md", "artifact-43"],
  ] as const) {
    expect(await resolveIssueDocumentId(client, "DSP-42", reference)).toBe(id);
  }
  await expect(resolveIssueDocumentId(client, "DSP-42", "plan.md")).rejects.toThrow(/plan\.md/);
});

// Dispatch suffixes a slug two documents would share, so on one issue a document's filename can be
// another's slug: `spec v2` has slug `spec-v2`, and a document named `spec-v2` gets `spec-v2-2`.
// Such a reference names two documents, so it is refused rather than taken by slug, and the hint
// names each document's id. A reference only one document answers to still resolves.
test("a reference that is one document's slug and another's filename is refused as ambiguous", async () => {
  const fetchImpl = async (_url: RequestInfo | URL): Promise<Response> =>
    response({
      key: "DSP-42",
      primary_artifact_id: "artifact-spec",
      artifacts: [
        { id: "artifact-spec", slug: "spec", name: "spec.md", primary: true },
        { id: "artifact-notes", slug: "notes-md", name: "notes.md", primary: false },
        { id: "artifact-v2", slug: "spec-v2", name: "spec v2", kind: "doc", primary: false },
        { id: "artifact-v2-2", slug: "spec-v2-2", name: "spec-v2", kind: "doc", primary: false },
      ],
    });
  const client = new DispatchClient("http://dispatch.test", "secret", fetchImpl as typeof fetch);
  await expect(resolveIssueDocumentId(client, "DSP-42", "spec-v2")).rejects.toThrow(
    '"spec-v2" names 2 documents on this issue; use the id: artifact-v2 (spec-v2, spec v2), artifact-v2-2 (spec-v2-2, spec-v2)'
  );
  for (const [reference, id] of [
    ["spec v2", "artifact-v2"],
    ["spec-v2-2", "artifact-v2-2"],
    ["artifact-v2-2", "artifact-v2-2"],
    ["spec", "artifact-spec"],
  ] as const) {
    expect(await resolveIssueDocumentId(client, "DSP-42", reference)).toBe(id);
  }
});

const anchoredRow: TablePosition = {
  id: "t-1",
  row: 5,
  column: 2,
  header: "Due",
  cells: ["5", "Red-teamer loop", "Today, Oct 1", "Task delivery owns the full loop. Stagin…"],
};

const anchoredCell: BlockPath = {
  id: "p-5-2",
  type: "paragraph",
  path: [
    { type: "table", id: "t-1", index: 3 },
    { type: "table_row", id: "r-5", index: 5 },
    { type: "table_cell", id: "c-5-2", index: 2 },
    { type: "paragraph", id: "p-5-2", index: 0 },
  ],
  table: anchoredRow,
};

describe("positionText", () => {
  test("names a cell by its row, the cells before it, and its column header", () => {
    expect(positionText(anchoredCell)).toBe("table[3] › row 5 (Red-teamer loop), column Due");
  });

  test("a first-column cell has no label, and a column the header row lacks reads by index", () => {
    expect(
      positionText({ ...anchoredCell, table: { ...anchoredRow, column: 0, header: "#" } })
    ).toBe("table[3] › row 5, column #");
    expect(positionText({ ...anchoredCell, table: { ...anchoredRow, header: "" } })).toBe(
      "table[3] › row 5 (Red-teamer loop), column 2"
    );
  });

  test("a row block reads by every cell, a table block by its place alone", () => {
    expect(
      positionText({
        id: "r-5",
        type: "table_row",
        path: anchoredCell.path.slice(0, 2),
        table: { ...anchoredRow, column: null, header: null },
      })
    ).toBe(
      "table[3] › row 5 (Red-teamer loop · Today, Oct 1 · Task delivery owns the full loop. Stagin…)"
    );
    expect(
      positionText({
        id: "t-1",
        type: "table",
        path: anchoredCell.path.slice(0, 1),
        table: { id: "t-1", row: null, column: null, header: null, cells: null },
      })
    ).toBe("table[3]");
  });

  test("a block outside a table reads as its path of types and child indexes", () => {
    expect(
      positionText({
        id: "p",
        type: "paragraph",
        path: [
          { type: "bullet_list", id: "l", index: 7 },
          { type: "list_item", id: "i", index: 0 },
          { type: "paragraph", id: "p", index: 0 },
        ],
      })
    ).toBe("bullet_list[7] › list_item[0] › paragraph[0]");
  });
});
