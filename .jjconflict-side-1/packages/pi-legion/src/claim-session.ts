import type { LegionCapabilityReportBody } from "@legion/contracts/legion-api";
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import { bootLegionClaim } from "@legion/pi-shared/legion-claim-core";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { SessionContext } from "@legion/pi-shared/pi-types";
import { claimEnvoyRole, onEnvoyRoleRegained } from "@legion/pi-shared/role-claim-bridge";
import { recordBootstrappedSession } from "@legion/pi-shared/subagent-session";
import pkg from "../package.json";
import { requiredEnvironment, requiredSecret } from "./classify";
import { exportJjSessionAttribution } from "./jj-attribution";

export interface ClaimSession {
  /** Boots the claim behind a root architect's or a phase worker's pane (`legion.ts` calls it for
   * every Legion session but the controller). A second call while the first runs waits on it; a
   * call once the claim is registered, from its session or another, boots nothing. */
  readonly bootstrap: (context: SessionContext) => Promise<void>;
  /** The claim `sessionID` registered, or undefined: before its registration, and for any other
   * session. */
  readonly capability: (sessionID: string) => ClaimCapability | undefined;
}

/**
 * Owns a root architect's or a phase worker's claim: the boot, through the claim boot every
 * harness shares (`bootLegionClaim`, `@legion/pi-shared/legion-claim-core`), and the capability it
 * registered. A Legion pane boots as its own Oh My Pi process and holds exactly one claim for its
 * whole lifetime, so the capability is plain closure state, set before `claims/ready`: the
 * daemon's task can arrive the moment ready answers, and the phase-stall hook reads the capability
 * when it does. The ready carries the session's capability report, measured from the
 * registration's answer on, so the daemon's `legion state` shows what this session can do.
 *
 * The transcript is recorded as the bootstrapped session before the registration; the jj
 * attribution (`exportJjSessionAttribution`) runs once it answered, before the capability is set.
 * A 4xx registration refusal, and any failure after a registration, exit the process.
 */
export function createClaimSession(deps: {
  readonly daemon: () => LegionDaemonClient;
  readonly persistedTranscript: (
    context: SessionContext
  ) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;
  /** Measures the six live capability rows for the claim's issue, against the task agents the
   * registration says the role's prompts dispatch (`measureCapabilities` in
   * `src/capability-report.ts`, behind the production host in `extensions/legion.ts`). */
  readonly measureCapabilities: (
    context: SessionContext,
    input: { readonly issue: string; readonly promptAgents: readonly string[] }
  ) => Promise<LegionCapabilityReportBody>;
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
    await bootLegionClaim({
      daemon,
      registration: {
        bootToken,
        sessionId: sessionID,
        ompSessionFile: sessionFile,
        agentId,
        pluginContract: pkg.legion.daemonApiVersion,
      },
      measure: (claim) =>
        deps.measureCapabilities(context, {
          issue: claim.issue,
          promptAgents: claim.promptAgents,
        }),
      afterRegister: () =>
        exportJjSessionAttribution(
          sessionFile,
          requiredEnvironment(process.env, "LEGION_STATE_DIR")
        ),
      setCapability: (registered) => {
        capability = registered;
      },
      // The claim's role topic is where the daemon sends every notice for an architect (the
      // notice executor, packages/daemon/internal/daemon/outbox.go): no issue topic carries one,
      // so no claim subscribes to one.
      claimRole: (role) => claimEnvoyRole(sessionID, role, context),
      onRoleRegained: onEnvoyRoleRegained,
      log: (line) => console.error(`[legion] ${line}`),
      stopping: "exiting so the daemon launches it again",
      stop: () => deps.exitProcess(1),
    });
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
