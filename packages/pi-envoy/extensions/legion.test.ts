import { afterEach, beforeEach, describe, expect, mock, spyOn, test } from "bun:test";
import {
  access,
  chmod,
  mkdir,
  mkdtemp,
  readdir,
  readFile,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
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
import pkg from "../package.json";
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
    workerAdmission: { queue: [] },
  };
}
/** RFC 4122 text form, the shape `node:crypto`'s `randomUUID()` mints for a spawn request id. */
const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const PLUGIN_VERSION = pkg.version;
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
const { resetLegionRoleClaimBridgeForTests } = await import("../src/legion/role-claim-bridge");
const { resetLegionBootstrappedSessionForTests, resetPrimaryEnvoyInstanceForTests } = await import(
  "../src/subagent-session"
);
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
  "LEGION_GRANT_FILE",
] as const;
// The suite's baseline is "not a Legion pane": every key above except HOME starts unset and is
// reset to unset after each test. Run from inside a worker pane — whose LEGION_BOOT_TOKEN_FILE,
// DISPATCH_URL, and friends are live — the suite would otherwise boot fixtures with the pane's
// own boot token and see a different environment from CI.
const baselineEnvironment: Record<(typeof environmentKeys)[number], string | undefined> =
  Object.fromEntries(
    environmentKeys.map((key) => [key, key === "HOME" ? process.env.HOME : undefined])
  ) as Record<(typeof environmentKeys)[number], string | undefined>;
for (const key of environmentKeys) {
  if (key !== "HOME") delete process.env[key];
}

const temporaryPaths: string[] = [];

beforeEach(() => {
  // A suite run from inside a Legion pane inherits that pane's LEGION_*/DISPATCH_* launch
  // environment; every test starts from none and sets only what it declares.
  for (const key of environmentKeys) delete process.env[key];
});

