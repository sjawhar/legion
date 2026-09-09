import { describe, expect, test } from "bun:test";
import type { ExecFn } from "../dispatch-cwd";
import { executeDispatchTool } from "../dispatch-execute";

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
        custom: true,
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
      topic: "notifications.dispatch.issue.DSP-41.>",
      ask: "ask-1",
    });
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

    await expect(
      executeDispatchTool({
        tool: "dispatch_doc_read",
        args: { ref: "dispatch://DSP-42/artifact/spec@v3" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).resolves.toEqual({
      text: "Version three\n\nOpen anchored asks/comments: ask ask-1",
      details: { issue: "DSP-42" },
    });
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
        topic: "notifications.dispatch.issue.DSP-42.>",
        applied: 2,
      },
    });
    expect(JSON.parse(requests[1]?.init.body as string)).toMatchObject({
      ops: [{ op: "replace", find: "draft", with: "final" }],
      summary: "Record final wording",
    });
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

  test("names the unmapped external repository and DISPATCH_REPO_PROJECTS", async () => {
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
            error: "repository is not mapped in DISPATCH_REPO_PROJECTS",
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
    ).rejects.toThrow("repository owner/repo is not mapped in DISPATCH_REPO_PROJECTS");
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

  test("reads the targeted ask from a Dispatch ask reference", async () => {
    const requests: string[] = [];
    const fetchImpl = async (url: RequestInfo | URL): Promise<Response> => {
      const target = new URL(String(url));
      requests.push(target.pathname + target.search);
      if (target.pathname === "/api/v1/asks/ask-42") {
        return response({
          id: "ask-42",
          issue_key: "DSP-42",
          author: { kind: "session", id: "author-1" },
          question: "Which API should we ship?",
          options: [{ label: "JSON", description: "Use the HTTP API." }, { label: "MCP" }],
          multiple: false,
          custom: true,
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
        });
      }
      throw new Error(`unexpected request: ${target.pathname}`);
    };

    await expect(
      executeDispatchTool({
        tool: "dispatch_read",
        args: { ref: "dispatch://DSP-42/ask/ask-42" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).resolves.toEqual({
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
      ].join("\n"),
      details: { issue: "DSP-42" },
    });
    expect(requests).toEqual(["/api/v1/asks/ask-42"]);
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
              version: 1,
              quote: "Initial wording",
              from: 0,
              to: 15,
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
                version: 2,
                quote: "Revised wording",
                from: 0,
                to: 15,
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

    await expect(
      executeDispatchTool({
        tool: "dispatch_read",
        args: { ref: "dispatch://DSP-42/comment/comment-42" },
        cwd: "/workspace",
        host: "omp",
        config,
        env: {},
        exec: repoExec("owner/repo"),
        fetchImpl: fetchImpl as typeof fetch,
      })
    ).resolves.toEqual({
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
    expect(requests).toEqual(["/api/v1/comments/comment-42"]);
  });
});
