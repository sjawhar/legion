import {
  controllerToken,
  legionControllerNoticeSubject,
  legionProjectToken,
} from "@legion/contracts";
import type { LegionGrant } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import pkg from "../../package.json";
import type { CommandContext, PiApi, SessionContext } from "../pi-types";
import { recordBootstrappedSession } from "../subagent-session";
import { classifySession, requiredControllerCapability, requiredEnvironment } from "./classify";
import { LegionDaemonApiError, type LegionDaemonClient } from "./daemon-client";
import { claimEnvoyRole, subscribeLegionNotice } from "./role-claim-bridge";

type PersistedTranscript = (
  context: CommandContext | SessionContext
) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;

export interface ControllerSession {
  readonly claim: (context: CommandContext | SessionContext) => Promise<void>;
  readonly handleSessionStart: (context: SessionContext) => Promise<void>;
  readonly reclaimAfterSessionChange: (context: SessionContext) => Promise<void>;
  readonly isClaimedSession: (sessionID: string) => boolean;
  readonly mintGrant: (sessionID: string) => Promise<LegionGrant>;
}

/**
 * Owns the controller session's identity, transcript, role claim, and grant path. This is
 * deliberately separate from the claim session (`claim-session.ts`) and the extension's
 * tool-routing policy: a controller holds no claim, and its session can change in place.
 */
