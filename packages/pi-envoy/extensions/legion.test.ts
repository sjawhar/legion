import { afterEach, beforeEach, describe, expect, mock, test } from "bun:test";
import { access, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import {
  agentSubject,
  type IssueKey,
  LEGION_ROLES,
  type LegionRole,
  roleToken,
} from "@legion/contracts";
import { z } from "zod";
import { classifySession } from "../src/legion/classify";
import { handleLegionControlDirective } from "../src/legion/control";
import type {
  CommandContext,
  PiApi,
  RegisteredTool,
  SessionContext,
  ZodNumberProperty,
  ZodProperty,
} from "../src/pi-types";

/** Minimal, fully-valid `GET /legion/v1/state` mock response — the contract schema is a
 * `strictObject` at every level, so a partial payload like `{ project }` now fails `.parse()`. */
function redactedLegionState(project: string) {
  return {
    project,
    version: 21,
    issues: {},
    trees: {},
    admission: { cap: 1, active: [], queue: [] },
    gates: {},
    roles: {},
    controllerPendingNotices: 0,
    pendingStatusWrites: [],
  };
}
const natsConnections: { readonly name: string }[] = [];
mock.module("nats", () => ({
  connect: async (options: { readonly name: string }) => {
    natsConnections.push({ name: options.name });
    return {
      close: async () => undefined,
      drain: async () => undefined,
      isClosed: () => false,
      publish: () => undefined,
      subscribe: () => ({
        unsubscribe: () => undefined,
        [Symbol.asyncIterator]: async function* () {
          await new Promise<never>(() => undefined);
        },
      }),
    };
  },
  StringCodec: () => ({
    decode: (data: Uint8Array) => new TextDecoder().decode(data),
    encode: (text: string) => new TextEncoder().encode(text),
  }),
}));
mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async () => undefined,
}));

// The extension modules must load after their OMP and NATS host dependencies are mocked.
const { default: envoyExtension } = await import("./envoy");
const { default: legionExtension, setLegionBootstrapExitForTests } = await import("./legion");

type RegisteredCommand = {
  readonly name: string;
  readonly description: string;
  readonly handler: (args: string, context: CommandContext) => Promise<void>;
};

type SentMessage = Parameters<PiApi["sendMessage"]>[0];

type TestPi = {
  readonly zod: PiApi["zod"] & {
    readonly discriminatedUnion: (key: string, options: readonly unknown[]) => unknown;
  };
  readonly sendMessage: PiApi["sendMessage"];
  readonly appendEntry: PiApi["appendEntry"];
  readonly getActiveTools: () => readonly string[];
  readonly setActiveTools: (tools: string[]) => Promise<void>;
  readonly on: (
    event: string,
    handler: (event: unknown, context: SessionContext) => Promise<unknown> | unknown
  ) => void;
  readonly registerTool: (tool: RegisteredTool) => void;
  readonly registerCommand: (name: string, command: Omit<RegisteredCommand, "name">) => void;
  readonly registerMessageRenderer: PiApi["registerMessageRenderer"];
};

type Handler = (event: unknown, context: SessionContext) => Promise<unknown> | unknown;

const originalFetch = globalThis.fetch;
const environmentKeys = [
  "ENVOY_NATS_URL",
  "ENVOY_URL",
  "LEGION_CONTROLLER",
  "LEGION_CONTROLLER_SECRET",
  "LEGION_DAEMON_URL",
  "LEGION_GENERATION",
  "LEGION_BOOT_TOKEN",
  "LEGION_TREE",
  "LEGION_ROLE",
  "LEGION_ISSUE",
  "LEGION_WORKSPACE",
  "LEGION_STATE_DIR",
  "HOME",
  "DISPATCH_URL",
  "DISPATCH_TOKEN",
  "LEGION_BOOT_TOKEN_FILE",
  "LEGION_CONTROLLER_SECRET_FILE",
  "DISPATCH_TOKEN_FILE",
] as const;
const originalEnvironment: Record<(typeof environmentKeys)[number], string | undefined> = {
  ENVOY_NATS_URL: process.env.ENVOY_NATS_URL,
  ENVOY_URL: process.env.ENVOY_URL,
  LEGION_CONTROLLER: process.env.LEGION_CONTROLLER,
  LEGION_CONTROLLER_SECRET: process.env.LEGION_CONTROLLER_SECRET,
  LEGION_DAEMON_URL: process.env.LEGION_DAEMON_URL,
  LEGION_GENERATION: process.env.LEGION_GENERATION,
  LEGION_BOOT_TOKEN: process.env.LEGION_BOOT_TOKEN,
  LEGION_TREE: process.env.LEGION_TREE,
  LEGION_ROLE: process.env.LEGION_ROLE,
  LEGION_ISSUE: process.env.LEGION_ISSUE,
  LEGION_WORKSPACE: process.env.LEGION_WORKSPACE,
  LEGION_STATE_DIR: process.env.LEGION_STATE_DIR,
  HOME: process.env.HOME,
  DISPATCH_URL: process.env.DISPATCH_URL,
  DISPATCH_TOKEN: process.env.DISPATCH_TOKEN,
  LEGION_BOOT_TOKEN_FILE: process.env.LEGION_BOOT_TOKEN_FILE,
  LEGION_CONTROLLER_SECRET_FILE: process.env.LEGION_CONTROLLER_SECRET_FILE,
  DISPATCH_TOKEN_FILE: process.env.DISPATCH_TOKEN_FILE,
};

const temporaryPaths: string[] = [];

beforeEach(() => {
  // A suite run from inside a Legion pane inherits that pane's LEGION_*/DISPATCH_* launch
  // environment; every test starts from none and sets only what it declares.
  for (const key of environmentKeys) delete process.env[key];
});

