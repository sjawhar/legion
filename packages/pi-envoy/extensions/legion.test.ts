import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  jest,
  mock,
  spyOn,
  test,
} from "bun:test";
import { cp, mkdir, mkdtemp, readdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { type IssueKey, LEGION_ROLES, type LegionRole, roleToken } from "@legion/contracts";
import { logger } from "@oh-my-pi/pi-utils";
import pkg from "../package.json";
import { noteInjectedUserTurn, resetInjectedUserTurnsForTests } from "../src/dispatch-user-turn";
import { classifySession } from "../src/legion/classify";
import { LOCAL_ENVOY_NOTICE } from "../src/legion/phase-stall";
import type {
  CommandContext,
  PiApi,
  RegisteredTool,
  SessionContext,
  ZodNumberProperty,
} from "../src/pi-types";

const natsConnections: {
  readonly name: string;
  readonly subjects: string[];
  readonly unsubscribed: string[];
  closed: boolean;
  /** The connection dies: every subscription's iterator ends, as nats.js ends them. */
  readonly drop: () => void;
}[] = [];
/** A connect for a connection name calls its gate, when a test sets one, and waits on it. */
const natsConnectGates = new Map<string, () => Promise<void>>();
// @legion/envoy-client/nats-auth resolves the NATS credential with the real nkey exports.
const { nkeyAuthenticator, nkeys } = await import("nats");
mock.module("nats", () => ({
  nkeyAuthenticator,
  nkeys,
  connect: async (options: { readonly name: string }) => {
    await natsConnectGates.get(options.name)?.();
    const endings: (() => void)[] = [];
    const connection = {
      name: options.name,
      subjects: [] as string[],
      unsubscribed: [] as string[],
      closed: false,
      drop: () => {
        connection.closed = true;
        for (const end of endings.splice(0)) end();
      },
    };
    natsConnections.push(connection);
    return {
      close: async () => undefined,
      drain: async () => undefined,
      isClosed: () => connection.closed,
      publish: () => undefined,
      subscribe: (subject: string) => {
        connection.subjects.push(subject);
        const ended = Promise.withResolvers<void>();
        endings.push(ended.resolve);
        return {
          unsubscribe: () => {
            connection.unsubscribed.push(subject);
          },
          [Symbol.asyncIterator]: () => ({
            next: async () => {
              await ended.promise;
              return { done: true, value: undefined };
            },
          }),
        };
      },
    };
  },
  StringCodec: () => ({
    decode: (data: Uint8Array) => new TextDecoder().decode(data),
    encode: (text: string) => new TextEncoder().encode(text),
  }),
}));

import { hostAgentRegistryMock, testAgentRoster } from "./test-host-registry";

mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async () => undefined,
  ...hostAgentRegistryMock,
}));

// The extension modules must load after their OMP and NATS host dependencies are mocked.
const { default: envoyExtension } = await import("./envoy");
const { resetLegionRoleClaimBridgeForTests } = await import("../src/legion/role-claim-bridge");
const { resetLegionBootstrappedSessionForTests } = await import("../src/subagent-session");
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
  readonly sendUserMessage: PiApi["sendUserMessage"];
  readonly appendEntry: PiApi["appendEntry"];
  readonly setSessionName: PiApi["setSessionName"];
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
  "LEGION_BOOT_TOKEN",
  "LEGION_TREE",
  "LEGION_PROJECT",
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

/** The repository every workspace in this file is a copy of: one `jj git init` before the tests.
 * A copy spawns no process, so a test's workspace costs none of bun's 5 s test timeout, which a
 * `jj` process, whose run time is the machine's load, would otherwise spend. */
let jjTemplate = "";

beforeAll(async () => {
  jjTemplate = await mkdtemp(path.join(os.tmpdir(), "legion-omp-extension-template-"));
  const child = Bun.spawn(["jj", "git", "init", jjTemplate], { stdout: "ignore", stderr: "pipe" });
  const exitCode = await child.exited;
  if (exitCode !== 0) {
    // A spawn killed under the runner exits non-zero with nothing on stderr, so name the code and
    // signal too: the empty stderr alone reads as jj refusing the command (LEGION-243).
    const stderr = (await new Response(child.stderr as ReadableStream<Uint8Array>).text()).trim();
    const signal = child.signalCode === null ? "" : `, signal ${child.signalCode}`;
    throw new Error(
      `jj git init exited ${exitCode}${signal}: ${stderr === "" ? "no stderr" : stderr}`
    );
  }
});

afterAll(async () => {
  await rm(jjTemplate, { force: true, recursive: true });
});

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
  natsConnectGates.clear();
  setLegionBootstrapExitForTests((code) => process.exit(code) as never);
  resetLegionBootstrappedSessionForTests();
  testAgentRoster().splice(0);
  resetInjectedUserTurnsForTests();
  for (const key of environmentKeys) {
    const value = baselineEnvironment[key];
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  await Promise.all(
    temporaryPaths.splice(0).map((directory) => rm(directory, { force: true, recursive: true }))
  );
});

/** A custom entry as OMP stores what `pi.appendEntry` persisted, and as `getBranch()` returns it
 * to a resumed session. */
type AppendedEntry = {
  readonly type: "custom";
  readonly customType: string;
  readonly data: unknown;
};

/** The session's title as the pinned host's session manager holds it: `pi.setSessionName` stores
 * the name with `titleSource: "user"` (oh-my-pi `modes/runtime-init.ts`), a rename does the same,
 * and OMP's title model stores `auto`. A fixture holds one, since OMP keeps one session manager per
 * process and a `/new` clears its title in place. `set` records each `pi.setSessionName` call. */
type HostTitle = { name?: string; source?: "auto" | "user"; readonly set: string[] };

function createPi(options: { readonly bindEnvoy?: boolean } = {}): {
  readonly commands: RegisteredCommand[];
  readonly handlers: Map<string, Handler>;
  readonly tools: RegisteredTool[];
  readonly sentMessages: SentMessage[];
  readonly entries: AppendedEntry[];
  readonly activeTools: string[];
  readonly title: HostTitle;
  readonly pi: TestPi;
} {
  const commands: RegisteredCommand[] = [];
  const handlers = new Map<string, Handler>();
  const registeredHandlers = new Map<string, Handler[]>();
  const tools: RegisteredTool[] = [];
  const sentMessages: SentMessage[] = [];
  const entries: AppendedEntry[] = [];
  const title: HostTitle = { set: [] };
  const activeTools = ["read", "task", "hub"];
  const property = (): ZodNumberProperty => ({
    optional: property,
    nullable: property,
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
      boolean: optional,
      array: () => optional(),
      enum: () => optional(),
      unknown: () => optional(),
      discriminatedUnion: () => ({}),
    },
    sendMessage: (message) => sentMessages.push(message),
    sendUserMessage: () => undefined,
    appendEntry: (customType, data) => {
      entries.push({ type: "custom", customType, data });
    },
    setSessionName: async (name) => {
      title.set.push(name);
      title.name = name;
      title.source = "user";
    },
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
  return { commands, handlers, tools, sentMessages, entries, activeTools, title, pi };
}

/** One SessionManager per pane, exactly as OMP hands it out: `/new` mutates the manager the
 * boot-time contexts (and every handler closure) already hold, so the live id and file are
 * read through it, never frozen in a context object. */
function controllerPane(ticks: (() => void)[]): {
  readonly context: SessionContext;
  switchTo: (id: string, file: string) => void;
} {
  let liveSessionID = "ses_pane_first";
  let liveSessionFile = "/tmp/first.jsonl";
  return {
    context: {
      ...sessionContext(liveSessionID),
      sessionManager: {
        getSessionId: () => liveSessionID,
        getSessionFile: () => liveSessionFile,
        ensureOnDisk: async () => undefined,
        getEntries: () => [],
      },
      setInterval: (callback) => ticks.push(callback),
    },
    switchTo: (id, file) => {
      liveSessionID = id;
      liveSessionFile = file;
    },
  };
}

function sessionContext(
  sessionID: string,
  sessionFile = "/tmp/session.jsonl",
  ensureOnDisk: () => Promise<void> = async () => undefined,
  title?: HostTitle
): SessionContext {
  return {
    cwd: "/tmp/legion-workspace",
    hasUI: true,
    taskDepth: 0,
    sessionManager: {
      getSessionId: () => sessionID,
      getSessionFile: () => sessionFile,
      ensureOnDisk,
      getEntries: () => [],
      ...(title === undefined ? {} : hostTitleReader(title)),
    },
    setInterval: () => undefined,
    setTimeout: () => undefined,
    ui: { notify: () => undefined },
  };
}

/** The session manager's title reads, answered from `title` as OMP's are from its own state. */
function hostTitleReader(
  title: HostTitle
): Pick<SessionContext["sessionManager"], "getSessionName" | "getHeader"> {
  return {
    getSessionName: () => title.name,
    getHeader: () => ({
      ...(title.name === undefined ? {} : { title: title.name }),
      ...(title.source === undefined ? {} : { titleSource: title.source }),
    }),
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
  await cp(jjTemplate, directory, { recursive: true });
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

interface DaemonRequest {
  readonly path: string;
  readonly body: unknown;
}

interface ClaimPane {
  readonly claimToken: string;
  /** The daemon's `claims/register` answer: the claim it issued this pane. */
  readonly registration: {
    readonly claimToken: string;
    readonly tree: IssueKey;
    readonly issue: IssueKey;
    readonly role: LegionRole;
    readonly generation: number;
    readonly secret: string;
  };
  readonly bootToken: string;
  /** The pane's `LEGION_GRANT_FILE`, `<claim>-grant` in a 0700 secrets directory. */
  readonly grantFile: string;
  readonly secretsDir: string;
  /** Every request the pane made, daemon and Envoy listener alike, in order. */
  readonly requests: DaemonRequest[];
  readonly exits: number[];
  /** What `console.error` printed while `start()` ran. */
  readonly errors: string[];
  readonly intervals: (() => void)[];
  readonly tools: RegisteredTool[];
  readonly activeTools: string[];
  readonly entries: AppendedEntry[];
  readonly title: HostTitle;
  readonly handlers: Map<string, Handler>;
  readonly context: SessionContext;
  readonly toolCall: Handler;
  /** Runs `session_start` for the pane's session, as Oh My Pi does when the process starts. */
  readonly start: () => Promise<unknown>;
}

/** A pane the daemon launched for a claim (a root architect, or a phase worker): the identity
 * variables the tmux runtime sets (`packages/daemon/internal/runtime/tmux/spawn.go`'s
 * `panePairs`), the boot token as a 0600 file behind `LEGION_BOOT_TOKEN_FILE`, and
 * `LEGION_GRANT_FILE` naming `<claim>-grant` beside it, under a state directory of its own. The
 * stub answers `extraRoutes` first, then the daemon's claim routes (`register` and `ready`
 * override their answers) and grants (`grant-<n>`), and 404s every other daemon path exactly as
 * the daemon's catch-all does, so a route the extension reached that the daemon does not serve is
 * visible in `requests` and never mistaken for a success. Each pane is an Oh My Pi process of its
 * own, so nothing an earlier pane recorded process-wide (its bootstrapped session, its Envoy role
 * bridge) carries over. `branch` is what the session's `getBranch()` returns: a resumed session's
 * transcript entries. `title` is the title the session already carries when it starts.
 * `bindEnvoy: false` loads legion.ts alone (`createPi`'s option). Nothing runs until `start()`;
 * `bootPane` is the started pane. */
async function claimPane(options: {
  readonly role: LegionRole;
  readonly tree?: IssueKey;
  readonly issue?: IssueKey;
  readonly sessionId?: string;
  readonly sessionFile?: string;
  readonly workspace?: string;
  readonly register?: () => Response | Promise<Response>;
  readonly ready?: (attempt: number) => Response | Promise<Response>;
  readonly readyPosted?: () => void;
  readonly roleHolder?: () => string | undefined;
  readonly extraRoutes?: (url: URL, body: unknown) => Response | undefined;
  readonly ensureOnDisk?: () => Promise<void>;
  readonly branch?: readonly unknown[];
  readonly title?: { readonly name: string; readonly source: "auto" | "user" };
  readonly bindEnvoy?: boolean;
}): Promise<ClaimPane> {
  resetLegionBootstrappedSessionForTests();
  resetLegionRoleClaimBridgeForTests();
  const tree = options.tree ?? "REPO-42";
  const issue = options.issue ?? "REPO-43";
  const sessionId = options.sessionId ?? `ses_${options.role}`;
  const workspace = options.workspace ?? "/tmp/legion-workspace";
  const claimToken = roleToken("omp", issue, options.role);
  const registration = {
    claimToken,
    tree,
    issue,
    role: options.role,
    generation: 2,
    secret: `secret-${sessionId}`,
  };
  const bootToken = `boot-${sessionId}`;
  const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-pane-state-"));
  const secretsDir = path.join(stateDir, "secrets");
  await mkdir(secretsDir, { mode: 0o700 });
  temporaryPaths.push(stateDir);
  const bootTokenFile = path.join(secretsDir, claimToken);
  const grantFile = path.join(secretsDir, `${claimToken}-grant`);
  await writeFile(bootTokenFile, `${bootToken}\n`, { mode: 0o600 });
  process.env.ENVOY_URL = "http://envoy.test";
  process.env.LEGION_DAEMON_URL = "http://daemon.test";
  process.env.LEGION_BOOT_TOKEN_FILE = bootTokenFile;
  process.env.LEGION_PROJECT = "omp";
  process.env.LEGION_TREE = tree;
  process.env.LEGION_ISSUE = issue;
  process.env.LEGION_ROLE = options.role;
  process.env.LEGION_WORKSPACE = workspace;
  process.env.LEGION_STATE_DIR = stateDir;
  process.env.LEGION_GRANT_FILE = grantFile;

  const requests: DaemonRequest[] = [];
  let readyAttempts = 0;
  let grants = 0;
  globalThis.fetch = (async (input, init) => {
    const url = new URL(input.toString());
    const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
    requests.push({ path: url.pathname, body });
    const extra = options.extraRoutes?.(url, body);
    if (extra !== undefined) return extra;
    if (url.pathname === "/legion/v1/claims/register") {
      return (await options.register?.()) ?? Response.json(registration);
    }
    if (url.pathname === "/legion/v1/claims/ready") {
      readyAttempts += 1;
      options.readyPosted?.();
      return (await options.ready?.(readyAttempts)) ?? new Response(null, { status: 204 });
    }
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({ grantId: `grant-${grants}`, expiresAt: "2099-01-01T00:00:00Z" });
    }
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: "no route" }, { status: 404 });
    }
    if (url.pathname === `/v1/roles/${claimToken}`) {
      const holder = options.roleHolder?.() ?? sessionId;
      if (holder === "") {
        return Response.json({ error: `no holder for role ${claimToken}` }, { status: 404 });
      }
      return Response.json({ role: claimToken, holder, last_seen: 1 });
    }
    return Response.json({
      session_id: sessionId,
      machine_id: "machine",
      dir: workspace,
      topics: [claimToken],
    });
  }) as typeof fetch;

  const exits: number[] = [];
  setLegionBootstrapExitForTests((code) => {
    exits.push(code);
    throw new Error("process would exit");
  });
  const intervals: (() => void)[] = [];
  const fixture = createPi({ bindEnvoy: options.bindEnvoy });
  if (options.title !== undefined) {
    fixture.title.name = options.title.name;
    fixture.title.source = options.title.source;
  }
  legionExtension(fixture.pi);
  const sessionStart = fixture.handlers.get("session_start");
  const toolCall = fixture.handlers.get("tool_call");
  if (sessionStart === undefined || toolCall === undefined) {
    throw new Error("the pane's lifecycle handlers were not registered");
  }
  const base = sessionContext(
    sessionId,
    options.sessionFile ?? `/tmp/${sessionId}.jsonl`,
    options.ensureOnDisk,
    fixture.title
  );
  const context: SessionContext = {
    ...base,
    sessionManager: { ...base.sessionManager, getBranch: () => options.branch ?? [] },
    cwd: workspace,
    setInterval: (callback) => {
      intervals.push(callback);
    },
  };
  const errors: string[] = [];
  return {
    claimToken,
    registration,
    bootToken,
    grantFile,
    secretsDir,
    requests,
    exits,
    errors,
    intervals,
    tools: fixture.tools,
    activeTools: fixture.activeTools,
    entries: fixture.entries,
    title: fixture.title,
    handlers: fixture.handlers,
    context,
    toolCall,
    start: async () => {
      const errorLog = spyOn(console, "error").mockImplementation((...args: unknown[]) => {
        errors.push(args.map(String).join(" "));
      });
      try {
        return await sessionStart({}, context);
      } finally {
        errorLog.mockRestore();
      }
    },
  };
}

