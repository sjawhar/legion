import { afterEach, expect, mock, test } from "bun:test";
import { agentSubject, roleToken } from "@legion/contracts";

mock.module("nats", () => ({
  connect: async () => ({
    isClosed: () => false,
    close: async () => undefined,
    drain: async () => undefined,
    subscribe: () => ({
      unsubscribe: () => undefined,
      [Symbol.asyncIterator]: async function* () {
        await new Promise<never>(() => undefined);
      },
    }),
  }),
  StringCodec: () => ({
    decode: (data: Uint8Array) => new TextDecoder().decode(data),
  }),
}));

mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async () => undefined,
}));

// OMP loads each manifest entry under a distinct mtime query, so the Legion
// entry's local Envoy import must not share module-scoped state with Envoy's entry.
const { default: envoyExtension } = await import("./envoy.ts?envoy-entry");
const { default: legionExtension } = await import("./legion.ts?legion-entry");

type Context = {
  readonly cwd: string;
  readonly sessionManager: {
    readonly getSessionId: () => string;
    readonly getSessionFile: () => string | undefined;
  };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  readonly ui: { readonly notify: (message: string, level: "warning") => void };
};

type Handler = (event: unknown, context: Context) => Promise<unknown>;

const originalFetch = globalThis.fetch;
const originalEnvironment = {
  ENVOY_NATS_URL: process.env.ENVOY_NATS_URL,
  ENVOY_REGISTER_SESSION: process.env.ENVOY_REGISTER_SESSION,
  ENVOY_URL: process.env.ENVOY_URL,
  LEGION_BOOT_TOKEN: process.env.LEGION_BOOT_TOKEN,
  LEGION_DAEMON_URL: process.env.LEGION_DAEMON_URL,
  LEGION_GENERATION: process.env.LEGION_GENERATION,
  LEGION_PROJECT: process.env.LEGION_PROJECT,
  LEGION_TREE: process.env.LEGION_TREE,
  LEGION_STATE_DIR: process.env.LEGION_STATE_DIR,
} as const;

afterEach(() => {
  globalThis.fetch = originalFetch;
  for (const [key, value] of Object.entries(originalEnvironment)) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
});

test("keeps a Legion role claimant fresh regardless of extension initialization order", async () => {
  const handlers = new Map<string, Handler[]>();
  const intervals: (() => void)[] = [];
  const registrations: { readonly session_id: string; readonly topics: readonly string[] }[] = [];
  const heartbeatRegistration = Promise.withResolvers<void>();
  const tree = "owner/repo#42";
  const sessionID = "ses_legion_root";
  const role = roleToken("omp", tree, "architect");
  process.env.ENVOY_NATS_URL = "nats://nats-under-test:4222";
  delete process.env.ENVOY_REGISTER_SESSION;
  process.env.ENVOY_URL = "http://envoy.test";
  process.env.LEGION_DAEMON_URL = "http://daemon.test";
  process.env.LEGION_GENERATION = "3";
  process.env.LEGION_BOOT_TOKEN = "claim-heartbeat";
  process.env.LEGION_PROJECT = "omp";
  process.env.LEGION_STATE_DIR = "/tmp/legion-state";
  process.env.LEGION_TREE = tree;
  globalThis.fetch = (async (input, init) => {
    const url = new URL(input.toString());
    if (url.pathname === "/legion/v1/process/started") {
      return Response.json({
        roleTokens: { architect: role },
        controlSubject: "legion.ctl.owner-repo-42.3",
        secret: "root-secret",
      });
    }
    if (url.pathname === "/v1/interests/subscribe") {
      const body = JSON.parse(init?.body?.toString() ?? "{}") as {
        readonly session_id: string;
        readonly topics: readonly string[];
      };
      registrations.push(body);
      if (registrations.length === 2) heartbeatRegistration.resolve();
      return Response.json({ session_id: body.session_id, machine_id: "test", dir: "/tmp", topics: body.topics });
    }
    return Response.json({ session_id: sessionID, machine_id: "test", dir: "/tmp", topics: [] });
  }) as typeof fetch;
  const activeTools = ["read", "task", "hub"];
  const createPi = () => ({
    agents: {
      list: () => [],
      get: () => undefined,
      ensureLive: async (agentId: string) => ({ id: agentId }),
      prompt: async () => undefined,
    },
    zod: {
      object: (shape: unknown) => shape,
      string: () => ({ optional: () => undefined }),
      array: () => ({ optional: () => undefined }),
      enum: () => ({ optional: () => undefined }),
      unknown: () => ({ optional: () => undefined }),
      discriminatedUnion: () => ({}),
    },
    sendMessage: () => undefined,
    getActiveTools: () => activeTools,
    setActiveTools: async (tools: string[]) => {
      activeTools.splice(0, activeTools.length, ...tools);
    },
    on: (event: string, handler: Handler) => {
      const eventHandlers = handlers.get(event);
      if (eventHandlers === undefined) handlers.set(event, [handler]);
      else eventHandlers.push(handler);
    },
    registerTool: () => undefined,
    registerCommand: () => undefined,
  });
  const envoyPi = createPi();
  const legionPi = createPi();
  const context: Context = {
    cwd: "/tmp/legion-workspace",
    sessionManager: {
      getSessionId: () => sessionID,
      getSessionFile: () => "/tmp/legion-root.jsonl",
    },
    setInterval: (callback) => intervals.push(callback),
    ui: { notify: () => undefined },
  };

  legionExtension(legionPi);
  envoyExtension(envoyPi as never);
  const sessionStart = handlers.get("session_start")?.[0];
  const beforeAgentStart = handlers.get("before_agent_start")?.[0];
  if (sessionStart === undefined || beforeAgentStart === undefined) {
    throw new Error("Legion lifecycle handlers were not registered");
  }
  await sessionStart({}, context);
  await beforeAgentStart({ prompt: "Start the root architect" }, context);

  expect(intervals).toHaveLength(1);
  expect(registrations).toHaveLength(1);
  intervals[0]?.();
  await heartbeatRegistration.promise;
  expect(registrations).toHaveLength(2);
  expect(registrations[1]).toMatchObject({
    session_id: sessionID,
    topics: [agentSubject(sessionID)],
  });
});
