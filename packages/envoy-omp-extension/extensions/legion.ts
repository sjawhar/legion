import path from "node:path";
import {
  controllerToken,
  formatIssueKey,
  type IssueKey,
  LEGION_ROLES,
  type LegionRole,
  parseIssueKey,
  parseRoleToken,
  roleToken,
} from "@legion/contracts";
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults";
import {
  provisionIssueWorkspace,
  type RunResult,
  type WorkspaceCommandOptions,
  type WorkspaceSpec,
} from "@legion/workspace";
import { connect, type NatsConnection, StringCodec, type Subscription } from "nats";
import { createLegionDaemonClient } from "../src/legion/daemon-client";
import { claimEnvoyRole, type EnvoySessionContext } from "./envoy";

export type LegionSessionKind =
  | { kind: "root-architect"; tree: string }
  | { kind: "controller" }
  | { kind: "phase-worker"; role: LegionRole }
  | { kind: "sub-architect" }
  | { kind: "not-legion" };

export function classifySession(
  env: NodeJS.ProcessEnv,
  agentName: string | undefined,
  taskDepth: number
): LegionSessionKind {
  if (env.LEGION_CONTROLLER !== undefined && env.LEGION_TREE !== undefined) {
    throw new Error("Legion session has both controller and tree launch markers");
  }

  if (env.LEGION_CONTROLLER === "1") return { kind: "controller" };

  if (taskDepth === 0 && env.LEGION_TREE) {
    return { kind: "root-architect", tree: env.LEGION_TREE };
  }

  if (agentName === "legion-architect" && taskDepth >= 1) {
    return { kind: "sub-architect" };
  }

  const role = LEGION_ROLES.find((candidate) => agentName === `legion-${candidate}`);
  if (role && role !== "architect") return { kind: "phase-worker", role };

  return { kind: "not-legion" };
}

type SessionContext = EnvoySessionContext & {
  readonly taskDepth?: number;
  readonly sessionManager: {
    readonly getSessionId: () => string;
    readonly getSessionFile: () => string | undefined;
  };
};

type BeforeAgentStartEvent = { readonly prompt: string };
type ToolCallEvent = {
  readonly toolName: string;
  readonly toolCallId?: string;
  readonly input: Record<string, unknown>;
};

type ToolResultEvent = {
  readonly toolName: string;
  readonly toolCallId: string;
  readonly input: Record<string, unknown>;
  readonly details: unknown;
  readonly isError: boolean;
};

type ToolCallEventResult = {
  readonly block?: boolean;
  readonly reason?: string;
  readonly input?: Record<string, unknown>;
};
type RootBootstrap = { readonly role: string; readonly secret: string; readonly agentId: string };

const workerBudgetPermits = new Map<string, () => void>();

export function registerWorkerBudgetPermit(sessionID: string, release: () => void): void {
  if (workerBudgetPermits.has(sessionID)) {
    throw new Error(`Legion worker budget permit already registered for ${sessionID}`);
  }
  workerBudgetPermits.set(sessionID, release);
}

function releaseWorkerBudgetPermit(sessionID: string): void {
  const release = workerBudgetPermits.get(sessionID);
  if (!release) return;
  workerBudgetPermits.delete(sessionID);
  release();
}
type WorkerSession = {
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly token: string;
  readonly spawnToken?: string;
  readonly secret: string;
  readonly agentId: string;
};

type WorkerSpawn = {
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly token: string;
  readonly spawnToken: string;
  readonly workspace: string;
};

type PendingLegionSpawn = {
  readonly toolCallId: string;
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly token: string;
  readonly spawnToken: string;
  readonly release: () => void;
};

const pendingLegionSpawns = new Map<string, Set<PendingLegionSpawn>>();
const pendingLegionSpawnsByToken = new Map<string, PendingLegionSpawn>();
const rootBootstraps = new Map<string, Promise<RootBootstrap>>();
let workerBudgetLimit: number | undefined;
let workerBudgetInUse = 0;
const workerBudgetWaiters: (() => void)[] = [];

async function acquireWorkerBudget(limit: number): Promise<() => void> {
  if (workerBudgetLimit === undefined) workerBudgetLimit = limit;
  if (workerBudgetLimit !== limit && workerBudgetInUse === 0 && workerBudgetWaiters.length === 0) {
    workerBudgetLimit = limit;
  }
  if (workerBudgetInUse >= workerBudgetLimit) {
    await new Promise<void>((resolve) => workerBudgetWaiters.push(resolve));
  } else {
    workerBudgetInUse += 1;
  }
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const waiter = workerBudgetWaiters.shift();
    if (waiter) {
      waiter();
      return;
    }
    workerBudgetInUse -= 1;
  };
}
const workerSessions = new Map<string, WorkerSession>();
function addPendingLegionSpawn(pending: PendingLegionSpawn): void {
  const forToolCall = pendingLegionSpawns.get(pending.toolCallId) ?? new Set<PendingLegionSpawn>();
  forToolCall.add(pending);
  pendingLegionSpawns.set(pending.toolCallId, forToolCall);
  pendingLegionSpawnsByToken.set(pending.spawnToken, pending);
}

