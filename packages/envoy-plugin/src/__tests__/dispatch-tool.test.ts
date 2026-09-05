import { afterEach, describe, expect, it } from "bun:test";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { dispatchToolShape } from "@legion/envoy-client/dispatch-contract";
import { tool } from "@opencode-ai/plugin/tool";
// The workspace-root zod: the same build `dispatchToolShape` is made with, so it
// can render the contract as JSON Schema for comparison with the host-zod copy.
import { z } from "zod";

const originalFetch = globalThis.fetch;
const originalEnv = { ...process.env };

afterEach(() => {
  globalThis.fetch = originalFetch;
  process.env = { ...originalEnv };
});

function fakeGhOnPath(): void {
  const dir = mkdtempSync(path.join(tmpdir(), "fake-gh-"));
  writeFileSync(path.join(dir, "gh"), "#!/bin/sh\necho test-token\n", { mode: 0o755 });
  process.env.PATH = `${dir}:${process.env.PATH ?? ""}`;
}

// Dynamic import on purpose: each case needs a fresh plugin module so process.env
// (HOME, DISPATCH_MCP_URL, PATH) set above is what the plugin reads at load.
async function loadPlugin(query: string) {
  const { default: plugin } = await import(`../server?${query}`);
  return plugin({ serverUrl: new URL("http://127.0.0.1:4096/") });
}

interface RecordedPost {
  readonly headers: Record<string, string>;
  readonly body: { readonly params: { readonly arguments: Record<string, unknown> } };
}

const toolContext = (sessionID: string) =>
  ({
    sessionID,
    directory: "/tmp",
    messageID: "m",
    agent: "a",
    metadata: () => undefined,
  }) as never;

describe("dispatch tool", () => {
  it("is absent when dispatch is not enabled", async () => {
    delete process.env.DISPATCH_MCP_URL;
    process.env.HOME = mkdtempSync(path.join(tmpdir(), "no-envoy-json-"));
    const plugin = await loadPlugin("disabled");
    expect("dispatch" in plugin.tool).toBe(false);
    plugin.dispose();
  });

  it("posts one stateless call carrying the OpenCode session id and title", async () => {
    fakeGhOnPath();
    process.env.DISPATCH_MCP_URL = "http://127.0.0.1:1/mcp";
    const posts: RecordedPost[] = [];
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = input.toString();
      if (url === "http://127.0.0.1:1/mcp") {
        posts.push({
          headers: init?.headers as Record<string, string>,
          body: JSON.parse(String(init?.body)),
        });
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
      }
      if (url === "http://127.0.0.1:4096/session/ses_oc") {
        return new Response(JSON.stringify({ title: "OpenCode title" }), {
          headers: { "content-type": "application/json" },
        });
      }
      return new Response("{}", { status: 200 });
    }) as typeof fetch;
    const plugin = await loadPlugin("enabled");
    const output = await plugin.tool.dispatch.execute(
      { thread: "acme-org/example-repo#5", context: "c", question: "q" },
      toolContext("ses_oc")
    );
    expect(JSON.parse(output)).toEqual({
      thread: 5,
      url: "https://github.com/acme-org/example-repo/issues/5",
    });
    expect(posts).toHaveLength(1);
    expect(posts[0]?.headers.Authorization).toBe("Bearer test-token");
    expect(posts[0]?.headers["Mcp-Session-Id"]).toBeUndefined();
    const args = posts[0]?.body.params.arguments ?? {};
    expect(args.thread).toBe("acme-org/example-repo#5");
    expect(args.origin).toMatchObject({
      host: "opencode",
      cwd: "/tmp",
      sessionId: "ses_oc",
      sessionTitle: "OpenCode title",
    });
    plugin.dispose();
  });

  it("throws the argument error for a mixed-mode call without calling the service", async () => {
    process.env.DISPATCH_MCP_URL = "http://127.0.0.1:1/mcp";
    let fetched = 0;
    globalThis.fetch = (async (_input: RequestInfo | URL) => {
      fetched++;
      return new Response("{}");
    }) as typeof fetch;
    const plugin = await loadPlugin("invalid");
    await expect(
      plugin.tool.dispatch.execute(
        { subject: "s", thread: "5", context: "c", question: "q" },
        toolContext("s")
      )
    ).rejects.toThrow(
      "dispatch: pass either subject (open a thread) or thread (continue one), not both"
    );
    expect(fetched).toBe(0);
    plugin.dispose();
  });

  it("exposes the contract's flat schema, field for field, as JSON Schema", async () => {
    process.env.DISPATCH_MCP_URL = "http://127.0.0.1:1/mcp";
    const plugin = await loadPlugin("schema");
    const hostZod = tool.schema;
    const fromPlugin = hostZod.toJSONSchema(hostZod.object(plugin.tool.dispatch.args), {
      io: "input",
    });
    const fromContract = z.toJSONSchema(z.object(dispatchToolShape), { io: "input" });
    expect(fromPlugin).toEqual(fromContract);
    expect(Object.keys(fromContract.properties ?? {})).toEqual([
      "subject",
      "thread",
      "context",
      "question",
      "ask",
      "urgency",
      "repo",
      "parent",
    ]);
    expect(fromContract.required).toEqual(["context", "question"]);
    plugin.dispose();
  });

  it("refuses to load on an invalid envoy.json, naming the file and key", async () => {
    delete process.env.DISPATCH_MCP_URL;
    const home = mkdtempSync(path.join(tmpdir(), "bad-envoy-json-"));
    const dir = path.join(home, ".config", "opencode");
    mkdirSync(dir, { recursive: true });
    writeFileSync(
      path.join(dir, "envoy.json"),
      JSON.stringify({ dispatch: { enabled: true, defaultRepo: "o/r" } })
    );
    process.env.HOME = home;
    await expect(loadPlugin("bad-config")).rejects.toThrow(/envoy\.json.*dispatch\.defaultRepo/);
  });
});
