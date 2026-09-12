import { afterEach, beforeEach, describe, expect, mock, spyOn, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { hostname } from "node:os";
import { join } from "node:path";
import {
  DISPATCH_ISSUE_TOPIC_PREFIX,
  dispatchIssueSubject,
  dispatchToolSpecs,
} from "@legion/contracts";
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract";
import { decode } from "@toon-format/toon";
import { z } from "zod";
import type { MessageRenderer, MessageRendererTheme, PiApi } from "../src/pi-types";

type ToolResult = {
  readonly content: readonly { readonly type: "text"; readonly text: string }[];
  readonly details: Readonly<Record<string, unknown>>;
  readonly isError?: boolean;
};

type RegisteredTool = {
  readonly name: string;
  readonly description: string;
  readonly parameters: unknown;
  readonly execute: (
    id: string,
    params: Record<string, unknown>,
    signal?: AbortSignal,
    onUpdate?: unknown,
    context?: SessionContext
  ) => Promise<ToolResult>;
};

type RegisteredCommand = {
  readonly name: string;
  readonly description: string | undefined;
  readonly handler: (args: string, context: CommandContext) => Promise<void>;
};

type SessionContext = {
  readonly cwd: string;
  readonly sessionManager: {
    readonly getSessionId: () => string;
    readonly getSessionName?: () => string | undefined;
  };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  readonly ui: { readonly notify: (message: string, level: "warning") => void };
};

type CommandContext = {
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning" | "error") => void;
  };
};

type TestPi = {
  readonly zod: typeof z;
  readonly registerTool: (tool: RegisteredTool) => void;
  readonly registerCommand: (name: string, command: Omit<RegisteredCommand, "name">) => void;
  readonly registerMessageRenderer: PiApi["registerMessageRenderer"];
  readonly on: (
    event:
      | "resources_discover"
      | "session_start"
      | "session_switch"
      | "session_branch"
      | "session_tree"
      | "session_shutdown"
      | "tool_result",
    handler: (event: unknown, context: SessionContext) => Promise<unknown>
  ) => void;
  readonly sendMessage: (message: { readonly content: string }, options: unknown) => void;
  readonly askEphemeral?: (input: {
    readonly prompt: string;
    readonly signal?: AbortSignal;
  }) => Promise<{ readonly replyText: string }>;
  readonly appendEntry: PiApi["appendEntry"];
};
type Subscription = {
  readonly unsubscribe: () => void;
  readonly [Symbol.asyncIterator]: () => AsyncIterator<{
    readonly subject: string;
    readonly data: Uint8Array;
    readonly reply: string;
  }>;
};

type SubscriptionControls = {
  readonly push: (data: string, reply?: string) => void;
  readonly end: () => void;
  readonly fail: (error: Error) => void;
  readonly active: () => boolean;
};

const natsState = {
  subscriptions: new Map<string, Subscription>(),
  controls: new Map<string, SubscriptionControls>(),
  controlsByTopic: new Map<string, SubscriptionControls[]>(),
  published: [] as { readonly subject: string; readonly data: Uint8Array | undefined }[],
  connectedNames: [] as string[],
  failConnects: 0,
  drainHangs: false,
  drainStarted: false,
};

