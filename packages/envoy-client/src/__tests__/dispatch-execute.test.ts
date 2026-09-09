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
      requests.push({ url: String(url), init: init ?? {} });
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

    expect(new URL(requests[0]?.url as string).pathname).toBe(
      "/api/v1/issues/owner%2Frepo%2341/asks"
    );
    expect(JSON.parse(requests[0]?.init.body as string)).toMatchObject({
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
});