function removePendingLegionSpawn(pending: PendingLegionSpawn): void {
  const forToolCall = pendingLegionSpawns.get(pending.toolCallId);
  if (forToolCall) {
    forToolCall.delete(pending);
    if (forToolCall.size === 0) pendingLegionSpawns.delete(pending.toolCallId);
  }
  pendingLegionSpawnsByToken.delete(pending.spawnToken);
}

function releasePendingLegionSpawn(pending: PendingLegionSpawn): void {
  removePendingLegionSpawn(pending);
  pending.release();
}

function transferPendingLegionSpawn(pending: PendingLegionSpawn): () => void {
  removePendingLegionSpawn(pending);
  return pending.release;
}

type CommandContext = {
  readonly cwd: string;
  readonly sessionManager: { readonly getSessionId: () => string };
};

type ToolResult = {
  readonly content: readonly { readonly type: "text"; readonly text: string }[];
  readonly details: Readonly<Record<string, unknown>>;
  readonly isError?: boolean;
};

type RegisteredTool = {
  readonly name: string;
  readonly label: string;
  readonly description: string;
  readonly defaultInactive: boolean;
  readonly parameters: unknown;
  readonly execute: (
    id: string,
    parameters: Record<string, unknown>,
    signal: AbortSignal | undefined,
    onUpdate: unknown,
    context: SessionContext
  ) => Promise<ToolResult>;
};

type ZodProperty = { readonly optional: () => unknown };

type ExtensionAgentsApi = {
  readonly list: () => readonly { readonly id: string }[];
  readonly get: (agentId: string) => { readonly id: string } | undefined;
  readonly ensureLive: (
    agentId: string,
    options: { readonly parentSessionFile: string }
  ) => Promise<{ readonly id: string }>;
  readonly prompt: (agentId: string, content: string) => Promise<void>;
};

type PiApi = {
  readonly zod: {
    readonly object: (shape: Readonly<Record<string, unknown>>) => unknown;
    readonly string: () => ZodProperty;
    readonly array: (item: unknown) => ZodProperty;
    readonly enum: (values: readonly string[]) => ZodProperty;
    readonly unknown: () => ZodProperty;
  };
  readonly agents?: ExtensionAgentsApi;
  readonly sendMessage: (message: { readonly type: string }) => void;
  readonly getActiveTools: () => readonly string[];
  readonly setActiveTools: (tools: string[]) => Promise<void>;
  readonly on: (
    event:
      | "session_start"
      | "before_agent_start"
      | "tool_call"
      | "tool_result"
      | "session_shutdown",
    handler: (event: unknown, context: SessionContext) => Promise<unknown>
  ) => void;
  readonly registerTool: (tool: RegisteredTool) => void;
  readonly registerCommand: (
    name: string,
    command: {
      readonly description: string;
      readonly handler: (args: string, context: CommandContext) => Promise<void>;
    }
  ) => void;
};

function requiredEnvironment(env: NodeJS.ProcessEnv, key: string): string {
  const value = env[key];
  if (!value) throw new Error(`${key} is required for Legion`);
  return value;
}

function requiredControllerCapability(env: NodeJS.ProcessEnv): string {
  const secret = env.LEGION_CONTROLLER_SECRET;
  if (!secret) {
    throw new Error(
      "LEGION_CONTROLLER_SECRET is required to claim the controller. Launch OMP with LEGION_CONTROLLER_SECRET in its environment before running /legion-claim-controller."
    );
  }
  return secret;
}

function generation(env: NodeJS.ProcessEnv): number {
  const value = Number(requiredEnvironment(env, "LEGION_GENERATION"));
  if (!Number.isSafeInteger(value)) throw new Error("LEGION_GENERATION must be an integer");
  return value;
}
function isRootSession(env: NodeJS.ProcessEnv, context: SessionContext): boolean {
  const rootWorkspace = env.LEGION_ROOT_WORKSPACE;
  return rootWorkspace ? context.cwd === rootWorkspace : context.taskDepth === 0;
}


function parseWorkerSpawn(prompt: string, project: string): WorkerSpawn | undefined {
  const block = /<legion-spawn\s+([^>]*?)\/>/.exec(prompt);
  if (!block) return undefined;

  const attributes = new Map<string, string>();
  const attributePattern = /([A-Za-z]+)="([^"]*)"/g;
  for (const attribute of block[1].matchAll(attributePattern)) {
    const key = attribute[1];
    const value = attribute[2];
    if (!key || value === undefined || attributes.has(key)) return undefined;
    attributes.set(key, value);
  }
  if (block[1].replace(attributePattern, "").trim() !== "") return undefined;

  const issue = attributes.get("issue");
  const role = attributes.get("role");
  const token = attributes.get("token");
  const tree = attributes.get("tree");
  const spawnToken = attributes.get("spawnToken") ?? token;
  const workspace = attributes.get("workspace");
  if (
    !issue ||
    !role ||
    !token ||
    !tree ||
    !spawnToken ||
    !workspace ||
    !LEGION_ROLES.includes(role as LegionRole)
  )
    return undefined;

  const parsedToken = parseRoleToken(project, token);
  if (
    !parsedToken ||
    "controller" in parsedToken ||
    parsedToken.issue !== issue ||
    parsedToken.role !== role
  )
    return undefined;
  return { tree, issue, role, token, spawnToken, workspace };
}
function workerAgentId(context: SessionContext): string {
  const sessionFile = context.sessionManager.getSessionFile();
  if (!sessionFile?.endsWith(".jsonl")) {
    throw new Error("Legion worker session must have a persisted transcript");
  }
  const agentId = path.basename(sessionFile, ".jsonl");
  if (!agentId) throw new Error("Legion worker transcript has no agent id");
  return agentId;
}

