import { afterEach, beforeEach, describe, expect, mock, test } from "bun:test";
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract";
import { existsSync } from "node:fs";
import { join } from "node:path";
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

type RegisteredCommand = {
  readonly name: string;
  readonly description: string | undefined;
  readonly handler: (args: string, context: CommandContext) => Promise<void>;
};

type SessionContext = {
  readonly cwd: string;
  readonly sessionManager: { readonly getSessionId: () => string };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  readonly ui: { readonly notify: (message: string, level: "warning") => void };
};

type CommandContext = {
  readonly ui: { readonly notify: (message: string, level: "info" | "warning" | "error") => void };
};

type TestPi = {
  readonly zod: typeof z;
  readonly registerTool: (tool: RegisteredTool) => void;
  readonly registerCommand: (name: string, command: Omit<RegisteredCommand, "name">) => void;
  readonly on: (
    event: "resources_discover" | "session_start" | "session_switch" | "session_branch" | "session_tree" | "session_shutdown",
    handler: (event: unknown, context: SessionContext) => Promise<unknown>,
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
  readonly active: () => boolean;
};

const natsState = {
  subscriptions: new Map<string, Subscription>(),
  controls: new Map<string, SubscriptionControls>(),
  connectedNames: [] as string[],
  failConnects: 0,
  drainHangs: false,
};

const clipboardState = {
  copiedSessionIDs: [] as string[],
  error: undefined as Error | undefined,
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
      drain: async () => {
        if (natsState.drainHangs) await new Promise(() => undefined);
      },
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
          active: () => active,
        };
        natsState.controls.set(topic, controls);
        const subscription: Subscription = {
          unsubscribe: () => {
            active = false;
            // nats.js ends the async iterator on unsubscribe(), which is what
            // makes a deliberate close indistinguishable from a dropped
            // connection inside the pump. A mock that leaves the iterator
            // parked cannot observe that, and lets resubscribe bugs ship green.
            notify();
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

mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async (text: string) => {
    if (clipboardState.error !== undefined) throw clipboardState.error;
    clipboardState.copiedSessionIDs.push(text);
  },
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
  natsState.drainHangs = false;
  delete process.env.ENVOY_REGISTER_SESSION;
  clipboardState.copiedSessionIDs.length = 0;
  clipboardState.error = undefined;
  delete process.env.ENVOY_RESUBSCRIBE_DELAY_MS;
});

function createPi(options: { readonly clipboardError?: Error } = {}) {
  clipboardState.error = options.clipboardError;
  const commands: RegisteredCommand[] = [];
  const tools: RegisteredTool[] = [];
  const handlers = new Map<string, (event: unknown, context: SessionContext) => Promise<unknown>>();
  const messages: string[] = [];
  const pi: TestPi = {
    zod: z,
    registerTool: (tool) => tools.push(tool),
    registerCommand: (name, command) => commands.push({ name, ...command }),
    on: (event, handler) => handlers.set(event, handler),
    sendMessage: (message) => messages.push(message.content),
  };
  return { commands, copiedSessionIDs: clipboardState.copiedSessionIDs, handlers, messages, pi, tools };
}

function sessionContext(sessionID = "ses_omp"): SessionContext {
  return {
    cwd: "/tmp/envoy-omp-test",
    sessionManager: { getSessionId: () => sessionID },
    setInterval: () => undefined,
    ui: { notify: () => undefined },
  };
}

function commandContext(notifications: string[]): CommandContext {
  return { ui: { notify: (message) => notifications.push(message) } };
}

function response(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200 });
}

