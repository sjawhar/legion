import path from "node:path";
import {
  controllerToken,
  formatIssueKey,
  type IssueKey,
  isLegionRole,
  type LegionRole,
  parseIssueKey,
  roleToken,
} from "@legion/contracts";
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults";
import { provisionIssueWorkspace, type WorkspaceSpec } from "@legion/workspace";
import { connect, type NatsConnection, StringCodec, type Subscription } from "nats";
import {
  generation,
  isRootSession,
  positiveIntegerEnvironment,
  requiredControllerCapability,
  requiredEnvironment,
} from "../src/legion/classify";
import {
  handleLegionControlDirective,
  type LegionControlDirective,
  parseControlDirective,
} from "../src/legion/control";
import { createLegionDaemonClient } from "../src/legion/daemon-client";
import { installWorkerGhShim, workerGhEnvironment } from "../src/legion/gh-shim";
import type {
  BeforeAgentStartEvent,
  CommandContext,
  PiApi,
  SessionContext,
  ToolCallEvent,
  ToolCallEventResult,
  ToolResultEvent,
} from "../src/legion/pi-types";
import { legionSpawnBlockPattern, parseWorkerSpawn, workerAgentId } from "../src/legion/spawn";
import { createEnvoyDispatchTool, createLegionTool } from "../src/legion/tools";
import {
  acquireWorkerBudget,
  addPendingLegionSpawn,
  ensureWorkerBudgetPermit,
  type PendingLegionSpawn,
  pendingLegionSpawns,
  pendingLegionSpawnsByToken,
  type RootBootstrap,
  registerWorkerBudgetPermit,
  releasePendingLegionSpawn,
  releaseWorkerBudgetPermit,
  rootBootstraps,
  transferPendingLegionSpawn,
  type WorkerSession,
  workerSessions,
} from "../src/legion/worker-budget";
import { runWorkspaceCommand, setJjIdentity } from "../src/legion/workspace-helpers";
import { claimEnvoyRole, deleteEnvoyInterest, type EnvoySessionContext } from "./envoy";

