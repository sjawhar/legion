import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { agentSubject, roleToken } from "@legion/contracts";
import legionExtension, {
  classifySession,
  handleLegionControlDirective,
  registerWorkerBudgetPermit,
} from "./legion";

type SessionContext = {
  readonly cwd: string;
  readonly taskDepth?: number;
  readonly sessionManager: {
    readonly getSessionId: () => string;
    readonly getSessionFile: () => string | undefined;
  };
};

type CommandContext = {
  readonly cwd: string;
  readonly sessionManager: { readonly getSessionId: () => string };
};

type RegisteredCommand = {
  readonly name: string;
  readonly description: string;
  readonly handler: (args: string, context: CommandContext) => Promise<void>;
};

type ToolResult = {
  readonly content: readonly { readonly type: "text"; readonly text: string }[];
  readonly details: Readonly<Record<string, unknown>>;
  readonly isError?: boolean;
};

type RegisteredTool = {
  readonly name: string;
  readonly defaultInactive?: boolean;
  readonly execute: (
    id: string,
    parameters: Record<string, unknown>,
    signal: AbortSignal | undefined,
    onUpdate: unknown,
    context: SessionContext
  ) => Promise<ToolResult>;
};

type ZodProperty = { readonly optional: () => unknown };

type TestAgent = { readonly id: string };

type ExtensionAgentsApi = {
  readonly list: () => readonly TestAgent[];
  readonly get: (agentId: string) => TestAgent | undefined;
  readonly ensureLive: (
    agentId: string,
    options: { readonly parentSessionFile: string }
  ) => Promise<TestAgent>;
  readonly prompt: (agentId: string, content: string) => Promise<void>;
};

type TestPi = {
  readonly zod: {
    readonly object: (shape: Readonly<Record<string, unknown>>) => unknown;
    readonly string: () => ZodProperty;
    readonly array: (item: unknown) => ZodProperty;
    readonly enum: (values: readonly string[]) => ZodProperty;
    readonly unknown: () => unknown;
    readonly discriminatedUnion: (key: string, options: readonly unknown[]) => unknown;
  };
  readonly agents: ExtensionAgentsApi;
  readonly sendMessage: (message: { readonly type: string }) => void;
  readonly on: (
    event: string,
    handler: (event: unknown, context: SessionContext) => Promise<unknown> | unknown
  ) => void;
  readonly registerTool: (tool: RegisteredTool) => void;
  readonly registerCommand: (name: string, command: Omit<RegisteredCommand, "name">) => void;
};

type Handler = (event: unknown, context: SessionContext) => Promise<unknown> | unknown;

const originalFetch = globalThis.fetch;
const environmentKeys = [
  "ENVOY_URL",
  "LEGION_CONTROLLER",
  "LEGION_DAEMON_URL",
  "LEGION_GENERATION",
  "LEGION_BOOT_TOKEN",
  "LEGION_PROJECT",
  "LEGION_TREE",
  "LEGION_WORKER_BUDGET",
  "LEGION_MAX_RECURSION_DEPTH",
] as const;
const originalEnvironment: Record<(typeof environmentKeys)[number], string | undefined> = {
  ENVOY_URL: process.env.ENVOY_URL,
  LEGION_CONTROLLER: process.env.LEGION_CONTROLLER,
  LEGION_DAEMON_URL: process.env.LEGION_DAEMON_URL,
  LEGION_GENERATION: process.env.LEGION_GENERATION,
  LEGION_BOOT_TOKEN: process.env.LEGION_BOOT_TOKEN,
  LEGION_PROJECT: process.env.LEGION_PROJECT,
  LEGION_TREE: process.env.LEGION_TREE,
  LEGION_WORKER_BUDGET: process.env.LEGION_WORKER_BUDGET,
  LEGION_MAX_RECURSION_DEPTH: process.env.LEGION_MAX_RECURSION_DEPTH,
};

const temporaryPaths: string[] = [];

