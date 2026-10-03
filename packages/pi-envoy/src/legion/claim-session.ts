import type { LegionRole } from "@legion/contracts";
import { messageFor } from "@legion/envoy-client/errors";
import pkg from "../../package.json";
import type { SessionContext } from "../pi-types";
import { recordBootstrappedSession } from "../subagent-session";
import { requiredEnvironment, requiredSecret } from "./classify";
import { LegionDaemonApiError, type LegionDaemonClient } from "./daemon-client";
import { exportJjSessionAttribution } from "./jj-attribution";
import { claimEnvoyRole, onEnvoyRoleRegained } from "./role-claim-bridge";

/** The claim a session registered: its own id, the daemon's answer for tree, issue and role, and
 * the secret every credentialed request carries. */
export interface ClaimCapability {
  readonly sessionID: string;
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly secret: string;
}

export interface ClaimSession {
  /** Boots the claim behind a root architect's or a phase worker's pane (`legion.ts` calls it for
   * every Legion session but the controller). A second call while the first runs waits on it; a
   * call once the claim is registered, from its session or another, boots nothing. */
  readonly bootstrap: (context: SessionContext) => Promise<void>;
  /** The claim `sessionID` registered, or undefined: before its registration, and for any other
   * session. */
  readonly capability: (sessionID: string) => ClaimCapability | undefined;
}

// Bounds retries of the transient `claims/ready` request.
const READY_RETRY_ATTEMPTS = 3;
const READY_RETRY_DELAY_MS = 1_000;

function isNetworkOrTimeoutError(error: unknown): boolean {
  if (!(error instanceof Error)) return false;
  if (error.name === "NetworkError" || error.name === "TimeoutError") return true;

  const code = "code" in error && typeof error.code === "string" ? error.code : undefined;
  return (
    code === "ConnectionRefused" ||
    code === "ConnectionTimeout" ||
    code === "EAI_AGAIN" ||
    code === "ECONNREFUSED" ||
    code === "ECONNRESET" ||
    code === "EHOSTUNREACH" ||
    code === "ENETUNREACH" ||
    code === "ENOTFOUND" ||
    code === "ETIMEDOUT" ||
    (error instanceof TypeError &&
      (error.message === "Failed to fetch" || error.message === "fetch failed"))
  );
}

/** A 4xx registration failure cannot succeed on retry. It exits, while a 5xx or a request that
 * never reached the daemon propagates and leaves the pane for the registration deadline. */
function exitOnRegistrationRefusal(error: unknown, exitProcess: (code: number) => never): never {
  if (error instanceof LegionDaemonApiError) {
    console.error(
      `[legion] claims/register registration failed (${error.status}): ${error.detail}`
    );
    if (error.status >= 400 && error.status < 500) exitProcess(1);
  }
  throw error;
}

/** Retries only a ready request that can pass on its own: a 5xx or a network failure. */
async function callReadyWithRetry(label: string, call: () => Promise<void>): Promise<void> {
  for (let attempt = 1; ; attempt++) {
    try {
      await call();
      return;
    } catch (error) {
      const retryable =
        error instanceof LegionDaemonApiError
          ? error.status >= 500 && error.status < 600
          : isNetworkOrTimeoutError(error);
      if (!retryable) throw error;
      if (attempt === READY_RETRY_ATTEMPTS) {
        console.error(`[legion] ${label} failed after ${attempt} attempts: ${messageFor(error)}`);
        throw error;
      }
      console.error(
        `[legion] ${label} failed (attempt ${attempt}/${READY_RETRY_ATTEMPTS}), retrying: ${messageFor(error)}`
      );
      const retryDelay = Promise.withResolvers<void>();
      setTimeout(retryDelay.resolve, READY_RETRY_DELAY_MS);
      await retryDelay.promise;
    }
  }
}

/**
 * Owns a root architect's or a phase worker's claim through `/legion/v1/claims/*`: the boot, and
 * the capability it registered. A Legion pane boots as its own Oh My Pi process and holds exactly
 * one claim for its whole lifetime, so the capability is plain closure state, set before
 * `claims/ready`: the daemon's task can arrive the moment ready answers, and the phase-stall hook
 * reads the capability when it does.
 */
export function createClaimSession(deps: {
  readonly daemon: () => LegionDaemonClient;
  readonly persistedTranscript: (
    context: SessionContext
  ) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;
  readonly exitProcess: (code: number) => never;
}): ClaimSession {
  let capability: ClaimCapability | undefined;
  let booting: Promise<void> | undefined;

  const boot = async (
    context: SessionContext,
    sessionID: string,
    bootToken: string,
    daemon: LegionDaemonClient
  ): Promise<void> => {
    const { sessionFile, agentId } = await deps.persistedTranscript(context);
    recordBootstrappedSession(sessionFile);

    const claim = await daemon
      .register({
        bootToken,
        sessionId: sessionID,
        ompSessionFile: sessionFile,
        agentId,
        pluginContract: pkg.legion.daemonApiVersion,
      })
      .catch((error) => exitOnRegistrationRefusal(error, deps.exitProcess));
    const ready = {
      claimToken: claim.claimToken,
      sessionId: sessionID,
      secret: claim.secret,
      generation: claim.generation,
    };

    try {
      const stateDir = requiredEnvironment(process.env, "LEGION_STATE_DIR");
      await exportJjSessionAttribution(sessionFile, stateDir);
      capability = {
        sessionID,
        tree: claim.tree,
        issue: claim.issue,
        role: claim.role,
        secret: claim.secret,
      };
      // The claim's role topic is where the daemon sends every notice for an architect (the
      // notice executor, packages/daemon-go/internal/daemon/outbox.go): no issue topic carries one,
      // so no claim subscribes to one.
      await claimEnvoyRole(sessionID, claim.claimToken, context);
      await callReadyWithRetry("claims/ready", () => daemon.ready(ready));
      onEnvoyRoleRegained(async (role, reason) => {
        if (role !== claim.claimToken) return;
        try {
          await callReadyWithRetry("claims/ready after role regain", () => daemon.ready(ready));
          console.error(`[legion] re-ran claims/ready after role ${role} was ${reason}`);
        } catch (error) {
          console.error(
            `[legion] claims/ready after role ${role} was ${reason} failed; the daemon's queued task stays undelivered until the next regain or boot: ${messageFor(error)}`
          );
        }
      });
    } catch (error) {
      console.error(
        `[legion] claim boot failed after claims/register registered ${claim.claimToken}; exiting so the daemon launches it again: ${messageFor(error)}`
      );
      deps.exitProcess(1);
    }
  };

  const bootstrap = async (context: SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    if (capability !== undefined && capability.sessionID !== sessionID) return;
    if (booting !== undefined) return booting;

    const bootToken = requiredSecret(process.env, "LEGION_BOOT_TOKEN");
    booting = boot(context, sessionID, bootToken, deps.daemon());
    try {
      await booting;
    } catch (error) {
      booting = undefined;
      throw error;
    }
  };

  return {
    bootstrap,
    capability: (sessionID) => (capability?.sessionID === sessionID ? capability : undefined),
  };
}
