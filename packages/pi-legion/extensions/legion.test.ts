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
import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { type IssueKey, type LegionRole, roleToken } from "@legion/contracts";
import { readSessionTitle, sessionDirectory } from "@legion/envoy-client/dispatch-session-state";
import { noteInjectedUserTurn } from "@legion/pi-shared/injected-user-turns";
import {
  ENVOY_PLUGIN_INTERFACE_KEY,
  ENVOY_PLUGIN_INTERFACE_VERSION,
  type EnvoyPluginInterface,
  envoyPluginInterface,
  LEGACY_LEGION_LOADED_KEY,
  LOCAL_ENVOY_NOTICE,
  resetEnvoyPluginInterfaceForTests,
} from "@legion/pi-shared/interface";
import type {
  CommandContext,
  PiApi,
  RegisteredTool,
  SessionContext,
  ZodNumberProperty,
} from "@legion/pi-shared/pi-types";
import { hostAgentRegistryMock, testAgentRoster } from "@legion/pi-shared/test/host-registry";
import { logger } from "@oh-my-pi/pi-utils";
import pkg from "../package.json";
import { classifySession } from "../src/classify";

const natsConnections: {
  readonly name: string;
  readonly subjects: string[];
  readonly unsubscribed: string[];
  closed: boolean;
  /** The connection dies: every subscription's iterator ends, as nats.js ends them. */
  readonly drop: () => void;
}[] = [];
/** One ordered log of two kinds of event a test cares about seeing in order: `sendUserMessage`
 * (createPi's stub pushes here too) and `subscribe:<subject>` (below). Reset in beforeEach. */
const eventOrder: string[] = [];
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
        eventOrder.push(`subscribe:${subject}`);
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

mock.module("@oh-my-pi/pi-coding-agent", () => ({
  copyToClipboard: async () => undefined,
  ...hostAgentRegistryMock,
}));

// The extension modules must load after their OMP and NATS host dependencies are mocked. The
// Envoy entry is the sibling plugin's, reached by path: these panes run both, as a Legion pane does.
const { default: envoyExtension } = await import("../../pi-envoy/extensions/envoy");
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
  "LEGION_CONTROLLER_START_MESSAGE",
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
  "LEGION_WORKSPACE_RECREATED",
  "DISPATCH_STATE_DIR",
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

beforeEach(async () => {
  // A suite run from inside a Legion pane inherits that pane's LEGION_*/DISPATCH_* launch
  // environment; every test starts from none and sets only what it declares. The `dispatch`
  // command's state (the session title file `titleSession` writes) goes to a scratch directory.
  for (const key of environmentKeys) delete process.env[key];
  const dispatchState = await mkdtemp(path.join(os.tmpdir(), "legion-dispatch-state-"));
  temporaryPaths.push(dispatchState);
  process.env.DISPATCH_STATE_DIR = dispatchState;
});