afterEach(async () => {
  globalThis.fetch = originalFetch;
  for (const key of environmentKeys) {
    const value = originalEnvironment[key];
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  await Promise.all(
    temporaryPaths.splice(0).map((directory) => rm(directory, { force: true, recursive: true }))
  );
});

function createPi(options: { readonly agents?: ExtensionAgentsApi } = {}): {
  readonly commands: RegisteredCommand[];
  readonly handlers: Map<string, Handler>;
  readonly tools: RegisteredTool[];
  readonly sentMessages: { readonly type: string }[];
  readonly pi: TestPi;
} {
  const commands: RegisteredCommand[] = [];
  const handlers = new Map<string, Handler>();
  const tools: RegisteredTool[] = [];
  const sentMessages: { readonly type: string }[] = [];
  const optional = (): ZodProperty => ({ optional: () => undefined });
  const agents =
    options.agents ??
    ({
      list: () => [],
      get: () => undefined,
      ensureLive: async (agentId) => ({ id: agentId }),
      prompt: async () => undefined,
    } satisfies ExtensionAgentsApi);
  const pi: TestPi = {
    zod: {
      object: (shape) => shape,
      string: optional,
      array: () => optional(),
      enum: () => optional(),
      unknown: () => undefined,
      discriminatedUnion: () => ({}),
    },
    agents,
    sendMessage: (message) => sentMessages.push(message),
    on: (event, handler) => handlers.set(event, handler),
    registerTool: (tool) => tools.push(tool),
    registerCommand: (name, command) => commands.push({ name, ...command }),
  };
  return { commands, handlers, tools, sentMessages, pi };
}

function sessionContext(sessionID: string, sessionFile = "/tmp/session.jsonl"): SessionContext {
  return {
    cwd: "/tmp/legion-workspace",
    sessionManager: {
      getSessionId: () => sessionID,
      getSessionFile: () => sessionFile,
    },
  };
}
function injectedTask(result: unknown): string {
  if (
    typeof result !== "object" ||
    result === null ||
    Array.isArray(result) ||
    !("input" in result) ||
    typeof result.input !== "object" ||
    result.input === null ||
    Array.isArray(result.input) ||
    !("task" in result.input) ||
    typeof result.input.task !== "string"
  ) {
    throw new Error("Legion task spawn did not inject a prompt");
  }
  return result.input.task;
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
describe("Legion OMP extension", () => {
  test("classifies root architects, controllers, phase workers, sub-architects, and ordinary sessions", () => {
    expect(
      classifySession({ LEGION_TREE: "owner/repo#42", LEGION_PROJECT: "omp" }, undefined, 0)
    ).toEqual({ kind: "root-architect", tree: "owner/repo#42" });
    expect(
      classifySession({ LEGION_CONTROLLER: "1", LEGION_PROJECT: "omp" }, undefined, 0)
    ).toEqual({ kind: "controller" });
    expect(classifySession({}, "legion-reviewer", 1)).toEqual({
      kind: "phase-worker",
      role: "reviewer",
    });
    expect(classifySession({}, "legion-architect", 1)).toEqual({ kind: "sub-architect" });
    expect(classifySession({}, "legion-architect", 0)).toEqual({ kind: "not-legion" });
    expect(classifySession({}, "scout", 1)).toEqual({ kind: "not-legion" });
  });
  test("registers the root process before claiming its role and agent delivery subject", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "owner/repo#42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-root-registration";
    process.env.LEGION_PROJECT = "omp";
    process.env.LEGION_TREE = tree;
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

    expect(requests).toEqual([
      {
        path: "/legion/v1/process/started",
        body: {
          tree,
          generation: 3,
          bootToken: "boot-root-registration",
          rootSessionId: "ses_root",
          ompSessionFile: "/tmp/session.jsonl",
        },
      },
      { path: "/v1/roles/set", body: { session_id: "ses_root", role: token } },
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
    ]);
    const secondFixture = createPi();
    legionExtension(secondFixture.pi);
    const secondSessionStart = secondFixture.handlers.get("session_start");
    if (secondSessionStart === undefined)
      throw new Error("second session_start handler was not registered");
    await secondSessionStart({}, sessionContext("ses_child"));
    expect(requests).toHaveLength(3);
  });
  test("claims the controller role at startup and on demand for an interactive session", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const token = "legion-omp-controller";
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_CONTROLLER = "1";
    process.env.LEGION_PROJECT = "omp";
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
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
    await claimCommand.handler("", {
      cwd: "/tmp/legion-workspace",
      sessionManager: { getSessionId: () => "ses_interactive" },
    });

    expect(requests).toEqual([
      { path: "/v1/roles/set", body: { session_id: "ses_controller", role: token } },
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
      { path: "/v1/roles/set", body: { session_id: "ses_interactive", role: token } },
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
    ]);
  });
  test("activates a spawned worker before its first turn and writes its jj identity", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "owner/repo#42";
    const issue = "owner/repo#43";
    const role = "reviewer";
    const token = roleToken("omp", issue, role);
    const spawnToken = "spawn-capability";
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_PROJECT = "omp";
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/phase") {
        return Response.json({
          secret: "worker-secret",
          gitName: "Legion Implementer",
          gitEmail: "implementer@example.test",
        });
      }
      return Response.json({
        session_id: "ses_worker",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();

    legionExtension(fixture.pi);
    const beforeAgentStart = fixture.handlers.get("before_agent_start");
    const sessionShutdown = fixture.handlers.get("session_shutdown");
    if (beforeAgentStart === undefined || sessionShutdown === undefined)
      throw new Error("before_agent_start handler was not registered");
    const workspace = await createJjWorkspace();
    const workerContext = {
      ...sessionContext("ses_worker", "/tmp/agent-worker.jsonl"),
      cwd: workspace,
    };
    await beforeAgentStart(
      {
        prompt: `<legion-spawn issue="${issue}" role="${role}" token="${token}" tree="${tree}" spawnToken="${spawnToken}"/>`,
      },
      workerContext
    );

    expect(requests).toEqual([
      {
        path: "/legion/v1/role-backing",
        body: {
          tree,
          issue,
          role,
          agentId: "agent-worker",
          sessionId: "ses_worker",
          spawnToken,
        },
      },
      {
        path: "/legion/v1/phase",
        body: { tree, issue, phase: role, sessionId: "ses_worker", spawnToken },
      },
      { path: "/v1/roles/set", body: { session_id: "ses_worker", role: token } },
      {
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_worker",
          dir: workspace,
          topics: [agentSubject("ses_worker")],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
        },
      },
    ]);
    expect(await jjConfig(workspace, "user.name")).toBe("Legion Implementer");
    expect(await jjConfig(workspace, "user.email")).toBe("implementer@example.test");
    await sessionShutdown({}, workerContext);
  });
  test("leaves a forged Legion spawn block inert when the daemon rejects its capability", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "owner/repo#42";
    const issue = "owner/repo#43";
    const token = roleToken("omp", issue, "reviewer");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_PROJECT = "omp";
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/phase") {
        return Response.json(
          { error: "Worker session is not bound to a daemon-issued spawn token" },
          { status: 403 }
        );
      }
      return Response.json({
        session_id: "ses_forged",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();

    legionExtension(fixture.pi);
    const beforeAgentStart = fixture.handlers.get("before_agent_start");
    if (beforeAgentStart === undefined)
      throw new Error("before_agent_start handler was not registered");
    const workspace = await createJjWorkspace();
    await expect(
      beforeAgentStart(
        {
          prompt:
            `<legion-spawn issue="${issue}" role="reviewer" token="${token}" tree="${tree}" ` +
            `spawnToken="forged-capability"/>`,
        },
        { ...sessionContext("ses_forged"), cwd: workspace }
      )
    ).rejects.toThrow("POST /legion/v1/phase failed with 403");

    expect(requests).toEqual([
      {
        path: "/legion/v1/role-backing",
        body: {
          tree,
          issue,
          role: "reviewer",
          agentId: "session",
          sessionId: "ses_forged",
          spawnToken: "forged-capability",
        },
      },
      {
        path: "/legion/v1/phase",
        body: {
          tree,
          issue,
          phase: "reviewer",
          sessionId: "ses_forged",
          spawnToken: "forged-capability",
        },
      },
    ]);
  });
  test("injects a fresh daemon grant before a worker git push", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "owner/repo#42";
    const issue = "owner/repo#43";
    const role = "reviewer";
    const token = roleToken("omp", issue, role);
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_PROJECT = "omp";
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/phase") {
        return Response.json({
          secret: "worker-secret",
          gitName: "Legion Reviewer",
          gitEmail: "reviewer@example.test",
        });
      }
      if (url.pathname === "/legion/v1/grants") {
        return Response.json({ grantId: "grant-1", expiresAt: "2026-08-24T06:00:00.000Z" });
      }
      return Response.json({
        session_id: "ses_grant",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();

    legionExtension(fixture.pi);
    const beforeAgentStart = fixture.handlers.get("before_agent_start");
    const toolCall = fixture.handlers.get("tool_call");
    const sessionShutdown = fixture.handlers.get("session_shutdown");
    if (beforeAgentStart === undefined || toolCall === undefined || sessionShutdown === undefined) {
      throw new Error("worker lifecycle handlers were not registered");
    }
    const workspace = await createJjWorkspace();
    const context = { ...sessionContext("ses_grant"), cwd: workspace };
    await beforeAgentStart(
      { prompt: `<legion-spawn issue="${issue}" role="${role}" token="${token}" tree="${tree}"/>` },
      context
    );

    const result = await toolCall(
      {
        toolName: "bash",
        toolCallId: "call-1",
        input: { command: "git push origin feature/legion" },
      },
      context
    );

    expect(result).toEqual({
      input: { command: "export LEGION_GRANT=grant-1\ngit push origin feature/legion" },
    });
    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/grants",
      body: { tree, issue, sessionId: "ses_grant", secret: "worker-secret" },
    });
    await sessionShutdown({}, context);
  });
  test("parks a worker by deleting its interest and releasing its worker-budget permit", async () => {
    const deletedSessions: string[] = [];
    const tree = "owner/repo#42";
    const issue = "owner/repo#43";
    const role = "reviewer";
    const token = roleToken("omp", issue, role);
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_PROJECT = "omp";
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      if (init?.method === "DELETE") {
        deletedSessions.push(url.pathname);
        return Response.json({});
      }
      if (url.pathname === "/legion/v1/phase") {
        return Response.json({
          secret: "worker-secret",
          gitName: "Legion Reviewer",
          gitEmail: "reviewer@example.test",
        });
      }
      return Response.json({
        session_id: "ses_park",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();

    legionExtension(fixture.pi);
    const beforeAgentStart = fixture.handlers.get("before_agent_start");
    const sessionShutdown = fixture.handlers.get("session_shutdown");
    if (beforeAgentStart === undefined || sessionShutdown === undefined) {
      throw new Error("worker lifecycle handlers were not registered");
    }
    const workspace = await createJjWorkspace();
    const context = { ...sessionContext("ses_park"), cwd: workspace };
    await beforeAgentStart(
      { prompt: `<legion-spawn issue="${issue}" role="${role}" token="${token}" tree="${tree}"/>` },
      context
    );
    await sessionShutdown({}, context);

    expect(deletedSessions).toEqual(["/v1/interests/ses_park"]);
  });
  test("reclaims a parked worker role without registering a second phase", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "owner/repo#42";
    const issue = "owner/repo#43";
    const role = "reviewer";
    const token = roleToken("omp", issue, role);
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_PROJECT = "omp";
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/phase") {
        return Response.json({
          secret: "worker-secret",
          gitName: "Legion Reviewer",
          gitEmail: "reviewer@example.test",
        });
      }
      return Response.json({
        session_id: "ses_revived",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();

    legionExtension(fixture.pi);
    const beforeAgentStart = fixture.handlers.get("before_agent_start");
    const sessionStart = fixture.handlers.get("session_start");
    const sessionShutdown = fixture.handlers.get("session_shutdown");
    if (beforeAgentStart === undefined || sessionStart === undefined || sessionShutdown === undefined) {
      throw new Error("worker lifecycle handlers were not registered");
    }
    const workspace = await createJjWorkspace();
    const context = { ...sessionContext("ses_revived"), cwd: workspace };
    await beforeAgentStart(
      { prompt: `<legion-spawn issue="${issue}" role="${role}" token="${token}" tree="${tree}"/>` },
      context
    );
    await sessionShutdown({}, context);
    await sessionStart({}, context);

    expect(requests.slice(-3)).toEqual([
      {
        path: "/legion/v1/role-backing",
        body: {
          tree,
          issue,
          role,
          agentId: "session",
          sessionId: "ses_revived",
          spawnToken: token,
        },
      },
      { path: "/v1/roles/set", body: { session_id: "ses_revived", role: token } },
      {
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_revived",
          dir: workspace,
          topics: [agentSubject("ses_revived")],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
        },
      },
    ]);
    expect(requests.filter((request) => request.path === "/legion/v1/phase")).toHaveLength(1);
    await sessionShutdown({}, context);
  });
  test("rebinds a parked worker's durable backing before re-claiming its role", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "owner/repo#42";
    const issue = "owner/repo#43";
    const token = roleToken("omp", issue, "reviewer");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-rebind";
    process.env.LEGION_PROJECT = "omp";
    process.env.LEGION_TREE = tree;
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (init?.method === "DELETE") return Response.json({});
      if (url.pathname === "/legion/v1/spawn-token") return Response.json({ spawnToken: "spawn-capability" });
      if (url.pathname === "/legion/v1/phase") {
        return Response.json({
          secret: "worker-secret",
          gitName: "Legion Reviewer",
          gitEmail: "reviewer@example.test",
        });
      }
      if (url.pathname === "/legion/v1/role-backing") return Response.json({});
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: roleToken("omp", tree, "architect") },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      return Response.json({
        session_id: body?.session_id,
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [token],
      });
    }) as typeof fetch;
    const fixture = createPi();
    const rootContext = sessionContext("ses_root");

    legionExtension(fixture.pi);
    const toolCall = fixture.handlers.get("tool_call");
    const toolResult = fixture.handlers.get("tool_result");
    const beforeAgentStart = fixture.handlers.get("before_agent_start");
    const sessionShutdown = fixture.handlers.get("session_shutdown");
    const sessionStart = fixture.handlers.get("session_start");
    if (
      toolCall === undefined ||
      toolResult === undefined ||
      beforeAgentStart === undefined ||
      sessionShutdown === undefined ||
      sessionStart === undefined
    ) {
      throw new Error("worker lifecycle handlers were not registered");
    }
    const spawnCall = {
      toolName: "task",
      toolCallId: "spawn-revivable-worker",
      input: {
        agent: "legion-reviewer",
        task: `Legion-Issue: ${issue}\nReview the implementation`,
      },
    };
    const prompt = injectedTask(await toolCall(spawnCall, rootContext));
    const workspace = await createJjWorkspace();
    const workerContext = {
      ...sessionContext("ses_revivable", "/tmp/agent-revivable.jsonl"),
      cwd: workspace,
    };
    await beforeAgentStart({ prompt }, workerContext);
    await toolResult(
      {
        ...spawnCall,
        details: {},
        isError: false,
      },
      rootContext
    );
    await sessionShutdown({}, workerContext);

    const rebindStart = requests.length;
    await sessionStart({}, workerContext);

    expect(requests.slice(rebindStart)).toEqual([
      {
        path: "/legion/v1/role-backing",
        body: {
          tree,
          issue,
          role: "reviewer",
          agentId: "agent-revivable",
          sessionId: "ses_revivable",
          spawnToken: "spawn-capability",
        },
      },
      { path: "/v1/roles/set", body: { session_id: "ses_revivable", role: token } },
      {
        path: "/v1/interests/subscribe",
        body: {
          session_id: "ses_revivable",
          dir: workspace,
          topics: [agentSubject("ses_revivable")],
          port: 0,
          title: "",
          driving: false,
          self_subscribed: true,
        },
      },
    ]);
    expect(requests.filter((request) => request.path === "/legion/v1/phase")).toHaveLength(1);
    await sessionShutdown({}, workerContext);
  });
  test("leaves a non-Legion prompt without a machine spawn block unclaimed", async () => {
    const requests: string[] = [];
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_PROJECT = "omp";
    globalThis.fetch = (async (input) => {
      requests.push(new URL(input.toString()).pathname);
      return Response.json({});
    }) as typeof fetch;
    const fixture = createPi();

    legionExtension(fixture.pi);
    const beforeAgentStart = fixture.handlers.get("before_agent_start");
    if (beforeAgentStart === undefined)
      throw new Error("before_agent_start handler was not registered");
    await beforeAgentStart(
      { prompt: "Review the implementation and return your findings." },
      sessionContext("ses_ordinary")
    );

    expect(requests).toEqual([]);
  });
  test("proxies architect issue creation through the daemon and returns the issue identity", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "owner/repo#42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-issue-create";
    process.env.LEGION_PROJECT = "omp";
    process.env.LEGION_TREE = tree;
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
      if (url.pathname === "/legion/v1/issues") {
        return Response.json({
          issue: "owner/repo#43",
          url: "https://github.test/owner/repo/issues/43",
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
    const context = sessionContext("ses_architect");

    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");
    await sessionStart({}, context);
    const legion = fixture.tools.find((tool) => tool.name === "legion");
    if (legion === undefined) throw new Error("legion tool was not registered");

    const result = await legion.execute(
      "call-1",
      { op: "issue_create", title: "Child work", body: "Do it", labels: ["needs-approval"] },
      undefined,
      undefined,
      context
    );

    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/issues",
      body: { tree, title: "Child work", body: "Do it", labels: ["needs-approval"] },
    });
    expect(result).toEqual({
      content: [
        {
          type: "text",
          text: JSON.stringify({
            issue: "owner/repo#43",
            url: "https://github.test/owner/repo/issues/43",
          }),
        },
      ],
      details: { issue: "owner/repo#43", url: "https://github.test/owner/repo/issues/43" },
    });
  });
  test("proxies architect Dispatch requests through the daemon without holding a bearer", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "owner/repo#42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-dispatch";
    process.env.LEGION_PROJECT = "omp";
    process.env.LEGION_TREE = tree;
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
      if (url.pathname === "/legion/v1/dispatch-threads") {
        return Response.json({
          thread: 77,
          url: "https://github.test/owner/repo/issues/77",
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
    const context = sessionContext("ses_architect");

    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");
    await sessionStart({}, context);
    const dispatch = fixture.tools.find((tool) => tool.name === "envoy_dispatch");
    if (dispatch === undefined) throw new Error("envoy_dispatch tool was not registered");

    const result = await dispatch.execute(
      "dispatch-1",
      {
        parent: tree,
        subject: "Need approval",
        body: "Choose A or B",
        ask: [{ question: "A?" }],
        urgency: "blocking",
      },
      undefined,
      undefined,
      context
    );

    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/dispatch-threads",
      body: {
        tree,
        issue: tree,
        role: "architect",
        sessionId: "ses_architect",
        secret: "root-secret",
        parent: tree,
        subject: "Need approval",
        body: "Choose A or B",
        ask: [{ question: "A?" }],
        urgency: "blocking",
      },
    });
    expect(result).toEqual({
      content: [
        {
          type: "text",
          text: JSON.stringify({ thread: 77, url: "https://github.test/owner/repo/issues/77" }),
        },
      ],
      details: { thread: 77, url: "https://github.test/owner/repo/issues/77" },
    });
  });
  test("maps every remaining legion operation to its daemon proxy endpoint", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "owner/repo#42";
    const issue = "owner/repo#43";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-operations";
    process.env.LEGION_PROJECT = "omp";
    process.env.LEGION_TREE = tree;
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
      if (url.pathname === "/legion/v1/issues/comment") {
        return Response.json({
          commentId: 99,
          url: "https://github.test/owner/repo/issues/43#issuecomment-99",
        });
      }
      if (url.pathname === "/legion/v1/issues/labels")
        return Response.json({ labels: ["needs-approval"] });
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
        input: { op: "wave_release", children: [issue] },
        request: { path: "/legion/v1/waves/release", body: { tree, children: [issue] } },
        details: { released: [issue] },
      },
      {
        input: { op: "comment", issue, body: "Status update" },
        request: {
          path: "/legion/v1/issues/comment",
          body: { tree, issue, body: "Status update" },
        },
        details: { commentId: 99, url: "https://github.test/owner/repo/issues/43#issuecomment-99" },
      },
      {
        input: { op: "post_spec", issue, body: "Specification" },
        request: { path: "/legion/v1/issues/body", body: { tree, issue, body: "Specification" } },
        details: {},
      },
      {
        input: { op: "label_add", issue, label: "needs-approval" },
        request: {
          path: "/legion/v1/issues/labels",
          body: { tree, issue, add: ["needs-approval"], remove: [] },
        },
        details: { labels: ["needs-approval"] },
      },
      {
        input: { op: "label_remove", issue, label: "needs-approval" },
        request: {
          path: "/legion/v1/issues/labels",
          body: { tree, issue, add: [], remove: ["needs-approval"] },
        },
        details: { labels: ["needs-approval"] },
      },
      {
        input: { op: "escalate", kind: "capacity", context: { reason: "No slots" } },
        request: {
          path: "/legion/v1/escalate",
          body: { tree, kind: "capacity", context: { reason: "No slots" } },
        },
        details: {},
      },
      {
        input: { op: "request_refile", issue, rationale: "Independent work" },
        request: {
          path: "/legion/v1/escalate",
          body: { tree, kind: "re-file", context: { issue, rationale: "Independent work" } },
        },
        details: {},
      },
      {
        input: { op: "issue_close", issue, comment: "Completed" },
        request: { path: "/legion/v1/issues/close", body: { tree, issue, comment: "Completed" } },
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
  });
  test("withholds architect-only Legion tools until an architect session is confirmed", () => {
    const fixture = createPi();

    legionExtension(fixture.pi);

    expect(fixture.tools).toEqual([]);
  });
  test("fails at load when OMP lacks the required agent revival API", () => {
    const fixture = createPi();
    const pi = { ...fixture.pi, agents: undefined } as unknown as TestPi;

    expect(() => legionExtension(pi)).toThrow("pi.agents is required for Legion");
  });
  test("revives a worker in code, then acknowledges without sending an architect message", async () => {
    const ensured: { readonly agentId: string; readonly parentSessionFile: string }[] = [];
    const fixture = createPi({
      agents: {
        list: () => [],
        get: () => undefined,
        ensureLive: async (agentId, options) => {
          ensured.push({ agentId, parentSessionFile: options.parentSessionFile });
          return { id: agentId };
        },
        prompt: async () => undefined,
      },
    });
    let acknowledgements = 0;

    await handleLegionControlDirective(
      {
        type: "revive-worker",
        role: "implementer",
        agentId: "agent-worker",
        parentSessionFile: "/state/root.jsonl",
        redeliver: { topic: "notifications.role.worker", payload: "{}", eventId: "evt-1" },
      },
      {
        agents: fixture.pi.agents,
        reclaimArchitect: async () => undefined,
        requestShutdown: () => fixture.pi.sendMessage({ type: "shutdown-request" }),
        acknowledge: () => {
          acknowledgements += 1;
        },
        reject: () => {
          throw new Error("unexpected control rejection");
        },
      }
    );

    expect(ensured).toEqual([{ agentId: "agent-worker", parentSessionFile: "/state/root.jsonl" }]);
    expect(acknowledgements).toBe(1);
    expect(fixture.sentMessages).toEqual([]);
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
        agents: fixture.pi.agents,
        reclaimArchitect: async () => {
          reclaims += 1;
        },
        requestShutdown: () => fixture.pi.sendMessage({ type: "shutdown-request" }),
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
  test("nacks a failed worker revival without sending an architect message", async () => {
    const fixture = createPi({
      agents: {
        list: () => [],
        get: () => undefined,
        ensureLive: async () => {
          throw new Error("missing worker transcript");
        },
        prompt: async () => undefined,
      },
    });
    const rejected: string[] = [];
    let acknowledgements = 0;

    await handleLegionControlDirective(
      {
        type: "revive-worker",
        role: "reviewer",
        agentId: "agent-reviewer",
        parentSessionFile: "/state/root.jsonl",
        redeliver: { topic: "notifications.role.reviewer", payload: "{}", eventId: "evt-3" },
      },
      {
        agents: fixture.pi.agents,
        reclaimArchitect: async () => undefined,
        requestShutdown: () => fixture.pi.sendMessage({ type: "shutdown-request" }),
        acknowledge: () => {
          acknowledgements += 1;
        },
        reject: (error) => rejected.push(error),
      }
    );

    expect(rejected).toEqual(["missing worker transcript"]);
    expect(acknowledgements).toBe(0);
    expect(fixture.sentMessages).toEqual([]);
  });
  test("reports root process exit to the daemon on session shutdown", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "owner/repo#42";
    const token = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-process-exit";
    process.env.LEGION_PROJECT = "omp";
    process.env.LEGION_TREE = tree;
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
      body: { tree, generation: 3 },
    });
  });
  test("injects a daemon-minted Legion spawn capability into a named worker task", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const tree = "owner/repo#42";
    const issue = "owner/repo#43";
    const token = roleToken("omp", issue, "reviewer");
    const rootToken = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-injection";
    process.env.LEGION_PROJECT = "omp";
    process.env.LEGION_TREE = tree;
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/process/started") {
        return Response.json({
          roleTokens: { architect: rootToken },
          controlSubject: "legion.ctl.owner-repo-42.3",
          secret: "root-secret",
        });
      }
      if (url.pathname === "/legion/v1/spawn-token") return Response.json({ spawnToken: "spawn-capability" });
      return Response.json({
        session_id: "ses_architect",
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [rootToken],
      });
    }) as typeof fetch;
    const fixture = createPi();
    const context = sessionContext("ses_architect");

    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    const toolCall = fixture.handlers.get("tool_call");
    const toolResult = fixture.handlers.get("tool_result");
    if (sessionStart === undefined || toolCall === undefined || toolResult === undefined) {
      throw new Error("spawn handlers were not registered");
    }
    await sessionStart({}, context);
    const result = await toolCall(
      {
        toolName: "task",
        toolCallId: "spawn-reviewer",
        input: {
          agent: "legion-reviewer",
          task: `Legion-Issue: ${issue}\nReview the implementation`,
        },
      },
      context,
    );

    expect(requests.at(-1)).toEqual({
      path: "/legion/v1/spawn-token",
      body: { tree, issue, role: "reviewer", sessionId: "ses_architect" },
    });
    expect(result).toEqual({
      input: {
        agent: "legion-reviewer",
        task:
          `Legion-Issue: ${issue}\nReview the implementation\n\n` +
          `<legion-spawn issue="${issue}" role="reviewer" token="${token}" tree="${tree}" spawnToken="spawn-capability"/>`,
      },
    });
    await toolResult(
      {
        toolName: "task",
        toolCallId: "spawn-reviewer",
        input: {
          agent: "legion-reviewer",
          task: `Legion-Issue: ${issue}\nReview the implementation`,
        },
        details: {},
        isError: true,
      },
      context
    );
  });
  test("holds six live worker permits through task completion, releases failed spawns, and refuses sub-architects at the depth cap", async () => {
    const tree = "owner/repo#42";
    const issue = "owner/repo#43";
    let spawnNumber = 0;
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_PROJECT = "omp";
    process.env.LEGION_TREE = tree;
    process.env.LEGION_WORKER_BUDGET = "6";
    process.env.LEGION_MAX_RECURSION_DEPTH = "8";
    globalThis.fetch = (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      if (init?.method === "DELETE") return Response.json({});
      if (url.pathname === "/legion/v1/spawn-token") {
        spawnNumber += 1;
        return Response.json({ spawnToken: `spawn-capability-${spawnNumber}` });
      }
      if (url.pathname === "/legion/v1/phase") {
        return Response.json({
          secret: "worker-secret",
          gitName: "Legion Reviewer",
          gitEmail: "reviewer@example.test",
        });
      }
      if (url.pathname === "/legion/v1/role-backing") return Response.json({});
      return Response.json({
        session_id: body?.session_id,
        machine_id: "machine",
        dir: "/tmp/legion-workspace",
        topics: [],
      });
    }) as typeof fetch;
    const fixture = createPi();
    const rootContext = sessionContext("ses_budget_root");

    legionExtension(fixture.pi);
    const toolCall = fixture.handlers.get("tool_call");
    const toolResult = fixture.handlers.get("tool_result");
    const beforeAgentStart = fixture.handlers.get("before_agent_start");
    const sessionShutdown = fixture.handlers.get("session_shutdown");
    if (
      toolCall === undefined ||
      toolResult === undefined ||
      beforeAgentStart === undefined ||
      sessionShutdown === undefined
    ) {
      throw new Error("spawn lifecycle handlers were not registered");
    }
    const workspace = await createJjWorkspace();
    const spawnLiveWorker = async (index: number): Promise<SessionContext> => {
      const spawnCall = {
        toolName: "task",
        toolCallId: `spawn-live-${index}`,
        input: {
          agent: "legion-reviewer",
          task: `Legion-Issue: ${issue}\nReview worker ${index}`,
        },
      };
      const prompt = injectedTask(await toolCall(spawnCall, rootContext));
      const workerContext = { ...sessionContext(`ses_live_${index}`), cwd: workspace };
      await beforeAgentStart({ prompt }, workerContext);
      await toolResult(
        {
          ...spawnCall,
          details: { results: [{ id: `agent-live-${index}`, sessionId: `ses_live_${index}` }] },
          isError: false,
        },
        rootContext
      );
      return workerContext;
    };

    const liveWorkers: SessionContext[] = [];
    for (let index = 0; index < 6; index += 1) {
      liveWorkers.push(await spawnLiveWorker(index));
    }
    let seventhReady = false;
    const seventh = Promise.resolve(
      toolCall(
        {
          toolName: "task",
          toolCallId: "spawn-seventh",
          input: {
            agent: "legion-reviewer",
            task: `Legion-Issue: ${issue}\nWait for capacity`,
          },
        },
        rootContext
      )
    ).then((result: unknown) => {
      seventhReady = true;
      return result;
    });
    await Promise.resolve();
    expect(seventhReady).toBeFalse();

    const [firstWorker, ...remainingWorkers] = liveWorkers;
    if (!firstWorker) throw new Error("expected one live worker");
    await sessionShutdown({}, firstWorker);
    const seventhResult = await seventh;
    expect(injectedTask(seventhResult)).toContain('spawnToken="spawn-capability-7"');
    await toolResult(
      {
        toolName: "task",
        toolCallId: "spawn-seventh",
        input: {
          agent: "legion-reviewer",
          task: `Legion-Issue: ${issue}\nWait for capacity`,
        },
        details: {},
        isError: true,
      },
      rootContext
    );
    await Promise.all(
      remainingWorkers.map((workerContext) => sessionShutdown({}, workerContext))
    );

    process.env.LEGION_WORKER_BUDGET = "1";
    const failedSpawn = await toolCall(
      {
        toolName: "task",
        toolCallId: "spawn-failed",
        input: { agent: "legion-reviewer", task: `Legion-Issue: ${issue}\nThis spawn fails` },
      },
      rootContext
    );
    expect(injectedTask(failedSpawn)).toContain("legion-spawn");
    const spawnedBeforeReplacement = spawnNumber;
    const replacement = Promise.resolve(
      toolCall(
        {
          toolName: "task",
          toolCallId: "spawn-replacement",
          input: { agent: "legion-reviewer", task: `Legion-Issue: ${issue}\nReplacement worker` },
        },
        rootContext
      )
    );
    await Promise.resolve();
    expect(spawnNumber).toBe(spawnedBeforeReplacement);
    await toolResult(
      {
        toolName: "task",
        toolCallId: "spawn-failed",
        input: { agent: "legion-reviewer", task: `Legion-Issue: ${issue}\nThis spawn fails` },
        details: {},
        isError: true,
      },
      rootContext
    );
    expect(injectedTask(await replacement)).toContain("legion-spawn");
    expect(spawnNumber).toBe(spawnedBeforeReplacement + 1);
    await toolResult(
      {
        toolName: "task",
        toolCallId: "spawn-replacement",
        input: { agent: "legion-reviewer", task: `Legion-Issue: ${issue}\nReplacement worker` },
        details: {},
        isError: true,
      },
      rootContext
    );

    process.env.LEGION_WORKER_BUDGET = "6";
    const allowedArchitect = await toolCall(
      {
        toolName: "task",
        toolCallId: "spawn-depth-six",
        input: { agent: "legion-architect", task: `Legion-Issue: ${issue}\nDecompose this work` },
      },
      { ...rootContext, taskDepth: 6 }
    );
    expect(injectedTask(allowedArchitect)).toContain("legion-spawn");
    await toolResult(
      {
        toolName: "task",
        toolCallId: "spawn-depth-six",
        input: { agent: "legion-architect", task: `Legion-Issue: ${issue}\nDecompose this work` },
        details: {},
        isError: true,
      },
      rootContext
    );
    await expect(
      toolCall(
        {
          toolName: "task",
          toolCallId: "spawn-depth-seven",
          input: { agent: "legion-architect", task: `Legion-Issue: ${issue}\nDecompose this work` },
        },
        { ...rootContext, taskDepth: 7 }
      )
    ).resolves.toEqual({
      block: true,
      reason:
        "sub-architect at depth 7 would place its workers at the recursion cap (8); escalate to your parent architect instead",
    });
    await expect(
      toolCall(
        {
          toolName: "task",
          toolCallId: "missing-issue",
          input: { agent: "legion-reviewer", task: "Review the implementation" },
        },
        rootContext
      )
    ).resolves.toEqual({
      block: true,
      reason: "legion spawns must name their issue: Legion-Issue: owner/repo#n",
    });
  });
  test("blocks code tools in a root architect session", async () => {
    const tree = "owner/repo#42";
    const architectToken = roleToken("omp", tree, "architect");
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_GENERATION = "3";
    process.env.LEGION_BOOT_TOKEN = "boot-architect-policy";
    process.env.LEGION_PROJECT = "omp";
    process.env.LEGION_TREE = tree;
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
});