export function createControllerSession(deps: {
  readonly daemon: () => LegionDaemonClient;
  readonly persistedTranscript: PersistedTranscript;
  readonly pi: PiApi;
}): ControllerSession {
  const { daemon, persistedTranscript, pi } = deps;
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let mintControllerGrant: (() => Promise<LegionGrant>) | undefined;
  // Whether this process has ever sent LEGION_CONTROLLER_START_MESSAGE: the gate is its own flag
  // rather than `controllerSessionID`, which a claim that throws after the send (the subscription
  // below, say) never reaches setting, so a retry would otherwise send it again.
  let startMessageSent = false;
  // The controller's own transcript as of the last successful claim, which a session navigation
  // compares to decide whether to claim again; a hand-started takeover records none.
  let controllerTranscript: string | undefined;

  /**
   * Makes the context's session the controller: it registers on the claim route with the
   * capability `legion controller start` fetched in place of a boot token, then claims the role
   * token the registration names (`legion-<project>-controller`), then subscribes to that
   * project's controller topic (`notifications.legion.<project>.controller`,
   * `notify.ControllerTopic` in the daemon, which lists what it carries) for as long as it holds
   * the role: a later `legion controller start` takes the role, and Envoy closes the subscription
   * at this session's next heartbeat. The project is `LEGION_PROJECT`, and it must be the
   * daemon's, or the topic would be one nobody publishes on. Both are settled before registering,
   * since a registration replaces the running controller's session and secret: an unset
   * `LEGION_PROJECT` stops the claim, and so does one whose controller role is not that of the
   * project `GET /legion/v1/state` names (`legionProjectToken`, the rule the daemon applies to its
   * own). The registration's claim token is compared once more after it, the daemon's own answer.
   * A session `legion controller start` launched (`launched`, never a hand-started takeover) reads
   * its LEGION_CONTROLLER_START_MESSAGE before any of that — a missing value refuses here, before
   * registering replaces the running controller's session and claims its role, rather than
   * leaving a half-claimed controller behind a refusal found only after. It is sent as this
   * session's first turn right after the role claim and before the subscription below opens:
   * `pi.sendUserMessage` runs the skill's start procedure deterministically, with nothing typed,
   * rather than leaving it to race a wake for the session's one first-turn slot (LEGION-392 found
   * that race — Oh My Pi's own positional-argument first message can lose it to an Envoy notice
   * delivered during this same async claim). `startMessageSent` — not `controllerSessionID`, which
   * a claim that throws after the send (the subscription below, say) never reaches setting — keeps
   * this process from sending it twice: a retried claim after such a failure, and a later
   * `/legion-claim-controller` in the same session, send nothing more.
   * The subscription is a live wake only: an Oh My Pi session subscribes over core NATS, so a
   * notice published while no controller runs never reaches one, and the controller skill reads
   * `legion state` and Dispatch's triage listing at boot for what it missed. Its grants are minted
   * with the registration's own secret, which a later `legion controller start` revokes. Nothing
   * re-runs on a role regain: the daemon holds nothing for a controller, and the Envoy heartbeat
   * keeps the role itself. A refusal is logged and propagates without exiting — the operator
   * started this session and reads it; a refused capability was replaced by a later start. The
   * transcript is reported on every claim, a takeover's included, because the route requires one;
   * the daemon records only the session.
   */
  const claim = async (context: CommandContext | SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    const capability = controllerCapability ?? requiredControllerCapability(process.env);
    controllerCapability = capability;
    const launched = classifySession(process.env).kind === "controller";
    // Read and validate before anything below mutates daemon-side state (the registration that
    // replaces the running controller, the role claim): a missing value refuses here, leaving the
    // previous controller, if any, still running and still registered.
    const startMessage = launched
      ? requiredEnvironment(process.env, "LEGION_CONTROLLER_START_MESSAGE")
      : undefined;
    // The controller's own transcript, which persistedTranscript puts on disk, is recorded so the
    // controller's own `task` subagents are recognised even when the transcript is not a file on
    // disk; a hand-started takeover records nothing.
    const { sessionFile, agentId } = await persistedTranscript(context);
    if (launched) recordBootstrappedSession(sessionFile);

    const project = requiredEnvironment(process.env, "LEGION_PROJECT");
    const client = daemon();
    const { daemon: served } = await client.state();
    const servedRole = controllerToken(legionProjectToken(served.project, "the daemon's project"));
    if (controllerToken(project) !== servedRole) {
      throw new Error(
        `LEGION_PROJECT ${project} names controller role ${controllerToken(project)}, but the daemon at ${requiredEnvironment(process.env, "LEGION_DAEMON_URL")} serves project ${served.project}, whose controller role is ${servedRole}`
      );
    }
    const registration = await client
      .registerController({
        bootToken: capability,
        sessionId: sessionID,
        ompSessionFile: sessionFile,
        agentId,
        pluginContract: pkg.legion.daemonApiVersion,
      })
      .catch((error: unknown) => {
        if (error instanceof LegionDaemonApiError) {
          console.error(
            `[legion] claims/register for the controller failed (${error.status}): ${error.detail}`
          );
        }
        throw error;
      });
    if (controllerToken(project) !== registration.claimToken) {
      throw new Error(
        `LEGION_PROJECT ${project} names controller role ${controllerToken(project)}, but the daemon registered this controller as ${registration.claimToken}`
      );
    }
    const envoyContext = "setInterval" in context ? context : undefined;
    await claimEnvoyRole(sessionID, registration.claimToken, envoyContext);
    // Send once per process, ever: before the subscription below can deliver an external wake
    // that would otherwise race it for the one first-turn slot (LEGION-392). A hand-started
    // takeover (`startMessage` undefined) sends nothing: the operator's own session speaks for
    // itself.
    if (startMessage !== undefined && !startMessageSent) {
      pi.sendUserMessage(startMessage);
      startMessageSent = true;
    }
    await subscribeLegionNotice(
      sessionID,
      legionControllerNoticeSubject(project),
      envoyContext,
      registration.claimToken
    );
    mintControllerGrant = () =>
      client.controllerGrant({ sessionId: sessionID, secret: registration.secret });
    controllerSessionID = sessionID;
    controllerTranscript = launched ? sessionFile : undefined;
  };

  const handleSessionStart = async (context: SessionContext): Promise<void> => {
    if (classifySession(process.env).kind !== "controller") return;
    const sessionID = context.sessionManager.getSessionId();
    if (controllerSessionID === undefined || controllerSessionID === sessionID) {
      await claim(context);
    }
  };

  /**
   * `/new`, `/resume`, or `/fork` can replace the controller session and its transcript in place.
   * The caller never passes a `task` subagent's session (legion.ts `afterSessionChange`).
   */
  const reclaimAfterSessionChange = async (context: SessionContext): Promise<void> => {
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

  const mintGrant = async (sessionID: string): Promise<LegionGrant> => {
    if (controllerSessionID !== sessionID || mintControllerGrant === undefined) {
      throw new Error("Controller session is not registered; cannot mint its grant");
    }
    return mintControllerGrant();
  };

  return { claim, handleSessionStart, reclaimAfterSessionChange, isClaimedSession, mintGrant };
}
