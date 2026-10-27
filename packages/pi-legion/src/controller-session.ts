import {
  controllerToken,
  legionControllerNoticeSubject,
  legionProjectToken,
  slackMentionTopic,
} from "@legion/contracts";
import type { LegionGrant } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import { LegionDaemonApiError, type LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { CommandContext, PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { claimEnvoyRole, subscribeLegionNotice } from "@legion/pi-shared/role-claim-bridge";
import { markStartupTurnSent } from "@legion/pi-shared/startup-turn";
import { recordBootstrappedSession } from "@legion/pi-shared/subagent-session";
import { logger } from "@oh-my-pi/pi-utils";
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
 * The controller `legion controller start` launched for the operator: the controller marker
 * (`LEGION_CONTROLLER=1`) without the boot token of a controller the daemon launched itself. It
 * claims from `/legion-controller-start` and sends its own start message.
 */
function operatorLaunchedController(env: NodeJS.ProcessEnv): boolean {
  return classifySession(env).kind === "controller" && !daemonLaunchedController(env);
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
  // The session the daemon last registered as the controller and the grant minter for its
  // registration's secret, set as soon as that registration is in: the `legion` tool answers for
  // that session from then on, the start turn included, whatever the rest of the claim does.
  let controllerSessionID: string | undefined;
  let controllerCapability: string | undefined;
  let mintControllerGrant: (() => Promise<LegionGrant>) | undefined;
  // Whether this process has ever sent LEGION_CONTROLLER_START_MESSAGE.
  let startMessageSent = false;
  // Whether the claim that made the last registration went on to hold the role and its topics: a
  // claim that fails after its registration leaves the session registered, with no role or wakes.
  let claimComplete = false;
  // The controller's own transcript as of the last registration, which a session navigation
  // compares to decide whether to claim again; a hand-started takeover records none.
  let controllerTranscript: string | undefined;
  // Every claim, and every `/legion-controller-start` decision, runs after the one before it:
  // `serialised` chains each onto this, and counts the ones queued or running.
  let claimChain: Promise<void> = Promise.resolve();
  let claimsQueued = 0;

  const serialised = (task: () => Promise<void>): Promise<void> => {
    claimsQueued += 1;
    const run = claimChain.then(task).finally(() => {
      claimsQueued -= 1;
    });
    claimChain = run.catch(() => undefined);
    return run;
  };

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
   * Once the registration is in, the session is the controller to the `legion` tool: its grants
   * are minted with the registration's secret from then on, before the role claim, so the start
   * turn's `read_state` and `set_status` work while the rest of the claim runs, and after it fails.
   *
   * The operator-launched controller (`operatorLaunchedController`) reads its
   * LEGION_CONTROLLER_START_MESSAGE before any of that — a missing value refuses here, before
   * registering replaces the running controller's session and claims its role, rather than
   * leaving a half-claimed controller behind a refusal found only after. That message runs the
   * skill's start procedure as the session's first turn, with nothing typed, ahead of any Envoy
   * card or Legion notice that would otherwise race it for the session's one first-turn slot
   * (LEGION-392). It is sent once the registration is in, right before the role claim, so before
   * anything can reach the session on the role's lane or the controller topic. Not after the role
   * claim: the listener forwards a card on the role's lane as soon as it records the role, and
   * that card can reach the session before the role claim's own answer does, which on a pi-envoy
   * that steers every card would start the card's turn first. A role claim refused after the send
   * leaves the start message sent, its turn holding the registration, and the claim's refusal is
   * the operator's to see.
   *
   * That order holds only outside Oh My Pi's startup, so this controller claims from
   * `/legion-controller-start` (`startCommand`), never from `session_start`. Oh My Pi holds every
   * send a `session_start` handler makes and drains them all at the end of its startup, while an
   * Envoy card is a steer that starts its own turn: a role claim made during startup lets a card
   * on the role's lane, or a notice on the controller topic, start its turn ahead of the held
   * start message, and a turn started there runs inside the startup wait, before the TUI shows it
   * and before any input is read. `legion controller start` puts the command on Oh My Pi's
   * command line, which Oh My Pi runs after its startup and once the TUI listens, so the role's
   * lane and the controller topic open only once the start message is queued. A claim from
   * `/legion-claim-controller`, or a re-claim after a session change, sends a message still
   * unsent the same way. A send to an idle session marks the session current at the send in the
   * Envoy plugin interface's `startupTurn` right before it; a send queued behind a turn the
   * operator already started marks nothing, since that run, the session's first, has already
   * started. `startMessageSent` keeps this process from sending it twice: a retried claim after a
   * failure past the send, and a later `/legion-claim-controller` in the same session, send
   * nothing more.
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
   *
   * One claim runs at a time, in the order they were asked for: each registration replaces the
   * controller secret the daemon holds, so two claims racing could leave this session holding the
   * secret a later registration replaced, and every controller grant refused. Oh My Pi reads the
   * terminal while `/legion-controller-start`'s claim runs, so `/legion-claim-controller` or the
   * command itself can be typed before it ends. A claim that has to wait says so once, since the
   * daemon client sets no request timeout and a hung claim would otherwise hold it silently.
   */
  const claim = (context: CommandContext | SessionContext): Promise<void> => {
    if (claimsQueued > 0) {
      context.ui.notify(
        "legion: waiting for the controller claim already running to finish.",
        "info"
      );
    }
    return serialised(() => claimOnce(context));
  };

  const claimOnce = async (context: CommandContext | SessionContext): Promise<void> => {
    const supervised = daemonLaunchedController(process.env);
    if (!supervised) return claimWith(context, false);
    try {
      await claimWith(context, true);
    } catch (error) {
      logger.error(
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
    // launched carries none: the daemon delivers its start message at each ready.
    const startMessage = operatorLaunchedController(process.env)
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
          logger.error(
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
    // The daemon now holds this registration, and only its secret mints a controller grant: the
    // `legion` tool answers for this session with it from here on, before the start message below
    // starts a turn that calls the tool, and whether or not the role claim after it succeeds.
    claimComplete = false;
    mintControllerGrant = () =>
      client.controllerGrant({ sessionId: sessionID, secret: registration.secret });
    controllerSessionID = sessionID;
    controllerTranscript = launched ? sessionFile : undefined;
    const envoyContext = "setInterval" in context ? context : undefined;
    // Once per process, ever, and before the role claim and the subscription below, so no card on
    // the role's lane and no wake on the controller topic can race the start turn for the one
    // first-turn slot (LEGION-392): a card the listener forwards as soon as it records the role can
    // arrive before the role claim resolves here. The session's first turn when it is idle, marked
    // as the startup turn of the session current at the send (a `/new` may have run since the
    // registration). When the operator already started a turn, queued behind it as a follow-up,
    // which starts no turn of its own and runs inside that run, whose `agent_start` has already
    // fired, so nothing is marked. A hand-started takeover (`startMessage` undefined) sends
    // nothing: the operator's own session speaks for itself. Nor does the daemon's own controller,
    // whose start message the daemon delivers once it reports ready below.
    if (startMessage !== undefined && !startMessageSent) {
      if ("isIdle" in context && !context.isIdle()) {
        pi.sendUserMessage(startMessage, { deliverAs: "followUp" });
      } else {
        markStartupTurnSent(context.sessionManager.getSessionId());
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
      markStartupTurnSent(sessionID);
      await client.ready({
        claimToken: registration.claimToken,
        sessionId: sessionID,
        secret: registration.secret,
        generation: registration.generation,
      });
    }
    claimComplete = true;
  };

  /**
   * Only the controller the daemon launched claims at `session_start`: it has no start message of
   * its own to queue ahead of its role's lane, and its claim ends in the ready at which the daemon
   * sends it one. The operator-launched controller claims from `/legion-controller-start`
   * (`startCommand`), after Oh My Pi's startup, and until its registration is in, its `legion`
   * tool answers that the session holds no registered claim.
   */
  const handleSessionStart = async (context: SessionContext): Promise<void> => {
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
   * claim. A refusal anywhere in it is logged and shown to the operator as one warning, which says
   * what the claim left: refused before the send, nothing was sent, and `/legion-claim-controller`
   * claims again and sends the start message once that claim succeeds; refused after it (the role
   * claim or the subscription), the start turn runs holding the registration, and
   * `/legion-claim-controller` claims the role and its wakes again. The command itself never fails
   * past that notice. Run again once the start message is sent — after a claim that succeeded, or
   * one that failed past the send — it claims nothing: there is nothing to do, or, while the claim
   * is unfinished, a warning naming `/legion-claim-controller`. A hand-started takeover and the
   * controller the daemon launched (which claimed at `session_start`) have nothing to do either,
   * and the command says so. Oh My Pi reads the terminal while the claim runs, so the operator can
   * run the command again before it ends: that run decides after the claim in flight.
   */
  const startCommand = (context: CommandContext): Promise<void> =>
    serialised(async () => {
      if (!operatorLaunchedController(process.env) || claimComplete) {
        context.ui.notify(
          "legion: nothing to do: the controller claim already succeeded in this process, or this session claims no controller from this command (a hand-started takeover, or the controller the daemon launched).",
          "info"
        );
        return;
      }
      if (startMessageSent) {
        context.ui.notify(
          "legion: this process already sent the start message, but its controller claim did not finish; run /legion-claim-controller to claim the controller role and its wakes again.",
          "warning"
        );
        return;
      }
      try {
        await claimOnce(context);
      } catch (error) {
        const message = messageFor(error);
        logger.error(`[legion] the controller claim failed: ${message}`);
        context.ui.notify(
          startMessageSent
            ? `legion: the controller claim failed after the start message was sent: ${message}. The start turn has the controller's registration; run /legion-claim-controller to claim the controller role and its wakes again.`
            : `legion: the controller claim failed: ${message}. Run /legion-claim-controller to claim again; it sends the start message once the claim succeeds.`,
          "warning"
        );
      }
    });

  /**
   * `/new`, `/resume`, or `/fork` can replace the controller session and its transcript in place.
   * The caller never passes a `task` subagent's session (legion.ts `afterSessionChange`). A session
   * that is still the one last registered, on the same transcript, claims again only when that
   * claim did not finish.
   */
  const reclaimAfterSessionChange = async (context: SessionContext): Promise<void> => {
    if (classifySession(process.env).kind !== "controller") return;
    if (
      claimComplete &&
      context.sessionManager.getSessionId() === controllerSessionID &&
      context.sessionManager.getSessionFile() === controllerTranscript
    ) {
      return;
    }
    try {
      await claim(context);
    } catch (error) {
      // A claim refused after its registration left this session registered: its grants mint.
      const registered = controllerSessionID === context.sessionManager.getSessionId();
      context.ui.notify(
        registered
          ? `legion: re-claiming the controller for this session failed (${messageFor(error)}). The session keeps its controller registration, so the \`legion\` tool's set_status still works, but until a re-claim succeeds controller wakes and merges will not reach this session.`
          : `legion: re-claiming the controller for this session failed (${messageFor(error)}). Until a re-claim succeeds, controller wakes and merges will not reach this session, and the \`legion\` tool's set_status and its other operations that call the daemon have no controller grant to mint and fail.`,
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
