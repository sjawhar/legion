import type { LegionCapabilityReportBody, LegionRegistration } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  callReadyWithRetry,
  LegionDaemonApiError,
  type LegionDaemonClient,
  type ReadyInput,
  type RegisterInput,
} from "./daemon-client";
import type { ClaimCapability } from "./legion-phase-ops";

/** What a host gives the claim boot: the registration it sends, and how it claims the Envoy role,
 * hears a regain, records the capability, logs and stops. */
export interface LegionClaimHost {
  readonly daemon: LegionDaemonClient;
  /** `claims/register`'s body: the pane's boot token, the session, its transcript, and the
   * contract this build speaks (`pluginContract`, the package's `legion.daemonApiVersion`). */
  readonly registration: RegisterInput;
  /** The capability report `claims/ready` carries, measured from the registration's answer on,
   * beside the steps after it. A rejection costs the report, never the claim. */
  readonly measure?: (claim: LegionRegistration) => Promise<LegionCapabilityReportBody>;
  /** Runs once the registration answered, before the capability is recorded. */
  readonly afterRegister?: (claim: LegionRegistration) => Promise<void>;
  /** Records the registered claim. It is recorded before `claims/ready`: the daemon's task can
   * arrive the moment ready answers, and what serves it reads the capability then. */
  readonly setCapability: (capability: ClaimCapability) => void;
  /** Claims `role`, the claim token, on Envoy for this session: the topic the daemon sends the
   * claim's task and every notice on. Throws when the role cannot be held. */
  readonly claimRole: (role: string) => Promise<void>;
  /** Sets the one hook the Envoy heartbeat runs after it re-establishes this session as a role's
   * holder, with why it did (`reclaimed`, `reregistered`). */
  readonly onRoleRegained: (listener: (role: string, reason: string) => Promise<void>) => void;
  /** Writes one line to the agent's log. */
  readonly log: (line: string) => void;
  /** What `stop` does, as a log line names it: `exiting so the daemon launches it again`. */
  readonly stopping: string;
  /** Ends the claim's agent, so the daemon launches it again. */
  readonly stop: () => void;
}

/**
 * Boots a claim on `/legion/v1/claims/*`, the same on every harness: `claims/register`; the
 * capability measurement started (`measure`); `afterRegister`; the capability recorded; the Envoy
 * role claimed; `claims/ready`, awaited on the measurement and carrying its report, retried on a
 * 5xx or transport failure (`callReadyWithRetry`); and, once ready answered, the same ready again
 * each time the heartbeat regains the role.
 *
 * A registration the daemon refuses (a 4xx: a spent boot token, another contract) and any failure
 * after a registration are logged and `stop` the agent, since neither changes on retry. Any other
 * refusal of the registration (a 5xx) is logged and rejects, as a request that never reached the
 * daemon does unlogged: both leave the agent to the daemon's registration deadline.
 */
export async function bootLegionClaim(host: LegionClaimHost): Promise<void> {
  const { daemon } = host;
  let claim: LegionRegistration;
  try {
    claim = await daemon.register(host.registration);
  } catch (error) {
    if (!(error instanceof LegionDaemonApiError)) throw error;
    host.log(`claims/register registration failed (${error.status}): ${error.detail}`);
    if (error.status < 400 || error.status >= 500) throw error;
    host.stop();
    return;
  }
  const { measure } = host;
  const measuring =
    measure === undefined
      ? undefined
      : Promise.resolve()
          .then(() => measure(claim))
          .catch((error: unknown) => {
            host.log(
              `capability measurement failed; ready carries no report: ${messageFor(error)}`
            );
            return undefined;
          });

  try {
    await host.afterRegister?.(claim);
    host.setCapability({
      sessionID: host.registration.sessionId,
      tree: claim.tree,
      issue: claim.issue,
      role: claim.role,
      secret: claim.secret,
    });
    await host.claimRole(claim.claimToken);
    const capabilities = await measuring;
    const ready: ReadyInput = {
      claimToken: claim.claimToken,
      sessionId: host.registration.sessionId,
      secret: claim.secret,
      generation: claim.generation,
      ...(capabilities === undefined ? {} : { capabilities }),
    };
    await callReadyWithRetry("claims/ready", () => daemon.ready(ready));
    host.onRoleRegained(async (role, reason) => {
      if (role !== claim.claimToken) return;
      try {
        await callReadyWithRetry("claims/ready after role regain", () => daemon.ready(ready));
        host.log(`re-ran claims/ready after role ${role} was ${reason}`);
      } catch (error) {
        host.log(
          `claims/ready after role ${role} was ${reason} failed; the daemon's queued task stays undelivered until the next regain or boot: ${messageFor(error)}`
        );
      }
    });
  } catch (error) {
    host.log(
      `claim boot failed after claims/register registered ${claim.claimToken}; ${host.stopping}: ${messageFor(error)}`
    );
    host.stop();
  }
}