export default function legionExtension(pi: PiApi): void {
  const agents = pi.agents;
  const defaults = envoyDefaultsFromEnvironment(process.env);
  let rootSessionID: string | undefined;
  let rootArchitectRole: string | undefined;
  let rootSecret: string | undefined;
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let controlConnection: NatsConnection | undefined;
  let controlSubscription: Subscription | undefined;
  const controlCodec = StringCodec();
  let workspaceStateDir: string | undefined;
  const workspaceProvisions = new Map<IssueKey, Promise<WorkspaceSpec>>();
  const roleDaemon = () => {
    return createLegionDaemonClient(requiredEnvironment(process.env, "LEGION_DAEMON_URL"), fetch, {
      recoveryToken: (sessionId) => {
        if (sessionId === rootSessionID) {
          return requiredEnvironment(process.env, "LEGION_BOOT_TOKEN");
        }
        const worker = workerSessions.get(sessionId);
        if (worker === undefined) {
          throw new Error(`Legion session ${sessionId} has no persisted recovery token`);
        }
        return worker.spawnToken;
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
    });
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
      return { role, secret: started.secret };
    })();
    rootBootstraps.set(bootToken, bootstrap);
    try {
      const root = await bootstrap;
      rootSessionID = sessionID;
      rootArchitectRole = root.role;
      rootSecret = root.secret;
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
  const recoverWorkerSession = async (
    context: SessionContext
  ): Promise<WorkerSession | undefined> => {
    const sessionID = context.sessionManager.getSessionId();
    const existing = workerSessions.get(sessionID);
    if (existing) return existing;
    const daemonUrl = process.env.LEGION_DAEMON_URL;
    const project = process.env.LEGION_PROJECT;
    if (
      !process.env.LEGION_TREE ||
      !daemonUrl ||
      !project ||
      process.env.LEGION_CONTROLLER === "1" ||
      (context.taskDepth ?? 0) === 0 ||
      isRootSession(process.env, context)
    ) {
      return undefined;
    }
    throw new Error(`Legion worker session ${sessionID} has no persisted recovery token`);
  };

  pi.on("session_start", async (_event, context) => {
    const sessionID = context.sessionManager.getSessionId();
    if (process.env.LEGION_CONTROLLER === "1") {
      if (controllerSessionID === undefined || controllerSessionID === sessionID)
        await claimController(context);
      return;
    }
    const worker = await recoverWorkerSession(context);
    if (worker) {
      await ensureWorkerBudgetPermit(sessionID);
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
      } catch (error) {
        releaseWorkerBudgetPermit(sessionID);
        throw error;
      }
      return;
    }
    if (isRootSession(process.env, context)) await bootstrapRoot(context);
  });

  pi.on("before_agent_start", async (event, context) => {
    const sessionID = context.sessionManager.getSessionId();
    if (workerSessions.has(sessionID)) {
      await ensureWorkerBudgetPermit(sessionID);
      return;
    }
    const project = process.env.LEGION_PROJECT;
    const spawn = project
      ? parseWorkerSpawn((event as BeforeAgentStartEvent).prompt, project)
      : undefined;
    if (!spawn) {
      if (isRootSession(process.env, context)) await bootstrapRoot(context);
      return;
    }

    const pending = pendingLegionSpawnsByToken.get(spawn.spawnToken);
    const release = pending
      ? transferPendingLegionSpawn(pending)
      : await acquireWorkerBudget(
          positiveIntegerEnvironment(process.env, "LEGION_WORKER_BUDGET", "6")
        );
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
        const role = agent.slice("legion-".length);
        const task = taskInput.task;
        if (!isLegionRole(role) || typeof task !== "string") return undefined;
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
        const maxDepth = positiveIntegerEnvironment(process.env, "LEGION_MAX_RECURSION_DEPTH", "8");
        if (role === "architect" && depth + 2 > maxDepth) {
          throw new Error(
            `sub-architect at depth ${depth} would place its workers at the recursion cap (${maxDepth}); escalate to your parent architect instead`
          );
        }
        const release = await acquireWorkerBudget(
          positiveIntegerEnvironment(process.env, "LEGION_WORKER_BUDGET", "6")
        );
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
          const taskWithoutMachineBlocks = task.replace(legionSpawnBlockPattern, "").trimEnd();
          return {
            ...taskInput,
            task: `${taskWithoutMachineBlocks}\n\n<legion-spawn issue="${issue}" role="${role}" token="${token}" tree="${tree}" spawnToken="${spawn.spawnToken}" workspace="${workspace.workspaceDir}"/>`,
          };
        } catch (error) {
          release();
          throw error;
        }
      };
      try {
        const rawTasks: unknown[] | undefined = Array.isArray(toolCall.input.tasks)
          ? toolCall.input.tasks
          : undefined;
        if (rawTasks) {
          let injected = false;
          const tasks: unknown[] = [];
          for (const task of rawTasks) {
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
    const priorWorker = workerSessions.get(sessionID);
    let worker: WorkerSession | undefined;
    try {
      worker = priorWorker ?? (await recoverWorkerSession(context));
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
      await ensureWorkerBudgetPermit(sessionID);
      if (!priorWorker) await claimRole(sessionID, worker.token, context);
      const grant = await roleDaemon().grant({
        tree: worker.tree,
        issue: worker.issue,
        sessionId: sessionID,
        secret: worker.secret,
      });
      const stateDir = requiredEnvironment(process.env, "LEGION_STATE_DIR");
      const workerBin = await installWorkerGhShim(stateDir);
      return {
        input: {
          ...toolCall.input,
          command: `${workerGhEnvironment(grant.grantId, stateDir, workerBin)}\n${toolCall.input.command}`,
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

  pi.on("agent_end", async (event, context) => {
    const willContinue =
      typeof event === "object" &&
      event !== null &&
      "willContinue" in event &&
      event.willContinue === true;
    if (willContinue) return;
    releaseWorkerBudgetPermit(context.sessionManager.getSessionId());
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
    pi.registerTool(createLegionTool({ pi, roleDaemon, architectSession }));
    pi.registerTool(createEnvoyDispatchTool({ pi, roleDaemon, architectSession }));
  };

  pi.registerCommand("legion-claim-controller", {
    description: "Claim the Legion controller role and register daemon authority for this session",
    handler: async (_args, context) => claimController(context),
  });
}
