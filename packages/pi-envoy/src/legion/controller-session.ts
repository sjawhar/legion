import type { GrantResponse } from "@legion/contracts";
import { messageFor } from "@legion/envoy-client/errors";
import type { CommandContext, SessionContext } from "../pi-types";
import { classifySession, requiredControllerCapability } from "./classify";

type PersistedTranscript = (
  context: CommandContext | SessionContext
) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;

export interface ControllerSession {
  readonly claim: (context: CommandContext | SessionContext) => Promise<void>;
  readonly handleSessionStart: (context: SessionContext) => Promise<void>;
  readonly reclaimAfterSessionChange: (context: SessionContext) => Promise<void>;
  readonly isClaimedSession: (sessionID: string) => boolean;
  readonly mintGrant: (sessionID: string) => Promise<GrantResponse>;
}

/** One controller claim as the daemon's API takes it. */
export interface ControllerClaim {
  readonly sessionID: string;
  /** `LEGION_CONTROLLER_SECRET(_FILE)`: the capability the daemon issued this controller. */
  readonly capability: string;
  readonly context: CommandContext | SessionContext;
}

/** How the daemon makes a session its controller: the Envoy role claim and the daemon's own
 * registration, in the order its API needs, answering the grant minter the registration
 * authorises (`goControllerDaemon`, `go-bootstrap.ts`). */
export interface ControllerDaemon {
  readonly claim: (claim: ControllerClaim) => Promise<() => Promise<GrantResponse>>;
}

/**
 * Owns the controller session's identity, resume transcript, role claim, and grant path. This is
 * deliberately separate from the extension's root/worker bootstrap and tool-routing policy: a
 * controller has no worker capability or recovery token, and its session can change in place.
 */
export function createControllerSession(
  persistedTranscript: PersistedTranscript,
  checkSubagentSession: (context: SessionContext) => Promise<boolean>,
  daemon: ControllerDaemon
): ControllerSession {
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let mintControllerGrant: (() => Promise<GrantResponse>) | undefined;
  // The daemon pane's own transcript as of the last successful claim, which a session navigation
  // compares to decide whether to claim again; a hand-started takeover records none.
  let controllerTranscript: string | undefined;

  /** Claims the controller role for the context's session and registers it with the daemon. */
  const claim = async (context: CommandContext | SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    const capability = controllerCapability ?? requiredControllerCapability(process.env);
    controllerCapability = capability;
    const transcript =
      classifySession(process.env).kind === "controller"
        ? (await persistedTranscript(context)).sessionFile
        : undefined;
    mintControllerGrant = await daemon.claim({ sessionID, capability, context });
    controllerSessionID = sessionID;
    controllerTranscript = transcript;
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
