import { afterEach, beforeEach, describe, expect, mock, test } from "bun:test";
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

type SubscriptionControls = {
  readonly push: (data: string) => void;
  readonly end: () => void;
  readonly fail: (error: Error) => void;
};

const natsState = {
  subscriptions: new Map<string, Subscription>(),
  controls: new Map<string, SubscriptionControls>(),
  connectedNames: [] as string[],
  failConnects: 0,
};

mock.module("nats", () => ({
  connect: async ({ name }: { readonly name: string }) => {
    if (natsState.failConnects > 0) {
      natsState.failConnects -= 1;
      throw new Error("CONNECTION_REFUSED");
    }
    natsState.connectedNames.push(name);
    return {
      isClosed: () => false,
      drain: async () => undefined,
      subscribe: (topic: string) => {
        let active = true;
        const queue: { readonly subject: string; readonly data: Uint8Array }[] = [];
        let wake: (() => void) | undefined;
        let ended = false;
        let failure: Error | undefined;
        const notify = () => {
          wake?.();
          wake = undefined;
        };
        const controls = {
          push: (data: string) => {
            queue.push({ subject: topic, data: new TextEncoder().encode(data) });
            notify();
          },
          end: () => {
            ended = true;
            notify();
          },
          fail: (error: Error) => {
            failure = error;
            notify();
          },
        };
        natsState.controls.set(topic, controls);
        const subscription: Subscription = {
          unsubscribe: () => {
            active = false;
          },
          [Symbol.asyncIterator]: () => ({
            next: async (): Promise<IteratorResult<{ readonly subject: string; readonly data: Uint8Array }>> => {
              for (;;) {
                if (failure) throw failure;
                if (!active || ended) return { done: true, value: undefined };
                const item = queue.shift();
                if (item) return { done: false, value: item };
                await new Promise<void>((resolve) => {
                  wake = resolve;
                });
              }
            },
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

const originalNatsUrl = process.env.ENVOY_NATS_URL;

beforeEach(() => {
  process.env.ENVOY_NATS_URL = "nats://nats-under-test:4222";
});

afterEach(() => {
  if (originalNatsUrl === undefined) delete process.env.ENVOY_NATS_URL;
  else process.env.ENVOY_NATS_URL = originalNatsUrl;
  globalThis.fetch = originalFetch;
  natsState.connectedNames.length = 0;
  natsState.subscriptions.clear();
  natsState.controls.clear();
  natsState.failConnects = 0;
  delete process.env.ENVOY_RESUBSCRIBE_DELAY_MS;
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

  test("disables inbound messaging loudly when ENVOY_NATS_URL is unset", async () => {
    delete process.env.ENVOY_NATS_URL;
    const notifications: string[] = [];
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { getSessionId: () => "ses_unconfigured" },
      setInterval: (callback) => intervals.push(callback),
      ui: { notify: (message) => notifications.push(message) },
    };
    const { default: envoyExtension } = await import("./envoy.ts?unconfigured");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    expect(notifications[0]).toContain("ENVOY_NATS_URL is not set");
    expect(natsState.connectedNames.length).toBe(0);
    expect(intervals.length).toBe(0);
    expect(fixture.tools.map((tool) => tool.name)).toContain("envoy_send");
  });

  test("retries NATS in the background when the initial connection fails", async () => {
    natsState.failConnects = 1;
    const notifications: string[] = [];
    const intervals: { callback: () => void; intervalMs: number }[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { getSessionId: () => "ses_retry" },
      setInterval: (callback, intervalMs) => intervals.push({ callback, intervalMs }),
      ui: { notify: (message) => notifications.push(message) },
    };
    const { default: envoyExtension } = await import("./envoy.ts?retry");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    expect(notifications[0]).toContain("retrying in the background");
    expect(natsState.subscriptions.has("notifications.agent.ses_retry")).toBe(false);
    const retry = intervals[0];
    if (retry === undefined) throw new Error("retry interval was not registered");

    retry.callback();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(natsState.subscriptions.has("notifications.agent.ses_retry")).toBe(true);
    expect(notifications[1]).toContain("NATS connection established");
    const connectionsAfterRecovery = natsState.connectedNames.length;

    retry.callback();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(natsState.connectedNames.length).toBe(connectionsAfterRecovery);
  });

  test("a failed message injection does not tear down the subscription", async () => {
    globalThis.fetch = async () => response([]);
    const { default: envoyExtension } = await import("./envoy.ts?deliver-throw");
    const fixture = createPi();
    let throwNext = true;
    const delivered: string[] = [];
    const pi: TestPi = {
      ...fixture.pi,
      sendMessage: (message) => {
        if (throwNext) {
          throwNext = false;
          throw new Error("injection rejected mid-compaction");
        }
        delivered.push(message.content);
      },
    };

    envoyExtension(pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_pump"));
    const controls = natsState.controls.get("notifications.agent.ses_pump");
    expect(controls).toBeDefined();

    controls?.push("first message hits the throwing window");
    await new Promise((resolve) => setTimeout(resolve, 10));
    controls?.push("second message must still deliver");
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(delivered.length).toBe(1);
    expect(delivered[0]).toContain("second message must still deliver");
  });

  test("an ended subscription iterator resubscribes instead of going deaf", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async () => response([]);
    const { default: envoyExtension } = await import("./envoy.ts?resubscribe");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_deaf"));
    const first = natsState.controls.get("notifications.agent.ses_deaf");
    expect(first).toBeDefined();

    // Kill the iterator the way a closed/errored connection does.
    first?.end();
    await new Promise((resolve) => setTimeout(resolve, 60));

    const second = natsState.controls.get("notifications.agent.ses_deaf");
    expect(second).toBeDefined();
    expect(second).not.toBe(first);

    second?.push("post-recovery message");
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(fixture.messages.some((m) => m.includes("post-recovery message"))).toBe(true);
  });
});