async function setJjIdentity(cwd: string, gitName: string, gitEmail: string): Promise<void> {
  for (const [key, value] of [
    ["user.name", gitName],
    ["user.email", gitEmail],
  ] as const) {
    const child = Bun.spawn(["jj", "config", "set", "--repo", key, JSON.stringify(value)], {
      cwd,
      stdout: "ignore",
      stderr: "pipe",
    });
    const exitCode = await child.exited;
    if (exitCode === 0) continue;
    const stderr = await new Response(child.stderr as ReadableStream<Uint8Array>).text();
    throw new Error(`jj config set ${key} failed: ${stderr.trim()}`);
  }
}
async function deleteEnvoyInterest(baseUrl: string, sessionID: string): Promise<void> {
  const response = await fetch(`${baseUrl}/v1/interests/${encodeURIComponent(sessionID)}`, {
    method: "DELETE",
  });
  const responseBody = await response.text();
  if (!response.ok) {
    throw new Error(
      `DELETE /v1/interests/${sessionID} failed with ${response.status}: ${responseBody}`
    );
  }
}

async function runWorkspaceCommand(
  cmd: string[],
  opts?: WorkspaceCommandOptions
): Promise<RunResult> {
  const child = Bun.spawn(cmd, {
    ...(opts?.cwd === undefined ? {} : { cwd: opts.cwd }),
    ...(opts?.env === undefined ? {} : { env: { ...process.env, ...opts.env } }),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout as ReadableStream<Uint8Array>).text(),
    new Response(child.stderr as ReadableStream<Uint8Array>).text(),
  ]);
  return { exitCode, stdout, stderr };
}

function toolSuccess(details: Readonly<Record<string, unknown>>): ToolResult {
  return { content: [{ type: "text", text: JSON.stringify(details) }], details };
}

function toolFailure(error: unknown): ToolResult {
  const message = error instanceof Error ? error.message : String(error);
  return { content: [{ type: "text", text: message }], details: {}, isError: true };
}

const LEGION_TOOL_LABELS = [
  "needs-approval",
  "human-approved",
  "legion-child",
  "legion-backlog",
] as const;

function legionToolSchema(pi: PiApi): unknown {
  const z = pi.zod;
  return z.object({
    op: z.enum([
      "issue_create",
      "wave_release",
      "comment",
      "post_spec",
      "label_add",
      "label_remove",
      "escalate",
      "request_refile",
      "issue_close",
    ]),
    title: z.string().optional(),
    body: z.string().optional(),
    labels: z.array(z.enum(LEGION_TOOL_LABELS)).optional(),
    children: z.array(z.string()).optional(),
    issue: z.string().optional(),
    label: z.enum(LEGION_TOOL_LABELS).optional(),
    kind: z.enum(["re-file", "capacity", "cross-tree"]).optional(),
    context: z.unknown().optional(),
    rationale: z.string().optional(),
    comment: z.string().optional(),
  });
}

function envoyDispatchToolSchema(pi: PiApi): unknown {
  const z = pi.zod;
  return z.object({
    parent: z.string(),
    subject: z.string(),
    body: z.string(),
    ask: z.array(z.unknown()).optional(),
    urgency: z.enum(["low", "med", "high", "blocking"]).optional(),
  });
}

type Redelivery = { readonly topic: string; readonly payload: string; readonly eventId: string };

export type LegionControlDirective =
  | {
      readonly type: "revive-worker";
      readonly role: LegionRole;
      readonly agentId: string;
      readonly parentSessionFile: string;
      readonly redeliver: Redelivery;
    }
  | { readonly type: "reclaim-architect"; readonly redeliver: Redelivery }
  | { readonly type: "shutdown" };

type LegionControlActions = {
  readonly agents?: ExtensionAgentsApi;
  readonly reclaimArchitect: () => Promise<void>;
  readonly requestShutdown: () => void;
  readonly acknowledge: () => void;
  readonly reject: (error: string) => void;
};

export async function handleLegionControlDirective(
  directive: LegionControlDirective,
  actions: LegionControlActions
): Promise<void> {
  try {
    switch (directive.type) {
      case "revive-worker":
        if (!actions.agents) {
          throw new Error("reviving Legion workers requires an OMP host with pi.agents");
        }
        await actions.agents.ensureLive(directive.agentId, {
          parentSessionFile: directive.parentSessionFile,
        });
        break;
      case "reclaim-architect":
        await actions.reclaimArchitect();
        break;
      case "shutdown":
        actions.requestShutdown();
        break;
    }
    actions.acknowledge();
  } catch (error) {
    actions.reject(error instanceof Error ? error.message : String(error));
  }
}

