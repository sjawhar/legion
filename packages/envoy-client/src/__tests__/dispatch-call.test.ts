import { describe, expect, it } from "bun:test";
import { executeDispatch, prepareDispatchCall } from "../dispatch-call";
import { DispatchArgumentError } from "../dispatch-contract";
import type { ExecFn } from "../dispatch-cwd";

function fakeExec(script: Record<string, string>): ExecFn {
  return async (file, args) => {
    const key = [file, ...args].join(" ");
    if (!(key in script)) throw new Error(`unscripted: ${key}`);
    return { stdout: script[key] as string };
  };
}

const withRemote = fakeExec({
  "jj git remote list": "origin https://github.com/acme-org/example-repo.git",
});
const noRemote = fakeExec({});

describe("prepareDispatchCall", () => {
  it("fills repo from the cwd and stamps host + session identity onto the origin", async () => {
    const args = await prepareDispatchCall({
      call: { subject: "s", context: "c", question: "q" },
      cwd: "/repo",
      host: "omp",
      sessionId: "ses_1",
      sessionTitle: "fix login",
      env: { TMUX_PANE: "%3" },
      exec: fakeExec({
        "jj git remote list": "origin https://github.com/acme-org/example-repo.git",
        "tmux display-message -p -t %3 #S:#I.#P #{pane_id}": "main:3.0 %3\n",
      }),
    });
    expect(args.repo).toBe("acme-org/example-repo");
    expect(args.origin).toMatchObject({
      host: "omp",
      cwd: "/repo",
      tmux: "main:3.0",
      pane: "%3",
      sessionId: "ses_1",
      sessionTitle: "fix login",
    });
    expect(typeof args.origin.machine).toBe("string");
  });

  it("omits session fields the host could not supply", async () => {
    const args = await prepareDispatchCall({
      call: { subject: "s", context: "c", question: "q" },
      cwd: "/repo",
      host: "claude",
      sessionId: "abc",
      env: {},
      exec: withRemote,
    });
    expect(args.origin.sessionId).toBe("abc");
    expect("sessionTitle" in args.origin).toBe(false);
  });

  it("leaves an explicit repo and a qualified parent alone, even without a cwd remote", async () => {
    const explicit = await prepareDispatchCall({
      call: { subject: "s", context: "c", question: "q", repo: "explicit/repo" },
      cwd: "/repo",
      host: "omp",
      env: {},
      exec: noRemote,
    });
    expect(explicit.repo).toBe("explicit/repo");
    const qualified = await prepareDispatchCall({
      call: { subject: "s", context: "c", question: "q", parent: "acme-org/example-repo#42#9001" },
      cwd: "/repo",
      host: "omp",
      env: {},
      exec: noRemote,
    });
    expect(qualified.repo).toBeUndefined();
    const bareParent = await prepareDispatchCall({
      call: { subject: "s", context: "c", question: "q", parent: "42#9001" },
      cwd: "/repo",
      host: "omp",
      env: {},
      exec: withRemote,
    });
    expect(bareParent.repo).toBe("acme-org/example-repo");
  });

  it("names the cwd and the fix when an opening call has no repo", async () => {
    await expect(
      prepareDispatchCall({
        call: { subject: "s", context: "c", question: "q" },
        cwd: "/repo",
        host: "omp",
        env: {},
        exec: noRemote,
      })
    ).rejects.toThrow(
      new DispatchArgumentError("dispatch: /repo has no GitHub remote; pass repo=owner/name")
    );
  });

  it("fills repo for a bare thread and skips it for a qualified one", async () => {
    const bare = await prepareDispatchCall({
      call: { thread: "12", context: "c", question: "q" },
      cwd: "/repo",
      host: "opencode",
      sessionId: "ses",
      env: {},
      exec: withRemote,
    });
    expect(bare.repo).toBe("acme-org/example-repo");
    expect(bare.thread).toBe("12");
    const qualified = await prepareDispatchCall({
      call: { thread: "acme-org/example-repo#12", context: "c", question: "q" },
      cwd: "/repo",
      host: "opencode",
      env: {},
      exec: noRemote,
    });
    expect(qualified.repo).toBeUndefined();
    await expect(
      prepareDispatchCall({
        call: { thread: "12", context: "c", question: "q" },
        cwd: "/repo",
        host: "omp",
        env: {},
        exec: noRemote,
      })
    ).rejects.toThrow("dispatch: /repo has no GitHub remote; pass thread=owner/name#<n>");
  });
});

describe("executeDispatch", () => {
  it("prepares the call and posts it with a token minted through the same exec", async () => {
    const exec = fakeExec({
      "jj git remote list": "origin https://github.com/acme-org/example-repo.git",
      "gh auth token": "test-token\n",
    });
    const posts: Array<{ url: string; init: RequestInit }> = [];
    const fetchImpl = (async (url: string | URL | Request, init?: RequestInit) => {
      posts.push({ url: url.toString(), init: init ?? {} });
      return new Response(
        JSON.stringify({
          jsonrpc: "2.0",
          id: 1,
          result: {
            content: [
              {
                type: "text",
                text: '{"thread":5,"url":"https://github.com/acme-org/example-repo/issues/5"}',
              },
            ],
          },
        }),
        { headers: { "content-type": "application/json" } }
      );
    }) as typeof fetch;

    const result = await executeDispatch({
      call: { subject: "s", context: "c", question: "q" },
      cwd: "/work",
      host: "omp",
      sessionId: "ses_1",
      sessionTitle: undefined,
      serviceUrl: "http://127.0.0.1:1/mcp",
      env: {},
      exec,
      fetchImpl,
    });

    expect(result).toEqual({
      thread: 5,
      url: "https://github.com/acme-org/example-repo/issues/5",
    });
    expect(posts).toHaveLength(1);
    expect(posts[0]?.url).toBe("http://127.0.0.1:1/mcp");
    const headers = posts[0]?.init.headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer test-token");
    const body = JSON.parse(String(posts[0]?.init.body)) as {
      params: { arguments: Record<string, unknown> };
    };
    expect(body.params.arguments.repo).toBe("acme-org/example-repo");
    expect(body.params.arguments.origin).toMatchObject({
      host: "omp",
      cwd: "/work",
      sessionId: "ses_1",
    });
    expect("sessionTitle" in (body.params.arguments.origin as object)).toBe(false);
  });

  it("fails before any request when the token cannot be minted", async () => {
    let fetched = 0;
    const fetchImpl = (async (_url: string | URL | Request, _init?: RequestInit) => {
      fetched++;
      return new Response("{}");
    }) as typeof fetch;
    await expect(
      executeDispatch({
        call: { thread: "acme-org/example-repo#5", context: "c", question: "q" },
        cwd: "/work",
        host: "claude",
        serviceUrl: "http://127.0.0.1:1/mcp",
        env: {},
        exec: noRemote,
        fetchImpl,
      })
    ).rejects.toThrow("dispatch: gh auth token returned empty in /work");
    expect(fetched).toBe(0);
  });
});
