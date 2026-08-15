import { afterEach, describe, expect, mock, test } from "bun:test";
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract";
import { z } from "zod";

type ToolResult = {
  readonly content: readonly { readonly type: "text"; readonly text: string }[];
  readonly details: Readonly<Record<string, unknown>>;
  readonly isError?: boolean;
};

type RegisteredTool = {
  readonly name: string;
  readonly description: string;
  readonly parameters: unknown;
  readonly execute: (id: string, params: Record<string, unknown>) => Promise<ToolResult>;
};

type SessionContext = {
  readonly cwd: string;
  readonly sessionManager: { readonly getSessionId: () => string };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  readonly ui: { readonly notify: (message: string, level: "warning") => void };
};

type TestPi = {
  readonly zod: typeof z;
  readonly registerTool: (tool: RegisteredTool) => void;
  readonly on: (
    event: "session_start" | "session_shutdown",
    handler: (event: unknown, context: SessionContext) => Promise<void>,
  ) => void;
  readonly sendMessage: (message: { readonly content: string }, options: unknown) => void;
};

type Subscription = {
  readonly unsubscribe: () => void;
  readonly [Symbol.asyncIterator]: () => AsyncIterator<{ readonly subject: string; readonly data: Uint8Array }>;
};

const natsState = {
  subscriptions: new Map<string, Subscription>(),
  connectedNames: [] as string[],
};

mock.module("nats", () => ({
  connect: async ({ name }: { readonly name: string }) => {
    natsState.connectedNames.push(name);
    return {
      isClosed: () => false,
      drain: async () => undefined,
      subscribe: (topic: string) => {
        let active = true;
        const subscription: Subscription = {
          unsubscribe: () => {
            active = false;
          },
          [Symbol.asyncIterator]: () => ({
            next: async () => (active ? { done: false, value: await new Promise<never>(() => {}) } : { done: true }),
          }),
        };
        natsState.subscriptions.set(topic, subscription);
        return subscription;
      },
    };
  },
  StringCodec: () => ({ decode: (data: Uint8Array) => new TextDecoder().decode(data) }),
}));

const originalFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = originalFetch;
  natsState.connectedNames.length = 0;
  natsState.subscriptions.clear();
});

function createPi() {
  const tools: RegisteredTool[] = [];
  const handlers = new Map<string, (event: unknown, context: SessionContext) => Promise<void>>();
  const messages: string[] = [];
  const pi: TestPi = {
    zod: z,
    registerTool: (tool) => tools.push(tool),
    on: (event, handler) => handlers.set(event, handler),
    sendMessage: (message) => messages.push(message.content),
  };
  return { handlers, messages, pi, tools };
}

function sessionContext(sessionID = "ses_omp"): SessionContext {
  return {
    cwd: "/tmp/envoy-omp-test",
    sessionManager: { getSessionId: () => sessionID },
    setInterval: () => undefined,
    ui: { notify: () => undefined },
  };
}

function response(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200 });
}

describe("envoy OMP extension", () => {
  test("registers the shared eight-tool contract and delegates HTTP operations to EnvoyClient", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/v1/sessions") return response([]);
      if (url.pathname === "/v1/interests/ses_omp") return response({ session_id: "ses_omp", machine_id: "test", dir: "/tmp", topics: [] });
      return response({
        event_id: "evt_1",
        source: "agent",
        source_event_id: "evt_1",
        topic: "notifications.agent.ses_target",
        dedupe_key: "dedupe_1",
        issued_at: 1,
        payload_summary: "message",
        trace_id: "trace_1",
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());

    expect(fixture.tools.map((tool) => ({ name: tool.name, description: tool.description }))).toEqual(
      envoyToolSpecs.map((spec) => ({ name: spec.name, description: spec.description })),
    );

    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role: "controller" });
    await fixture.tools.find((tool) => tool.name === "envoy_list")?.execute("", {});
    await fixture.tools.find((tool) => tool.name === "envoy_send")?.execute("", {
      target_session: "ses_target",
      message: "direct",
    });
    await fixture.tools.find((tool) => tool.name === "envoy_publish")?.execute("", {
      topic: "team.test",
      message: "broadcast",
    });
    await fixture.tools.find((tool) => tool.name === "envoy_sessions")?.execute("", {});

    expect(requests).toEqual([
      { path: "/v1/roles/set", body: { session_id: "ses_omp", role: "controller" } },
      { path: "/v1/interests/ses_omp", body: undefined },
      {
        path: "/v1/messages/send",
        body: { source_session: "ses_omp", target_session: "ses_target", message: "direct" },
      },
      {
        path: "/v1/messages/publish",
        body: { source_session: "ses_omp", topic: "team.test", message: "broadcast" },
      },
      { path: "/v1/sessions", body: undefined },
    ]);
  });

  test("registers an optional self-subscribed interest on session start", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    globalThis.fetch = async (input, init) => {
      requests.push({
        path: new URL(input.toString()).pathname,
        body: init?.body === undefined ? undefined : JSON.parse(init.body.toString()),
      });
      return response({ session_id: "ses_register", machine_id: "test", dir: "/tmp", topics: [] });
    };
    process.env.ENVOY_REGISTER_SESSION = "1";
    const { default: envoyExtension } = await import("./envoy.ts?register-session");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_register"));

    expect(requests).toContainEqual({
      path: "/v1/interests/subscribe",
      body: {
        session_id: "ses_register",
        dir: "/tmp/envoy-omp-test",
        topics: ["notifications.agent.ses_register"],
        port: 0,
        title: "",
        driving: false,
        self_subscribed: true,
      },
    });
    delete process.env.ENVOY_REGISTER_SESSION;
  });

  test("reports the active session directory through envoy_whoami", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?whoami-directory");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_whoami"));
    const whoami = fixture.tools.find((tool) => tool.name === "envoy_whoami");
    if (whoami === undefined) throw new Error("envoy_whoami was not registered");

    const result = await whoami.execute("", {});

    expect(JSON.parse(result.content[0]?.text ?? "")).toMatchObject({
      session_id: "ses_whoami",
      dir: "/tmp/envoy-omp-test",
    });
  });
});