afterEach(async () => {
  globalThis.fetch = originalFetch;
  // OMP's `session_shutdown` removes each envoy instance from the process-wide interface's
  // role-claim bridge; this suite binds a fixture per test and never shuts it down, so it clears
  // the interface itself — otherwise a stale instance still serving a reused session id (its
  // client bound to a previous test's fetch stub) would capture a later test's claim, and one
  // test's bootstrapped session would make the next test's transcript look like a subagent's.
  resetEnvoyPluginInterfaceForTests();
  natsConnections.splice(0);
  eventOrder.length = 0;
  natsConnectGates.clear();
  setLegionBootstrapExitForTests((code) => process.exit(code) as never);
  delete (globalThis as Record<symbol, unknown>)[LEGACY_LEGION_LOADED_KEY];
  testAgentRoster().splice(0);
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
  /** Every `sendMessage` call's options, in the order of sentMessages. */
  readonly sentMessageOptions: unknown[];
  readonly sentUserMessages: string[];
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
  const sentMessageOptions: unknown[] = [];
  const sentUserMessages: string[] = [];
  const entries: AppendedEntry[] = [];
  const title: HostTitle = { set: [] };
  const activeTools = ["read", "task", "wait"];
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
    sendMessage: (message, sendOptions) => {
      sentMessages.push(message);
      sentMessageOptions.push(sendOptions);
    },
    sendUserMessage: (content) => {
      if (typeof content !== "string") {
        throw new Error(
          "createPi's sendUserMessage stub tracks string content only; no legion.test.ts fixture call sends ContentBlock[] (images)"
        );
      }
      sentUserMessages.push(content);
      eventOrder.push("sendUserMessage");
    },
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
  return {
    commands,
    handlers,
    tools,
    sentMessages,
    sentMessageOptions,
    sentUserMessages,
    entries,
    activeTools,
    title,
    pi,
  };
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
  /** Every request the pane made, daemon and Envoy listener alike, in order. */
  readonly requests: DaemonRequest[];
  readonly exits: number[];
  /** What `console.error` printed while `start()` ran. */
  readonly errors: string[];
  readonly intervals: (() => void)[];
  readonly tools: RegisteredTool[];
  readonly commands: RegisteredCommand[];
  readonly activeTools: string[];
  readonly entries: AppendedEntry[];
  readonly sentMessages: SentMessage[];
  readonly sentMessageOptions: unknown[];
  readonly title: HostTitle;
  readonly handlers: Map<string, Handler>;
  readonly context: SessionContext;
  /** Runs `session_start` for the pane's session, as Oh My Pi does when the process starts. */
  readonly start: () => Promise<unknown>;
}

/** A pane the daemon launched for a claim (a root architect, or a phase worker): the identity
 * variables the tmux runtime sets (`packages/daemon/internal/runtime/tmux/spawn.go`'s
 * `panePairs`) and the boot token as a 0600 file behind `LEGION_BOOT_TOKEN_FILE`, under a state
 * directory of its own. The stub answers `extraRoutes` first, then the daemon's claim routes
 * (`register` and `ready` override their answers) and grants (`grant-<n>`), and 404s every other
 * daemon path exactly as the daemon's catch-all does, so a route the extension reached that the
 * daemon does not serve is visible in `requests` and never mistaken for a success. Each pane is an
 * Oh My Pi process of its own, so nothing an earlier pane recorded process-wide (its bootstrapped
 * session, its Envoy role bridge) carries over. `branch` is what the session's `getBranch()`
 * returns: a resumed session's transcript entries. `title` is the title the session already
 * carries when it starts. `bindEnvoy: false` loads legion.ts alone (`createPi`'s option). Nothing
 * runs until `start()`; `bootPane` is the started pane. */
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
  resetEnvoyPluginInterfaceForTests();
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
  if (sessionStart === undefined) throw new Error("the pane's session_start was not registered");
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
    requests,
    exits,
    errors,
    intervals,
    tools: fixture.tools,
    commands: fixture.commands,
    activeTools: fixture.activeTools,
    entries: fixture.entries,
    sentMessages: fixture.sentMessages,
    sentMessageOptions: fixture.sentMessageOptions,
    title: fixture.title,
    handlers: fixture.handlers,
    context,
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
  describe("the Envoy plugin the Legion entry claims through", () => {
    const store = globalThis as typeof globalThis & { [key: symbol]: unknown };
    /** `logger.error` lines while `run` ran, as the daemon's boot log would carry them. `run` is
     * usually an `expect(…).rejects` assertion, which bun's types declare `void` though it is
     * awaited. */
    const errorsLogged = async (run: () => unknown): Promise<string[]> => {
      const lines: string[] = [];
      const stopSink = logger.registerLogSink((entry) => {
        if (entry.level === "error") lines.push(entry.message);
      });
      try {
        await run();
      } finally {
        stopSink();
      }
      return lines;
    };
    /** The interface an Envoy entry built at `version` would have published from `from`. */
    const foreignInterface = (version: number, from: string): EnvoyPluginInterface => ({
      version,
      publishers: [from],
      roleClaim: { instances: [], managedSessions: new Set(), regained: undefined },
      injectedUserTurns: new Map(),
      bootstrappedSession: { file: undefined },
    });

    test("a worker whose process has no Envoy plugin logs one line naming @sjawhar/pi-envoy, the version it needs and none, exits, and claims nothing", async () => {
      // legion.ts alone: no Envoy entry published the interface.
      const pane = await claimPane({ role: "implementer", bindEnvoy: false });

      const lines = await errorsLogged(() =>
        expect(pane.start()).rejects.toThrow("process would exit")
      );

      expect(pane.exits).toEqual([1]);
      expect(lines).toHaveLength(1);
      expect(lines[0]).toContain("@sjawhar/pi-envoy");
      expect(lines[0]).toContain(`interface version ${ENVOY_PLUGIN_INTERFACE_VERSION}`);
      expect(lines[0]).toContain("found none");
      expect(daemonRequests(pane.requests)).toEqual([]);
      expect(pane.tools.find((tool) => tool.name === "legion")).toBeUndefined();
    });

    test("a worker beside an Envoy plugin at another interface version logs one line naming both versions and exits", async () => {
      const pane = await claimPane({ role: "implementer", bindEnvoy: false });
      const other = ENVOY_PLUGIN_INTERFACE_VERSION + 1;
      store[ENVOY_PLUGIN_INTERFACE_KEY] = foreignInterface(
        other,
        "file:///plugins/pi-envoy-next/dist/envoy.js"
      );

      const lines = await errorsLogged(() =>
        expect(pane.start()).rejects.toThrow("process would exit")
      );

      expect(pane.exits).toEqual([1]);
      expect(lines).toHaveLength(1);
      expect(lines[0]).toContain("@sjawhar/pi-envoy");
      expect(lines[0]).toContain(`interface version ${ENVOY_PLUGIN_INTERFACE_VERSION}`);
      expect(lines[0]).toContain(`found version ${other}`);
      expect(lines[0]).toContain("file:///plugins/pi-envoy-next/dist/envoy.js");
      expect(daemonRequests(pane.requests)).toEqual([]);
    });

    test("a worker beside the pre-split @sjawhar/pi-legion-envoy logs one line naming both entries and the uninstall, and exits", async () => {
      const pane = await claimPane({ role: "implementer" });
      store[LEGACY_LEGION_LOADED_KEY] = "file:///plugins/pi-legion-envoy/dist/legion.js";

      const lines = await errorsLogged(() =>
        expect(pane.start()).rejects.toThrow("process would exit")
      );

      expect(pane.exits).toEqual([1]);
      expect(lines).toHaveLength(1);
      expect(lines[0]).toContain("file:///plugins/pi-legion-envoy/dist/legion.js");
      expect(lines[0]).toContain("uninstall @sjawhar/pi-legion-envoy");
      expect(lines[0]).toContain(import.meta.dir);
      expect(daemonRequests(pane.requests)).toEqual([]);
    });

    test("the launched controller is checked too", async () => {
      const controller = await launchedController({ sessionId: "ses_controller_alone" });
      // The Envoy entry's publish is gone, as in a process that never loaded it.
      resetEnvoyPluginInterfaceForTests();

      const lines = await errorsLogged(() =>
        expect(
          controller.handlers.get("session_start")?.({}, controller.context("ses_controller_alone"))
        ).rejects.toThrow("process would exit")
      );

      expect(controller.exits).toEqual([1]);
      expect(lines).toHaveLength(1);
      expect(lines[0]).toContain("@sjawhar/pi-envoy");
      expect(daemonRequests(controller.requests)).toEqual([]);
    });

    test("a person's own session is untouched: no Legion environment, no check, no exit", async () => {
      const fixture = createPi({ bindEnvoy: false });
      const exits: number[] = [];
      setLegionBootstrapExitForTests((code) => {
        exits.push(code);
        throw new Error("process would exit");
      });
      legionExtension(fixture.pi);

      const lines = await errorsLogged(async () => {
        await expect(
          fixture.handlers.get("session_start")?.({}, sessionContext("ses_person"))
        ).resolves.toBeUndefined();
      });

      expect(exits).toEqual([]);
      expect(lines).toEqual([]);
    });

    test("/legion-claim-controller without the Envoy plugin answers the sentence, claims nothing, and never exits", async () => {
      process.env.ENVOY_URL = "http://envoy.test";
      process.env.LEGION_DAEMON_URL = "http://daemon.test";
      process.env.LEGION_PROJECT = "omp";
      process.env.LEGION_CONTROLLER_SECRET = "controller-capability";
      const requests: string[] = [];
      globalThis.fetch = (async (input) => {
        requests.push(new URL(input.toString()).pathname);
        return Response.json({});
      }) as typeof fetch;
      const fixture = createPi({ bindEnvoy: false });
      const exits: number[] = [];
      setLegionBootstrapExitForTests((code) => {
        exits.push(code);
        throw new Error("process would exit");
      });
      legionExtension(fixture.pi);
      const claimCommand = fixture.commands.find(
        (command) => command.name === "legion-claim-controller"
      );
      if (claimCommand === undefined)
        throw new Error("controller claim command was not registered");

      await expect(claimCommand.handler("", sessionContext("ses_interactive"))).rejects.toThrow(
        `needs @sjawhar/pi-envoy at interface version ${ENVOY_PLUGIN_INTERFACE_VERSION}, found none`
      );

      expect(exits).toEqual([]);
      expect(requests).toEqual([]);
    });
  });
  // A root architect's or phase worker's pane carries its claim's boot token, which is not the
  // controller's. `/legion-claim-controller` run there is a takeover by hand, which needs the
  // operator's capability: it stops before any daemon call, so it never registers the worker's own
  // token (whose re-registration would replace its claim's capability) and never exits the worker.
  test("/legion-claim-controller in a phase worker's pane calls no daemon route and does not exit", async () => {
    const goldenState = JSON.parse(
      await readFile(
        path.join(import.meta.dir, "../../contracts/fixtures/daemon-api/state.json"),
        "utf8"
      )
    );
    const pane = await bootPane({
      role: "implementer",
      sessionId: "ses_worker",
      // The daemon serves its state for the pane's project, so a claim that went on would reach
      // claims/register.
      extraRoutes: (url) =>
        url.pathname === "/legion/v1/state"
          ? Response.json({ ...goldenState, daemon: { ...goldenState.daemon, project: "OMP" } })
          : undefined,
    });
    expect(pane.exits).toEqual([]);
    const booted = pane.requests.length;
    const claimCommand = pane.commands.find(
      (command) => command.name === "legion-claim-controller"
    );
    if (claimCommand === undefined) throw new Error("controller claim command was not registered");

    const refusal = await claimCommand.handler("", pane.context).then(
      () => undefined,
      (error: unknown) => error
    );
    expect(daemonRequests(pane.requests.slice(booted))).toEqual([]);
    expect(pane.exits).toEqual([]);
    expect(String(refusal)).toContain(
      "LEGION_CONTROLLER_SECRET or LEGION_CONTROLLER_SECRET_FILE is required to claim the controller."
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
    if (subagentStart === undefined) throw new Error("subagent handlers were not registered");
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
  /** A hook's outcome: a rejected hook is a failed session start or navigation. */
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
  const hook = (handlers: Map<string, Handler>, name: string): Handler => {
    const found = handlers.get(name);
    if (found === undefined) throw new Error(`the ${name} handler was not registered`);
    return found;
  };
  test("a subagent whose first transcript publish loses the session lock is still recognised at the next hook", async () => {
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
    // A session navigation is the next hook that asks the subagent check (`afterSessionChange`).
    const navigates = () => hook(pane.handlers, "session_tree")({}, pane.context);
    const warnings: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn") warnings.push(JSON.stringify(entry));
    });
    let outcomes: HookOutcome[];
    try {
      outcomes = await hookOutcomes([() => pane.start(), navigates, navigates]);
    } finally {
      stopSink();
    }

    // Every hook answers as a subagent's: no claim, no daemon route, no exit, no tool.
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

    // The start is the one hook asked; a later navigation would be answered the same way.
    const outcomes = await hookOutcomes([() => pane.start()]);

    expect(outcomes).toEqual([{ value: undefined }]);
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
  test("registers no tool_call hook: nothing a pane runs is refused or minted for, in any role", async () => {
    // No pane rule and no grant mint (LEGION-631): the `legion` tool's own operations mint their
    // grants in-process, and the role prompts alone say who runs what. legion.ts is loaded alone
    // (`bindEnvoy: false`): envoy.ts registers a `tool_call` hook of its own, which writes the
    // session's title for the `dispatch` command, and the fixture's handlers are shared.
    const worker = await claimPane({
      role: "implementer",
      sessionId: "ses_implementer_no_hook",
      bindEnvoy: false,
    });
    expect(worker.handlers.get("tool_call")).toBeUndefined();
    const root = await claimPane({
      role: "architect",
      issue: "REPO-42",
      sessionId: "ses_root_no_hook",
      bindEnvoy: false,
    });
    expect(root.handlers.get("tool_call")).toBeUndefined();
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
    /** The session's hooks, driven as OMP drives them. */
    const session = (handlers: Map<string, Handler>, context: SessionContext) => ({
      arrives: (message: unknown) => hook(handlers, "message_start")(message, context),
      settles: (text: string, signal?: AbortSignal) =>
        hook(handlers, "session_stop")(settlingOn(text, signal), context),
    });
    /** jj's root commit: `@-` of a workspace fresh from `jj git init`, which is what a phase that
     * writes no handoff (retro, here) reports as the commit it stands on. */
    const ROOT_COMMIT = "0000000000000000000000000000000000000000";
    const bootStalling = async (options: {
      readonly role?: LegionRole;
      readonly issue?: IssueKey;
      readonly branch?: readonly unknown[];
      /** The daemon's answer to `POST /legion/v1/handoff/complete`: success unless given. */
      readonly complete?: () => Response;
    }) => {
      // The pane's workspace is a jj repository, as an issue workspace is: `handoff_complete`
      // finds the commit it reports there with PATH's jj. REPO-43 stands at retro, a phase that
      // writes no handoff, so the commit is the one the workspace stands on.
      const workspace = await createJjWorkspace();
      const state = {
        daemon: {
          project: "OMP",
          schemaVersion: 1,
          boots: 1,
          firstBootAt: "2026-09-18T14:03:27Z",
          startedAt: "2026-09-22T09:15:02Z",
        },
        admission: { cap: 1, active: ["REPO-42"], waiting: [] },
        issues: {
          "REPO-43": {
            key: "REPO-43",
            generation: 2,
            phase: "retro",
            status: "in_progress",
            workers: {},
          },
        },
        pendingStatusWrites: [],
        capabilities: [],
      };
      const booted = await bootPane({
        role: options.role ?? "implementer",
        issue: options.issue,
        sessionId: "ses_stall",
        workspace,
        branch: options.branch,
        extraRoutes: (url) => {
          if (url.pathname === "/legion/v1/state") return Response.json(state);
          if (url.pathname === "/legion/v1/grants") {
            return Response.json({ grantId: "grant-stall", expiresAt: "2099-01-01T00:00:00Z" });
          }
          if (url.pathname === "/legion/v1/handoff/complete") {
            return options.complete?.() ?? Response.json({});
          }
          return undefined;
        },
      });
      const legionTool = booted.tools.find((tool) => tool.name === "legion");
      if (legionTool === undefined) throw new Error("the worker has no legion tool");
      return {
        ...booted,
        ...session(booted.handlers, booted.context),
        legionTool,
        /** Calls the tool's `handoff_complete`: its result, and the daemon requests it made. */
        completes: async () => {
          const before = booted.requests.length;
          const result = await legionTool.execute(
            "call-complete",
            { op: "handoff_complete", summary: "Done." },
            undefined,
            undefined,
            booted.context
          );
          return { result, calls: daemonRequests(booted.requests.slice(before)) };
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
      // final completion came back as text, not a tool call.
      const result = await worker.settles(
        'court\n<invoke name="legion">\n<parameter name="op">handoff_complete</parameter>\n<parameter name="summary">Round 2 done.</parameter>\n</invoke>'
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

    test("a successful handoff_complete posts the completion with a fresh grant and the commit found in the pane, and closes the phase; an inbound event does not reopen it, the next assignment does", async () => {
      const worker = await bootStalling({});
      await worker.arrives(assignment);

      const { result, calls } = await worker.completes();
      expect(result).toEqual({ content: [{ type: "text", text: "{}" }], details: {} });
      // The issue's phase is read from the daemon's state, the commit is found in the pane's
      // workspace with its jj, and the grant is minted in-process and travels only in the request;
      // nothing is written to the pane.
      expect(calls).toEqual([
        { path: "/legion/v1/state", body: undefined },
        {
          path: "/legion/v1/grants",
          body: {
            sessionId: "ses_stall",
            secret: worker.registration.secret,
            tree: "REPO-42",
            issue: "REPO-43",
          },
        },
        {
          path: "/legion/v1/handoff/complete",
          body: {
            grantId: "grant-stall",
            summary: "Done.",
            verdict: "",
            ready: false,
            commit: ROOT_COMMIT,
          },
        },
      ]);
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

    test("a handoff_complete the daemon refuses is an error result naming its code and leaves the phase open", async () => {
      const worker = await bootStalling({
        complete: () =>
          Response.json(
            {
              error: `the implementer reported commit ${ROOT_COMMIT} for its previous phase of REPO-43; write and commit this phase's handoff before completing`,
              code: "HANDOFF_NOT_NEW",
            },
            { status: 409 }
          ),
      });
      await worker.arrives(assignment);

      const { result, calls } = await worker.completes();
      expect(result).toMatchObject({
        content: [
          {
            type: "text",
            text: expect.stringContaining(
              `POST /legion/v1/handoff/complete failed with 409 HANDOFF_NOT_NEW: the implementer reported commit ${ROOT_COMMIT}`
            ),
          },
        ],
        isError: true,
      });
      expect(calls.map((call) => call.path)).toEqual([
        "/legion/v1/state",
        "/legion/v1/grants",
        "/legion/v1/handoff/complete",
      ]);
      expect(await worker.settles("Reported.")).toEqual(followUp("handoff_complete"));
    });

    test("the Envoy extension's own notice (its session-id-changed notice) does not re-arm a quiet stall", async () => {
      const worker = await bootStalling({});
      await worker.arrives(assignment);
      expect(await worker.settles("Done, I think.")).toEqual(followUp("handoff_complete"));

      // A branch re-mints the worker's session id, and the Envoy extension steers its notice into
      // the session: the session's own doing, not an event from outside.
      await worker.arrives({
        message: {
          ...envoyEvent.message,
          content: "envoy:\n  notice: session id changed",
          details: LOCAL_ENVOY_NOTICE,
        },
      });
      expect(await worker.settles("Noted the new session id.")).toBeUndefined();

      await worker.arrives(envoyEvent);
      expect(await worker.settles("Read the answer.")).toEqual(followUp("handoff_complete"));
    });

    test("the legion tool has no handoff_message: a call reaches no daemon route, and a message for another role goes through envoy_publish", async () => {
      // Nothing reads a handoff message: no operation or prompt reads `.legion/messages/`, while a
      // role's live questions go through envoy_publish and a later phase reads the handoff files.
      const worker = await bootStalling({});
      const requestsBefore = worker.requests.length;
      const result = await worker.legionTool.execute(
        "call-message",
        { op: "handoff_message", sender: "implement", recipient: "test", body: "look at the diff" },
        undefined,
        undefined,
        worker.context
      );
      expect(result.isError).toBe(true);
      expect(daemonRequests(worker.requests.slice(requestsBefore))).toEqual([]);
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
      await completed.completes();
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
            text: "handoff_complete is not available to a root architect session, which runs no phase",
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
 * answers `grant`'s refusal), takes an issue-status write, and 404s every other daemon path as
 * the daemon's catch-all does; its Envoy side keeps a role holder and an interest registry, as
 * the listener does, and names no holder while `listener.lost` is set. */
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
  /** LEGION_CONTROLLER_START_MESSAGE, the text `legion controller start` carries for the
   * extension to send as the session's first turn; a fixed literal unless a test says otherwise. */
  readonly startMessage?: string;
  /** The Go-written state document the daemon's `GET /legion/v1/state` answers with, its project
   * replaced by `daemonProject`: `state.json` unless a test names another. */
  readonly stateFixture?: string;
}): Promise<{
  readonly token: string;
  readonly registration: Record<string, unknown>;
  /** The 0700 secrets directory the controller's capability file sits in. */
  readonly secretsDir: string;
  readonly requests: DaemonRequest[];
  readonly sentUserMessages: string[];
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
  await writeFile(capabilityFile, "controller-capability\n", { mode: 0o600 });
  process.env.LEGION_CONTROLLER = "1";
  process.env.LEGION_ROLE = "controller";
  process.env.LEGION_CONTROLLER_SECRET_FILE = capabilityFile;
  process.env.LEGION_DAEMON_URL = "http://daemon.test";
  process.env.ENVOY_URL = "http://envoy.test";
  process.env.LEGION_PROJECT = project;
  process.env.LEGION_STATE_DIR = stateDir;
  process.env.LEGION_CONTROLLER_START_MESSAGE =
    options.startMessage ?? "Legion controller start: follow skill://legion-controller";

  const requests: DaemonRequest[] = [];
  const goldenState = JSON.parse(
    await readFile(
      path.join(
        import.meta.dir,
        "../../contracts/fixtures/daemon-api",
        options.stateFixture ?? "state.json"
      ),
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
    if (url.pathname === "/legion/v1/issues/status") return Response.json({});
    if (url.pathname === "/legion/v1/claims/ready") return new Response(null, { status: 204 });
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
    secretsDir,
    requests,
    sentUserMessages: fixture.sentUserMessages,
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
    // The controller's `legion` tool: `read_state` and `set_status`, registered with its claim.
    expect(controller.tools.map((tool) => tool.name)).toContain("legion");
  });

  // LEGION-392's race: Oh My Pi's own positional-argument first message can lose the session's one
  // first-turn slot to an Envoy notice. The extension now sends the start message itself, right
  // after the role claim and before the live wake subscription opens (claim() in
  // controller-session.ts: claimEnvoyRole, then pi.sendUserMessage, then subscribeLegionNotice —
  // no await between the send and the claim it follows, so nothing scheduled after the claim can
  // run first), so nothing can race it.
  test("sends LEGION_CONTROLLER_START_MESSAGE as the session's first turn once its claim succeeds, before the controller-topic subscribe opens", async () => {
    const topic = "notifications.legion.omp.controller";
    const controller = await launchedController({
      sessionId: "ses_controller_start",
      startMessage: "Legion controller start: follow skill://legion-controller's start procedure",
    });
    await controller.handlers.get("session_start")?.(
      {},
      controller.context("ses_controller_start")
    );

    expect(controller.sentUserMessages).toEqual([
      "Legion controller start: follow skill://legion-controller's start procedure",
    ]);
    // The real ordering guarantee: the send precedes the subscribe that opens the live wake
    // channel a notice could otherwise race it on.
    expect(eventOrder.indexOf("sendUserMessage")).toBeGreaterThanOrEqual(0);
    expect(eventOrder.indexOf(`subscribe:${topic}`)).toBeGreaterThan(
      eventOrder.indexOf("sendUserMessage")
    );
  });

  // The start message is read and validated before anything that mutates daemon-side state
  // (registerController, which replaces the running controller's session; claimEnvoyRole, which
  // takes its role): a missing value refuses here, leaving the previous controller, if any, still
  // running and still registered.
  test("a launched claim with LEGION_CONTROLLER_START_MESSAGE unset refuses and makes no /legion/v1/claims/register request", async () => {
    const controller = await launchedController({ sessionId: "ses_controller_no_start_message" });
    delete process.env.LEGION_CONTROLLER_START_MESSAGE;

    await expect(
      controller.handlers.get("session_start")?.(
        {},
        controller.context("ses_controller_no_start_message")
      )
    ).rejects.toThrow("LEGION_CONTROLLER_START_MESSAGE is required for Legion");

    expect(
      controller.requests.filter((request) => request.path === "/legion/v1/claims/register")
    ).toEqual([]);
    expect(controller.sentUserMessages).toEqual([]);
  });

  // startMessageSent, not controllerSessionID, gates the send: a claim that throws after sending
  // (here, the controller-topic subscribe) never reaches the controllerSessionID assignment at
  // the end of claim(), so a guard keyed on controllerSessionID would send a second time on retry.
  // Forcing the subscribe itself to throw (rather than a connect envoy.ts's own session_start
  // handler retries quietly in the background) needs the same technique the subscription-opening
  // tests below use: close the connection envoy.ts already opened eagerly, from the
  // registration's own callback, so ensureConnection() inside subscribe() dials again — only then
  // does a rejected dial reach subscribeNotice uncaught.
  test("a claim whose controller-topic subscribe throws, then retried, sends one start message in total", async () => {
    let subscribeAttempts = 0;
    const controller = await launchedController({
      sessionId: "ses_controller_subscribe_retry",
      register: () => {
        const live = natsConnections.find(
          (candidate) => candidate.name === "omp-ses_controller_subscribe_retry"
        );
        if (live !== undefined) live.closed = true;
        natsConnectGates.set("omp-ses_controller_subscribe_retry", async () => {
          subscribeAttempts += 1;
          if (subscribeAttempts === 1) throw new Error("nats: connection refused");
        });
        return undefined;
      },
    });

    await expect(
      controller.handlers.get("session_start")?.(
        {},
        controller.context("ses_controller_subscribe_retry")
      )
    ).rejects.toThrow("nats: connection refused");
    expect(controller.sentUserMessages).toEqual([
      "Legion controller start: follow skill://legion-controller",
    ]);

    // Retried in the same session and process: /legion-claim-controller re-enters claim()
    // directly, as a lost-claim recovery would.
    const claimCommand = controller.commands.find(
      (command) => command.name === "legion-claim-controller"
    );
    if (claimCommand === undefined) throw new Error("controller claim command was not registered");
    await claimCommand.handler("", controller.context("ses_controller_subscribe_retry"));

    expect(controller.sentUserMessages).toEqual([
      "Legion controller start: follow skill://legion-controller",
    ]);
  });

  // Reading LEGION_CONTROLLER_START_MESSAGE is gated on `launched` (a `legion controller start`
  // launch, never a hand-started takeover), checked on a process that has made no prior claim at
  // all: `startMessageSent` alone would not catch a dropped `launched &&` here, since it starts
  // `false` the same way a genuine first launch does.
  test("a hand-started takeover with no prior claim in this process sends no start message of its own", async () => {
    const controller = await launchedController({ sessionId: "ses_controller_takeover_only" });
    // No session_start: this process's controllerSession has made no claim yet. A hand-started
    // takeover carries no controller marker.
    delete process.env.LEGION_CONTROLLER;
    delete process.env.LEGION_ROLE;
    delete process.env.LEGION_CONTROLLER_START_MESSAGE;
    const claimCommand = controller.commands.find(
      (command) => command.name === "legion-claim-controller"
    );
    if (claimCommand === undefined) throw new Error("controller claim command was not registered");
    await claimCommand.handler("", controller.context("ses_controller_takeover_only"));

    expect(controller.sentUserMessages).toEqual([]);
  });

  test("a hand-started takeover sends no start message of its own: the operator's own session speaks for itself", async () => {
    const controller = await launchedController({ sessionId: "ses_controller_takeover_pane" });
    await controller.handlers.get("session_start")?.(
      {},
      controller.context("ses_controller_takeover_pane")
    );
    delete process.env.LEGION_CONTROLLER;
    delete process.env.LEGION_ROLE;
    const claimCommand = controller.commands.find(
      (command) => command.name === "legion-claim-controller"
    );
    if (claimCommand === undefined) throw new Error("controller claim command was not registered");
    await claimCommand.handler("", controller.context("ses_controller_takeover_hand"));

    // One send, from the fresh launch; the hand-started takeover added none.
    expect(controller.sentUserMessages).toEqual([
      "Legion controller start: follow skill://legion-controller",
    ]);
  });

  test("a session switch (/new, /resume) does not resend the start message: only a fresh process launch does", async () => {
    const controller = await launchedController({ sessionId: "ses_controller_switch_before" });
    await controller.handlers.get("session_start")?.(
      {},
      controller.context("ses_controller_switch_before")
    );
    await controller.handlers.get("session_switch")?.(
      {},
      controller.context("ses_controller_switch_after", "/tmp/ses_controller_switch_after.jsonl")
    );

    expect(controller.sentUserMessages).toEqual([
      "Legion controller start: follow skill://legion-controller",
    ]);
  });

  // `controller: daemon`: the pod carries its launch's boot token, never a capability fetched over
  // the operator's bearer. The session registers with that token and, holding the role and the
  // topic, reports ready, which is when the daemon hands it the start message. The state it reads
  // first is the daemon's own while that daemon holds the controller's claim (internal/projection's
  // golden), which the plugin's strict client parses. The pod carries no
  // LEGION_CONTROLLER_START_MESSAGE, and the session sends no start message of its own: the
  // daemon's delivery at ready is its one start message.
  test("a controller the daemon launched registers with its boot token, then reports ready once it holds the role", async () => {
    const controller = await launchedController({
      sessionId: "ses_controller_pod",
      stateFixture: "state-controller-claim.json",
    });
    const bootFile = path.join(controller.secretsDir, "LEGION_BOOT_TOKEN");
    await writeFile(bootFile, "launch-boot-token\n", { mode: 0o600 });
    delete process.env.LEGION_CONTROLLER_SECRET_FILE;
    delete process.env.LEGION_CONTROLLER_START_MESSAGE;
    process.env.LEGION_BOOT_TOKEN_FILE = bootFile;
    try {
      await controller.handlers.get("session_start")?.(
        {},
        controller.context("ses_controller_pod")
      );
    } finally {
      delete process.env.LEGION_BOOT_TOKEN_FILE;
    }
    expect(daemonRequests(controller.requests)).toEqual([
      { path: "/legion/v1/state", body: undefined },
      {
        path: "/legion/v1/claims/register",
        body: {
          bootToken: "launch-boot-token",
          sessionId: "ses_controller_pod",
          ompSessionFile: "/tmp/ses_controller_pod.jsonl",
          agentId: "ses_controller_pod",
          pluginContract: pkg.legion.daemonApiVersion,
        },
      },
      {
        path: "/legion/v1/claims/ready",
        body: {
          claimToken: controller.token,
          sessionId: "ses_controller_pod",
          secret: controller.registration.secret,
          generation: controller.registration.generation,
        },
      },
    ]);
    const paths = controller.requests.map((request) => request.path);
    expect(paths.indexOf("/v1/roles/set")).toBeLessThan(paths.indexOf("/legion/v1/claims/ready"));
    expect(controller.sentUserMessages).toEqual([]);
    expect(controller.exits).toEqual([]);
  });

  // Nobody reads a daemon-launched controller's session, so a claim it cannot complete exits Oh My
  // Pi and the daemon relaunches it, as a pane's failed boot does.
  test("a daemon-launched controller whose registration is refused exits", async () => {
    const controller = await launchedController({
      sessionId: "ses_controller_pod",
      register: () => Response.json({ error: "invalid boot token" }, { status: 403 }),
    });
    const bootFile = path.join(controller.secretsDir, "LEGION_BOOT_TOKEN");
    await writeFile(bootFile, "stale-boot-token\n", { mode: 0o600 });
    delete process.env.LEGION_CONTROLLER_SECRET_FILE;
    process.env.LEGION_BOOT_TOKEN_FILE = bootFile;
    try {
      // The test's exit hook throws in place of exiting; the throw ends the handler.
      await controller.handlers.get("session_start")?.(
        {},
        controller.context("ses_controller_pod")
      );
    } catch {
      // process would exit
    } finally {
      delete process.env.LEGION_BOOT_TOKEN_FILE;
    }
    expect(controller.exits).toEqual([1]);
    expect(controller.requests.map((request) => request.path)).not.toContain(
      "/legion/v1/claims/ready"
    );
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

  test("set_status fails naming the daemon's refusal when it refuses the controller grant", async () => {
    const controller = await launchedController({
      sessionId: "ses_controller_refused",
      grant: () => Response.json({ error: "Invalid controller capability" }, { status: 403 }),
    });
    const context = controller.context("ses_controller_refused");
    await controller.handlers.get("session_start")?.({}, context);
    const legion = controller.tools.find((tool) => tool.name === "legion");

    await expect(
      legion?.execute(
        "controller-refused",
        { op: "set_status", issue: "REPO-42", status: "icebox" },
        undefined,
        undefined,
        context
      )
    ).resolves.toMatchObject({
      isError: true,
      content: [
        {
          type: "text",
          text: "POST /legion/v1/grants failed with 403: Invalid controller capability",
        },
      ],
    });
    expect(controller.requests.map((request) => request.path)).not.toContain(
      "/legion/v1/issues/status"
    );
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

      // The operator types /new into the pane.
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

  test("the controller's legion tool reads the whole state without a grant and sets a status with a grant minted from its registration secret", async () => {
    const controller = await launchedController({ sessionId: "ses_controller_tool" });
    const context = controller.context("ses_controller_tool");
    // Nothing is registered before the claim: the tool arrives with it.
    expect(controller.tools.map((tool) => tool.name)).not.toContain("legion");

    await controller.handlers.get("session_start")?.({}, context);
    const legion = controller.tools.find((tool) => tool.name === "legion");
    if (legion === undefined) throw new Error("the controller's legion tool was not registered");
    const requestsAfterClaim = controller.requests.length;

    await expect(
      legion.execute("controller-state", { op: "read_state" }, undefined, undefined, context)
    ).resolves.toMatchObject({
      details: { daemon: { project: "OMP" }, issues: expect.any(Object) },
    });
    await expect(
      legion.execute(
        "controller-status",
        { op: "set_status", issue: "REPO-42", status: "todo" },
        undefined,
        undefined,
        context
      )
    ).resolves.toEqual({ content: [{ type: "text", text: "{}" }], details: {} });
    // The state read mints nothing; the status write mints one controller grant (`/grants`'
    // `{sessionId, secret}` form, the registration's secret) and posts it with the request.
    expect(daemonRequests(controller.requests.slice(requestsAfterClaim))).toEqual([
      { path: "/legion/v1/state", body: undefined },
      {
        path: "/legion/v1/grants",
        body: { sessionId: "ses_controller_tool", secret: controller.registration.secret },
      },
      {
        path: "/legion/v1/issues/status",
        body: { grantId: "controller-grant-1", issue: "REPO-42", status: "todo" },
      },
    ]);
    // A claim's operations are not the controller's.
    await expect(
      legion.execute(
        "controller-record",
        { op: "read_record", issue: "REPO-42" },
        undefined,
        undefined,
        context
      )
    ).resolves.toMatchObject({
      isError: true,
      content: [{ type: "text", text: "read_record is not available to a controller session" }],
    });
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
    envoyPluginInterface().bootstrappedSession.file = undefined;
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
    envoyPluginInterface().bootstrappedSession.file = undefined;
    const second = createPi();
    legionExtension(second.pi);
    await second.handlers.get("session_start")?.({}, first.context("ses_controller_second"));
    replaced = true;

    // The first session's process died, and the session is resumed within the listener's reap
    // window: its transcript records the role claim, which the listener now refuses it.
    envoyPluginInterface().bootstrappedSession.file = undefined;
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
    envoyPluginInterface().bootstrappedSession.file = undefined;
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
      envoyPluginInterface().bootstrappedSession.file = undefined;
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
      envoyPluginInterface().bootstrappedSession.file = undefined;
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
      envoyPluginInterface().bootstrappedSession.file = undefined;
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

  test("the session's title file holds the Legion title for the pane's first dispatch command", async () => {
    const worker = await bootPane({ role: "implementer", sessionId: "ses_title_file" });

    expect(worker.title.set).toEqual(["Legion implementer · REPO-43"]);
    expect(readSessionTitle(sessionDirectory(process.env, "ses_title_file"))).toBe(
      "Legion implementer · REPO-43"
    );
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

describe("the recreated-workspace notice", () => {
  const notices = (pane: ClaimPane) =>
    pane.sentMessages.flatMap((message, i) =>
      "customType" in message && message.customType === "legion-workspace-recreated"
        ? [{ message, options: pane.sentMessageOptions[i] }]
        : []
    );
  // A resumed session, its history uncompacted turns, whose role launcher said its workspace was
  // recreated: at session start the plugin saves one notice naming the issue's branch, as a steer
  // that starts no turn, so Oh My Pi stores it ahead of the next turn whatever starts that turn.
  test("a resume told LEGION_WORKSPACE_RECREATED=true saves one notice ahead of its next turn", async () => {
    process.env.LEGION_WORKSPACE_RECREATED = "true";
    const history = [
      { type: "message", message: { role: "user", content: [{ type: "text", text: "TURN-1" }] } },
      {
        type: "message",
        message: { role: "assistant", content: [{ type: "text", text: "reply 1" }] },
      },
      { type: "message", message: { role: "user", content: [{ type: "text", text: "TURN-2" }] } },
      {
        type: "message",
        message: { role: "assistant", content: [{ type: "text", text: "reply 2" }] },
      },
    ];
    const pane = await bootPane({ role: "implementer", issue: "REPO-43", branch: history });
    expect(notices(pane)).toEqual([
      {
        message: {
          customType: "legion-workspace-recreated",
          content: expect.stringContaining(
            "Your workspace was recreated since your last turn: it holds what was pushed to legion/REPO-43"
          ),
          display: true,
          details: { id: expect.any(String) },
        },
        options: { deliverAs: "steer", triggerTurn: false },
      },
    ]);
  });

  test("a resume told false, or nothing, saves no notice", async () => {
    for (const value of ["false", undefined]) {
      if (value === undefined) delete process.env.LEGION_WORKSPACE_RECREATED;
      else process.env.LEGION_WORKSPACE_RECREATED = value;
      const pane = await bootPane({ role: "implementer", sessionId: `ses_recreated_${value}` });
      expect(notices(pane)).toEqual([]);
      expect(
        await pane.handlers.get("before_agent_start")?.({ prompt: "task" }, pane.context)
      ).toBeUndefined();
    }
  });

  // A session that is no Legion session is left alone even with the variable set: only the role
  // launcher sets it, beside the role variables, and pi-legion is inert outside a Legion session.
  test("a session that is no Legion session saves no notice", async () => {
    process.env.LEGION_WORKSPACE_RECREATED = "true";
    const fixture = createPi({ bindEnvoy: false });
    legionExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, sessionContext("ses_person"));
    expect(fixture.sentMessages).toEqual([]);
    expect(
      await fixture.handlers.get("before_agent_start")?.(
        { prompt: "task" },
        sessionContext("ses_person")
      )
    ).toBeUndefined();
  });

  /** The pane's own notice id, its before_agent_start and context handlers, and a context whose
   * `getBranch()` returns what `branch()` does at each call. */
  const recoveryPane = async (branch: () => readonly unknown[]) => {
    process.env.LEGION_WORKSPACE_RECREATED = "true";
    const pane = await bootPane({ role: "implementer", issue: "REPO-43" });
    const sent = notices(pane)[0]?.message;
    const id =
      sent !== undefined && "details" in sent && typeof sent.details?.id === "string"
        ? sent.details.id
        : undefined;
    const beforeAgentStart = pane.handlers.get("before_agent_start");
    const requestOf = pane.handlers.get("context");
    if (id === undefined || beforeAgentStart === undefined || requestOf === undefined) {
      throw new Error("the pane sent no notice, or registered no before_agent_start or context");
    }
    const context = {
      ...pane.context,
      sessionManager: { ...pane.context.sessionManager, getBranch: branch },
    };
    return { pane, id, beforeAgentStart, requestOf, context };
  };
  const entry = (id: string) => ({
    type: "custom_message",
    customType: "legion-workspace-recreated",
    content: "Your workspace was recreated since your last turn: …",
    display: true,
    details: { id },
  });
  const copy = (id: string) => ({
    role: "custom",
    customType: "legion-workspace-recreated",
    content: "…",
    details: { id },
  });
  const task = { role: "user", content: [{ type: "text", text: "TURN-2" }] };
  const turn = (role: "user" | "assistant", text: string) => ({
    type: "message",
    message: { role, content: [{ type: "text", text }] },
  });

  // The daemon's next task is an RPC prompt, and Oh My Pi first recovers a failed last turn: an
  // empty `length` stop is dropped by moving the branch back to that turn's parent, which takes the
  // copy saved after it off the branch. before_agent_start runs after that recovery and puts the
  // notice into the run's messages when the branch no longer holds this process's copy; while the
  // branch holds it, nothing is added. Once the first run starts, nothing is owed. The recovery left
  // the saved copy in the live context, so from the re-send on each request keeps the first copy
  // alone, and before it no request is touched.
  test("a prompt whose recovery dropped the saved notice carries it again, once", async () => {
    let branch: readonly unknown[] = [];
    const { id, pane, beforeAgentStart, requestOf, context } = await recoveryPane(() => branch);
    branch = [
      turn("user", "TURN-1"),
      { type: "message", message: { role: "assistant", content: [], stopReason: "length" } },
      entry(id),
    ];

    expect(await beforeAgentStart({ prompt: "task" }, context)).toBeUndefined();
    expect(await requestOf({ messages: [copy(id), task, copy(id)] }, context)).toBeUndefined();
    branch = branch.slice(0, 1);
    expect(await beforeAgentStart({ prompt: "task" }, context)).toEqual({
      message: {
        customType: "legion-workspace-recreated",
        content: expect.stringContaining("it holds what was pushed to legion/REPO-43"),
        display: true,
        details: { id },
      },
    });
    await pane.handlers.get("agent_start")?.({}, context);
    expect(await beforeAgentStart({ prompt: "task" }, context)).toBeUndefined();
    expect(await requestOf({ messages: [copy(id), task, copy(id)] }, context)).toEqual({
      messages: [copy(id), task],
    });
    expect(await requestOf({ messages: [copy(id), task] }, context)).toBeUndefined();
  });

  // A session recreated twice carries the earlier recreation's notice in its history. When the
  // recovery drops this process's copy, that earlier notice is not it: the notice is sent again,
  // and the request filter leaves the earlier one where the history put it, keeping this process's
  // first copy too. The same holds when the earlier notice sits after the last good reply, as one
  // saved by a process lost before its turn's first reply does.
  test("a notice an earlier recreation saved does not stand in for this process's", async () => {
    let branch: readonly unknown[] = [];
    const { id, beforeAgentStart, requestOf, context } = await recoveryPane(() => branch);
    const earlier = "an-earlier-process";
    for (const recovered of [
      [turn("user", "TURN-1"), entry(earlier), turn("user", "X"), turn("assistant", "reply X")],
      [turn("user", "TURN-1"), turn("assistant", "reply 1"), entry(earlier), turn("user", "X")],
    ]) {
      branch = recovered;
      expect(await beforeAgentStart({ prompt: "task" }, context)).toEqual({
        message: expect.objectContaining({ details: { id } }),
      });
    }
    expect(
      await requestOf({ messages: [copy(earlier), copy(id), task, copy(id)] }, context)
    ).toEqual({ messages: [copy(earlier), copy(id), task] });
  });
});