function parseControlDirective(raw: string): LegionControlDirective {
  const payload: unknown = JSON.parse(raw);
  if (typeof payload !== "object" || payload === null || !("type" in payload)) {
    throw new Error("Legion control directive must be an object with a type");
  }
  const redelivery = (): Redelivery => {
    if (
      !("redeliver" in payload) ||
      typeof payload.redeliver !== "object" ||
      payload.redeliver === null ||
      !("topic" in payload.redeliver) ||
      typeof payload.redeliver.topic !== "string" ||
      !("payload" in payload.redeliver) ||
      typeof payload.redeliver.payload !== "string" ||
      !("eventId" in payload.redeliver) ||
      typeof payload.redeliver.eventId !== "string"
    ) {
      throw new Error("Legion control directive is missing redelivery metadata");
    }
    return {
      topic: payload.redeliver.topic,
      payload: payload.redeliver.payload,
      eventId: payload.redeliver.eventId,
    };
  };
  if (payload.type === "shutdown") return { type: "shutdown" };
  if (payload.type === "reclaim-architect")
    return { type: "reclaim-architect", redeliver: redelivery() };
  if (
    payload.type !== "revive-worker" ||
    !("role" in payload) ||
    typeof payload.role !== "string" ||
    !LEGION_ROLES.includes(payload.role as LegionRole) ||
    !("agentId" in payload) ||
    typeof payload.agentId !== "string" ||
    !("parentSessionFile" in payload) ||
    typeof payload.parentSessionFile !== "string"
  ) {
    throw new Error("Invalid Legion control directive");
  }
  return {
    type: "revive-worker",
    role: payload.role as LegionRole,
    agentId: payload.agentId,
    parentSessionFile: payload.parentSessionFile,
    redeliver: redelivery(),
  };
}

