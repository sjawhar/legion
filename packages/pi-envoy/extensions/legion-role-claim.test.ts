import { afterEach, beforeEach, expect, mock, test } from "bun:test";
import { agentSubject, roleToken } from "@legion/contracts";
import type { ZodNumberProperty } from "../src/pi-types";

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
// Distinct mtime queries force distinct module instances, exercising the
// cross-module role-claim bridge documented in envoy.ts.
const { default: envoyExtension } = await import("./envoy.ts?envoy-entry");
const { default: legionExtension } = await import("./legion.ts?legion-entry");
type Context = {
  readonly cwd: string;
  readonly sessionManager: {
    readonly getSessionId: () => string;
    readonly getSessionFile: () => string | undefined;
    readonly ensureOnDisk: () => Promise<void>;
  };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  readonly ui: { readonly notify: (message: string, level: "warning") => void };
};
type Handler = (event: unknown, context: Context) => Promise<unknown>;
const originalFetch = globalThis.fetch;
const originalEnvironment = {
  ENVOY_NATS_URL: process.env.ENVOY_NATS_URL,
  ENVOY_URL: process.env.ENVOY_URL,
  LEGION_BOOT_TOKEN: process.env.LEGION_BOOT_TOKEN,
  LEGION_DAEMON_URL: process.env.LEGION_DAEMON_URL,
  LEGION_GENERATION: process.env.LEGION_GENERATION,
  LEGION_ISSUE: process.env.LEGION_ISSUE,
  LEGION_ROLE: process.env.LEGION_ROLE,
  LEGION_TREE: process.env.LEGION_TREE,
  LEGION_STATE_DIR: process.env.LEGION_STATE_DIR,
  HOME: process.env.HOME,
  DISPATCH_URL: process.env.DISPATCH_URL,
  DISPATCH_TOKEN: process.env.DISPATCH_TOKEN,
} as const;
// The envoy extension registers the dispatch tools whenever the developer's own
// ~/.config/opencode/envoy.json enables dispatch; this file's zod stub is not a real schema
// builder, so the resolution must see no user config and no DISPATCH_* override.
beforeEach(() => {
  process.env.HOME = "/nonexistent-home-for-legion-tests";
  delete process.env.DISPATCH_URL;
  delete process.env.DISPATCH_TOKEN;
});
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
  const tree = "REPO-42";
  const sessionID = "ses_legion_root";
  const role = roleToken("omp", tree, "architect");
  process.env.ENVOY_NATS_URL = "nats://nats-under-test:4222";
  process.env.ENVOY_URL = "http://envoy.test";
  process.env.LEGION_DAEMON_URL = "http://daemon.test";
  process.env.LEGION_GENERATION = "3";
  process.env.LEGION_BOOT_TOKEN = "claim-heartbeat";
  process.env.LEGION_STATE_DIR = "/tmp/legion-state";
  process.env.LEGION_TREE = tree;
  process.env.LEGION_ROLE = "architect";
  process.env.LEGION_ISSUE = tree;
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
      if (registrations.length === 3) heartbeatRegistration.resolve();
      return Response.json({
        session_id: body.session_id,
        machine_id: "test",
        dir: "/tmp",
        topics: body.topics,
      });
    }
    return Response.json({ session_id: sessionID, machine_id: "test", dir: "/tmp", topics: [] });
  }) as typeof fetch;
  const activeTools = ["read", "task", "hub"];
  const property = (): ZodNumberProperty => ({
    optional: property,
    describe: property,
    int: property,
  });
  const createPi = () => ({
    zod: {
      object: (shape: unknown) => shape,
      string: property,
      number: property,
      array: property,
      enum: property,
      unknown: property,
      discriminatedUnion: () => ({}),
    },
    sendMessage: () => undefined,
    appendEntry: () => undefined,
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
    registerMessageRenderer: () => undefined,
  });
  const envoyPi = createPi();
  const legionPi = createPi();
  const context: Context = {
    cwd: "/tmp/legion-workspace",
    sessionManager: {
      getSessionId: () => sessionID,
      getSessionFile: () => "/tmp/legion-root.jsonl",
      ensureOnDisk: async () => undefined,
    },
    setInterval: (callback) => intervals.push(callback),
    ui: { notify: () => undefined },
  };

  // Legion initializes before Envoy — the opposite of load order in the
  // manifest — to prove claimEnvoyRole's bridge waits rather than binding to
  // a claim function that was never set.
  legionExtension(legionPi as never);
  envoyExtension(envoyPi as never);
  const sessionStart = handlers.get("session_start")?.[0];
  if (sessionStart === undefined) {
    throw new Error("Legion lifecycle handlers were not registered");
  }
  await sessionStart({}, context);

  expect(intervals).toHaveLength(1);
  // Establishing the role claimant registers before the role is applied, then
  // refreshes it after the claim.
  expect(registrations).toHaveLength(2);
  intervals[0]?.();
  await heartbeatRegistration.promise;
  expect(registrations).toHaveLength(3);
  expect(registrations[2]).toMatchObject({
    session_id: sessionID,
    topics: [agentSubject(sessionID)],
  });
});
