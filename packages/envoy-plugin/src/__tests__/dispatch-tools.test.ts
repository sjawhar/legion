import { expect, spyOn, test } from "bun:test";
import { tool } from "@opencode-ai/plugin/tool";
import { logger } from "../log";
import initPlugin from "../server";

const expectedDispatchTools = [
  ["dispatch_issue", ["project", "title"]],
  ["dispatch_ask", ["issue", "question"]],
  ["dispatch_comment", ["issue", "body"]],
  ["dispatch_suggest", ["issue", "artifact", "quote", "replace_with"]],
  ["dispatch_message", ["issue", "body"]],
  ["dispatch_doc_edit", ["issue", "artifact", "ops"]],
  ["dispatch_doc_read", []],
  ["dispatch_artifact", ["issue", "name", "path"]],
  ["dispatch_read", []],
] as const;

type RegisteredTool = {
  readonly args: Record<string, never>;
  execute(args: Record<string, unknown>, context: unknown): Promise<unknown>;
};

const toolContext = (sessionID: string) =>
  ({
    sessionID,
    directory: "/tmp/opencode-workspace",
    messageID: "message_1",
    agent: "build",
    metadata: () => undefined,
  }) as never;

test("registers every native Dispatch tool with its shared schema and executes an ask as OpenCode", async () => {
  const previous = { ...process.env };
  let received: unknown;
  const service = Bun.serve({
    port: 0,
    fetch: async (request) => {
      if (new URL(request.url).pathname === "/api/v1/issues/DSP-41/asks") {
        received = await request.json();
        return Response.json({
          id: "ask-1",
          issue_key: "DSP-41",
          author: { kind: "session", id: "ses_opencode" },
          question: "Should the branch merge?",
          options: [],
          multiple: false,
          custom: true,
          urgency: "med",
          anchor: null,
          state: "open",
          answer: null,
          created_at: "2026-09-09T00:00:00Z",
        });
      }
      return new Response(null, { status: 404 });
    },
  });
  process.env.DISPATCH_URL = `http://127.0.0.1:${service.port}`;
  process.env.DISPATCH_TOKEN = "test-token";
  const warn = spyOn(logger, "warn").mockImplementation(() => {});

  let dispose: (() => void) | undefined;
  try {
    const hooks = await initPlugin({ serverUrl: new URL(`http://127.0.0.1:${service.port}/`) });
    dispose = hooks.dispose;
    const tools = hooks.tool as unknown as Record<string, RegisteredTool>;

    expect(Object.keys(tools).filter((name) => name.startsWith("dispatch_"))).toEqual(
      expectedDispatchTools.map(([name]) => name)
    );
    for (const [name, required] of expectedDispatchTools) {
      const registered = tools[name];
      expect(registered).toBeDefined();
      const schema = tool.schema.toJSONSchema(tool.schema.object(registered.args), {
        io: "input",
      }) as {
        readonly required?: readonly string[];
      };
      expect(schema.required ?? []).toEqual(required);
    }

    const result = await tools.dispatch_ask?.execute(
      { issue: "DSP-41", question: "Should the branch merge?" },
      toolContext("ses_opencode")
    );

    expect(result).toEqual({
      title: "Dispatch",
      output: "Opened ask ask-1: Should the branch merge?",
      metadata: {
        issue: "DSP-41",
        topic: "notifications.dispatch.issue.DSP-41.>",
        ask: "ask-1",
      },
    });
    expect(received).toMatchObject({
      question: "Should the branch merge?",
      actor: {
        kind: "session",
        id: "ses_opencode",
        origin: { host: "opencode", cwd: "/tmp/opencode-workspace" },
      },
    });
    expect(warn).not.toHaveBeenCalled();
  } finally {
    warn.mockRestore();
    dispose?.();
    service.stop(true);
    process.env = { ...previous };
  }
});