export default function legionExtension(pi: PiApi): void {
  const agents = pi.agents;
  const defaults = envoyDefaultsFromEnvironment(process.env);
  let rootSessionID: string | undefined;
  let rootArchitectRole: string | undefined;
  let rootSecret: string | undefined;
  let rootAgentId: string | undefined;
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let controlConnection: NatsConnection | undefined;
  let controlSubscription: Subscription | undefined;
  const controlCodec = StringCodec();
  let workspaceStateDir: string | undefined;
  const workspaceProvisions = new Map<IssueKey, Promise<WorkspaceSpec>>();
  const roleDaemon = () => {
    return createLegionDaemonClient(
      requiredEnvironment(process.env, "LEGION_DAEMON_URL"),
      fetch,
      {
        agentId: (sessionId) => {
          if (sessionId === rootSessionID) {
            if (rootAgentId === undefined) {
              throw new Error("Legion root architect has no transcript-derived agent id");
            }
            return rootAgentId;
          }
          const worker = workerSessions.get(sessionId);
          if (worker === undefined) {
            throw new Error(`Legion session ${sessionId} has no registered role backing`);
          }
          return worker.agentId;
        },
        onRecovered: (sessionId, recovered) => {
          if (sessionId === rootSessionID) {
            const tree = requiredEnvironment(process.env, "LEGION_TREE");
            if (
              recovered.tree !== tree ||
              recovered.issue !== tree ||
              recovered.role !== "architect"
            ) {
              throw new Error("Daemon recovered a capability for a different root architect role");
            }
            rootSecret = recovered.secret;
            return;
          }
          const worker = workerSessions.get(sessionId);
          if (
            worker === undefined ||
            recovered.tree !== worker.tree ||
            recovered.issue !== worker.issue ||
            recovered.role !== worker.role
          ) {
            throw new Error("Daemon recovered a capability for a different Legion worker role");
          }
          workerSessions.set(sessionId, { ...worker, secret: recovered.secret });
        },
      }
    );
  };
  const provisionWorkspace = async (
    issue: IssueKey,
    capability: { readonly tree: string; readonly sessionId: string; readonly secret: string }
  ): Promise<WorkspaceSpec> => {
    if (workspaceStateDir === undefined) {
      throw new Error("Legion workspace state directory is unavailable before root bootstrap");
    }
    const existing = workspaceProvisions.get(issue);
    if (existing !== undefined) return await existing;

    const provision = provisionIssueWorkspace(issue, {
      extensionPackage: path.resolve(import.meta.dir, ".."),
      stateDir: workspaceStateDir,
      credentialHelper: requiredEnvironment(process.env, "LEGION_CREDENTIAL_HELPER"),
      provisioningToken: async () =>
        (
          await roleDaemon().provisioningCredential({
            tree: capability.tree,
            issue,
            sessionId: capability.sessionId,
            secret: capability.secret,
          })
        ).token,
      run: runWorkspaceCommand,
    }).catch((error: unknown) => {
      workspaceProvisions.delete(issue);
      throw error;
    });
    workspaceProvisions.set(issue, provision);
    return await provision;
  };
  const claimRole = async (
    sessionID: string,
    role: string,
    context?: EnvoySessionContext
  ): Promise<void> => {
    await claimEnvoyRole(sessionID, role, context);
  };

  const claimController = async (context: CommandContext | SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    const daemon = createLegionDaemonClient(requiredEnvironment(process.env, "LEGION_DAEMON_URL"));
    const capability = controllerCapability ?? requiredControllerCapability(process.env);
    controllerCapability = capability;
    const { project } = await daemon.state();
    await claimRole(
      sessionID,
      controllerToken(project),
      "setInterval" in context ? context : undefined
    );
    await daemon.controllerReady({ secret: capability, sessionId: sessionID });
    controllerSessionID = sessionID;
  };

  const reclaimArchitect = async (): Promise<void> => {
    if (!rootSessionID || !rootArchitectRole) {
      throw new Error("Legion root architect is not available for reclamation");
    }
    await claimRole(rootSessionID, rootArchitectRole);
  };

  const startControlSubscription = async (): Promise<void> => {
    const subject = process.env.LEGION_CONTROL_SUBJECT;
    if (!subject || controlSubscription) return;
    if (defaults.natsUrls.length === 0) {
      throw new Error("ENVOY_NATS_URL is required for Legion control directives");
    }
    const connection = await connect({
      servers: [...defaults.natsUrls],
      name: `legion-control-${rootSessionID ?? "unknown"}`,
      reconnect: true,
      maxReconnectAttempts: -1,
      reconnectTimeWait: 2_000,
    });
    const subscription = connection.subscribe(subject);
    controlConnection = connection;
    controlSubscription = subscription;
    void (async () => {
      for await (const message of subscription) {
        const reply = message.reply;
        let directive: LegionControlDirective;
        try {
          directive = parseControlDirective(controlCodec.decode(message.data));
        } catch (error) {
          if (reply) {
            connection.publish(
              reply,
              controlCodec.encode(
                JSON.stringify({
                  type: "nack",
                  error: error instanceof Error ? error.message : String(error),
                })
              )
            );
          }
          continue;
        }
        await handleLegionControlDirective(directive, {
          agents,
          reclaimArchitect,
          requestShutdown: () => pi.sendMessage({ type: "shutdown-request" }),
          acknowledge: () => {
            if (reply)
              connection.publish(reply, controlCodec.encode(JSON.stringify({ type: "ack" })));
          },
          reject: (error) => {
            if (reply)
              connection.publish(
                reply,
                controlCodec.encode(JSON.stringify({ type: "nack", error }))
              );
          },
        });
      }
    })();
  };

  const bootstrapRoot = async (context: SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    const tree = process.env.LEGION_TREE;
    if (!tree || (rootSessionID !== undefined && rootSessionID !== sessionID)) return;

    const bootToken = requiredEnvironment(process.env, "LEGION_BOOT_TOKEN");
    if (rootBootstraps.has(bootToken)) return;
    const sessionFile = context.sessionManager.getSessionFile();
    if (!sessionFile) throw new Error("Legion root session must be persisted");
    const agentId = workerAgentId(context);

    const bootstrap = (async (): Promise<RootBootstrap> => {
      const started = await createLegionDaemonClient(
        requiredEnvironment(process.env, "LEGION_DAEMON_URL")
      ).processStarted({
        tree,
        generation: generation(process.env),
        bootToken,
        rootSessionId: sessionID,
        agentId,
        ompSessionFile: sessionFile,
      });
      workspaceStateDir = requiredEnvironment(process.env, "LEGION_STATE_DIR");
      const role = started.roleTokens.architect;
      if (!role) throw new Error("Legion daemon did not return an architect role token");
      return { role, secret: started.secret, agentId };
    })();
    rootBootstraps.set(bootToken, bootstrap);
    try {
      const root = await bootstrap;
      rootSessionID = sessionID;
      rootArchitectRole = root.role;
      rootSecret = root.secret;
      rootAgentId = root.agentId;
      await claimRole(sessionID, root.role, context);
      await startControlSubscription();
      await roleDaemon().processReady({
        tree,
        sessionId: sessionID,
        secret: root.secret,
      });
      registerArchitectTools();
      await activateLegionTool();
    } catch (error) {
      if (rootBootstraps.get(bootToken) === bootstrap) rootBootstraps.delete(bootToken);
      throw error;
    }
  };

  pi.on("session_start", async (_event, context) => {
    const sessionID = context.sessionManager.getSessionId();
    if (process.env.LEGION_CONTROLLER === "1") {
      if (controllerSessionID === undefined || controllerSessionID === sessionID)
        await claimController(context);
      return;
    }
    let worker = workerSessions.get(sessionID);
    if (
      !worker &&
      process.env.LEGION_TREE &&
      (context.taskDepth ?? 0) > 0 &&
      !isRootSession(process.env, context)
    ) {
      const agentId = workerAgentId(context);
      const recovered = await createLegionDaemonClient(
        requiredEnvironment(process.env, "LEGION_DAEMON_URL")
      ).workerSession({ sessionId: sessionID, agentId });
      const parsedTree = parseIssueKey(recovered.tree);
      const parsedIssue = parseIssueKey(recovered.issue);
      if (!parsedTree || !parsedIssue)
        throw new Error("Legion daemon returned an invalid worker session");
      const tree = formatIssueKey(parsedTree.owner, parsedTree.repo, parsedTree.number);
      const issue = formatIssueKey(parsedIssue.owner, parsedIssue.repo, parsedIssue.number);
      worker = {
        ...recovered,
        tree,
        issue,
        token: roleToken(requiredEnvironment(process.env, "LEGION_PROJECT"), issue, recovered.role),
        agentId,
      };
      workerSessions.set(sessionID, worker);
      await claimRole(sessionID, worker.token, context);
      return;
    }
    if (worker) {
      if (!worker.spawnToken) {
        await claimRole(sessionID, worker.token, context);
        return;
      }
      const release = await acquireWorkerBudget(Number(process.env.LEGION_WORKER_BUDGET ?? "6"));
      try {
        await createLegionDaemonClient(
          requiredEnvironment(process.env, "LEGION_DAEMON_URL")
        ).roleBacking({
          tree: worker.tree,
          issue: worker.issue,
          role: worker.role,
          agentId: worker.agentId,
          sessionId: sessionID,
          spawnToken: worker.spawnToken,
        });
        await claimRole(sessionID, worker.token, context);
        registerWorkerBudgetPermit(sessionID, release);
      } catch (error) {
        release();
        throw error;
      }
      return;
    }
    if (isRootSession(process.env, context)) await bootstrapRoot(context);
  });

  pi.on("before_agent_start", async (event, context) => {
    const sessionID = context.sessionManager.getSessionId();
    if (workerSessions.has(sessionID)) return;
    const project = process.env.LEGION_PROJECT;
    const spawn = project
      ? parseWorkerSpawn((event as BeforeAgentStartEvent).prompt, project)
      : undefined;
    if (!spawn) {
      await bootstrapRoot(context);
      return;
    }

    const pending = pendingLegionSpawnsByToken.get(spawn.spawnToken);
    const release = pending
      ? transferPendingLegionSpawn(pending)
      : await acquireWorkerBudget(Number(process.env.LEGION_WORKER_BUDGET ?? "6"));
    try {
      const agentId = workerAgentId(context);
      const daemon = createLegionDaemonClient(
        requiredEnvironment(process.env, "LEGION_DAEMON_URL")
      );
      await daemon.roleBacking({
        tree: spawn.tree,
        issue: spawn.issue,
        role: spawn.role,
        agentId,
        sessionId: sessionID,
        spawnToken: spawn.spawnToken,
      });
      const phase = await daemon.phase({
        tree: spawn.tree,
        issue: spawn.issue,
        phase: spawn.role,
        spawnToken: spawn.spawnToken,
        sessionId: sessionID,
      });
      await setJjIdentity(spawn.workspace, phase.gitName, phase.gitEmail);
      workerSessions.set(sessionID, {
        tree: spawn.tree,
        issue: spawn.issue,
        role: spawn.role,
        token: spawn.token,
        spawnToken: spawn.spawnToken,
        agentId,
        secret: phase.secret,
      });
      registerWorkerBudgetPermit(sessionID, release);
      if (spawn.role === "architect") {
        registerArchitectTools();
        await activateLegionTool();
      }
      await claimRole(sessionID, spawn.token, context);
    } catch (error) {
      workerSessions.delete(sessionID);
      release();
      throw error;
    }
  });

  pi.on("tool_call", async (event, context): Promise<ToolCallEventResult | undefined> => {
    const toolCall = event as ToolCallEvent;
    const sessionID = context.sessionManager.getSessionId();
    if (
      sessionID === rootSessionID &&
      ["edit", "write", "bash", "apply_patch"].includes(toolCall.toolName)
    ) {
      return { block: true, reason: "the architect delegates all code work to phase workers" };
    }
    if (toolCall.toolName === "task") {
      const injectLegionSpawn = async (
        taskInput: Record<string, unknown>
      ): Promise<Record<string, unknown> | undefined> => {
        const agent = taskInput.agent;
        if (typeof agent !== "string" || !agent.startsWith("legion-")) return undefined;
        const role = agent.slice("legion-".length) as LegionRole;
        const task = taskInput.task;
        if (!LEGION_ROLES.includes(role) || typeof task !== "string") return undefined;
        const issueText = task.split(/\r?\n/, 1)[0]?.slice("Legion-Issue: ".length);
        const parsedIssue =
          issueText && task.startsWith("Legion-Issue: ") ? parseIssueKey(issueText) : undefined;
        const issue = parsedIssue
          ? formatIssueKey(parsedIssue.owner, parsedIssue.repo, parsedIssue.number)
          : undefined;
        if (!issue) {
          throw new Error("legion spawns must name their issue: Legion-Issue: owner/repo#n");
        }
        if (!toolCall.toolCallId) throw new Error("Legion task spawn is missing a tool call id");
        const architect = architectSession(context);
        const workspace = await provisionWorkspace(issue, {
          tree: architect.tree,
          sessionId: sessionID,
          secret: architect.secret,
        });
        const tree = requiredEnvironment(process.env, "LEGION_TREE");
        const depth = context.taskDepth ?? 0;
        const maxDepth = Number(process.env.LEGION_MAX_RECURSION_DEPTH ?? "8");
        if (role === "architect" && depth + 2 > maxDepth) {
          throw new Error(
            `sub-architect at depth ${depth} would place its workers at the recursion cap (${maxDepth}); escalate to your parent architect instead`
          );
        }
        const release = await acquireWorkerBudget(Number(process.env.LEGION_WORKER_BUDGET ?? "6"));
        try {
          const token = roleToken(requiredEnvironment(process.env, "LEGION_PROJECT"), issue, role);
          const spawn = await roleDaemon().spawnToken({
            tree,
            issue,
            role,
            sessionId: sessionID,
            secret: architect.secret,
          });
          const pending: PendingLegionSpawn = {
            toolCallId: toolCall.toolCallId,
            tree,
            issue,
            role,
            token,
            spawnToken: spawn.spawnToken,
            release,
          };
          addPendingLegionSpawn(pending);
          return {
            ...taskInput,
            task: `${task}\n\n<legion-spawn issue="${issue}" role="${role}" token="${token}" tree="${tree}" spawnToken="${spawn.spawnToken}" workspace="${workspace.workspaceDir}"/>`,
          };
        } catch (error) {
          release();
          throw error;
        }
      };
      try {
        if (Array.isArray(toolCall.input.tasks)) {
          let injected = false;
          const tasks: unknown[] = [];
          for (const task of toolCall.input.tasks) {
            if (typeof task !== "object" || task === null || Array.isArray(task)) {
              tasks.push(task);
              continue;
            }
            const rewritten = await injectLegionSpawn(task as Record<string, unknown>);
            if (rewritten) injected = true;
            tasks.push(rewritten ?? task);
          }
          if (injected) return { input: { ...toolCall.input, tasks } };
        } else {
          const rewritten = await injectLegionSpawn(toolCall.input);
          if (rewritten) return { input: rewritten };
        }
      } catch (error) {
        return { block: true, reason: error instanceof Error ? error.message : String(error) };
      }
    }
    if (toolCall.toolName !== "bash" || typeof toolCall.input.command !== "string")
      return undefined;
    const worker = workerSessions.get(sessionID);
    if (!worker) {
      if (
        process.env.LEGION_TREE &&
        process.env.LEGION_CONTROLLER !== "1" &&
        sessionID !== rootSessionID
      ) {
        return {
          block: true,
          reason: "Legion worker session is not registered; cannot mint LEGION_GRANT",
        };
      }
      return undefined;
    }
    try {
      const grant = await roleDaemon().grant({
        tree: worker.tree,
        issue: worker.issue,
        sessionId: sessionID,
        secret: worker.secret,
      });
      return {
        input: {
          ...toolCall.input,
          command: `export LEGION_GRANT=${grant.grantId}\n${toolCall.input.command}`,
        },
      };
    } catch (error) {
      return { block: true, reason: error instanceof Error ? error.message : String(error) };
    }
  });
  pi.on("tool_result", async (event) => {
    const result = event as ToolResultEvent;
    const pending = pendingLegionSpawns.get(result.toolCallId);
    if (!pending || !result.isError) return;
    for (const spawn of [...pending]) releasePendingLegionSpawn(spawn);
  });

  pi.on("session_shutdown", async (_event, context) => {
    const sessionID = context.sessionManager.getSessionId();
    if (sessionID === rootSessionID && process.env.LEGION_TREE) {
      try {
        const architect = architectSession(context);
        await roleDaemon().processExit({
          tree: architect.tree,
          generation: generation(process.env),
          sessionId: sessionID,
          secret: architect.secret,
        });
      } finally {
        controlSubscription?.unsubscribe();
        controlSubscription = undefined;
        await controlConnection?.close();
        controlConnection = undefined;
      }
      return;
    }
    if (!workerSessions.has(sessionID)) return;
    try {
      await deleteEnvoyInterest(defaults.envoyUrl, sessionID);
    } finally {
      releaseWorkerBudgetPermit(sessionID);
    }
  });

  const architectSession = (
    context: SessionContext
  ): { tree: string; issue: string; role: LegionRole; secret: string } => {
    const sessionID = context.sessionManager.getSessionId();
    if (sessionID === rootSessionID && process.env.LEGION_TREE) {
      if (!rootSecret) throw new Error("Legion root architect capability is unavailable");
      return {
        tree: process.env.LEGION_TREE,
        issue: process.env.LEGION_TREE,
        role: "architect",
        secret: rootSecret,
      };
    }
    const worker = workerSessions.get(sessionID);
    if (worker?.role === "architect") {
      return { tree: worker.tree, issue: worker.issue, role: worker.role, secret: worker.secret };
    }
    throw new Error("legion is available only to root and sub-architect sessions");
  };

  const activateLegionTool = async (): Promise<void> => {
    const activeTools = pi.getActiveTools();
    if (activeTools.includes("legion")) return;
    await pi.setActiveTools([...activeTools, "legion"]);
  };

  let architectToolsRegistered = false;
  const registerArchitectTools = (): void => {
    if (architectToolsRegistered) return;
    architectToolsRegistered = true;
    pi.registerTool({
      name: "legion",
      label: "legion",
      description: "Perform a Legion lifecycle write through the Legion daemon.",
      defaultInactive: true,
      parameters: legionToolSchema(pi),
      execute: async (_id, parameters, _signal, _onUpdate, context) => {
        try {
          const architect = architectSession(context);
          const daemon = roleDaemon();
          const stringInput = (name: string): string => {
            const value = parameters[name];
            if (typeof value !== "string")
              throw new Error(`${String(parameters.op)} requires ${name}`);
            return value;
          };
          switch (parameters.op) {
            case "issue_create": {
              const labels = parameters.labels;
              if (
                labels !== undefined &&
                (!Array.isArray(labels) ||
                  labels.some(
                    (label) =>
                      typeof label !== "string" ||
                      !LEGION_TOOL_LABELS.includes(label as (typeof LEGION_TOOL_LABELS)[number])
                  ))
              ) {
                throw new Error("issue_create labels must use surviving Legion labels");
              }
              return toolSuccess(
                await daemon.issueCreate({
                  tree: architect.tree,
                  sessionId: context.sessionManager.getSessionId(),
                  secret: architect.secret,
                  title: stringInput("title"),
                  body: stringInput("body"),
                  labels: labels ?? [],
                })
              );
            }
            case "wave_release": {
              const children = parameters.children;
              if (!Array.isArray(children) || children.some((child) => typeof child !== "string")) {
                throw new Error("wave_release requires children");
              }
              return toolSuccess(
                await daemon.waveRelease({
                  tree: architect.tree,
                  children,
                  sessionId: context.sessionManager.getSessionId(),
                  secret: architect.secret,
                })
              );
            }
            case "comment":
              return toolSuccess(
                await daemon.comment({
                  tree: architect.tree,
                  sessionId: context.sessionManager.getSessionId(),
                  secret: architect.secret,
                  issue: stringInput("issue"),
                  body: stringInput("body"),
                })
              );
            case "post_spec":
              await daemon.postBody({
                tree: architect.tree,
                sessionId: context.sessionManager.getSessionId(),
                secret: architect.secret,
                issue: stringInput("issue"),
                body: stringInput("body"),
              });
              return toolSuccess({});
            case "label_add":
            case "label_remove": {
              const label = stringInput("label");
              if (!LEGION_TOOL_LABELS.includes(label as (typeof LEGION_TOOL_LABELS)[number])) {
                throw new Error("label changes must use surviving Legion labels");
              }
              return toolSuccess(
                await daemon.labels({
                  tree: architect.tree,
                  sessionId: context.sessionManager.getSessionId(),
                  secret: architect.secret,
                  issue: stringInput("issue"),
                  add: parameters.op === "label_add" ? [label] : [],
                  remove: parameters.op === "label_remove" ? [label] : [],
                })
              );
            }
            case "escalate": {
              const kind = stringInput("kind");
              if (kind !== "re-file" && kind !== "capacity" && kind !== "cross-tree") {
                throw new Error("Unknown Legion escalation kind");
              }
              if (!("context" in parameters) || parameters.context === undefined)
                throw new Error("escalate requires context");
              await daemon.escalate({
                tree: architect.tree,
                kind,
                context: parameters.context,
                sessionId: context.sessionManager.getSessionId(),
                secret: architect.secret,
              });
              return toolSuccess({});
            }
            case "request_refile":
              await daemon.escalate({
                tree: architect.tree,
                sessionId: context.sessionManager.getSessionId(),
                secret: architect.secret,
                kind: "re-file",
                context: { issue: stringInput("issue"), rationale: stringInput("rationale") },
              });
              return toolSuccess({});
            case "issue_close": {
              const comment = parameters.comment;
              if (comment !== undefined && typeof comment !== "string")
                throw new Error("issue_close comment must be a string");
              await daemon.issueClose({
                tree: architect.tree,
                sessionId: context.sessionManager.getSessionId(),
                secret: architect.secret,
                issue: stringInput("issue"),
                ...(comment === undefined ? {} : { comment }),
              });
              return toolSuccess({});
            }
            default:
              throw new Error(`Unsupported legion operation: ${String(parameters.op)}`);
          }
        } catch (error) {
          return toolFailure(error);
        }
      },
    });

    pi.registerTool({
      name: "envoy_dispatch",
      label: "envoy_dispatch",
      description:
        "Open an architect-owned Dispatch thread and route replies back to this Legion role.",
      defaultInactive: true,
      parameters: envoyDispatchToolSchema(pi),
      execute: async (_id, parameters, _signal, _onUpdate, context) => {
        try {
          const architect = architectSession(context);
          const { parent, subject, body, ask, urgency } = parameters;
          if (
            typeof parent !== "string" ||
            typeof subject !== "string" ||
            typeof body !== "string"
          ) {
            throw new Error("envoy_dispatch requires parent, subject, and body");
          }
          let dispatchUrgency: "low" | "med" | "high" | "blocking" | undefined;
          if (urgency === undefined) {
            dispatchUrgency = undefined;
          } else if (
            urgency === "low" ||
            urgency === "med" ||
            urgency === "high" ||
            urgency === "blocking"
          ) {
            dispatchUrgency = urgency;
          } else {
            throw new Error("envoy_dispatch urgency must be low, med, high, or blocking");
          }
          const result = await roleDaemon().dispatchThread({
            tree: architect.tree,
            issue: architect.issue,
            role: architect.role,
            sessionId: context.sessionManager.getSessionId(),
            secret: architect.secret,
            parent,
            subject,
            body,
            ...(ask === undefined ? {} : { ask }),
            ...(dispatchUrgency === undefined ? {} : { urgency: dispatchUrgency }),
          });
          return toolSuccess(result);
        } catch (error) {
          return toolFailure(error);
        }
      },
    });
  };

  pi.registerCommand("legion-claim-controller", {
    description: "Claim the Legion controller role and register daemon authority for this session",
    handler: async (_args, context) => claimController(context),
  });
}