describe("envoy OMP extension", () => {
  test("discovers the bundled envoy skill from the repository root", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?resources-discover");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    const resourcesDiscover = fixture.handlers.get("resources_discover");
    if (resourcesDiscover === undefined) throw new Error("resources_discover was not registered");

    const result = await resourcesDiscover({}, sessionContext());
    if (
      typeof result !== "object" ||
      result === null ||
      !("skillPaths" in result) ||
      !Array.isArray(result.skillPaths) ||
      typeof result.skillPaths[0] !== "string"
    ) {
      throw new Error("resources_discover did not return a skill path");
    }

    expect(existsSync(join(result.skillPaths[0], "envoy", "SKILL.md"))).toBe(true);
  });

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

  test("registers /whoami and copies the current session ID to the clipboard", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?whoami-command");
    const fixture = createPi();
    const notifications: string[] = [];

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_command"));
    const whoami = fixture.commands.find((command) => command.name === "whoami");
    if (whoami === undefined) throw new Error("/whoami was not registered");

    await whoami.handler("", commandContext(notifications));

    expect(fixture.copiedSessionIDs).toEqual(["ses_command"]);
    expect(notifications[0]).toContain("ses_command");
    expect(notifications[0]).toContain("Session ID copied");
  });

  test("copies the new session identity from /whoami after an in-process switch", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?whoami-command-switch");
    const fixture = createPi();
    const notifications: string[] = [];

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_command"));
    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_command"));
    const whoami = fixture.commands.find((command) => command.name === "whoami");
    if (whoami === undefined) throw new Error("/whoami was not registered");

    await whoami.handler("", commandContext(notifications));

    expect(fixture.copiedSessionIDs).toEqual(["ses_after_command"]);
    expect(notifications[0]).toContain("ses_after_command");
    expect(notifications[0]).not.toContain("ses_before_command");
  });

  test("still displays the current session ID when clipboard copy fails", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?whoami-command-copy-failure");
    const fixture = createPi({ clipboardError: new Error("clipboard unavailable") });
    const notifications: string[] = [];

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_clipboard_unavailable"));
    const whoami = fixture.commands.find((command) => command.name === "whoami");
    if (whoami === undefined) throw new Error("/whoami was not registered");

    await whoami.handler("", commandContext(notifications));

    expect(fixture.copiedSessionIDs).toEqual([]);
    expect(notifications[0]).toContain("ses_clipboard_unavailable");
    expect(notifications[0]).toContain("Could not copy");
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

  test("rebinds the direct subscription after an in-process session switch", async () => {
    globalThis.fetch = async () => response([]);
    const { default: envoyExtension } = await import("./envoy.ts?session-switch");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_switch"));
    const beforeSwitch = natsState.controls.get("notifications.agent.ses_before_switch");
    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_switch"));

    expect(beforeSwitch?.active()).toBe(false);
    expect(natsState.controls.get("notifications.agent.ses_after_switch")?.active()).toBe(true);
  });

  test("an in-process session switch does not resurrect the previous session's topic", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async () => response([]);
    const { default: envoyExtension } = await import("./envoy.ts?switch-no-resubscribe");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_left_behind"));
    const abandoned = natsState.controls.get("notifications.agent.ses_left_behind");
    expect(abandoned).toBeDefined();

    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_current"));
    // Well past ENVOY_RESUBSCRIBE_DELAY_MS, so the pump has had its chance to
    // treat this deliberate close as an outage and recover from it.
    await new Promise((resolve) => setTimeout(resolve, 60));

    // A resubscribe installs a new controls object for the topic; the original
    // surviving means no second subscription was ever opened.
    expect(natsState.controls.get("notifications.agent.ses_left_behind")).toBe(abandoned);
    expect(abandoned?.active()).toBe(false);
    expect(natsState.controls.get("notifications.agent.ses_current")?.active()).toBe(true);

    abandoned?.push("addressed to the session we switched away from");
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(fixture.messages.some((m) => m.includes("addressed to the session we switched away from"))).toBe(false);
  });

  test("envoy_unsubscribe is not undone by the resubscribe path", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async () => response([]);
    const { default: envoyExtension } = await import("./envoy.ts?unsubscribe-stays");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_unsub"));
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    const unsubscribeTool = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    if (subscribeTool === undefined || unsubscribeTool === undefined) throw new Error("subscription tools were not registered");

    await subscribeTool.execute("", { topics: ["team.standup"] });
    const dropped = natsState.controls.get("team.standup");
    expect(dropped).toBeDefined();

    const result = await unsubscribeTool.execute("", { topics: ["team.standup"] });
    expect(result.details.removed).toEqual(["team.standup"]);
    await new Promise((resolve) => setTimeout(resolve, 60));

    expect(natsState.controls.get("team.standup")).toBe(dropped);

    dropped?.push("published after the tool said it was unsubscribed");
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(fixture.messages.some((m) => m.includes("published after the tool said it was unsubscribed"))).toBe(false);
  });

  test("a deliberately closed topic still recovers from a genuine death once it is back", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async () => response([]);
    const { default: envoyExtension } = await import("./envoy.ts?marker-is-consumed");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_home"));
    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_away"));
    await new Promise((resolve) => setTimeout(resolve, 60));

    // Switch back: the topic we closed on purpose is legitimately wanted again.
    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_home"));
    const reopened = natsState.controls.get("notifications.agent.ses_home");
    expect(reopened?.active()).toBe(true);

    // Now kill it the way a dropped connection does. The close marker from the
    // first switch must have been consumed, so recovery still happens.
    reopened?.end();
    await new Promise((resolve) => setTimeout(resolve, 60));

    const recovered = natsState.controls.get("notifications.agent.ses_home");
    expect(recovered).toBeDefined();
    expect(recovered).not.toBe(reopened);

    recovered?.push("delivered after a genuine iterator death");
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(fixture.messages.some((m) => m.includes("delivered after a genuine iterator death"))).toBe(true);
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

  test("a heartbeat tick during a registry outage warns once instead of rejecting unhandled", async () => {
    process.env.ENVOY_REGISTER_SESSION = "1";
    // The extension captures fetch by value at creation, so the blip must be
    // flipped inside the same function object rather than by reassigning
    // globalThis.fetch afterwards.
    let registryDown = false;
    globalThis.fetch = async () => {
      if (registryDown) throw new Error("network unreachable");
      return response({ session_id: "ses_heartbeat", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?heartbeat-blip");
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { getSessionId: () => "ses_heartbeat" },
      setInterval: (callback) => intervals.push(callback),
      ui: { notify: (message) => notifications.push(message) },
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    // The healthy start must land on the heartbeat branch, not the NATS-retry
    // branch; a failed registration here would register the wrong interval.
    expect(notifications).toEqual([]);
    expect(intervals.length).toBe(1);

    // The internet blip: every registry call now rejects. bun:test swallows
    // unhandled rejections before user listeners, so the observable contract
    // is the owned rejection's warning: exactly one per outage, however many
    // ticks elapse. On the unfixed code the tick rejects unhandled (fatal in
    // OMP via postmortem exitAfterFatal) and no warning ever appears.
    registryDown = true;
    for (const tick of intervals) tick();
    await new Promise((resolve) => setTimeout(resolve, 20));
    for (const tick of intervals) tick();
    await new Promise((resolve) => setTimeout(resolve, 20));

    const warnings = notifications.filter((message) => message.includes("registry heartbeat failed"));
    expect(warnings).toHaveLength(1);
  });

  test("a rebind during a network outage notifies instead of failing the handler", async () => {
    process.env.ENVOY_REGISTER_SESSION = "1";
    let registryDown = false;
    globalThis.fetch = async () => {
      if (registryDown) throw new Error("network unreachable");
      return response({ session_id: "ses_rebind", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?rebind-blip");
    const fixture = createPi();
    const notifications: string[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { getSessionId: () => "ses_rebind" },
      setInterval: () => undefined,
      ui: { notify: (message) => notifications.push(message) },
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    registryDown = true;
    const switched: SessionContext = {
      ...context,
      sessionManager: { getSessionId: () => "ses_rebind_next" },
    };

    // The handler must resolve; a rejection here surfaces as an extension
    // handler failure on a live session.
    await fixture.handlers.get("session_switch")?.({}, switched);

    expect(notifications.some((message) => message.includes("rebind failed"))).toBe(true);
  });

  test("session_shutdown resolves within its budget when drain hangs on a dead connection", async () => {
    globalThis.fetch = async () => response([]);
    const { default: envoyExtension } = await import("./envoy.ts?shutdown-hang");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_shutdown"));

    natsState.drainHangs = true;
    const startedAt = Date.now();
    await fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_shutdown"));

    // OMP kills shutdown handlers at 2s; the bounded drain must finish first.
    expect(Date.now() - startedAt).toBeLessThan(1_900);
  });
});