async function bootPane(options: Parameters<typeof claimPane>[0]): Promise<ClaimPane> {
  const pane = await claimPane(options);
  await pane.start();
  return pane;
}

const daemonRequests = <T extends { readonly path: string }>(requests: readonly T[]) =>
  requests.filter((request) => request.path.startsWith("/legion/"));

const grantRequests = (requests: readonly DaemonRequest[]) =>
  requests.filter((request) => request.path === "/legion/v1/grants");

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
  test("a claim registers, claims its Envoy role, and reports ready through the claim routes alone", async () => {
    const pane = await bootPane({ role: "tester", sessionId: "ses_worker" });

    expect(daemonRequests(pane.requests)).toEqual([
      {
        path: "/legion/v1/claims/register",
        body: {
          bootToken: pane.bootToken,
          sessionId: "ses_worker",
          ompSessionFile: "/tmp/ses_worker.jsonl",
          agentId: "ses_worker",
          pluginContract: pkg.legion.daemonApiVersion,
        },
      },
      {
        path: "/legion/v1/claims/ready",
        body: {
          claimToken: pane.claimToken,
          sessionId: "ses_worker",
          secret: pane.registration.secret,
          generation: pane.registration.generation,
        },
      },
    ]);
    // The Envoy role is the claim token, claimed after the registration issued it and before ready.
    const paths = pane.requests.map((request) => request.path);
    const roleClaim = pane.requests.findIndex(
      (request) =>
        request.path === "/v1/roles/set" &&
        JSON.stringify(request.body) ===
          JSON.stringify({ session_id: "ses_worker", role: pane.claimToken })
    );
    expect(roleClaim).toBeGreaterThan(paths.indexOf("/legion/v1/claims/register"));
    expect(roleClaim).toBeLessThan(paths.indexOf("/legion/v1/claims/ready"));
    expect(pane.exits).toEqual([]);
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
    expect(fixture.title.set).toEqual([]);
  });
  for (const [kind, role, issue] of [
    ["root-architect", "architect", "REPO-42"],
    ["phase-worker", "implementer", "REPO-43"],
  ] as const) {
    test(`never bootstraps, claims a role, or exits for a subagent session, even with ${kind} environment`, async () => {
      const { childFile } = await createSubagentTranscriptPaths();
      const pane = await claimPane({
        role,
        issue,
        sessionId: `ses_sub_${role}`,
        sessionFile: childFile,
      });

      await pane.start();

      expect(daemonRequests(pane.requests)).toEqual([]);
      expect(pane.requests.some((request) => request.path === "/v1/roles/set")).toBe(false);
      expect(pane.exits).toEqual([]);
      expect(pane.tools.find((tool) => tool.name === "legion")).toBeUndefined();
      expect(pane.title.set).toEqual([]);

      // No tool gate was installed for this session either: a plain bash call, which an
      // unregistered root/phase worker would otherwise have blocked, passes through untouched.
      await expect(
        pane.toolCall(
          { toolName: "bash", toolCallId: `call-sub-${role}-bash`, input: { command: "ls" } },
          pane.context
        )
      ).resolves.toBeUndefined();
    });
  }
  test("recognises a subagent by the session this process already bootstrapped when no transcript is on disk", async () => {
    // With the transcript in a SQL row there is no parent `.jsonl` beside the subagent's path, so
    // the on-disk layout says nothing; the guard must still fall back on what this process booted.
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

    const root = await bootPane({
      role: "architect",
      issue: "REPO-42",
      sessionId: "ses_root_sql",
      sessionFile: rootFile,
    });
    expect(root.tools.find((tool) => tool.name === "legion")).toBeDefined();
    const requestsAfterRoot = root.requests.length;

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
    const afterRoot = root.requests.slice(requestsAfterRoot).map((request) => request.path);
    expect(afterRoot.filter((requestPath) => requestPath !== "/v1/interests/subscribe")).toEqual(
      []
    );
    expect(root.exits).toEqual([]);
    expect(subagent.tools.find((tool) => tool.name === "legion")).toBeUndefined();
    await expect(
      subagentToolCall(
        { toolName: "bash", toolCallId: "call-sql-subagent-bash", input: { command: "ls" } },
        subagentContext
      )
    ).resolves.toBeUndefined();
  });
  test("refuses a subagent's operation-log rewrite and `legion handoff complete` in a phase-worker pane while its other calls stay ungated", async () => {
    const { childFile } = await createSubagentTranscriptPaths();
    const pane = await bootPane({
      role: "implementer",
      sessionId: "ses_sub_worker_jj",
      sessionFile: childFile,
    });
    const { toolCall, context } = pane;

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
    // A subagent has no legion tool, and a handoff from its bash would complete the worker's phase
    // where the phase stall cannot see it.
    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "call-sub-worker-handoff",
          input: { command: "legion handoff complete --summary done" },
        },
        context
      )
    ).resolves.toEqual({ block: true, reason: expect.stringContaining("`legion` tool") });
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
    expect(daemonRequests(pane.requests)).toEqual([]);
  });
  // Oh My Pi's `ensureOnDisk` publishes the transcript and throws its SessionLockError when another
  // writer holds that file's publish lock; the rewrite is discarded and a later publish may succeed.
  const sessionLockError = (sessionFile: string): Error => {
    const error = new Error(
      `Session publish lock unavailable for ${sessionFile}: another writer holds the publish lock. The staged rewrite was discarded without publishing.`
    );
    error.name = "SessionLockError";
    return error;
  };
  /** A hook's outcome: a rejected hook is a failed tool call (or session start). */
  type HookOutcome = { readonly value: unknown } | { readonly error: string };
  /** Each hook's outcome, run one after another in order. */
  const hookOutcomes = async (hooks: readonly (() => unknown)[]): Promise<HookOutcome[]> => {
    const outcomes: HookOutcome[] = [];
    for (const hook of hooks) {
      outcomes.push(
        await Promise.resolve()
          .then(hook)
          .then(
            (value) => ({ value }),
            (error: unknown) => ({ error: error instanceof Error ? error.message : String(error) })
          )
      );
    }
    return outcomes;
  };
  test("a subagent whose first transcript publish loses the session lock is still recognised at the next hook, and no tool call fails", async () => {
    const { childFile } = await createSubagentTranscriptPaths();
    // The host's roster does not list this session, so the transcript decides; its first publish
    // loses the lock to another writer and every later one succeeds.
    const lockError = sessionLockError(childFile);
    let publishes = 0;
    // legion.ts alone, so the publish that loses the lock is this instance's own first check (each
    // extension instance keeps its own answer; envoy.ts asks through the same check).
    const pane = await claimPane({
      role: "implementer",
      sessionId: "ses_sub_lock",
      sessionFile: childFile,
      bindEnvoy: false,
      ensureOnDisk: async () => {
        publishes++;
        if (publishes === 1) throw lockError;
      },
    });
    const bash = (id: string) =>
      pane.toolCall({ toolName: "bash", toolCallId: id, input: { command: "ls" } }, pane.context);
    const warnings: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(JSON.stringify(entry));
    });
    let outcomes: HookOutcome[];
    try {
      outcomes = await hookOutcomes([
        () => pane.start(),
        () => bash("call-sub-lock-first"),
        () => bash("call-sub-lock-second"),
      ]);
    } finally {
      stopSink();
    }

    // Every hook answers as a subagent's: no claim, and a bash call an unregistered phase worker
    // would have had blocked passes through ungated.
    expect(outcomes).toEqual([{ value: undefined }, { value: undefined }, { value: undefined }]);
    expect(daemonRequests(pane.requests)).toEqual([]);
    expect(pane.exits).toEqual([]);
    expect(pane.tools.find((tool) => tool.name === "legion")).toBeUndefined();
    // The failed publish was asked again once, at the next hook, and the settled answer is kept.
    expect(publishes).toBe(2);
    expect(warnings.filter((warning) => warning.includes(lockError.message))).toHaveLength(1);
  });
  test("a subagent the host's roster names is recognised without publishing its transcript", async () => {
    // No parent transcript sits beside this path (a `--no-session` parent's subagents land in a
    // temporary omp-task-* directory), and every publish loses the lock: only the roster can say.
    const baseDirectory = await mkdtemp(path.join(os.tmpdir(), "legion-roster-subagent-"));
    temporaryPaths.push(baseDirectory);
    const childFile = path.join(baseDirectory, "omp-task-scout", "Scout.jsonl");
    testAgentRoster().push(
      {
        id: "Main",
        kind: "main",
        session: { sessionManager: { getSessionId: () => "ses_roster_parent" } },
        sessionFile: null,
      },
      {
        id: "Scout",
        kind: "sub",
        session: { sessionManager: { getSessionId: () => "ses_roster_sub" } },
        sessionFile: childFile,
      }
    );
    let publishes = 0;
    const pane = await claimPane({
      role: "architect",
      issue: "REPO-42",
      sessionId: "ses_roster_sub",
      sessionFile: childFile,
      ensureOnDisk: async () => {
        publishes++;
        throw sessionLockError(childFile);
      },
    });

    const outcomes = await hookOutcomes([
      () => pane.start(),
      () =>
        pane.toolCall(
          { toolName: "bash", toolCallId: "call-roster-sub-bash", input: { command: "ls" } },
          pane.context
        ),
    ]);

    expect(outcomes).toEqual([{ value: undefined }, { value: undefined }]);
    expect(daemonRequests(pane.requests)).toEqual([]);
    expect(pane.exits).toEqual([]);
    expect(pane.tools.find((tool) => tool.name === "legion")).toBeUndefined();
    expect(publishes).toBe(0);
  });
  test("a session the host's roster calls main boots as the top-level session even where its transcript sits like a subagent's", async () => {
    // The roster's answer outranks the transcript layout. A check that let the layout win would
    // need the transcript, and so a publish, before it could answer: LEGION-491's lock race again.
    // On disk the layout says subagent: a `.jsonl` sits beside this transcript's directory.
    const { childFile } = await createSubagentTranscriptPaths();
    testAgentRoster().push({
      id: "Main",
      kind: "main",
      session: { sessionManager: { getSessionId: () => "ses_roster_main" } },
      sessionFile: childFile,
    });
    const pane = await bootPane({
      role: "architect",
      issue: "REPO-42",
      sessionId: "ses_roster_main",
      sessionFile: childFile,
    });

    expect(daemonRequests(pane.requests).map((request) => request.path)).toContain(
      "/legion/v1/claims/register"
    );
    expect(pane.tools.find((tool) => tool.name === "legion")).toBeDefined();
    expect(pane.exits).toEqual([]);
  });
  test("throws naming the missing variable when a phase worker boots without LEGION_BOOT_TOKEN", async () => {
    const pane = await claimPane({ role: "tester" });
    delete process.env.LEGION_BOOT_TOKEN_FILE;

    await expect(pane.start()).rejects.toThrow("LEGION_BOOT_TOKEN is required for Legion");
  });
  test("boots a phase worker with the boot token read from LEGION_BOOT_TOKEN_FILE, ignoring LEGION_BOOT_TOKEN", async () => {
    const pane = await claimPane({ role: "tester" });
    process.env.LEGION_BOOT_TOKEN = "decoy";
    await pane.start();

    expect(daemonRequests(pane.requests)[0]).toMatchObject({
      path: "/legion/v1/claims/register",
      body: { bootToken: pane.bootToken },
    });
  });
  test("exits the phase worker naming LEGION_BOOT_TOKEN_FILE and its path when the file is unreadable, never falling back to LEGION_BOOT_TOKEN", async () => {
    const pane = await claimPane({ role: "tester" });
    process.env.LEGION_BOOT_TOKEN = "decoy";
    process.env.LEGION_BOOT_TOKEN_FILE = "/nonexistent/legion-secrets/tester";

    await expect(pane.start()).rejects.toThrow(
      "LEGION_BOOT_TOKEN_FILE names /nonexistent/legion-secrets/tester, which could not be read"
    );
    expect(daemonRequests(pane.requests)).toEqual([]);
  });
  test("registers the legion tool, and only it, once a sub-architect's claim boots", async () => {
    const pane = await claimPane({ role: "architect" });
    const toolsBeforeBoot = pane.tools.length;

    await pane.start();

    expect(pane.tools.slice(toolsBeforeBoot).map((tool) => tool.name)).toEqual(["legion"]);
    expect(pane.activeTools).toContain("legion");
  });
  test("allows a single `legion ...` bash invocation for an architect worker but blocks chaining, other commands, and `legion handoff complete`", async () => {
    const { toolCall, context } = await bootPane({ role: "architect" });
    const denied = "the architect delegates all code work to phase workers";
    const isAllowed = async (command: string): Promise<boolean> => {
      const result = await toolCall(
        { toolName: "bash", toolCallId: `call-${command}`, input: { command } },
        context
      );
      return !(typeof result === "object" && result !== null && "block" in result && result.block);
    };

    expect(await isAllowed("legion gh -- pr view 1")).toBe(true);
    expect(await isAllowed("legion state")).toBe(true);
    // A sub-architect's handoffs are the legion tool's actions, never a bash command.
    expect(await isAllowed("legion handoff complete --summary x")).toBe(false);
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
    // for all of them: a boot that wrote its identity there would set the author for every other
    // tree (LEGION-44). A sentinel written before boot must survive it — neither overwritten
    // with the daemon-reported identity nor removed (that one-time cleanup is provisioning's,
    // daemon-side).
    const workspace = await createJjWorkspace();
    await setJjRepoConfig(workspace, "user.name", "Sentinel Before Boot");

    await bootPane({ role: "implementer", workspace });

    expect(await jjRepoConfig(workspace, "user.name")).toBe('user.name = "Sentinel Before Boot"');
    expect(await jjRepoConfig(workspace, "user.email")).toBe("");
  });
  test("restricts phase-worker tool access per LEGION_ROLE", async () => {
    const blockedReason = (role: LegionRole, toolName: string): string | undefined => {
      if (role === "architect" && ["edit", "write", "apply_patch"].includes(toolName)) {
        return "the architect delegates all code work to phase workers";
      }
      if (role === "architect" && toolName === "task") {
        return "the architect delegates only through child issues and the daemon's phase workers; Legion runs one agent per process";
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
      const { toolCall, context } = await bootPane({ role, sessionId: `ses_${role}` });
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
    const { toolCall: architectToolCall, context: architectContext } = await bootPane({
      role: "architect",
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
        "the architect delegates only through child issues and the daemon's phase workers; Legion runs one agent per process",
    });

    const { toolCall: implementerToolCall, context: implementerContext } = await bootPane({
      role: "implementer",
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
      const { toolCall, context } = await bootPane({ role, sessionId: `ses_${role}_xd` });

      // A tool-device invocation (write to an `xd://` path, the scheme in any case, as Oh My Pi
      // routes it) is a tool call, not a file mutation, and must pass for every gated role.
      for (const path of ["xd://dispatch_ask", "XD://dispatch_doc_edit"]) {
        await expect(
          toolCall(
            { toolName: "write", toolCallId: `call-${role}-xd-ok`, input: { path, content: "{}" } },
            context
          )
        ).resolves.toBeUndefined();
      }

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
    const { toolCall, context, requests } = await bootPane({
      role: "implementer",
      sessionId: "ses_implementer_jj_log",
    });
    const mints = (): number => grantRequests(requests).length;
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
    const { toolCall, context, requests } = await bootPane({
      role: "implementer",
      sessionId: "ses_implementer_jj_ok",
    });
    const mints = (): number => grantRequests(requests).length;
    const mintsBefore = mints();
    // The spec's negatives plus the exact commands the legion-worker skill has every role run:
    // the conflict merge, the fingerprint (a `|` inside quotes), the handoff split, the log filter.
    const allowed = [
      "jj restore src/x.ts",
      'jj -R "$LEGION_WORKSPACE" restore packages/pi-envoy/extensions/legion.ts',
      "jj op log",
      'jj -R "$LEGION_WORKSPACE" op log -n 5',
      "jj op show",
      'jj describe -m "undo this"',
      'jj -R "$LEGION_WORKSPACE" new legion/LEGION-1 main@origin -m "merge: resolve conflict against main@origin"',
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
      architect: "legion gh -- pr view 7",
    };
    for (const role of LEGION_ROLES) {
      const { toolCall, context } = await bootPane({ role, sessionId: `ses_${role}_jj_guard` });
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
    const { toolCall, context } = await bootPane({
      role: "tester",
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
  test("refuses `legion handoff complete` in a phase worker's bash, eval code, and hub input, and leaves the shell's write and read alone", async () => {
    // The phase stall closes only on the tool's handoff_complete; a completion run from bash
    // would leave it open and draw a follow-up asking the worker to complete again. Writes and
    // reads leave no phase open, and stdin is the shell's route for a payload past argv's cap.
    const { toolCall, context, requests } = await bootPane({
      role: "implementer",
      sessionId: "ses_implementer_handoff_bash",
    });
    const mints = (): number => grantRequests(requests).length;
    const bash = (command: string) => ({ toolName: "bash", input: { command } });
    const refused: { readonly toolName: string; readonly input: Record<string, unknown> }[] = [
      bash("legion handoff complete --summary done"),
      bash('legion "handoff" "complete" --summary done'),
      bash('"$LEGION_STATE_DIR/bin/legion" handoff complete --summary done'),
      bash("env LEGION_GRANT=x legion handoff complete --summary done"),
      bash("bash -lc 'legion handoff complete --summary done'"),
      {
        toolName: "eval",
        input: {
          language: "py",
          code: 'subprocess.run(["legion", "handoff", "complete", "--summary", s])',
        },
      },
      {
        toolName: "hub",
        input: {
          op: "start",
          name: "x",
          application: "legion",
          args: ["handoff", "complete", "--summary", "done"],
        },
      },
    ];
    const allowed: { readonly toolName: string; readonly input: Record<string, unknown> }[] = [
      bash("legion gh -- pr view 7"),
      bash(`legion handoff write --phase implement --data '{"proof":["ran it"]}'`),
      bash(
        "jq '.rounds += [$r]' --argjson r '{}' .legion/test.json | legion handoff write --phase test"
      ),
      bash("legion handoff read"),
      bash('"$LEGION_STATE_DIR/bin/legion" handoff read --phase plan'),
      bash("legion handoff write --help"),
      bash("legion state"),
      bash("legion threads resolve --pr 7 --repo o/r"),
      // A path through `legion/handoff`, and a message that mentions a handoff, run no handoff.
      bash("cat packages/pi-envoy/src/legion/handoff-actions.ts"),
      bash('jj -R "$LEGION_WORKSPACE" split -m "implement: record handoff" .legion/implement.json'),
      { toolName: "eval", input: { language: "py", code: 'print(read(".legion/plan.json"))' } },
    ];
    const mintsBefore = mints();
    const allowedByMistake: string[] = [];
    for (const [index, call] of refused.entries()) {
      const result = await toolCall({ ...call, toolCallId: `call-handoff-${index}` }, context);
      const blocked =
        typeof result === "object" && result !== null && "block" in result && result.block === true;
      if (!blocked) allowedByMistake.push(JSON.stringify(call));
    }
    expect(allowedByMistake).toEqual([]);
    // A refused command never mints a grant: nothing ran, so nothing ran under one.
    expect(mints()).toBe(mintsBefore);
    const refusedByMistake: string[] = [];
    for (const [index, call] of allowed.entries()) {
      const result = await toolCall({ ...call, toolCallId: `call-handoff-ok-${index}` }, context);
      if (result !== undefined) {
        refusedByMistake.push(`${JSON.stringify(call)} -> ${JSON.stringify(result)}`);
      }
    }
    expect(refusedByMistake).toEqual([]);
    // The refusal names the command and the tool action that does it instead.
    const named = await toolCall(
      { ...bash("legion handoff complete --summary done"), toolCallId: "call-handoff-named" },
      context
    );
    for (const phrase of [
      "legion handoff complete --summary done",
      "`legion` tool",
      "handoff_complete",
    ]) {
      expect(named).toEqual({ block: true, reason: expect.stringContaining(phrase) });
    }
  });
  test("mints a grant before every tool call Oh My Pi serves by running gh, and before no other", async () => {
    const { toolCall, context, grantFile, requests } = await bootPane({ role: "implementer" });

    // Oh My Pi resolves `pr://` and `issue://` through its internal-URL router, which runs `gh`,
    // from every tool that takes a path (the scheme in any case; a list split on `;`, `,`, or
    // whitespace; one pair of outer double quotes stripped), and its `github` tool runs `gh` for
    // every op. On a Legion pane `gh` is the shim that runs `legion gh`, which redeems the grant
    // file; a grant lives 60 seconds, so each of these mints its own.
    const served = [
      { toolName: "read", input: { path: "pr://acme/widgets/7" } },
      { toolName: "read", input: { path: "issue://7:1-20" } },
      { toolName: "grep", input: { pattern: "fix", path: "src; PR://acme/widgets/7" } },
      { toolName: "glob", input: { path: "issue://acme/widgets" } },
      { toolName: "ast_edit", input: { ops: [], paths: ["src/a.ts", "pr://7"] } },
      { toolName: "read", input: { path: "src, pr://acme/widgets/7" } },
      { toolName: "grep", input: { pattern: "fix", path: "src issue://acme/widgets/8" } },
      { toolName: "read", input: { path: '"pr://acme/widgets/7"' } },
      { toolName: "github", input: { op: "pr_view", pr: "7" } },
    ];
    for (const [index, call] of served.entries()) {
      await expect(
        toolCall({ ...call, toolCallId: `call-gh-served-${index}` }, context)
      ).resolves.toBeUndefined();
      expect({ call, minted: grantRequests(requests).length }).toEqual({ call, minted: index + 1 });
    }
    expect(await grantFileContents(grantFile)).toEqual({
      grant: `grant-${served.length}`,
      mode: 0o600,
    });

    // A read of a file, or of a GitHub URL (fetched over HTTP, never through gh), redeems nothing,
    // and neither does a path whose `pr://` follows no separator: Oh My Pi reads it as a file.
    const unserved = [
      { toolName: "read", input: { path: "src/pr-view.ts" } },
      { toolName: "read", input: { path: "docs/pr://x" } },
      { toolName: "read", input: { path: "https://github.com/acme/widgets/pull/7" } },
      { toolName: "grep", input: { pattern: "pr://", path: "src" } },
      { toolName: "read", input: { path: "skill://pr-review" } },
    ];
    for (const [index, call] of unserved.entries()) {
      await toolCall({ ...call, toolCallId: `call-gh-unserved-${index}` }, context);
    }
    expect(grantRequests(requests)).toHaveLength(served.length);
  });
  /**
   * Fixture note: `createPi().on` keeps every registered handler and its aggregate returns the
   * last non-undefined result, mirroring the host's `emitToolCall`, which also never chains one
   * handler's revised input into the next. Stacked handlers are therefore observable only by
   * counting `/legion/v1/grants` requests, never by inspecting a returned input.
   */
  test("ignores whatever env or text the model supplied and never rewrites command or env", async () => {
    const { toolCall, context, grantFile, requests } = await bootPane({ role: "implementer" });

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
    expect(grantRequests(requests)).toHaveLength(1);
  });
  test("creates the grant file's missing directory 0700, as an Agent Sandbox pod's empty state volume has none", async () => {
    const { toolCall, context, secretsDir } = await bootPane({ role: "implementer" });
    const grantFile = path.join(secretsDir, "state", "secrets", "x-grant");
    process.env.LEGION_GRANT_FILE = grantFile;

    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "call-new-dir", input: { command: "jj git push" } },
        context
      )
    ).resolves.toBeUndefined();
    expect(await grantFileContents(grantFile)).toEqual({ grant: "grant-1", mode: 0o600 });
    expect((await stat(path.dirname(grantFile))).mode & 0o777).toBe(0o700);
  });
  test("blocks the command naming the path when the grant file cannot be written", async () => {
    const workspace = await createJjWorkspace();
    const { toolCall, context, secretsDir, requests } = await bootPane({
      role: "implementer",
      workspace,
    });
    // A regular file where the grant file's directory should be: no directory can be made there.
    const notADirectory = path.join(secretsDir, "not-a-directory");
    await writeFile(notADirectory, "", { mode: 0o600 });
    const unwritable = path.join(notADirectory, "secrets", "x-grant");
    process.env.LEGION_GRANT_FILE = unwritable;

    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "call-unwritable", input: { command: "jj git push" } },
        context
      )
    ).resolves.toEqual({
      block: true,
      reason: expect.stringMatching(
        new RegExp(
          `^LEGION_GRANT_FILE ${unwritable.replaceAll(".", "\\.")} could not be written: .*ENOTDIR`
        )
      ),
    });
    // Mint-then-write: the grant was minted (one wasted 60 s grant), nothing ran under a stale one.
    expect(grantRequests(requests)).toHaveLength(1);
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
    const { toolCall, context, requests } = await bootPane({ role: "implementer" });
    const blocked = {
      block: true,
      reason:
        "LEGION_GRANT_FILE is not set on this pane: the daemon that launched it predates this plugin; relaunch the pane from a daemon on the matching release (a daemon restart keeps a live pane as it was launched)",
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
    expect(grantRequests(requests)).toHaveLength(0);
  });
  test("blocks a booted worker's bash calls when the daemon refuses to mint a grant", async () => {
    const { toolCall, context } = await bootPane({
      role: "reviewer",
      extraRoutes: (url) =>
        url.pathname === "/legion/v1/grants"
          ? Response.json({ error: "grant minting unavailable" }, { status: 503 })
          : undefined,
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
      reason: "POST /legion/v1/grants failed with 503: grant minting unavailable",
    });
  });
  test("blocks a bash call from a worker session that has not completed its boot handshake", async () => {
    // The pane's launch environment is complete; only the boot handshake is missing.
    const { toolCall } = await claimPane({ role: "implementer" });

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
  test("materializes the session transcript before the boot handshake", async () => {
    const order: string[] = [];
    const pane = await claimPane({
      role: "architect",
      issue: "REPO-42",
      sessionId: "ses_ensure_on_disk",
      ensureOnDisk: async () => {
        order.push("ensureOnDisk");
      },
      extraRoutes: (url) => {
        order.push(`fetch:${url.pathname}`);
        return undefined;
      },
    });
    await pane.start();

    expect(order.indexOf("ensureOnDisk")).toBeGreaterThanOrEqual(0);
    expect(order.indexOf("ensureOnDisk")).toBeLessThan(
      order.indexOf("fetch:/legion/v1/claims/register")
    );
  });
  test("registers no legion tool until a session's claim boots", () => {
    const fixture = createPi();

    legionExtension(fixture.pi);

    expect(fixture.tools.find((tool) => tool.name === "legion")).toBeUndefined();
  });
  test("blocks code tools in a root architect session", async () => {
    const { toolCall, context } = await bootPane({
      role: "architect",
      issue: "REPO-42",
      sessionId: "ses_policy_architect",
    });

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
  test("admits a root architect's single `legion` bash command, `legion handoff read` included, except `legion handoff complete`", async () => {
    const { toolCall, context } = await bootPane({
      role: "architect",
      issue: "REPO-42",
      sessionId: "ses_root_architect_handoff",
    });
    const bash = (command: string) =>
      toolCall({ toolName: "bash", toolCallId: `call-${command}`, input: { command } }, context);

    await expect(bash("legion gh -- pr view 1")).resolves.toBeUndefined();
    await expect(bash("legion state")).resolves.toBeUndefined();
    // A root architect reads a committed handoff from the root issue's workspace with the shell.
    await expect(bash("legion handoff read --phase plan")).resolves.toBeUndefined();
    await expect(bash('legion "handoff" read')).resolves.toBeUndefined();
    await expect(bash("legion handoff complete --summary x")).resolves.toEqual({
      block: true,
      reason: expect.stringContaining("`legion` tool's `handoff_complete`"),
    });
  });

  describe("a phase worker left idle with its phase open", () => {
    // What OMP hands the hooks: the daemon's assignment arrives as a user message (its RPC
    // `prompt`), an Envoy delivery as an `envoy-message` custom message, and `session_stop` fires
    // when a top-level run is about to settle, carrying the run's last assistant message.
    const assignment = {
      message: {
        role: "user",
        attribution: "user",
        content: [{ type: "text", text: "Implement REPO-43." }],
      },
    };
    const envoyEvent = {
      message: {
        role: "custom",
        customType: "envoy-message",
        display: true,
        content: "envoy:\n  notice: CI finished",
      },
    };
    const settlingOn = (text: string, signal = new AbortController().signal) => ({
      messages: [],
      turn_id: 0,
      last_assistant_message: {
        role: "assistant",
        content: [{ type: "text", text }],
        stopReason: "stop",
      },
      session_id: "ses_stall",
      stop_hook_active: false,
      signal,
    });
    const originalPath = process.env.PATH;
    afterEach(() => {
      process.env.PATH = originalPath;
    });
    /** A stand-in `legion` first on PATH, where the daemon's launcher is on a pane's: it records its
     * arguments and the grant file's contents, and exits `exitCode`. Returns its call log. */
    const fakeLegion = async (exitCode: number): Promise<string> => {
      const bin = await mkdtemp(path.join(os.tmpdir(), "legion-fake-cli-"));
      temporaryPaths.push(bin);
      const log = path.join(bin, "calls.log");
      await writeFile(
        path.join(bin, "legion"),
        `#!/bin/sh\nprintf '%s\\n' "$*" >> '${log}'\nprintf 'grant %s\\n' "$(cat "$LEGION_GRANT_FILE" 2>/dev/null)" >> '${log}'\ncat > '${path.join(bin, "stdin")}'\necho "legion ran"\nexit ${exitCode}\n`,
        { mode: 0o755 }
      );
      process.env.PATH = `${bin}:${originalPath}`;
      return log;
    };
    const hook = (handlers: Map<string, Handler>, name: string): Handler => {
      const found = handlers.get(name);
      if (found === undefined) throw new Error(`the ${name} handler was not registered`);
      return found;
    };
    /** The session's hooks, driven as OMP drives them. */
    const session = (handlers: Map<string, Handler>, context: SessionContext) => ({
      arrives: (message: unknown) => hook(handlers, "message_start")(message, context),
      settles: (text: string, signal?: AbortSignal) =>
        hook(handlers, "session_stop")(settlingOn(text, signal), context),
    });
    const bootStalling = async (options: {
      readonly role?: LegionRole;
      readonly issue?: IssueKey;
      readonly branch?: readonly unknown[];
    }) => {
      const workspace = await mkdtemp(path.join(os.tmpdir(), "legion-stall-workspace-"));
      temporaryPaths.push(workspace);
      const booted = await bootPane({
        role: options.role ?? "implementer",
        issue: options.issue,
        sessionId: "ses_stall",
        workspace,
        branch: options.branch,
        extraRoutes: (url) =>
          url.pathname === "/legion/v1/grants"
            ? Response.json({ grantId: "grant-stall", expiresAt: "2099-01-01T00:00:00Z" })
            : undefined,
      });
      const legionTool = booted.tools.find((tool) => tool.name === "legion");
      if (legionTool === undefined) throw new Error("the worker has no legion tool");
      return {
        ...booted,
        ...session(booted.handlers, booted.context),
        legionTool,
        /** Calls the tool's `handoff_complete` with a stand-in `legion` exiting `exitCode`. */
        completes: async (exitCode: number) => {
          const log = await fakeLegion(exitCode);
          const result = await legionTool.execute(
            "call-complete",
            { op: "handoff_complete", summary: "Done." },
            undefined,
            undefined,
            booted.context
          );
          return { result, calls: (await readFile(log, "utf8")).split("\n").filter(Boolean) };
        },
      };
    };
    const followUp = (text: string) => ({
      continue: true,
      additionalContext: expect.stringContaining(text),
    });

    test("a turn that settles after the assignment with no handoff gets one follow-up: run the legion tool's handoff_complete, or reply WAITING", async () => {
      const worker = await bootStalling({});
      await worker.arrives(assignment);

      const result = await worker.settles("I pushed the change and CI is green.");

      expect(result).toEqual(followUp("handoff_complete"));
      expect(result).toEqual(followUp("WAITING"));
      expect(result).not.toEqual(followUp("written as text"));
    });

    test("a turn that ends on a tool call written as text is told the call did not run", async () => {
      const worker = await bootStalling({});
      await worker.arrives(assignment);

      // The Stage 3 transcript's last message (1ee62d31, the round-2 implementer): the model's
      // final `legion handoff complete` came back as text, not a tool call.
      const result = await worker.settles(
        'court\n<invoke name="bash">\n<parameter name="command">cd -- "$LEGION_WORKSPACE" && legion handoff complete --summary \'Round 2 done.\'</parameter>\n</invoke>'
      );

      expect(result).toEqual(followUp("written as text"));
      expect(result).toEqual(followUp("handoff_complete"));
    });

    test("one follow-up per stall: the next settle is quiet until a new inbound event opens the next stall", async () => {
      const worker = await bootStalling({});
      await worker.arrives(assignment);
      expect(await worker.settles("Done, I think.")).toEqual(followUp("handoff_complete"));

      expect(await worker.settles("Still thinking about it.")).toBeUndefined();

      await worker.arrives(envoyEvent);
      expect(await worker.settles("Read the notice.")).toEqual(followUp("handoff_complete"));
    });

    test("a WAITING reply gets no follow-up, and none comes until a new inbound event", async () => {
      const worker = await bootStalling({});
      await worker.arrives(assignment);

      expect(await worker.settles("Pushed.\nWAITING: CI on the pull request")).toBeUndefined();
      expect(await worker.settles("Nothing new yet.")).toBeUndefined();

      await worker.arrives(envoyEvent);
      expect(await worker.settles("CI passed.")).toEqual(followUp("handoff_complete"));
    });

    test("a successful handoff_complete runs legion handoff complete with a fresh grant and closes the phase; an inbound event does not reopen it, the next assignment does", async () => {
      const worker = await bootStalling({});
      await worker.arrives(assignment);

      const { result, calls } = await worker.completes(0);
      expect(result).toEqual({
        content: [{ type: "text", text: "legion ran" }],
        details: { operation: "handoff_complete", exitCode: 0 },
      });
      expect(calls).toEqual(["handoff complete --summary Done.", "grant grant-stall"]);
      expect(await worker.settles("Reported.")).toBeUndefined();

      // After its phase a worker stays to answer other roles' questions over Envoy.
      await worker.arrives(envoyEvent);
      expect(await worker.settles("Answered the reviewer.")).toBeUndefined();

      await worker.arrives(assignment);
      expect(await worker.settles("Round 2 pushed.")).toEqual(followUp("handoff_complete"));
    });

    // A person's Send or Aside from Dispatch's Agents page reaches the session as its own user
    // turn (extensions/envoy.ts), a user message exactly like the daemon's assignment. It is an
    // inbound event, as its Envoy card was before: it does not open a phase, and it wakes a stall
    // that already had its follow-up or a WAITING reply.
    const personsMessage = {
      message: {
        role: "user",
        attribution: "user",
        content: [{ type: "text", text: "Where is the dashboard?" }],
        timestamp: 1,
      },
    };

    test("a person's direct message the session took as its own turn opens no phase; the daemon's assignment still does", async () => {
      const worker = await bootStalling({});
      noteInjectedUserTurn("ses_stall", "Where is the dashboard?", "m-1");
      await worker.arrives(personsMessage);
      expect(await worker.settles("It is at /dash.")).toBeUndefined();

      await worker.arrives(assignment);
      expect(await worker.settles("Pushed.")).toEqual(followUp("handoff_complete"));
    });

    test("a person's direct message wakes a stall that had its WAITING reply, as its card did", async () => {
      const worker = await bootStalling({});
      await worker.arrives(assignment);
      expect(await worker.settles("WAITING: CI on the pull request")).toBeUndefined();

      noteInjectedUserTurn("ses_stall", "Where is the dashboard?", "m-1");
      await worker.arrives(personsMessage);
      expect(await worker.settles("It is at /dash.")).toEqual(followUp("handoff_complete"));
    });

    test("the same words arriving again as a user message are the daemon's, not the person's", async () => {
      const worker = await bootStalling({});
      noteInjectedUserTurn("ses_stall", "Where is the dashboard?", "m-1");
      await worker.arrives(personsMessage);
      expect(await worker.settles("It is at /dash.")).toBeUndefined();

      await worker.arrives({ message: { ...personsMessage.message, timestamp: 2 } });
      expect(await worker.settles("Answered.")).toEqual(followUp("handoff_complete"));
    });

    test("a handoff_complete whose command fails is an error result and leaves the phase open", async () => {
      const worker = await bootStalling({});
      await worker.arrives(assignment);

      const { result } = await worker.completes(1);
      expect(result).toEqual({
        content: [{ type: "text", text: "legion ran" }],
        details: { operation: "handoff_complete", exitCode: 1 },
        isError: true,
      });
      expect(await worker.settles("Reported.")).toEqual(followUp("handoff_complete"));
    });

    test("the Envoy extension's own notice (a dispatch_ask's follow notice) does not re-arm a quiet stall", async () => {
      const worker = await bootStalling({});
      await worker.arrives(assignment);
      expect(await worker.settles("Done, I think.")).toEqual(followUp("handoff_complete"));

      // The worker answers the follow-up by opening a dispatch_ask, and the Envoy extension steers
      // its follow notice into the session: the worker's own doing, not an event from outside.
      await worker.arrives({
        message: {
          ...envoyEvent.message,
          content: "Following ask ask-1 on REPO-43: its answer and replies reach you directly.",
          details: LOCAL_ENVOY_NOTICE,
        },
      });
      expect(await worker.settles("Asked the human which schema to use.")).toBeUndefined();

      await worker.arrives(envoyEvent);
      expect(await worker.settles("Read the answer.")).toEqual(followUp("handoff_complete"));
    });

    test("handoff_complete takes a phase only when it is the worker's own handoff phase, and refuses any other naming the expected one", async () => {
      const planner = await bootStalling({ role: "planner" });
      await planner.arrives(assignment);
      const log = await fakeLegion(0);

      expect(
        await planner.legionTool.execute(
          "call-wrong-phase",
          { op: "handoff_complete", phase: "implement", summary: "Planned." },
          undefined,
          undefined,
          planner.context
        )
      ).toEqual({
        content: [
          {
            type: "text",
            text: 'handoff_complete\'s phase is "plan" for a planner: omit phase, or pass "plan"',
          },
        ],
        details: {},
        isError: true,
      });
      expect(await planner.settles("Tried to report.")).toEqual(followUp("handoff_complete"));

      expect(
        await planner.legionTool.execute(
          "call-own-phase",
          { op: "handoff_complete", phase: "plan", summary: "Planned." },
          undefined,
          undefined,
          planner.context
        )
      ).toEqual({
        content: [{ type: "text", text: "legion ran" }],
        details: { operation: "handoff_complete", exitCode: 0 },
      });
      // The command takes no phase: the pane's LEGION_ROLE already names it.
      expect((await readFile(log, "utf8")).split("\n").filter(Boolean)).toEqual([
        "handoff complete --summary Planned.",
        "grant grant-stall",
      ]);

      const merger = await bootStalling({ role: "merger" });
      expect(
        await merger.legionTool.execute(
          "call-merger-phase",
          { op: "handoff_complete", phase: "review", summary: "Ready.", ready: true },
          undefined,
          undefined,
          merger.context
        )
      ).toEqual({
        content: [
          {
            type: "text",
            text: "handoff_complete takes no phase for a merger, which writes no handoff",
          },
        ],
        details: {},
        isError: true,
      });
    });

    test("the handoff actions run the daemon's own legion handoff commands, handoff_write's payload on stdin", async () => {
      const worker = await bootStalling({});
      const log = await fakeLegion(0);
      const stdin = path.join(path.dirname(log), "stdin");
      await worker.legionTool.execute(
        "call",
        { op: "handoff_write", phase: "implement", data: { proof: ["ran it"] } },
        undefined,
        undefined,
        worker.context
      );
      expect(await readFile(stdin, "utf8")).toBe('{"proof":["ran it"]}');
      await worker.legionTool.execute(
        "call",
        { op: "handoff_read", phase: "plan" },
        undefined,
        undefined,
        worker.context
      );
      expect(await readFile(stdin, "utf8")).toBe("");
      const calls = (await readFile(log, "utf8")).split("\n").filter(Boolean);
      expect(calls.filter((line) => !line.startsWith("grant "))).toEqual([
        "handoff write --phase implement",
        "handoff read --phase plan",
      ]);
    });

    test("handoff_write delivers a payload over one argv string's 128 KiB cap", async () => {
      // Linux refuses a single argument over MAX_ARG_STRLEN (131,072 bytes) with E2BIG, and a
      // tester's handoff that accumulates review rounds outgrows it (this repository's largest
      // was 179,251 bytes).
      const worker = await bootStalling({});
      const log = await fakeLegion(0);
      const rounds = Array.from({ length: 2000 }, (_, round) => ({ round, note: "r".repeat(80) }));
      const data = { verdict: "pass", rounds };
      expect(JSON.stringify(data).length).toBeGreaterThan(128 * 1024);
      const result = await worker.legionTool.execute(
        "call",
        { op: "handoff_write", phase: "test", data },
        undefined,
        undefined,
        worker.context
      );
      expect(result).not.toHaveProperty("isError");
      expect(await readFile(path.join(path.dirname(log), "stdin"), "utf8")).toBe(
        JSON.stringify(data)
      );
      expect((await readFile(log, "utf8")).split("\n")[0]).toBe("handoff write --phase test");
    });

    test("the legion tool has no handoff_message: a call runs no command, and a message for another role goes through envoy_publish", async () => {
      // Nothing reads a handoff message: no action or prompt reads `.legion/messages/`, while a
      // role's live questions go through envoy_publish and a later phase reads the handoff files.
      const worker = await bootStalling({});
      const log = await fakeLegion(0);
      const result = await worker.legionTool.execute(
        "call-message",
        { op: "handoff_message", sender: "implement", recipient: "test", body: "look at the diff" },
        undefined,
        undefined,
        worker.context
      );
      expect(result.isError).toBe(true);
      expect(await readFile(log, "utf8").catch(() => "")).toBe("");
      expect(worker.legionTool.description).toContain("envoy_publish");
      expect(worker.legionTool.description).not.toContain("handoff_message");
    });

    test("a phase worker is refused the architect operations", async () => {
      const worker = await bootStalling({});
      await expect(
        worker.legionTool.execute(
          "call-release",
          { op: "release_children", issues: ["REPO-44"] },
          undefined,
          undefined,
          worker.context
        )
      ).resolves.toMatchObject({
        isError: true,
        content: [
          { type: "text", text: "release_children is not available to a phase-worker session" },
        ],
      });
    });

    test("before any assignment a settle is quiet, and an inbound event opens nothing", async () => {
      const worker = await bootStalling({});

      expect(await worker.settles("Booted.")).toBeUndefined();
      await worker.arrives(envoyEvent);
      expect(await worker.settles("Read the notice.")).toBeUndefined();
    });

    test("a settle the host aborted sends nothing and leaves the stall unanswered", async () => {
      const worker = await bootStalling({});
      await worker.arrives(assignment);
      const aborted = new AbortController();
      aborted.abort();

      expect(await worker.settles("Interrupted.", aborted.signal)).toBeUndefined();
      expect(await worker.settles("Done.")).toEqual(followUp("handoff_complete"));
    });

    test("a resumed worker restores its phase from the transcript entries it wrote", async () => {
      const waiting = await bootStalling({});
      await waiting.arrives(assignment);
      await waiting.settles("WAITING: CI");
      // The daemon relaunches the worker with --resume: a fresh process whose only memory is the
      // session's branch. The next turn starts from an Envoy notice, not a new assignment.
      const resumedOpen = await bootStalling({ branch: waiting.entries });
      await resumedOpen.arrives(envoyEvent);
      expect(await resumedOpen.settles("CI passed.")).toEqual(followUp("handoff_complete"));

      const completed = await bootStalling({});
      await completed.arrives(assignment);
      await completed.completes(0);
      const resumedClosed = await bootStalling({ branch: completed.entries });
      await resumedClosed.arrives(envoyEvent);
      expect(await resumedClosed.settles("Answered the question.")).toBeUndefined();
    });

    test("a sub-architect is never nudged", async () => {
      const subArchitect = await bootStalling({ role: "architect", issue: "REPO-43" });
      await subArchitect.arrives(assignment);

      expect(await subArchitect.settles("Waves released.")).toBeUndefined();
    });

    test("a root architect is never nudged", async () => {
      const pane = await bootPane({ role: "architect", issue: "REPO-42", sessionId: "ses_stall" });
      const root = session(pane.handlers, pane.context);
      await root.arrives(assignment);

      expect(await root.settles("Spec posted.")).toBeUndefined();
      const legionTool = pane.tools.find((tool) => tool.name === "legion");
      await expect(
        legionTool?.execute(
          "call-root-complete",
          { op: "handoff_complete", summary: "Done." },
          undefined,
          undefined,
          pane.context
        )
      ).resolves.toMatchObject({
        isError: true,
        content: [
          {
            type: "text",
            text: expect.stringContaining(
              "handoff_complete is not available to a root architect session; a root architect reads handoffs with `legion handoff read"
            ),
          },
        ],
      });
    });

    test("the controller is never nudged", async () => {
      const controller = await launchedController({ sessionId: "ses_controller_nudge" });
      const context = controller.context("ses_controller_nudge");
      await hook(controller.handlers, "session_start")({}, context);
      const pane = session(controller.handlers, context);
      await pane.arrives(assignment);

      expect(await pane.settles("Admitted REPO-43.")).toBeUndefined();
    });

    /** Answers every request as the Envoy listener does, so a session with no daemon never
     * reaches a real listener; the requests show nothing Legion-specific was called. */
    const listenerOnly = (sessionId: string): { readonly path: string }[] => {
      const requests: { readonly path: string }[] = [];
      process.env.ENVOY_URL = "http://envoy.test";
      globalThis.fetch = (async (input) => {
        requests.push({ path: new URL(input.toString()).pathname });
        return Response.json({
          session_id: sessionId,
          machine_id: "machine",
          dir: "/tmp/legion-workspace",
          topics: [],
        });
      }) as typeof fetch;
      return requests;
    };

    test("a session with no Legion environment is never nudged", async () => {
      const requests = listenerOnly("ses_plain");
      const fixture = createPi();
      legionExtension(fixture.pi);
      const context = sessionContext("ses_plain");
      await hook(fixture.handlers, "session_start")({}, context);
      const plain = session(fixture.handlers, context);
      await plain.arrives(assignment);

      expect(await plain.settles("Here is the answer.")).toBeUndefined();
      expect(fixture.entries.filter((entry) => entry.customType.startsWith("legion"))).toEqual([]);
      expect(requests.filter((request) => request.path.startsWith("/legion/"))).toEqual([]);
    });

    test("a task subagent of a phase worker is never nudged", async () => {
      const { childFile } = await createSubagentTranscriptPaths();
      const pane = await claimPane({
        role: "implementer",
        sessionId: "ses_subagent",
        sessionFile: childFile,
      });
      await pane.start();
      const subagent = session(pane.handlers, pane.context);
      await subagent.arrives(assignment);

      expect(await subagent.settles("Found the file.")).toBeUndefined();
      expect(daemonRequests(pane.requests)).toEqual([]);
    });
  });

  test("register_gate names the lookup and the reference when Dispatch cannot be reached", async () => {
    const pane = await bootPane({ role: "architect", issue: "REPO-42", sessionId: "ses_gate" });
    // The lookup reads the pane's Dispatch configuration when it runs, and fails to connect.
    process.env.DISPATCH_URL = "http://dispatch.unreachable.test";
    process.env.DISPATCH_TOKEN = "dispatch-token";
    const paneFetch = globalThis.fetch;
    globalThis.fetch = (async (input, init) => {
      if (new URL(input.toString()).host === "dispatch.unreachable.test") {
        throw new TypeError("Unable to connect. Is the computer able to access the url?");
      }
      return paneFetch(input, init);
    }) as typeof fetch;
    const legion = pane.tools.find((tool) => tool.name === "legion");
    if (legion === undefined) throw new Error("the legion tool was not registered");

    await expect(
      legion.execute(
        "register-gate",
        { op: "register_gate", issue: "REPO-42", artifactId: "spec", version: 2 },
        undefined,
        undefined,
        pane.context
      )
    ).resolves.toMatchObject({
      isError: true,
      content: [
        {
          type: "text",
          text: 'register_gate could not look up document "spec" on REPO-42 in Dispatch: Unable to connect. Is the computer able to access the url?',
        },
      ],
    });
    expect(pane.requests.some((request) => request.path === "/legion/v1/gates/register")).toBe(
      false
    );
  });

  test("subscribes no claim to an issue's notice topic", async () => {
    await bootPane({ role: "architect", sessionId: "ses_notice_architect" });
    await bootPane({ role: "implementer", sessionId: "ses_notice_worker" });

    // The daemon sends every notice to the owning architect's role topic, which the architect
    // claims as its Envoy role; a phase worker subscribed to its issue's topic would be woken by
    // notices meant for the architect.
    const subjects = natsConnections.flatMap((connection) => connection.subjects);
    expect(
      subjects.filter((subject) => subject.startsWith("notifications.legion.omp.REPO-"))
    ).toEqual([]);
  });

  test("mints and atomically replaces a claim's grant for every bash command", async () => {
    const { toolCall, context, grantFile, requests, registration } = await bootPane({
      role: "implementer",
      sessionId: "ses_grant",
    });

    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "grant-one",
          input: { command: "legion gh -- pr view 7" },
        },
        context
      )
    ).resolves.toBeUndefined();
    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "grant-two", input: { command: "legion state" } },
        context
      )
    ).resolves.toBeUndefined();

    expect(await grantFileContents(grantFile)).toEqual({ grant: "grant-2", mode: 0o600 });
    // Each mint names the claim's session and secret, and its tree and issue.
    const grant = {
      path: "/legion/v1/grants",
      body: {
        sessionId: "ses_grant",
        secret: registration.secret,
        tree: "REPO-42",
        issue: "REPO-43",
      },
    };
    expect(grantRequests(requests)).toEqual([grant, grant]);
    // The atomic rename leaves no `<file>.<pid>.<uuid>` residue beside the grant file.
    const secretFiles = await readdir(path.dirname(grantFile));
    expect(secretFiles.filter((name) => name.startsWith(`${path.basename(grantFile)}.`))).toEqual(
      []
    );
  });

  for (const [status, sentence] of [
    [400, 'invalid request body: json: unknown field "pluginVersion"'],
    [403, "Invalid boot token"],
    [404, "no route"],
    [409, "Worker respawn must resume the same agent session"],
  ] as const) {
    test(`a ${status} at claims/register exits the process with one line naming the route, status, and sentence`, async () => {
      const pane = await claimPane({
        role: "implementer",
        sessionId: `ses_refused_${status}`,
        register: () => Response.json({ error: sentence }, { status }),
      });

      await expect(pane.start()).rejects.toThrow("process would exit");
      expect(pane.exits).toEqual([1]);
      expect(pane.errors).toEqual([
        `[legion] claims/register registration failed (${status}): ${sentence}`,
      ]);
      expect(daemonRequests(pane.requests).map((request) => request.path)).toEqual([
        "/legion/v1/claims/register",
      ]);
      expect(pane.requests.map((request) => request.path)).not.toContain("/v1/roles/set");
    });
  }

  for (const status of [500, 503]) {
    test(`a ${status} at claims/register propagates and the process stays for the registration deadline`, async () => {
      const pane = await claimPane({
        role: "implementer",
        sessionId: `ses_daemon_${status}`,
        register: () => Response.json({ error: "register failed" }, { status }),
      });

      await expect(pane.start()).rejects.toThrow(
        `POST /legion/v1/claims/register failed with ${status}: register failed`
      );
      expect(pane.exits).toEqual([]);
      expect(pane.errors).toEqual([
        `[legion] claims/register registration failed (${status}): register failed`,
      ]);
    });
  }

  test("a registration that never reached the daemon propagates and the process stays", async () => {
    const pane = await claimPane({
      role: "implementer",
      sessionId: "ses_unreachable",
      register: () => {
        throw new TypeError("fetch failed");
      },
    });

    await expect(pane.start()).rejects.toThrow("fetch failed");
    expect(pane.exits).toEqual([]);
  });

  test("claims/ready is retried on a 5xx and on a transport failure, three attempts a second apart", async () => {
    const recovered = await claimPane({
      role: "implementer",
      sessionId: "ses_ready_recovers",
      ready: (attempt) => {
        if (attempt === 1) return Response.json({ error: "daemon unavailable" }, { status: 503 });
        if (attempt === 2) throw new TypeError("fetch failed");
        return new Response(null, { status: 204 });
      },
    });
    const started = Date.now();
    await recovered.start();
    expect(Date.now() - started).toBeGreaterThanOrEqual(1_900);
    expect(
      daemonRequests(recovered.requests).filter((r) => r.path === "/legion/v1/claims/ready")
    ).toHaveLength(3);
    expect(recovered.exits).toEqual([]);
  });

  test("claims/ready failing transiently three times ends the boot and exits once", async () => {
    const exhausted = await claimPane({
      role: "implementer",
      sessionId: "ses_ready_exhausted",
      ready: () => Response.json({ error: "daemon unavailable" }, { status: 503 }),
    });
    await expect(exhausted.start()).rejects.toThrow("process would exit");
    expect(
      daemonRequests(exhausted.requests).filter((r) => r.path === "/legion/v1/claims/ready")
    ).toHaveLength(3);
    expect(exhausted.exits).toEqual([1]);
  });

  test("a 4xx at claims/ready is not retried: the boot ends and the process exits once", async () => {
    const pane = await claimPane({
      role: "implementer",
      sessionId: "ses_ready_refused",
      ready: () => Response.json({ error: "Invalid session secret" }, { status: 403 }),
    });

    await expect(pane.start()).rejects.toThrow("process would exit");
    expect(
      daemonRequests(pane.requests).filter((r) => r.path === "/legion/v1/claims/ready")
    ).toHaveLength(1);
    expect(pane.exits).toEqual([1]);
  });

  test("a claim that regains its Envoy role reports ready again", async () => {
    let holder: string | undefined = "ses_regain";
    const secondReady = Promise.withResolvers<void>();
    let readies = 0;
    const pane = await claimPane({
      role: "implementer",
      sessionId: "ses_regain",
      roleHolder: () => holder,
      readyPosted: () => {
        readies += 1;
        if (readies === 2) secondReady.resolve();
      },
    });
    await pane.start();

    holder = "";
    pane.intervals[0]?.();
    await secondReady.promise;

    const ready = {
      path: "/legion/v1/claims/ready",
      body: {
        claimToken: pane.claimToken,
        sessionId: "ses_regain",
        secret: pane.registration.secret,
        generation: pane.registration.generation,
      },
    };
    expect(daemonRequests(pane.requests).filter((request) => request.path === ready.path)).toEqual([
      ready,
      ready,
    ]);
  });
});

