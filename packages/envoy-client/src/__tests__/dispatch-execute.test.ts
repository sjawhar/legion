import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { dispatchDocumentSubject, dispatchIssueSubject } from "@legion/contracts";
import type { ExecFn } from "../dispatch-cwd";
import { executeDispatchTool } from "../dispatch-execute";
import { dispatchSubscriptionTopic } from "../dispatch-subscribe";

function response(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    headers: { "Content-Type": "application/json" },
  });
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
      topic: dispatchIssueSubject("DSP-41", ">"),
      ask: "ask-1",
    });
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

  test("posts a message reply_to as a bare id or a dispatch://.../message/<id> reference, and cites the result", async () => {
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
      args: { issue: "DSP-42", body: "Sounds good", reply_to: "message-1" },
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
        reply_to: "dispatch://DSP-42/message/message-1",
      },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(bodies).toMatchObject([
      { body: "Sounds good", reply_to: "message-1" },
      { body: "Sounds good", reply_to: "message-1" },
    ]);
    expect(bareIDResult.text).toBe(
      "Posted message message-2 (dispatch://DSP-42/message/message-2)"
    );
    expect(refResult.text).toBe("Posted message message-2 (dispatch://DSP-42/message/message-2)");
  });

  test("rejects a message reply_to referencing a non-message dispatch reference", async () => {
    const fetchImpl = (() => {
      throw new Error("network must not be called");
    }) as unknown as typeof fetch;

    await expect(
      executeDispatchTool({
        tool: "dispatch_message",
        args: {
          issue: "DSP-42",
          body: "Sounds good",
          reply_to: "dispatch://DSP-42/comment/comment-1",
        },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl,
      })
    ).rejects.toThrow(
      /reply_to must be a bare message id or a dispatch:\/\/\.\.\.\/message\/<id> reference/
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

  test("dispatch_search needs no issue and renders results with absolute links", async () => {
    const results = [
      {
        kind: "document",
        issue: { key: "LEGION-2", title: "Astrolabe", status: "triage" },
        artifact: { slug: "spec", name: "spec.md" },
        id: "artifact-2",
        snippet: "…the <mark>astrolabe</mark> measures…",
        rank: 1,
        href: "/issues/LEGION-2/spec?q=astrolabe",
      },
      {
        kind: "comment",
        issue: { key: "LEGION-2", title: "Astrolabe", status: "triage" },
        id: "comment-2",
        snippet: "Comment about <mark>astrolabe</mark>",
        rank: 0.5,
        href: "/issues/LEGION-2/spec#comment-2",
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
        '2 results for "astrolabe" (12 ms)',
        "LEGION-2 [triage] Astrolabe - document spec.md: …the **astrolabe** measures… -> http://dispatch.test/issues/LEGION-2/spec?q=astrolabe",
        "LEGION-2 [triage] Astrolabe - comment: Comment about **astrolabe** -> http://dispatch.test/issues/LEGION-2/spec#comment-2",
      ].join("\n")
    );
    expect(result.details).toEqual({ query: "astrolabe", results });
    expect(dispatchSubscriptionTopic(result.details)).toBeNull();
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

    expect(result.text).toBe("Created LEGION-13: New global search work");
    expect(requests).toEqual([{ body: expect.objectContaining({ force: true }) }]);
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
    expect(dispatchSubscriptionTopic(result.details)).toBeNull();
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
        "dispatch://PROJECT/artifact/<slug>"
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
    expect(result.text).toContain("spec.md at version 3");
    expect(result.text).toContain("ask ask-9");
    expect(result.details).toMatchObject({ issue: "DSP-42", ask: "ask-9", version: 3 });
    expect(result.details?.topic).toBe("notifications.dispatch.issue.DSP-42.>");
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

    expect(result.text).toContain("already approved at version 3 by sjawhar");
    expect(result.text).not.toContain("ask ");
    expect(result.details).toMatchObject({ issue: "DSP-42", artifact: "artifact-42", version: 3 });
    expect(result.details?.topic).toBeUndefined();
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
    ).rejects.toThrow(/ambiguous/);
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
      text: "Applied 2 ops (no new version)",
      details: {
        issue: "DSP-42",
        topic: dispatchIssueSubject("DSP-42", ">"),
        applied: 2,
      },
    });
    expect(JSON.parse(requests[1]?.init.body as string)).toMatchObject({
      ops: [{ op: "replace", find: "draft", with: "final" }],
      summary: "Record final wording",
    });
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
      expect(result.details.topic).toBe(dispatchDocumentSubject("CORE", "runbook-md", ">"));
      expect(result.details).toMatchObject({
        project: "CORE",
        document: "CORE/runbook-md",
      });
    } else {
      expect(dispatchSubscriptionTopic(result.details)).toBeNull();
    }
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
        "(artifact slug garrett-reply-draft-md; dispatch://DSP-42/artifact/garrett-reply-draft-md)"
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
      if (target.pathname === "/api/v1/asks/ask-42") {
        return response({
          ask: {
            id: "ask-42",
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
              ask_id: "ask-42",
              resolved: false,
              suggestion: null,
              created_at: "2026-09-08T23:59:00Z",
            },
          ],
        });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/ask/ask-42" },
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
      ].join("\n"),
      details: { issue: "DSP-42" },
    });
    expect(dispatchSubscriptionTopic(result.details)).toBeNull();
    expect(requests).toEqual(["/api/v1/asks/ask-42"]);
  });

  test("posts a comment reply to an ask using reply_to_ask", async () => {
    const requests: Array<{ pathname: string; body: unknown }> = [];
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
        ask_id: "ask-42",
        resolved: false,
        suggestion: null,
        created_at: "2026-09-09T00:00:00Z",
      });
    };

    const result = await executeDispatchTool({
      tool: "dispatch_comment",
      args: { issue: "DSP-42", body: "I'd go with JSON.", reply_to_ask: "ask-42" },
      cwd: "/workspace",
      host: "omp",
      config,
      env: {},
      exec: repoExec("owner/repo"),
      fetchImpl: fetchImpl as typeof fetch,
    });

    expect(result.text).toBe("Posted comment comment-1");
    expect(requests).toEqual([
      {
        pathname: "/api/v1/issues/DSP-42/comments",
        body: expect.objectContaining({ ask_id: "ask-42", body: "I'd go with JSON." }),
      },
    ]);
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
      if (target.pathname === "/api/v1/comments/comment-42") {
        return response({
          comment: {
            id: "comment-42",
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
              reply_to: "comment-42",
              resolved: false,
              suggestion: null,
              created_at: "2026-09-09T00:01:00Z",
            },
          ],
        });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    const result = await executeDispatchTool({
      tool: "dispatch_read",
      args: { ref: "dispatch://DSP-42/comment/comment-42" },
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
        "comment-42 · session reviewer-1",
        "> Initial wording",
        "Body: Please revise this.",
        "Reply chain:",
        "comment-43 · user sami",
        "> Revised wording",
        "Body: Revised.",
      ].join("\n"),
      details: { issue: "DSP-42" },
    });
    expect(dispatchSubscriptionTopic(result.details)).toBeNull();
    expect(requests).toEqual(["/api/v1/comments/comment-42"]);
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
            reply_to: null,
            created_at: "2026-09-09T00:00:00Z",
          },
          replies: [
            {
              id: "message-43",
              issue_key: "DSP-42",
              author: { kind: "user", id: "sami" },
              body: "Sounds good.",
              reply_to: "message-42",
              created_at: "2026-09-09T00:01:00Z",
            },
          ],
        });
      }
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
      ].join("\n"),
      details: { issue: "DSP-42" },
    });
    expect(dispatchSubscriptionTopic(result.details)).toBeNull();
    expect(requests).toEqual(["/api/v1/issues/DSP-42/messages/message-42"]);
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
    expect(dispatchSubscriptionTopic(result.details)).toBeNull();
    expect(result.text).toContain("References:\n- CORE/runbook-md · depth 1 via comment comment-1");
  });

  test("keeps an issue summary readable when its references are unavailable", async () => {
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const pathname = new URL(String(url)).pathname;
      if (pathname === "/api/v1/issues/DSP-42") {
        return response({
          key: "DSP-42",
          title: "Dispatch issue",
          status: "open",
          route: null,
          open_asks: [],
          last_seq: 0,
        });
      }
      if (pathname === "/api/v1/issues/DSP-42/events") return response([]);
      if (pathname === "/api/v1/issues/DSP-42/references") {
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
    expect(result.text).toContain("References:\n- unavailable");
    expect(result.details).toEqual({ issue: "DSP-42" });
  });

  test("reads recent events from a Dispatch log reference", async () => {
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
            type: "comment.created",
            actor: { kind: "user", id: "sami" },
            created_at: "2026-09-09T00:02:00Z",
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
        "- #3 comment.created · user sami · 2026-09-09T00:02:00Z",
      ].join("\n"),
      details: { issue: "DSP-42" },
    });
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
    details: {
      issue: "DSP-42",
      topic: dispatchIssueSubject("DSP-42", ">"),
      ask: "ask-42",
    },
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

test("subscribes to the project document topic after resolving a document ask", async () => {
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
      topic: "notifications.dispatch.document.CORE.design-notes.>",
      ask: "ask-document",
    },
  });
  expect(requests).toEqual([
    "/api/v1/asks/ask-document/resolve",
    "/api/v1/artifacts/a4cf7999-cab2-4326-939d-cb1e76733cc3",
  ]);
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
    details: {
      issue: "DSP-42",
      topic: dispatchIssueSubject("DSP-42", ">"),
      ask: "ask-42",
    },
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

test("subscribes to the document topic after editing a document ask", async () => {
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
      topic: "notifications.dispatch.document.CORE.design-notes.>",
      ask: "ask-document",
    },
  });
  expect(requests).toEqual([
    "/api/v1/asks/ask-document",
    "/api/v1/artifacts/a4cf7999-cab2-4326-939d-cb1e76733cc3",
  ]);
});