afterEach(async () => {
  globalThis.fetch = originalFetch;
  // OMP's `session_shutdown` removes each envoy instance from the process-wide role-claim
  // bridge; this suite binds a fixture per test and never shuts it down, so it clears the
  // bridge itself — otherwise a stale instance still serving a reused session id (its client
  // bound to a previous test's fetch stub) would capture a later test's claim.
  resetLegionRoleClaimBridgeForTests();
  natsConnections.splice(0);
  setLegionBootstrapExitForTests((code) => process.exit(code) as never);
  resetLegionBootstrappedSessionForTests();
  resetPrimaryEnvoyInstanceForTests();
  for (const key of environmentKeys) {
    const value = baselineEnvironment[key];
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  await Promise.all(
    temporaryPaths.splice(0).map((directory) => rm(directory, { force: true, recursive: true }))
  );
});

function createPi(options: { readonly bindEnvoy?: boolean } = {}): {
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
  // `bindEnvoy: false` lets a test bind legion.ts before envoy.ts, so the two extensions'
  // handlers for one session event run in the opposite order to the manifest's.
  if (options.bindEnvoy !== false) envoyExtension(pi as never);
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

/**
 * A listener-plus-daemon stub for the controller-pane tests. It records every request, keeps a
 * per-session interest registry — a role claim registers its role topic exactly as the real
 * listener does, so a rebind's registry lookup can find a role the session already holds —
 * arbitrates a soft claim the listener's way (granted when the role is unheld, already this
 * session's, or held by the declared previous session; 409 with the live holder otherwise),
 * and answers the heartbeat's role read from `holder`, or "no holder" while `lostClaim` is set.
 */
function controllerPaneListener(token: string): {
  readonly fetch: typeof fetch;
  readonly requests: { readonly path: string; readonly body: unknown }[];
  readonly registry: { holder: string | undefined; lostClaim: boolean };
  readonly readyPosted: (count: number) => Promise<void>;
} {
  const requests: { readonly path: string; readonly body: unknown }[] = [];
  const topics = new Map<string, Set<string>>();
  const registry = { holder: undefined as string | undefined, lostClaim: false };
  const readyWaiters: { readonly count: number; readonly resolve: () => void }[] = [];
  const interest = (sessionID: string) => ({
    session_id: sessionID,
    machine_id: "machine",
    dir: "/tmp/legion-workspace",
    topics: [...(topics.get(sessionID) ?? [])],
  });
  const stub = (async (input, init) => {
    const url = new URL(input.toString());
    const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
    requests.push({ path: url.pathname, body });
    if (url.pathname === "/legion/v1/state") return Response.json(redactedLegionState("omp"));
    if (url.pathname === "/legion/v1/controller/ready") {
      const posted = requests.filter((request) => request.path === url.pathname).length;
      for (const waiter of readyWaiters.splice(0)) {
        if (waiter.count <= posted) waiter.resolve();
        else readyWaiters.push(waiter);
      }
      return Response.json({});
    }
    if (url.pathname === "/legion/v1/grants") {
      return Response.json({
        grantId: `grant-for-${body?.sessionId}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname === "/v1/interests/subscribe") {
      const held = topics.get(body.session_id) ?? new Set<string>();
      for (const topic of body.topics ?? []) held.add(topic);
      topics.set(body.session_id, held);
      return Response.json(interest(body.session_id));
    }
    if (url.pathname === "/v1/interests/unsubscribe") {
      const held = topics.get(body.session_id);
      for (const topic of body.topics ?? []) held?.delete(topic);
      return Response.json({ removed: body.topics ?? [] });
    }
    if (url.pathname.startsWith("/v1/interests/")) {
      const sessionID = url.pathname.slice("/v1/interests/".length);
      if (!topics.has(sessionID)) {
        return Response.json({ error: `no interests for ${sessionID}` }, { status: 404 });
      }
      return Response.json(interest(sessionID));
    }
    if (url.pathname === `/v1/roles/${token}`) {
      if (registry.lostClaim || registry.holder === undefined) {
        return Response.json({ error: `no holder for role ${token}` }, { status: 404 });
      }
      return Response.json({ role: token, holder: registry.holder, last_seen: 1 });
    }
    if (url.pathname === "/v1/roles/set") {
      const holder = registry.holder;
      if (
        body.soft === true &&
        holder !== undefined &&
        holder !== body.session_id &&
        holder !== body.previous_session_id
      ) {
        return Response.json(
          { error: `role ${body.role} is held by ${holder}`, role: body.role, holder },
          { status: 409 }
        );
      }
      registry.holder = body.session_id;
      registry.lostClaim = false;
      const held = topics.get(body.session_id) ?? new Set<string>();
      held.add(`notifications.role.${body.role}`);
      topics.set(body.session_id, held);
      return Response.json(interest(body.session_id));
    }
    return Response.json(interest(body?.session_id ?? ""));
  }) as typeof fetch;
  return {
    fetch: stub,
    requests,
    registry,
    readyPosted: (count) => {
      const posted = requests.filter((request) => request.path === "/legion/v1/controller/ready");
      if (posted.length >= count) return Promise.resolve();
      const { promise, resolve } = Promise.withResolvers<void>();
      readyWaiters.push({ count, resolve });
      return promise;
    },
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
  // Both transcripts exist on disk, as they do once `ensureOnDisk` has run for the subagent.
  const childFile = path.join(childDirectory, "child.jsonl");
  await writeFile(childFile, "");
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

/** `key` at repository scope for the repo `directory` belongs to, as `jj config list` prints it
 * (`user.name = "…"`), or `""` when the repository scope does not set it. `--repo` is the scope;
 * `-R` names the repo. `--include-overridden` (its `# ` marker stripped) keeps the answer the same
 * when the process running the tests — a Legion pane — carries `JJ_USER`/`JJ_EMAIL`, which would
 * otherwise hide the repository value. */
async function jjRepoConfig(directory: string, key: string): Promise<string> {
  const child = Bun.spawn(
    ["jj", "config", "list", "--repo", "--include-overridden", "-R", directory, key],
    { stdout: "pipe", stderr: "pipe" }
  );
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout as ReadableStream<Uint8Array>).text(),
    new Response(child.stderr as ReadableStream<Uint8Array>).text(),
  ]);
  if (exitCode !== 0) throw new Error(`jj config list failed: ${stderr}`);
  return stdout.trim().replace(/^# /, "");
}

async function setJjRepoConfig(directory: string, key: string, value: string): Promise<void> {
  const child = Bun.spawn(
    ["jj", "config", "set", "--repo", "-R", directory, key, JSON.stringify(value)],
    { stdout: "ignore", stderr: "pipe" }
  );
  if ((await child.exited) !== 0) {
    const stderr = await new Response(child.stderr as ReadableStream<Uint8Array>).text();
    throw new Error(`jj config set failed: ${stderr}`);
  }
}
/** What `legion` will read from the pane's grant file after a worker's `tool_call` handler ran:
 * the trimmed contents and the file mode. Throws (ENOENT) when the hook never wrote it, so a
 * test never asserts against an absent file. */
async function grantFileContents(file: string): Promise<{ grant: string; mode: number }> {
  return {
    grant: (await readFile(file, "utf8")).trim(),
    mode: (await stat(file)).mode & 0o777,
  };
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
  readonly intervals?: (() => void)[];
}): Promise<{
  readonly toolCall: Handler;
  readonly context: SessionContext;
  readonly token: string;
  /** The pane's `LEGION_GRANT_FILE`, under a 0700 secrets dir, exactly as the daemon names it. */
  readonly grantFile: string;
  readonly secretsDir: string;
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
  const secretsDir = await mkdtemp(path.join(os.tmpdir(), "legion-secrets-"));
  await chmod(secretsDir, 0o700);
  temporaryPaths.push(secretsDir);
  const grantFile = path.join(secretsDir, `${token}-grant`);
  process.env.LEGION_GRANT_FILE = grantFile;
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
  const context: SessionContext = {
    ...sessionContext(sessionId),
    cwd: options.workspace,
    setInterval: (callback) => {
      options.intervals?.push(callback);
    },
  };
  await sessionStart({}, context);
  return { toolCall, context, token, grantFile, secretsDir };
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
          pluginVersion: PLUGIN_VERSION,
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
  test("two legion tool calls issued at once both succeed after the daemon forgot the session's secret, with one recovery", async () => {
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
    // The daemon's capability map: `root-secret` until the test "restarts" the daemon, then
    // whatever the newest /worker-session minted (stored at mint time; only the response is held).
    let current: string | undefined = "root-secret";
    let minted = 0;
    const held: (() => void)[] = [];
    const firstRecoverySeen = Promise.withResolvers<void>();
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
      if (url.pathname === "/legion/v1/worker-session") {
        minted += 1;
        const secret = `recovered-root-${minted}`;
        current = secret;
        const gate = Promise.withResolvers<void>();
        held.push(gate.resolve);
        firstRecoverySeen.resolve();
        await gate.promise;
        return Response.json({ tree, issue: tree, role: "architect", secret });
      }
      if (url.pathname.startsWith("/legion/v1/")) {
        if (current === undefined || body?.secret !== current) {
          return Response.json({ error: "Invalid session secret" }, { status: 403 });
        }
        if (url.pathname === "/legion/v1/waves/release") {
          return Response.json({ released: ["REPO-43"] });
        }
        return Response.json({});
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
    current = undefined; // the daemon restarted: its in-memory capability map is gone

    const releaseWave = legionTool.execute(
      "call-release",
      { op: "release_wave", issues: ["REPO-43"] },
      undefined,
      undefined,
      context
    );
    const escalate = legionTool.execute(
      "call-escalate",
      { op: "escalate", kind: "capacity", context: { reason: "No slots" } },
      undefined,
      undefined,
      context
    );
    await firstRecoverySeen.promise;
    // One macrotask tick, never a wall-clock wait: every microtask the two refusals queued has run,
    // so a second recovery request (the defect) would already be visible before the gates open.
    await new Promise<void>((resolve) => setImmediate(resolve));
    for (const resume of held) resume();
    const results = await Promise.all([releaseWave, escalate]);

    expect(results.map((result) => result.isError)).toEqual([undefined, undefined]);
    expect(requests.filter((request) => request.path === "/legion/v1/worker-session")).toEqual([
      {
        path: "/legion/v1/worker-session",
        body: { sessionId: "ses_root", recoveryToken: "root-recovery" },
      },
    ]);
    expect(
      requests
        .filter((request) => request.body?.secret === "recovered-root-1")
        .map((request) => request.path)
        .sort()
    ).toEqual(["/legion/v1/escalate", "/legion/v1/waves/release"]);
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
          capabilities: ["aside", "steer"],
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
          pluginVersion: PLUGIN_VERSION,
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
          capabilities: ["aside", "steer"],
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
    // A hand-started takeover session carries no daemon-pane marker.
    delete process.env.LEGION_CONTROLLER;
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
          capabilities: ["aside", "steer"],
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
          capabilities: ["aside", "steer"],
        },
      },
      { path: "/v1/roles/set", body: { session_id: "ses_controller", role: token } },
      {
        path: "/legion/v1/controller/ready",
        body: {
          secret: "controller-secret",
          sessionId: "ses_controller",
          ompSessionFile: "/tmp/session.jsonl",
          pluginVersion: PLUGIN_VERSION,
        },
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
          capabilities: ["aside", "steer"],
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
          capabilities: ["aside", "steer"],
        },
      },
      { path: "/v1/roles/set", body: { session_id: "ses_interactive", role: token } },
      // The takeover command moves the role and session id but never reports a transcript: the
      // daemon pane's recorded file must stay the pane's own.
      {
        path: "/legion/v1/controller/ready",
        body: {
          secret: "controller-secret",
          sessionId: "ses_interactive",
          pluginVersion: PLUGIN_VERSION,
        },
      },
    ]);

    // The same command typed into the daemon pane itself (the marker present) is the manual
    // override for a lost claim, and the pane's own transcript is the right resume target.
    process.env.LEGION_CONTROLLER = "1";
    await claimCommand.handler("", sessionContext("ses_pane", "/tmp/pane.jsonl"));
    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/controller/ready",
      body: {
        secret: "controller-secret",
        sessionId: "ses_pane",
        ompSessionFile: "/tmp/pane.jsonl",
        pluginVersion: PLUGIN_VERSION,
      },
    });
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
      body: {
        secret: "controller-secret",
        sessionId: "ses_controller",
        ompSessionFile: "/tmp/session.jsonl",
        pluginVersion: PLUGIN_VERSION,
      },
    };
    // The re-run retains the pane transcript. This repairs a first-ever ready call that arrived
    // before the daemon recorded its locator: the next regain gives the daemon the file again.
    const readyAgain = {
      path: "/legion/v1/controller/ready",
      body: {
        secret: "controller-secret",
        sessionId: "ses_controller",
        ompSessionFile: "/tmp/session.jsonl",
        pluginVersion: PLUGIN_VERSION,
      },
    };
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready]);

    // A steady tick: the listener still names this session, so nothing is claimed or re-run.
    intervals[0]?.();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready]);
    expect(requests.filter((request) => request.path === "/v1/roles/set")).toHaveLength(1);

    // The listener no longer names this session: the next tick soft-claims and re-runs
    // controller/ready.
    listenerHoldsClaim = false;
    intervals[0]?.();
    await secondReady.promise;
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready, readyAgain]);
    expect(
      requests.filter((request) => request.path === "/v1/roles/set").map((request) => request.body)
    ).toEqual([
      { session_id: "ses_controller", role: token },
      { session_id: "ses_controller", role: token, soft: true },
    ]);
  });
  test("a controller's regain hook survives a task subagent's in-process extension re-bind", async () => {
    // OMP binds every extension factory again for each in-process `task` subagent, so a second
    // legionExtension(pi) — with no Legion identity — runs in the controller's process. It must
    // not replace the controller's regain listener on the process-wide bridge slot.
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
        return Response.json({ role: token, holder: "ses_controller_rebind", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") listenerHoldsClaim = true;
      return Response.json({
        session_id: body?.session_id,
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const controller = createPi();
    legionExtension(controller.pi);
    const controllerStart = controller.handlers.get("session_start");
    if (controllerStart === undefined) throw new Error("controller handlers were not registered");
    const intervals: (() => void)[] = [];
    await controllerStart(
      {},
      {
        ...sessionContext("ses_controller_rebind"),
        setInterval: (callback) => intervals.push(callback),
      }
    );
    const ready = {
      path: "/legion/v1/controller/ready",
      body: {
        secret: "controller-secret",
        sessionId: "ses_controller_rebind",
        ompSessionFile: "/tmp/session.jsonl",
        pluginVersion: PLUGIN_VERSION,
      },
    };
    const readyAgain = {
      path: "/legion/v1/controller/ready",
      body: {
        secret: "controller-secret",
        sessionId: "ses_controller_rebind",
        ompSessionFile: "/tmp/session.jsonl",
        pluginVersion: PLUGIN_VERSION,
      },
    };
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready]);

    // The controller runs a `task`: a fresh extension instance binds and its subagent session
    // starts in this same process, exactly as OMP does it.
    const subagent = createPi();
    legionExtension(subagent.pi);
    const subagentStart = subagent.handlers.get("session_start");
    if (subagentStart === undefined) throw new Error("subagent handlers were not registered");
    const { childFile } = await createSubagentTranscriptPaths();
    await subagentStart({}, sessionContext("ses_controller_subagent", childFile));
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready]);

    listenerHoldsClaim = false;
    intervals[0]?.();
    await secondReady.promise;
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready, readyAgain]);
    expect(
      requests.filter((request) => request.path === "/v1/roles/set").map((request) => request.body)
    ).toEqual([
      { session_id: "ses_controller_rebind", role: token },
      { session_id: "ses_controller_rebind", role: token, soft: true },
    ]);
  });
  test("a root architect that regains its role re-runs process/ready so the overseer catch-up replays", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "REPO-42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-root-regain";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    let listenerHoldsClaim = true;
    const secondReady = Promise.withResolvers<void>();
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
      if (url.pathname === "/legion/v1/process/ready") {
        if (requests.filter((request) => request.path === url.pathname).length === 2) {
          secondReady.resolve();
        }
        return Response.json({});
      }
      if (url.pathname === `/v1/roles/${token}`) {
        if (!listenerHoldsClaim) {
          return Response.json({ error: `no holder for role ${token}` }, { status: 404 });
        }
        return Response.json({ role: token, holder: "ses_root", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") listenerHoldsClaim = true;
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
    const intervals: (() => void)[] = [];
    await sessionStart(
      {},
      { ...sessionContext("ses_root"), setInterval: (callback) => intervals.push(callback) }
    );
    const ready = {
      path: "/legion/v1/process/ready",
      body: { tree, sessionId: "ses_root", secret: "root-secret", generation: 3 },
    };
    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready]);

    listenerHoldsClaim = false;
    intervals[0]?.();
    await secondReady.promise;

    expect(requests.filter((request) => request.path === ready.path)).toEqual([ready, ready]);
    expect(
      requests.filter((request) => request.path === "/v1/roles/set").map((request) => request.body)
    ).toEqual([
      { session_id: "ses_root", role: token },
      { session_id: "ses_root", role: token, soft: true },
    ]);
  });
  test("a phase worker that regains its role re-runs nothing: the daemon's own no-holder recovery prompts its catch-up", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();
    const token = roleToken("omp", "REPO-43", "implementer");
    const intervals: (() => void)[] = [];
    let listenerHoldsClaim = true;
    const reasserted = Promise.withResolvers<void>();
    await bootWorker({
      role: "implementer",
      sessionId: "ses_worker_regain",
      workspace,
      requests,
      intervals,
      extraRoutes: (url, body) => {
        if (url.pathname === `/v1/roles/${token}`) {
          if (!listenerHoldsClaim) {
            return Response.json({ error: `no holder for role ${token}` }, { status: 404 });
          }
          return Response.json({ role: token, holder: "ses_worker_regain", last_seen: 1 });
        }
        const soft =
          typeof body === "object" && body !== null && "soft" in body && body.soft === true;
        if (url.pathname === "/v1/roles/set" && soft) {
          listenerHoldsClaim = true;
          reasserted.resolve();
        }
        return undefined;
      },
    });
    expect(requests.filter((request) => request.path === "/legion/v1/worker/ready")).toHaveLength(
      1
    );

    listenerHoldsClaim = false;
    intervals[0]?.();
    await reasserted.promise;
    await new Promise((resolve) => setTimeout(resolve, 20));

    expect(
      requests.filter((request) => request.path === "/v1/roles/set").map((request) => request.body)
    ).toEqual([
      { session_id: "ses_worker_regain", role: token },
      { session_id: "ses_worker_regain", role: token, soft: true },
    ]);
    // The daemon's resumeWorker -> spawnWorker path (processes.ts) already prompts or queues the
    // worker's catch-up on a no-holder 404, and /worker/ready is a no-op on a confirmed claim.
    expect(requests.filter((request) => request.path === "/legion/v1/worker/ready")).toHaveLength(
      1
    );
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
      sessionManager: {
        getSessionId: () => "ses_interactive",
        getSessionFile: () => "/tmp/session.jsonl",
        ensureOnDisk: async () => undefined,
      },
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
          capabilities: ["aside", "steer"],
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
          capabilities: ["aside", "steer"],
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
        body: {
          secret: "controller-capability",
          sessionId: "ses_interactive",
          pluginVersion: PLUGIN_VERSION,
        },
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
        sessionManager: {
          getSessionId: () => "ses_interactive",
          getSessionFile: () => "/tmp/session.jsonl",
          ensureOnDisk: async () => undefined,
        },
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
        pluginVersion: PLUGIN_VERSION,
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
  test("recognises a subagent by the session this process already bootstrapped when no transcript is on disk", async () => {
    // With the transcript in a SQL row there is no parent `.jsonl` beside the subagent's path, so
    // the on-disk layout says nothing; the guard must still fall back on what this process booted.
    const requests: { readonly path: string }[] = [];
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error(`exitProcess(${code})`);
    });
    const tree = "REPO-42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-root-sql";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      requests.push({ path: url.pathname });
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: token },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      return Response.json({
        session_id: "ses_root_sql",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    // Neither transcript exists on disk: the row keys below name files nothing ever wrote.
    const baseDirectory = await mkdtemp(path.join(os.tmpdir(), "legion-sql-subagent-"));
    temporaryPaths.push(baseDirectory);
    const rootFile = path.join(baseDirectory, "sessions", "-repo", "2026_root.jsonl");
    const subagentFile = path.join(
      baseDirectory,
      "sessions",
      "-repo",
      "2026_root",
      "2026_task.jsonl"
    );

    const root = createPi();
    legionExtension(root.pi);
    const rootStart = root.handlers.get("session_start");
    if (rootStart === undefined) throw new Error("root session_start handler was not registered");
    await rootStart({}, sessionContext("ses_root_sql", rootFile));
    expect(root.tools.find((tool) => tool.name === "legion")).toBeDefined();
    const requestsAfterRoot = requests.length;

    // The root runs a `task`: a fresh extension instance binds in the same process and its
    // subagent session starts with a different transcript path and the inherited environment.
    const subagent = createPi();
    legionExtension(subagent.pi);
    const subagentStart = subagent.handlers.get("session_start");
    const subagentToolCall = subagent.handlers.get("tool_call");
    if (subagentStart === undefined || subagentToolCall === undefined) {
      throw new Error("subagent handlers were not registered");
    }
    const subagentContext = sessionContext("ses_root_sql_task", subagentFile);
    await subagentStart({}, subagentContext);

    // Only Envoy's own direct-subject registration for the new session may follow: no Legion
    // daemon route and no role claim.
    const afterRoot = requests.slice(requestsAfterRoot).map((request) => request.path);
    expect(afterRoot.filter((requestPath) => requestPath !== "/v1/interests/subscribe")).toEqual(
      []
    );
    expect(exits).toEqual([]);
    expect(subagent.tools.find((tool) => tool.name === "legion")).toBeUndefined();
    await expect(
      subagentToolCall(
        { toolName: "bash", toolCallId: "call-sql-subagent-bash", input: { command: "ls" } },
        subagentContext
      )
    ).resolves.toBeUndefined();
  });
  test("refuses a subagent's operation-log rewrite in a phase-worker pane while its other calls stay ungated", async () => {
    const requests: { readonly path: string }[] = [];
    const { childFile } = await createSubagentTranscriptPaths();
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "1";
    process.env.LEGION_BOOT_TOKEN = "boot-subagent-worker-jj";
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ROLE = "implementer";
    process.env.LEGION_ISSUE = "REPO-43";
    process.env.LEGION_WORKSPACE = "/tmp/legion-workspace";
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      requests.push({ path: url.pathname });
      return Response.json({
        session_id: "ses_sub_worker_jj",
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
    const context = sessionContext("ses_sub_worker_jj", childFile);
    await sessionStart({}, context);

    // LEGION-45: the subagent's bash runs in the same pane, against the same shared operation
    // log, as the phase worker that spawned it -- the one gate that binds a subagent.
    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "call-sub-worker-jj-log",
          input: { command: 'jj -R "$LEGION_WORKSPACE" undo' },
        },
        context
      )
    ).resolves.toEqual({
      block: true,
      reason: expect.stringContaining("every Legion issue workspace shares"),
    });
    // Every other gate stays off: the call passes, and no grant or daemon route is touched.
    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "call-sub-worker-legion-state",
          input: { command: "legion state" },
        },
        context
      )
    ).resolves.toBeUndefined();
    expect(requests.some((request) => request.path.startsWith("/legion/"))).toBe(false);
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
    const secretsDir = await mkdtemp(path.join(os.tmpdir(), "legion-secrets-"));
    temporaryPaths.push(secretsDir);
    const grantFile = path.join(secretsDir, `${token}-grant`);
    process.env.LEGION_GRANT_FILE = grantFile;
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

    expect(result).toBeUndefined();
    expect(await grantFileContents(grantFile)).toEqual({ grant: "grant-recovered", mode: 0o600 });
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
    process.env.LEGION_GRANT_FILE = path.join(secretsDir, `${token}-grant`);
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
    // A hand-started takeover session carries no daemon-pane marker.
    delete process.env.LEGION_CONTROLLER;
    await claimCommand.handler("", sessionContext("ses_interactive"));

    expect(requests.filter((request) => request.path === "/legion/v1/controller/ready")).toEqual([
      {
        path: "/legion/v1/controller/ready",
        body: {
          secret: "file-controller-secret",
          sessionId: "ses_controller",
          ompSessionFile: "/tmp/session.jsonl",
          pluginVersion: PLUGIN_VERSION,
        },
      },
      {
        path: "/legion/v1/controller/ready",
        body: {
          secret: "file-controller-secret",
          sessionId: "ses_interactive",
          pluginVersion: PLUGIN_VERSION,
        },
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
  test("leaves the repository-scoped jj config untouched at worker boot: identity is the pane's environment, not config", async () => {
    // Every issue workspace is a workspace of one shared clone, and `--repo` config is one file
    // for all of them: a boot that wrote its identity there set the author for every other tree
    // (LEGION-44). A sentinel written before boot must survive it — neither overwritten with the
    // daemon-reported identity nor removed (that one-time cleanup is provisioning's, daemon-side).
    const workspace = await createJjWorkspace();
    await setJjRepoConfig(workspace, "user.name", "Sentinel Before Boot");

    await bootWorker({ role: "implementer", workspace });

    expect(await jjRepoConfig(workspace, "user.name")).toBe('user.name = "Sentinel Before Boot"');
    expect(await jjRepoConfig(workspace, "user.email")).toBe("");
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
  test("refuses a phase worker's bash command that would rewrite the shared jj operation log", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();
    const { toolCall, context } = await bootWorker({
      role: "implementer",
      workspace,
      sessionId: "ses_implementer_jj_log",
      requests,
      extraRoutes: (url) =>
        url.pathname === "/legion/v1/grants"
          ? Response.json({ grantId: "grant-jj-log", expiresAt: "2099-01-01T00:00:00.000Z" })
          : undefined,
    });
    const mints = (): number =>
      requests.filter((request) => request.path === "/legion/v1/grants").length;
    const mintsBefore = mints();
    // Every form the spec names, plus the whole-argument-list, pipeline-position, quoted-shell,
    // unquoted-message-word, and unterminated-quote (plain-text fallback) cases, and (review
    // round 1) the quoted and backslash-escaped forms bash hands to jj as the identical argv.
    const refused = [
      "jj undo",
      'jj -R "$LEGION_WORKSPACE" undo',
      "cd ws && jj undo",
      "jj op restore 63461aba",
      "jj op abandon",
      "jj op revert",
      "jj abandon",
      "jj --repository p undo",
      "jj operation restore 63461aba",
      "jj op undo",
      "jj op log -n 1 | head -1 | xargs jj op restore",
      "jj --at-op 805478f4 op restore 805478f4",
      "jj describe -m undo this",
      "sh -c 'jj -R ws undo'",
      "(cd ws && jj abandon)",
      "cd ws\njj -R . undo",
      "env JJ_CONFIG=/x /usr/local/bin/jj undo",
      'jj un"do"',
      'jj -R "$LEGION_WORKSPACE" describe -m \'undo',
      'jj "undo"',
      "jj 'undo'",
      'jj op "restore" @-',
      '"jj" undo',
      "jj \\u\\n\\d\\o",
      // A one-word message equal to a blocked word is the spec's stated tradeoff: one rephrase.
      'jj describe -m "undo"',
      // Redirection operators split words as bash does and never end the simple command; a
      // backslash-newline is line continuation, not part of the next word -- outside quotes and
      // inside double quotes alike (bash removes both characters in both places).
      "jj undo>/dev/null",
      "jj 2>&1 undo",
      "jj op 2>&1 restore x",
      "jj \\\nundo",
      'jj "un\\\ndo"',
    ];
    const allowedByMistake: string[] = [];
    for (const command of refused) {
      const result = await toolCall(
        { toolName: "bash", toolCallId: `call-jj-log-${command}`, input: { command } },
        context
      );
      const blocked =
        typeof result === "object" && result !== null && "block" in result && result.block === true;
      if (!blocked) allowedByMistake.push(command);
    }
    expect(allowedByMistake).toEqual([]);
    // A refused command never mints a grant: nothing ran, so nothing ran under one.
    expect(mints()).toBe(mintsBefore);
    // The refusal names the command, says the log is shared by every issue workspace, and gives
    // the recovery rule (new commit, or jj restore of files; otherwise the architect).
    const result = await toolCall(
      {
        toolName: "bash",
        toolCallId: "call-jj-log-named",
        input: { command: 'jj -R "$LEGION_WORKSPACE" undo' },
      },
      context
    );
    for (const phrase of [
      "jj -R $LEGION_WORKSPACE undo",
      "every Legion issue workspace shares",
      "new commit",
      "jj restore <paths>",
      "architect",
    ]) {
      expect(result).toEqual({ block: true, reason: expect.stringContaining(phrase) });
    }
  });
  test("leaves file-level jj restore, jj op log, jj op show, and quoted message words alone", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();
    const { toolCall, context } = await bootWorker({
      role: "implementer",
      workspace,
      sessionId: "ses_implementer_jj_ok",
      requests,
      extraRoutes: (url) =>
        url.pathname === "/legion/v1/grants"
          ? Response.json({ grantId: "grant-jj-ok", expiresAt: "2099-01-01T00:00:00.000Z" })
          : undefined,
    });
    const mints = (): number =>
      requests.filter((request) => request.path === "/legion/v1/grants").length;
    const mintsBefore = mints();
    // The spec's negatives plus the exact commands the legion-worker skill has every role run:
    // the rebase revset, the fingerprint (a `|` inside quotes), the handoff split, the log filter.
    const allowed = [
      "jj restore src/x.ts",
      'jj -R "$LEGION_WORKSPACE" restore packages/pi-envoy/extensions/legion.ts',
      "jj op log",
      'jj -R "$LEGION_WORKSPACE" op log -n 5',
      "jj op show",
      'jj describe -m "undo this"',
      "jj -R \"$LEGION_WORKSPACE\" rebase -s 'roots(main@origin..@)' -d main@origin",
      'cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && jj -R "$LEGION_WORKSPACE" diff --from "fork_point(main@origin | abc123)" --to abc123 --git --context 0 \'~(.legion | docs/solutions)\' | sed -e \'/^@@/d\' -e \'/^index /d\' | sha256sum',
      'jj -R "$LEGION_WORKSPACE" split -m "plan: record handoff" .legion/plan.json',
      'jj -R "$LEGION_WORKSPACE" log -r \'description(glob:"undo*")\'',
      "jj -R \"$LEGION_WORKSPACE\" log -r 'ancestors(@, 5)'",
      "jj --at-op 805478f4 restore src/x.ts",
      "legion state",
      // Redirections are part of the simple command they sit in, on the allowed side too.
      "jj op log 2>&1 | head",
      "jj restore f 2>/dev/null",
      // Single quotes keep a backslash-newline literally in bash, so this word is not `undo`.
      "jj 'un\\\ndo'",
    ];
    const refusedByMistake: string[] = [];
    for (const command of allowed) {
      const result = await toolCall(
        { toolName: "bash", toolCallId: `call-jj-ok-${command}`, input: { command } },
        context
      );
      if (result !== undefined) refusedByMistake.push(`${command} -> ${JSON.stringify(result)}`);
    }
    expect(refusedByMistake).toEqual([]);
    // Each allowed command went down the ordinary path: one grant minted per call.
    expect(mints()).toBe(mintsBefore + allowed.length);
  });
  test("applies the operation-log guard to every phase-worker role and leaves each role's sanctioned jj commands alone", async () => {
    // What each role actually runs per the legion-worker skill and its role prompt.
    const sanctioned: Record<LegionRole, string> = {
      planner: 'jj -R "$LEGION_WORKSPACE" split -m "plan: record handoff" .legion/plan.json',
      implementer:
        'cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" new && rm -rf .legion && jj -R "$LEGION_WORKSPACE" describe -m "chore: remove .legion handoffs" && jj -R "$LEGION_WORKSPACE" bookmark set legion/REPO-43 && jj -R "$LEGION_WORKSPACE" git push --bookmark legion/REPO-43',
      tester: 'jj -R "$LEGION_WORKSPACE" split -m "test: record handoff" .legion/test.json',
      reviewer:
        'cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" file list -r @- .legion && legion gh -- api --method POST repos/o/r/pulls/7/reviews --input body.json',
      merger: 'jj -R "$LEGION_WORKSPACE" diff --from abc123 --to def456 --summary',
      architect: "legion handoff complete --summary x",
    };
    for (const role of LEGION_ROLES) {
      const workspace = await createJjWorkspace();
      const { toolCall, context } = await bootWorker({
        role,
        workspace,
        sessionId: `ses_${role}_jj_guard`,
        extraRoutes: (url) =>
          url.pathname === "/legion/v1/grants"
            ? Response.json({ grantId: `grant-${role}`, expiresAt: "2099-01-01T00:00:00.000Z" })
            : undefined,
      });
      const undo = await toolCall(
        {
          toolName: "bash",
          toolCallId: `call-${role}-jj-undo`,
          input: { command: 'jj -R "$LEGION_WORKSPACE" undo' },
        },
        context
      );
      // A sub-architect's pane classifies as a phase worker's, so the operation-log guard (judged
      // from the environment, ahead of every role gate so that it also binds a subagent) answers
      // before its blanket bash gate; the root architect's pane, by contrast, never reaches it.
      expect(undo).toEqual({
        block: true,
        reason: expect.stringContaining("every Legion issue workspace shares"),
      });
      await expect(
        toolCall(
          {
            toolName: "bash",
            toolCallId: `call-${role}-jj-sanctioned`,
            input: { command: sanctioned[role] },
          },
          context
        )
      ).resolves.toBeUndefined();
    }
  });
  test("refuses eval code and hub input that mention jj with an operation-log rewrite, by the plain-text rule", async () => {
    const workspace = await createJjWorkspace();
    const { toolCall, context } = await bootWorker({
      role: "tester",
      workspace,
      sessionId: "ses_tester_jj_eval_hub",
    });
    const refused: { readonly toolName: string; readonly input: Record<string, unknown> }[] = [
      {
        toolName: "eval",
        input: {
          language: "py",
          code: 'import subprocess\nsubprocess.run(["jj", "-R", ws, "undo"])',
        },
      },
      { toolName: "eval", input: { language: "js", code: `await Bun.$\`jj op restore \${id}\`` } },
      // An argv literal separates the words with `", "`; the rule allows any non-word run.
      { toolName: "eval", input: { language: "py", code: 'run(["jj", "op", "restore", op_id])' } },
      {
        toolName: "hub",
        input: { op: "start", name: "x", application: "jj", args: ["-R", "/ws", "undo"] },
      },
      {
        toolName: "hub",
        input: { op: "start", name: "x", application: "bash", args: ["-c", "jj op restore 1"] },
      },
      { toolName: "hub", input: { op: "send", name: "shell", text: "jj undo" } },
    ];
    const allowed: { readonly toolName: string; readonly input: Record<string, unknown> }[] = [
      {
        toolName: "eval",
        input: { language: "py", code: 'run(["jj", "op", "log"]); run(["jj", "restore", "f"])' },
      },
      { toolName: "eval", input: { language: "py", code: 'print(read("jj-notes.md"))' } },
      {
        toolName: "hub",
        input: { op: "start", name: "web", application: "bun", args: ["run", "dev"] },
      },
      { toolName: "hub", input: { op: "logs", name: "web" } },
      { toolName: "hub", input: {} },
    ];
    const allowedByMistake: string[] = [];
    for (const [index, call] of refused.entries()) {
      const result = await toolCall({ ...call, toolCallId: `call-jj-text-${index}` }, context);
      const blocked =
        typeof result === "object" && result !== null && "block" in result && result.block === true;
      if (!blocked) allowedByMistake.push(JSON.stringify(call));
    }
    expect(allowedByMistake).toEqual([]);
    const refusedByMistake: string[] = [];
    for (const [index, call] of allowed.entries()) {
      const result = await toolCall({ ...call, toolCallId: `call-jj-text-ok-${index}` }, context);
      if (result !== undefined) refusedByMistake.push(JSON.stringify(call));
    }
    expect(refusedByMistake).toEqual([]);
    // Same message shape as the bash refusal, naming the tool and the words it found.
    const named = await toolCall(
      {
        toolName: "hub",
        toolCallId: "call-jj-text-named",
        input: { op: "start", name: "x", application: "jj", args: ["undo"] },
      },
      context
    );
    for (const phrase of ["hub: jj undo", "every Legion issue workspace shares"]) {
      expect(named).toEqual({ block: true, reason: expect.stringContaining(phrase) });
    }
  });
  test("writes the minted grant to LEGION_GRANT_FILE as a 0600 file and leaves the bash input untouched", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();

    const { toolCall, context, grantFile } = await bootWorker({
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

    const command = "legion gh -- pr view 7";
    const result = await toolCall(
      { toolName: "bash", toolCallId: "call-1", input: { command } },
      context
    );

    // Neither `command` (model-visible once written back — LEGION-12) nor `env` (dropped by a
    // plugin that replaces the bash tool — LEGION-52) carries anything: the hook returns nothing.
    expect(result).toBeUndefined();
    expect(await grantFileContents(grantFile)).toEqual({ grant: "grant-1", mode: 0o600 });
    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/grants",
      body: {
        tree: "REPO-42",
        issue: "REPO-43",
        sessionId: "ses_reviewer",
        secret: "worker-secret",
      },
    });
  });
  /**
   * Fixture note: `createPi().on` keeps every registered handler and its aggregate returns the
   * last non-undefined result, mirroring the host's `emitToolCall`, which also never chains one
   * handler's revised input into the next. Stacked handlers are therefore observable only by
   * counting `/legion/v1/grants` requests, never by inspecting a returned input.
   */
  test("ignores whatever env or text the model supplied and never rewrites command or env", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();
    let minted = 0;
    const { toolCall, context, grantFile } = await bootWorker({
      role: "implementer",
      workspace,
      requests,
      extraRoutes: (url) => {
        if (url.pathname === "/legion/v1/grants") {
          minted++;
          return Response.json({
            grantId: `grant-${minted}`,
            expiresAt: "2099-01-01T00:00:00.000Z",
          });
        }
        return undefined;
      },
    });

    // A model imitating an earlier session's shape: a stale grant in env and in the command text.
    const command = `export LEGION_GRANT='stale-imitated-grant'; legion credential get`;
    const result = await toolCall(
      {
        toolName: "bash",
        toolCallId: "call-env",
        input: {
          command,
          env: { LEGION_GRANT: "stale-imitated-grant", PATH: "/model/path" },
        },
      },
      context
    );

    expect(result).toBeUndefined();
    expect(await grantFileContents(grantFile)).toEqual({ grant: "grant-1", mode: 0o600 });
    expect(requests.filter((request) => request.path === "/legion/v1/grants")).toHaveLength(1);
  });
  test("overwrites the file with a fresh mint on every call, leaving no temp file behind", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();
    let minted = 0;
    const { toolCall, context, grantFile, secretsDir } = await bootWorker({
      role: "implementer",
      workspace,
      requests,
      extraRoutes: (url) => {
        if (url.pathname === "/legion/v1/grants") {
          minted++;
          return Response.json({
            grantId: `grant-${minted}`,
            expiresAt: "2099-01-01T00:00:00.000Z",
          });
        }
        return undefined;
      },
    });

    await toolCall(
      { toolName: "bash", toolCallId: "call-twice", input: { command: "echo hi" } },
      context
    );
    // The same tool call again (a host double-invocation) and a later one: each mints afresh
    // and the file always holds the newest grant.
    await toolCall(
      { toolName: "bash", toolCallId: "call-twice", input: { command: "echo hi" } },
      context
    );

    expect(await grantFileContents(grantFile)).toEqual({ grant: "grant-2", mode: 0o600 });
    // One mint per invocation: the extension never caches a grant, and the host invokes the hook
    // once per loop dispatch.
    expect(requests.filter((request) => request.path === "/legion/v1/grants")).toHaveLength(2);
    // The atomic rename leaves exactly the named file: no `<file>.<pid>.<uuid>` residue.
    expect(await readdir(secretsDir)).toEqual([path.basename(grantFile)]);
  });
  test("blocks the command naming the path when the grant file cannot be written", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();
    const { toolCall, context, secretsDir } = await bootWorker({
      role: "implementer",
      workspace,
      requests,
      extraRoutes: (url) => {
        if (url.pathname === "/legion/v1/grants") {
          return Response.json({ grantId: "grant-1", expiresAt: "2099-01-01T00:00:00.000Z" });
        }
        return undefined;
      },
    });
    const unwritable = path.join(secretsDir, "missing-dir", "x-grant");
    process.env.LEGION_GRANT_FILE = unwritable;

    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "call-unwritable", input: { command: "jj git push" } },
        context
      )
    ).resolves.toEqual({
      block: true,
      reason: expect.stringContaining(`LEGION_GRANT_FILE ${unwritable} could not be written`),
    });
    // Mint-then-write: the grant was minted (one wasted 60 s grant), nothing ran under a stale one.
    expect(requests.filter((request) => request.path === "/legion/v1/grants")).toHaveLength(1);

    // A relative pointer (an operator's own export) is refused before any temp file could land
    // in OMP's cwd — the issue workspace.
    process.env.LEGION_GRANT_FILE = "relative/x-grant";
    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "call-relative", input: { command: "jj git push" } },
        context
      )
    ).resolves.toEqual({
      block: true,
      reason: "LEGION_GRANT_FILE relative/x-grant could not be written: the path is not absolute",
    });
    expect(await readdir(workspace)).not.toContain(expect.stringMatching(/^x-grant\./));
  });
  test("blocks when the pane carries no LEGION_GRANT_FILE, or a blank one, without minting", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();
    const { toolCall, context } = await bootWorker({ role: "implementer", workspace, requests });
    const blocked = {
      block: true,
      reason:
        "LEGION_GRANT_FILE is not set on this pane: the daemon that launched it predates this plugin; restart the daemon on the matching release",
    };

    delete process.env.LEGION_GRANT_FILE;
    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "call-no-file", input: { command: "jj git push" } },
        context
      )
    ).resolves.toEqual(blocked);
    process.env.LEGION_GRANT_FILE = "  ";
    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "call-blank-file", input: { command: "jj git push" } },
        context
      )
    ).resolves.toEqual(blocked);
    expect(requests.filter((request) => request.path === "/legion/v1/grants")).toHaveLength(0);
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
    // The pane's launch environment is complete; only the boot handshake is missing.
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ISSUE = "REPO-43";
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
      reason: "Legion worker session is not registered; cannot mint its grant",
    });
  });
  test("writes a claimed controller's grant to the pane's LEGION_GRANT_FILE and leaves the bash input untouched", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const secretsDir = await mkdtemp(path.join(os.tmpdir(), "legion-secrets-"));
    await chmod(secretsDir, 0o700);
    temporaryPaths.push(secretsDir);
    // The daemon names the controller pane's grant file exactly as a worker's
    // (`credentialProcessEnvironment` in processes.ts).
    const grantFile = path.join(secretsDir, "legion-omp-controller-grant");
    process.env.LEGION_GRANT_FILE = grantFile;
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_CONTROLLER = "1";
    process.env.LEGION_ROLE = "controller";
    process.env.LEGION_CONTROLLER_SECRET = "controller-secret";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/state") return Response.json(redactedLegionState("omp"));
      if (url.pathname === "/legion/v1/controller/ready") return Response.json({});
      if (url.pathname === "/legion/v1/grants") {
        return Response.json({ grantId: "controller-grant-1", expiresAt: "2099-01-01T00:00:00Z" });
      }
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

    // Before the claim completes nothing can mint for the controller, so its call passes
    // through unwrapped rather than being blocked as an unregistered worker.
    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "call-controller-unclaimed", input: { command: "true" } },
        context
      )
    ).resolves.toBeUndefined();

    await sessionStart({}, context);
    const result = await toolCall(
      {
        toolName: "bash",
        toolCallId: "call-controller-bash",
        input: {
          command: "legion gh -- pr merge 7 --squash",
          env: { KEEP: "model-value", LEGION_GRANT: "made-up-by-the-model" },
        },
      },
      context
    );

    // The controller grant travels exactly as a worker's does: written to the pane's grant file,
    // with neither `command` nor `env` rewritten — the hook returns nothing.
    expect(result).toBeUndefined();
    expect(await grantFileContents(grantFile)).toEqual({
      grant: "controller-grant-1",
      mode: 0o600,
    });
    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/grants",
      body: { sessionId: "ses_controller_bash", secret: "controller-secret" },
    });
    // LEGION-45: the controller does not commit and is not a phase worker; the operation-log guard
    // never binds it. Its `jj undo` reaches the grant wrapper like any other bash call: not
    // blocked, a second grant minted and written.
    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "call-controller-jj",
          input: { command: "jj -R /tmp/legion-workspace undo" },
        },
        context
      )
    ).resolves.toBeUndefined();
    expect(requests.filter((request) => request.path === "/legion/v1/grants")).toHaveLength(2);
  });
  test("blocks a controller bash call when the daemon refuses the controller grant", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const secretsDir = await mkdtemp(path.join(os.tmpdir(), "legion-secrets-"));
    temporaryPaths.push(secretsDir);
    process.env.LEGION_GRANT_FILE = path.join(secretsDir, "legion-omp-controller-grant");
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
      if (url.pathname === "/legion/v1/grants") {
        return Response.json({ error: "Invalid controller capability" }, { status: 403 });
      }
      return Response.json({
        session_id: "ses_controller_refused",
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
    const context = sessionContext("ses_controller_refused");
    await sessionStart({}, context);

    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "call-controller-refused",
          input: { command: "legion state" },
        },
        context
      )
    ).resolves.toEqual({
      block: true,
      reason: 'POST /legion/v1/grants failed with 403: {"error":"Invalid controller capability"}',
    });
  });
  /** The daemon pane's environment for the controller re-claim tests; returns the grant file the
   * daemon would name on the pane. */
  const controllerPaneEnvironment = async (): Promise<string> => {
    const secretsDir = await mkdtemp(path.join(os.tmpdir(), "legion-secrets-"));
    temporaryPaths.push(secretsDir);
    const grantFile = path.join(secretsDir, "legion-omp-controller-grant");
    process.env.LEGION_GRANT_FILE = grantFile;
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_CONTROLLER = "1";
    process.env.LEGION_CONTROLLER_SECRET = "controller-secret";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    return grantFile;
  };
  /** One SessionManager per pane, exactly as OMP hands it out: `/new` mutates the manager the
   * boot-time contexts (and every handler closure) already hold, so the live id and file are
   * read through it, never frozen in a context object. */
  const controllerPane = (
    ticks: (() => void)[]
  ): { readonly context: SessionContext; switchTo: (id: string, file: string) => void } => {
    let liveSessionID = "ses_pane_first";
    let liveSessionFile = "/tmp/first.jsonl";
    return {
      context: {
        ...sessionContext(liveSessionID),
        sessionManager: {
          getSessionId: () => liveSessionID,
          getSessionFile: () => liveSessionFile,
          ensureOnDisk: async () => undefined,
        },
        setInterval: (callback) => ticks.push(callback),
      },
      switchTo: (id, file) => {
        liveSessionID = id;
        liveSessionFile = file;
      },
    };
  };
  const controllerReadyBody = (sessionId: string, ompSessionFile?: string) => ({
    path: "/legion/v1/controller/ready",
    body: {
      secret: "controller-secret",
      sessionId,
      ...(ompSessionFile === undefined ? {} : { ompSessionFile }),
      pluginVersion: PLUGIN_VERSION,
    },
  });

  test("re-claims the controller and re-reports its transcript when /new changes the pane's session id, keeping bash wrapped", async () => {
    const grantFile = await controllerPaneEnvironment();
    const token = "legion-omp-controller";
    const listener = controllerPaneListener(token);
    globalThis.fetch = listener.fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const sessionSwitch = fixture.handlers.get("session_switch");
    const toolCall = fixture.handlers.get("tool_call");
    if (sessionStart === undefined || sessionSwitch === undefined || toolCall === undefined) {
      throw new Error("controller lifecycle handlers were not registered");
    }
    const pane = controllerPane([]);
    await sessionStart({}, pane.context);

    // Sami types /new into the pane: OMP moves it to a fresh session id and transcript.
    pane.switchTo("ses_pane_second", "/tmp/second.jsonl");
    await sessionSwitch({ reason: "new" }, pane.context);

    const readies = listener.requests.filter((r) => r.path === "/legion/v1/controller/ready");
    expect(readies).toEqual([
      controllerReadyBody("ses_pane_first", "/tmp/first.jsonl"),
      controllerReadyBody("ses_pane_second", "/tmp/second.jsonl"),
    ]);
    expect(listener.registry.holder).toBe("ses_pane_second");

    const result = await toolCall(
      { toolName: "bash", toolCallId: "call-after-switch", input: { command: "legion state" } },
      pane.context
    );
    // Minted for the new session through the same wrapper as the boot-time claim: its grant is
    // written to the pane's grant file, the bash input untouched.
    expect(result).toBeUndefined();
    expect(await grantFileContents(grantFile)).toEqual({
      grant: "grant-for-ses_pane_second",
      mode: 0o600,
    });
    expect(listener.requests.at(-1)).toEqual({
      path: "/legion/v1/grants",
      body: { sessionId: "ses_pane_second", secret: "controller-secret" },
    });
  });

  test("a tree navigation that changes nothing and a task subagent's switch re-claim nothing; a tree navigation after the transcript moved under the same id re-reports the moved file", async () => {
    await controllerPaneEnvironment();
    const token = "legion-omp-controller";
    const listener = controllerPaneListener(token);
    globalThis.fetch = listener.fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const sessionTree = fixture.handlers.get("session_tree");
    if (sessionStart === undefined || sessionTree === undefined) {
      throw new Error("controller lifecycle handlers were not registered");
    }
    const pane = controllerPane([]);
    await sessionStart({}, pane.context);

    // A tree navigation that leaves the session id and transcript as they were re-claims
    // nothing: every /controller/ready runs a forced resync and must not be posted for nothing.
    await sessionTree({ newLeafId: "leaf" }, pane.context);
    // A task-spawned subagent inside the pane loads its own instance of this module with the
    // pane's environment; its switch events must never take the controller role or record the
    // subagent's transcript as the pane's.
    const { childFile } = await createSubagentTranscriptPaths();
    const subagent = createPi();
    legionExtension(subagent.pi);
    const subagentSwitch = subagent.handlers.get("session_switch");
    if (subagentSwitch === undefined) throw new Error("subagent switch handler missing");
    await subagentSwitch({ reason: "new" }, sessionContext("ses_subagent", childFile));
    expect(listener.requests.filter((r) => r.path === "/legion/v1/controller/ready")).toEqual([
      controllerReadyBody("ses_pane_first", "/tmp/first.jsonl"),
    ]);
    expect(listener.registry.holder).toBe("ses_pane_first");

    // `!cd <dir>` (or /move) typed into the pane relocates the transcript under the same session
    // id with no session event of its own; the next tree navigation is where the daemon's
    // recorded resume target follows it.
    pane.switchTo("ses_pane_first", "/tmp/elsewhere/first.jsonl");
    await sessionTree({ newLeafId: "leaf-2" }, pane.context);
    expect(listener.requests.filter((r) => r.path === "/legion/v1/controller/ready")).toEqual([
      controllerReadyBody("ses_pane_first", "/tmp/first.jsonl"),
      controllerReadyBody("ses_pane_first", "/tmp/elsewhere/first.jsonl"),
    ]);
  });

  // OMP dispatches one session event to every extension's handler; the manifest lists envoy.ts
  // before legion.ts, but nothing in the code may depend on that. The claim legion.ts makes on
  // /new must reach the pane's own envoy instance in either order, because only that instance's
  // heartbeat re-asserts the role (LEGION-29): under the previous last-bound-wins routing the
  // legion-first order handed the claim to the subagent's instance, whose heartbeat then saw the
  // pane's id as drift and soft-claimed the controller role for the subagent's session.
  for (const order of ["envoy.ts", "legion.ts"] as const) {
    test(`after /new in a pane that ran a task subagent, the heartbeat re-asserts the controller for the pane's new session when ${order} handles the switch first`, async () => {
      await controllerPaneEnvironment();
      const token = "legion-omp-controller";
      const listener = controllerPaneListener(token);
      globalThis.fetch = listener.fetch;
      const fixture = createPi({ bindEnvoy: order === "envoy.ts" });
      legionExtension(fixture.pi);
      if (order === "legion.ts") envoyExtension(fixture.pi as never);
      const sessionStart = fixture.handlers.get("session_start");
      const sessionSwitch = fixture.handlers.get("session_switch");
      if (sessionStart === undefined || sessionSwitch === undefined) {
        throw new Error("controller lifecycle handlers were not registered");
      }
      const paneTicks: (() => void)[] = [];
      const pane = controllerPane(paneTicks);
      await sessionStart({}, pane.context);
      expect(listener.registry.holder).toBe("ses_pane_first");

      // The pane runs a `task`: a fresh pair of extension instances binds in this process and
      // the subagent's session starts.
      const { childFile } = await createSubagentTranscriptPaths();
      const subagent = createPi();
      legionExtension(subagent.pi);
      const subagentStart = subagent.handlers.get("session_start");
      if (subagentStart === undefined) throw new Error("subagent handlers were not registered");
      const subagentTicks: (() => void)[] = [];
      await subagentStart(
        {},
        {
          ...sessionContext("ses_subagent", childFile),
          setInterval: (callback) => subagentTicks.push(callback),
        }
      );

      // Sami types /new into the pane.
      pane.switchTo("ses_pane_second", "/tmp/second.jsonl");
      await sessionSwitch({ reason: "new" }, pane.context);
      await listener.readyPosted(2);
      expect(listener.registry.holder).toBe("ses_pane_second");

      // A subagent shares the pane's Envoy identity: its instance registers no session and so
      // runs no heartbeat that could see the pane's new id as drift.
      expect(subagentTicks).toHaveLength(0);
      expect(listener.registry.holder).toBe("ses_pane_second");

      // The listener later loses sight of the pane (a reaped claim, a listener restart). Only
      // the pane's own heartbeat is registered here, and only a topic its instance holds is
      // re-asserted — so the claim must have reached the pane's instance: it soft-claims for the
      // session the pane holds now and re-runs controller/ready with its transcript, repairing a
      // first ready call that arrived before the daemon had a locator.
      listener.registry.lostClaim = true;
      expect(paneTicks).toHaveLength(1);
      paneTicks[0]?.();
      await listener.readyPosted(3);
      expect(
        listener.requests.filter((r) => r.path === "/legion/v1/controller/ready").at(-1)
      ).toEqual(controllerReadyBody("ses_pane_second", "/tmp/second.jsonl"));
      expect(listener.requests.filter((r) => r.path === "/v1/roles/set").at(-1)).toEqual({
        path: "/v1/roles/set",
        body: { session_id: "ses_pane_second", role: token, soft: true },
      });
      expect(listener.registry.holder).toBe("ses_pane_second");
      const claimsBySubagent = listener.requests.filter(
        (r) =>
          r.path === "/v1/roles/set" &&
          typeof r.body === "object" &&
          r.body !== null &&
          "session_id" in r.body &&
          r.body.session_id === "ses_subagent"
      );
      expect(claimsBySubagent).toEqual([]);
    });
  }
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
  test("logs and exits on a non-retryable worker registration refusal", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    const errorLog = spyOn(console, "error").mockImplementation(() => {});
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_BOOT_TOKEN = "stale-plugin-contract";
    process.env.LEGION_GENERATION = "1";
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ISSUE = "REPO-43";
    process.env.LEGION_ROLE = "implementer";
    process.env.LEGION_WORKSPACE = "/tmp/legion-workspace";
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/worker/started") {
        return Response.json({ error: "pluginVersion is required" }, { status: 400 });
      }
      return Response.json({});
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("worker lifecycle handler was not registered");

    try {
      await expect(sessionStart({}, sessionContext("ses_bad_contract"))).rejects.toThrow(
        "process would exit"
      );
      expect(exits).toEqual([1]);
      expect(errorLog.mock.calls.map(String)).toContain(
        '[legion] worker/started registration failed (400): {"error":"pluginVersion is required"}'
      );
    } finally {
      errorLog.mockRestore();
    }
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
  test("exits the process when the daemon refuses the session at worker/started with the same-agent 409", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    const errorLog = spyOn(console, "error").mockImplementation(() => {});
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_BOOT_TOKEN = "resumed-boot-token";
    process.env.LEGION_GENERATION = "2";
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ISSUE = "REPO-43";
    process.env.LEGION_ROLE = "implementer";
    process.env.LEGION_WORKSPACE = "/tmp/legion-workspace";
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/worker/started") {
        return Response.json(
          { error: "Worker respawn must resume the same agent session" },
          { status: 409 }
        );
      }
      return Response.json({});
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("worker lifecycle handler was not registered");

    try {
      await expect(sessionStart({}, sessionContext("ses_fresh_not_resumed"))).rejects.toThrow(
        "process would exit"
      );
      expect(exits).toEqual([1]);
      expect(errorLog.mock.calls.map(String).join("\n")).toContain(
        "Worker respawn must resume the same agent session"
      );
    } finally {
      errorLog.mockRestore();
    }
  });
  test("does not exit on a 500 at worker/started: the error propagates and the process stays for the daemon's retry", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_BOOT_TOKEN = "boot-token";
    process.env.LEGION_GENERATION = "1";
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ISSUE = "REPO-43";
    process.env.LEGION_ROLE = "implementer";
    process.env.LEGION_WORKSPACE = "/tmp/legion-workspace";
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/worker/started") {
        return Response.json({ error: "state save failed" }, { status: 500 });
      }
      return Response.json({});
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("worker lifecycle handler was not registered");

    await expect(sessionStart({}, sessionContext("ses_daemon_500"))).rejects.toThrow(
      "state save failed"
    );
    expect(exits).toEqual([]);
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
  test("exits the process when the daemon refuses the session at process/started with the same-agent 409", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    const errorLog = spyOn(console, "error").mockImplementation(() => {});
    const tree = "REPO-42";
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "4";
    process.env.LEGION_BOOT_TOKEN = "resumed-root-boot-token";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json(
          { error: "Worker respawn must resume the same agent session" },
          { status: 409 }
        );
      }
      return Response.json({});
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("root lifecycle handler was not registered");

    try {
      await expect(sessionStart({}, sessionContext("ses_root_fresh_not_resumed"))).rejects.toThrow(
        "process would exit"
      );
      expect(exits).toEqual([1]);
      expect(errorLog.mock.calls.map(String).join("\n")).toContain(
        "Worker respawn must resume the same agent session"
      );
    } finally {
      errorLog.mockRestore();
    }
  });
  test("does not exit on a 500 at process/started: the error propagates and the process stays for the daemon's retry", async () => {
    const exits: number[] = [];
    setLegionBootstrapExitForTests((code) => {
      exits.push(code);
      throw new Error("process would exit");
    });
    const tree = "REPO-42";
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "root-boot-token";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_ROLE = "architect";
    process.env.LEGION_ISSUE = tree;
    globalThis.fetch = (async (input) => {
      const url = new URL(input.toString());
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({ error: "state save failed" }, { status: 500 });
      }
      return Response.json({});
    }) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("root lifecycle handler was not registered");

    await expect(sessionStart({}, sessionContext("ses_root_daemon_500"))).rejects.toThrow(
      "state save failed"
    );
    expect(exits).toEqual([]);
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
        input: {
          op: "register_gate",
          issue,
          artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
          version: 2,
        },
        request: {
          path: "/legion/v1/gates/register",
          body: {
            tree,
            issue,
            artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
            version: 2,
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
            requestId: expect.stringMatching(UUID_PATTERN),
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

    // One request id per spawn_worker tool call: two more identical calls mint two more ids.
    const spawnInput = { op: "spawn_worker", issue, role: "implementer", task: "Implement again" };
    await legion.execute("call-spawn-a", spawnInput, undefined, undefined, context);
    await legion.execute("call-spawn-b", spawnInput, undefined, undefined, context);
    const spawnRequestIds = requests
      .filter((request) => request.path === "/legion/v1/worker/spawn")
      .map(({ body }) =>
        body !== null && typeof body === "object" && "requestId" in body
          ? body.requestId
          : undefined
      );
    expect(spawnRequestIds).toHaveLength(3);
    expect(new Set(spawnRequestIds).size).toBe(3);

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

    // The retired ask-id form is refused before any request leaves the session.
    expect(
      await legion.execute(
        "call-ask-id",
        { op: "register_gate", issue, askId: "ask-1" },
        undefined,
        undefined,
        context
      )
    ).toEqual({
      content: [{ type: "text", text: 'register_gate does not accept field "askId"' }],
      details: {},
      isError: true,
    });
    expect(
      await legion.execute(
        "call-no-version",
        { op: "register_gate", issue, artifactId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4" },
        undefined,
        undefined,
        context
      )
    ).toEqual({
      content: [{ type: "text", text: "register_gate requires a positive integer version" }],
      details: {},
      isError: true,
    });
    // The slug the architect typed into dispatch_request_approval is not the document id its
    // result carries; the daemon's approval events name the id, so a slug never opens a gate.
    expect(
      await legion.execute(
        "call-slug",
        { op: "register_gate", issue, artifactId: "spec", version: 2 },
        undefined,
        undefined,
        context
      )
    ).toEqual({
      content: [
        {
          type: "text",
          text: 'register_gate requires artifactId to be the document id (a UUID) from dispatch_request_approval\'s result, not "spec"',
        },
      ],
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
    // LEGION-45: the root architect's blanket bash gate answers first; the phase-worker
    // operation-log guard never reaches it (its behaviour is unchanged by that guard).
    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "architect-bash-jj", input: { command: "jj undo" } },
        context
      )
    ).resolves.toEqual({
      block: true,
      reason: "the architect delegates all code work to phase workers",
    });
  });
  test("ships a roles/<role>.md residue file for every LegionRole", async () => {
    // The daemon reads packages/pi-envoy/roles/${role}.md for every phase-worker role, including a
    // sub-architect (packages/daemon/src/daemon/processes.ts launchWorker). Phase workers also compose
    // their core and headless mechanics parts; the sub-architect remains single-file. A missing residue
    // 500s the spawn.
    for (const role of LEGION_ROLES) {
      const rolePath = path.join(import.meta.dir, "..", "roles", `${role}.md`);
      await access(rolePath);
      // A zero-byte file would pass the existence check above and boot a worker with no
      // instructions at all -- fail loudly on that instead of leaving it a silent runtime bug.
      expect((await readFile(rolePath, "utf8")).trim()).not.toBe("");
    }
  });
  test("keeps shared phase-worker mechanics in one fragment and required-skills guidance in the applicable residues", async () => {
    const rolesDir = path.join(import.meta.dir, "..", "roles");
    const phaseRoles = ["planner", "implementer", "tester", "reviewer", "merger"] as const;
    const rolesWithRequiredSkillsSentence = ["implementer", "reviewer", "tester"];
    const requiredSkillsSentence =
      "Then read the plan handoff's `requiredSkills` for your role and follow those too.";
    const mechanics = await readFile(path.join(rolesDir, "mechanics", "headless.md"), "utf8");

    expect(mechanics).toContain("## Step one: find this repository's skills");
    expect(mechanics).toContain("When your phase is done, stay in this session afterwards:");

    for (const role of phaseRoles) {
      const residue = await readFile(path.join(rolesDir, `${role}.md`), "utf8");
      expect(residue).toContain(`# Legion ${role.charAt(0).toUpperCase()}${role.slice(1)}`);
      expect(residue.includes(requiredSkillsSentence)).toBe(
        rolesWithRequiredSkillsSentence.includes(role)
      );
    }
  });
});
