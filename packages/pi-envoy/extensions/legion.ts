import path from "node:path";
import { controllerToken, type LegionRole } from "@legion/contracts";
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults";
import { messageFor } from "@legion/envoy-client/errors";
import { logger } from "@oh-my-pi/pi-utils";
import { connect, type NatsConnection, StringCodec, type Subscription } from "nats";
import {
  classifySession,
  generation,
  requiredControllerCapability,
  requiredEnvironment,
} from "../src/legion/classify";
import {
  handleLegionControlDirective,
  type LegionControlDirective,
  parseControlDirective,
} from "../src/legion/control";
import { createLegionDaemonClient, LegionDaemonApiError } from "../src/legion/daemon-client";
import { installWorkerGhShim, workerGhEnvironment } from "../src/legion/gh-shim";
import { exportJjSessionAttribution } from "../src/legion/jj-attribution";
import { createLegionTool } from "../src/legion/tools";
import { setJjIdentity } from "../src/legion/workspace-helpers";
import type { CommandContext, PiApi, SessionContext, ToolCallEventResult } from "../src/pi-types";
import { claimEnvoyRole } from "./envoy";

interface LegionCapability {
  readonly kind: "root-architect" | "phase-worker";
  readonly sessionID: string;
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly roleToken: string;
  readonly secret: string;
}

// Fatal bootstrap failures call this instead of `process.exit` directly, so a
// test can substitute a throwing stand-in without killing the test runner.
// Production callers never override it.
let exitProcess: (code: number) => never = (code) => process.exit(code) as never;
export function setLegionBootstrapExitForTests(hook: (code: number) => never): void {
  exitProcess = hook;
}

async function persistedTranscript(
  context: SessionContext
): Promise<{ readonly sessionFile: string; readonly agentId: string }> {
  await context.sessionManager.ensureOnDisk();
  const sessionFile = context.sessionManager.getSessionFile();
  if (!sessionFile?.endsWith(".jsonl")) {
    throw new Error("Legion session must have a persisted transcript");
  }
  const agentId = path.basename(sessionFile, ".jsonl");
  if (!agentId) throw new Error("Legion session transcript has no agent id");
  return { sessionFile, agentId };
}

// An architect delegates code work, but its prompt requires `legion handoff
// write/complete` and `legion gh --` to report its own phase and touch GitHub.
// Allow bash only for a single `legion ...` invocation: no chaining outside a
// quoted argument. This is a conservative character scan, not a shell parser --
// it rejects some legitimate quoting it can't reason about (nested quotes,
// escapes) rather than risk letting a chained command through.
function isSingleLegionCommand(command: unknown): boolean {
  if (typeof command !== "string") return false;
  const trimmed = command.trim();
  if (trimmed.length === 0) return false;
  let quote: '"' | "'" | undefined;
  for (const char of trimmed) {
    if (quote !== undefined) {
      if (char === quote) quote = undefined;
      continue;
    }
    if (char === '"' || char === "'") {
      quote = char;
      continue;
    }
    if (char === "\n" || char === ";" || char === "&" || char === "|") return false;
  }
  if (quote !== undefined) return false;
  return trimmed.split(/\s+/, 1)[0] === "legion";
}

// Read by the daemon's startup probe (packages/daemon/src/daemon/index.ts,
// verifyLegionPluginLoaded) to prove this extension actually loaded from an
// ambient installed-plugin discovery -- not just that a manifest file exists,
// which stays true even when the plugin is disabled or unregistered in OMP's
// own plugin registry.
const LEGION_LOADED_MARKER = Symbol.for("legion.pi-envoy.legion-loaded");

/** Code-mutation tools blocked for an architect session (root or sub-architect) and a reviewer
 * (whose only sanctioned mutation is the final `.legion/` cleanup commit, made via `bash`). */
const CODE_MUTATION_TOOLS = ["edit", "write", "apply_patch"];
/** The merger verifies and reports only: no code mutation, and no further Legion spawns. */
const MERGER_BLOCKED_TOOLS = [...CODE_MUTATION_TOOLS, "task"];