afterEach(async () => {
  globalThis.fetch = originalFetch;
  natsConnections.splice(0);
  setLegionBootstrapExitForTests((code) => process.exit(code) as never);
  for (const key of environmentKeys) {
    const value = originalEnvironment[key];
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  await Promise.all(
    temporaryPaths.splice(0).map((directory) => rm(directory, { force: true, recursive: true }))
  );
});

function createPi(): {
  readonly commands: RegisteredCommand[];
  readonly handlers: Map<string, Handler>;
  readonly tools: RegisteredTool[];
  readonly sentMessages: SentMessage[];
  readonly activeTools: string[];
  readonly pi: TestPi;
} {
  const commands: RegisteredCommand[] = [];
  const handlers = new Map<string, Handler>();
  const registeredHandlers = new Map<string, Handler[]>();
  const tools: RegisteredTool[] = [];
  const sentMessages: SentMessage[] = [];
  const activeTools = ["read", "task", "hub"];
  const property = (): ZodNumberProperty => ({
    optional: property,
    describe: property,
    int: property,
  });
  const optional = property;
  process.env.ENVOY_NATS_URL = "nats://nats-under-test:4222";
  process.env.LEGION_STATE_DIR ??= "/tmp/legion-state";
  // The envoy extension registers the dispatch tools whenever the developer's own
  // ~/.config/opencode/envoy.json enables dispatch; this fixture's zod stub is not a real
  // schema builder, so the resolution must see no user config.
  process.env.HOME = "/nonexistent-home-for-legion-tests";
  const pi: TestPi = {
    zod: {
      object: (shape) => shape,
      string: optional,
      number: optional,
      array: () => optional(),
      enum: () => optional(),
      unknown: () => optional(),
      discriminatedUnion: () => ({}),
    },
    sendMessage: (message) => sentMessages.push(message),
    appendEntry: () => undefined,
    on: (eventName, handler) => {
      const eventHandlers = registeredHandlers.get(eventName);
      if (eventHandlers === undefined) registeredHandlers.set(eventName, [handler]);
      else eventHandlers.push(handler);
      handlers.set(eventName, async (event, context) => {
        let result: unknown;
        for (const registeredHandler of registeredHandlers.get(eventName) ?? []) {
          const next = await registeredHandler(event, context);
          if (next !== undefined) result = next;
        }
        return result;
      });
    },
    registerTool: (tool) => tools.push(tool),
    getActiveTools: () => activeTools,
    setActiveTools: async (tools) => {
      activeTools.splice(0, activeTools.length, ...tools);
    },
    registerCommand: (name, command) => commands.push({ name, ...command }),
    registerMessageRenderer: () => undefined,
  };
  envoyExtension(pi as never);
  return { commands, handlers, tools, sentMessages, activeTools, pi };
}

/** A real zod-backed `pi.zod`, unlike `createPi()`'s identity-passthrough fake: lets a test parse
 * raw tool input through the actual schema `legionToolSchema` builds, proving a field survives
 * (or a malformed input is rejected) at the real registered-tool boundary, not only through a
 * mocked direct `execute()` call. */
function createRealZodPi(): TestPi["zod"] {
  return {
    object: (shape) => z.object(shape as Record<string, z.ZodTypeAny>),
    string: () => z.string() as unknown as ZodProperty,
    number: () => z.number() as unknown as ZodNumberProperty,
    array: (item) => z.array(item as z.ZodTypeAny) as unknown as ZodProperty,
    enum: (values) => z.enum(values as [string, ...string[]]) as unknown as ZodProperty,
    unknown: () => z.unknown() as unknown as ZodProperty,
    discriminatedUnion: () => ({}),
  };
}

function sessionContext(
  sessionID: string,
  sessionFile = "/tmp/session.jsonl",
  ensureOnDisk: () => Promise<void> = async () => undefined
): SessionContext {
  return {
    cwd: "/tmp/legion-workspace",
    taskDepth: 0,
    sessionManager: {
      getSessionId: () => sessionID,
      getSessionFile: () => sessionFile,
      ensureOnDisk,
    },
    setInterval: () => undefined,
    ui: { notify: () => undefined },
  };
}

/** Lays out a `{ parentFile, childFile }` transcript pair matching OMP's own subagent
 * convention (oh-my-pi packages/coding-agent/src/session/session-manager.ts:143-154): the
 * child's transcript sits inside a directory named after the parent's transcript file, minus
 * its `.jsonl` extension, so `fs.existsSync(path.dirname(childFile) + ".jsonl")` finds
 * `parentFile`. */
async function createSubagentTranscriptPaths(): Promise<{
  readonly parentFile: string;
  readonly childFile: string;
}> {
  const baseDirectory = await mkdtemp(path.join(os.tmpdir(), "legion-subagent-"));
  temporaryPaths.push(baseDirectory);
  const parentFile = path.join(baseDirectory, "parent.jsonl");
  await writeFile(parentFile, "");
  const childDirectory = path.join(baseDirectory, "parent");
  await mkdir(childDirectory, { recursive: true });
  const childFile = path.join(childDirectory, "child.jsonl");
  return { parentFile, childFile };
}
async function createJjWorkspace(): Promise<string> {
  const directory = await mkdtemp(path.join(os.tmpdir(), "legion-omp-extension-"));
  temporaryPaths.push(directory);
  const child = Bun.spawn(["jj", "git", "init", directory], { stdout: "ignore", stderr: "pipe" });
  const exitCode = await child.exited;
  if (exitCode !== 0) {
    const stderr = await new Response(child.stderr as ReadableStream<Uint8Array>).text();
    throw new Error(`jj git init failed: ${stderr}`);
  }
  return directory;
}

async function jjConfig(directory: string, key: string): Promise<string> {
  const child = Bun.spawn(["jj", "config", "get", "--repository", directory, key], {
    cwd: directory,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout as ReadableStream<Uint8Array>).text(),
    new Response(child.stderr as ReadableStream<Uint8Array>).text(),
  ]);
  if (exitCode !== 0) throw new Error(`jj config get failed: ${stderr}`);
  return stdout.trim();
}
async function commandOutput(
  command: string[],
  cwd?: string,
  env?: NodeJS.ProcessEnv
): Promise<string> {
  const child = Bun.spawn(command, { cwd, env, stdout: "pipe", stderr: "pipe" });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout as ReadableStream<Uint8Array>).text(),
    new Response(child.stderr as ReadableStream<Uint8Array>).text(),
  ]);
  if (exitCode !== 0) throw new Error(`${command.join(" ")} failed: ${stderr}`);
  return stdout.trim();
}

/** Boots a phase-worker session and returns its tool_call handler bound to that session. */
async function bootWorker(options: {
  readonly role: LegionRole;
  readonly tree?: IssueKey;
  readonly issue?: IssueKey;
  readonly sessionId?: string;
  readonly workspace: string;
  readonly requests?: { readonly path: string; readonly body: unknown }[];
  readonly extraRoutes?: (url: URL, body: unknown) => Response | undefined;
}): Promise<{
  readonly toolCall: Handler;
  readonly context: SessionContext;
  readonly token: string;
}> {
  const tree = options.tree ?? "REPO-42";
  const issue = options.issue ?? "REPO-43";
  const sessionId = options.sessionId ?? `ses_${options.role}`;
  const token = roleToken("omp", issue, options.role);
  process.env.ENVOY_URL = "http://envoy.test";
  process.env.LEGION_DAEMON_URL = "http://daemon.test";
  process.env.LEGION_BOOT_TOKEN = `boot-${sessionId}`;
  process.env.LEGION_GENERATION = "1";
  process.env.LEGION_TREE = tree;
  process.env.LEGION_ISSUE = issue;
  process.env.LEGION_ROLE = options.role;
  process.env.LEGION_WORKSPACE = options.workspace;
  globalThis.fetch = (async (input, init) => {
    const url = new URL(input.toString());
    const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
    options.requests?.push({ path: url.pathname, body });
    const extra = options.extraRoutes?.(url, body);
    if (extra) return extra;
    if (url.pathname === "/legion/v1/worker/started") {
      return Response.json({
        roleToken: token,
        secret: "worker-secret",
        gitName: "Legion Worker",
        gitEmail: "worker@example.test",
      });
    }
    if (url.pathname === "/legion/v1/worker/ready") return Response.json({});
    return Response.json({
      session_id: sessionId,
      machine_id: "machine",
      dir: options.workspace,
      topics: [token],
    });
  }) as typeof fetch;
  const fixture = createPi();
  legionExtension(fixture.pi);
  const sessionStart = fixture.handlers.get("session_start");
  const toolCall = fixture.handlers.get("tool_call");
  if (sessionStart === undefined || toolCall === undefined) {
    throw new Error("worker lifecycle handlers were not registered");
  }
  const context = { ...sessionContext(sessionId), cwd: options.workspace };
  await sessionStart({}, context);
  return { toolCall, context, token };
}