/** The session `legion controller start` launches against the daemon: the `LEGION_CONTROLLER`
 * marker, and the controller capability as a 0600 file behind `LEGION_CONTROLLER_SECRET_FILE`
 * (`packages/daemon/cmd/legion/controller.go`). The stub answers the claim registration with
 * the controller's registration (or `register`'s answer, when it gives one), mints grants (or
 * answers `grant`'s refusal), and 404s every other daemon path as the daemon's catch-all does; its
 * Envoy side keeps a role holder and an interest registry, as the listener does, and names no
 * holder while `listener.lost` is set. */
async function launchedController(options: {
  readonly sessionId: string;
  readonly register?: (
    body: Record<string, unknown>
  ) => Response | undefined | Promise<Response | undefined>;
  /** The project token the controller carries as LEGION_PROJECT; `legion controller start`
   * derives it from the configured project spelling. */
  readonly project?: string;
  /** The project the daemon's `GET /legion/v1/state` names, as its operator wrote it: `OMP`,
   * whose token is `omp`, unless a test says otherwise. */
  readonly daemonProject?: string;
  /** Which extension handles a session event first: the manifest's `envoy.ts` unless a test
   * binds `legion.ts` first. */
  readonly order?: "envoy.ts" | "legion.ts";
  /** The daemon's answer to every `/legion/v1/grants`, in place of a minted grant. */
  readonly grant?: () => Response;
}): Promise<{
  readonly token: string;
  readonly registration: Record<string, unknown>;
  readonly grantFile: string;
  readonly requests: DaemonRequest[];
  readonly exits: number[];
  readonly tools: RegisteredTool[];
  readonly commands: RegisteredCommand[];
  readonly title: HostTitle;
  readonly handlers: Map<string, Handler>;
  readonly context: (sessionId: string, sessionFile?: string) => SessionContext;
  /** The listener lost the role's claim: its role read names no holder until the next claim. */
  readonly listener: { lost: boolean };
  readonly holder: () => string;
  /** Settles at the next soft claim the listener grants after the call. */
  readonly nextSoftClaim: () => Promise<void>;
}> {
  const project = options.project ?? "omp";
  const token = `legion-${project}-controller`;
  const registration = {
    claimToken: token,
    role: "controller",
    generation: 2,
    secret: `controller-secret-${options.sessionId}`,
  };
  const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-controller-"));
  const secretsDir = path.join(stateDir, "secrets");
  await mkdir(secretsDir, { mode: 0o700 });
  temporaryPaths.push(stateDir);
  const capabilityFile = path.join(secretsDir, token);
  const grantFile = path.join(secretsDir, `${token}-grant`);
  await writeFile(capabilityFile, "controller-capability\n", { mode: 0o600 });
  process.env.LEGION_CONTROLLER = "1";
  process.env.LEGION_ROLE = "controller";
  process.env.LEGION_CONTROLLER_SECRET_FILE = capabilityFile;
  process.env.LEGION_DAEMON_URL = "http://daemon.test";
  process.env.ENVOY_URL = "http://envoy.test";
  process.env.LEGION_PROJECT = project;
  process.env.LEGION_STATE_DIR = stateDir;
  process.env.LEGION_GRANT_FILE = grantFile;

  const requests: DaemonRequest[] = [];
  const goldenState = JSON.parse(
    await readFile(
      path.join(import.meta.dir, "../../contracts/fixtures/daemon-api/state.json"),
      "utf8"
    )
  );
  const daemonState = {
    ...goldenState,
    daemon: { ...goldenState.daemon, project: options.daemonProject ?? project.toUpperCase() },
  };
  let grants = 0;
  let holder = options.sessionId;
  const listener = { lost: false };
  let softClaim = Promise.withResolvers<void>();
  const interests = new Map<string, Set<string>>();
  globalThis.fetch = (async (input, init) => {
    const url = new URL(input.toString());
    const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
    requests.push({ path: url.pathname, body });
    if (url.pathname === "/legion/v1/claims/register") {
      return (await options.register?.(body)) ?? Response.json(registration);
    }
    if (url.pathname === "/legion/v1/grants") {
      if (options.grant !== undefined) return options.grant();
      grants += 1;
      return Response.json({
        grantId: `controller-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname === "/legion/v1/state") return Response.json(daemonState);
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: "no route" }, { status: 404 });
    }
    // The listener's role registry: a hard claim is last-claim-wins, and a soft claim from any
    // session but the live holder or the holder's declared successor is refused with that holder,
    // unless the listener lost the claim.
    if (url.pathname === "/v1/roles/set") {
      if (
        body?.soft === true &&
        body.session_id !== holder &&
        body.previous_session_id !== holder &&
        !listener.lost
      ) {
        return Response.json(
          { error: `role ${token} is held by ${holder}`, role: token, holder },
          { status: 409 }
        );
      }
      holder = body?.session_id ?? holder;
      listener.lost = false;
      if (body?.soft === true) {
        softClaim.resolve();
        softClaim = Promise.withResolvers<void>();
      }
    }
    if (url.pathname === `/v1/roles/${token}`) {
      if (listener.lost) {
        return Response.json({ error: `no holder for role ${token}` }, { status: 404 });
      }
      return Response.json({ role: token, holder, last_seen: 1 });
    }
    // The listener's interest registry: a registration adds its topics, an unsubscribe removes
    // them, and a read returns what the session holds.
    if (url.pathname === "/v1/interests/subscribe") {
      const topics = interests.get(body.session_id) ?? new Set<string>();
      for (const topic of body.topics ?? []) topics.add(topic);
      interests.set(body.session_id, topics);
    }
    if (url.pathname === "/v1/interests/unsubscribe") {
      for (const topic of body.topics ?? []) interests.get(body.session_id)?.delete(topic);
    }
    const read = /^\/v1\/interests\/(?!subscribe$|unsubscribe$)([^/]+)$/.exec(url.pathname);
    const session = read?.[1] ?? body?.session_id ?? options.sessionId;
    return Response.json({
      session_id: session,
      machine_id: "machine",
      dir: "/tmp/legion-workspace",
      topics: read === null ? [token] : [...(interests.get(session) ?? [])],
    });
  }) as typeof fetch;

  const exits: number[] = [];
  setLegionBootstrapExitForTests((code) => {
    exits.push(code);
    throw new Error("process would exit");
  });
  const fixture = createPi({ bindEnvoy: options.order !== "legion.ts" });
  legionExtension(fixture.pi);
  if (options.order === "legion.ts") envoyExtension(fixture.pi as never);
  return {
    token,
    registration,
    grantFile,
    requests,
    exits,
    tools: fixture.tools,
    commands: fixture.commands,
    title: fixture.title,
    handlers: fixture.handlers,
    context: (sessionId, sessionFile = `/tmp/${sessionId}.jsonl`) =>
      sessionContext(sessionId, sessionFile, undefined, fixture.title),
    listener,
    holder: () => holder,
    nextSoftClaim: () => softClaim.promise,
  };
}

describe("the operator-launched controller (LEGION_CONTROLLER=1)", () => {
  test("reads the daemon's project, registers on the claim route with its capability, then claims the controller role, and asks nothing else", async () => {
    const controller = await launchedController({ sessionId: "ses_controller" });
    await controller.handlers.get("session_start")?.({}, controller.context("ses_controller"));

    expect(daemonRequests(controller.requests)).toEqual([
      { path: "/legion/v1/state", body: undefined },
      {
        path: "/legion/v1/claims/register",
        body: {
          bootToken: "controller-capability",
          sessionId: "ses_controller",
          ompSessionFile: "/tmp/ses_controller.jsonl",
          agentId: "ses_controller",
          pluginContract: pkg.legion.daemonApiVersion,
        },
      },
    ]);
    const paths = controller.requests.map((request) => request.path);
    const roleClaim = controller.requests.findIndex(
      (request) =>
        request.path === "/v1/roles/set" &&
        JSON.stringify(request.body) ===
          JSON.stringify({ session_id: "ses_controller", role: controller.token })
    );
    expect(roleClaim).toBeGreaterThan(paths.indexOf("/legion/v1/claims/register"));
    expect(controller.exits).toEqual([]);
    // The `legion` tool is an architect's and a worker's; the controller does not get it.
    expect(controller.tools.map((tool) => tool.name)).not.toContain("legion");
  });

  test("claims with the capability LEGION_CONTROLLER_SECRET_FILE names over LEGION_CONTROLLER_SECRET, and /legion-claim-controller moves the role to a hand-started session", async () => {
    const controller = await launchedController({ sessionId: "ses_controller_pane" });
    process.env.LEGION_CONTROLLER_SECRET = "decoy";
    await controller.handlers.get("session_start")?.({}, controller.context("ses_controller_pane"));
    // A hand-started takeover session carries no controller marker.
    delete process.env.LEGION_CONTROLLER;
    delete process.env.LEGION_ROLE;
    const claimCommand = controller.commands.find(
      (command) => command.name === "legion-claim-controller"
    );
    if (claimCommand === undefined) throw new Error("controller claim command was not registered");
    await claimCommand.handler("", controller.context("ses_controller_takeover"));

    expect(
      controller.requests
        .filter((request) => request.path === "/legion/v1/claims/register")
        .map((request) => request.body)
    ).toEqual([
      expect.objectContaining({
        bootToken: "controller-capability",
        sessionId: "ses_controller_pane",
      }),
      expect.objectContaining({
        bootToken: "controller-capability",
        sessionId: "ses_controller_takeover",
      }),
    ]);
    expect(controller.holder()).toBe("ses_controller_takeover");
  });

  test("blocks a bash call when the daemon refuses the controller grant", async () => {
    const controller = await launchedController({
      sessionId: "ses_controller_refused",
      grant: () => Response.json({ error: "Invalid controller capability" }, { status: 403 }),
    });
    const context = controller.context("ses_controller_refused");
    await controller.handlers.get("session_start")?.({}, context);

    await expect(
      controller.handlers.get("tool_call")?.(
        {
          toolName: "bash",
          toolCallId: "controller-refused",
          input: { command: "legion state" },
        },
        context
      )
    ).resolves.toEqual({
      block: true,
      reason: "POST /legion/v1/grants failed with 403: Invalid controller capability",
    });
  });

  test("a tree navigation that changes nothing and a task subagent's switch claim nothing; a tree navigation after the transcript moved under the same id claims again with the moved file", async () => {
    const controller = await launchedController({ sessionId: "ses_pane_first" });
    await controller.handlers.get("session_start")?.(
      {},
      controller.context("ses_pane_first", "/tmp/first.jsonl")
    );
    const registrations = () =>
      controller.requests.filter((request) => request.path === "/legion/v1/claims/register");

    // A tree navigation that leaves the session id and transcript as they were claims nothing:
    // every registration replaces the running controller's session and secret.
    await controller.handlers.get("session_tree")?.(
      { newLeafId: "leaf" },
      controller.context("ses_pane_first", "/tmp/first.jsonl")
    );
    // A task-spawned subagent inside the pane loads its own instance of this module with the
    // pane's environment; its switch events must never take the controller role.
    const { childFile } = await createSubagentTranscriptPaths();
    const subagent = createPi();
    legionExtension(subagent.pi);
    await subagent.handlers.get("session_switch")?.(
      { reason: "new" },
      sessionContext("ses_subagent", childFile)
    );
    expect(subagent.title.set).toEqual([]);
    expect(registrations()).toEqual([
      expect.objectContaining({
        body: expect.objectContaining({ ompSessionFile: "/tmp/first.jsonl" }),
      }),
    ]);
    expect(controller.holder()).toBe("ses_pane_first");

    // `!cd <dir>` (or /move) typed into the pane relocates the transcript under the same session
    // id with no session event of its own; the next tree navigation is where it is claimed again.
    await controller.handlers.get("session_tree")?.(
      { newLeafId: "leaf-2" },
      controller.context("ses_pane_first", "/tmp/elsewhere/first.jsonl")
    );
    expect(registrations()).toEqual([
      expect.objectContaining({
        body: expect.objectContaining({ ompSessionFile: "/tmp/first.jsonl" }),
      }),
      expect.objectContaining({
        body: expect.objectContaining({ ompSessionFile: "/tmp/elsewhere/first.jsonl" }),
      }),
    ]);
  });

  // OMP dispatches one session event to every extension's handler; the manifest lists envoy.ts
  // before legion.ts, but nothing in the code may depend on that. The claim legion.ts makes on
  // /new must reach the pane's own envoy instance in either order, because only that instance's
  // heartbeat re-asserts the role (LEGION-29): with last-bound-wins routing the legion-first
  // order would hand the claim to the subagent's instance, whose heartbeat would then see the
  // pane's id as drift and soft-claim the controller role for the subagent's session.
  for (const order of ["envoy.ts", "legion.ts"] as const) {
    test(`after /new in a pane that ran a task subagent, the heartbeat re-asserts the controller for the pane's new session when ${order} handles the switch first`, async () => {
      const controller = await launchedController({ sessionId: "ses_pane_first", order });
      const paneTicks: (() => void)[] = [];
      const pane = controllerPane(paneTicks);
      await controller.handlers.get("session_start")?.({}, pane.context);
      expect(controller.holder()).toBe("ses_pane_first");

      // The pane runs a `task`: a fresh pair of extension instances binds in this process and
      // the subagent's session starts.
      const { childFile } = await createSubagentTranscriptPaths();
      const subagent = createPi();
      legionExtension(subagent.pi);
      const subagentTicks: (() => void)[] = [];
      await subagent.handlers.get("session_start")?.(
        {},
        {
          ...sessionContext("ses_subagent", childFile),
          setInterval: (callback) => subagentTicks.push(callback),
        }
      );

      // Sami types /new into the pane.
      pane.switchTo("ses_pane_second", "/tmp/second.jsonl");
      await controller.handlers.get("session_switch")?.({ reason: "new" }, pane.context);
      expect(controller.holder()).toBe("ses_pane_second");

      // A subagent shares the pane's Envoy identity: its instance registers no session and so
      // runs no heartbeat that could see the pane's new id as drift.
      expect(subagentTicks).toHaveLength(0);

      // The listener later loses sight of the pane (a reaped claim, a listener restart). Only
      // the pane's own heartbeat is registered here, and only a topic its instance holds is
      // re-asserted — so the claim must have reached the pane's instance: it soft-claims for the
      // session the pane holds now.
      controller.listener.lost = true;
      expect(paneTicks).toHaveLength(1);
      const reasserted = controller.nextSoftClaim();
      paneTicks[0]?.();
      await reasserted;
      const softClaims = controller.requests.filter(
        (request) =>
          request.path === "/v1/roles/set" &&
          typeof request.body === "object" &&
          request.body !== null &&
          "soft" in request.body
      );
      expect(softClaims.at(-1)).toEqual({
        path: "/v1/roles/set",
        body: { session_id: "ses_pane_second", role: controller.token, soft: true },
      });
      expect(softClaims.map((request) => JSON.stringify(request.body))).not.toContainEqual(
        expect.stringContaining('"session_id":"ses_subagent"')
      );
      expect(controller.holder()).toBe("ses_pane_second");
    });
  }

  test("mints nothing before its claim, then a grant with its registration secret for every bash command, leaving the input as the model wrote it", async () => {
    const controller = await launchedController({ sessionId: "ses_controller_bash" });
    const context = controller.context("ses_controller_bash");
    const toolCall = controller.handlers.get("tool_call");
    if (toolCall === undefined) throw new Error("tool_call handler was not registered");

    // Before the claim completes nothing can mint for the controller, so its call passes through
    // unwrapped rather than being blocked as an unregistered worker.
    await expect(
      toolCall(
        { toolName: "bash", toolCallId: "controller-unclaimed", input: { command: "true" } },
        context
      )
    ).resolves.toBeUndefined();
    expect(daemonRequests(controller.requests)).toEqual([]);

    await controller.handlers.get("session_start")?.({}, context);
    // The grant travels exactly as a worker's does: written to the pane's grant file, with neither
    // `command` nor `env` rewritten — the hook returns nothing.
    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "controller-env",
          input: {
            command: "legion state",
            env: { KEEP: "model-value", LEGION_GRANT: "made-up-by-the-model" },
          },
        },
        context
      )
    ).resolves.toBeUndefined();
    expect(await grantFileContents(controller.grantFile)).toEqual({
      grant: "controller-grant-1",
      mode: 0o600,
    });
    // LEGION-45: the controller does not commit and is not a phase worker; the operation-log guard
    // never binds it. Its `jj undo` reaches the grant wrapper like any other bash call.
    await expect(
      toolCall(
        {
          toolName: "bash",
          toolCallId: "controller-jj",
          input: { command: "jj -R /tmp/legion-workspace undo" },
        },
        context
      )
    ).resolves.toBeUndefined();
    expect(
      daemonRequests(controller.requests).filter((request) => request.path === "/legion/v1/grants")
    ).toEqual([
      {
        path: "/legion/v1/grants",
        body: { sessionId: "ses_controller_bash", secret: controller.registration.secret },
      },
      {
        path: "/legion/v1/grants",
        body: { sessionId: "ses_controller_bash", secret: controller.registration.secret },
      },
    ]);
  });

  test("subscribes to its project's controller topic", async () => {
    const controller = await launchedController({ sessionId: "ses_controller_notices" });
    await controller.handlers.get("session_start")?.(
      {},
      controller.context("ses_controller_notices")
    );

    expect(natsConnections.flatMap((connection) => connection.subjects)).toContain(
      "notifications.legion.omp.controller"
    );
  });

  test("closes its controller-topic subscription once a later controller takes the role", async () => {
    const topic = "notifications.legion.omp.controller";
    const first = await launchedController({ sessionId: "ses_controller_first" });
    const ticks: (() => void)[] = [];
    const refused = Promise.withResolvers<void>();
    await first.handlers.get("session_start")?.(
      {},
      {
        ...first.context("ses_controller_first"),
        setInterval: (callback) => ticks.push(callback),
        ui: {
          notify: (message) => {
            if (message.includes("this session no longer holds it")) refused.resolve();
          },
        },
      }
    );
    // A later `legion controller start`: a second session registers and takes the role. It is
    // another process, so this one's record of the session it bootstrapped does not apply to it.
    resetLegionBootstrappedSessionForTests();
    const second = createPi();
    legionExtension(second.pi);
    await second.handlers.get("session_start")?.({}, first.context("ses_controller_second"));

    // The first session's next heartbeat finds the role held by the second and is refused.
    for (const tick of ticks) tick();
    await refused.promise;

    const firstConnection = natsConnections.find(
      (candidate) => candidate.name === "omp-ses_controller_first"
    );
    const secondConnection = natsConnections.find(
      (candidate) => candidate.name === "omp-ses_controller_second"
    );
    expect(firstConnection?.unsubscribed).toEqual([topic]);
    expect(secondConnection?.subjects).toContain(topic);
    expect(secondConnection?.unsubscribed).toEqual([]);
  });

  test("a replaced controller that is resumed takes no controller wakes", async () => {
    const topic = "notifications.legion.omp.controller";
    let replaced = false;
    // The daemon refuses the first session's capability once a later start replaced it.
    const first = await launchedController({
      sessionId: "ses_controller_first",
      register: (body) =>
        replaced && body.sessionId === "ses_controller_first"
          ? Response.json({ error: "Invalid boot token" }, { status: 403 })
          : undefined,
    });
    await first.handlers.get("session_start")?.({}, first.context("ses_controller_first"));
    // A later `legion controller start` takes the role, in another process.
    resetLegionBootstrappedSessionForTests();
    const second = createPi();
    legionExtension(second.pi);
    await second.handlers.get("session_start")?.({}, first.context("ses_controller_second"));
    replaced = true;

    // The first session's process died, and the session is resumed within the listener's reap
    // window: its transcript records the role claim, which the listener now refuses it.
    resetLegionBootstrappedSessionForTests();
    // Only the resumed instance connects from here on; the first instance's connections came
    // before this point.
    const connectionsBeforeResume = natsConnections.length;
    const resumed = createPi();
    legionExtension(resumed.pi);
    const context = first.context("ses_controller_first");
    const errorLog = spyOn(console, "error").mockImplementation(() => undefined);
    try {
      await expect(
        resumed.handlers.get("session_start")?.(
          {},
          {
            ...context,
            sessionManager: {
              ...context.sessionManager,
              getBranch: () => [
                { type: "custom", customType: "envoy-role-claim", data: { role: first.token } },
              ],
            },
          }
        )
      ).rejects.toThrow("Invalid boot token");
    } finally {
      errorLog.mockRestore();
    }

    expect(
      first.requests.filter(
        (request) =>
          request.path === "/v1/roles/set" &&
          JSON.stringify(request.body) ===
            JSON.stringify({ session_id: "ses_controller_first", role: first.token, soft: true })
      )
    ).toHaveLength(1);
    const resumedConnections = natsConnections.slice(connectionsBeforeResume);
    expect(resumedConnections.flatMap((connection) => connection.subjects)).toContain(
      "notifications.agent.ses_controller_first"
    );
    expect(resumedConnections.flatMap((connection) => connection.subjects)).not.toContain(topic);
  });

  test("a controller whose role ends while its topic subscription is opening does not keep it", async () => {
    const topic = "notifications.legion.omp.controller";
    const ticks: (() => void)[] = [];
    const refused = Promise.withResolvers<void>();
    const connectGate = Promise.withResolvers<void>();
    const reconnecting = Promise.withResolvers<void>();
    // Registration is where the claim stands when its connection drops: the role claim and the
    // topic's subscription follow, and the subscription waits for the reconnect.
    const first = await launchedController({
      sessionId: "ses_controller_first",
      register: () => {
        const live = natsConnections.find(
          (candidate) => candidate.name === "omp-ses_controller_first"
        );
        if (live !== undefined) live.closed = true;
        natsConnectGates.set("omp-ses_controller_first", () => connectGate.promise);
        reconnecting.resolve();
        return undefined;
      },
    });
    const starting = first.handlers.get("session_start")?.(
      {},
      {
        ...first.context("ses_controller_first"),
        setInterval: (callback) => ticks.push(callback),
        ui: {
          notify: (message) => {
            if (message.includes("this session no longer holds it")) refused.resolve();
          },
        },
      }
    );
    await reconnecting.promise;
    // While the subscription waits, a later `legion controller start` takes the role, and the
    // first session's heartbeat is refused.
    resetLegionBootstrappedSessionForTests();
    const second = createPi();
    legionExtension(second.pi);
    await second.handlers.get("session_start")?.({}, first.context("ses_controller_second"));
    for (const tick of ticks) tick();
    await refused.promise;
    connectGate.resolve();
    await starting;

    const firstSubjects = natsConnections
      .filter((candidate) => candidate.name === "omp-ses_controller_first")
      .flatMap((connection) =>
        connection.subjects.filter((subject) => !connection.unsubscribed.includes(subject))
      );
    expect(firstSubjects).not.toContain(topic);
    expect(
      first.requests.filter(
        (request) =>
          request.path === "/v1/interests/subscribe" && JSON.stringify(request.body).includes(topic)
      )
    ).toEqual([]);
  });

  test("a controller whose role ends while its dropped topic waits to resubscribe does not take it back", async () => {
    const topic = "notifications.legion.omp.controller";
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "1";
    try {
      const first = await launchedController({ sessionId: "ses_controller_first" });
      const ticks: (() => void)[] = [];
      const refused = Promise.withResolvers<void>();
      await first.handlers.get("session_start")?.(
        {},
        {
          ...first.context("ses_controller_first"),
          setInterval: (callback) => ticks.push(callback),
          ui: {
            notify: (message) => {
              if (message.includes("this session no longer holds it")) refused.resolve();
            },
          },
        }
      );
      // The connection dies; each subject's pump retries (the agent subject's and the topic's),
      // and both reconnects wait on the gate.
      const connectGate = Promise.withResolvers<void>();
      const reconnecting = Promise.withResolvers<void>();
      let reconnects = 0;
      natsConnectGates.set("omp-ses_controller_first", () => {
        reconnects += 1;
        if (reconnects === 2) reconnecting.resolve();
        return connectGate.promise;
      });
      natsConnections.find((candidate) => candidate.name === "omp-ses_controller_first")?.drop();
      await reconnecting.promise;
      // Meanwhile a later `legion controller start` takes the role, and the first session's
      // heartbeat is refused.
      resetLegionBootstrappedSessionForTests();
      const second = createPi();
      legionExtension(second.pi);
      await second.handlers.get("session_start")?.({}, first.context("ses_controller_second"));
      for (const tick of ticks) tick();
      await refused.promise;
      connectGate.resolve();
      // Every continuation of the reconnect settles before the next macrotask; then one more
      // heartbeat registers whatever the session holds.
      const settled = Promise.withResolvers<void>();
      setImmediate(settled.resolve);
      await settled.promise;
      for (const tick of ticks) tick();
      const registered = Promise.withResolvers<void>();
      setImmediate(registered.resolve);
      await registered.promise;

      const openSubjects = natsConnections
        .filter((candidate) => candidate.name === "omp-ses_controller_first" && !candidate.closed)
        .flatMap((connection) =>
          connection.subjects.filter((subject) => !connection.unsubscribed.includes(subject))
        );
      expect(openSubjects).toContain("notifications.agent.ses_controller_first");
      expect(openSubjects).not.toContain(topic);
      expect(
        first.requests.filter(
          (request) =>
            request.path === "/v1/interests/subscribe" &&
            JSON.stringify(request.body).includes(topic)
        )
      ).toEqual([]);
    } finally {
      delete process.env.ENVOY_RESUBSCRIBE_DELAY_MS;
    }
  });

  test("a controller whose role ends during a failing reconnect keeps the topic closed through every later retry, and a resume does not get it back", async () => {
    const topic = "notifications.legion.omp.controller";
    process.env.ENVOY_RESUBSCRIBE_DELAY_MS = "1";
    try {
      let replaced = false;
      // The daemon refuses the first session's capability once a later start replaced it.
      const first = await launchedController({
        sessionId: "ses_controller_first",
        register: (body) =>
          replaced && body.sessionId === "ses_controller_first"
            ? Response.json({ error: "Invalid boot token" }, { status: 403 })
            : undefined,
      });
      const ticks: (() => void)[] = [];
      const refused = Promise.withResolvers<void>();
      await first.handlers.get("session_start")?.(
        {},
        {
          ...first.context("ses_controller_first"),
          setInterval: (callback) => ticks.push(callback),
          ui: {
            notify: (message) => {
              if (message.includes("this session no longer holds it")) refused.resolve();
            },
          },
        }
      );
      // The connection dies, and both pumps' retries wait on a reconnect that will fail.
      const failingConnect = Promise.withResolvers<void>();
      const reconnecting = Promise.withResolvers<void>();
      let reconnects = 0;
      natsConnectGates.set("omp-ses_controller_first", () => {
        reconnects += 1;
        if (reconnects === 2) reconnecting.resolve();
        return failingConnect.promise;
      });
      natsConnections.find((candidate) => candidate.name === "omp-ses_controller_first")?.drop();
      await reconnecting.promise;
      // A later `legion controller start` takes the role, and the heartbeat is refused while the
      // reconnect is in flight.
      resetLegionBootstrappedSessionForTests();
      const second = createPi();
      legionExtension(second.pi);
      await second.handlers.get("session_start")?.({}, first.context("ses_controller_second"));
      replaced = true;
      for (const tick of ticks) tick();
      await refused.promise;

      // The reconnect fails, each retry schedules the next on the long interval, and the next
      // reconnect succeeds.
      jest.useFakeTimers();
      const reconnected = Promise.withResolvers<void>();
      natsConnectGates.set("omp-ses_controller_first", async () => reconnected.resolve());
      failingConnect.reject(new Error("nats: connection refused"));
      for (let turn = 0; turn < 50; turn++) await Promise.resolve();
      jest.advanceTimersByTime(60_000);
      jest.useRealTimers();
      await reconnected.promise;
      const settled = Promise.withResolvers<void>();
      setImmediate(settled.resolve);
      await settled.promise;
      for (const tick of ticks) tick();
      const registered = Promise.withResolvers<void>();
      setImmediate(registered.resolve);
      await registered.promise;

      const openSubjects = natsConnections
        .filter((candidate) => candidate.name === "omp-ses_controller_first" && !candidate.closed)
        .flatMap((connection) =>
          connection.subjects.filter((subject) => !connection.unsubscribed.includes(subject))
        );
      expect(openSubjects).toContain("notifications.agent.ses_controller_first");
      expect(openSubjects).not.toContain(topic);
      expect(
        first.requests.filter(
          (request) =>
            request.path === "/v1/interests/subscribe" &&
            JSON.stringify(request.body).includes(topic)
        )
      ).toEqual([]);

      // The replaced session is resumed in a new process: nothing in the registry brings the
      // topic back.
      resetLegionBootstrappedSessionForTests();
      const connectionsBeforeResume = natsConnections.length;
      const resumed = createPi();
      legionExtension(resumed.pi);
      const context = first.context("ses_controller_first");
      const errorLog = spyOn(console, "error").mockImplementation(() => undefined);
      try {
        await expect(
          resumed.handlers.get("session_start")?.(
            {},
            {
              ...context,
              sessionManager: {
                ...context.sessionManager,
                getBranch: () => [
                  { type: "custom", customType: "envoy-role-claim", data: { role: first.token } },
                ],
              },
            }
          )
        ).rejects.toThrow("Invalid boot token");
      } finally {
        errorLog.mockRestore();
      }
      const resumedSubjects = natsConnections
        .slice(connectionsBeforeResume)
        .flatMap((connection) => connection.subjects);
      expect(resumedSubjects).toContain("notifications.agent.ses_controller_first");
      expect(resumedSubjects).not.toContain(topic);
    } finally {
      jest.useRealTimers();
      delete process.env.ENVOY_RESUBSCRIBE_DELAY_MS;
    }
  });

  test("a session switch in the controller registers the new session", async () => {
    const controller = await launchedController({ sessionId: "ses_controller_before" });
    await controller.handlers.get("session_start")?.(
      {},
      controller.context("ses_controller_before")
    );
    await controller.handlers.get("session_switch")?.(
      {},
      controller.context("ses_controller_after", "/tmp/ses_controller_after.jsonl")
    );

    const registrations = controller.requests.filter(
      (request) => request.path === "/legion/v1/claims/register"
    );
    expect(registrations.map((request) => JSON.stringify(request.body))).toEqual([
      expect.stringContaining('"sessionId":"ses_controller_before"'),
      expect.stringContaining('"sessionId":"ses_controller_after"'),
    ]);
    expect(daemonRequests(controller.requests).map((request) => request.path)).toEqual([
      "/legion/v1/state",
      "/legion/v1/claims/register",
      "/legion/v1/state",
      "/legion/v1/claims/register",
    ]);
  });

  // Oh My Pi runs one event's handlers extension by extension in manifest order (envoy.ts, then
  // legion.ts), awaiting each, but moves on to the next extension when a handler outlasts its
  // 30-second budget (`ExtensionRunner.emit` and `EXTENSION_HANDLER_TIMEOUT_MS`,
  // packages/coding-agent/src/extensibility/extensions/runner.ts at the pinned fork release), so a
  // slow rebind can still be running when legion.ts reclaims. Nothing may depend on the order.
  for (const order of ["envoy.ts", "legion.ts"] as const) {
    test(`keeps its controller topic open across /new and /resume when ${order} handles the switch first`, async () => {
      const topic = "notifications.legion.omp.controller";
      const controller = await launchedController({ sessionId: "ses_controller_before", order });
      await controller.handlers.get("session_start")?.(
        {},
        controller.context("ses_controller_before")
      );
      // A subject is open while it was subscribed more often than unsubscribed on a live connection.
      const openSubjects = () =>
        natsConnections
          .filter((connection) => !connection.closed)
          .flatMap((connection) =>
            [...new Set(connection.subjects)].filter(
              (subject) =>
                connection.subjects.filter((candidate) => candidate === subject).length >
                connection.unsubscribed.filter((candidate) => candidate === subject).length
            )
          );
      expect(openSubjects()).toContain(topic);

      for (const [reason, sessionId] of [
        ["new", "ses_controller_new"],
        ["resume", "ses_controller_resumed"],
      ] as const) {
        await controller.handlers.get("session_switch")?.(
          { reason },
          controller.context(sessionId, `/tmp/${sessionId}.jsonl`)
        );
        expect(openSubjects()).toContain(`notifications.agent.${sessionId}`);
        expect(openSubjects()).toContain(topic);
      }
    });
  }

  test("refuses before it registers when LEGION_PROJECT is not the daemon's project", async () => {
    // Registering replaces the running controller's session and secret, so a claim for another
    // project must stop before it.
    const controller = await launchedController({ sessionId: "ses_controller_other_project" });
    process.env.LEGION_PROJECT = "demo";

    await expect(
      controller.handlers.get("session_start")?.(
        {},
        controller.context("ses_controller_other_project")
      )
    ).rejects.toThrow(
      "LEGION_PROJECT demo names controller role legion-demo-controller, but the daemon at http://daemon.test serves project OMP, whose controller role is legion-omp-controller"
    );
    expect(controller.requests.map((request) => request.path)).not.toContain(
      "/legion/v1/claims/register"
    );
    expect(controller.requests.map((request) => request.path)).not.toContain("/v1/roles/set");
    expect(natsConnections.flatMap((connection) => connection.subjects)).not.toContain(
      "notifications.legion.demo.controller"
    );
  });

  test("refuses before claiming the role when the registration names another controller role", async () => {
    const controller = await launchedController({
      sessionId: "ses_controller_other_role",
      daemonProject: "demo",
    });
    process.env.LEGION_PROJECT = "demo";

    await expect(
      controller.handlers.get("session_start")?.(
        {},
        controller.context("ses_controller_other_role")
      )
    ).rejects.toThrow(
      "LEGION_PROJECT demo names controller role legion-demo-controller, but the daemon registered this controller as legion-omp-controller"
    );
    expect(controller.requests.map((request) => request.path)).not.toContain("/v1/roles/set");
    expect(natsConnections.flatMap((connection) => connection.subjects)).not.toContain(
      "notifications.legion.demo.controller"
    );
  });

  test("a controller without LEGION_PROJECT refuses before it registers", async () => {
    // Registering replaces the running controller's session and secret, so a claim that cannot
    // name its topic must stop before it.
    const controller = await launchedController({ sessionId: "ses_controller_no_project" });
    delete process.env.LEGION_PROJECT;

    await expect(
      controller.handlers.get("session_start")?.(
        {},
        controller.context("ses_controller_no_project")
      )
    ).rejects.toThrow("LEGION_PROJECT is required for Legion");
    expect(controller.requests.map((request) => request.path)).not.toContain(
      "/legion/v1/claims/register"
    );
  });

  test("a refused capability is logged and propagates, and the operator's session is not ended", async () => {
    const controller = await launchedController({
      sessionId: "ses_controller_replaced",
      register: () => Response.json({ error: "Invalid boot token" }, { status: 403 }),
    });

    const errors: string[] = [];
    const errorLog = spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      errors.push(args.map(String).join(" "));
    });
    try {
      await expect(
        controller.handlers.get("session_start")?.(
          {},
          controller.context("ses_controller_replaced")
        )
      ).rejects.toThrow("Invalid boot token");
    } finally {
      errorLog.mockRestore();
    }
    expect(controller.exits).toEqual([]);
    expect(errors).toContain(
      "[legion] claims/register for the controller failed (403): Invalid boot token"
    );
    expect(controller.requests.map((request) => request.path)).not.toContain("/v1/roles/set");
  });
});

