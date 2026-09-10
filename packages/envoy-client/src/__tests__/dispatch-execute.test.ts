import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { dispatchIssueSubject } from "@legion/contracts";
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
      details: {
        issue: "DSP-42",
        topic: dispatchIssueSubject("DSP-42", ">"),
      },
    });
    expect(dispatchSubscriptionTopic(result.details)).toBe(dispatchIssueSubject("DSP-42", ">"));
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
        topic: dispatchIssueSubject("DSP-42", ">"),
        applied: 2,
      },
    });
    expect(JSON.parse(requests[1]?.init.body as string)).toMatchObject({
      ops: [{ op: "replace", find: "draft", with: "final" }],
      summary: "Record final wording",
    });
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
      args: { issue: "DSP-42", name: "spec.md", content: "# Spec\n", primary: true },
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
    expect(JSON.parse(requests[0]?.init.body as string)).toMatchObject({
      name: "spec.md",
      content: "# Spec\n",
      primary: true,
      actor: { kind: "session", id: "session-42" },
    });
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
      ].join("\n"),
      details: {
        issue: "DSP-42",
        topic: dispatchIssueSubject("DSP-42", ">"),
      },
    });
    expect(dispatchSubscriptionTopic(result.details)).toBe(dispatchIssueSubject("DSP-42", ">"));
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
      details: {
        issue: "DSP-42",
        topic: dispatchIssueSubject("DSP-42", ">"),
      },
    });
    expect(dispatchSubscriptionTopic(result.details)).toBe(dispatchIssueSubject("DSP-42", ">"));
    expect(requests).toEqual(["/api/v1/comments/comment-42"]);
  });
  test("returns a subscription topic when reading an issue summary", async () => {
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

    expect(result.details).toEqual({
      issue: "DSP-42",
      topic: dispatchIssueSubject("DSP-42", ">"),
    });
    expect(dispatchSubscriptionTopic(result.details)).toBe(dispatchIssueSubject("DSP-42", ">"));
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
      details: { issue: "DSP-42", topic: dispatchIssueSubject("DSP-42", ">") },
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
      details: { issue: "DSP-42", topic: dispatchIssueSubject("DSP-42", ">") },
    });
  });
});
