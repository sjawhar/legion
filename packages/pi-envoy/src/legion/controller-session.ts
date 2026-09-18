import { controllerToken, type GrantResponse } from "@legion/contracts";
import { messageFor } from "@legion/envoy-client/errors";
import type { CommandContext, SessionContext } from "../pi-types";
import { classifySession, requiredControllerCapability, requiredEnvironment } from "./classify";
import { createLegionDaemonClient, type LegionDaemonClient } from "./daemon-client";
import { claimEnvoyRole, onEnvoyRoleRegained, type RoleRegainReason } from "./role-claim-bridge";

type PersistedTranscript = (
  context: CommandContext | SessionContext
) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;

type ReadyRerunner = (
  endpoint: "controller/ready" | "process/ready",
  role: string,
  reason: RoleRegainReason,
  call: () => Promise<void>
) => Promise<void>;

export interface ControllerSession {
  readonly claim: (context: CommandContext | SessionContext) => Promise<void>;
  readonly handleSessionStart: (context: SessionContext) => Promise<void>;
  readonly reclaimAfterSessionChange: (context: SessionContext) => Promise<void>;
  readonly isClaimedSession: (sessionID: string) => boolean;
  readonly mintGrant: (sessionID: string) => Promise<GrantResponse>;
}

/**
 * Owns the controller session's identity, resume transcript, role claim, and grant path. This is
 * deliberately separate from the extension's root/worker bootstrap and tool-routing policy: a
 * controller has no worker capability or recovery token, and its session can change in place.
 */
export function createControllerSession(
  persistedTranscript: PersistedTranscript,
  checkSubagentSession: (context: SessionContext) => Promise<boolean>,
  rerunReadyAfterRegain: ReadyRerunner,
  pluginVersion: string
): ControllerSession {
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let controllerDaemon: LegionDaemonClient | undefined;
  const controllerDaemonClient = (): LegionDaemonClient => {
    controllerDaemon ??= createLegionDaemonClient(
      requiredEnvironment(process.env, "LEGION_DAEMON_URL")
    );
    return controllerDaemon;
  };
  // The transcript the last successful controller claim reported. A takeover from a hand-started
  // session reports none, so a later session navigation can preserve the daemon pane's target.
  let controllerTranscript: string | undefined;

  /**
   * Claims the controller role for the context's session and posts `/controller/ready`. The
   * transcript the daemon records, and later resumes into a fresh pane, is reported only from the
   * daemon pane itself. A hand-started takeover moves the role but leaves that transcript untouched.
   */
  const claim = async (context: CommandContext | SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    const daemon = controllerDaemonClient();
    const secret = controllerCapability ?? requiredControllerCapability(process.env);
    controllerCapability = secret;
    const { project } = await daemon.state();
    const token = controllerToken(project);
    await claimEnvoyRole(sessionID, token, "setInterval" in context ? context : undefined);
    const ompSessionFile =
      classifySession(process.env).kind === "controller"
        ? (await persistedTranscript(context)).sessionFile
        : undefined;
    await daemon.controllerReady({
      secret,
      sessionId: sessionID,
      ...(ompSessionFile === undefined ? {} : { ompSessionFile }),
      pluginVersion,
    });
    controllerSessionID = sessionID;
    controllerTranscript = ompSessionFile;
    onEnvoyRoleRegained(async (role, reason) => {
      if (role !== token) return;
      await rerunReadyAfterRegain("controller/ready", role, reason, () =>
        daemon.controllerReady({
          secret,
          sessionId: sessionID,
          ...(controllerTranscript === undefined ? {} : { ompSessionFile: controllerTranscript }),
          pluginVersion,
        })
      );
    });
  };

  const handleSessionStart = async (context: SessionContext): Promise<void> => {
    if (classifySession(process.env).kind !== "controller") return;
    const sessionID = context.sessionManager.getSessionId();
    if (controllerSessionID === undefined || controllerSessionID === sessionID) {
      await claim(context);
    }
  };

  /** `/new`, `/resume`, or `/fork` can replace the controller session and its transcript in place. */
  const reclaimAfterSessionChange = async (context: SessionContext): Promise<void> => {
    if (await checkSubagentSession(context)) return;
    if (classifySession(process.env).kind !== "controller") return;
    if (
      context.sessionManager.getSessionId() === controllerSessionID &&
      context.sessionManager.getSessionFile() === controllerTranscript
    ) {
      return;
    }
    try {
      await claim(context);
    } catch (error) {
      context.ui.notify(
        `legion: re-claiming the controller for this session failed (${messageFor(error)}). Until a re-claim succeeds, controller wakes and merges will not reach this session and its shell commands run without a Legion grant, so legion gh is unavailable.`,
        "warning"
      );
    }
  };

  const isClaimedSession = (sessionID: string): boolean =>
    controllerSessionID === sessionID && controllerCapability !== undefined;

  const mintGrant = async (sessionID: string): Promise<GrantResponse> => {
    if (controllerSessionID !== sessionID || controllerCapability === undefined) {
      throw new Error("Controller session is not registered; cannot mint its grant");
    }
    const secret = controllerCapability;
    return controllerDaemonClient().grant({ sessionId: sessionID, secret });
  };

  return { claim, handleSessionStart, reclaimAfterSessionChange, isClaimedSession, mintGrant };
}
