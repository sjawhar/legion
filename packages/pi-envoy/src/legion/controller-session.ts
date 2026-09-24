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

/** One controller claim as a daemon's API takes it. */
export interface ControllerClaim {
  readonly sessionID: string;
  /** `LEGION_CONTROLLER_SECRET(_FILE)`: the capability the daemon issued this controller. */
  readonly capability: string;
  /** The daemon pane's own transcript, reported only from the pane (the `LEGION_CONTROLLER`
   * marker); a hand-started takeover reports none. */
  readonly ompSessionFile: string | undefined;
  readonly context: CommandContext | SessionContext;
}

/** How one daemon makes a session its controller: the Envoy role claim and the daemon's own
 * registration, in the order that daemon's API needs, answering the grant minter the registration
 * authorises. The TypeScript daemon's is `typescriptControllerDaemon`; the Go daemon's is
 * `goControllerDaemon` (`go-bootstrap.ts`). */
export interface ControllerDaemon {
  readonly claim: (claim: ControllerClaim) => Promise<() => Promise<GrantResponse>>;
}

/** The TypeScript daemon: the controller role is `controllerToken` of the daemon's project, claimed
 * before `/controller/ready` records this session, which re-runs whenever the Envoy heartbeat
 * regains the role (the daemon queues controller notices until it does). Grants are minted with
 * the controller capability itself. */
export function typescriptControllerDaemon(
  rerunReadyAfterRegain: ReadyRerunner,
  pluginVersion: string
): ControllerDaemon {
  let daemon: LegionDaemonClient | undefined;
  const client = (): LegionDaemonClient => {
    daemon ??= createLegionDaemonClient(requiredEnvironment(process.env, "LEGION_DAEMON_URL"));
    return daemon;
  };
  return {
    claim: async ({ sessionID, capability, ompSessionFile, context }) => {
      const daemon = client();
      const { project } = await daemon.state();
      const token = controllerToken(project);
      await claimEnvoyRole(sessionID, token, "setInterval" in context ? context : undefined);
      const ready = {
        secret: capability,
        sessionId: sessionID,
        ...(ompSessionFile === undefined ? {} : { ompSessionFile }),
        pluginVersion,
      };
      await daemon.controllerReady(ready);
      onEnvoyRoleRegained(async (role, reason) => {
        if (role !== token) return;
        await rerunReadyAfterRegain("controller/ready", role, reason, () =>
          daemon.controllerReady(ready)
        );
      });
      return () => daemon.grant({ sessionId: sessionID, secret: capability });
    },
  };
}

/**
 * Owns the controller session's identity, resume transcript, role claim, and grant path. This is
 * deliberately separate from the extension's root/worker bootstrap and tool-routing policy: a
 * controller has no worker capability or recovery token, and its session can change in place.
 * `daemon` is read on every claim: the pane's `LEGION_DAEMON_API` picks the daemon.
 */
export function createControllerSession(
  persistedTranscript: PersistedTranscript,
  checkSubagentSession: (context: SessionContext) => Promise<boolean>,
  daemon: () => ControllerDaemon
): ControllerSession {
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let mintControllerGrant: (() => Promise<GrantResponse>) | undefined;
  // The transcript the last successful controller claim reported. A takeover from a hand-started
  // session reports none, so a later session navigation can preserve the daemon pane's target.
  let controllerTranscript: string | undefined;

  /**
   * Claims the controller role for the context's session and registers it with the daemon. The
   * transcript the daemon records, and later resumes into a fresh pane, is reported only from the
   * daemon pane itself. A hand-started takeover moves the role but leaves that transcript untouched.
   */
  const claim = async (context: CommandContext | SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    const capability = controllerCapability ?? requiredControllerCapability(process.env);
    controllerCapability = capability;
    const ompSessionFile =
      classifySession(process.env).kind === "controller"
        ? (await persistedTranscript(context)).sessionFile
        : undefined;
    mintControllerGrant = await daemon().claim({ sessionID, capability, ompSessionFile, context });
    controllerSessionID = sessionID;
    controllerTranscript = ompSessionFile;
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
    controllerSessionID === sessionID && mintControllerGrant !== undefined;

  const mintGrant = async (sessionID: string): Promise<GrantResponse> => {
    if (controllerSessionID !== sessionID || mintControllerGrant === undefined) {
      throw new Error("Controller session is not registered; cannot mint its grant");
    }
    return mintControllerGrant();
  };

  return { claim, handleSessionStart, reclaimAfterSessionChange, isClaimedSession, mintGrant };
}
