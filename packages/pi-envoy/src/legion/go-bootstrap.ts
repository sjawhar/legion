import { messageFor } from "@legion/envoy-client/errors";
import {
  controllerProject,
  legionControllerNoticeSubject,
  legionNoticeSubject,
  type LegionRole,
} from "@legion/contracts";
import pkg from "../../package.json";
import { classifySession, requiredEnvironment, requiredSecret } from "./classify";
import type { ControllerDaemon } from "./controller-session";
import { LegionGoDaemonApiError, type LegionGoDaemonClient } from "./go-daemon-client";
import { exportJjSessionAttribution } from "./jj-attribution";
import { claimEnvoyRole, onEnvoyRoleRegained, subscribeLegionNotice } from "./role-claim-bridge";
import type { CommandContext, SessionContext } from "../pi-types";

export interface GoClaimCapability {
  readonly kind: "phase-worker";
  readonly sessionID: string;
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly roleToken: string;
  readonly secret: string;
}

export interface GoBootstrapState {
  capability(): { readonly sessionID: string } | undefined;
  setCapability(capability: GoClaimCapability): void;
  bootstrap(): Promise<void> | undefined;
  setBootstrap(bootstrap: Promise<void> | undefined): void;
  daemon(): LegionGoDaemonClient;
  exitProcess(code: number): never;
  persistedTranscript(
    context: SessionContext
  ): Promise<{ readonly sessionFile: string; readonly agentId: string }>;
  recordBootstrappedSession(sessionFile: string): void;
}

// The Go path keeps this policy beside its client until Stage 7 removes the TypeScript daemon path.
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
function exitOnGoRegistrationRefusal(error: unknown, exitProcess: (code: number) => never): never {
  if (error instanceof LegionGoDaemonApiError) {
    console.error(
      `[legion] claims/register registration failed (${error.status}): ${error.detail}`
    );
    if (error.status >= 400 && error.status < 500) exitProcess(1);
  }
  throw error;
}

/** Retries only a Go ready request that can pass on its own: a 5xx or a network failure. */
async function callGoReadyWithRetry(label: string, call: () => Promise<void>): Promise<void> {
  for (let attempt = 1; ; attempt++) {
    try {
      await call();
      return;
    } catch (error) {
      const retryable =
        error instanceof LegionGoDaemonApiError
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
 * The Go daemon's controller: the session registers on the claim route with the capability `legion
 * controller start` fetched in place of a boot token, then claims the role token the registration
 * names (`legion-<project>-controller`), then subscribes to that project's controller topic
 * (`notifications.legion.<project>.controller`), where the daemon publishes every hold and a tree
 * architect's failed claim. The subscription is a live wake only: an Oh My Pi session subscribes
 * over core NATS, so a notice published while no controller runs never reaches one, and the
 * controller skill reads `legion state` at boot for what it missed. Its grants are minted with the
 * registration's own secret, which a later `legion controller start` revokes. Nothing re-runs on a
 * role regain: the Go daemon holds nothing for a controller, and the Envoy heartbeat keeps the role
 * itself. A refusal is logged and propagates without exiting — the operator started this session
 * and reads it; a refused capability was replaced by a later start. The transcript is reported on
 * every claim, a takeover's included, because the route requires one; the Go daemon records only
 * the session.
 */
export function goControllerDaemon(
  daemon: () => LegionGoDaemonClient,
  transcript: (
    context: CommandContext | SessionContext
  ) => Promise<{ readonly sessionFile: string; readonly agentId: string }>
): ControllerDaemon {
  return {
    claim: async ({ sessionID, capability, context }) => {
      const { sessionFile, agentId } = await transcript(context);
      const client = daemon();
      const registration = await client
        .registerController({
          bootToken: capability,
          sessionId: sessionID,
          ompSessionFile: sessionFile,
          agentId,
          pluginContract: pkg.legion.goDaemonApiVersion,
        })
        .catch((error: unknown) => {
          if (error instanceof LegionGoDaemonApiError) {
            console.error(
              `[legion] claims/register for the controller failed (${error.status}): ${error.detail}`
            );
          }
          throw error;
        });
      const envoyContext = "setInterval" in context ? context : undefined;
      await claimEnvoyRole(sessionID, registration.claimToken, envoyContext);
      await subscribeLegionNotice(
        sessionID,
        legionControllerNoticeSubject(controllerProject(registration.claimToken)),
        envoyContext
      );
      return () => client.controllerGrant({ sessionId: sessionID, secret: registration.secret });
    },
  };
}

/**
 * Boots a claim the Go daemon launched. The TypeScript daemon's bootstrap remains in legion.ts:
 * its ready route, recovery client and root-only tool path are a distinct API until Stage 7 removes
 * it. This path speaks only `/legion/v1/claims/*` through the Go client.
 */
export async function bootstrapGoClaim(
  context: SessionContext,
  state: GoBootstrapState
): Promise<void> {
  if (classifySession(process.env).kind === "not-legion") return;
  const sessionID = context.sessionManager.getSessionId();
  const capability = state.capability();
  if (capability !== undefined && capability.sessionID !== sessionID) return;
  const existing = state.bootstrap();
  if (existing !== undefined) return existing;

  const bootToken = requiredSecret(process.env, "LEGION_BOOT_TOKEN");
  const daemon = state.daemon();
  const bootstrap = (async () => {
    const { sessionFile, agentId } = await state.persistedTranscript(context);
    state.recordBootstrappedSession(sessionFile);

    const claim = await daemon
      .register({
        bootToken,
        sessionId: sessionID,
        ompSessionFile: sessionFile,
        agentId,
        pluginContract: pkg.legion.goDaemonApiVersion,
      })
      .catch((error) => exitOnGoRegistrationRefusal(error, state.exitProcess));
    const ready = {
      claimToken: claim.claimToken,
      sessionId: sessionID,
      secret: claim.secret,
      generation: claim.generation,
    };

    try {
      const stateDir = requiredEnvironment(process.env, "LEGION_STATE_DIR");
      await exportJjSessionAttribution(sessionFile, stateDir);
      state.setCapability({
        kind: "phase-worker",
        sessionID,
        tree: claim.tree,
        issue: claim.issue,
        role: claim.role,
        roleToken: claim.claimToken,
        secret: claim.secret,
      });
      await claimEnvoyRole(sessionID, claim.claimToken, context);
      await subscribeLegionNotice(
        sessionID,
        legionNoticeSubject(
          requiredEnvironment(process.env, "LEGION_PROJECT"),
          claim.role === "architect" ? claim.tree : claim.issue
        ),
        context
      );
      await callGoReadyWithRetry("claims/ready", () => daemon.ready(ready));
      onEnvoyRoleRegained(async (role, reason) => {
        if (role !== claim.claimToken) return;
        try {
          await callGoReadyWithRetry("claims/ready after role regain", () => daemon.ready(ready));
          console.error(`[legion] re-ran claims/ready after role ${role} was ${reason}`);
        } catch (error) {
          console.error(
            `[legion] claims/ready after role ${role} was ${reason} failed; the daemon's queued task stays undelivered until the next regain or boot: ${messageFor(error)}`
          );
        }
      });
    } catch (error) {
      console.error(
        `[legion] Go claim boot failed after claims/register registered ${claim.claimToken}; exiting so the daemon launches it again: ${messageFor(error)}`
      );
      state.exitProcess(1);
    }
  })();
  state.setBootstrap(bootstrap);
  try {
    await bootstrap;
  } catch (error) {
    state.setBootstrap(undefined);
    throw error;
  }
}
