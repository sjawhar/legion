import {
  controllerToken,
  legionControllerNoticeSubject,
  legionProjectToken,
  slackMentionTopic,
} from "@legion/contracts";
import type { LegionGrant } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import { LegionDaemonApiError, type LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import { envoyPluginInterface } from "@legion/pi-shared/interface";
import type { CommandContext, PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { claimEnvoyRole, subscribeLegionNotice } from "@legion/pi-shared/role-claim-bridge";
import { recordBootstrappedSession } from "@legion/pi-shared/subagent-session";
import pkg from "../package.json";
import {
  classifySession,
  requiredControllerCapability,
  requiredEnvironment,
  requiredSecret,
} from "./classify";
import { daemonControllerSlackChannels, daemonLaunchedController } from "./slack-channels";

type PersistedTranscript = (
  context: CommandContext | SessionContext
) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;

export interface ControllerSession {
  readonly claim: (context: CommandContext | SessionContext) => Promise<void>;
  readonly handleSessionStart: (context: SessionContext) => Promise<void>;
  readonly reclaimAfterSessionChange: (context: SessionContext) => Promise<void>;
  readonly startCommand: (context: CommandContext) => Promise<void>;
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
  readonly exitProcess: (code: number) => never;
}): ControllerSession {
  const { daemon, persistedTranscript, pi } = deps;
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let mintControllerGrant: (() => Promise<LegionGrant>) | undefined;
  // Whether this process has ever sent LEGION_CONTROLLER_START_MESSAGE: the gate is its own flag
  // rather than `controllerSessionID`, which a claim that throws after the start is sent (the
  // subscription below, say) never reaches setting, so a retry would otherwise send it again.
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
   *
   * A session `legion controller start` launched (`launched`, never a hand-started takeover) reads
   * its LEGION_CONTROLLER_START_MESSAGE before any of that — a missing value refuses here, before
   * registering replaces the running controller's session and claims its role, rather than
   * leaving a half-claimed controller behind a refusal found only after. That message runs the
   * skill's start procedure as the session's first turn, with nothing typed, ahead of any Envoy
   * card or Legion notice that would otherwise race it for the session's one first-turn slot
   * (LEGION-392). It is sent once the registration is in, right before the role claim, so before
   * anything can reach the session on the role's lane or the controller topic. Not after the role
   * claim: the listener forwards a card on the role's lane as soon as it records the role, and
   * that card can reach the session before the role claim's own answer does, which on a pi-envoy
   * that steers every card would start the card's turn first. A role claim refused after the send
   * leaves the start message sent, and the claim's refusal is the operator's to see.
   *
   * That order holds only outside Oh My Pi's startup, so this launched controller claims from
   * `/legion-controller-start` (`startCommand`), never from `session_start`. Oh My Pi holds every
   * send a `session_start` handler makes and drains them all at the end of its startup, while an
   * Envoy card is a steer that starts its own turn: a role claim made during startup lets a card
   * on the role's lane, or a notice on the controller topic, start its turn ahead of the held
   * start message, and a turn started there runs inside the startup wait, before the TUI shows it
   * and before any input is read. `legion controller start` puts the command on Oh My Pi's
   * command line, which Oh My Pi runs after its startup and once the TUI listens, so the role's
   * lane and the controller topic open only once the start message is queued. A claim from
   * `/legion-claim-controller`, or a re-claim after a session change, sends a message still
   * unsent the same way. Each send marks this session in the Envoy plugin interface's
   * `startupTurn` as sent right before it. `startMessageSent` — not `controllerSessionID`, which
   * a claim that throws after the send (the role claim or the subscription below, say) never
   * reaches setting — keeps this process from sending it twice: a retried claim after such a
   * failure, and a later `/legion-claim-controller` in the same session, send nothing more.
   *
   * The subscription is a live wake only: an Oh My Pi session subscribes over core NATS, so a
   * notice published while no controller runs never reaches one, and the controller skill reads
   * the daemon's state (the `legion` tool's `read_state`) and Dispatch's triage listing at boot
   * for what it missed. Its grants are minted with the registration's own secret, which a later
   * `legion controller start` revokes. Nothing re-runs on a role regain: the daemon holds nothing
   * for a controller, and the Envoy heartbeat keeps the role itself. A refusal is logged and
   * propagates without exiting — the operator started this session and reads it; a refused
   * capability was replaced by a later start. The transcript is reported on every claim, a
   * takeover's included, because the route requires one; the daemon records only the session.
   *
   * A controller the daemon launched itself (`controller: daemon`) is the controller's own session
   * (`LEGION_CONTROLLER=1`) carrying its launch's boot token (`LEGION_BOOT_TOKEN_FILE`) in place of
   * a capability: it claims at `session_start` (`handleSessionStart`), registers with that token,
   * as every pane does, subscribes to each Slack channel its launch names
   * (`daemonControllerSlackChannels`) for as long as it holds the role, and once it holds the role
   * and the topics it reports ready on `claims/ready`, which is when the daemon hands it the start
   * message; it marks `startupTurn` as sent right before that report. Any step of its claim that
   * fails exits Oh My Pi, so the daemon relaunches it, as a pane's failed boot does. A root
   * architect's or phase worker's pane carries a boot token too, its own claim's:
   * `/legion-claim-controller` run there is a takeover by hand like any other, which needs the
   * capability and stops before any daemon call without it.
   */
  const claim = async (context: CommandContext | SessionContext): Promise<void> => {
    const supervised = daemonLaunchedController(process.env);
    if (!supervised) return claimWith(context, false);
    try {
      await claimWith(context, true);
    } catch (error) {
      console.error(
        `[legion] the daemon-launched controller's claim failed; exiting so the daemon launches it again: ${messageFor(error)}`
      );
      deps.exitProcess(1);
    }
  };

  const claimWith = async (
    context: CommandContext | SessionContext,
    supervised: boolean
  ): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    const capability =
      controllerCapability ??
      (supervised
        ? requiredSecret(process.env, "LEGION_BOOT_TOKEN")
        : requiredControllerCapability(process.env));
    controllerCapability = capability;
    const launched = classifySession(process.env).kind === "controller";
    // Read and validate before anything below mutates daemon-side state (the registration that
    // replaces the running controller, the role claim): a missing value refuses here, leaving the
    // previous controller, if any, still running and still registered. A controller the daemon
    // launched (`supervised`) carries none: the daemon delivers its start message at each ready.
    const startMessage =
      launched && !supervised
        ? requiredEnvironment(process.env, "LEGION_CONTROLLER_START_MESSAGE")
        : undefined;
    // The controller's own transcript, which persistedTranscript puts on disk, is recorded so the
    // controller's own `task` subagents are recognised even when the transcript is not a file on
    // disk; a hand-started takeover records nothing.
    const { sessionFile, agentId } = await persistedTranscript(context);
    if (launched) recordBootstrappedSession(sessionFile);

    const project = requiredEnvironment(process.env, "LEGION_PROJECT");
    // Only the controller the daemon launched can reply in Slack, so only it hears the project's
    // channels; any other ignores whatever LEGION_SLACK_CHANNELS it inherited.
    const slackChannels = supervised ? daemonControllerSlackChannels(process.env) : [];
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
    // Once per process, ever, and before the role claim and the subscription below, so no card on
    // the role's lane and no wake on the controller topic can race the start turn for the one
    // first-turn slot (LEGION-392): a card the listener forwards as soon as it records the role can
    // arrive before the role claim resolves here. The session's first turn when it is idle; when
    // the operator already started a turn, queued behind it as a follow-up, which starts no turn of
    // its own. A hand-started takeover (`startMessage` undefined) sends nothing: the operator's own
    // session speaks for itself. Nor does the daemon's own controller, whose start message the
    // daemon delivers once it reports ready below.
    if (startMessage !== undefined && !startMessageSent) {
      const mark = envoyPluginInterface().startupTurn;
      mark.session = sessionID;
      mark.sentAt = Date.now();
      if ("isIdle" in context && context.isIdle?.() === false) {
        pi.sendUserMessage(startMessage, { deliverAs: "followUp" });
      } else {
        pi.sendUserMessage(startMessage);
      }
      startMessageSent = true;
    }
    await claimEnvoyRole(sessionID, registration.claimToken, envoyContext);
    await subscribeLegionNotice(
      sessionID,
      legionControllerNoticeSubject(project),
      envoyContext,
      registration.claimToken
    );
    // The project's Slack channels, as the daemon that launched this controller names them: each
    // channel's app mentions, for as long as this session holds the controller role, never
    // registered with the listener, so a resume hears the list it is launched with and no other.
    for (const channel of slackChannels) {
      await subscribeLegionNotice(
        sessionID,
        slackMentionTopic(channel),
        envoyContext,
        registration.claimToken
      );
    }
    if (supervised) {
      // The daemon delivers its start prompt at this ready, so the start is sent from here on.
      const mark = envoyPluginInterface().startupTurn;
      mark.session = sessionID;
      mark.sentAt = Date.now();
      await client.ready({
        claimToken: registration.claimToken,
        sessionId: sessionID,
        secret: registration.secret,
        generation: registration.generation,
      });
    }
    mintControllerGrant = () =>
      client.controllerGrant({ sessionId: sessionID, secret: registration.secret });
    controllerSessionID = sessionID;
    controllerTranscript = launched ? sessionFile : undefined;
  };

  /**
   * Only the controller the daemon launched claims at `session_start`: it has no start message of
   * its own to queue ahead of its role's lane, and its claim ends in the ready at which the daemon
   * sends it one. The operator-launched controller claims from `/legion-controller-start`
   * (`startCommand`), after Oh My Pi's startup.
   */
  const handleSessionStart = async (context: SessionContext): Promise<void> => {
    if (classifySession(process.env).kind !== "controller") return;
    if (!daemonLaunchedController(process.env)) return;
    const sessionID = context.sessionManager.getSessionId();
    if (controllerSessionID === undefined || controllerSessionID === sessionID) {
      await claim(context);
    }
  };

  /**
   * `/legion-controller-start`, which `legion controller start` puts on Oh My Pi's command line
   * and Oh My Pi runs once its startup is over and the TUI listens: the operator-launched
   * controller's whole claim (`claim`), which sends the start message right before the role
   * claim. A refusal anywhere in it is logged and shown to the operator as one warning naming
   * `/legion-claim-controller`, which claims again and sends the start message once that claim
   * succeeds, if it is still unsent; the command itself never fails past that notice. Once a claim
   * has succeeded in this process — the command run again — and in a hand-started takeover or the
   * controller the daemon launched (which claimed at `session_start`), there is nothing to do, and
   * the command says so.
   */
  const startCommand = async (context: CommandContext): Promise<void> => {
    if (
      classifySession(process.env).kind !== "controller" ||
      daemonLaunchedController(process.env) ||
      controllerSessionID !== undefined
    ) {
      context.ui.notify(
        "legion: nothing to do: the controller claim already succeeded in this process, or this session claims no controller from this command (a hand-started takeover, or the controller the daemon launched).",
        "info"
      );
      return;
    }
    try {
      await claim(context);
    } catch (error) {
      const message = messageFor(error);
      console.error(`[legion] the controller claim failed: ${message}`);
      context.ui.notify(
        `legion: the controller claim failed: ${message}. Run /legion-claim-controller to claim again; it sends the start message once the claim succeeds.`,
        "warning"
      );
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
        `legion: re-claiming the controller for this session failed (${messageFor(error)}). Until a re-claim succeeds, controller wakes and merges will not reach this session, and the \`legion\` tool's set_status and its other operations that call the daemon have no controller grant to mint and fail.`,
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

  return {
    claim,
    handleSessionStart,
    reclaimAfterSessionChange,
    startCommand,
    isClaimedSession,
    mintGrant,
  };
}
