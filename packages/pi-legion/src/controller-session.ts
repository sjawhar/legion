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
  readonly sendStartMessage: (context: CommandContext) => Promise<void>;
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
  // rather than `controllerSessionID`, which a claim that throws after the start is due or sent
  // (the subscription below, say) never reaches setting, so a retry would otherwise send it again.
  let startMessageSent = false;
  // The start message the claim at Oh My Pi's startup made due, kept for `sendStartMessage`
  // (`/legion-controller-start`) to send once the startup wait is over; undefined once sent.
  let dueStartMessage: string | undefined;
  // The claim `handleSessionStart` runs during Oh My Pi's startup, while it runs: a handler past
  // Oh My Pi's handler timeout keeps running, so `/legion-controller-start` can arrive before it
  // ends and waits for it.
  let startupClaim: Promise<void> | undefined;
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
   * (LEGION-392). The claim run at Oh My Pi's startup (`handleSessionStart`) does not send it:
   * Oh My Pi holds every send a `session_start` handler makes and drains them all at the end of
   * its startup, so a turn started there runs inside the startup wait, before the TUI shows it and
   * before any input is read. Instead, right after the role claim and before the subscription
   * below opens, it makes the message due and marks this session in the Envoy plugin interface's
   * `startupTurn`, which keeps the Envoy entry's startup wake behind the start turn.
   * `legion controller start` puts `/legion-controller-start` on Oh My Pi's command line, which
   * Oh My Pi runs after its startup and once the TUI listens; that command (`sendStartMessage`)
   * sends the due message. A claim run after startup (`/legion-claim-controller`, a re-claim after
   * a session change) is outside the startup wait and sends a message still unsent itself.
   * `startMessageSent` — not `controllerSessionID`, which a claim that throws after the start is
   * due (the subscription below, say) never reaches setting — keeps this process from sending it
   * twice: a retried claim after such a failure, and a later `/legion-claim-controller` in the
   * same session, send nothing more.
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
   * a capability: it registers with that token, as every pane does, subscribes to each Slack
   * channel its launch names (`daemonControllerSlackChannels`) for as long as it holds the role,
   * and once it holds the role and the topics it reports ready on `claims/ready`, which is when the
   * daemon hands it the start message; it marks `startupTurn` as sent right before that report.
   * Any step of its claim that fails exits Oh My Pi, so the daemon relaunches it, as a pane's
   * failed boot does. A root architect's or phase worker's pane carries a boot token too, its own
   * claim's: `/legion-claim-controller` run there is a takeover by hand like any other, which needs
   * the capability and stops before any daemon call without it.
   */
  const claim = (context: CommandContext | SessionContext): Promise<void> =>
    claimFrom(context, false);

  const claimFrom = async (
    context: CommandContext | SessionContext,
    atStartup: boolean
  ): Promise<void> => {
    const supervised = daemonLaunchedController(process.env);
    if (!supervised) return claimWith(context, false, atStartup);
    try {
      await claimWith(context, true, atStartup);
    } catch (error) {
      console.error(
        `[legion] the daemon-launched controller's claim failed; exiting so the daemon launches it again: ${messageFor(error)}`
      );
      deps.exitProcess(1);
    }
  };

  const recordStartMessageSent = (): void => {
    startMessageSent = true;
    dueStartMessage = undefined;
    const mark = envoyPluginInterface().startupTurn;
    if (mark.session !== undefined) mark.sentAt = Date.now();
  };

  const claimWith = async (
    context: CommandContext | SessionContext,
    supervised: boolean,
    atStartup: boolean
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
    await claimEnvoyRole(sessionID, registration.claimToken, envoyContext);
    // Once per process, ever, and before the subscription below can deliver an external wake that
    // would otherwise race the start turn for the one first-turn slot (LEGION-392). At Oh My Pi's
    // startup the message is only made due, and the mark holds the Envoy entry's startup wake
    // until `/legion-controller-start` sends it after the startup wait; a claim after startup
    // sends it here. A hand-started takeover (`startMessage` undefined) sends nothing: the
    // operator's own session speaks for itself. Nor does the daemon's own controller, whose start
    // message the daemon delivers once it reports ready below.
    if (startMessage !== undefined && !startMessageSent) {
      if (atStartup) {
        dueStartMessage = startMessage;
        const mark = envoyPluginInterface().startupTurn;
        mark.session = sessionID;
        mark.sentAt = undefined;
      } else {
        pi.sendUserMessage(startMessage);
        recordStartMessageSent();
      }
    }
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

  const handleSessionStart = async (context: SessionContext): Promise<void> => {
    if (classifySession(process.env).kind !== "controller") return;
    const sessionID = context.sessionManager.getSessionId();
    if (controllerSessionID === undefined || controllerSessionID === sessionID) {
      const running = claimFrom(context, true);
      startupClaim = running;
      try {
        await running;
      } finally {
        if (startupClaim === running) startupClaim = undefined;
      }
    }
  };

  /**
   * `/legion-controller-start`: sends the start message the claim at Oh My Pi's startup made due,
   * as the session's first turn — or, when the operator started a turn first, queued behind it as
   * a follow-up, which starts no turn of its own. A startup claim still running is waited for.
   */
  const sendStartMessage = async (context: CommandContext): Promise<void> => {
    const pending = startupClaim;
    if (pending !== undefined) await pending.catch(() => undefined);
    if (dueStartMessage !== undefined) {
      const message = dueStartMessage;
      if (context.isIdle?.() === false) pi.sendUserMessage(message, { deliverAs: "followUp" });
      else pi.sendUserMessage(message);
      recordStartMessageSent();
      return;
    }
    if (
      !startMessageSent &&
      classifySession(process.env).kind === "controller" &&
      !daemonLaunchedController(process.env)
    ) {
      context.ui.notify(
        "legion: the controller claim failed, so no start message was sent. Run /legion-claim-controller to claim the controller again; it sends the start message once the claim succeeds.",
        "warning"
      );
      return;
    }
    context.ui.notify(
      "legion: no controller start message to send: it was already sent, or this session has none.",
      "info"
    );
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
    sendStartMessage,
    isClaimedSession,
    mintGrant,
  };
}
