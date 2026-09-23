import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { legionNoticeSubject, type LegionRole } from "@legion/contracts";
import pkg from "../../package.json";
import { classifySession, requiredEnvironment, requiredSecret } from "./classify";
import { LegionGoDaemonApiError, type LegionGoDaemonClient } from "./go-daemon-client";
import { exportJjSessionAttribution } from "./jj-attribution";
import { claimEnvoyRole, onEnvoyRoleRegained, subscribeLegionNotice } from "./role-claim-bridge";
import type { SessionContext } from "../pi-types";

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
 * Boots a claim the Go daemon launched. The TypeScript daemon's bootstrap remains in legion.ts:
 * its ready route, recovery client and root-only tool path are a distinct API until Stage 7 removes
 * it. This path speaks only `/legion/v1/claims/*` through the Go client.
 */
export async function bootstrapGoClaim(
  context: SessionContext,
  state: GoBootstrapState
): Promise<void> {
  const classification = classifySession(process.env);
  if (classification.kind === "not-legion") return;
  if (classification.kind === "controller") {
    throw new Error(
      "LEGION_DAEMON_API=go names the Go daemon, which launches no controller session"
    );
  }
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
      process.env.LEGION_GRANT_FILE = path.join(stateDir, "secrets", `${claim.claimToken}-grant`);
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