export default function legionExtension(pi: PiApi): void {
  logger.debug("extension instance loaded", { extension: import.meta.url });
  (globalThis as Record<symbol, unknown>)[LEGION_LOADED_MARKER] = import.meta.url;
  const defaults = envoyDefaultsFromEnvironment(process.env);
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let controlConnection: NatsConnection | undefined;
  let controlSubscription: Subscription | undefined;
  const controlCodec = StringCodec();

  // A Legion root or phase-worker session boots as its own OMP process and
  // holds exactly one role for its whole lifetime, so its identity lives in
  // plain closure state.
  let capability: LegionCapability | undefined;
  let bootstrap: Promise<void> | undefined;

  const roleDaemon = () => {
    return createLegionDaemonClient(requiredEnvironment(process.env, "LEGION_DAEMON_URL"), fetch, {
      recoveryToken: (sessionId) => {
        // A worker's LEGION_BOOT_TOKEN is its recovery token exactly like the
        // root's: it is single-use to redeem the initial capability, but the
        // daemon accepts it again on /worker-session to reissue a secret it
        // has since forgotten (e.g. after a daemon restart).
        if (capability !== undefined && sessionId === capability.sessionID) {
          return requiredEnvironment(process.env, "LEGION_BOOT_TOKEN");
        }
        throw new Error(`Legion session ${sessionId} has no persisted recovery token`);
      },
      onRecovered: (sessionId, recovered) => {
        if (capability === undefined || sessionId !== capability.sessionID) {
          throw new Error(`Legion session ${sessionId} has no persisted recovery token`);
        }
        if (
          recovered.tree !== capability.tree ||
          recovered.issue !== capability.issue ||
          recovered.role !== capability.role
        ) {
          throw new Error("Daemon recovered a capability for a different Legion role");
        }
        capability = { ...capability, secret: recovered.secret };
      },
    });
  };

  const claimController = async (context: CommandContext | SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    const daemon = createLegionDaemonClient(requiredEnvironment(process.env, "LEGION_DAEMON_URL"));
    const secret = controllerCapability ?? requiredControllerCapability(process.env);
    controllerCapability = secret;
    const { project } = await daemon.state();
    await claimEnvoyRole(
      sessionID,
      controllerToken(project),
      "setInterval" in context ? context : undefined
    );
    await daemon.controllerReady({ secret, sessionId: sessionID });
    controllerSessionID = sessionID;
  };

  const reclaimArchitect = async (): Promise<void> => {
    if (capability === undefined || capability.kind !== "root-architect") {
      throw new Error("Legion root architect is not available for reclamation");
    }
    await claimEnvoyRole(capability.sessionID, capability.roleToken);
  };

  const startControlSubscription = async (sessionID: string): Promise<void> => {
    const subject = process.env.LEGION_CONTROL_SUBJECT;
    if (!subject || controlSubscription) return;
    if (defaults.natsUrls.length === 0) {
      throw new Error("ENVOY_NATS_URL is required for Legion control directives");
    }
    const connection = await connect({
      servers: [...defaults.natsUrls],
      name: `legion-control-${sessionID}`,
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
                  error: messageFor(error),
                })
              )
            );
          }
          continue;
        }
        await handleLegionControlDirective(directive, {
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

  const bootstrapRoot = async (context: SessionContext, tree: string): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    if (capability !== undefined && capability.sessionID !== sessionID) return;
    if (bootstrap) return bootstrap;

    const bootToken = requiredEnvironment(process.env, "LEGION_BOOT_TOKEN");

    bootstrap = (async () => {
      const { sessionFile, agentId } = await persistedTranscript(context);

      const started = await (async () => {
        try {
          return await createLegionDaemonClient(
            requiredEnvironment(process.env, "LEGION_DAEMON_URL")
          ).processStarted({
            tree,
            generation: generation(process.env),
            bootToken,
            rootSessionId: sessionID,
            agentId,
            ompSessionFile: sessionFile,
          });
        } catch (error) {
          if (error instanceof LegionDaemonApiError && error.status === 403) {
            console.error(
              `[legion] worker boot token rejected; exiting for the daemon to respawn: ${messageFor(error)}`
            );
            exitProcess(1);
          }
          throw error;
        }
      })();
      const roleToken = started.roleTokens.architect;
      if (!roleToken) throw new Error("Legion daemon did not return an architect role token");

      try {
        await exportJjSessionAttribution(
          sessionFile,
          requiredEnvironment(process.env, "LEGION_STATE_DIR")
        );
        capability = {
          kind: "root-architect",
          sessionID,
          tree,
          issue: tree,
          role: "architect",
          roleToken,
          secret: started.secret,
        };
        await claimEnvoyRole(sessionID, roleToken, context);
        await startControlSubscription(sessionID);
        await roleDaemon().processReady({
          tree,
          sessionId: sessionID,
          secret: started.secret,
        });
        registerArchitectTools();
        await activateLegionTool();
      } catch (error) {
        console.error(
          `[legion] root bootstrap failed after process/started registered a role; exiting so the daemon respawns a fresh attempt: ${messageFor(error)}`
        );
        exitProcess(1);
      }
    })();
    try {
      await bootstrap;
    } catch (error) {
      bootstrap = undefined;
      throw error;
    }
  };

  const bootstrapWorker = async (
    context: SessionContext,
    role: LegionRole,
    tree: string,
    issue: string
  ): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    if (capability !== undefined && capability.sessionID !== sessionID) return;
    if (bootstrap) return bootstrap;

    const bootToken = requiredEnvironment(process.env, "LEGION_BOOT_TOKEN");
    const workspace = requiredEnvironment(process.env, "LEGION_WORKSPACE");

    bootstrap = (async () => {
      const { sessionFile, agentId } = await persistedTranscript(context);

      const started = await (async () => {
        try {
          return await createLegionDaemonClient(
            requiredEnvironment(process.env, "LEGION_DAEMON_URL")
          ).workerStarted({
            tree,
            issue,
            role,
            bootToken,
            sessionId: sessionID,
            agentId,
            ompSessionFile: sessionFile,
          });
        } catch (error) {
          if (error instanceof LegionDaemonApiError && error.status === 403) {
            console.error(
              `[legion] worker boot token rejected; exiting for the daemon to respawn: ${messageFor(error)}`
            );
            exitProcess(1);
          }
          throw error;
        }
      })();

      try {
        await exportJjSessionAttribution(
          sessionFile,
          requiredEnvironment(process.env, "LEGION_STATE_DIR")
        );
        await setJjIdentity(workspace, started.gitName, started.gitEmail);
        capability = {
          kind: "phase-worker",
          sessionID,
          tree,
          issue,
          role,
          roleToken: started.roleToken,
          secret: started.secret,
        };
        await claimEnvoyRole(sessionID, started.roleToken, context);
        if (role === "architect") {
          registerArchitectTools();
          await activateLegionTool();
        }
        await roleDaemon().workerReady({
          tree,
          issue,
          role,
          sessionId: sessionID,
          secret: started.secret,
          generation: generation(process.env),
        });
      } catch (error) {
        console.error(
          `[legion] worker bootstrap failed after worker/started registered a role; exiting so the daemon respawns a fresh attempt: ${messageFor(error)}`
        );
        exitProcess(1);
      }
    })();
    try {
      await bootstrap;
    } catch (error) {
      bootstrap = undefined;
      throw error;
    }
  };

  pi.on("session_start", async (_event, context) => {
    const classification = classifySession(process.env);
    switch (classification.kind) {
      case "controller": {
        const sessionID = context.sessionManager.getSessionId();
        if (controllerSessionID === undefined || controllerSessionID === sessionID) {
          await claimController(context);
        }
        return;
      }
      case "phase-worker":
        await bootstrapWorker(
          context,
          classification.role,
          classification.tree,
          classification.issue
        );
        return;
      case "root-architect":
        await bootstrapRoot(context, classification.tree);
        return;
      case "not-legion":
        return;
    }
  });

  pi.on("tool_call", async (toolCall, context): Promise<ToolCallEventResult | undefined> => {
    const sessionID = context.sessionManager.getSessionId();
    const active = capability?.sessionID === sessionID ? capability : undefined;
    // `role === "architect"` covers both kinds: the root architect and a sub-architect (a
    // phase worker with role "architect") both delegate all code work to phase workers.
    if (
      active?.role === "architect" &&
      (CODE_MUTATION_TOOLS.includes(toolCall.toolName) ||
        (toolCall.toolName === "bash" && !isSingleLegionCommand(toolCall.input.command)))
    ) {
      return { block: true, reason: "the architect delegates all code work to phase workers" };
    }
    // Only a phase-worker session (never the root or sub-architect kinds above) is further
    // restricted by role below.
    if (active?.kind === "phase-worker") {
      if (active.role === "reviewer" && CODE_MUTATION_TOOLS.includes(toolCall.toolName)) {
        return {
          block: true,
          reason: "the reviewer edits nothing except the final .legion/ cleanup commit via bash",
        };
      }
      if (active.role === "merger" && MERGER_BLOCKED_TOOLS.includes(toolCall.toolName)) {
        return { block: true, reason: "the merger only verifies and reports" };
      }
    }
    if (toolCall.toolName !== "bash" || typeof toolCall.input.command !== "string")
      return undefined;
    if (active === undefined) {
      // A worker (root or phase) whose own boot handshake has not completed
      // yet has no capability to mint a grant with. The controller is
      // exempt: the daemon also sets LEGION_ROLE=controller on its process,
      // but a controller never claims a Legion role here.
      if (process.env.LEGION_ROLE !== undefined && process.env.LEGION_CONTROLLER !== "1") {
        return {
          block: true,
          reason: "Legion worker session is not registered; cannot mint LEGION_GRANT",
        };
      }
      return undefined;
    }
    try {
      const grant = await roleDaemon().grant({
        tree: active.tree,
        issue: active.issue,
        sessionId: sessionID,
        secret: active.secret,
      });
      const stateDir = requiredEnvironment(process.env, "LEGION_STATE_DIR");
      const workerBin = await installWorkerGhShim(stateDir);
      return {
        input: {
          ...toolCall.input,
          command: [
            workerGhEnvironment(grant.grantId, stateDir, workerBin),
            toolCall.input.command,
          ].join("\n"),
        },
      };
    } catch (error) {
      return { block: true, reason: messageFor(error) };
    }
  });

  pi.on("session_shutdown", async (_event, context) => {
    const sessionID = context.sessionManager.getSessionId();
    if (
      capability === undefined ||
      capability.kind !== "root-architect" ||
      capability.sessionID !== sessionID
    ) {
      return;
    }
    try {
      await roleDaemon().processExit({
        tree: capability.tree,
        generation: generation(process.env),
        sessionId: sessionID,
        secret: capability.secret,
      });
    } finally {
      controlSubscription?.unsubscribe();
      controlSubscription = undefined;
      await controlConnection?.close();
      controlConnection = undefined;
    }
  });

  const architectSession = (
    context: SessionContext
  ): { tree: string; issue: string; role: LegionRole; secret: string } => {
    const sessionID = context.sessionManager.getSessionId();
    if (
      capability !== undefined &&
      capability.sessionID === sessionID &&
      capability.role === "architect"
    ) {
      return {
        tree: capability.tree,
        issue: capability.issue,
        role: capability.role,
        secret: capability.secret,
      };
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
  };

  pi.registerCommand("legion-claim-controller", {
    description: "Claim the Legion controller role and register daemon authority for this session",
    handler: async (_args, context) => claimController(context),
  });
}