const targetedDispatchPayload = readFileSync(
  new URL("../../contracts/fixtures/dispatch-targeted-delivery.json", import.meta.url),
  "utf8"
);

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
        natsState.drainStarted = true;
        if (natsState.drainHangs) await Promise.withResolvers<never>().promise;
      },
      publish: (subject: string, data?: Uint8Array) => natsState.published.push({ subject, data }),
      subscribe: (topic: string) => {
        let active = true;
        const queue: {
          readonly subject: string;
          readonly data: Uint8Array;
          readonly reply: string;
        }[] = [];
        let wake: (() => void) | undefined;
        let ended = false;
        let failure: Error | undefined;
        const notify = () => {
          wake?.();
          wake = undefined;
        };
        const controls = {
          push: (data: string, reply = "") => {
            queue.push({ subject: topic, data: new TextEncoder().encode(data), reply });
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
        const controlsForTopic = natsState.controlsByTopic.get(topic);
        if (controlsForTopic === undefined) natsState.controlsByTopic.set(topic, [controls]);
        else controlsForTopic.push(controls);
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
            next: async (): Promise<
              IteratorResult<{
                readonly subject: string;
                readonly data: Uint8Array;
                readonly reply: string;
              }>
            > => {
              for (;;) {
                if (failure) throw failure;
                if (!active || ended) return { done: true, value: undefined };
                const item = queue.shift();
                if (item) return { done: false, value: item };
                const pending = Promise.withResolvers<void>();
                wake = pending.resolve;
                await pending.promise;
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
const originalHome = process.env.HOME;
const originalPath = process.env.PATH;
const originalDispatchUrl = process.env.DISPATCH_URL;
const originalDispatchToken = process.env.DISPATCH_TOKEN;
// The Legion daemon exports DISPATCH_TOKEN_FILE into every pane it spawns; a suite run from one
// must not see it, or the extension resolves a real Dispatch config and registers the tools.
const originalDispatchTokenFile = process.env.DISPATCH_TOKEN_FILE;
const originalTmuxPane = process.env.TMUX_PANE;

beforeEach(() => {
  process.env.ENVOY_NATS_URL = "nats://nats-under-test:4222";
  delete process.env.DISPATCH_URL;
  delete process.env.DISPATCH_TOKEN;
  delete process.env.DISPATCH_TOKEN_FILE;
  process.env.HOME = "/nonexistent-home-for-envoy-tests";
  delete process.env.TMUX_PANE;
});

afterEach(() => {
  if (originalNatsUrl === undefined) delete process.env.ENVOY_NATS_URL;
  else process.env.ENVOY_NATS_URL = originalNatsUrl;
  globalThis.fetch = originalFetch;
  if (originalDispatchUrl === undefined) delete process.env.DISPATCH_URL;
  else process.env.DISPATCH_URL = originalDispatchUrl;
  if (originalDispatchToken === undefined) delete process.env.DISPATCH_TOKEN;
  else process.env.DISPATCH_TOKEN = originalDispatchToken;
  if (originalDispatchTokenFile === undefined) delete process.env.DISPATCH_TOKEN_FILE;
  else process.env.DISPATCH_TOKEN_FILE = originalDispatchTokenFile;
  if (originalHome === undefined) delete process.env.HOME;
  else process.env.HOME = originalHome;
  if (originalPath === undefined) delete process.env.PATH;
  else process.env.PATH = originalPath;
  if (originalTmuxPane === undefined) delete process.env.TMUX_PANE;
  else process.env.TMUX_PANE = originalTmuxPane;
  clipboardState.copiedSessionIDs.length = 0;
  clipboardState.error = undefined;
  delete process.env.ENVOY_RESUBSCRIBE_DELAY_MS;
  natsState.connectedNames.length = 0;
  natsState.published.length = 0;
  natsState.subscriptions.clear();
  natsState.controls.clear();
  natsState.controlsByTopic.clear();
  natsState.failConnects = 0;
  natsState.drainHangs = false;
  natsState.drainStarted = false;
});

function createPi(options: { readonly clipboardError?: Error; readonly zod?: typeof z } = {}) {
  clipboardState.error = options.clipboardError;
  const commands: RegisteredCommand[] = [];
  const tools: RegisteredTool[] = [];
  const handlers = new Map<string, (event: unknown, context: SessionContext) => Promise<unknown>>();
  const renderers = new Map<string, MessageRenderer>();
  const messages: string[] = [];
  const deliveries: { readonly content: string; readonly options: unknown }[] = [];
  // Persisted custom entries, in the shape a later `getBranch()` returns them.
  const entries: {
    readonly type: "custom";
    readonly customType: string;
    readonly data: unknown;
  }[] = [];
  const pi: TestPi = {
    zod: options.zod ?? z,
    registerTool: (tool) => tools.push(tool),
    registerCommand: (name, command) => commands.push({ name, ...command }),
    registerMessageRenderer: (customType, renderer) => renderers.set(customType, renderer),
    on: (event, handler) => handlers.set(event, handler),
    sendMessage: (message, options) => {
      messages.push(message.content);
      deliveries.push({ content: message.content, options });
    },
    appendEntry: (customType, data) => {
      entries.push({ type: "custom", customType, data });
    },
  };
  return {
    commands,
    copiedSessionIDs: clipboardState.copiedSessionIDs,
    deliveries,
    entries,
    handlers,
    messages,
    pi,
    renderers,
    tools,
  };
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
  return {
    // A host command context always carries a session manager; an empty ID is
    // how it reports a session that has not been created yet.
    sessionManager: { getSessionId: () => "" },
    ui: { notify: (message) => notifications.push(message) },
  };
}

function forwardedRoleEnvelope(role: string, summary: string, dedupeKey: string) {
  return JSON.stringify({
    event_id: `evt-${dedupeKey}`,
    source: "envoy",
    source_event_id: `source-${dedupeKey}`,
    topic: `notifications.role.${role}`,
    dedupe_key: `envoy.role.forward.${dedupeKey}`,
    issued_at: 1,
    payload_summary: summary,
    trace_id: `trace-${dedupeKey}`,
  });
}

function targetedDispatchEnvelope(mode: "aside" | "btw" | "steer", dedupeKey: string): string {
  return JSON.stringify({
    dedupe_key: dedupeKey,
    event_id: `dispatch-${dedupeKey}`,
    issued_at: 1,
    payload: JSON.stringify({
      event: {
        actor: { id: "alice", kind: "user" },
        issue_key: "CORE-1",
        payload: {
          author: { id: "alice", kind: "user" },
          body: `Delivery ${mode} ${dedupeKey}`,
          created_at: "2026-09-12T00:00:00Z",
          deliveries: [],
          id: `message-${dedupeKey}`,
          in_reply_to: null,
          issue_key: "CORE-1",
          target: "session:ses_delivery",
        },
        type: "message.created",
      },
      delivery: { attempt: 1, mode },
    }),
    payload_summary: `Delivery ${mode} ${dedupeKey}`,
    source: "dispatch",
    source_event_id: "1",
    topic: "notifications.agent.ses_delivery",
    trace_id: dedupeKey,
  });
}

function response(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200 });
}

function responseWithRegistration(
  input: string | URL | Request,
  init: RequestInit | undefined,
  fallback: unknown
): Response {
  if (new URL(input.toString()).pathname !== "/v1/interests/subscribe") return response(fallback);
  const body = JSON.parse(init?.body?.toString() ?? "{}") as {
    readonly session_id: string;
    readonly dir: string;
    readonly topics: readonly string[];
  };
  return response({
    session_id: body.session_id,
    machine_id: "test",
    dir: body.dir,
    topics: body.topics,
  });
}

const dispatchToolNames = dispatchToolSpecs.map((spec) => spec.name);

test("declares all thirteen native Dispatch tools", () => {
  expect(dispatchToolNames).toHaveLength(13);
  expect(dispatchToolNames).toContain("dispatch_edit_ask");
});

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
      if (url.pathname === "/v1/roles/set") {
        const role = typeof body?.role === "string" ? body.role : "";
        return response({
          session_id: "ses_omp",
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${role}`],
        });
      }
      if (url.pathname === "/v1/interests/subscribe") {
        return response({
          session_id: "ses_omp",
          machine_id: "test",
          dir: "/tmp",
          topics: ["notifications.agent.ses_omp"],
        });
      }
      if (url.pathname === "/v1/sessions") return response([]);
      if (url.pathname === "/v1/interests/ses_omp")
        return response({ session_id: "ses_omp", machine_id: "test", dir: "/tmp", topics: [] });
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

    expect(
      fixture.tools
        .filter((tool) => tool.name.startsWith("envoy_"))
        .map((tool) => ({ name: tool.name, description: tool.description }))
    ).toEqual(envoyToolSpecs.map((spec) => ({ name: spec.name, description: spec.description })));

    await fixture.tools
      .find((tool) => tool.name === "envoy_role_set")
      ?.execute("", { role: "controller" });
    await fixture.tools.find((tool) => tool.name === "envoy_list")?.execute("", {});
    const sent = await fixture.tools
      .find((tool) => tool.name === "envoy_send")
      ?.execute("", {
        session_id: "ses_target",
        message: "direct",
        in_reply_to: "event-before",
        supersedes: "event-obsolete",
        urgency: "blocking",
        expects_reply: "required",
        expires_at: 1_788_956_000_000,
      });
    expect(sent?.content[0]?.text).toBe(
      "sent evt_1 to ses_target (recipient unconfirmed by listener)"
    );
    expect(sent?.details).toMatchObject({
      event_id: "evt_1",
      recipient: "ses_target",
      confirmed: false,
    });
    await fixture.tools
      .find((tool) => tool.name === "envoy_publish")
      ?.execute("", {
        topic: "team.test",
        message: "broadcast",
      });
    await fixture.tools.find((tool) => tool.name === "envoy_sessions")?.execute("", {});

    expect(requests).toEqual([
      // session_start registers the direct subject before tool calls.
      {
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_omp",
          dir: "/tmp/envoy-omp-test",
          topics: ["notifications.agent.ses_omp"],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
          capabilities: ["aside"],
        },
      },
      { path: "/v1/roles/set", body: { session_id: "ses_omp", role: "controller" } },
      { path: "/v1/interests/ses_omp", body: undefined },
      {
        path: "/v1/messages/send",
        body: {
          source: "agent",
          source_session: "ses_omp",
          target_session: "ses_target",
          message: "direct",
          in_reply_to: "event-before",
          supersedes: "event-obsolete",
          urgency: "blocking",
          expects_reply: "required",
          expires_at: 1_788_956_000_000,
          idempotency_key: expect.any(String),
        },
      },
      {
        path: "/v1/messages/publish",
        body: {
          source: "agent",
          source_session: "ses_omp",
          topic: "team.test",
          message: "broadcast",
          idempotency_key: expect.any(String),
        },
      },
      { path: "/v1/sessions", body: undefined },
    ]);
  });

  test("builds every tool parameter schema through the injected pi.zod, never the bundled zod", async () => {
    // OMP's converter reads internals only its own Zod produces; a field built from this
    // package's zod import registers here but fails to load in a real session. The fixture
    // normally hands the extension the same zod the contract imports, which cannot tell the
    // two apart, so this pi.zod tracks every schema it (or a chained call on one) creates.
    const built = new WeakSet<object>();
    const track = <Schema extends object>(schema: Schema): Schema => {
      const proxy = new Proxy(schema, {
        get(target, property, receiver) {
          const value: unknown = Reflect.get(target, property, receiver);
          if (typeof value !== "function") return value;
          return (...args: unknown[]) => {
            const result: unknown = Reflect.apply(value, target, args);
            return result instanceof z.ZodType ? track(result) : result;
          };
        },
      });
      built.add(proxy);
      return proxy;
    };
    const foreign: string[] = [];
    const injected = {
      ...z,
      object: (shape: z.ZodRawShape) => {
        for (const field of Object.values(shape)) {
          if (!built.has(field)) foreign.push("object field");
        }
        return track(z.object(shape));
      },
      string: () => track(z.string()),
      number: () => track(z.number()),
      boolean: () => track(z.boolean()),
      array: (item: z.ZodType) => {
        if (!built.has(item)) foreign.push("array element");
        return track(z.array(item));
      },
      enum: (values: readonly [string, ...string[]]) => track(z.enum(values)),
      unknown: () => track(z.unknown()),
    } as unknown as typeof z;
    const fixture = createPi({ zod: injected });
    process.env.DISPATCH_URL = "http://127.0.0.1:8767";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const { default: envoyExtension } = await import("./envoy.ts?injected-zod-identity");
    envoyExtension(fixture.pi);

    for (const tool of fixture.tools) {
      const parameters = tool.parameters as z.ZodObject<z.ZodRawShape>;
      if (!built.has(parameters)) foreign.push(tool.name);
      for (const [key, field] of Object.entries(parameters.shape)) {
        if (!built.has(field)) foreign.push(`${tool.name}.${key}`);
      }
    }
    expect(foreign).toEqual([]);
  });

  test("uses the shared metadata schema to reject an invalid urgency", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?shared-metadata-validation");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    const send = fixture.tools.find((tool) => tool.name === "envoy_send");
    const spec = envoyToolSpecs.find((candidate) => candidate.name === "envoy_send");
    if (send === undefined || spec === undefined) throw new Error("envoy_send was not registered");

    const input = { session_id: "ses_target", message: "direct", urgency: "urgent" };
    const expected = z.object(spec.arguments(z) as unknown as z.ZodRawShape).safeParse(input);
    const actual = (send.parameters as z.ZodType).safeParse(input);
    if (expected.success || actual.success) throw new Error("invalid urgency unexpectedly parsed");

    expect(actual.error.message).toBe(expected.error.message);
  });

  test("surfaces a missing target session as an envoy_send tool error", async () => {
    globalThis.fetch = async (input) => {
      if (new URL(input.toString()).pathname === "/v1/interests/subscribe") {
        return response({
          session_id: "ses_omp",
          machine_id: "test",
          dir: "/tmp",
          topics: ["notifications.agent.ses_omp"],
        });
      }
      return new Response(JSON.stringify({ error: "no live session ses_missing" }), {
        status: 404,
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts?missing-target-error");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const send = fixture.tools.find((tool) => tool.name === "envoy_send");
    if (send === undefined) throw new Error("envoy_send was not registered");

    const result = await send.execute("", { session_id: "ses_missing", message: "hello" });

    expect(result.isError).toBe(true);
    expect(result.content[0]?.text).toContain("no live session ses_missing");
  });

  test("subscribes to the structured Dispatch topic from a successful tool result", async () => {
    const interestRegistrations: unknown[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe" && init?.body !== undefined) {
        interestRegistrations.push(JSON.parse(String(init.body)));
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?dispatch-auto-subscribe");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    await fixture.handlers.get("tool_result")?.(
      {
        toolName: "dispatch_ask",
        toolCallId: "call_1",
        input: {},
        details: { topic: dispatchIssueSubject("LEGION-1", ">") },
        isError: false,
      },
      sessionContext()
    );

    const topic = dispatchIssueSubject("LEGION-1", ">");
    const base = `${DISPATCH_ISSUE_TOPIC_PREFIX}LEGION-1`;
    expect(natsState.controls.has(base)).toBe(true);
    expect(natsState.controls.has(topic)).toBe(true);
    const lastRegistration = interestRegistrations.at(-1) as { topics?: string[] } | undefined;
    expect(lastRegistration?.topics).toEqual(expect.arrayContaining([base, topic]));
  });

  test("tells the agent the first time a write subscribes it, and stays quiet on repeat writes to the same issue", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?dispatch-subscribe-notice");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const topic = dispatchIssueSubject("LEGION-1", ">");
    const toolResult = {
      toolName: "dispatch_ask",
      toolCallId: "call_1",
      input: {},
      details: { topic },
      isError: false,
    };

    await fixture.handlers.get("tool_result")?.(toolResult, sessionContext());
    // Model-visible, not a UI-only notification: the host never lets a
    // tool_result handler amend the result the model already saw, so this
    // goes through the same sendMessage/steer channel `deliver` uses for
    // inbound envelopes.
    expect(fixture.messages).toEqual([
      `Subscribed to LEGION-1 (every event on this issue reaches you; envoy_unsubscribe ${topic} to stop).`,
    ]);
    expect(fixture.deliveries[0]?.options).toEqual({ deliverAs: "steer", triggerTurn: false });

    await fixture.handlers.get("tool_result")?.(
      { ...toolResult, toolCallId: "call_2" },
      sessionContext()
    );
    expect(fixture.messages).toHaveLength(1);
  });

  test("rejects malformed wildcard bases before opening subscriptions", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?malformed-wildcard-base");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const subscribe = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    if (subscribe === undefined) throw new Error("subscription tool was not registered");

    for (const topic of ["a..b.>", "a.>"]) {
      const result = await subscribe.execute("", { topics: [topic] });
      expect(result.isError).toBe(true);
      expect(result.content[0]?.text).toContain("concrete base");
      expect(natsState.controls.has(topic)).toBe(false);
    }
  });

  test("a resumed session re-subscribes its persisted interests and re-claims its held role", async () => {
    const roleClaims: { readonly session_id: string; readonly role: string }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/ses_omp") {
        return response({
          session_id: "ses_omp",
          machine_id: "test",
          dir: "/tmp",
          topics: [
            "notifications.agent.ses_omp",
            "notifications.github.sjawhar.legion.issue.91.>",
            "notifications.role.legion-controller",
          ],
        });
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}");
        roleClaims.push({ session_id: body.session_id, role: body.role });
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?interest-recovery");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    const resumed = {
      ...sessionContext(),
      sessionManager: { ...sessionContext().sessionManager, getBranch: () => [{}] },
    };
    await fixture.handlers.get("session_start")?.({}, resumed);
    expect(natsState.controls.has("notifications.github.sjawhar.legion.issue.91.>")).toBe(true);
    expect(natsState.controls.has("notifications.github.sjawhar.legion.issue.91")).toBe(true);

    // Role deliveries arrive on the agent subject; a role is never a NATS
    // subscription of its own. It is a server-side claim, re-asserted so the
    // extension knows it holds one again (envoy_unsubscribe can release it).
    expect(natsState.controls.has("notifications.role.legion-controller")).toBe(false);
    expect(roleClaims).toEqual([{ session_id: "ses_omp", role: "legion-controller" }]);
    const unsubscribe = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    const result = await unsubscribe?.execute("", {
      topics: ["notifications.role.legion-controller"],
    });
    expect(result?.content[0]?.text).toContain("notifications.role.legion-controller");
  });

  test("a session rebind moves a held role from the dead id to the new one", async () => {
    const roleClaims: { readonly session_id: string; readonly role: string }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/ses_before") {
        return response({
          session_id: "ses_before",
          machine_id: "test",
          dir: "/tmp",
          topics: ["notifications.agent.ses_before", "notifications.role.pr-queue"],
        });
      }
      // The new id has no registry row yet — exactly the post-branch state.
      if (url.pathname === "/v1/interests/ses_after") return response({ error: "not found" });
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}");
        roleClaims.push({ session_id: body.session_id, role: body.role });
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-rebind");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before"));
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    await roleTool?.execute("", { role: "pr-queue" });
    expect(roleClaims).toEqual([{ session_id: "ses_before", role: "pr-queue" }]);

    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after"));

    expect(roleClaims).toEqual([
      { session_id: "ses_before", role: "pr-queue" },
      { session_id: "ses_after", role: "pr-queue" },
    ]);
  });

  test("a rebind re-claims a held role from memory when the registry rows are gone", async () => {
    const registeredSessions = new Set<string>();
    const roleClaims: { readonly session_id: string; readonly role: string }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id?: unknown;
        };
        if (typeof body.session_id === "string") registeredSessions.add(body.session_id);
        return responseWithRegistration(input, init, {});
      }
      if (url.pathname === "/v1/interests/ses_before_memory")
        return response({ error: "nats: key not found" });
      if (url.pathname === "/v1/interests/ses_after_memory")
        return response({ error: "nats: key not found" });
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id?: unknown;
          readonly role?: unknown;
        };
        if (typeof body.session_id !== "string" || typeof body.role !== "string") {
          return new Response(JSON.stringify({ error: "invalid role claim" }), { status: 400 });
        }
        if (!registeredSessions.has(body.session_id)) {
          return new Response(JSON.stringify({ error: "session is not registered" }), {
            status: 404,
          });
        }
        roleClaims.push({ session_id: body.session_id, role: body.role });
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-memory-rebind");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_memory"));
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    await roleTool?.execute("", { role: "pr-queue" });
    expect(roleClaims).toEqual([{ session_id: "ses_before_memory", role: "pr-queue" }]);

    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_memory"));

    expect(roleClaims).toEqual([
      { session_id: "ses_before_memory", role: "pr-queue" },
      { session_id: "ses_after_memory", role: "pr-queue" },
    ]);
  });

  test("new and resume switches do not carry the outgoing session role", async () => {
    const roleClaims: { readonly session_id: string; readonly role: string }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        return responseWithRegistration(input, init, {});
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}");
        roleClaims.push({ session_id: body.session_id, role: body.role });
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      if (url.pathname.startsWith("/v1/interests/")) {
        const session = url.pathname.slice("/v1/interests/".length);
        const topics =
          session === "ses_loaded"
            ? [`notifications.agent.${session}`, "notifications.role.reviewer"]
            : [`notifications.agent.${session}`];
        return response({
          session_id: session,
          machine_id: "test",
          dir: "/tmp",
          topics,
        });
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-new-resume-switch");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_outgoing"));
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    await roleTool?.execute("", { role: "pr-queue" });
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, sessionContext("ses_fresh"));
    await roleTool?.execute("", { role: "reviewer" });
    await fixture.handlers.get("session_switch")?.(
      { reason: "resume" },
      {
        ...sessionContext("ses_loaded"),
        sessionManager: {
          ...sessionContext("ses_loaded").sessionManager,
          getBranch: () => [
            { type: "custom", customType: "envoy-role-claim", data: { role: "sre" } },
          ],
        },
      }
    );

    // The role held by ses_outgoing does not follow a `new` switch, and the
    // `resume` switch claims what the loaded transcript itself recorded (sre),
    // not what the outgoing process remembered (reviewer) or what a stale
    // listener row for the loaded id says (reviewer).
    expect(roleClaims).toEqual([
      { session_id: "ses_outgoing", role: "pr-queue" },
      { session_id: "ses_fresh", role: "reviewer" },
      { session_id: "ses_loaded", role: "sre" },
    ]);
  });

  test("resume after listener reaping re-claims the role from the transcript", async () => {
    // Life 1: claim a role. The listener later reaps the dead session's rows,
    // so life 2 finds no interest row at all — only the transcript remembers.
    const roleClaims: { readonly session_id: string; readonly role: string }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/ses_reaped")
        return response({ error: "nats: key not found" });
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}");
        roleClaims.push({ session_id: body.session_id, role: body.role });
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: firstLife } = await import("./envoy.ts?role-reaped-life-1");
    const first = createPi();
    firstLife(first.pi);
    await first.handlers.get("session_start")?.({}, sessionContext("ses_reaped"));
    await first.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role: "sre" });
    expect(roleClaims).toEqual([{ session_id: "ses_reaped", role: "sre" }]);

    const { default: secondLife } = await import("./envoy.ts?role-reaped-life-2");
    const second = createPi();
    secondLife(second.pi);
    const resumed = {
      ...sessionContext("ses_reaped"),
      sessionManager: {
        ...sessionContext("ses_reaped").sessionManager,
        getBranch: () => first.entries,
      },
    };
    await second.handlers.get("session_start")?.({}, resumed);

    expect(roleClaims).toEqual([
      { session_id: "ses_reaped", role: "sre" },
      { session_id: "ses_reaped", role: "sre" },
    ]);
  });

  test("automatic reclaim is a soft claim: a live holder's 409 is honoured, an explicit claim stays hard", async () => {
    // The parent of a /fork: its transcript still records `pr-queue`, but the
    // fork moved the live claim to the child. The listener arbitrates: the
    // extension sends soft:true and the listener answers 409 with the holder.
    const roleClaims: { readonly session_id: string; readonly soft: boolean | undefined }[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/ses_fork_parent") {
        return response({ error: "nats: key not found" });
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}");
        roleClaims.push({ session_id: body.session_id, soft: body.soft });
        if (body.soft === true) {
          return Response.json(
            {
              error: "role pr-queue is held by ses_fork_child",
              role: "pr-queue",
              holder: "ses_fork_child",
            },
            { status: 409 }
          );
        }
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-no-steal");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    const resumedParent = {
      ...sessionContext("ses_fork_parent"),
      sessionManager: {
        ...sessionContext("ses_fork_parent").sessionManager,
        getBranch: () => [
          { type: "custom", customType: "envoy-role-claim", data: { role: "pr-queue" } },
        ],
      },
    };
    await fixture.handlers.get("session_start")?.({}, resumedParent);

    // One attempt, soft, refused — and the parent does not believe it holds
    // the role afterwards.
    expect(roleClaims).toEqual([{ session_id: "ses_fork_parent", soft: true }]);
    const unsubscribe = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    const refused = await unsubscribe?.execute("", { topics: ["notifications.role.pr-queue"] });
    expect(refused?.content[0]?.text).toBe("Unsubscribed: (none)");

    // The user's explicit envoy_role_set is a hard claim and takes it.
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    await roleTool?.execute("", { role: "pr-queue" });
    expect(roleClaims).toEqual([
      { session_id: "ses_fork_parent", soft: true },
      { session_id: "ses_fork_parent", soft: undefined },
    ]);
  });

  test("a role released before the process died is not re-claimed on resume", async () => {
    const roleClaims: string[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      // A stale listener row still names the role: the recorded release wins.
      if (url.pathname === "/v1/interests/ses_released")
        return response({
          session_id: "ses_released",
          machine_id: "test",
          dir: "/tmp",
          topics: ["notifications.agent.ses_released", "notifications.role.sre"],
        });
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}");
        roleClaims.push(body.role);
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: [`notifications.role.${body.role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: firstLife } = await import("./envoy.ts?role-released-life-1");
    const first = createPi();
    firstLife(first.pi);
    await first.handlers.get("session_start")?.({}, sessionContext("ses_released"));
    await first.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role: "sre" });
    await first.tools
      .find((tool) => tool.name === "envoy_unsubscribe")
      ?.execute("", { topics: ["notifications.role.sre"] });
    expect(roleClaims).toEqual(["sre"]);

    const { default: secondLife } = await import("./envoy.ts?role-released-life-2");
    const second = createPi();
    secondLife(second.pi);
    const resumed = {
      ...sessionContext("ses_released"),
      sessionManager: {
        ...sessionContext("ses_released").sessionManager,
        getBranch: () => first.entries,
      },
    };
    await second.handlers.get("session_start")?.({}, resumed);

    expect(roleClaims).toEqual(["sre"]);
  });

  test("does not subscribe when a tool result is an error or has no Dispatch topic", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?dispatch-auto-subscribe-negative");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    await fixture.handlers.get("tool_result")?.(
      {
        toolName: "dispatch_ask",
        toolCallId: "failed",
        input: {},
        details: { topic: dispatchIssueSubject("LEGION-1", ">") },
        isError: true,
      },
      sessionContext()
    );
    await fixture.handlers.get("tool_result")?.(
      {
        toolName: "dispatch_read",
        toolCallId: "read",
        input: {},
        details: { issue: "LEGION-1" },
        isError: false,
      },
      sessionContext()
    );

    expect(natsState.controls.has(dispatchIssueSubject("LEGION-1", ">"))).toBe(false);
  });

  test("registers every shared Dispatch tool when URL and token are available", async () => {
    process.env.DISPATCH_URL = "http://127.0.0.1:8767";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const { default: envoyExtension } = await import("./envoy.ts?native-dispatch-registration");
    const fixture = createPi();

    envoyExtension(fixture.pi);

    expect(fixture.tools.map((tool) => tool.name)).toEqual(
      expect.arrayContaining([...envoyToolSpecs.map((spec) => spec.name), ...dispatchToolNames])
    );
    expect(fixture.tools).toHaveLength(envoyToolSpecs.length + dispatchToolSpecs.length);
  });

  test("executes dispatch_ask with the calling session identity and returns its structured result", async () => {
    process.env.DISPATCH_URL = "http://127.0.0.1:8767";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const requests: Array<{ readonly url: URL; readonly init: RequestInit | undefined }> = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      requests.push({ url, init });
      return new Response(
        JSON.stringify({ id: "ask_1", issue_key: "LEGION-1", question: "Should we ship B3?" }),
        { headers: { "content-type": "application/json" } }
      );
    };
    const { default: envoyExtension } = await import("./envoy.ts?native-dispatch-ask");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    const ask = fixture.tools.find((candidate) => candidate.name === "dispatch_ask");
    if (ask === undefined) throw new Error("dispatch_ask was not registered");
    const context = {
      ...sessionContext("ses_live"),
      sessionManager: { getSessionId: () => "ses_live", getSessionName: () => "current title" },
    };

    const result = await ask.execute(
      "call_ask",
      { issue: "LEGION-1", question: "Should we ship B3?" },
      undefined,
      undefined,
      context
    );

    expect(result).toEqual({
      content: [{ type: "text", text: "Opened ask ask_1: Should we ship B3?" }],
      details: {
        issue: "LEGION-1",
        topic: dispatchIssueSubject("LEGION-1", ">"),
        ask: "ask_1",
      },
    });
    expect(requests).toHaveLength(1);
    expect(requests[0]?.url.pathname).toBe("/api/v1/issues/LEGION-1/asks");
    expect(new Headers(requests[0]?.init?.headers).get("authorization")).toBe(
      "Bearer dispatch-token"
    );
    expect(JSON.parse(String(requests[0]?.init?.body))).toMatchObject({
      question: "Should we ship B3?",
      actor: {
        kind: "session",
        id: "ses_live",
        origin: { host: "omp", cwd: "/tmp/envoy-omp-test", session_title: "current title" },
      },
    });
  });

  test("a dispatch tool call follows a changed server URL without reloading the extension", async () => {
    process.env.DISPATCH_URL = "http://127.0.0.1:8767";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const hosts: string[] = [];
    globalThis.fetch = async (input) => {
      hosts.push(new URL(input.toString()).host);
      return new Response(JSON.stringify({ id: "ask_1", issue_key: "LEGION-1", question: "Q?" }), {
        headers: { "content-type": "application/json" },
      });
    };
    // Every test in this block imports its own extension instance (query-string variants), the
    // file's established way to get a fresh load-time state per test.
    const { default: envoyExtension } = await import("./envoy.ts?native-dispatch-moved");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    const ask = fixture.tools.find((candidate) => candidate.name === "dispatch_ask");
    if (ask === undefined) throw new Error("dispatch_ask was not registered");
    const context = {
      ...sessionContext("ses_live"),
      sessionManager: { getSessionId: () => "ses_live", getSessionName: () => "t" },
    };

    await ask.execute(
      "call_1",
      { issue: "LEGION-1", question: "Q?" },
      undefined,
      undefined,
      context
    );
    // Dispatch moved: the same live session must reach the new host on its next call.
    process.env.DISPATCH_URL = "http://dispatch.moved.test:9000";
    await ask.execute(
      "call_2",
      { issue: "LEGION-1", question: "Q?" },
      undefined,
      undefined,
      context
    );

    expect(hosts).toEqual(["127.0.0.1:8767", "dispatch.moved.test:9000"]);

    // A configuration that has since broken fails the call with its own reason,
    // never with a request to the stale endpoint.
    delete process.env.DISPATCH_TOKEN;
    const failed = await ask.execute(
      "call_3",
      { issue: "LEGION-1", question: "Q?" },
      undefined,
      undefined,
      context
    );
    expect(failed.isError).toBe(true);
    expect(hosts).toHaveLength(2);
  });

  test("executes dispatch_search without an issue and returns rows in details", async () => {
    process.env.DISPATCH_URL = "http://127.0.0.1:8767";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const requests: Array<{ readonly url: URL; readonly init: RequestInit | undefined }> = [];
    const results = [
      {
        kind: "document",
        issue: { key: "LEGION-2", title: "Astrolabe", status: "triage" },
        artifact: { slug: "spec", name: "spec.md" },
        id: "artifact-2",
        snippet: "<mark>astrolabe</mark>",
        rank: 1,
        href: "/issues/LEGION-2/spec?q=astrolabe",
      },
    ];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      requests.push({ url, init });
      if (url.pathname !== "/api/v1/search") throw new Error(`unexpected request: ${url.pathname}`);
      return new Response(JSON.stringify({ results, took_ms: 7 }), {
        headers: { "Content-Type": "application/json" },
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts?native-dispatch-search");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    const search = fixture.tools.find((candidate) => candidate.name === "dispatch_search");
    if (search === undefined) throw new Error("dispatch_search was not registered");

    const result = await search.execute(
      "call_search",
      { query: "astrolabe" },
      undefined,
      undefined,
      sessionContext("ses_live")
    );

    expect(requests).toHaveLength(1);
    expect(requests[0]?.url.pathname).toBe("/api/v1/search");
    expect(requests[0]?.url.searchParams.get("q")).toBe("astrolabe");
    expect(new Headers(requests[0]?.init?.headers).get("authorization")).toBe(
      "Bearer dispatch-token"
    );
    expect(result.content).toEqual([
      {
        type: "text",
        text: '1 result for "astrolabe" (7 ms)\nLEGION-2 [triage] Astrolabe - document spec.md: **astrolabe** -> http://127.0.0.1:8767/issues/LEGION-2/spec?q=astrolabe',
      },
    ]);
    expect(result.isError).toBeUndefined();
    expect(result.details).toEqual({ query: "astrolabe", results });
    expect(result.details).not.toHaveProperty("topic");
  });
  test("does not register Dispatch tools and reports the missing token once at session start", async () => {
    process.env.DISPATCH_URL = "http://127.0.0.1:8767";
    delete process.env.DISPATCH_TOKEN;
    process.env.HOME = "/nonexistent-home-for-dispatch-gating";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?native-dispatch-disabled");
    const fixture = createPi();
    envoyExtension(fixture.pi);

    expect(fixture.tools.map((tool) => tool.name)).toEqual(envoyToolSpecs.map((spec) => spec.name));
    const notifications: string[] = [];
    await fixture.handlers.get("session_start")?.(
      {},
      { ...sessionContext(), ui: { notify: (message) => notifications.push(message) } }
    );
    expect(
      notifications.filter((message) => message.startsWith("envoy: dispatch tool disabled — "))
    ).toEqual(["envoy: dispatch tool disabled — dispatch.token must be a non-empty bearer token"]);
  });

  test("envoy_sessions rejects a non-string machine filter before calling Envoy", async () => {
    const requests: string[] = [];
    globalThis.fetch = async (input) => {
      requests.push(new URL(input.toString()).pathname);
      return response([
        {
          session_id: "ses_other",
          machine_id: "other-machine",
          dir: "/tmp",
          port: 0,
          title: "",
          topics: [],
          self_subscribed: true,
        },
      ]);
    };
    const { default: envoyExtension } = await import("./envoy.ts?invalid-session-machine");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    const sessionsTool = fixture.tools.find((tool) => tool.name === "envoy_sessions");
    if (sessionsTool === undefined) throw new Error("envoy_sessions was not registered");

    const result = await sessionsTool.execute("", { machine: 42 });

    expect(result.isError).toBe(true);
    expect(result.content[0]?.text).toBe("machine must be a string");
    expect(requests).toEqual([]);
  });

  test("injects a structured envelope payload as a TOON block that round-trips to the payload", async () => {
    const payload = {
      failed: [
        { check: "typecheck", conclusion: "failure" },
        { check: "unit", conclusion: "failure" },
      ],
      kind: "checks",
      number: 42,
    };
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?toon-structured-payload");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(
      JSON.stringify({
        event_id: "evt-toon-structured",
        source: "envoy",
        source_event_id: "source-toon-structured",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "envoy.agent.toon-structured",
        issued_at: 1,
        payload_summary: "CI failures",
        payload: JSON.stringify(payload),
        trace_id: "trace-toon-structured",
      })
    );
    await injected.promise;

    const content = fixture.messages[0];
    if (content === undefined) throw new Error("structured delivery was not injected");
    expect(decode(content)).toEqual({
      envoy: {
        to: "you (ses_…)",
        from: "envoy",
        at: "1970-01-01T00:00:00Z",
        id: "evt-toon-structured",
        summary: "CI failures",
        message: payload,
      },
    });
  });

  test("injects a non-JSON envelope payload as raw text without serializing its envelope", async () => {
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?toon-plain-text-payload");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(
      JSON.stringify({
        event_id: "evt-toon-plain-text",
        source: "envoy",
        source_event_id: "source-toon-plain-text",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "envoy.agent.toon-plain-text",
        issued_at: 1,
        payload_summary: "plain text",
        payload: "plain-text payload",
        trace_id: "trace-toon-plain-text",
      })
    );
    await injected.promise;

    const content = fixture.messages[0];
    if (content === undefined) throw new Error("plain-text delivery was not injected");
    expect(decode(content)).toEqual({
      envoy: {
        to: "you (ses_…)",
        from: "envoy",
        at: "1970-01-01T00:00:00Z",
        id: "evt-toon-plain-text",
        summary: "plain text",
        message: "plain-text payload",
      },
    });
  });

  test("names the sender and injects a reply instruction for a peer agent message", async () => {
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?sender-reply-hint");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(
      JSON.stringify({
        event_id: "evt-peer-message",
        source: "agent",
        source_session: "ses_peer",
        source_event_id: "source-peer-message",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "envoy.agent.peer-message",
        issued_at: 1,
        payload_summary: "hello from a peer",
        trace_id: "trace-peer-message",
      })
    );
    await injected.promise;
    const content = fixture.messages[0];
    if (content === undefined) throw new Error("peer delivery was not injected");
    expect(decode(content)).toEqual({
      envoy: {
        to: "you (ses_…)",
        from: "ses_peer",
        at: "1970-01-01T00:00:00Z",
        id: "evt-peer-message",
        reply_with: 'envoy_send(session_id="ses_peer", message="...")',
        summary: "hello from a peer",
      },
    });
  });

  test("renders a human envelope without reply metadata", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?human-rendering");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(
      JSON.stringify({
        event_id: "evt-human-message",
        source: "human",
        source_event_id: "human-message",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "human.agent.message",
        issued_at: 1,
        payload_summary: "please review this",
        trace_id: "trace-human-message",
      })
    );
    await injected.promise;

    const content = fixture.messages[0];
    if (content === undefined) throw new Error("human delivery was not injected");
    expect(decode(content)).toEqual({
      envoy: {
        to: "you (ses_…)",
        from: "human",
        at: "1970-01-01T00:00:00Z",
        id: "evt-human-message",
        summary: "please review this",
      },
    });
  });

  test("renders a message this session sent to itself as an ordinary delivery", async () => {
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?self-echo");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(
      JSON.stringify({
        event_id: "evt-self-echo",
        source: "agent",
        source_session: "ses_omp",
        source_event_id: "source-self-echo",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "envoy.agent.self-echo",
        issued_at: 1,
        payload_summary: "note to self",
        trace_id: "trace-self-echo",
      })
    );
    await injected.promise;
    const content = fixture.messages[0];
    if (content === undefined) throw new Error("echo delivery was not injected");
    const note = decode(content) as { envoy: Record<string, unknown> };
    expect(note.envoy).toEqual({
      to: "you (ses_…)",
      from: "ses_omp",
      at: "1970-01-01T00:00:00Z",
      id: "evt-self-echo",
      reply_with: 'envoy_send(session_id="ses_omp", message="...")',
      summary: "note to self",
    });
  });
  test("deduplicates a dispatch event without suppressing a later envelope", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?dispatch-echo");
    const fixture = createPi();
    const afterEcho = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (message.content.includes("new message")) afterEcho.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    agent.push(
      JSON.stringify({
        event_id: "evt-dispatch-echo",
        source: "github",
        source_event_id: "github-dispatch-echo",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "github.dispatch.echo",
        issued_at: 1,
        payload_summary: "Keep the thread open?",
        payload: JSON.stringify({}),
        trace_id: "trace-dispatch-echo",
      })
    );
    agent.push(
      JSON.stringify({
        event_id: "evt-dispatch-later-copy",
        source: "github",
        source_event_id: "github-dispatch-later-copy",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "github.dispatch.echo",
        issued_at: 1,
        payload_summary: "Keep the thread open?",
        payload: JSON.stringify({}),
        trace_id: "trace-dispatch-later-copy",
      })
    );
    agent.push(
      JSON.stringify({
        event_id: "evt-after-dispatch-echo",
        source: "github",
        source_event_id: "github-after-dispatch-echo",
        topic: "notifications.agent.ses_omp",
        dedupe_key: "github.after-dispatch-echo",
        issued_at: 1,
        payload_summary: "new message",
        trace_id: "trace-after-dispatch-echo",
      })
    );
    await afterEcho.promise;

    expect(fixture.messages).toHaveLength(2);
    expect(fixture.messages[0]).toContain("Keep the thread open?");
    expect(fixture.messages[1]).toContain("new message");
  });

  test("never exposes a malformed envelope frame", async () => {
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?toon-malformed-envelope");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push("{this is not JSON");
    await injected.promise;

    const content = fixture.messages[0];
    if (content === undefined) throw new Error("malformed delivery was not injected");
    expect(decode(content)).toEqual({
      envoy: { topic: "notifications.agent.ses_omp", unrecognised: "payload was not JSON" },
    });
    expect(content).not.toContain("{this is not JSON");
  });

  test("injects a role event forwarded to the claimed agent subject with original role-topic framing", async () => {
    let currentRoleTopic = "";
    const requests: string[] = [];
    const unregistrations: (readonly string[])[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
      requests.push(url.pathname);
      if (url.pathname === "/v1/roles/set") {
        const role = typeof body?.role === "string" ? body.role : "";
        currentRoleTopic = `notifications.role.${role}`;
        return response({
          session_id: "ses_role_a",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [currentRoleTopic],
        });
      }
      if (url.pathname === "/v1/interests/unsubscribe") {
        const topics = Array.isArray(body?.topics)
          ? body.topics.filter((topic): topic is string => typeof topic === "string")
          : [];
        unregistrations.push(topics);
        return response({});
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-agent-delivery");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_role_a"));
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    const unsubscribeTool = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    if (roleTool === undefined || unsubscribeTool === undefined)
      throw new Error("role tools were not registered");
    await roleTool.execute("", { role: "legion-controller" });

    expect(natsState.controls.get(currentRoleTopic)).toBeUndefined();
    const agent = natsState.controls.get("notifications.agent.ses_role_a");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(forwardedRoleEnvelope("legion-controller", "role message", "role-agent-delivery"));
    await injected.promise;
    const roleDelivery = fixture.messages[0];
    if (roleDelivery === undefined) throw new Error("role delivery was not injected");
    expect(decode(roleDelivery)).toEqual({
      envoy: {
        from: "envoy",
        at: "1970-01-01T00:00:00Z",
        id: "evt-role-agent-delivery",
        summary: "role message",
      },
    });
    expect(requests).not.toContain("/v1/sessions");

    await roleTool.execute("", { role: "legion-reviewer" });
    expect(unregistrations).toEqual([["notifications.role.legion-controller"]]);
    await unsubscribeTool.execute("", { topics: ["notifications.role.legion-reviewer"] });
    expect(unregistrations).toEqual([
      ["notifications.role.legion-controller"],
      ["notifications.role.legion-reviewer"],
    ]);
  });

  test("injects a post-takeover forwarded role event into B only", async () => {
    const role = "legion-controller";
    const roleTopic = `notifications.role.${role}`;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
      if (url.pathname === "/v1/roles/set") {
        const holder = typeof body?.session_id === "string" ? body.session_id : "";
        return response({
          session_id: holder,
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [roleTopic],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtensionA } = await import("./envoy.ts?role-agent-takeover-a");
    const fixtureA = createPi();
    envoyExtensionA(fixtureA.pi);
    await fixtureA.handlers.get("session_start")?.({}, sessionContext("ses_role_a"));
    const roleToolA = fixtureA.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleToolA === undefined) throw new Error("role tool was not registered for session A");
    await roleToolA.execute("", { role });

    const { default: envoyExtensionB } = await import("./envoy.ts?role-agent-takeover-b");
    const fixtureB = createPi();
    const injectedB = Promise.withResolvers<void>();
    envoyExtensionB({
      ...fixtureB.pi,
      sendMessage: (message, options) => {
        fixtureB.pi.sendMessage(message, options);
        injectedB.resolve();
      },
    });
    await fixtureB.handlers.get("session_start")?.({}, sessionContext("ses_role_b"));
    const roleToolB = fixtureB.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleToolB === undefined) throw new Error("role tool was not registered for session B");
    await roleToolB.execute("", { role });

    expect(natsState.controls.get(roleTopic)).toBeUndefined();
    const agentB = natsState.controls.get("notifications.agent.ses_role_b");
    if (agentB === undefined) throw new Error("B agent subject was not subscribed");
    agentB.push(forwardedRoleEnvelope(role, "post-takeover role event", "role-agent-takeover"));
    await injectedB.promise;
    expect(fixtureA.messages.some((message) => message.includes("post-takeover role event"))).toBe(
      false
    );
    const takeoverDelivery = fixtureB.messages[0];
    if (takeoverDelivery === undefined) throw new Error("takeover delivery was not injected");
    expect(decode(takeoverDelivery)).toEqual({
      envoy: {
        from: "envoy",
        at: "1970-01-01T00:00:00Z",
        id: "evt-role-agent-takeover",
        summary: "post-takeover role event",
      },
    });
  });

  test("keeps forwarded role delivery live while the listener HTTP API is unavailable", async () => {
    const role = "legion-controller";
    let listenerAvailable = true;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/roles/set") {
        return response({
          session_id: "ses_role_a",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      if (!listenerAvailable) throw new Error("listener API unavailable");
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-agent-listener-down");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_role_a"));
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleTool === undefined) throw new Error("role tool was not registered");
    await roleTool.execute("", { role });

    const agent = natsState.controls.get("notifications.agent.ses_role_a");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    listenerAvailable = false;
    agent.push(
      forwardedRoleEnvelope(role, "role event while listener is down", "role-agent-listener-down")
    );
    await injected.promise;
    expect(
      fixture.messages.some((message) => message.includes("role event while listener is down"))
    ).toBe(true);
  });

  test("a role claim keeps the existing registration fresh", async () => {
    const registrations: {
      readonly session_id: string;
      readonly self_subscribed: boolean;
      readonly topics: readonly string[];
    }[] = [];
    const heartbeatRegistration = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/roles/set") {
        return response({
          session_id: "ses_role_heartbeat",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: ["notifications.role.legion-controller"],
        });
      }
      if (url.pathname === "/v1/roles/legion-controller") {
        return response({ role: "legion-controller", holder: "ses_role_heartbeat", last_seen: 1 });
      }
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id: string;
          readonly self_subscribed: boolean;
          readonly topics: readonly string[];
        };
        registrations.push(body);
        if (registrations.length === 2) heartbeatRegistration.resolve();
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: body.topics,
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension } = await import("./envoy.ts?role-claim-heartbeat");
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_role_heartbeat"),
      setInterval: (callback) => intervals.push(callback),
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    expect(intervals).toHaveLength(1);
    expect(registrations).toHaveLength(1);
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleTool === undefined) throw new Error("role tool was not registered");
    await roleTool.execute("", { role: "legion-controller" });

    intervals[0]?.();
    await heartbeatRegistration.promise;
    expect(registrations).toHaveLength(2);
    expect(registrations[1]).toMatchObject({
      session_id: "ses_role_heartbeat",
      self_subscribed: true,
      topics: ["notifications.agent.ses_role_heartbeat"],
    });
  });

  test("a heartbeat tick re-asserts a held role the listener no longer holds, softly and without a transcript entry", async () => {
    const role = "legion-controller";
    let listenerHoldsClaim = true;
    const roleClaims: Record<string, unknown>[] = [];
    const roleReads: string[] = [];
    const reasserted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        roleReads.push(url.pathname);
        if (!listenerHoldsClaim) {
          return Response.json({ error: `no holder for role ${role}` }, { status: 404 });
        }
        return response({ role, holder: "ses_reassert", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>;
        roleClaims.push(body);
        listenerHoldsClaim = true;
        if (body.soft === true) reasserted.resolve();
        return response({
          session_id: "ses_reassert",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension, onEnvoyRoleRegained } = await import(
      "./envoy.ts?heartbeat-reassert"
    );
    const regained: { readonly role: string; readonly reason: string }[] = [];
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_reassert"),
      setInterval: (callback) => intervals.push(callback),
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    const roleTool = fixture.tools.find((tool) => tool.name === "envoy_role_set");
    if (roleTool === undefined) throw new Error("role tool was not registered");
    await roleTool.execute("", { role });
    expect(roleClaims).toEqual([{ session_id: "ses_reassert", role }]);

    // The listener drops the claim out from under the session (a reaped claim, NATS data
    // loss, an older listener build). Nothing in this process notices until the heartbeat.
    listenerHoldsClaim = false;
    intervals[0]?.();
    await reasserted.promise;
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(roleReads).toEqual([`/v1/roles/${role}`]);
    expect(roleClaims).toEqual([
      { session_id: "ses_reassert", role },
      { session_id: "ses_reassert", role, soft: true },
    ]);
    // The held role did not change, so the durable record is not rewritten.
    expect(fixture.entries.filter((entry) => entry.customType === "envoy-role-claim")).toEqual([
      { type: "custom", customType: "envoy-role-claim", data: { role } },
    ]);
    expect(regained).toEqual([{ role, reason: "reclaimed" }]);
  });

  test("heartbeat ticks while this session is the live holder issue no claim and append no transcript entry", async () => {
    const role = "legion-controller";
    const roleClaims: unknown[] = [];
    let roleReads = 0;
    let registrations = 0;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        roleReads += 1;
        return response({ role, holder: "ses_quiet", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        roleClaims.push(JSON.parse(init?.body?.toString() ?? "{}"));
        return response({
          session_id: "ses_quiet",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      if (url.pathname === "/v1/interests/subscribe") registrations += 1;
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension, onEnvoyRoleRegained } = await import(
      "./envoy.ts?heartbeat-quiet"
    );
    const regained: unknown[] = [];
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_quiet"),
      setInterval: (callback) => intervals.push(callback),
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });
    const claimEntries = () =>
      fixture.entries.filter((entry) => entry.customType === "envoy-role-claim");
    expect(claimEntries()).toHaveLength(1);
    expect(registrations).toBe(1);

    for (let tick = 0; tick < 3; tick += 1) {
      intervals[0]?.();
      await new Promise((resolve) => setTimeout(resolve, 10));
    }

    expect(registrations).toBe(4);
    expect(roleReads).toBe(3);
    expect(roleClaims).toEqual([{ session_id: "ses_quiet", role }]);
    expect(claimEntries()).toHaveLength(1);
    expect(regained).toEqual([]);
  });

  test("a 409 on re-assertion drops the local claim, warns once, and ends re-assertion for that role", async () => {
    const role = "pr-queue";
    const roleClaims: Record<string, unknown>[] = [];
    let roleReads = 0;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        roleReads += 1;
        // A newer live session took the role (a fork child, a second controller).
        return response({ role, holder: "ses_newer", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>;
        roleClaims.push(body);
        if (body.soft === true) {
          return Response.json(
            { error: `role ${role} is held by ses_newer`, role, holder: "ses_newer" },
            { status: 409 }
          );
        }
        return response({
          session_id: "ses_refused",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension, onEnvoyRoleRegained } = await import(
      "./envoy.ts?heartbeat-409"
    );
    const regained: unknown[] = [];
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const refused = Promise.withResolvers<void>();
    const context: SessionContext = {
      ...sessionContext("ses_refused"),
      setInterval: (callback) => intervals.push(callback),
      ui: {
        notify: (message) => {
          notifications.push(message);
          if (message.includes("is now held by")) refused.resolve();
        },
      },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });

    intervals[0]?.();
    await refused.promise;
    await new Promise((resolve) => setTimeout(resolve, 0));
    // Two more ticks: re-registration continues, re-assertion of this role does not.
    for (let tick = 0; tick < 2; tick += 1) {
      intervals[0]?.();
      await new Promise((resolve) => setTimeout(resolve, 10));
    }

    expect(roleClaims).toEqual([
      { session_id: "ses_refused", role },
      { session_id: "ses_refused", role, soft: true },
    ]);
    expect(roleReads).toBe(1);
    expect(notifications.filter((message) => message.includes("is now held by"))).toEqual([
      `envoy: role ${role} is now held by session ses_newer; this session no longer holds it`,
    ]);
    expect(regained).toEqual([]);
    // The local claim is gone: an unsubscribe of "everything" finds no role left to release …
    const unsubscribe = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    const released = await unsubscribe?.execute("", {});
    expect(released?.content[0]?.text).toBe("Unsubscribed: (none)");
    // … and no release entry was written: the 409 mirrors setEnvoyRole's refused soft reclaim,
    // so a later resume still lets the listener arbitrate from the transcript.
    expect(fixture.entries.filter((entry) => entry.customType === "envoy-role-claim")).toEqual([
      { type: "custom", customType: "envoy-role-claim", data: { role } },
    ]);
  });

  test("the first healthy heartbeat after a registry outage fires the regain hook even though the claim survived", async () => {
    // A listener restart that outlasts the envoy_sessions TTL: the durable claim is intact, but
    // until this session re-registers the listener answers "no holder" for it, and the daemon's
    // publishes in that window went undelivered. The re-registration itself is the regain.
    const role = "legion-controller";
    let registryDown = false;
    const roleClaims: unknown[] = [];
    globalThis.fetch = async (input, init) => {
      if (registryDown) throw new Error("network unreachable");
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        return response({ role, holder: "ses_outage", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        roleClaims.push(JSON.parse(init?.body?.toString() ?? "{}"));
        return response({
          session_id: "ses_outage",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension, onEnvoyRoleRegained } = await import(
      "./envoy.ts?heartbeat-outage-regain"
    );
    const regained: { readonly role: string; readonly reason: string }[] = [];
    const hooked = Promise.withResolvers<void>();
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
      hooked.resolve();
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const warned = Promise.withResolvers<void>();
    const context: SessionContext = {
      ...sessionContext("ses_outage"),
      setInterval: (callback) => intervals.push(callback),
      ui: {
        notify: (message) => {
          notifications.push(message);
          if (message.includes("registry heartbeat failed")) warned.resolve();
        },
      },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });

    registryDown = true;
    intervals[0]?.();
    await warned.promise;
    // Let the failed tick's own `.finally` release the healing guard before the next tick.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(regained).toEqual([]);

    registryDown = false;
    intervals[0]?.();
    await hooked.promise;
    expect(regained).toEqual([{ role, reason: "reregistered" }]);
    // The claim survived: only the explicit hard claim ever went out.
    expect(roleClaims).toEqual([{ session_id: "ses_outage", role }]);

    // The following healthy tick is quiet again.
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(regained).toHaveLength(1);
    expect(
      notifications.filter((message) => message.includes("registry heartbeat failed"))
    ).toHaveLength(1);
  });

  test("a regain hook that never settles does not block the next heartbeat tick's registration", async () => {
    // The hook is legion.ts's daemon round-trip (/controller/ready drains held notices and forces
    // a resync), which this side cannot bound. The heartbeat's healing latch must release once
    // registration and the claim are settled, or a stuck hook would stop re-registration and
    // let the session's registry entry lapse.
    const role = "legion-controller";
    let listenerHoldsClaim = true;
    let registrations = 0;
    const thirdRegistration = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        if (!listenerHoldsClaim) {
          return Response.json({ error: `no holder for role ${role}` }, { status: 404 });
        }
        return response({ role, holder: "ses_hung_hook", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        listenerHoldsClaim = true;
        return response({
          session_id: "ses_hung_hook",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      if (url.pathname === "/v1/interests/subscribe") {
        registrations += 1;
        if (registrations === 3) thirdRegistration.resolve();
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension, onEnvoyRoleRegained } = await import(
      "./envoy.ts?heartbeat-hung-hook"
    );
    let hookCalls = 0;
    onEnvoyRoleRegained(async () => {
      hookCalls += 1;
      await Promise.withResolvers<void>().promise;
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_hung_hook"),
      setInterval: (callback) => intervals.push(callback),
      ui: { notify: (message) => notifications.push(message) },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });
    expect(registrations).toBe(1);

    listenerHoldsClaim = false;
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(hookCalls).toBe(1);
    expect(registrations).toBe(2);

    // The hook is still pending. The next tick must still register.
    intervals[0]?.();
    await thirdRegistration.promise;
    expect(registrations).toBe(3);
    expect(hookCalls).toBe(1);
    expect(notifications).toEqual([]);
  });

  test("a failed role read does not make the next healthy heartbeat report a regain", async () => {
    // Only a lapsed registration can leave the listener answering "no holder" for a claim that
    // survived; a role read that fails after a successful registration is a plain heartbeat
    // error and must not trigger a redundant daemon ready call on the following tick.
    const role = "legion-controller";
    let roleReadBroken = false;
    const roleClaims: unknown[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        // A body the client cannot parse fails immediately, without the transport's 5xx retry.
        if (roleReadBroken) return response({});
        return response({ role, holder: "ses_role_read", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        roleClaims.push(JSON.parse(init?.body?.toString() ?? "{}"));
        return response({
          session_id: "ses_role_read",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension, onEnvoyRoleRegained } = await import(
      "./envoy.ts?heartbeat-role-read-failure"
    );
    const regained: unknown[] = [];
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const warned = Promise.withResolvers<void>();
    const context: SessionContext = {
      ...sessionContext("ses_role_read"),
      setInterval: (callback) => intervals.push(callback),
      ui: {
        notify: (message) => {
          notifications.push(message);
          if (message.includes("registry heartbeat failed")) warned.resolve();
        },
      },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });

    roleReadBroken = true;
    intervals[0]?.();
    await warned.promise;
    await new Promise((resolve) => setTimeout(resolve, 0));

    roleReadBroken = false;
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(regained).toEqual([]);
    expect(roleClaims).toEqual([{ session_id: "ses_role_read", role }]);
    expect(
      notifications.filter((message) => message.includes("registry heartbeat failed"))
    ).toHaveLength(1);
  });

  test("a failed registration's regain survives a role-read failure on the recovery tick", async () => {
    // Tick N: registration fails (the listener may now answer "no holder" for this session).
    // Tick N+1: registration lands but the role read errors. Tick N+2: healthy. The regain owed
    // since tick N must still fire exactly once at N+2, or the daemon's held work stays stranded.
    const role = "legion-controller";
    let registryDown = false;
    let roleReadBroken = false;
    let registrations = 0;
    const roleClaims: unknown[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        if (registryDown) throw new Error("network unreachable");
        registrations += 1;
      }
      if (url.pathname === `/v1/roles/${role}`) {
        // A body the client cannot parse fails immediately, without the transport's 5xx retry.
        if (roleReadBroken) return response({});
        return response({ role, holder: "ses_late_regain", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        roleClaims.push(JSON.parse(init?.body?.toString() ?? "{}"));
        return response({
          session_id: "ses_late_regain",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension, onEnvoyRoleRegained } = await import(
      "./envoy.ts?heartbeat-late-regain"
    );
    const regained: { readonly role: string; readonly reason: string }[] = [];
    const hooked = Promise.withResolvers<void>();
    onEnvoyRoleRegained(async (regainedRole: string, reason: string) => {
      regained.push({ role: regainedRole, reason });
      hooked.resolve();
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const warned = Promise.withResolvers<void>();
    const context: SessionContext = {
      ...sessionContext("ses_late_regain"),
      setInterval: (callback) => intervals.push(callback),
      ui: {
        notify: (message) => {
          if (message.includes("registry heartbeat failed")) warned.resolve();
        },
      },
    };
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });
    expect(registrations).toBe(1);

    registryDown = true;
    intervals[0]?.();
    await warned.promise;
    await new Promise((resolve) => setTimeout(resolve, 0));

    registryDown = false;
    roleReadBroken = true;
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(registrations).toBe(2);
    expect(regained).toEqual([]);

    roleReadBroken = false;
    intervals[0]?.();
    await hooked.promise;
    // The hook fires detached; let the tick's own chain release the healing latch.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(regained).toEqual([{ role, reason: "reregistered" }]);
    // The claim survived throughout: only the explicit hard claim ever went out.
    expect(roleClaims).toEqual([{ session_id: "ses_late_regain", role }]);

    // A further healthy tick adds nothing.
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(registrations).toBe(4);
    expect(regained).toHaveLength(1);
  });

  test("registers a self-subscribed interest on session start", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    globalThis.fetch = async (input, init) => {
      requests.push({
        path: new URL(input.toString()).pathname,
        body: init?.body === undefined ? undefined : JSON.parse(init.body.toString()),
      });
      return response({ session_id: "ses_register", machine_id: "test", dir: "/tmp", topics: [] });
    };
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
        capabilities: ["aside"],
      },
    });
  });
  test("answers a targeted BTW delivery ephemerally and registers the capability", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const registrations: unknown[] = [];
    const replies: unknown[] = [];
    const replyPosted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const path = new URL(input.toString()).pathname;
      if (path === "/v1/interests/subscribe") {
        registrations.push(JSON.parse(init?.body?.toString() ?? "{}"));
      }
      if (path === "/api/v1/messages/message-1/reply") {
        replies.push(JSON.parse(init?.body?.toString() ?? "{}"));
        replyPosted.resolve();
      }
      return response({
        session_id: "ses_target",
        machine_id: "test",
        dir: "/tmp",
        topics: ["notifications.agent.ses_target"],
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts?targeted-btw");
    const fixture = createPi();
    const asked: string[] = [];

    envoyExtension({
      ...fixture.pi,
      askEphemeral: async ({ prompt }) => {
        asked.push(prompt);
        return { replyText: "Yes, ship it." };
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_target"));
    const agent = natsState.controls.get("notifications.agent.ses_target");
    if (agent === undefined) throw new Error("agent subject was not subscribed");
    agent.push(
      JSON.stringify({
        event_id: "dispatch-btw",
        source: "dispatch",
        source_event_id: "1",
        topic: "notifications.agent.ses_target",
        dedupe_key: "dispatch-btw",
        issued_at: 1,
        payload_summary: "Can this ship?",
        payload: targetedDispatchPayload,
        trace_id: "dispatch-btw",
      })
    );
    await replyPosted.promise;

    expect(asked).toEqual(["Can this ship?"]);
    expect(fixture.deliveries).toEqual([]);
    expect(replies).toEqual([
      {
        actor: { kind: "session", id: "ses_target" },
        attempt: 1,
        body: "Yes, ship it.",
      },
    ]);
    expect(registrations).toMatchObject([{ capabilities: ["aside", "btw"] }]);
  });

  test("delivers targeted aside and steer frames through their requested primary-turn modes", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?targeted-modes");
    const fixture = createPi();
    const delivered = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (fixture.deliveries.length === 2) delivered.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_delivery"));
    const agent = natsState.controls.get("notifications.agent.ses_delivery");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    agent.push(targetedDispatchEnvelope("aside", "targeted-aside"));
    agent.push(targetedDispatchEnvelope("steer", "targeted-steer"));
    await delivered.promise;

    expect(fixture.deliveries.map((delivery) => delivery.options)).toEqual([
      { deliverAs: "aside", triggerTurn: true },
      { deliverAs: "steer", triggerTurn: true },
    ]);
  });

  test("deduplicates targeted Dispatch frames by dedupe key", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?targeted-dedupe");
    const fixture = createPi();
    const distinctDelivery = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (fixture.deliveries.length === 2) distinctDelivery.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_delivery"));
    const agent = natsState.controls.get("notifications.agent.ses_delivery");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    agent.push(targetedDispatchEnvelope("steer", "targeted-duplicate"));
    agent.push(targetedDispatchEnvelope("steer", "targeted-duplicate"));
    agent.push(targetedDispatchEnvelope("steer", "targeted-distinct"));
    await distinctDelivery.promise;

    expect(fixture.deliveries).toHaveLength(2);
    expect(fixture.deliveries[0]?.content).toContain("targeted-duplicate");
    expect(fixture.deliveries[1]?.content).toContain("targeted-distinct");
  });

  test("reports an ephemeral delivery rejection to Dispatch without steering it", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const replies: unknown[] = [];
    const posted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      if (
        new URL(input.toString()).pathname === "/api/v1/messages/message-targeted-rejection/reply"
      ) {
        replies.push(JSON.parse(init?.body?.toString() ?? "{}"));
        posted.resolve();
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?targeted-btw-rejection");
    const fixture = createPi();
    envoyExtension({
      ...fixture.pi,
      askEphemeral: async () => {
        throw new Error("No active model on session");
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_delivery"));
    const agent = natsState.controls.get("notifications.agent.ses_delivery");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    agent.push(targetedDispatchEnvelope("btw", "targeted-rejection"));
    await posted.promise;

    expect(replies).toEqual([
      {
        actor: { id: "ses_delivery", kind: "session" },
        attempt: 1,
        error: "No active model on session",
      },
    ]);
    expect(fixture.deliveries).toEqual([]);
  });

  test("fails closed for a malformed targeted frame and reports the error to Dispatch", async () => {
    process.env.DISPATCH_URL = "http://dispatch.test";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const replies: unknown[] = [];
    const posted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      if (new URL(input.toString()).pathname === "/api/v1/messages/message-malformed/reply") {
        replies.push(JSON.parse(init?.body?.toString() ?? "{}"));
        posted.resolve();
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?targeted-malformed");
    const fixture = createPi();
    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_delivery"));
    const agent = natsState.controls.get("notifications.agent.ses_delivery");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    agent.push(
      JSON.stringify({
        event_id: "dispatch-malformed",
        source: "dispatch",
        source_event_id: "1",
        topic: "notifications.agent.ses_delivery",
        dedupe_key: "dispatch-malformed",
        issued_at: 1,
        payload_summary: "Can this ship?",
        payload: JSON.stringify({
          event: {
            actor: { id: "alice", kind: "user" },
            issue_key: "CORE-1",
            payload: { body: "Can this ship?", id: "message-malformed" },
            type: "message.created",
          },
          delivery: { attempt: 1, mode: "btw" },
        }),
        trace_id: "dispatch-malformed",
      })
    );

    await posted.promise;
    expect(fixture.deliveries).toEqual([]);
    expect(replies).toEqual([
      {
        actor: { id: "ses_delivery", kind: "session" },
        attempt: 1,
        error: "Invalid Dispatch targeted delivery frame",
      },
    ]);
  });
  test("registers every session and starts its heartbeat on session start", async () => {
    const registrations: unknown[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id: string;
          readonly dir: string;
          readonly topics: readonly string[];
        };
        registrations.push(body);
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: body.dir,
          topics: body.topics,
        });
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?always-register");
    const fixture = createPi();
    const heartbeats: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_every_session"),
      setInterval: (callback) => heartbeats.push(callback),
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    expect(registrations).toHaveLength(1);
    expect(heartbeats).toHaveLength(1);
  });

  test("registers the session title from the host and refreshes it on heartbeat", async () => {
    const subscribeTitles: unknown[] = [];
    const heartbeatSubscribe = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe" && init?.body !== undefined) {
        subscribeTitles.push(JSON.parse(init.body.toString()).title);
        if (subscribeTitles.length === 2) heartbeatSubscribe.resolve();
      }
      return response({ session_id: "ses_titled", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?register-title");
    const fixture = createPi();
    // Titles are assigned by omp after the first turn, so session_start
    // registers before one exists; the heartbeat must pick it up later.
    let sessionName: string | undefined;
    const heartbeats: (() => void)[] = [];
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { getSessionId: () => "ses_titled", getSessionName: () => sessionName },
      setInterval: (callback) => {
        heartbeats.push(callback);
      },
      ui: { notify: () => undefined },
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    expect(subscribeTitles).toEqual([""]);

    sessionName = "fix envoy ps titles";
    for (const tick of heartbeats) tick();
    await heartbeatSubscribe.promise;
    expect(subscribeTitles).toEqual(["", "fix envoy ps titles"]);
  });

  test("keeps registry registration aligned with live subscriptions", async () => {
    const registrations: { readonly topics: readonly string[] }[] = [];
    const unregistrations: (readonly string[])[] = [];
    const registryTopics = new Set<string>();
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id: string;
          readonly dir: string;
          readonly topics: readonly string[];
        };
        registrations.push(body);
        for (const topic of body.topics) registryTopics.add(topic);
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: body.dir,
          topics: [...registryTopics],
        });
      }
      if (url.pathname === "/v1/interests/unsubscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly topics: readonly string[];
        };
        unregistrations.push(body.topics);
        for (const topic of body.topics) registryTopics.delete(topic);
        return response({});
      }
      if (url.pathname === "/v1/interests/ses_registry") {
        return response({
          session_id: "ses_registry",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [...registryTopics],
        });
      }
      return response({ session_id: "ses_registry", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?live-registration");
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_registry"),
      setInterval: (callback) => intervals.push(callback),
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    const unsubscribeTool = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    const listTool = fixture.tools.find((tool) => tool.name === "envoy_list");
    if (subscribeTool === undefined || unsubscribeTool === undefined || listTool === undefined) {
      throw new Error("subscription tools were not registered");
    }

    const topic = "notifications.github.o.r.pr.42.>";
    const base = "notifications.github.o.r.pr.42";
    await subscribeTool.execute("", { topics: [topic] });
    expect(registrations.at(-1)?.topics).toEqual(["notifications.agent.ses_registry", base, topic]);
    expect(natsState.controls.get(base)?.active()).toBe(true);
    expect(natsState.controls.get(topic)?.active()).toBe(true);

    for (const tick of intervals) tick();
    expect(registrations.at(-1)?.topics).toEqual(["notifications.agent.ses_registry", base, topic]);
    const subscribed = await listTool.execute("", {});
    expect(subscribed.details.interests).toEqual([
      { topic: "notifications.agent.ses_registry", source: "both" },
      { topic: base, source: "both" },
      { topic, source: "both" },
    ]);

    await unsubscribeTool.execute("", { topics: [topic] });
    expect(unregistrations).toEqual([[base, topic]]);
    expect(registrations.at(-1)?.topics).toEqual(["notifications.agent.ses_registry"]);
    const result = await listTool.execute("", {});
    expect(JSON.parse(result.content[0]?.text ?? "")).toMatchObject({
      topics: ["notifications.agent.ses_registry"],
    });
    expect(result.details.interests).toEqual([
      { topic: "notifications.agent.ses_registry", source: "both" },
    ]);

    // The session's own inbox survives "remove all" and an explicit request
    // alike: it is how direct messages reach the session, not a subscription
    // the tool manages.
    await subscribeTool.execute("", { topics: [topic] });
    const inbox = "notifications.agent.ses_registry";
    const removeAll = await unsubscribeTool.execute("", {});
    expect(removeAll.details.removed).toEqual([base, topic]);
    expect(natsState.controls.get(inbox)?.active()).toBe(true);
    const explicit = await unsubscribeTool.execute("", { topics: [inbox] });
    expect(explicit.details.removed).toEqual([]);
    expect(natsState.controls.get(inbox)?.active()).toBe(true);
    expect(registrations.at(-1)?.topics).toEqual([inbox]);
  });

  test("merges locally live subscriptions into envoy_list before the next heartbeat", async () => {
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/v1/interests/subscribe") {
        const body = JSON.parse(init?.body?.toString() ?? "{}") as {
          readonly session_id: string;
          readonly dir: string;
          readonly topics: readonly string[];
        };
        return response({
          session_id: body.session_id,
          machine_id: "test",
          dir: body.dir,
          topics: body.topics,
        });
      }
      if (url.pathname === "/v1/interests/ses_list") {
        return response({
          session_id: "ses_list",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: ["notifications.agent.ses_list", "notifications.registry-only"],
        });
      }
      return response({ session_id: "ses_list", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?list-live-subscriptions");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_list"));
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    const listTool = fixture.tools.find((tool) => tool.name === "envoy_list");
    if (subscribeTool === undefined || listTool === undefined)
      throw new Error("subscription tools were not registered");

    await subscribeTool.execute("", { topics: ["notifications.live-only"] });
    const result = await listTool.execute("", {});

    expect(JSON.parse(result.content[0]?.text ?? "")).toMatchObject({
      topics: [
        "notifications.agent.ses_list",
        "notifications.registry-only",
        "notifications.live-only",
      ],
    });
    expect(result.details.interests).toEqual([
      { topic: "notifications.agent.ses_list", source: "both" },
      { topic: "notifications.registry-only", source: "registry" },
      { topic: "notifications.live-only", source: "live" },
    ]);
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
      machine_id: hostname(),
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
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
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
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?session-switch");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_switch"));
    const beforeSwitch = natsState.controls.get("notifications.agent.ses_before_switch");
    await fixture.handlers.get("session_switch")?.({}, sessionContext("ses_after_switch"));

    expect(beforeSwitch?.active()).toBe(false);
    expect(natsState.controls.get("notifications.agent.ses_after_switch")?.active()).toBe(true);
  });

  test("tells the agent its identity changed when a branch re-mints the session id", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?branch-identity-notice");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_branch"));
    await fixture.handlers.get("session_branch")?.(
      { previousSessionFile: "/tmp/old.jsonl" },
      sessionContext("ses_after_branch")
    );

    expect(fixture.deliveries.length).toBe(1);
    expect(fixture.deliveries[0]?.content).toContain("ses_before_branch");
    expect(fixture.deliveries[0]?.content).toContain("ses_after_branch");
    // The human just rewound; an informational notice must wait for the next
    // turn rather than starting one.
    expect(fixture.deliveries[0]?.options).toEqual({ deliverAs: "steer", triggerTurn: false });
  });

  test("tells the agent its identity changed when a fork re-mints the session id", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?fork-identity-notice");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_fork"));
    await fixture.handlers.get("session_switch")?.(
      { reason: "fork" },
      sessionContext("ses_after_fork")
    );

    expect(fixture.deliveries.length).toBe(1);
    expect(fixture.deliveries[0]?.content).toContain("ses_before_fork");
    expect(fixture.deliveries[0]?.content).toContain("ses_after_fork");
    expect(fixture.deliveries[0]?.options).toEqual({ deliverAs: "steer", triggerTurn: false });
  });

  test("tells the agent its identity changed when a handoff re-mints the session id", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?handoff-identity-notice");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    // /handoff carries the agent's own words (and any identity it named)
    // into the new session, so the stale-identity notice must fire.
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_before_handoff"));
    await fixture.handlers.get("session_switch")?.(
      { reason: "handoff" },
      sessionContext("ses_after_handoff")
    );

    expect(fixture.deliveries.length).toBe(1);
    expect(fixture.deliveries[0]?.content).toContain("ses_before_handoff");
    expect(fixture.deliveries[0]?.content).toContain("ses_after_handoff");
  });

  test("stays silent about identity when a switch replaces the transcript or keeps the id", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?switch-identity-silent");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_original"));
    // A fresh conversation ("new") and a loaded one ("resume") both carry a
    // transcript that already matches its own id — no stale identity to flag.
    await fixture.handlers.get("session_switch")?.({ reason: "new" }, sessionContext("ses_fresh"));
    await fixture.handlers.get("session_switch")?.(
      { reason: "resume" },
      sessionContext("ses_loaded")
    );
    // Tree navigation rebinds without changing the session id.
    await fixture.handlers.get("session_tree")?.(
      { newLeafId: "leaf" },
      sessionContext("ses_loaded")
    );

    expect(fixture.deliveries.length).toBe(0);
  });

  test("stays silent about identity when the session gains its first id", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?first-identity-silent");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    // A fresh TUI: session_start fires before any session exists, so the
    // transcript never carried an identity that could go stale.
    await fixture.handlers.get("session_start")?.({}, sessionContext(""));
    await fixture.handlers.get("session_branch")?.(
      { previousSessionFile: "/tmp/old.jsonl" },
      sessionContext("ses_first")
    );

    expect(fixture.deliveries.length).toBe(0);
  });

  test("a network outage during rebind does not swallow the identity notice", async () => {
    let registryDown = false;
    globalThis.fetch = async () => {
      if (registryDown) throw new Error("network unreachable");
      return response({
        session_id: "ses_outage_before",
        machine_id: "test",
        dir: "/tmp",
        topics: [],
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts?identity-notice-outage");
    const fixture = createPi();
    const notifications: string[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_outage_before"),
      ui: { notify: (message) => notifications.push(message) },
    };

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);

    registryDown = true;
    await fixture.handlers.get("session_branch")?.(
      { previousSessionFile: "/tmp/old.jsonl" },
      { ...context, sessionManager: { getSessionId: () => "ses_outage_after" } }
    );

    // The id change is a fact about the transcript, not the network: the
    // degraded-rebind warning and the identity notice must both land.
    expect(notifications.some((message) => message.includes("rebind failed"))).toBe(true);
    expect(fixture.deliveries.length).toBe(1);
    expect(fixture.deliveries[0]?.content).toContain("ses_outage_before");
    expect(fixture.deliveries[0]?.content).toContain("ses_outage_after");
  });

  test("an in-process session switch does not resurrect the previous session's topic", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
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

    expect(
      fixture.messages.some((m) => m.includes("addressed to the session we switched away from"))
    ).toBe(false);
  });

  test("envoy_unsubscribe is not undone by the resubscribe path", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?unsubscribe-stays");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_unsub"));
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    const unsubscribeTool = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
    if (subscribeTool === undefined || unsubscribeTool === undefined)
      throw new Error("subscription tools were not registered");

    await subscribeTool.execute("", { topics: ["team.standup"] });
    const dropped = natsState.controls.get("team.standup");
    expect(dropped).toBeDefined();

    const result = await unsubscribeTool.execute("", { topics: ["team.standup"] });
    expect(result.details.removed).toEqual(["team.standup"]);
    await new Promise((resolve) => setTimeout(resolve, 60));

    expect(natsState.controls.get("team.standup")).toBe(dropped);

    dropped?.push("published after the tool said it was unsubscribed");
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(
      fixture.messages.some((m) => m.includes("published after the tool said it was unsubscribed"))
    ).toBe(false);
  });

  test("a subscription.removed notice drops the local subscription so dead-connection recovery does not resurrect it", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?subscription-removed-drops-local");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_dropped"));
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    if (subscribeTool === undefined) throw new Error("subscription tool was not registered");

    const topic = dispatchIssueSubject("LEGION-1", ">");
    await subscribeTool.execute("", { topics: [topic] });
    const dropped = natsState.controls.get(topic);
    expect(dropped).toBeDefined();

    const inbox = natsState.controls.get("notifications.agent.ses_dropped");
    inbox?.push(
      JSON.stringify({
        source: "dispatch",
        topic: "notifications.agent.ses_dropped",
        payload: JSON.stringify({
          issue_key: "LEGION-1",
          type: "subscription.removed",
          actor: { kind: "user", id: "alice" },
          notify: true,
          payload: {
            session_id: "ses_dropped",
            by: { kind: "user", id: "alice" },
            topics: [topic],
          },
        }),
      })
    );
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(natsState.controls.get(topic)).toBe(dropped);
    expect(dropped?.active()).toBe(false);

    // Well past ENVOY_RESUBSCRIBE_DELAY_MS: the dead-connection recovery path
    // had its chance to treat this deliberate close as an outage. It must not.
    await new Promise((resolve) => setTimeout(resolve, 60));
    expect(natsState.controls.get(topic)).toBe(dropped);
  });

  test("a subscription.removed notice naming a different session leaves this session's subscription and emits no notice", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import(
      "./envoy.ts?subscription-removed-other-session"
    );
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_other"));
    const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    if (subscribeTool === undefined) throw new Error("subscription tool was not registered");

    const topic = dispatchIssueSubject("LEGION-1", ">");
    await subscribeTool.execute("", { topics: [topic] });
    const kept = natsState.controls.get(topic);
    expect(kept).toBeDefined();

    // This issue's own topic reaches every subscriber, not just the session
    // named in the payload — ses_other must not act on a removal that names
    // a different session (ses_target).
    const inbox = natsState.controls.get("notifications.agent.ses_other");
    inbox?.push(
      JSON.stringify({
        source: "dispatch",
        topic,
        payload: JSON.stringify({
          issue_key: "LEGION-1",
          type: "subscription.removed",
          actor: { kind: "user", id: "alice" },
          notify: true,
          payload: { session_id: "ses_target", by: { kind: "user", id: "alice" }, topics: [topic] },
        }),
      })
    );
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(natsState.controls.get(topic)).toBe(kept);
    expect(kept?.active()).toBe(true);
    expect(fixture.messages.some((m) => m.includes("Unsubscribed"))).toBe(false);
  });

  test("envoy_unsubscribe deregisters a topic while its pump waits to retry", async () => {
    const unregistrations: (readonly string[])[] = [];
    const retryScheduled = Promise.withResolvers<void>();
    const originalSetTimeout = globalThis.setTimeout;
    globalThis.setTimeout = ((_: () => void) => {
      retryScheduled.resolve();
      return undefined as never;
    }) as typeof setTimeout;
    try {
      globalThis.fetch = async (input, init) => {
        const url = new URL(input.toString());
        const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
        if (url.pathname === "/v1/interests/unsubscribe") {
          const topics = Array.isArray(body?.topics)
            ? body.topics.filter((topic): topic is string => typeof topic === "string")
            : [];
          unregistrations.push(topics);
          return response({});
        }
        if (url.pathname === "/v1/interests/subscribe") {
          return response({
            session_id: "ses_awaiting_retry",
            machine_id: "test",
            dir: "/tmp/envoy-omp-test",
            topics: body?.topics ?? [],
          });
        }
        return response({});
      };
      const { default: envoyExtension } = await import("./envoy.ts?unsubscribe-awaiting-retry");
      const fixture = createPi();

      envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, sessionContext("ses_awaiting_retry"));
      const subscribeTool = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
      const unsubscribeTool = fixture.tools.find((tool) => tool.name === "envoy_unsubscribe");
      if (subscribeTool === undefined || unsubscribeTool === undefined)
        throw new Error("subscription tools were not registered");

      const topic = "notifications.retrying";
      await subscribeTool.execute("", { topics: [topic] });
      const controls = natsState.controls.get(topic);
      if (controls === undefined) throw new Error("subscription was not created");
      controls.end();
      await retryScheduled.promise;

      const result = await unsubscribeTool.execute("", { topics: [topic] });
      expect(result.details.removed).toEqual([topic]);
      expect(unregistrations).toEqual([[topic]]);
    } finally {
      globalThis.setTimeout = originalSetTimeout;
    }
  });

  test("a deliberately closed topic still recovers from a genuine death once it is back", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
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

    recovered?.push(
      forwardedRoleEnvelope(
        "legion-controller",
        "delivered after a genuine iterator death",
        "genuine-death"
      )
    );
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(
      fixture.messages.some((m) => m.includes("delivered after a genuine iterator death"))
    ).toBe(true);
  });

  test("permits redelivery after a failed injection and warns with the envelope id", async () => {
    // Query isolation gives this stateful extension its own NATS subscription.
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?deliver-retry");
    const fixture = createPi();
    let throwNext = true;
    const failedInjection = Promise.withResolvers<void>();
    const followingDelivery = Promise.withResolvers<void>();
    const delivered: string[] = [];
    const warning = spyOn(console, "warn").mockImplementation(() => {});
    const pi: TestPi = {
      ...fixture.pi,
      sendMessage: (message) => {
        if (throwNext) {
          throwNext = false;
          failedInjection.resolve();
          throw new Error("injection rejected mid-compaction");
        }
        delivered.push(message.content);
        if (message.content.includes("the following message proves the pump continued")) {
          followingDelivery.resolve();
        }
      },
    };

    try {
      envoyExtension(pi);
      await fixture.handlers.get("session_start")?.({}, sessionContext("ses_pump"));
      const controls = natsState.controls.get("notifications.agent.ses_pump");
      expect(controls).toBeDefined();

      const envelope = forwardedRoleEnvelope(
        "legion-controller",
        "the redelivery repairs the failed injection",
        "retry-after-failure"
      );
      controls?.push(envelope);
      await failedInjection.promise;
      controls?.push(envelope);
      controls?.push(
        forwardedRoleEnvelope(
          "legion-controller",
          "the following message proves the pump continued",
          "after-retry"
        )
      );
      await followingDelivery.promise;

      expect(delivered).toHaveLength(2);
      expect(delivered[0]).toContain("the redelivery repairs the failed injection");
      expect(warning).toHaveBeenCalledTimes(1);
      expect(warning).toHaveBeenCalledWith(
        expect.stringContaining("evt-retry-after-failure"),
        expect.any(Error)
      );
    } finally {
      warning.mockRestore();
    }
  });

  test("deduplicates a redelivery after successful injection", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    // Query isolation gives this stateful extension its own NATS subscription.
    const { default: envoyExtension } = await import("./envoy.ts?deliver-dedupe");
    const fixture = createPi();
    const deliveryAfterDuplicate = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        if (message.content.includes("the next unique delivery proves the duplicate was skipped")) {
          deliveryAfterDuplicate.resolve();
        }
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_dedupe"));
    const controls = natsState.controls.get("notifications.agent.ses_dedupe");
    if (controls === undefined) throw new Error("agent subject was not subscribed");

    const envelope = forwardedRoleEnvelope(
      "legion-controller",
      "only the first delivery injects",
      "dedupe-after-success"
    );
    controls.push(envelope);
    controls.push(envelope);
    controls.push(
      forwardedRoleEnvelope(
        "legion-controller",
        "the next unique delivery proves the duplicate was skipped",
        "after-dedupe"
      )
    );
    await deliveryAfterDuplicate.promise;

    expect(fixture.messages).toHaveLength(2);
    expect(fixture.messages[0]).toContain("only the first delivery injects");
  });

  test("inbound envoy messages deliver as steering so they interrupt an in-flight turn", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?deliver-as-steer");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_steer"));
    const controls = natsState.controls.get("notifications.agent.ses_steer");
    expect(controls).toBeDefined();

    controls?.push(forwardedRoleEnvelope("legion-controller", "steer me mid-turn", "steer"));
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(fixture.deliveries.length).toBe(1);
    expect(fixture.deliveries[0]?.content).toContain("steer me mid-turn");
    expect(fixture.deliveries[0]?.options).toEqual({ deliverAs: "steer", triggerTurn: true });
  });

  test("acknowledges an agent-subject request after steering injection", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
    const { default: envoyExtension } = await import("./envoy.ts?agent-receipt");
    const fixture = createPi();
    const injected = Promise.withResolvers<void>();
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        injected.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_receipt"));
    const controls = natsState.controls.get("notifications.agent.ses_receipt");
    if (controls === undefined) throw new Error("agent subject was not subscribed");

    controls.push(
      forwardedRoleEnvelope("legion-controller", "receipt event", "agent-receipt"),
      "_INBOX.receipt"
    );
    await injected.promise;
    await Promise.resolve();
    await Promise.resolve();

    expect(natsState.published.map((message) => message.subject)).toEqual(["_INBOX.receipt"]);
  });

  test("an ended subscription iterator resubscribes instead of going deaf", async () => {
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "10";
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
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

    second?.push(forwardedRoleEnvelope("legion-controller", "post-recovery message", "recovery"));
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(fixture.messages.some((m) => m.includes("post-recovery message"))).toBe(true);
  });

  test("a heartbeat tick during a registry outage warns once instead of rejecting unhandled", async () => {
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
    const warned = Promise.withResolvers<void>();
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { getSessionId: () => "ses_heartbeat" },
      setInterval: (callback) => intervals.push(callback),
      ui: {
        notify: (message) => {
          notifications.push(message);
          if (message.includes("registry heartbeat failed")) warned.resolve();
        },
      },
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
    await warned.promise;
    for (const tick of intervals) tick();
    await Promise.resolve();

    const warnings = notifications.filter((message) =>
      message.includes("registry heartbeat failed")
    );
    expect(warnings).toHaveLength(1);
  });

  test("a rebind during a network outage notifies instead of failing the handler", async () => {
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

  test("deregisters a live session before draining its NATS connection", async () => {
    const requests: { readonly method: string; readonly path: string }[] = [];
    let drainHadStartedAtDeregistration: boolean | undefined;
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      requests.push({ method: init?.method ?? "GET", path: url.pathname });
      if (url.pathname === "/v1/sessions/ses_shutdown") {
        drainHadStartedAtDeregistration = natsState.drainStarted;
      }
      return response({
        session_id: "ses_shutdown",
        machine_id: "test",
        dir: "/tmp",
        topics: ["notifications.agent.ses_shutdown"],
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts?shutdown-deregister");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_shutdown"));
    await fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_shutdown"));

    expect(drainHadStartedAtDeregistration).toBe(false);
    expect(requests).toEqual([
      { method: "POST", path: "/v1/interests/subscribe" },
      { method: "DELETE", path: "/v1/sessions/ses_shutdown" },
    ]);
  });

  test("session_shutdown resolves within its budget when drain hangs on a dead connection", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, []);
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

  test("starts NATS drain while listener deregistration is still pending", async () => {
    const deletion = Promise.withResolvers<Response>();
    const deleteStarted = Promise.withResolvers<void>();
    globalThis.fetch = async (input, init) => {
      const path = new URL(input.toString()).pathname;
      if (path === "/v1/interests/subscribe") return responseWithRegistration(input, init, []);
      if (path === "/v1/sessions/ses_shutdown") {
        deleteStarted.resolve();
        return deletion.promise;
      }
      return response({});
    };
    const { default: envoyExtension } = await import("./envoy.ts?shutdown-parallel-drain");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_shutdown"));
    const shutdown = fixture.handlers.get("session_shutdown")?.({}, sessionContext("ses_shutdown"));
    if (shutdown === undefined) throw new Error("session_shutdown handler was not registered");

    await deleteStarted.promise;
    await Promise.resolve();
    expect(natsState.drainStarted).toBe(true);

    deletion.reject(new Error("listener cleanup failed"));
    await shutdown;
  });

  test("/whoami reads the live session ID when the session was created after session_start", async () => {
    globalThis.fetch = async () =>
      response({ session_id: "ses_live", machine_id: "test", dir: "/tmp", topics: [] });
    const { default: envoyExtension } = await import("./envoy.ts?whoami-live-id");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    // A fresh TUI: session_start fires before any session exists.
    await fixture.handlers.get("session_start")?.({}, sessionContext(""));
    const whoami = fixture.commands.find((command) => command.name === "whoami");
    if (whoami === undefined) throw new Error("/whoami was not registered");

    // The session materialized later; the command context carries the live
    // session manager and must win over the extension's cached empty ID.
    const notifications: string[] = [];
    await whoami.handler("", {
      ui: { notify: (message) => notifications.push(message) },
      sessionManager: { getSessionId: () => "ses_live" },
    });

    expect(fixture.copiedSessionIDs).toEqual(["ses_live"]);
    expect(notifications[0]).toContain("ses_live");
  });

  test("the heartbeat heals a stale registration when the session ID drifts", async () => {
    const registered: string[] = [];
    globalThis.fetch = async (_input: string | URL | Request, init?: RequestInit) => {
      if (init?.body !== undefined) {
        const body = JSON.parse(init.body.toString());
        if (typeof body.session_id === "string") registered.push(body.session_id);
      }
      return response({ session_id: "ses_created", machine_id: "test", dir: "/tmp", topics: [] });
    };
    const { default: envoyExtension } = await import("./envoy.ts?heartbeat-drift");
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    let liveSessionID = "";
    const context: SessionContext = {
      cwd: "/tmp/envoy-omp-test",
      sessionManager: { getSessionId: () => liveSessionID },
      setInterval: (callback) => intervals.push(callback),
      ui: { notify: () => undefined },
    };

    envoyExtension(fixture.pi);
    // A fresh TUI has no direct subject and no registry row until the host
    // assigns an id, but it still starts the heartbeat that observes that id.
    await fixture.handlers.get("session_start")?.({}, context);
    expect(registered).toEqual([]);
    expect(natsState.controls.has("notifications.agent.")).toBe(false);
    expect(intervals).toHaveLength(1);

    // A tick while the host still has no id registers nothing: the listener
    // rejects an empty session id, and there is no identity to advertise yet.
    // The guard returns before any request is issued, so no wait is needed.
    for (const tick of intervals) tick();
    expect(registered).toEqual([]);

    // The session materializes later; the next heartbeat must rebind the
    // NATS topic and re-register under the live ID instead of heartbeating
    // the dead identity forever.
    liveSessionID = "ses_created";
    for (const tick of intervals) tick();
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(registered).toEqual(["ses_created"]);
    expect(natsState.controls.has("notifications.agent.ses_created")).toBe(true);
  });

  test("surfaces listener subscribe warnings in the envoy_subscribe result", async () => {
    globalThis.fetch = async (input, init) => {
      const body = JSON.parse(init?.body?.toString() ?? "{}") as {
        readonly session_id?: string;
        readonly dir?: string;
        readonly topics?: readonly string[];
      };
      if (new URL(input.toString()).pathname !== "/v1/interests/subscribe") return response({});
      return response({
        session_id: body.session_id ?? "ses_omp",
        machine_id: "example-host",
        dir: body.dir ?? "/tmp",
        topics: body.topics ?? [],
        warnings: ["no matching event in stream"],
      });
    };
    const { default: envoyExtension } = await import("./envoy.ts?subscribe-warnings");
    const fixture = createPi();

    envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const subscribe = fixture.tools.find((tool) => tool.name === "envoy_subscribe");
    expect(subscribe).toBeDefined();
    if (subscribe === undefined) return;

    const result = await subscribe.execute("", {
      topics: ["notifications.github.example-org.example-repo.pr.7"],
    });

    expect(result.content[0]?.text).toContain("Warnings: no matching event in stream");
    expect(result.details).toMatchObject({ warnings: ["no matching event in stream"] });
  });

  test("returns the 50 newest rendered deliveries from envoy_inbox", async () => {
    globalThis.fetch = async (input, init) => responseWithRegistration(input, init, {});
    const { default: envoyExtension } = await import("./envoy.ts?inbox");
    const fixture = createPi();
    const delivered = Promise.withResolvers<void>();
    let deliveryCount = 0;
    envoyExtension({
      ...fixture.pi,
      sendMessage: (message, options) => {
        fixture.pi.sendMessage(message, options);
        deliveryCount += 1;
        if (deliveryCount === 51) delivered.resolve();
      },
    });
    await fixture.handlers.get("session_start")?.({}, sessionContext());
    const agent = natsState.controls.get("notifications.agent.ses_omp");
    if (agent === undefined) throw new Error("agent subject was not subscribed");

    for (let index = 1; index <= 51; index += 1) {
      agent.push(
        JSON.stringify({
          event_id: `event-${index}`,
          source: "agent",
          source_session: "01a00000-0000-7000-0000-000000000001",
          topic: "notifications.agent.ses_omp",
          dedupe_key: `dedupe-${index}`,
          issued_at: Date.parse("2026-09-07T04:41:12Z"),
          payload_summary: `summary-${index}`,
        })
      );
    }
    await delivered.promise;

    const inbox = fixture.tools.find((tool) => tool.name === "envoy_inbox");
    expect(inbox).toBeDefined();
    if (inbox === undefined) return;
    const result = await inbox.execute("", {});
    const entries = JSON.parse(result.content[0]?.text ?? "[]") as Array<{ event_id: string }>;

    expect(entries).toHaveLength(50);
    expect(entries[0]).toMatchObject({
      event_id: "event-51",
      at: "2026-09-07T04:41:12Z",
      from: "01a00000-0000-7000-0000-000000000001",
      summary: "summary-51",
    });
    expect(entries.at(-1)?.event_id).toBe("event-2");
  });

  test("registers a plain-text renderer for envoy-message so inbound content never renders as a markdown code block", async () => {
    const { default: envoyExtension } = await import("./envoy.ts?message-renderer");
    const fixture = createPi();
    envoyExtension(fixture.pi);

    const renderer = fixture.renderers.get("envoy-message");
    if (renderer === undefined) throw new Error("envoy-message renderer was not registered");

    // TOON nests a structured `message` value under a `message:` key at
    // 4-space indent — the exact shape CommonMark reads as an indented code
    // block. The renderer must show the human body verbatim regardless,
    // since it displays raw text and never runs content through Markdown.
    const content = [
      "envoy:",
      "  from: human",
      "  summary: dispatch answer",
      "  message:",
      "    **bold** answer",
      "",
      "    - one",
      "    - two",
    ].join("\n");
    const theme: MessageRendererTheme = {
      fg: (_color, text) => text,
      bold: (text) => text,
      boxRound: {
        topLeft: "+",
        topRight: "+",
        bottomLeft: "+",
        bottomRight: "+",
        horizontal: "-",
        vertical: "|",
      },
    };

    const component = renderer({ customType: "envoy-message", content }, { expanded: true }, theme);
    if (component === undefined) throw new Error("renderer returned no component");

    const lines = component.render(80).map((line) => Bun.stripANSI(line));
    const rendered = lines.join("\n");
    expect(rendered).toContain("**bold** answer");
    const oneIndex = lines.findIndex((line) => line.includes("- one"));
    const twoIndex = lines.findIndex((line) => line.includes("- two"));
    expect(oneIndex).toBeGreaterThan(-1);
    expect(twoIndex).toBeGreaterThan(oneIndex);
    // A markdown paragraph merge (the bug's actual symptom) would join
    // adjacent lines onto one row; plain-text rendering keeps them apart.
    expect(rendered).not.toContain("- one - two");
  });
});
