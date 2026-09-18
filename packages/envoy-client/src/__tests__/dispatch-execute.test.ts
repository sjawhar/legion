import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { dispatchToolSpecs, zodSchemaApi } from "@legion/contracts";
import { z } from "zod";
import type { ExecFn } from "../dispatch-cwd";
import { executeDispatchTool } from "../dispatch-execute";
import { dispatchFollowNotice } from "../dispatch-subscribe";
import { ToolInputError } from "../tool-input-errors";

function response(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    headers: { "Content-Type": "application/json" },
  });
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
        question: "Ship it?",
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
      args: { question: "Ship it?" },
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
      question: "Ship it?",
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
      "Asked ask-1 on DSP-41 (urgency med): Ship it?\n" +
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
      { issue: "DSP-41", question: "Ship this change?", ref },
      fetchImpl as typeof fetch
    );

    expect(requests).toEqual([
      expect.objectContaining({ question: `Ship this change?\n\nRef: ${ref}` }),
    ]);
    expect(result.text).toStartWith(
      `Asked ask-1 on DSP-41 (urgency med): Ship this change?\n\nRef: ${ref}\n`
    );
  });

  test("names the configured Dispatch URL when its transport is unreachable", async () => {
    const fetchImpl = (() => {
      throw new TypeError("Unable to connect. Is the computer able to access the url?");
    }) as unknown as typeof fetch;

    await expect(
      executeAsk({ issue: "DSP-41", question: "Should this ship?" }, fetchImpl)
    ).rejects.toThrow(
      "If the Dispatch URL changed, restart this agent process so it picks up the new configuration."
    );
  });

  test("does not duplicate an ask ref already in the question", async () => {
    const requests: unknown[] = [];
    const ref = "dispatch://DSP-41/message/message-1";
    const question = `Ship this change?\n\nRef: ${ref}`;
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
          question: "Ship this change?",
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

    const failure = await executeDispatchTool({
      tool: "dispatch_message",
      args: { message: "x", in_reply_to: "nope" },
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
    expect(failure).toBeInstanceOf(ToolInputError);
    if (!(failure instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(failure.problems).toEqual([
      "issue is required; supply issue or set LEGION_ISSUE",
      "body is required (string)",
      'unknown field "message"; allowed: issue, body, in_reply_to',
      "in_reply_to must be a full message id (uuid) or a dispatch://KEY/message/<id> reference",
    ]);
    expect(failure.message).toBe(
      [
        "dispatch_message was not called: 4 problems",
        "- issue is required; supply issue or set LEGION_ISSUE",
        "- body is required (string)",
        '- unknown field "message"; allowed: issue, body, in_reply_to',
        "- in_reply_to must be a full message id (uuid) or a dispatch://KEY/message/<id> reference",
        "- Allowed keys: issue, body, in_reply_to",
        '- Example: dispatch_message({"issue":"DSP-1","body":"Implementation started."})',
      ].join("\n")
    );
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
            { id: "01234567-0000-4000-8000-000000000001", question: "Should we ship first?" },
            { id: "89abcdef-0000-4000-8000-000000000002", question: "Should we ship second?" },
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
      "ask IDs are UUIDs; use the full ask ID; this issue's open asks: 01234567… Should we ship first?, 89abcdef… Should we ship second?",
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
            question: "Should we ship?",
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
        "- 1m 5s · P0 · LEGION-1: Reminder · Should we ship? · http://dispatch.test/issues/LEGION-1?ask=ask-1",
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

  test("dispatch_issues passes each optional filter through, omits absent ones, and returns the documented row shape", async () => {
    const requests: URL[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target);
      return response([
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
        },
      ]);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_issues",
      args: {
        project: "AGENTC",
        status: "todo",
        parent: "AGENTC-9",
        label: "bug",
        updated_since: "2026-09-01T00:00:00Z",
      },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(requests).toHaveLength(1);
    expect(requests[0]?.pathname).toBe("/api/v1/issues");
    expect(Object.fromEntries(requests[0]?.searchParams ?? [])).toEqual({
      project: "AGENTC",
      status: "todo",
      parent: "AGENTC-9",
      label: "bug",
      updated_since: "2026-09-01T00:00:00Z",
    });
    expect(result.details).toEqual({
      issues: [
        {
          key: "AGENTC-1",
          title: "First",
          status: "todo",
          priority: 1,
          parent: null,
          labels: ["bug"],
          open_asks: 2,
          updated_at: "2026-09-13T00:00:00Z",
        },
      ],
    });
  });

  test("dispatch_issues omits absent optional filters and clamps the row count to limit", async () => {
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
      const target = new URL(String(url));
      requests.push(target);
      return response(issues);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_issues",
      args: { project: "AGENTC", limit: 2 },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(Object.fromEntries(requests[0]?.searchParams ?? [])).toEqual({ project: "AGENTC" });
    expect(result.details.issues).toHaveLength(2);
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
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      requests.push({ body: JSON.parse(String(init?.body)) });
      return response({ key: "LEGION-13", title: "New global search work" });
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

  test("dispatch_issue forwards initial labels", async () => {
    const requests: Array<{ readonly body: unknown }> = [];
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      requests.push({ body: JSON.parse(String(init?.body)) });
      return response({ key: "LEGION-13", title: "New global search work" });
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
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      requests.push({ body: JSON.parse(String(init?.body)) });
      return response({ key: "LEGION-13", title: "Priority work" });
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
    const fetchImpl = async (_url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      requests.push({ body: JSON.parse(String(init?.body)) });
      return response({ key: "LEGION-14", title: "Assigned work" });
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
      return response({ kind: "agent", owner: "alice" });
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
    expect(result.details).toEqual({ session: "session-1", owner: "alice" });
    expect(result.text).toContain("alice");
  });

  test("dispatch_whoami reports a null owner under the shared token", async () => {
    const fetchImpl = async (): Promise<Response> => response({ kind: "agent", owner: null });

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

    expect(result.details).toEqual({ session: "session-1", owner: null });
    expect(result.text).toContain("shared token");
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
        args: { issue: "AGENTC-175", status: "done" },
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

  test("dispatch_request_approval opens the approval ask for the issue spec and reports its version", async () => {
    const posts: Array<{ path: string; body: unknown }> = [];
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
      if (target.pathname === "/api/v1/artifacts/artifact-42/approval-requests") {
        posts.push({ path: target.pathname, body: JSON.parse(String(init?.body)) });
        return response({
          ask: { id: "ask-9", issue_key: "DSP-42", artifact_id: null, kind: "approval" },
          artifact_id: "artifact-42",
          version: 3,
        });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_request_approval",
      args: { issue: "DSP-42" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(posts).toHaveLength(1);
    expect((posts[0]?.body as { actor: { kind: string } }).actor.kind).toBe("session");
    // The architect copies the document id and version from this text into register_gate, so
    // both must be stated — the id in particular, since the slug it typed is not the id.
    expect(result.text).toContain("spec.md (document id artifact-42) at version 3");
    expect(result.text).toContain("ask ask-9");
    expect(result.details).toMatchObject({ issue: "DSP-42", ask: "ask-9", version: 3 });
    expect(result.details).toMatchObject({ follows: { ask: "ask-9" } });
    expect(result.details).not.toHaveProperty("topic");
  });

  test("dispatch_request_approval on a document approved at its current version opens nothing", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          primary_artifact_id: "artifact-42",
          artifacts: [{ id: "artifact-42", slug: "spec", name: "spec.md", primary: true }],
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
      args: { issue: "DSP-42" },
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
      "# Spec\n\nApproval: approved v2 by sjawhar, edited since (now v4) - request approval again"
    );
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

  test("renders a no-new-version document edit and forwards its summary", async () => {
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
        return response({ applied: 2, version: null });
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
      text: "Applied 2 ops (no new version) (not subscribed to DSP-42; envoy_subscribe notifications.dispatch.issue.DSP-42.> for every event on it)",
      details: { issue: "DSP-42", applied: 2 },
    });
    expect(JSON.parse(requests[1]?.init.body as string)).toMatchObject({
      ops: [{ op: "replace", find: "draft", with: "final" }],
      summary: "Record final wording",
    });
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
      { project: "CORE", artifact: "runbook-md", question: "Publish?" },
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
      { ref: "dispatch://CORE/artifact/runbook-md", question: "Publish?" },
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
        return response({ artifact, version: { number: 1 } });
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
              question: "Publish?",
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
      ["dispatch_ask", { project: "CORE", artifact: artifactReference, question: "Publish?" }],
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

  test("creates an unlinked external issue once, then resolves it for later tool calls", async () => {
    const requests: Array<{ url: string; init: RequestInit }> = [];
    let resolves = 0;
    const fetchImpl = async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const request = { url: String(url), init: init ?? {} };
      requests.push(request);
      const target = new URL(request.url);
      if (target.pathname === "/api/v1/issues/resolve") {
        resolves += 1;
        return resolves === 1
          ? new Response(JSON.stringify({ code: "NOT_FOUND", error: "issue not found" }), {
              status: 404,
              headers: { "Content-Type": "application/json" },
            })
          : response({ key: "DSP-42" });
      }
      if (target.pathname === "/api/v1/issues") return response({ key: "DSP-42" });
      if (target.pathname === "/api/v1/issues/DSP-42/messages") {
        return response({ id: `message-${resolves}`, issue_key: "DSP-42" });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };
    const input = {
      tool: "dispatch_message",
      args: { issue: "owner/repo#42", body: "Proceed" },
      cwd: "/workspace",
      host: "omp" as const,
      sessionId: "session-42",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    };

    await executeDispatchTool(input);
    await executeDispatchTool(input);

    expect(
      requests.map((request) => new URL(request.url).pathname + new URL(request.url).search)
    ).toEqual([
      "/api/v1/issues/resolve?ref=owner%2Frepo%2342",
      "/api/v1/issues",
      "/api/v1/issues/DSP-42/messages",
      "/api/v1/issues/resolve?ref=owner%2Frepo%2342",
      "/api/v1/issues/DSP-42/messages",
    ]);
    expect(JSON.parse(requests[1]?.init.body as string)).toMatchObject({
      external: "owner/repo#42",
      actor: { kind: "session", id: "session-42" },
    });
  });

  test("names the unmapped external repository and both project env vars", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const path = new URL(String(url)).pathname;
      if (path === "/api/v1/issues/resolve") {
        return new Response(JSON.stringify({ code: "NOT_FOUND", error: "issue not found" }), {
          status: 404,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (path === "/api/v1/issues") {
        return new Response(
          JSON.stringify({
            code: "PROJECT_UNMAPPED",
            error:
              "repository is not mapped in repository settings and DISPATCH_DEFAULT_PROJECT is not configured",
          }),
          { status: 400, headers: { "Content-Type": "application/json" } }
        );
      }
      throw new Error(`unexpected request: ${path}`);
    };

    await expect(
      executeDispatchTool({
        tool: "dispatch_message",
        args: { issue: "owner/repo#42", body: "Proceed" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).rejects.toThrow(
      "repository owner/repo is not mapped in repository settings and no DISPATCH_DEFAULT_PROJECT is configured"
    );
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
            question: "Which API should we ship?",
            options: [{ label: "JSON", description: "Use the HTTP API." }, { label: "MCP" }],
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
        "Question: Which API should we ship?",
        "Options:",
        "- JSON — Use the HTTP API.",
        "- MCP",
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
          ask: { id: askUuid, issue_key: "DSP-42", question: "Ship it?" },
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
  test("reading an issue summary does not subscribe the session to the issue", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      if (target.pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          title: "Dispatch issue",
          status: "open",
          route: null,
          open_asks: [],
          last_seq: 0,
          labels: ["frontend", "urgent"],
          priority: 1,
          assignee: "alice",
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
    expect(result.text).toContain(
      "Labels: frontend, urgent\nComponents: web (inherited from DSP-40) (retired: legacy-ui)\nRoute: none"
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
          components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
          route: null,
          open_asks: [],
          last_seq: 0,
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
    expect(result.text).toContain("Components: unassigned");
    expect(result.text).toContain("References:\n- unavailable");
    expect(result.text).toContain("Referenced by:\n- unavailable\nLinks:\n- unavailable");
    expect(result.details).toEqual({ issue: "DSP-42" });
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
            payload: { question: "Should we ship?" },
          },
          {
            seq: 4,
            type: "ask.answered",
            actor: { kind: "user", id: "sami" },
            created_at: "2026-09-09T00:04:00Z",
            payload: {
              question: "Should we ship?",
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
            payload: { question: "Should we reopen this?" },
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
        "- #3 ask.opened · session s1 · 2026-09-09T00:03:00Z · Should we ship?",
        "- #4 ask.answered · user sami · 2026-09-09T00:04:00Z · Should we ship? -> Keep the limits - No, trim the asks.",
        "- #5 artifact.version · session s1 · 2026-09-09T00:05:00Z · spec.md v3: Record D1",
        "- #6 issue.updated · user sami · 2026-09-09T00:06:00Z · status in_progress",
        "- #7 comment.anchor_refreshed · user sami · 2026-09-09T00:07:00Z · Anchor moved after the document edit.",
        "- #8 ask.anchor_refreshed · user sami · 2026-09-09T00:08:00Z · Should we reopen this?",
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
          { id: full, question: "Ship it?" },
          { id: "9999aaaa-1c2d-4e5f-8a9b-0c1d2e3f4a5b", question: "Other" },
        ]);
      }
      if (target.pathname === `/api/v1/asks/${full}`) {
        return response({
          ask: {
            id: full,
            question: "Ship it?",
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

    expect(result.text).toContain("Question: Ship it?");
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
          { id: full, question: "Ship it?" },
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
    if (pathname !== "/api/v1/asks/ask-42/resolve") {
      throw new Error(`unexpected request: ${pathname}`);
    }
    return response({
      id: "ask-42",
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
      ask: "ask-42",
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
    text: "Retracted ask ask-42: A newer question supersedes this one.",
    details: { issue: "DSP-42", ask: "ask-42" },
  });
  expect(requests).toHaveLength(1);
  expect(requests[0]).toEqual({
    pathname: "/api/v1/asks/ask-42/resolve",
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
    if (pathname === "/api/v1/asks/ask-document/resolve") {
      return response({
        id: "ask-document",
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
      ask: "ask-document",
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
    text: "Retracted ask ask-document: No longer needed.",
    details: {
      project: "CORE",
      artifact: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      document: "CORE/design-notes",
      ask: "ask-document",
    },
  });
  expect(requests).toEqual([
    "/api/v1/asks/ask-document/resolve",
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
    if (pathname !== "/api/v1/asks/ask-42") {
      throw new Error(`unexpected request: ${pathname}`);
    }
    return response({
      id: "ask-42",
      issue_key: "DSP-42",
      question: "Ship the revised plan?",
      state: "open",
    });
  };

  const result = await executeDispatchTool({
    tool: "dispatch_edit_ask",
    args: {
      ask: "dispatch://DSP-42/ask/ask-42",
      question: "Ship the revised plan?",
      options: [{ label: "Ship", description: "Approve the revision." }],
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
    text: "Ask edited: Ship the revised plan?",
    details: { issue: "DSP-42", ask: "ask-42" },
  });
  expect(requests).toEqual([
    {
      method: "PATCH",
      pathname: "/api/v1/asks/ask-42",
      body: {
        question: "Ship the revised plan?",
        options: [{ label: "Ship", description: "Approve the revision." }],
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
      args: { ask: "ask-42", question: "Ship the revised plan?" },
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
        question: "Ship the revised plan?",
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
    if (pathname === "/api/v1/asks/ask-document") {
      return response({
        id: "ask-document",
        issue_key: null,
        artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
        question: "Approve the document?",
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
    args: { ask: "ask-document", question: "Approve the document?" },
    cwd: "/workspace",
    host: "omp",
    sessionId: "session-42",
    config,
    env: {},
    exec: repoExec("owner/repo"),
    fetchImpl: fetchImpl as typeof fetch,
  });

  expect(result).toEqual({
    text: "Ask edited: Approve the document?",
    details: {
      project: "CORE",
      artifact: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      document: "CORE/design-notes",
      ask: "ask-document",
    },
  });
  expect(requests).toEqual([
    "/api/v1/asks/ask-document",
    "/api/v1/artifacts/a4cf7999-cab2-4326-939d-cb1e76733cc3",
  ]);
});