/** The Envoy registration a role claim sends: the last `/v1/interests/subscribe` before the
 * `/v1/roles/set` claiming `role` (envoy.ts registers the session, then claims). The listener lists
 * a session under the title its latest registration carried. */
function registrationBeforeClaim(
  requests: readonly { readonly path: string; readonly body: unknown }[],
  role: string
): unknown {
  const claim = requests.findIndex(
    ({ path, body }) =>
      path === "/v1/roles/set" &&
      typeof body === "object" &&
      body !== null &&
      "role" in body &&
      body.role === role
  );
  if (claim === -1) throw new Error(`no role claim for ${role}`);
  const registration = requests
    .slice(0, claim)
    .findLast((request) => request.path === "/v1/interests/subscribe");
  if (registration === undefined) throw new Error(`no registration before the claim of ${role}`);
  return registration.body;
}

describe("a Legion session's title", () => {
  test("a phase worker is titled by its role and issue before its role claim registers it with Envoy", async () => {
    const worker = await bootPane({ role: "implementer" });

    expect(worker.title.set).toEqual(["Legion implementer · REPO-43"]);
    expect(registrationBeforeClaim(worker.requests, worker.claimToken)).toMatchObject({
      title: "Legion implementer · REPO-43",
    });
  });

  test("a root architect is titled by its tree's issue", async () => {
    const pane = await bootPane({
      role: "architect",
      issue: "REPO-42",
      sessionId: "ses_root_title",
    });

    expect(pane.title.set).toEqual(["Legion architect · REPO-42"]);
    expect(registrationBeforeClaim(pane.requests, pane.claimToken)).toMatchObject({
      title: "Legion architect · REPO-42",
    });
  });

  test("a controller titles its lowercased project token with the canonical project spelling", async () => {
    // `legion controller start` gives the plugin LEGION_PROJECT=acme from controller.yaml's
    // project: ACME; Dispatch and Envoy must show the project as ACME.
    const controller = await launchedController({
      sessionId: "ses_acme_controller_title",
      project: "acme",
    });
    await controller.handlers.get("session_start")?.(
      {},
      controller.context("ses_acme_controller_title")
    );

    expect(controller.title.set).toEqual(["Legion controller · ACME"]);
    expect(registrationBeforeClaim(controller.requests, controller.token)).toMatchObject({
      title: "Legion controller · ACME",
    });
  });

  test("the controller titles the session a /new leaves it on", async () => {
    const controller = await launchedController({ sessionId: "ses_controller_title" });
    await controller.handlers.get("session_start")?.(
      {},
      controller.context("ses_controller_title")
    );

    expect(controller.title.set).toEqual(["Legion controller · OMP"]);
    expect(registrationBeforeClaim(controller.requests, controller.token)).toMatchObject({
      title: "Legion controller · OMP",
    });

    // `/new` clears the session manager's title in place and moves it to a fresh session.
    delete controller.title.name;
    delete controller.title.source;
    controller.requests.splice(0);
    await controller.handlers.get("session_switch")?.(
      { reason: "new" },
      controller.context("ses_controller_title_new")
    );

    expect(controller.title.set).toEqual(["Legion controller · OMP", "Legion controller · OMP"]);
    expect(registrationBeforeClaim(controller.requests, controller.token)).toMatchObject({
      session_id: "ses_controller_title_new",
      title: "Legion controller · OMP",
    });
  });

  test("a title a person chose is kept, and Envoy lists the session under it", async () => {
    const worker = await bootPane({
      role: "reviewer",
      title: { name: "Reviewing the flaky e2e", source: "user" },
    });

    expect(worker.title.set).toEqual([]);
    expect(registrationBeforeClaim(worker.requests, worker.claimToken)).toMatchObject({
      title: "Reviewing the flaky e2e",
    });
  });

  test("a title Oh My Pi generated from the first message gives way to the Legion title", async () => {
    const controller = await launchedController({ sessionId: "ses_controller_auto_title" });
    controller.title.name = "Legion Controller Start Procedure";
    controller.title.source = "auto";
    await controller.handlers.get("session_start")?.(
      {},
      controller.context("ses_controller_auto_title")
    );

    expect(controller.title.set).toEqual(["Legion controller · OMP"]);
    expect(registrationBeforeClaim(controller.requests, controller.token)).toMatchObject({
      title: "Legion controller · OMP",
    });
  });
});