describe("Legion OMP extension", () => {
  test("classifies controllers, root architects, sub-architects, phase workers, and ordinary sessions", () => {
    // The root architect's own issue key equals the tree's.
    expect(
      classifySession({
        LEGION_ROLE: "architect",
        LEGION_TREE: "REPO-42",
        LEGION_ISSUE: "REPO-42",
      })
    ).toEqual({ kind: "root-architect", tree: "REPO-42" });
    // A sub-architect on a child issue is a phase worker like any other role.
    expect(
      classifySession({
        LEGION_ROLE: "architect",
        LEGION_TREE: "REPO-42",
        LEGION_ISSUE: "REPO-43",
      })
    ).toEqual({
      kind: "phase-worker",
      role: "architect",
      tree: "REPO-42",
      issue: "REPO-43",
    });
    expect(classifySession({ LEGION_CONTROLLER: "1" })).toEqual({
      kind: "controller",
    });
    expect(
      classifySession({
        LEGION_ROLE: "reviewer",
        LEGION_TREE: "REPO-42",
        LEGION_ISSUE: "REPO-43",
      })
    ).toEqual({
      kind: "phase-worker",
      role: "reviewer",
      tree: "REPO-42",
      issue: "REPO-43",
    });
    expect(classifySession({})).toEqual({ kind: "not-legion" });
  });
  test("classifies the controller by LEGION_CONTROLLER alone, ignoring a redundant LEGION_ROLE=controller", () => {
    expect(classifySession({ LEGION_CONTROLLER: "1", LEGION_ROLE: "controller" })).toEqual({
      kind: "controller",
    });
  });
  test("rejects an unrecognized LEGION_ROLE value", () => {
    expect(() => classifySession({ LEGION_ROLE: "explorer", LEGION_TREE: "REPO-42" })).toThrow(
      'LEGION_ROLE "explorer" is not a Legion role'
    );
  });
  test("rejects a session launched with both controller and tree markers", () => {
    expect(() =>
      classifySession({
        LEGION_CONTROLLER: "1",
        LEGION_CONTROLLER_SECRET: "controller-secret",
        LEGION_TREE: "REPO-42",
      })
    ).toThrow("both controller and tree launch markers");
  });
  test("recovers a live root architect command after the daemon loses its capability map", async () => {
    const requests: {
      readonly path: string;
      readonly body: Record<string, unknown> | undefined;
    }[] = [];
    const tree = "REPO-42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "root-recovery";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body =
        init?.body == null
          ? undefined
          : (JSON.parse(init.body.toString()) as Record<string, unknown>);
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: token },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      if (url.pathname === "/legion/v1/process/ready") return Response.json({});
      if (url.pathname === "/legion/v1/worker-session") {
        return Response.json({
          tree,
          issue: tree,
          role: "architect",
          secret: "recovered-root-secret",
        });
      }
      if (url.pathname === "/legion/v1/waves/release") {
        if (body?.secret === "root-secret") {
          return Response.json({ error: "Invalid session secret" }, { status: 403 });
        }
        return Response.json({ released: ["REPO-43"] });
      }
      return Response.json({
        session_id: body?.session_id,
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;

    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined)
      throw new Error("Legion session_start handler was not registered");
    const context = sessionContext("ses_root", "/tmp/root-transcript.jsonl");
    await sessionStart({}, context);
    const legionTool = fixture.tools.find((tool) => tool.name === "legion");
    if (legionTool === undefined) throw new Error("Legion root tool was not registered");
    const result = await legionTool.execute(
      "call-root-recovery",
      { op: "release_wave", issues: ["REPO-43"] },
      undefined,
      undefined,
      context
    );

    expect(result.isError).toBeUndefined();
    expect(requests.filter((request) => request.path.startsWith("/legion/"))).toEqual([
      {
        path: "/legion/v1/process/started",
        body: {
          tree,
          generation: 3,
          bootToken: "root-recovery",
          rootSessionId: "ses_root",
          agentId: "root-transcript",
          ompSessionFile: "/tmp/root-transcript.jsonl",
        },
      },
      {
        path: "/legion/v1/process/ready",
        body: { tree, sessionId: "ses_root", secret: "root-secret", generation: 3 },
      },
      {
        path: "/legion/v1/waves/release",
        body: {
          tree,
          sessionId: "ses_root",
          secret: "root-secret",
          issues: ["REPO-43"],
        },
      },
      {
        path: "/legion/v1/worker-session",
        body: { sessionId: "ses_root", recoveryToken: "root-recovery" },
      },
      {
        path: "/legion/v1/waves/release",
        body: {
          tree,
          sessionId: "ses_root",
          secret: "recovered-root-secret",
          issues: ["REPO-43"],
        },
      },
    ]);
  });
  test("registers the root process before claiming its role and agent delivery subject", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "REPO-42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-root-registration";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: token },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      return Response.json({
        session_id: "ses_root",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();

    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");
    await sessionStart({}, sessionContext("ses_root"));
    expect(fixture.activeTools).toEqual(["read", "task", "hub", "legion"]);

    expect(requests).toEqual([
      // Envoy registers the direct subject before Legion claims the root role.
      {
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_root",
          dir: "/tmp/legion-workspace",
          topics: [agentSubject("ses_root")],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
        },
      },
      {
        path: "/legion/v1/process/started",
        body: {
          tree,
          generation: 3,
          bootToken: "boot-root-registration",
          rootSessionId: "ses_root",
          agentId: "session",
          ompSessionFile: "/tmp/session.jsonl",
        },
      },
      {
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_root",
          dir: "/tmp/legion-workspace",
          topics: [agentSubject("ses_root")],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
        },
      },
      { path: "/v1/roles/set", body: { session_id: "ses_root", role: token } },
      {
        path: "/legion/v1/process/ready",
        body: { tree, sessionId: "ses_root", secret: "root-secret", generation: 3 },
      },
    ]);
  });
  test("claims the controller role at startup and on demand for an interactive session", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const token = "legion-omp-controller";
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_CONTROLLER = "1";
    process.env.LEGION_CONTROLLER_SECRET = "controller-secret";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/state") return Response.json(redactedLegionState("omp"));
      if (url.pathname === "/legion/v1/controller/ready") return Response.json({});
      return Response.json({
        session_id: body?.session_id,
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();

    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const claimCommand = fixture.commands.find(
      (command) => command.name === "legion-claim-controller"
    );
    if (sessionStart === undefined || claimCommand === undefined) {
      throw new Error("controller handlers were not registered");
    }
    await sessionStart({}, sessionContext("ses_controller"));
    await sessionStart({}, sessionContext("ses_interactive"));
    await claimCommand.handler("", sessionContext("ses_interactive"));

    expect(requests).toEqual([
      // Envoy registers the direct subject before the controller claim.
      {
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_controller",
          dir: "/tmp/legion-workspace",
          topics: [agentSubject("ses_controller")],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
        },
      },
      { path: "/legion/v1/state", body: undefined },
      {
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_controller",
          dir: "/tmp/legion-workspace",
          topics: [agentSubject("ses_controller")],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
        },
      },
      { path: "/v1/roles/set", body: { session_id: "ses_controller", role: token } },
      {
        path: "/legion/v1/controller/ready",
        body: { secret: "controller-secret", sessionId: "ses_controller" },
      },
      {
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_interactive",
          dir: "/tmp/legion-workspace",
          topics: [agentSubject("ses_interactive")],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
        },
      },
      // The controller role was held by ses_controller in this same process;
      // the rebind to ses_interactive re-claims it under the live id from
      // memory as a soft claim naming its predecessor, before the explicit
      // claim command runs.
      {
        path: "/v1/roles/set",
        body: {
          session_id: "ses_interactive",
          role: token,
          soft: true,
          previous_session_id: "ses_controller",
        },
      },
      { path: "/legion/v1/state", body: undefined },
      {
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_interactive",
          dir: "/tmp/legion-workspace",
          topics: [agentSubject("ses_interactive")],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
        },
      },
      { path: "/v1/roles/set", body: { session_id: "ses_interactive", role: token } },
      {
        path: "/legion/v1/controller/ready",
        body: { secret: "controller-secret", sessionId: "ses_interactive" },
      },
    ]);
  });
  test("a controller that regains its role re-runs controller/ready so held notices drain", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const token = "legion-omp-controller";
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_CONTROLLER = "1";
    process.env.LEGION_CONTROLLER_SECRET = "controller-secret";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    let listenerHoldsClaim = true;
    const secondReady = Promise.withResolvers<void>();
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/state") return Response.json(redactedLegionState("omp"));
      if (url.pathname === "/legion/v1/controller/ready") {
        if (requests.filter((request) => request.path === url.pathname).length === 2) {
          secondReady.resolve();
        }
        return Response.json({});
      }
      if (url.pathname === `/v1/roles/${token}`) {
        if (!listenerHoldsClaim) {
          return Response.json({ error: `no holder for role ${token}` }, { status: 404 });
        }
        return Response.json({ role: token, holder: "ses_controller", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") listenerHoldsClaim = true;
      return Response.json({
        session_id: body?.session_id,
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("controller handlers were not registered");
    const intervals: (() => void)[] = [];
    await sessionStart(
      {},
      { ...sessionContext("ses_controller"), setInterval: (callback) => intervals.push(callback) }
    );
    const ready = {
      path: "/legion/v1/controller/ready",
      body: { secret: "controller-secret", sessionId: "ses_controller" },
    };
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready]);

    // A steady tick: the listener still names this session, so nothing is claimed or re-run.
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready]);
    expect(requests.filter((request) => request.path === "/v1/roles/set")).toHaveLength(1);

    // The listener lost the claim (tonight's incident): the next tick reclaims and drains.
    listenerHoldsClaim = false;
    intervals[0]?.();
    await secondReady.promise;
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready, ready]);
    expect(
      requests.filter((request) => request.path === "/v1/roles/set").map((request) => request.body)
    ).toEqual([
      { session_id: "ses_controller", role: token },
      { session_id: "ses_controller", role: token, soft: true },
    ]);
  });
  test("takes over the controller role through the daemon-ready handshake", async () => {
    const requests: { readonly method: string; readonly path: string; readonly body: unknown }[] =
      [];
    const project = "omp";
    const token = "legion-omp-controller";
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_CONTROLLER_SECRET = "controller-capability";
    delete process.env.LEGION_CONTROLLER;
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ method: init?.method ?? "GET", path: url.pathname, body });
      if (url.pathname === "/legion/v1/state") return Response.json(redactedLegionState(project));
      if (url.pathname === "/legion/v1/controller/ready") return Response.json({});
      return Response.json({
        session_id: body?.session_id,
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();

    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const claimCommand = fixture.commands.find(
      (command) => command.name === "legion-claim-controller"
    );
    if (sessionStart === undefined || claimCommand === undefined) {
      throw new Error("controller claim command was not registered");
    }
    await sessionStart({}, sessionContext("ses_interactive"));

    await claimCommand.handler("", {
      cwd: "/tmp/legion-workspace",
      sessionManager: { getSessionId: () => "ses_interactive" },
      ui: { notify: () => undefined },
    });

    expect(requests).toEqual([
      // Envoy registers the direct subject before the interactive controller claim.
      {
        method: "POST",
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_interactive",
          dir: "/tmp/legion-workspace",
          topics: [agentSubject("ses_interactive")],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
        },
      },
      { method: "GET", path: "/legion/v1/state", body: undefined },
      {
        method: "POST",
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_interactive",
          dir: "/tmp/legion-workspace",
          topics: [agentSubject("ses_interactive")],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
        },
      },
      {
        method: "POST",
        path: "/v1/roles/set",
        body: { session_id: "ses_interactive", role: token },
      },
      {
        method: "POST",
        path: "/legion/v1/controller/ready",
        body: { secret: "controller-capability", sessionId: "ses_interactive" },
      },
    ]);
  });
  test("requires the controller capability in the interactive session environment", async () => {
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    delete process.env.LEGION_CONTROLLER;
    delete process.env.LEGION_CONTROLLER_SECRET;
    const fixture = createPi();

    legionExtension(fixture.pi);
    const claimCommand = fixture.commands.find(
      (command) => command.name === "legion-claim-controller"
    );
    if (claimCommand === undefined) throw new Error("controller claim command was not registered");

    await expect(
      claimCommand.handler("", {
        cwd: "/tmp/legion-workspace",
        sessionManager: { getSessionId: () => "ses_interactive" },
        ui: { notify: () => undefined },
      })
    ).rejects.toThrow(
      "LEGION_CONTROLLER_SECRET or LEGION_CONTROLLER_SECRET_FILE is required to claim the controller. Launch OMP with one of them in its environment before running /legion-claim-controller."
    );
  });
  test("boots a phase worker from its environment and reports readiness to the daemon", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "REPO-42";
    const issue = "REPO-43";
    const role: LegionRole = "tester";
    const token = roleToken("omp", issue, role);
    const workspace = await createJjWorkspace();

    const { context } = await bootWorker({
      role,
      tree,
      issue,
      sessionId: "ses_worker",
      workspace,
      requests,
    });

    expect(requests.map((request) => request.path)).toEqual([
      "/v1/interests/subscribe",
      "/legion/v1/worker/started",
      "/v1/interests/subscribe",
      "/v1/roles/set",
      "/legion/v1/worker/ready",
    ]);
    expect(requests[1]).toEqual({
      path: "/legion/v1/worker/started",
      body: {
        tree,
        issue,
        role,
        bootToken: "boot-ses_worker",
        sessionId: "ses_worker",
        agentId: "session",
        ompSessionFile: "/tmp/session.jsonl",
      },
    });
    expect(requests.find((request) => request.path === "/v1/roles/set")).toEqual({
      path: "/v1/roles/set",
      body: { session_id: "ses_worker", role: token },
    });
    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/worker/ready",
      body: { tree, issue, role, sessionId: "ses_worker", secret: "worker-secret", generation: 1 },
    });
    expect(
      natsConnections.some((connection) => connection.name.startsWith("legion-control-"))
    ).toBe(false);
    expect(context.sessionManager.getSessionId()).toBe("ses_worker");
  });
  test("does nothing for a session with no Legion environment markers", async () => {
    const requests: { readonly path: string }[] = [];
    process.env.ENVOY_URL = "http://envoy.test";
    delete process.env.LEGION_TREE;
    delete process.env.LEGION_ROLE;
    delete process.env.LEGION_CONTROLLER;
    delete process.env.LEGION_DAEMON_URL;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      requests.push({ path: url.pathname });
      return Response.json({
        session_id: "ses_plain",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");

    await sessionStart({}, sessionContext("ses_plain"));

    // Envoy's own baseline self-registration (every session subscribes to its
    // own agent subject) still fires; nothing Legion-specific does.
    expect(requests.some((request) => request.path.startsWith("/legion/"))).toBe(false);
    expect(requests.some((request) => request.path === "/v1/roles/set")).toBe(false);
    expect(fixture.tools.find((tool) => tool.name === "legion")).toBeUndefined();
  });
  test("never bootstraps, claims a role, or exits for a subagent session, even with root-architect environment", async () => {
    const requests: { readonly path: string }[] = [];
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error(`exitProcess(${code})`);
    });
    const { childFile } = await createSubagentTranscriptPaths();
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-subagent-root";
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = "REPO-42";
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      requests.push({ path: url.pathname });
      return Response.json({
        session_id: "ses_sub_root",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const toolCall = fixture.handlers.get("tool_call");
    if (sessionStart === undefined || toolCall === undefined) {
      throw new Error("session_start or tool_call handler was not registered");
    }
    const context = sessionContext("ses_sub_root", childFile);

    await sessionStart({}, context);

    expect(requests.some((request) => request.path.startsWith("/legion/"))).toBe(false);
    expect(requests.some((request) => request.path === "/v1/roles/set")).toBe(false);
    expect(exits).toEqual([]);
    expect(fixture.tools.find((tool) => tool.name === "legion")).toBeUndefined();

    // No tool gate was installed for this session either: a plain bash call, which an
    // unregistered root/phase worker would otherwise have blocked, passes through untouched.
    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "call-sub-root-bash", input: { command: "ls" } },
        context
      )
    ).resolves.toBeUndefined();
  });
  test("never bootstraps, claims a role, or exits for a subagent session, even with phase-worker environment", async () => {
    const requests: { readonly path: string }[] = [];
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error(`exitProcess(${code})`);
    });
    const { childFile } = await createSubagentTranscriptPaths();
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "1";
    process.env.LEGION_BOOT_TOKEN = "boot-subagent-worker";
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ROLE = "implementer";
    process.env.LEGION_ISSUE = "REPO-43";
    process.env.LEGION_WORKSPACE = "/tmp/legion-workspace";
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      requests.push({ path: url.pathname });
      return Response.json({
        session_id: "ses_sub_worker",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const toolCall = fixture.handlers.get("tool_call");
    if (sessionStart === undefined || toolCall === undefined) {
      throw new Error("session_start or tool_call handler was not registered");
    }
    const context = sessionContext("ses_sub_worker", childFile);

    await sessionStart({}, context);

    expect(requests.some((request) => request.path.startsWith("/legion/"))).toBe(false);
    expect(requests.some((request) => request.path === "/v1/roles/set")).toBe(false);
    expect(exits).toEqual([]);
    expect(fixture.tools.find((tool) => tool.name === "legion")).toBeUndefined();

    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "call-sub-worker-bash", input: { command: "ls" } },
        context
      )
    ).resolves.toBeUndefined();
  });
  test("throws naming the missing variable when a phase worker boots without LEGION_BOOT_TOKEN", async () => {
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ISSUE = "REPO-43";
    process.env.LEGION_ROLE = "tester";
    delete process.env.LEGION_BOOT_TOKEN;
    globalThis.fetch = (async (_input, _init) => Response.json({})) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");

    await expect(sessionStart({}, sessionContext("ses_missing_boot_token"))).rejects.toThrow(
      "LEGION_BOOT_TOKEN is required for Legion"
    );
  });
  test("recovers a live worker's capability after the daemon loses its secret", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "REPO-42";
    const issue = "REPO-43";
    const role: LegionRole = "tester";
    const token = roleToken("omp", issue, role);
    const workspace = await createJjWorkspace();
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_BOOT_TOKEN = "boot-worker-recovery";
    process.env.LEGION_GENERATION = "1";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ISSUE = issue;
    process.env.LEGION_ROLE = role;
    process.env.LEGION_WORKSPACE = workspace;
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    temporaryPaths.push(stateDir);
    process.env.LEGION_STATE_DIR = stateDir;
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/worker/started") {
        return Response.json({
          roleToken: token,
          secret: "stale-secret",
          gitName: "Legion Tester",
          gitEmail: "tester@example.test",
        });
      }
      if (url.pathname === "/legion/v1/worker/ready") return Response.json({});
      if (url.pathname === "/legion/v1/grants") {
        if ((body as { readonly secret?: unknown } | undefined)?.secret === "stale-secret") {
          return Response.json({ error: "Invalid session secret" }, { status: 403 });
        }
        return Response.json({ grantId: "grant-recovered", expiresAt: "2099-01-01T00:00:00.000Z" });
      }
      if (url.pathname === "/legion/v1/worker-session") {
        return Response.json({ tree, issue, role, secret: "recovered-secret" });
      }
      return Response.json({
        session_id: "ses_worker_recovery",
        machine_id: "machine",
        dir: workspace,
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const toolCall = fixture.handlers.get("tool_call");
    if (sessionStart === undefined || toolCall === undefined) {
      throw new Error("worker lifecycle handlers were not registered");
    }
    const context = { ...sessionContext("ses_worker_recovery"), cwd: workspace };
    await sessionStart({}, context);

    const result = await toolCall(
      { toolName: "bash", toolCallId: "call-1", input: { command: "echo hi" } },
      context
    );

    if (
      typeof result !== "object" ||
      result === null ||
      !("input" in result) ||
      typeof result.input !== "object" ||
      result.input === null ||
      !("command" in result.input) ||
      typeof result.input.command !== "string"
    ) {
      throw new Error("worker shell was not rewritten after recovering its secret");
    }
    expect(result.input.command.split("\n")[0]).toBe("export LEGION_GRANT='grant-recovered'");
    expect(requests.filter((request) => request.path === "/legion/v1/grants")).toHaveLength(2);
    expect(requests.find((request) => request.path === "/legion/v1/worker-session")).toEqual({
      path: "/legion/v1/worker-session",
      body: { sessionId: "ses_worker_recovery", recoveryToken: "boot-worker-recovery" },
    });
  });
  test("boots a phase worker with the boot token read from LEGION_BOOT_TOKEN_FILE, ignoring LEGION_BOOT_TOKEN", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();
    const secretsDir = await mkdtemp(path.join(os.tmpdir(), "legion-secrets-"));
    temporaryPaths.push(secretsDir);
    const bootTokenFile = path.join(secretsDir, "legion-omp-repo-43-tester");
    await writeFile(bootTokenFile, "file-boot-token\n");
    process.env.LEGION_BOOT_TOKEN_FILE = bootTokenFile;

    await bootWorker({ role: "tester", workspace, requests, sessionId: "ses_file_worker" });

    expect(requests.find((request) => request.path === "/legion/v1/worker/started")).toMatchObject({
      body: { bootToken: "file-boot-token" },
    });
  });
  test("exits the phase worker naming LEGION_BOOT_TOKEN_FILE and its path when the file is unreadable, never falling back to LEGION_BOOT_TOKEN", async () => {
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ISSUE = "REPO-43";
    process.env.LEGION_ROLE = "tester";
    process.env.LEGION_BOOT_TOKEN = "decoy";
    process.env.LEGION_BOOT_TOKEN_FILE = "/nonexistent/legion-secrets/tester";
    globalThis.fetch = (async (_input, _init) => Response.json({})) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");

    await expect(sessionStart({}, sessionContext("ses_bad_boot_file"))).rejects.toThrow(
      "LEGION_BOOT_TOKEN_FILE names /nonexistent/legion-secrets/tester, which could not be read"
    );
  });
  test("uses the recovery token from LEGION_BOOT_TOKEN_FILE when the daemon has lost a worker's secret", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "REPO-42";
    const issue = "REPO-43";
    const role: LegionRole = "tester";
    const token = roleToken("omp", issue, role);
    const workspace = await createJjWorkspace();
    const secretsDir = await mkdtemp(path.join(os.tmpdir(), "legion-secrets-"));
    temporaryPaths.push(secretsDir);
    const bootTokenFile = path.join(secretsDir, token);
    await writeFile(bootTokenFile, "boot-worker-recovery\n");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_BOOT_TOKEN = "decoy";
    process.env.LEGION_BOOT_TOKEN_FILE = bootTokenFile;
    process.env.LEGION_GENERATION = "1";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ISSUE = issue;
    process.env.LEGION_ROLE = role;
    process.env.LEGION_WORKSPACE = workspace;
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    temporaryPaths.push(stateDir);
    process.env.LEGION_STATE_DIR = stateDir;
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/worker/started") {
        return Response.json({
          roleToken: token,
          secret: "stale-secret",
          gitName: "Legion Tester",
          gitEmail: "tester@example.test",
        });
      }
      if (url.pathname === "/legion/v1/worker/ready") return Response.json({});
      if (url.pathname === "/legion/v1/grants") {
        if ((body as { readonly secret?: unknown } | undefined)?.secret === "stale-secret") {
          return Response.json({ error: "Invalid session secret" }, { status: 403 });
        }
        return Response.json({ grantId: "grant-recovered", expiresAt: "2099-01-01T00:00:00.000Z" });
      }
      if (url.pathname === "/legion/v1/worker-session") {
        return Response.json({ tree, issue, role, secret: "recovered-secret" });
      }
      return Response.json({
        session_id: "ses_worker_file_recovery",
        machine_id: "machine",
        dir: workspace,
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const toolCall = fixture.handlers.get("tool_call");
    if (sessionStart === undefined || toolCall === undefined) {
      throw new Error("worker lifecycle handlers were not registered");
    }
    const context = { ...sessionContext("ses_worker_file_recovery"), cwd: workspace };
    await sessionStart({}, context);

    await toolCall(
      { toolName: "bash", toolCallId: "call-1", input: { command: "echo hi" } },
      context
    );

    expect(requests.find((request) => request.path === "/legion/v1/worker/started")).toMatchObject({
      body: { bootToken: "boot-worker-recovery" },
    });
    expect(requests.find((request) => request.path === "/legion/v1/worker-session")).toEqual({
      path: "/legion/v1/worker-session",
      body: { sessionId: "ses_worker_file_recovery", recoveryToken: "boot-worker-recovery" },
    });
  });
  test("claims the controller with the secret read from LEGION_CONTROLLER_SECRET_FILE", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const token = "legion-omp-controller";
    const secretsDir = await mkdtemp(path.join(os.tmpdir(), "legion-secrets-"));
    temporaryPaths.push(secretsDir);
    const secretFile = path.join(secretsDir, token);
    await writeFile(secretFile, "file-controller-secret\n");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_CONTROLLER = "1";
    process.env.LEGION_CONTROLLER_SECRET = "decoy";
    process.env.LEGION_CONTROLLER_SECRET_FILE = secretFile;
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/state") return Response.json(redactedLegionState("omp"));
      if (url.pathname === "/legion/v1/controller/ready") return Response.json({});
      return Response.json({
        session_id: body?.session_id,
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();

    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const claimCommand = fixture.commands.find(
      (command) => command.name === "legion-claim-controller"
    );
    if (sessionStart === undefined || claimCommand === undefined) {
      throw new Error("controller handlers were not registered");
    }
    await sessionStart({}, sessionContext("ses_controller"));
    await sessionStart({}, sessionContext("ses_interactive"));
    await claimCommand.handler("", sessionContext("ses_interactive"));

    expect(requests.filter((request) => request.path === "/legion/v1/controller/ready")).toEqual([
      {
        path: "/legion/v1/controller/ready",
        body: { secret: "file-controller-secret", sessionId: "ses_controller" },
      },
      {
        path: "/legion/v1/controller/ready",
        body: { secret: "file-controller-secret", sessionId: "ses_interactive" },
      },
    ]);
  });
  test("registers the Legion tool for a sub-architect worker", async () => {
    const workspace = await createJjWorkspace();
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_BOOT_TOKEN = "boot-sub-architect";
    process.env.LEGION_GENERATION = "1";
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ISSUE = "REPO-43";
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_WORKSPACE = workspace;
    const token = roleToken("omp", "REPO-43", "architect");
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/worker/started") {
        return Response.json({
          roleToken: token,
          secret: "worker-secret",
          gitName: "Legion Architect",
          gitEmail: "architect@example.test",
        });
      }
      if (url.pathname === "/legion/v1/worker/ready") return Response.json({});
      return Response.json({
        session_id: "ses_sub_architect",
        machine_id: "machine",
        dir: workspace,
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    const toolsBeforeLegion = fixture.tools.length;
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");

    await sessionStart({}, { ...sessionContext("ses_sub_architect"), cwd: workspace });

    expect(fixture.tools).toHaveLength(toolsBeforeLegion + 1);
    expect(fixture.tools.at(-1)?.name).toBe("legion");
    expect(fixture.activeTools).toContain("legion");
  });
  test("allows a single `legion ...` bash invocation for an architect worker but blocks chaining and other commands", async () => {
    const workspace = await createJjWorkspace();
    const { toolCall, context } = await bootWorker({
      role: "architect",
      workspace,
      extraRoutes: (url) => {
        if (url.pathname === "/legion/v1/grants") {
          return Response.json({
            grantId: "grant-architect",
            expiresAt: "2099-01-01T00:00:00.000Z",
          });
        }
        return undefined;
      },
    });
    const denied = "the architect delegates all code work to phase workers";
    const isAllowed = async (command: string): Promise<boolean> => {
      const result = await toolCall(
        { toolName: "bash", toolCallId: `call-${command}`, input: { command } },
        context
      );
      return !(typeof result === "object" && result !== null && "block" in result && result.block);
    };

    expect(await isAllowed("legion handoff complete --summary x")).toBe(true);
    expect(await isAllowed("legion gh -- pr view 1")).toBe(true);
    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "call-chained",
          input: { command: "echo hi && legion gh" },
        },
        context
      )
    ).resolves.toEqual({ block: true, reason: denied });
    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "call-non-legion", input: { command: "rm -rf x" } },
        context
      )
    ).resolves.toEqual({ block: true, reason: denied });
  });
  test("binds a booted worker's jj identity to LEGION_WORKSPACE", async () => {
    const workspace = await createJjWorkspace();

    await bootWorker({ role: "implementer", workspace });

    expect(await jjConfig(workspace, "user.name")).toBe("Legion Worker");
    expect(await jjConfig(workspace, "user.email")).toBe("worker@example.test");
  });
  test("restricts phase-worker tool access per LEGION_ROLE", async () => {
    const blockedReason = (role: LegionRole, toolName: string): string | undefined => {
      if (role === "architect" && ["edit", "write", "apply_patch"].includes(toolName)) {
        return "the architect delegates all code work to phase workers";
      }
      if (role === "architect" && toolName === "task") {
        return "the architect delegates only through spawn_worker; Legion runs one agent per process";
      }
      if (role === "reviewer" && ["edit", "write", "apply_patch"].includes(toolName)) {
        return "the reviewer edits nothing except the final .legion/ cleanup commit via bash";
      }
      if (role === "merger" && ["edit", "write", "apply_patch", "task"].includes(toolName)) {
        return "the merger only verifies and reports";
      }
      return undefined;
    };
    const roles: readonly LegionRole[] = [
      "planner",
      "implementer",
      "tester",
      "reviewer",
      "merger",
      "architect",
    ];
    const toolNames = ["edit", "write", "apply_patch", "task", "hub"];

    for (const role of roles) {
      const workspace = await createJjWorkspace();
      const { toolCall, context } = await bootWorker({ role, workspace, sessionId: `ses_${role}` });
      for (const toolName of toolNames) {
        const reason = blockedReason(role, toolName);
        const result = await toolCall(
          { toolName, toolCallId: `call-${role}-${toolName}`, input: {} },
          context
        );
        if (reason === undefined) expect(result).toBeUndefined();
        else expect(result).toEqual({ block: true, reason });
      }
    }
  });
  test("blocks the architect's task tool but allows an implementer's, per the one-agent-per-process rule", async () => {
    const architectWorkspace = await createJjWorkspace();
    const { toolCall: architectToolCall, context: architectContext } = await bootWorker({
      role: "architect",
      workspace: architectWorkspace,
      sessionId: "ses_architect_task",
    });
    await expect(
      architectToolCall(
        { toolName: "task", toolCallId: "call-architect-task", input: {} },
        architectContext
      )
    ).resolves.toEqual({
      block: true,
      reason:
        "the architect delegates only through spawn_worker; Legion runs one agent per process",
    });

    const implementerWorkspace = await createJjWorkspace();
    const { toolCall: implementerToolCall, context: implementerContext } = await bootWorker({
      role: "implementer",
      workspace: implementerWorkspace,
      sessionId: "ses_implementer_task",
    });
    await expect(
      implementerToolCall(
        { toolName: "task", toolCallId: "call-implementer-task", input: {} },
        implementerContext
      )
    ).resolves.toBeUndefined();
  });
  test("allows xd:// tool-device writes through the mutation gate but still blocks real file writes", async () => {
    const blockedReason = (role: LegionRole): string =>
      role === "merger"
        ? "the merger only verifies and reports"
        : role === "reviewer"
          ? "the reviewer edits nothing except the final .legion/ cleanup commit via bash"
          : "the architect delegates all code work to phase workers";

    for (const role of ["architect", "reviewer", "merger"] as const) {
      const workspace = await createJjWorkspace();
      const { toolCall, context } = await bootWorker({
        role,
        workspace,
        sessionId: `ses_${role}_xd`,
        extraRoutes: (url) => {
          if (url.pathname === "/legion/v1/grants") {
            return Response.json({
              grantId: `grant-${role}`,
              expiresAt: "2099-01-01T00:00:00.000Z",
            });
          }
          return undefined;
        },
      });

      // A tool-device invocation (write to an `xd://` path) is a tool call, not a file
      // mutation, and must pass for every gated role.
      await expect(
        toolCall(
          {
            toolName: "write",
            toolCallId: `call-${role}-xd-ok`,
            input: { path: "xd://dispatch_ask", content: "{}" },
          },
          context
        )
      ).resolves.toBeUndefined();

      // A real filesystem write is still blocked.
      await expect(
        toolCall(
          {
            toolName: "write",
            toolCallId: `call-${role}-fs`,
            input: { path: "/tmp/whatever.ts", content: "x" },
          },
          context
        )
      ).resolves.toEqual({ block: true, reason: blockedReason(role) });

      // A malformed/missing `path` never qualifies as a tool-device invocation: it is still
      // treated as a mutation and blocked.
      await expect(
        toolCall(
          { toolName: "write", toolCallId: `call-${role}-bad-path`, input: { path: 42 } },
          context
        )
      ).resolves.toEqual({ block: true, reason: blockedReason(role) });
      await expect(
        toolCall({ toolName: "write", toolCallId: `call-${role}-no-path`, input: {} }, context)
      ).resolves.toEqual({ block: true, reason: blockedReason(role) });
    }
  });
  test("rewrites a booted worker's bash calls with a fresh daemon grant and a PATH-scoped gh shim", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();
    const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-state-"));
    temporaryPaths.push(stateDir);
    process.env.LEGION_STATE_DIR = stateDir;

    const { toolCall, context } = await bootWorker({
      role: "reviewer",
      workspace,
      requests,
      extraRoutes: (url) => {
        if (url.pathname === "/legion/v1/grants") {
          return Response.json({ grantId: "grant-1", expiresAt: "2099-01-01T00:00:00.000Z" });
        }
        return undefined;
      },
    });

    const result = await toolCall(
      {
        toolName: "bash",
        toolCallId: "call-1",
        input: { command: "echo GH_TOKEN=$GH_TOKEN; echo CONFIG=$GH_CONFIG_DIR; which gh" },
      },
      context
    );

    if (
      typeof result !== "object" ||
      result === null ||
      !("input" in result) ||
      typeof result.input !== "object" ||
      result.input === null ||
      !("command" in result.input) ||
      typeof result.input.command !== "string"
    ) {
      throw new Error("worker shell was not rewritten with a daemon grant");
    }
    expect(result.input.command.split("\n")[0]).toBe("export LEGION_GRANT='grant-1'");
    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/grants",
      body: {
        tree: "REPO-42",
        issue: "REPO-43",
        sessionId: "ses_reviewer",
        secret: "worker-secret",
      },
    });
    expect(await readFile(path.join(stateDir, "worker-bin", "gh"), "utf8")).toContain(
      'exec legion gh -- "$@"'
    );

    const output = await commandOutput(["sh", "-c", result.input.command], workspace, {
      ...process.env,
      GH_TOKEN: "ambient-personal-token",
      GITHUB_TOKEN: "ambient-personal-token",
      GH_HOST: "ambient-host",
    });
    expect(output).toBe(
      [
        "GH_TOKEN=",
        `CONFIG=${path.join(stateDir, "gh")}`,
        path.join(stateDir, "worker-bin", "gh"),
      ].join("\n")
    );
  });
  test("blocks a booted worker's bash calls when the daemon refuses to mint a grant", async () => {
    const workspace = await createJjWorkspace();

    const { toolCall, context } = await bootWorker({
      role: "reviewer",
      workspace,
      extraRoutes: (url) => {
        if (url.pathname === "/legion/v1/grants") {
          return Response.json({ error: "grant minting unavailable" }, { status: 503 });
        }
        return undefined;
      },
    });

    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "call-grant-refused",
          input: { command: "env | grep LEGION" },
        },
        context
      )
    ).resolves.toEqual({
      block: true,
      reason: 'POST /legion/v1/grants failed with 503: {"error":"grant minting unavailable"}',
    });
  });
  test("blocks a bash call from a worker session that has not completed its boot handshake", async () => {
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ROLE = "implementer";
    const fixture = createPi();
    legionExtension(fixture.pi);
    const toolCall = fixture.handlers.get("tool_call");
    if (toolCall === undefined) throw new Error("worker tool_call handler was not registered");

    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "call-unregistered-worker",
          input: { command: "jj git fetch" },
        },
        sessionContext("ses_unregistered_worker")
      )
    ).resolves.toEqual({
      block: true,
      reason: "Legion worker session is not registered; cannot mint LEGION_GRANT",
    });
  });
  test("does not block bash calls from the controller session", async () => {
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_CONTROLLER = "1";
    process.env.LEGION_CONTROLLER_SECRET = "controller-secret";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/state") return Response.json(redactedLegionState("omp"));
      if (url.pathname === "/legion/v1/controller/ready") return Response.json({});
      return Response.json({
        session_id: "ses_controller_bash",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const toolCall = fixture.handlers.get("tool_call");
    if (sessionStart === undefined || toolCall === undefined) {
      throw new Error("controller lifecycle handlers were not registered");
    }
    const context = sessionContext("ses_controller_bash");
    await sessionStart({}, context);

    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "call-controller-bash",
          input: { command: "legion state" },
        },
        context
      )
    ).resolves.toBeUndefined();
  });
  test("materializes the session transcript before the boot handshake", async () => {
    const order: string[] = [];
    const tree = "REPO-42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-ensure-on-disk";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      order.push(`fetch:${url.pathname}`);
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: token },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      return Response.json({
        session_id: "ses_ensure_on_disk",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("root lifecycle handler was not registered");

    const context = sessionContext("ses_ensure_on_disk", "/tmp/legion-ensure.jsonl", async () => {
      order.push("ensureOnDisk");
    });
    await sessionStart({}, context);

    expect(order.indexOf("ensureOnDisk")).toBeGreaterThanOrEqual(0);
    expect(order.indexOf("ensureOnDisk")).toBeLessThan(
      order.indexOf("fetch:/legion/v1/process/started")
    );
  });
  test("exits the process when the daemon rejects a stale boot token at worker/started", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    const tree = "REPO-42";
    const issue = "REPO-43";
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_BOOT_TOKEN = "stale-boot-token";
    process.env.LEGION_GENERATION = "1";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ISSUE = issue;
    process.env.LEGION_ROLE = "implementer";
    process.env.LEGION_WORKSPACE = "/tmp/legion-workspace";
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/worker/started") {
        return Response.json({ error: "boot token expired" }, { status: 403 });
      }
      return Response.json({});
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("worker lifecycle handler was not registered");

    await expect(sessionStart({}, sessionContext("ses_stale_boot"))).rejects.toThrow(
      "process would exit"
    );
    expect(exits).toEqual([1]);
  });
  test("retries worker/ready three times on a 503 then exits once", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    const workspace = await createJjWorkspace();
    const tree = "REPO-42";
    const issue = "REPO-43";
    const role: LegionRole = "implementer";
    const token = roleToken("omp", issue, role);
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_BOOT_TOKEN = "boot-retries-after-started";
    process.env.LEGION_GENERATION = "1";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ISSUE = issue;
    process.env.LEGION_ROLE = role;
    process.env.LEGION_WORKSPACE = workspace;
    let readyAttempts = 0;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/worker/started") {
        return Response.json({
          roleToken: token,
          secret: "worker-secret",
          gitName: "Legion Worker",
          gitEmail: "worker@example.test",
        });
      }
      if (url.pathname === "/legion/v1/worker/ready") {
        readyAttempts += 1;
        return Response.json({ error: "daemon unavailable" }, { status: 503 });
      }
      return Response.json({
        session_id: "ses_retries_after_started",
        machine_id: "machine",
        dir: workspace,
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("worker lifecycle handler was not registered");

    const context = { ...sessionContext("ses_retries_after_started"), cwd: workspace };
    await expect(sessionStart({}, context)).rejects.toThrow("process would exit");

    expect(readyAttempts).toBe(3);
    expect(exits).toEqual([1]);
  });
  test("does not retry a worker/ready 404 and exits once after propagating", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    const workspace = await createJjWorkspace();
    const tree = "REPO-42";
    const issue = "REPO-43";
    const role: LegionRole = "implementer";
    const token = roleToken("omp", issue, role);
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_BOOT_TOKEN = "boot-ready-not-found";
    process.env.LEGION_GENERATION = "1";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ISSUE = issue;
    process.env.LEGION_ROLE = role;
    process.env.LEGION_WORKSPACE = workspace;
    let readyAttempts = 0;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/worker/started") {
        return Response.json({
          roleToken: token,
          secret: "worker-secret",
          gitName: "Legion Worker",
          gitEmail: "worker@example.test",
        });
      }
      if (url.pathname === "/legion/v1/worker/ready") {
        readyAttempts += 1;
        return Response.json({ error: "ready endpoint not found" }, { status: 404 });
      }
      return Response.json({
        session_id: "ses_ready_not_found",
        machine_id: "machine",
        dir: workspace,
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("worker lifecycle handler was not registered");

    const context = { ...sessionContext("ses_ready_not_found"), cwd: workspace };
    await expect(sessionStart({}, context)).rejects.toThrow("process would exit");

    expect(readyAttempts).toBe(1);
    expect(exits).toEqual([1]);
  });
  test("exits the process when worker/ready is rejected with 403 (auth)", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    const workspace = await createJjWorkspace();
    const tree = "REPO-42";
    const issue = "REPO-43";
    const role: LegionRole = "implementer";
    const token = roleToken("omp", issue, role);
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_BOOT_TOKEN = "boot-auth-rejected-after-started";
    process.env.LEGION_GENERATION = "1";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ISSUE = issue;
    process.env.LEGION_ROLE = role;
    process.env.LEGION_WORKSPACE = workspace;
    let readyAttempts = 0;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/worker/started") {
        return Response.json({
          roleToken: token,
          secret: "worker-secret",
          gitName: "Legion Worker",
          gitEmail: "worker@example.test",
        });
      }
      if (url.pathname === "/legion/v1/worker/ready") {
        readyAttempts += 1;
        return Response.json({ error: "session capability revoked" }, { status: 403 });
      }
      return Response.json({
        session_id: "ses_auth_rejected_after_started",
        machine_id: "machine",
        dir: workspace,
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("worker lifecycle handler was not registered");

    const context = { ...sessionContext("ses_auth_rejected_after_started"), cwd: workspace };
    await expect(sessionStart({}, context)).rejects.toThrow("process would exit");
    expect(exits).toEqual([1]);
    expect(readyAttempts).toBe(1);
  });
  test("exits the process when the daemon rejects a stale boot token at process/started", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    const tree = "REPO-42";
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "stale-root-boot-token";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({ error: "boot token expired" }, { status: 403 });
      }
      return Response.json({});
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("root lifecycle handler was not registered");

    await expect(sessionStart({}, sessionContext("ses_root_stale_boot"))).rejects.toThrow(
      "process would exit"
    );
    expect(exits).toEqual([1]);
  });
  test("retries a transient 5xx process/ready and completes root bootstrap without exiting", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    const tree = "REPO-42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-root-retries-after-started";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    let readyAttempts = 0;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: token },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      if (url.pathname === "/legion/v1/process/ready") {
        readyAttempts += 1;
        if (readyAttempts === 1) {
          return Response.json({ error: "daemon unavailable" }, { status: 500 });
        }
        return Response.json({});
      }
      return Response.json({
        session_id: "ses_root_retries_after_started",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("root lifecycle handler was not registered");

    await sessionStart({}, sessionContext("ses_root_retries_after_started"));

    expect(readyAttempts).toBe(2);
    expect(exits).toEqual([]);
  });
  test("exits the process when process/ready is rejected with 403 (auth)", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    const tree = "REPO-42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-root-auth-rejected-after-started";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    let readyAttempts = 0;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: token },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      if (url.pathname === "/legion/v1/process/ready") {
        readyAttempts += 1;
        return Response.json({ error: "session capability revoked" }, { status: 403 });
      }
      return Response.json({
        session_id: "ses_root_auth_rejected_after_started",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("root lifecycle handler was not registered");

    await expect(
      sessionStart({}, sessionContext("ses_root_auth_rejected_after_started"))
    ).rejects.toThrow("process would exit");
    expect(exits).toEqual([1]);
    expect(readyAttempts).toBe(1);
  });
  test("registers only the Legion tool for a confirmed architect session", async () => {
    const tree = "REPO-42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-legion-tool";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: token },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      return Response.json({
        session_id: "ses_architect",
        machine_id: "machine",
        dir: context.cwd,
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    const toolsBeforeLegion = fixture.tools.length;
    const context = sessionContext("ses_architect");

    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");
    await sessionStart({}, context);

    expect(fixture.tools).toHaveLength(toolsBeforeLegion + 1);
    expect(fixture.tools.at(-1)?.name).toBe("legion");
  });
  test("maps every remaining legion operation to its daemon proxy endpoint", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "REPO-42";
    const issue = "REPO-43";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-operations";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: token },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      if (url.pathname === "/legion/v1/waves/release") return Response.json({ released: [issue] });
      if (url.pathname === "/legion/v1/worker/spawn")
        return Response.json({ status: "spawned", roleToken: "role-token-implementer" });
      if (url.pathname.startsWith("/legion/v1/")) return Response.json({});
      return Response.json({
        session_id: "ses_architect",
        machine_id: "machine",
        dir: context.cwd,
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    const context = sessionContext("ses_architect");

    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");
    await sessionStart({}, context);
    const legion = fixture.tools.find((tool) => tool.name === "legion");
    if (legion === undefined) throw new Error("legion tool was not registered");

    const cases: {
      readonly input: Record<string, unknown>;
      readonly request: { readonly path: string; readonly body: unknown };
      readonly details: Record<string, unknown>;
    }[] = [
      {
        input: { op: "release_wave", issues: [issue] },
        request: {
          path: "/legion/v1/waves/release",
          body: { tree, issues: [issue], sessionId: "ses_architect", secret: "root-secret" },
        },
        details: { released: [issue] },
      },
      {
        input: { op: "set_status", issue, status: "todo" },
        request: {
          path: "/legion/v1/issues/status",
          body: {
            tree,
            issue,
            status: "todo",
            sessionId: "ses_architect",
            secret: "root-secret",
          },
        },
        details: {},
      },
      {
        input: { op: "register_gate", issue, askId: "ask-1" },
        request: {
          path: "/legion/v1/gates/register",
          body: {
            tree,
            issue,
            askId: "ask-1",
            sessionId: "ses_architect",
            secret: "root-secret",
          },
        },
        details: {},
      },
      {
        input: { op: "spawn_worker", issue, role: "implementer", task: "Implement the feature" },
        request: {
          path: "/legion/v1/worker/spawn",
          body: {
            tree,
            issue,
            role: "implementer",
            task: "Implement the feature",
            sessionId: "ses_architect",
            secret: "root-secret",
          },
        },
        details: { status: "spawned", roleToken: "role-token-implementer" },
      },
      {
        input: { op: "escalate", kind: "capacity", context: { reason: "No slots" } },
        request: {
          path: "/legion/v1/escalate",
          body: {
            tree,
            kind: "capacity",
            context: { reason: "No slots" },
            sessionId: "ses_architect",
            secret: "root-secret",
          },
        },
        details: {},
      },
    ];

    for (const entry of cases) {
      const result = await legion.execute(
        "call-remaining",
        entry.input,
        undefined,
        undefined,
        context
      );
      expect(result.isError).toBeUndefined();
      expect(requests.at(-1)).toEqual(entry.request);
      expect(result).toEqual({
        content: [{ type: "text", text: JSON.stringify(entry.details) }],
        details: entry.details,
      });
    }

    const requestCountBeforeRejectedField = requests.length;
    expect(
      await legion.execute(
        "call-extra-field",
        { op: "release_wave", issues: [issue], issue },
        undefined,
        undefined,
        context
      )
    ).toEqual({
      content: [{ type: "text", text: 'release_wave does not accept field "issue"' }],
      details: {},
      isError: true,
    });
    expect(requests).toHaveLength(requestCountBeforeRejectedField);
  });
  test("validates escalate's context and release_wave's issues through the real registered tool schema, not the mocked direct execute", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "REPO-42";
    const issue = "REPO-43";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-schema";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    const context = sessionContext("ses_architect");
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: token },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      if (url.pathname === "/legion/v1/waves/release") return Response.json({ released: [issue] });
      if (url.pathname.startsWith("/legion/v1/")) return Response.json({});
      return Response.json({
        session_id: "ses_architect",
        machine_id: "machine",
        dir: context.cwd,
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    const realPi: TestPi = { ...fixture.pi, zod: createRealZodPi() };

    legionExtension(realPi as never);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");
    await sessionStart({}, context);
    const legion = fixture.tools.find((tool) => tool.name === "legion");
    if (legion === undefined) throw new Error("legion tool was not registered");
    const schema = legion.parameters as { parse: (value: unknown) => Record<string, unknown> };

    const parsedEscalate = schema.parse({
      op: "escalate",
      kind: "capacity",
      context: { reason: "No slots" },
    });
    expect(parsedEscalate.context).toEqual({ reason: "No slots" });
    const escalateResult = await legion.execute(
      "call-escalate",
      parsedEscalate,
      undefined,
      undefined,
      context
    );
    expect(escalateResult.isError).toBeUndefined();
    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/escalate",
      body: {
        tree,
        kind: "capacity",
        context: { reason: "No slots" },
        sessionId: "ses_architect",
        secret: "root-secret",
      },
    });

    const parsedRelease = schema.parse({ op: "release_wave", issues: [issue] });
    expect(parsedRelease.issues).toEqual([issue]);
    const releaseResult = await legion.execute(
      "call-release",
      parsedRelease,
      undefined,
      undefined,
      context
    );
    expect(releaseResult.isError).toBeUndefined();
    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/waves/release",
      body: { tree, issues: [issue], sessionId: "ses_architect", secret: "root-secret" },
    });
  });
  test("withholds architect-only Legion tools until an architect session is confirmed", () => {
    const fixture = createPi();

    legionExtension(fixture.pi);

    expect(fixture.tools.find((tool) => tool.name === "legion")).toBeUndefined();
  });
  test("reclaims the architect in code and acknowledges without sending a model message", async () => {
    const fixture = createPi();
    let reclaims = 0;
    let acknowledgements = 0;

    await handleLegionControlDirective(
      {
        type: "reclaim-architect",
        redeliver: { topic: "notifications.role.architect", payload: "{}", eventId: "evt-2" },
      },
      {
        reclaimArchitect: async () => {
          reclaims += 1;
        },
        acknowledge: () => {
          acknowledgements += 1;
        },
        reject: () => {
          throw new Error("unexpected control rejection");
        },
      }
    );

    expect(reclaims).toBe(1);
    expect(acknowledgements).toBe(1);
    expect(fixture.sentMessages).toEqual([]);
  });
  test("reports root process exit to the daemon on session shutdown", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "REPO-42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-process-exit";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: token },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      if (url.pathname.startsWith("/legion/v1/")) return Response.json({});
      return Response.json({
        session_id: "ses_architect",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    const context = sessionContext("ses_architect");

    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const sessionShutdown = fixture.handlers.get("session_shutdown");
    if (sessionStart === undefined || sessionShutdown === undefined) {
      throw new Error("root lifecycle handlers were not registered");
    }
    await sessionStart({}, context);
    await sessionShutdown({}, context);

    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/process/exit",
      body: { tree, generation: 3, sessionId: "ses_architect", secret: "root-secret" },
    });
  });
  test("blocks code tools in a root architect session", async () => {
    const tree = "REPO-42";
    const architectToken = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-architect-policy";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: architectToken },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      return Response.json({
        session_id: body?.session_id,
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [architectToken],
      });
    }) as typeof fetch;
    const fixture = createPi();
    const context = sessionContext("ses_policy_architect");

    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const toolCall = fixture.handlers.get("tool_call");
    if (sessionStart === undefined || toolCall === undefined) {
      throw new Error("architect policy handlers were not registered");
    }
    await sessionStart({}, context);

    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "architect-bash",
          input: { command: "echo should-not-run" },
        },
        context
      )
    ).resolves.toEqual({
      block: true,
      reason: "the architect delegates all code work to phase workers",
    });
  });
  test("ships a roles/<role>.md prompt file for every LegionRole", async () => {
    // The daemon `cat`s packages/pi-envoy/roles/${role}.md at spawn for every
    // phase-worker role, including a sub-architect (packages/daemon/src/daemon/processes.ts
    // launchWorker). A missing file 500s the spawn.
    for (const role of LEGION_ROLES) {
      const rolePath = path.join(import.meta.dir, "..", "roles", `${role}.md`);
      await access(rolePath);
      // A zero-byte file would pass the existence check above and boot a worker with no
      // instructions at all -- fail loudly on that instead of leaving it a silent runtime bug.
      expect((await readFile(rolePath, "utf8")).trim()).not.toBe("");
    }
  });
  test("keeps the Step-one skill-discovery section and closing envoy-addressing paragraph identical, word for word, across every roles/<role>.md", async () => {
    // Six independent files, six independent editors: this is the drift guard. A change to
    // shared prose in one file and not the other five goes red here instead of silently
    // diverging (round-1's epilogue fix already drifted into five different line-wrap widths
    // before this test existed). The Step-one paragraph is identical across all six EXCEPT
    // that implementer/tester/reviewer each end it with one extra sentence pointing at the
    // plan handoff's `requiredSkills.implement`/`.test`/`.review` key -- architect, planner,
    // and merger have no corresponding key, so that sentence is deliberately absent there.
    const normalize = (section: string): string => section.split(/\s+/).filter(Boolean).join(" ");
    const requiredSkillsSentence =
      "Then read the plan handoff's `requiredSkills` for your role and follow those too.";
    const stepOneBase: Record<string, string> = {};
    const rolesWithRequiredSkillsSentence: string[] = [];
    const closingParagraphs: Record<string, string> = {};
    for (const role of LEGION_ROLES) {
      const rolePath = path.join(import.meta.dir, "..", "roles", `${role}.md`);
      const text = await readFile(rolePath, "utf8");
      const heading = "## Step one: find this repository's skills";
      const headingStart = text.indexOf(heading);
      if (headingStart === -1) throw new Error(`${role}.md is missing the Step-one heading`);
      // Skip the blank line separating the heading from its paragraph -- stopping right after
      // the heading text would make paragraphEnd find that same blank line, comparing "" for
      // every role and passing even when the real paragraph diverges.
      const afterHeading = text.slice(headingStart + heading.length).replace(/^\s+/, "");
      const paragraphEnd = afterHeading.indexOf("\n\n");
      if (paragraphEnd === -1) throw new Error(`${role}.md's Step-one paragraph has no end`);
      const paragraph = normalize(afterHeading.slice(0, paragraphEnd));
      if (paragraph.endsWith(requiredSkillsSentence)) {
        rolesWithRequiredSkillsSentence.push(role);
        stepOneBase[role] = paragraph.slice(0, -requiredSkillsSentence.length).trim();
      } else {
        stepOneBase[role] = paragraph;
      }

      const closingMatch =
        /When\s+your\s+phase\s+is\s+done,\s+stay\s+in\s+this\s+session\s+afterwards:[\s\S]*$/.exec(
          text
        );
      if (!closingMatch) throw new Error(`${role}.md is missing the closing envoy paragraph`);
      closingParagraphs[role] = normalize(closingMatch[0]);
    }
    const [firstRole, ...restRoles] = LEGION_ROLES;
    if (firstRole === undefined) throw new Error("LEGION_ROLES is empty");
    for (const role of restRoles) {
      expect(stepOneBase[role]).toBe(stepOneBase[firstRole]);
      expect(closingParagraphs[role]).toBe(closingParagraphs[firstRole]);
    }
    expect(rolesWithRequiredSkillsSentence.sort()).toEqual(["implementer", "reviewer", "tester"]);
  });
});
