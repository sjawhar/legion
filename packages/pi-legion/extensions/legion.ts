import { randomUUID } from "node:crypto";
import path from "node:path";
import { activeDispatchConfig } from "@legion/envoy-client/dispatch-config";
import { resolveIssueDocumentId } from "@legion/envoy-client/dispatch-execute";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import {
  sessionDirectory as dispatchSessionDirectory,
  writeSessionTitle,
} from "@legion/envoy-client/dispatch-session-state";
import { messageFor } from "@legion/envoy-client/errors";
import { matchInjectedUserTurn } from "@legion/pi-shared/injected-user-turns";
import {
  ENVOY_PLUGIN_INTERFACE_VERSION,
  LEGACY_LEGION_LOADED_KEY,
  LEGION_PLUGIN_LOADED_KEY,
  readEnvoyPluginInterface,
} from "@legion/pi-shared/interface";
import type { CommandContext, PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { subagentSessionCheck } from "@legion/pi-shared/subagent-session";
import { logger } from "@oh-my-pi/pi-utils";
import { createClaimSession } from "../src/claim-session";
import { classifySession, requiredEnvironment } from "../src/classify";
import { createControllerSession } from "../src/controller-session";
import { createLegionDaemonClient, type LegionDaemonClient } from "../src/daemon-client";
import { findHandoffCommit } from "../src/handoff-commit";
import {
  assistantText,
  inboundKind,
  PHASE_STALL_ENTRY,
  type PhaseStall,
  type PhaseStallInput,
  restorePhaseStall,
  stepPhaseStall,
} from "../src/phase-stall";
import { applySessionTitle, legionSessionTitle } from "../src/session-title";
import { createLegionTool } from "../src/tools";

// Fatal bootstrap failures call this instead of `process.exit` directly, so a
// test can substitute a throwing stand-in without killing the test runner.
// Production callers never override it.
let exitProcess: (code: number) => never = (code) => process.exit(code) as never;
export function setLegionBootstrapExitForTests(hook: (code: number) => never): void {
  exitProcess = hook;
}

async function persistedTranscript(
  context: CommandContext | SessionContext
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

/**
 * Why this Legion entry cannot run in this process, or undefined when it can. Legion claims roles
 * and reads deliveries through the interface the Envoy plugin publishes
 * (`@legion/pi-shared/interface`), so a process without `@sjawhar/pi-envoy`, or with one that
 * speaks another interface version, has nothing to claim through; and the pre-split
 * `@sjawhar/pi-legion-envoy` still installed beside this plugin would run a second Legion entry
 * against the same pane. The sentence names the remedy, since the daemon's boot log and the
 * operator's own session are where it lands.
 */
function envoyPluginRefusal(): string | undefined {
  const legacy = (globalThis as Record<symbol, unknown>)[LEGACY_LEGION_LOADED_KEY];
  if (legacy !== undefined) {
    const where = typeof legacy === "string" ? ` from ${legacy}` : "";
    return `Legion plugin at ${import.meta.url} found the pre-split @sjawhar/pi-legion-envoy loaded${where}; uninstall @sjawhar/pi-legion-envoy (omp plugin uninstall @sjawhar/pi-legion-envoy) and keep @sjawhar/pi-envoy beside @sjawhar/pi-legion`;
  }
  const reading = readEnvoyPluginInterface();
  switch (reading.kind) {
    case "present":
      return undefined;
    case "absent":
      return `Legion plugin at ${import.meta.url} needs @sjawhar/pi-envoy at interface version ${ENVOY_PLUGIN_INTERFACE_VERSION}, found none; install @sjawhar/pi-envoy beside @sjawhar/pi-legion`;
    case "mismatch":
      return `Legion plugin at ${import.meta.url} needs @sjawhar/pi-envoy at interface version ${reading.expected}, found version ${reading.found} from ${reading.from}; install the @sjawhar/pi-envoy released with this @sjawhar/pi-legion`;
  }
}

export default function legionExtension(pi: PiApi): void {
  // One instance per session (a `task` subagent gets its own); the id tells them apart in the log.
  const instance = randomUUID().slice(0, 8);
  logger.debug("extension instance loaded", { extension: import.meta.url, instance });
  // Read by the daemon's boot gate to prove this extension actually loaded from an ambient
  // installed-plugin discovery -- not just that a manifest file exists, which stays true even when
  // the plugin is disabled or unregistered in OMP's own plugin registry -- and which interface
  // version it speaks.
  (globalThis as Record<symbol, unknown>)[LEGION_PLUGIN_LOADED_KEY] = {
    from: import.meta.url,
    envoyInterface: ENVOY_PLUGIN_INTERFACE_VERSION,
  };

  // Gates session_start and the session-change re-claim below, one call per hook; its settled
  // answer is kept, so it runs once per session.
  const checkSubagentSession = subagentSessionCheck();

  // The phase-stall check (src/phase-stall.ts). It runs only in a session holding a
  // claim for its own id with a phase role, so never in an architect (a root or a sub-architect),
  // the controller (whose claim lives in controllerSession), a session with no Legion environment,
  // or a `task` subagent (whose instance returns at checkSubagentSession before any capability
  // exists). Restored from the transcript at session_start and appended to it on every change.
  let phaseStall: PhaseStall = "closed";
  const phaseWorkerSession = (context: SessionContext): boolean => {
    const role = claimSession.capability(context.sessionManager.getSessionId())?.role;
    return role !== undefined && role !== "architect";
  };
  const advancePhaseStall = (input: PhaseStallInput): string | undefined => {
    const step = stepPhaseStall(phaseStall, input);
    if (step.state !== phaseStall) {
      phaseStall = step.state;
      pi.appendEntry(PHASE_STALL_ENTRY, { state: phaseStall });
    }
    return step.followUp;
  };

  let daemonClient: LegionDaemonClient | undefined;
  const roleDaemon = (): LegionDaemonClient => {
    daemonClient ??= createLegionDaemonClient(
      requiredEnvironment(process.env, "LEGION_DAEMON_URL")
    );
    return daemonClient;
  };

  // A root architect's or phase worker's claim, and the operator-launched controller's.
  const claimSession = createClaimSession({
    daemon: roleDaemon,
    persistedTranscript,
    exitProcess: (code) => exitProcess(code),
  });
  const controllerSession = createControllerSession({
    daemon: roleDaemon,
    persistedTranscript,
    pi,
    exitProcess: (code) => exitProcess(code),
  });

  /**
   * Names the session by its Legion identity (`src/session-title.ts`), so every Dispatch
   * write stamps it as `origin.session_title` and the Envoy listener lists it. Runs before the
   * session claims its Envoy role: that claim registers the session, and the registration carries
   * the title then rather than at the next heartbeat. The `dispatch` command reads the title from
   * the session's `title` file, which envoy.ts rewrites only before a shell command whose session
   * name changed, so it is written here too, and the pane's first command already carries it.
   */
  const titleSession = async (context: SessionContext): Promise<void> => {
    const title = legionSessionTitle(classifySession(process.env), process.env.LEGION_PROJECT);
    if (title === undefined) return;
    await applySessionTitle(pi, context, title);
    const sessionID = context.sessionManager.getSessionId();
    if (sessionID === "") return;
    writeSessionTitle(
      dispatchSessionDirectory(process.env, sessionID),
      context.sessionManager.getSessionName?.() ?? title
    );
  };

  pi.on("session_start", async (_event, context) => {
    // A `task`-spawned subagent session loads a fresh instance of this whole module: bail out
    // before classification, or the inherited LEGION_* environment would look like a fresh
    // root/worker boot and its failure would exit the parent process. See isSubagentSession.
    if (await checkSubagentSession(context)) return;
    await titleSession(context);
    // A worker the daemon relaunched with --resume keeps its phase: its next turn may start from
    // an Envoy notice rather than a new assignment, and must find the phase still open.
    phaseStall = restorePhaseStall(context.sessionManager.getBranch?.() ?? []);
    const { kind } = classifySession(process.env);
    if (kind === "not-legion") return;
    // Every Legion session (a root architect, a phase worker, the controller) claims through the
    // Envoy plugin's interface, so a process without it ends here, the way a refused boot
    // registration does: one log line, then the exit the daemon sees and relaunches from. A
    // person's own session returned above and never exits.
    const refusal = envoyPluginRefusal();
    if (refusal !== undefined) {
      logger.error(refusal);
      exitProcess(1);
    }
    // The controller registers through the controller session and gets the `legion` tool's
    // controller operations (`read_state`, `set_status`), each minting its own controller grant.
    if (kind === "controller") {
      await controllerSession.handleSessionStart(context);
    } else {
      await claimSession.bootstrap(context);
    }
    registerLegionTool();
    await activateLegionTool();
  });

  // Mirrors envoy.ts: only a switch reports why the session changed; a branch or a tree
  // navigation carries no reason, and every one of them can leave the pane on a new session id.
  // The controller re-claims whatever session the pane is left on, so that session takes the
  // controller's title first; a session that keeps its id keeps its title. A `task` subagent's
  // instance does neither, and the subagent check is asked once for both.
  const afterSessionChange = async (context: SessionContext): Promise<void> => {
    if (await checkSubagentSession(context)) return;
    if (classifySession(process.env).kind === "controller") await titleSession(context);
    await controllerSession.reclaimAfterSessionChange(context);
  };
  pi.on("session_switch", (_event, context) => afterSessionChange(context));
  pi.on("session_branch", (_event, context) => afterSessionChange(context));
  pi.on("session_tree", (_event, context) => afterSessionChange(context));

  // The daemon's assignment (a user message) opens the phase; an Envoy delivery re-arms a stall
  // that already had its follow-up or a WAITING reply. A person's direct message that envoy.ts
  // sent in as the user's own turn is a user message too, but it is an inbound event, as its Envoy
  // card was: the record envoy.ts keeps of the turns it sent in says which user message that is,
  // whichever of the two extensions asks first. A turn that record misses falls through to
  // `inboundKind` and counts as an assignment; `packages/pi-envoy/AGENTS.md` (the phase-worker
  // section) says which turns it misses. The `legion` tool's successful `handoff_complete` closes
  // the phase (`onPhaseCompleted`, below).
  pi.on("message_start", async (event, context) => {
    if (!phaseWorkerSession(context)) return;
    const injected =
      matchInjectedUserTurn(context.sessionManager.getSessionId(), event.message) !== undefined;
    const kind = injected ? "inbound-event" : inboundKind(event.message);
    if (kind !== undefined) advancePhaseStall({ kind });
  });

  // The run is about to settle with nothing left for the host to continue: a phase still open
  // gets its one follow-up, which the host sends as the next turn of this same session.
  pi.on("session_stop", async (event, context) => {
    if (event.signal.aborted || !phaseWorkerSession(context)) return undefined;
    const followUp = advancePhaseStall({
      kind: "settle",
      lastAssistantText: assistantText(event.last_assistant_message),
    });
    return followUp === undefined ? undefined : { continue: true, additionalContext: followUp };
  });

  const onPhaseCompleted = (context: SessionContext): void => {
    if (phaseWorkerSession(context)) advancePhaseStall({ kind: "handoff-complete" });
  };

  const activateLegionTool = async (): Promise<void> => {
    const activeTools = pi.getActiveTools();
    if (activeTools.includes("legion")) return;
    await pi.setActiveTools([...activeTools, "legion"]);
  };

  // The session the tool answers for: a claim's (a root architect's, a sub-architect's or a phase
  // worker's, keyed by its role) or the claimed controller's; a `task` subagent holds neither and
  // is refused, as is a session whose boot has not registered yet.
  let legionToolRegistered = false;
  const registerLegionTool = (): void => {
    if (legionToolRegistered) return;
    legionToolRegistered = true;
    pi.registerTool(
      createLegionTool({
        pi,
        daemon: roleDaemon,
        session: (context) => {
          const sessionID = context.sessionManager.getSessionId();
          const active = claimSession.capability(sessionID);
          if (active !== undefined) {
            return {
              kind: active.role === "architect" ? "architect" : "phase-worker",
              role: active.role,
              sessionId: active.sessionID,
              tree: active.tree,
              issue: active.issue,
              secret: active.secret,
            };
          }
          if (controllerSession.isClaimedSession(sessionID)) {
            return { kind: "controller", sessionId: sessionID };
          }
          throw new Error("legion is available only to this session's registered claim");
        },
        controllerGrant: (sessionId) => controllerSession.mintGrant(sessionId),
        onPhaseCompleted,
        // The pane's own environment: its workspace and issue, and the identity the daemon put
        // on it; PATH's jj, the one its shell runs.
        handoffCommit: (phase, role) => findHandoffCommit({ phase, role, env: process.env }),
        resolveDocument: async (issue, reference) => {
          const config = activeDispatchConfig(process.env, { cwd: process.cwd() });
          if (config === null) {
            throw new Error(
              `register_gate cannot look up document "${reference}" on ${issue}: this pane has no Dispatch configured; pass the document's id`
            );
          }
          try {
            return await resolveIssueDocumentId(
              new DispatchClient(config.url, config.token, fetch),
              issue,
              reference
            );
          } catch (error) {
            throw new Error(
              `register_gate could not look up document "${reference}" on ${issue} in Dispatch: ${messageFor(error)}`
            );
          }
        },
      })
    );
  };

  pi.registerCommand("legion-claim-controller", {
    description: "Claim the Legion controller role and register daemon authority for this session",
    // Inside the controller's own session (the `LEGION_CONTROLLER` marker) this is the manual
    // override for a lost claim. From a hand-started session it is an interactive takeover: the
    // role and recorded session id move to this session (see `controllerSession.claim`), which
    // then gets the `legion` tool's controller operations as a launched controller does.
    // The operator started this session and reads it, so a missing Envoy plugin is answered as
    // the command's error, never an exit, and nothing is claimed.
    handler: async (_args, context) => {
      const refusal = envoyPluginRefusal();
      if (refusal !== undefined) throw new Error(refusal);
      await controllerSession.claim(context);
      registerLegionTool();
      await activateLegionTool();
    },
  });
}
