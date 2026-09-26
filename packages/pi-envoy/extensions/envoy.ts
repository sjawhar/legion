import { existsSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  agentSubject,
  DELIVERY_CAPABILITIES,
  type DeliveryCapability,
  dispatchToolSchema,
  dispatchToolSpecs,
  type OpenAsksResponse,
  ROLE_TOPIC_PREFIX,
  zodSchemaApi,
} from "@legion/contracts";
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults";
import {
  type DispatchDelivery,
  expectsLaneReceipt,
  inboundTimestamp,
  postDeliveryReply,
  rememberBounded,
  renderInbound,
  senderLabel,
} from "@legion/envoy-client/delivery";
import {
  type DispatchConfigResolution,
  resolveDispatchConfig,
} from "@legion/envoy-client/dispatch-config";
import { executeDispatchTool } from "@legion/envoy-client/dispatch-execute";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import {
  createFollowAnnouncer,
  subscriptionRemovedTopics,
} from "@legion/envoy-client/dispatch-subscribe";
import { messageFor } from "@legion/envoy-client/errors";
import { machineID } from "@legion/envoy-client/machine";
import {
  EnvoyToolOperation,
  envoyToolSpecs,
  type MessageMetadataArguments,
  parseEnvoyToolArguments,
  sendConfirmationText,
  toMessageMetadata,
} from "@legion/envoy-client/tool-contract";
import {
  createEnvoyClient,
  EnvoyApiError,
  expandSubscriptionTopics,
  mergeInterestSources,
} from "@legion/envoy-client/transport";
import { logger } from "@oh-my-pi/pi-utils";
import { encode } from "@toon-format/toon";
import { connect, type NatsConnection, StringCodec, type Subscription } from "nats";
import { LOCAL_ENVOY_NOTICE } from "../src/legion/phase-stall";
import {
  type LegionNoticeSubscription,
  type LegionRoleClaim,
  type LegionRoleClaimInstance,
  legionRoleClaimBridge,
  type RoleRegainReason,
} from "../src/legion/role-claim-bridge";
import type { PiApi, SessionContext, SessionSwitchReason, ToolResult } from "../src/pi-types";
import { isRegisteredSubagent, subagentSessionCheck } from "../src/subagent-session";
import { toolFailure, toolSuccess } from "../src/tool-result";
import { registerEnvoyMessageRenderer } from "./envoy-message-renderer";
import { registerEnvoyWhoamiCommand } from "./envoy-whoami-command";

const codec = StringCodec();
const NATS_RETRY_INTERVAL_MS = 15_000;

/**
 * Aside and Steer go through `pi.sendMessage` on every OMP build; BTW only where the host
 * exposes `pi.askEphemeral`, so a host without it advertises every capability but that one.
 */
const CAPABILITIES_WITHOUT_BTW: readonly DeliveryCapability[] = DELIVERY_CAPABILITIES.filter(
  (capability) => capability !== "btw"
);

/**
 * Transcript entry recording the role this session holds. Successful claims
 * write `{ role }`; legacy `{ role: null }` release records remain authoritative
 * when a resumed session decides whether to reclaim.
 */
const ROLE_CLAIM_ENTRY = "envoy-role-claim";

const OPEN_ASKS_TIMEOUT_MS = 3_000;

/**
 * Transcript entry marking a session Legion drives. The process-wide bridge knows the same
 * thing until the process ends; this is what a resumed session reads.
 */
const LEGION_MANAGED_ENTRY = "legion-managed-session";

/** Custom-message type of the run-end nudge itself; never displayed. */
const ASK_REMINDER_MESSAGE = "dispatch-ask-reminder";

/**
 * What the hidden self-check asks the agent, over a snapshot of its own conversation. One word
 * comes back; only WAITING buys the visible turn below. It asks nothing about Dispatch: the
 * extension has already read from Dispatch that nothing is open, the model is not a better
 * witness of that than the API is, and the agent this exists to catch is one that asked the
 * human in chat text — which such an agent can reasonably read as having asked.
 */
const ASK_SELF_CHECK_PROMPT =
  "Your run has just ended. Answer with exactly one word and nothing else: WAITING or PROCEEDING. " +
  "WAITING — you stopped because you need a decision, an approval, or information from a human. " +
  "PROCEEDING — you finished, you will carry on by yourself, or you are waiting only on tools, " +
  "subagents, or events.";

/**
 * A verdict whose first word is WAITING, after any markdown emphasis or quoting — the reply
 * convention the Legion phase-stall follow-up already uses (`src/legion/phase-stall.ts`).
 * Case-sensitive and first-word-only on purpose: "NOT WAITING", "I am WAITING on Sami" and
 * "Waiting." are all PROCEEDING here, because a false WAITING is the expensive error — the
 * steer it buys asserts the agent said it is waiting, which sends it to page a human with a
 * question nobody had, while a false PROCEEDING is only the silence of the status quo.
 */
const WAITING_VERDICT = /^\W*WAITING\b/;

/** The other choice, named anywhere in the reply: the model echoing the question, not answering it. */
const ECHOED_CHOICE = /\bPROCEEDING\b/;

const isWaitingVerdict = (reply: string): boolean =>
  WAITING_VERDICT.test(reply) && !ECHOED_CHOICE.test(reply);

/**
 * Bound on the self-check, imposed by the extension's own clock rather than the host's: it is a
 * whole-context model call on the session's own model, so it costs what a short turn costs; a
 * minute is long enough for a large context on a slow provider and short enough that a wedged
 * one does not hold the stop window open all night. `ENVOY_SELF_CHECK_TIMEOUT_MS` moves it for a
 * deployment whose provider is slower than that — past the bound the check is simply silent, so
 * one set too low turns the nudge off rather than making it wrong.
 */
const ASK_SELF_CHECK_TIMEOUT_MS = 60_000;

/**
 * Most self-checks one armed period pays for. Work re-arms the check, so an unattended run that
 * keeps working would otherwise buy a hidden model call after every settle; five bounds that.
 */
const ASK_CHECKS_PER_PERIOD = 5;

const UNASKED_WAIT_REMINDER =
  "You just said you are waiting on a human, but you have no open ask in Dispatch, so nobody knows you are waiting. Open it now with dispatch_ask — or dispatch_request_approval when what you need is approval of a document — naming exactly what you need and from whom. Do not reply just to acknowledge this reminder.";

/** Tools whose success means the agent opened the ask itself, so the nudge has nothing to say. */
const ASK_OPENING_TOOLS: readonly string[] = ["dispatch_ask", "dispatch_request_approval"];

/** Name prefix of every native Dispatch tool: talking to the humans, not the work itself. */
const DISPATCH_TOOL_PREFIX = "dispatch_";

/**
 * One arming period of the run-end nudge. A genuine user turn arms a period. `check_due` is the
 * outstanding check, in the shape of the host's own todo reminder: arming owes one, a completed
 * check spends it, the agent's next real work — a successful tool call that is not a Dispatch
 * write — owes another, and opening the ask itself spends it, since the nudge has nothing left
 * to say about that stop. A turn that only replies and stops does no work, so the nudge's own
 * continuation can never owe one, which is what keeps it from nudging itself forever. `checks`
 * counts the ones spent, capped at `ASK_CHECKS_PER_PERIOD`.
 *
 * `baseline_as_of` is the server clock the period's next Dispatch read asks from, and it moves:
 * every settle that resolves — silently, because an ask is open, or by spending a check —
 * carries it to that snapshot's `as_of`. Pinned to the arming turn it would never move, and
 * `opened_since` counts every ask this session authored after it, open or long answered, so one
 * ask would silence the rest of the period however long the session lived. A standing session
 * is woken by a human's answer through Envoy, which arms no period, so the window is the only
 * thing that can let the nudge speak again.
 *
 * In-memory only, for the life of this process's session. A cold start or a session change
 * begins at period 0, which the stop guard refuses, so nothing nudges before the next genuine
 * user turn arms a period — no transcript entry buys anything beyond that.
 */
interface AskAwarenessState {
  readonly session_id: string;
  readonly period: number;
  readonly baseline_as_of: string | null;
  readonly check_due: boolean;
  readonly checks: number;
}

interface LegionManagedEntry {
  readonly type: "custom";
  readonly customType: typeof LEGION_MANAGED_ENTRY;
  readonly data: { readonly session_id: string };
}

function isLegionManagedEntry(entry: unknown): entry is LegionManagedEntry {
  if (typeof entry !== "object" || entry === null) return false;
  if (!("type" in entry) || entry.type !== "custom") return false;
  if (!("customType" in entry) || entry.customType !== LEGION_MANAGED_ENTRY) return false;
  if (!("data" in entry) || typeof entry.data !== "object" || entry.data === null) return false;
  return "session_id" in entry.data && typeof entry.data.session_id === "string";
}

interface RoleClaimEntry {
  readonly type: "custom";
  readonly customType: typeof ROLE_CLAIM_ENTRY;
  readonly data: { readonly role: string | null };
}

interface EstablishSessionOptions {
  readonly carryPreviousSessionRole?: boolean;
}

function isRoleClaimEntry(entry: unknown): entry is RoleClaimEntry {
  if (typeof entry !== "object" || entry === null) return false;
  if (!("type" in entry) || entry.type !== "custom") return false;
  if (!("customType" in entry) || entry.customType !== ROLE_CLAIM_ENTRY) return false;
  if (!("data" in entry) || typeof entry.data !== "object" || entry.data === null) return false;
  if (!("role" in entry.data)) return false;
  return entry.data.role === null || typeof entry.data.role === "string";
}

function resolveSkillsDirectory(): string {
  const moduleDirectory = dirname(fileURLToPath(import.meta.url));
  const candidates = [
    // Packed layout: this file is bundled to dist/envoy.js and prepack stages
    // the repo skills/ directory beside it at dist/skills.
    resolve(moduleDirectory, "skills"),
    // Repo layout: this file runs from packages/pi-envoy/extensions/ and the
    // skills live at the repo root.
    resolve(moduleDirectory, "../../../skills"),
  ];
  const found = candidates.find((directory) => existsSync(directory));
  if (!found) {
    throw new Error(`legion skills directory not found; tried: ${candidates.join(", ")}`);
  }
  return found;
}
const SKILLS_DIRECTORY = resolveSkillsDirectory();

type ActiveDispatchConfig = DispatchConfigResolution & {
  readonly url: string;
  readonly token: string;
};

export default function envoyExtension(pi: PiApi): void {
  logger.debug("extension instance loaded", { extension: import.meta.url });
  const defaults = envoyDefaultsFromEnvironment(process.env);
  // One loader for the shared envoy.json contract: the dispatch tools are
  // registered only where the file names a service at load, and an invalid file
  // is reported at session start, not silently treated as off. The URL and
  // token themselves are re-read on every call (`currentDispatchConfig`) so a
  // Dispatch that moved - a new dispatch.serverUrl in envoy.json - takes effect
  // without /reload-plugins; a file that has since broken fails the call with
  // its own error instead of quietly using the stale endpoint.
  const dispatchConfig = resolveDispatchConfig(process.env, { cwd: process.cwd() });
  const activeDispatchConfig = (): ActiveDispatchConfig | null => {
    const fresh = resolveDispatchConfig(process.env, { cwd: process.cwd() });
    if (fresh.error !== null) throw new Error(`dispatch config: ${fresh.error}`);
    if (!fresh.enabled || fresh.url === null || fresh.token === null) return null;
    return fresh as ActiveDispatchConfig;
  };
  const currentDispatchConfig = (): ActiveDispatchConfig => {
    const config = activeDispatchConfig();
    if (config === null) {
      throw new Error("Dispatch is no longer configured (dispatch.serverUrl/token missing)");
    }
    return config;
  };
  const client = createEnvoyClient({ baseUrl: defaults.envoyUrl, fetch });
  const subscriptions = new Map<string, Subscription>();
  const dedupeKeys = new Set<string>();
  let connection: NatsConnection | undefined;
  let sessionDirectory = "";
  let sessionID = "";
  let heartbeatRegistered = false;
  let claimedRoleTopic: string | undefined;
  let activeSessionContext: SessionContext | undefined;
  const inbox: {
    event_id: string;
    at: string;
    from: string;
    summary: string;
  }[] = [];

  let askAwareness: AskAwarenessState = {
    session_id: "",
    period: 0,
    baseline_as_of: null,
    check_due: false,
    checks: 0,
  };
  // Bumped by everything that invalidates a stop-time check already in flight: a new arming
  // period, and a session change. The period alone cannot carry that — a rebound session's
  // restored period may equal the one the pending check read.
  let awarenessGeneration = 0;
  // One stop-time check at a time. `agent_end` handlers are not awaited by the host, so a second
  // stop can arrive while the first one's Dispatch round trip or self-check is open — it clears
  // every guard below, because `check_due` is not spent until both answers are back. The
  // re-checks after each await then make it silent, but only after it has spent a round trip and
  // a model call on an answer that can change nothing. This makes "one check per stop window"
  // the structure rather than an ordering a re-check happens to win: held from before the query
  // until after `check_due` is spent.
  let askCheckInFlight = false;
  // The self-check in flight, so whatever invalidates it can stop it. Dropping the verdict is
  // not enough: the call is a whole-context request on the session's own model, and one the
  // user superseded by typing competes with the turn they are waiting on for the same provider.
  let askCheckAbort: AbortController | undefined;
  const abortSelfCheck = (reason: string): void => {
    askCheckAbort?.abort(new Error(reason));
    askCheckAbort = undefined;
  };
  // Whether this session's transcript records Legion driving it. Not matched against the id the
  // guard runs under: `/fork` and `/handoff` mint a new id and carry the transcript, and the
  // session stays Legion-driven across one — the role-claim reader beside it is id-agnostic for
  // the same reason. `/new` and `/resume` install a transcript that is their own, so an empty or
  // replaced branch still reads as not-managed.
  let legionManagedTranscript = false;
  /** The bridge set is the process-local record; the transcript is what a fresh process reads. */
  const legionManaged = (id: string): boolean =>
    legionManagedTranscript || legionRoleClaimBridge().managedSessions.has(id);

  const availabilityWarningSessionIDs = new Set<string>();

  const warnAskAvailability = (context: SessionContext, error: unknown): void => {
    const sessionID = context.sessionManager.getSessionId();
    if (availabilityWarningSessionIDs.has(sessionID)) return;
    availabilityWarningSessionIDs.add(sessionID);
    context.ui.notify(
      `envoy: Dispatch open-ask check unavailable (${messageFor(error)}); the stop-time ask reminder is off until it recovers`,
      "warning"
    );
  };

  // The self-check is the whole trigger: a host without `pi.askEphemeral` can never nudge, so,
  // like a headless run and a Legion-driven session, it does not pay the arming round trip
  // either. The capability is the host's for the life of the process, so it is read once.
  const askEphemeral = pi.askEphemeral;
  const overriddenTimeout = Number(process.env.ENVOY_SELF_CHECK_TIMEOUT_MS);
  const selfCheckTimeoutMs =
    Number.isFinite(overriddenTimeout) && overriddenTimeout > 0
      ? overriddenTimeout
      : ASK_SELF_CHECK_TIMEOUT_MS;
  const selfCheckWarningSessionIDs = new Set<string>();

  /** A self-check that failed is silent by design; only the log says it happened. */
  const logSelfCheckFailure = (failedSessionID: string, error: unknown): void => {
    if (selfCheckWarningSessionIDs.has(failedSessionID)) return;
    selfCheckWarningSessionIDs.add(failedSessionID);
    logger.warn("envoy: run-end waiting self-check failed; no reminder was sent", {
      sessionID: failedSessionID,
      error: messageFor(error),
    });
  };

  const queryOpenAsks = async (
    requestedSessionID: string,
    since?: string
  ): Promise<{ readonly snapshot: OpenAsksResponse; readonly url: string } | null> => {
    const config = activeDispatchConfig();
    if (config === null) return null;
    const snapshot = await new DispatchClient(
      config.url,
      config.token,
      fetch,
      AbortSignal.timeout(OPEN_ASKS_TIMEOUT_MS)
    ).openAsks(requestedSessionID, since);
    availabilityWarningSessionIDs.delete(requestedSessionID);
    return { snapshot, url: config.url };
  };

  const markLegionManagedSession = (targetSessionID: string): void => {
    legionRoleClaimBridge().managedSessions.add(targetSessionID);
    if (legionManagedTranscript) return;
    legionManagedTranscript = true;
    pi.appendEntry(LEGION_MANAGED_ENTRY, { session_id: targetSessionID });
  };

  const restoreLocalSessionState = (context: SessionContext): void => {
    sessionDirectory = context.cwd;
    sessionID = context.sessionManager.getSessionId();
    activeSessionContext = context;
    const branch = context.sessionManager.getBranch?.() ?? [];
    // Only a genuine session change clears the armed period — a `/fork`, `/handoff`, resume or
    // switch, each of which mints a different id and leaves the period describing a
    // conversation this session is no longer in. Re-establishing the *same* session is not one:
    // the heartbeat's drift heal runs this for every fresh TUI once the host mints its id, and
    // the `session_start` NATS retry runs it every 15 s for the length of an Envoy outage.
    // Clearing on those disabled the nudge for exactly the sessions it was opened up for.
    if (askAwareness.session_id !== sessionID) {
      abortSelfCheck(`session changed from ${askAwareness.session_id || "none"} to ${sessionID}`);
      askAwareness = {
        session_id: sessionID,
        period: 0,
        baseline_as_of: null,
        check_due: false,
        checks: 0,
      };
      awarenessGeneration++;
    }
    legionManagedTranscript = branch.some(isLegionManagedEntry);
  };

  pi.on("resources_discover", async () => ({ skillPaths: [SKILLS_DIRECTORY] }));
  registerEnvoyMessageRenderer(pi);

  const ensureConnection = async (): Promise<NatsConnection> => {
    if (connection?.isClosed() === false) return connection;
    connection = await connect({
      servers: [...defaults.natsUrls],
      name: `omp-${sessionID || "unknown"}`,
      // Survive NATS drops after the first connection: nats.js re-subscribes
      // existing subscriptions on its own once reconnected.
      reconnect: true,
      maxReconnectAttempts: -1,
      reconnectTimeWait: 2_000,
    });
    return connection;
  };

  const postDispatchReply = async (
    delivery: DispatchDelivery,
    result: { readonly body?: string; readonly error?: string }
  ): Promise<void> => {
    await postDeliveryReply(currentDispatchConfig(), sessionID, delivery, result);
  };

  const deliver = async (subject: string, raw: string, reply: string): Promise<void> => {
    const rendered = renderInbound(raw, sessionID, subject);
    // Acknowledge first. The listener's role lane waits two seconds for this
    // receipt and reports a miss as a failed delivery, which the daemon answers
    // by re-sending the message (LEGION-101); everything below — the inbox
    // update, a Dispatch round trip, the injection — can outlast that window
    // under load. A frame renderInbound throws on never reaches this line, so
    // an undecodable frame is still not acknowledged. A receipt that cannot be
    // published is logged and delivery goes on: the message must not be lost
    // locally because the acknowledgement was.
    const directSubject = agentSubject(sessionID);
    if (
      expectsLaneReceipt({
        subject,
        directSubject,
        envelopeTopic: rendered.envelope?.topic,
        reply,
      })
    ) {
      try {
        (await ensureConnection()).publish(reply);
      } catch (error) {
        console.warn(
          `[envoy] failed to acknowledge envelope ${rendered.envelope?.event_id ?? "unknown"}`,
          error
        );
      }
    }
    // A human unsubscribed one of our topics: drop it locally too, the same
    // way envoy_unsubscribe does, so the resubscribe-on-drop recovery path
    // below does not undo the human's action a few seconds later. The event
    // also reaches the issue's own topic (every subscriber, not just the
    // removed session), so this only fires for a removal naming us.
    for (const topic of subscriptionRemovedTopics(raw, sessionID) ?? []) closeIntentionally(topic);
    const dedupeKey = rendered.envelope?.dedupe_key;
    const duplicate = dedupeKey !== undefined && dedupeKeys.has(dedupeKey);
    // Steering: mid-turn the message is injected at the next tool boundary
    // instead of waiting for the turn to finish; idle it still starts a turn
    // (triggerTurn), so wake-on-message behavior is unchanged.
    if (!duplicate && !rendered.skip) {
      const envelope = rendered.envelope;
      if (envelope !== undefined) {
        inbox.unshift({
          event_id: envelope.event_id ?? "unknown",
          at: inboundTimestamp(envelope.issued_at),
          from: senderLabel(envelope),
          summary: envelope.payload_summary ?? "unknown",
        });
        if (inbox.length > 50) inbox.pop();
      }
      try {
        if (rendered.rejectedDelivery !== undefined) {
          console.warn(
            `[envoy] rejecting malformed Dispatch targeted delivery ${rendered.rejectedDelivery.id}`
          );
          await postDispatchReply(rendered.rejectedDelivery, {
            error: "Invalid Dispatch targeted delivery frame",
          });
        } else if (rendered.malformedDelivery === true) {
          console.warn(
            "[envoy] dropping malformed Dispatch targeted delivery without a reply address"
          );
        } else if (rendered.delivery?.mode === "btw") {
          if (pi.askEphemeral === undefined) {
            await postDispatchReply(rendered.delivery, {
              error: "This OMP host does not support BTW delivery",
            });
          } else {
            try {
              const reply = await pi.askEphemeral({ prompt: rendered.delivery.body });
              await postDispatchReply(rendered.delivery, { body: reply.replyText });
            } catch (error) {
              await postDispatchReply(rendered.delivery, { error: messageFor(error) });
            }
          }
        } else {
          pi.sendMessage(
            { customType: "envoy-message", content: rendered.content, display: true },
            {
              deliverAs: rendered.delivery?.mode === "aside" ? "aside" : "steer",
              triggerTurn: true,
            }
          );
        }
      } catch (error) {
        console.warn(
          `[envoy] failed to deliver envelope ${envelope?.event_id ?? "unknown"}`,
          error
        );
        throw error;
      }
      if (dedupeKey !== undefined) rememberBounded(dedupeKeys, dedupeKey, 1000);
    }
  };

  const RESUBSCRIBE_DELAY_MS = Number(process.env.ENVOY_RESUBSCRIBE_DELAY_MS ?? "") || 5_000;
  let shuttingDown = false;

  // Topics this session closed on purpose: the session-switch rebind and the
  // envoy_unsubscribe tool. The pump's recovery path below exists for
  // connections that died under us, and it cannot tell those two cases apart on
  // its own, because an explicit unsubscribe() ends the async iterator exactly
  // the way a dropped connection does. Without this marker the recovery path
  // resurrects the topic seconds later, so a switched-away session keeps
  // receiving the previous session's traffic and envoy_unsubscribe silently
  // undoes itself.
  const intentionallyClosed = new Set<string>();

  // Topics whose pump is sitting in the resubscribe delay. Such a topic has no
  // entry in `subscriptions`, so this is the only way to know a deliberate close
  // still has something to suppress.
  const awaitingRetry = new Set<string>();

  // Drop a topic we no longer want. Every marker set here is consumed again by
  // the pump end-path, the retry callback, or the next subscribe(), so the set
  // cannot accumulate topics that were never live in the first place.
  const closeIntentionally = (topic: string): boolean => {
    const subscription = subscriptions.get(topic);
    if (subscription === undefined) {
      if (!awaitingRetry.has(topic)) return false;
      intentionallyClosed.add(topic);
      return true;
    }
    intentionallyClosed.add(topic);
    subscription.unsubscribe();
    subscriptions.delete(topic);
    return true;
  };

  const pump = async (topic: string, subscription: Subscription): Promise<void> => {
    try {
      for await (const message of subscription) {
        try {
          await deliver(message.subject, codec.decode(message.data), message.reply ?? "");
        } catch {
          // A failed injection is logged by deliver and must not tear down the
          // subscription; a redelivery can inject it after the host recovers.
        }
      }
    } catch {
      // Iterator failure falls through to the resubscribe path below.
    }
    // Only retire our own generation: a later subscribe() for this topic may
    // already own the map entry.
    if (subscriptions.get(topic) === subscription) subscriptions.delete(topic);
    if (shuttingDown) return;
    // A close we asked for is not an outage. Consume the marker so a later,
    // genuine death of this same topic still recovers.
    if (intentionallyClosed.delete(topic)) return;
    // Otherwise the iterator only ended because the connection was closed or
    // errored out from under nats.js's own reconnect handling. Re-establish
    // rather than staying silently deaf while the HTTP registration heartbeat
    // keeps the session looking healthy in the registry.
    const retry = (delayMs: number): void => {
      awaitingRetry.add(topic);
      setTimeout(() => {
        awaitingRetry.delete(topic);
        // A subscribe() during the delay already cleared the marker and owns the
        // topic, so there is nothing to do here.
        if (shuttingDown || subscriptions.has(topic)) return;
        // A close that landed during the delay leaves the marker for us instead
        // of the pump end-path. Consume it here so it cannot outlive the timer.
        if (intentionallyClosed.delete(topic)) return;
        void subscribe(topic).catch(() => retry(NATS_RETRY_INTERVAL_MS));
      }, delayMs);
    };
    retry(RESUBSCRIBE_DELAY_MS);
  };

  const subscribe = async (topic: string): Promise<boolean> => {
    const subjects = expandSubscriptionTopics([topic]).filter(
      (candidate) => !subscriptions.has(candidate)
    );
    if (subjects.length === 0) return false;
    const activeConnection = await ensureConnection();
    for (const subject of subjects) {
      const subscription = activeConnection.subscribe(subject);
      // A fresh subscription supersedes any earlier deliberate close of this topic.
      intentionallyClosed.delete(subject);
      subscriptions.set(subject, subscription);
      void pump(subject, subscription);
    }
    return true;
  };

  const registerSession = () =>
    client.subscribe({
      sessionID,
      directory: sessionDirectory,
      topics: [...new Set([agentSubject(sessionID), ...subscriptions.keys()])],
      port: 0,
      // Read at every registration: the heartbeat re-registers, which picks up
      // titles assigned after session_start and later renames.
      title: activeSessionContext?.sessionManager.getSessionName?.() ?? "",
      capabilities:
        typeof pi.askEphemeral === "function" ? DELIVERY_CAPABILITIES : CAPABILITIES_WITHOUT_BTW,
      driving: false,
      selfSubscribed: true,
    });

  /**
   * Heartbeat follow-up: make sure the listener still resolves this session as the live holder
   * of `claimedRoleTopic`. The listener can lose sight of a live holder without this process
   * noticing — a claim reaped after this session's registry entry lapsed, NATS data loss, an
   * older listener build. Reads first (`GET /v1/roles/<role>`), so a healthy tick writes
   * nothing; a soft claim goes out only when the listener does not name this session, and the
   * listener's soft-claim rule is the arbiter: a 409 (a different live holder) ends
   * re-assertion for good — the newer holder is correct, so the local claim is dropped exactly
   * as a refused automatic reclaim drops it (`setEnvoyRole`), with no transcript entry either
   * way. A regain (`"reclaimed"`), or the first healthy tick after a failed registration,
   * during which the listener may have answered "no holder" for a claim that survived
   * (`"reregistered"`), fires legion.ts's hook so the role's daemon ready call runs again. The
   * listener calls here propagate to the heartbeat's warn-once path; the hook runs detached
   * from that chain (see below).
   */
  const reassertRole = async (afterOutage: boolean, context: SessionContext): Promise<void> => {
    const topic = claimedRoleTopic;
    if (topic === undefined) return;
    // Both captured at entry: a session rebind or an explicit envoy_role_set
    // racing this tick changes them mid-flight, and that call is the truth.
    const id = sessionID;
    const role = topic.slice(ROLE_TOPIC_PREFIX.length);
    const holder = await client.getRole(role).then(
      (info) => info.holder,
      (error: unknown) => {
        // 404 is the listener's answer for "no live holder", not a failure.
        if (error instanceof EnvoyApiError && error.details.status === 404) return undefined;
        throw error;
      }
    );
    let reason: RoleRegainReason;
    if (holder === id) {
      if (!afterOutage) return;
      reason = "reregistered";
    } else {
      const result = await client.setRole({ sessionID: id, role, soft: true });
      if (claimedRoleTopic !== topic || sessionID !== id) {
        // Superseded while in flight, so a claim that landed here is handed straight back.
        if (result.claimed) await client.unsubscribe({ sessionID: id, topics: [topic] });
        return;
      }
      if (!result.claimed) {
        claimedRoleTopic = undefined;
        logger.warn("envoy: role re-assertion refused; held by another live session", {
          role,
          sessionID: id,
          holder: result.holder,
        });
        context.ui.notify(
          `envoy: role ${role} is now held by session ${result.holder}; this session no longer holds it`,
          "warning"
        );
        return;
      }
      logger.warn("envoy: role re-asserted after the listener lost the claim", {
        role,
        sessionID: id,
        previousHolder: holder ?? null,
      });
      reason = "reclaimed";
    }
    const regained = legionRoleClaimBridge().regained;
    if (regained === undefined) return;
    // Detached from the heartbeat chain: the hook is a daemon round-trip this side cannot bound
    // (`/controller/ready` drains held notices and runs a forced resync), and the chain's
    // `healing` latch must release as soon as registration and the claim are settled, or the
    // next tick could never register. `Promise.resolve().then` also catches a synchronous throw.
    void Promise.resolve()
      .then(() => regained(role, reason))
      .catch((error: unknown) => {
        logger.warn("envoy: role regain hook failed", { role, reason, error: messageFor(error) });
      });
  };

  const ensureHeartbeat = (context: SessionContext): void => {
    if (heartbeatRegistered) return;
    // Never let a heartbeat tick reject unhandled: OMP treats unhandled
    // rejections as fatal (postmortem exitAfterFatal), so a registry blip
    // would kill a live session. Warn once per outage; registration
    // self-heals on the next successful tick.
    let heartbeatOutageNotified = false;
    // Set only by a failed registration, never by a failed role check afterwards: the listener
    // answers "no holder" for a session whose registry entry lapsed, not for a role read that
    // failed, and only the former warrants a `"reregistered"` regain on the next healthy tick.
    // Cleared only once a whole tick — registration and re-assertion — has succeeded, so a
    // recovery tick whose role read errors still owes the regain to the next healthy one.
    let registrationFailed = false;
    let healing = false;
    context.setInterval(() => {
      // Sessions can be created lazily after session_start (a fresh TUI has no
      // session yet), and the ID this closure registered with goes stale. Heal
      // on drift instead of heartbeating a dead identity forever; until the
      // host mints an id there is nothing to register.
      const liveSessionID = context.sessionManager.getSessionId();
      if (liveSessionID === "") return;
      const drifted = liveSessionID !== sessionID;
      if (healing) return;
      healing = true;
      // A drifted id re-establishes the whole session (reclaimHeldRoles included); a steady one
      // re-registers, then checks the listener still resolves this session's role.
      const afterOutage = registrationFailed;
      void (drifted ? establishSession(context) : registerSession())
        .then(
          () => (drifted ? undefined : reassertRole(afterOutage, context)),
          (error: unknown) => {
            registrationFailed = true;
            throw error;
          }
        )
        .then(() => {
          registrationFailed = false;
          heartbeatOutageNotified = false;
        })
        .catch((error) => {
          if (heartbeatOutageNotified) return;
          heartbeatOutageNotified = true;
          context.ui.notify(
            `envoy: registry heartbeat failed (${messageFor(error)}); retrying every heartbeat`,
            "warning"
          );
        })
        .finally(() => {
          healing = false;
        });
    }, defaults.heartbeatMs);
    heartbeatRegistered = true;
  };

  const recoverRegisteredInterests = async (): Promise<void> => {
    // Registered interests deliver only through this session's own NATS
    // subscriptions — the listener skips push delivery for self-subscribed
    // sessions — so a resumed process must rebuild them or stay deaf to
    // topics it registered before it died (e.g. dispatch thread replies).
    // Quiet on failure like rebind: a brand-new session has no registry
    // entry, and a listener outage must not fail session start.
    // Role topics are not subscriptions (role deliveries arrive on the agent
    // subject); they are re-claimed by reclaimHeldRoles once registered.
    const registry = await client.getInterest(sessionID).catch(() => undefined);
    if (registry === undefined) return;
    for (const topic of registry.topics) {
      if (topic === agentSubject(sessionID) || topic.startsWith(ROLE_TOPIC_PREFIX)) continue;
      await subscribe(topic);
    }
  };

  /**
   * Claim `role` for this session. A hard claim (the default; what
   * envoy_role_set does) is last-claim-wins. A soft claim is what automatic
   * recovery uses: the listener grants it only when the role is unheld, its
   * holder is no longer live, or its holder is the id this session continues
   * (`previousSessionID` — a fork's parent is still heartbeating), and answers
   * with the live holder otherwise — atomically, so two resumers cannot both
   * believe they won. Returns whether the claim landed.
   */
  const setEnvoyRole = async (
    role: string,
    options: { soft?: boolean; previousSessionID?: string } = {}
  ): Promise<boolean> => {
    const topic = ROLE_TOPIC_PREFIX + role;
    const previousTopic = claimedRoleTopic;
    const result = await client.setRole({
      sessionID,
      role,
      soft: options.soft,
      previousSessionID: options.previousSessionID,
    });
    if (!result.claimed) {
      logger.warn("envoy: role held by another live session; not reclaimed", {
        role,
        sessionID,
        holder: result.holder,
      });
      claimedRoleTopic = undefined;
      return false;
    }
    claimedRoleTopic = topic;
    // The transcript is the one thing `omp --resume` guarantees, so it is
    // the durable record of the claim: the listener reaps a dead session's
    // interest row (role claim included) after its ten-minute stale-interest
    // grace window, and this process's memory dies with it.
    pi.appendEntry(ROLE_CLAIM_ENTRY, { role });
    if (activeSessionContext !== undefined) ensureHeartbeat(activeSessionContext);
    if (previousTopic !== undefined && previousTopic !== topic) {
      await client.unsubscribe({ sessionID, topics: [previousTopic] });
    }
    return true;
  };

  const transcriptClaimedRole = (branch: readonly unknown[]): string | null | undefined => {
    // The last claim entry wins: a claim followed by a release is no claim.
    let role: string | null | undefined;
    for (const entry of branch) {
      if (!isRoleClaimEntry(entry)) continue;
      role = entry.data.role;
    }
    return role;
  };

  const roleSources = (
    previousSessionID: string,
    carryPreviousSessionRole: boolean
  ): readonly string[] =>
    previousSessionID === "" || previousSessionID === sessionID || !carryPreviousSessionRole
      ? [sessionID]
      : [sessionID, previousSessionID];

  const reclaimHeldRoles = async (
    previousSessionID: string,
    branch: readonly unknown[],
    carryPreviousSessionRole: boolean
  ): Promise<void> => {
    // A role claim outlives the process it was made in, and the process that
    // resumes or rebinds the session must hold it again under its current
    // id — otherwise every publish to the role gets `no holder` until someone
    // re-runs envoy_role_set. A session holds at most one role, so the three
    // records are consulted in precedence order and the first answer wins:
    //  - the transcript entry written at claim time — the durable record
    //    (survives `omp --resume` after the listener has reaped the dead
    //    session's rows); a recorded release is final and vetoes the rest;
    //  - this process's memory (a rebind within one process, where the
    //    listener may be momentarily unreachable);
    //  - the listener's interest rows for the current id (same-id resume
    //    inside the reap window) and the previous id (rebind).
    // A `new`/`resume` switch installs an unrelated transcript, so the
    // outgoing session's memory and rows are not carried into it. setRole is
    // last-claim-wins with old-holder cleanup, so a same-id reclaim is a no-op
    // re-assert and a rebind is a clean move. Must run after registerSession:
    // the listener rejects a claim from an unregistered session. Quiet on
    // failure: session start must not depend on it.
    if (!carryPreviousSessionRole) claimedRoleTopic = undefined;
    const remembered = transcriptClaimedRole(branch);
    if (remembered === null) {
      claimedRoleTopic = undefined;
      return;
    }
    let role = remembered ?? claimedRoleTopic?.slice(ROLE_TOPIC_PREFIX.length);
    if (role === undefined) {
      for (const source of roleSources(previousSessionID, carryPreviousSessionRole)) {
        const registry = await client.getInterest(source).catch(() => undefined);
        const topic = registry?.topics.find((candidate) => candidate.startsWith(ROLE_TOPIC_PREFIX));
        if (topic !== undefined) {
          role = topic.slice(ROLE_TOPIC_PREFIX.length);
          break;
        }
      }
    }
    if (role === undefined) return;
    // An explicit envoy_role_set is last-claim-wins by contract. An automatic
    // reclaim is not: this session's transcript may be stale evidence (the
    // parent of a /fork whose role moved to the child; a second process on
    // the same transcript), and it must not take a role a different live
    // session holds. The listener decides that atomically for a soft claim;
    // the id this session continues is the one live holder it may supersede.
    const continued =
      carryPreviousSessionRole && previousSessionID !== "" && previousSessionID !== sessionID
        ? previousSessionID
        : undefined;
    await setEnvoyRole(role, { soft: true, previousSessionID: continued }).catch((error) => {
      logger.warn("envoy: role reclaim failed", { role, sessionID, error: messageFor(error) });
    });
  };

  let sessionEstablishment = Promise.resolve();

  const establishSessionNow = async (
    context: SessionContext,
    options: EstablishSessionOptions = {},
    previousSessionID = sessionID
  ): Promise<void> => {
    const previousTopic = previousSessionID === "" ? undefined : agentSubject(previousSessionID);
    if (options.carryPreviousSessionRole === false) claimedRoleTopic = undefined;
    restoreLocalSessionState(context);
    const branch = context.sessionManager.getBranch?.() ?? [];
    const resumed = branch.length > 0;
    if (sessionID === "") {
      if (previousTopic !== undefined) closeIntentionally(previousTopic);
      ensureHeartbeat(context);
      return;
    }
    const currentTopic = agentSubject(sessionID);
    if (previousTopic !== undefined && previousTopic !== currentTopic) {
      closeIntentionally(previousTopic);
    }
    await ensureConnection();
    await subscribe(currentTopic);
    if (resumed) {
      await recoverRegisteredInterests();
    }
    // Every session subscribes to its own agent subject, so every session is
    // registered; envoy_send treats an unregistered id as dead.
    await registerSession();
    if (resumed || previousSessionID !== "") {
      await reclaimHeldRoles(previousSessionID, branch, options.carryPreviousSessionRole ?? true);
    }
    ensureHeartbeat(context);
  };

  const establishSession = (
    context: SessionContext,
    options: EstablishSessionOptions = {},
    previousSessionID = sessionID
  ): Promise<void> => {
    const run = sessionEstablishment.then(
      () => establishSessionNow(context, options, previousSessionID),
      () => establishSessionNow(context, options, previousSessionID)
    );
    sessionEstablishment = run.catch(() => undefined);
    return run;
  };

  const bridge = legionRoleClaimBridge();
  const claim: LegionRoleClaim = async (targetSessionID, role, callerContext) => {
    const context = callerContext ?? activeSessionContext;
    if (context === undefined || context.sessionManager.getSessionId() !== targetSessionID) {
      throw new Error(`Envoy has no active session for Legion role claim: ${targetSessionID}`);
    }
    // Legion drives this session from here on, and the run-end ask nudge stays out of it.
    // Recorded before the claim runs: a claim that fails midway leaves the session just as
    // Legion-driven as one that succeeds.
    markLegionManagedSession(targetSessionID);
    if (sessionID !== targetSessionID) await establishSession(context);
    // The listener rejects a claim from a session it does not currently know
    // (its registration may have expired), so register before claiming.
    await registerSession();
    await setEnvoyRole(role);
  };

  const subscribeNotice: LegionNoticeSubscription = async (
    targetSessionID,
    topic,
    callerContext
  ) => {
    const context = callerContext ?? activeSessionContext;
    if (context === undefined || context.sessionManager.getSessionId() !== targetSessionID) {
      throw new Error(
        `Envoy has no active session for Legion notice subscription: ${targetSessionID}`
      );
    }
    if (sessionID !== targetSessionID) await establishSession(context);
    await subscribe(topic);
    await registerSession();
  };

  const claimInstance: LegionRoleClaimInstance = {
    claim,
    subscribe: subscribeNotice,
    // The manager's id, already moved by the time any session event is dispatched; the module
    // `sessionID` follows only once this instance's own rebind has run.
    sessionID: () => activeSessionContext?.sessionManager.getSessionId() ?? sessionID,
  };
  bridge.instances.push(claimInstance);

  // A `task` subagent loads its own instance of this module in the parent's process and fires
  // its own session_start. It shares the parent's Envoy identity: registering it would list an
  // untitled session per subagent, heartbeated for as long as the parent process lives. Two
  // tests, either enough: the transcript layout (file storage), and the host's own roster
  // (any storage, any transcript or none).
  const isSubagentTranscript = subagentSessionCheck();
  const isSubagent = async (context: SessionContext): Promise<boolean> =>
    (await isSubagentTranscript(context)) || isRegisteredSubagent(context);

  pi.on("session_start", async (_event, context) => {
    if (await isSubagent(context)) return;
    const previousSessionID = sessionID;
    restoreLocalSessionState(context);
    if (dispatchConfig.error !== null) {
      context.ui.notify(`envoy: dispatch tool disabled — ${dispatchConfig.error}`, "warning");
    }
    if (defaults.natsUrls.length === 0) {
      context.ui.notify(
        [
          "envoy: ENVOY_NATS_URL is not set; inbound envoy messages are disabled",
          "(outbound tools still work)",
        ].join(" "),
        "warning"
      );
      return;
    }
    let established = false;
    const establish = async (): Promise<void> => {
      await establishSession(context, {}, previousSessionID);
      established = true;
    };
    try {
      await establish();
    } catch (error) {
      context.ui.notify(
        `envoy: NATS unavailable (${messageFor(error)}); retrying in the background`,
        "warning"
      );
      let attempting = false;
      context.setInterval(() => {
        if (established || attempting) return;
        attempting = true;
        void establish()
          .then(() => context.ui.notify("envoy: NATS connection established", "warning"))
          // Quiet on purpose: the initial warning disclosed the outage, and a
          // fresh warning every retry tick would spam the session.
          .catch(() => undefined)
          .finally(() => {
            attempting = false;
          });
      }, NATS_RETRY_INTERVAL_MS);
    }
  });

  const rebind = async (
    reason: SessionSwitchReason | undefined,
    context: SessionContext
  ): Promise<void> => {
    if (await isSubagent(context)) return;
    const previousID = sessionID;
    restoreLocalSessionState(context);
    if (defaults.natsUrls.length === 0) return;
    try {
      const carryPreviousSessionRole = reason !== "new" && reason !== "resume";
      await establishSession(context, { carryPreviousSessionRole }, previousID);
    } catch (error) {
      // A switch during a network outage must degrade, not fail the handler;
      // the next switch or the NATS client's own reconnect re-establishes.
      context.ui.notify(
        `envoy: rebind failed (${messageFor(error)}); will recover on reconnect`,
        "warning"
      );
    }
    // A branch or fork carries the transcript — and a handoff its
    // agent-written summary — forward under a freshly minted session id. The
    // extension tracks the change, but any identity already baked into that
    // carried context — envoy_whoami output, ids the agent named in sent
    // messages — silently rots, and peers who saved the old id can no longer
    // reach this session. Tell the agent without starting a turn; the notice
    // lands when it next runs. A "new" or "resume" switch replaces the
    // transcript with one that matches its own id, so those stay silent.
    if (previousID === "" || previousID === sessionID) return;
    if (reason === "new" || reason === "resume") return;
    pi.sendMessage(
      {
        customType: "envoy-message",
        content: encode({
          envoy: {
            notice: "session id changed",
            previous_session_id: previousID,
            session_id: sessionID,
            detail:
              "This conversation now lives under a new session id. Any identity you shared earlier (envoy_whoami output, session ids named in messages) is stale, and peers using the old id cannot reach you. Re-run envoy_whoami before identifying yourself and re-announce the new id to peers you already introduced yourself to.",
          },
        }),
        display: true,
        details: LOCAL_ENVOY_NOTICE,
      },
      { deliverAs: "steer", triggerTurn: false }
    );
  };

  // Only a switch reports why the session changed; a branch or a tree
  // navigation carries no reason at all.
  pi.on("session_switch", (event, context) => rebind(event.reason, context));
  pi.on("session_branch", (_event, context) => rebind(undefined, context));
  pi.on("session_tree", (_event, context) => rebind(undefined, context));

  pi.on("session_shutdown", async () => {
    shuttingDown = true;
    const bound = bridge.instances.indexOf(claimInstance);
    if (bound !== -1) bridge.instances.splice(bound, 1);
    const deadline = Promise.withResolvers<void>();
    const timer = setTimeout(deadline.resolve, 1_000);
    try {
      const deregistration =
        sessionID === ""
          ? Promise.resolve()
          : client.unregisterSession(sessionID).catch(() => undefined);
      const draining = connection?.drain().catch(() => undefined) ?? Promise.resolve();
      await Promise.race([
        Promise.allSettled([deregistration, draining]).then(() => undefined),
        deadline.promise,
      ]);
    } finally {
      clearTimeout(timer);
      connection = undefined;
      activeSessionContext = undefined;
      subscriptions.clear();
      intentionallyClosed.clear();
      awaitingRetry.clear();
    }
  });

  // Every tool validates its own arguments inside execute (one refusal naming every
  // problem); the host's schema check is bypassed so it cannot pre-empt that with its own wording.
  for (const spec of envoyToolSpecs) {
    pi.registerTool({
      name: spec.name,
      label: spec.name,
      description: spec.description,
      parameters: schemaFor(pi, spec.operation),
      lenientArgValidation: true,
      execute: async (_id, parameters) => execute(spec.operation, parameters),
    });
  }

  if (dispatchConfig.enabled) {
    // Deliberately NOT registered strict: on installed OMP hosts a strict host schema makes
    // the coercion pass delete an unknown key beside valid required fields and validation then
    // "succeeds" with silently narrowed args, while the non-strict schema preserves unknown
    // root fields so they reach `executeDispatchTool`, whose own always-strict parse names the
    // field the caller invented. Verified against the live agent loop on 18.2.2 (legion #1242
    // review); the xd:// write path's half of this contract is can1357/oh-my-pi#12871.
    for (const spec of dispatchToolSpecs) {
      pi.registerTool({
        name: spec.name,
        label: spec.name,
        description: spec.description,
        parameters: dispatchToolSchema(spec, zodSchemaApi(pi.zod)),
        lenientArgValidation: true,
        execute: async (_id, params, signal, _onUpdate, context) => {
          try {
            const result = await executeDispatchTool({
              tool: spec.name,
              args: params,
              cwd: context.cwd,
              host: "omp",
              sessionId: context.sessionManager.getSessionId(),
              sessionTitle: context.sessionManager.getSessionName?.(),
              config: currentDispatchConfig(),
              signal,
              env: process.env,
            });
            return toolSuccess(result.text, result.details);
          } catch (error) {
            return toolFailure(error);
          }
        },
      });
    }
  }

  registerEnvoyWhoamiCommand(pi, () => sessionID);

  // Turn start injects nothing into the conversation; its open-asks query only arms the run-end
  // nudge below, the snapshot's `as_of` becoming the period's baseline. A session the stop can
  // never nudge does not pay for it: the host awaits this handler, so a slow or unreachable
  // Dispatch would add up to `OPEN_ASKS_TIMEOUT_MS` to the head of each of its turns and then
  // warn it about a reminder it never gets — and a host with no `pi.askEphemeral` cannot run the
  // self-check the nudge now turns on, so it is one of those sessions. `id === sessionID` is
  // deliberately not one of the conditions — a fresh TUI mints its id lazily and heals by drift,
  // so it would drop the first turn's arming.
  pi.on("before_agent_start", async (event, context) => {
    const id = context.sessionManager.getSessionId();
    if (
      id === "" ||
      event.prompt.trim() === "" ||
      !context.hasUI ||
      askEphemeral === undefined ||
      legionManaged(id)
    ) {
      return undefined;
    }
    // Memoized per instance: one transcript stat for the life of the session.
    if (await isSubagent(context)) return undefined;
    try {
      const open = await queryOpenAsks(id);
      if (open !== null) armAskAwareness(id, event.prompt, open.snapshot.as_of);
    } catch (error) {
      warnAskAvailability(context, error);
    }
    return undefined;
  });

  // Arms one nudge period. A genuine user turn is the only thing that arms one: this handler
  // runs for an ordinary prompt and for a steering batch carrying the user's own text, and
  // `prompt` is that text. A `triggerTurn` continuation of an agent-attributed custom message —
  // which is what the nudge below is — never reaches this handler at all (measured on OMP
  // 18.2.9: one before_agent_start for the user's prompt, none for the continuation), so the
  // nudge cannot re-arm itself, and one stop can produce at most one of them.
  function armAskAwareness(id: string, prompt: string, asOf: string): void {
    if (prompt.trim() === "") return;
    awarenessGeneration++;
    // The verdict of a check still in flight describes a run the user has already moved past.
    abortSelfCheck("a new user turn superseded the self-check");
    askAwareness = {
      session_id: id,
      period: (askAwareness.session_id === id ? askAwareness.period : 0) + 1,
      baseline_as_of: asOf,
      check_due: true,
      checks: 0,
    };
  }

  // The run-end nudge. `agent_end` is the bare stop signal — no message, no shape to read — so
  // the first trigger is Dispatch state: the agent stopped, this period opened no ask, and none
  // is open. That alone said nothing about whether the agent was waiting, and steering on it
  // made the model announce "nothing outstanding" after nearly every turn, so it is now only the
  // cheap precondition of a hidden self-check (below), and the steer follows the self-check's
  // WAITING verdict alone. The check itself is owed and spent like the host's own todo reminder:
  // the arming turn owes one, the check spends it whatever the verdict, opening the ask spends
  // it too, and the agent's next real work owes another (`tool_result` below), so a run that
  // keeps working keeps being checked without a new user turn. A settled turn that only replies
  // does no work, so the nudge's own continuation never owes a check and cannot nudge itself;
  // `ASK_CHECKS_PER_PERIOD` bounds the rest.
  //
  // A run with no UI (`omp -p`, and any other headless launch) never gets it: the host disposes
  // the session at the end of that one run, and its output is already printed, so the steered
  // continuation either races dispose and is recorded aborted with no provider reply, or wins
  // and bills a whole turn whose text nobody reads. Both were measured on OMP 18.2.9. An RPC
  // host — every Legion pane — carries a UI context and is nudged as a terminal is. So does an
  // ACP host, but a client that defers agent-initiated turns gets the steer queued as hidden
  // next-turn context instead of a turn of its own, consumed when the user next prompts.
  pi.on("agent_end", async (event, context) => {
    const id = context.sessionManager.getSessionId();
    // Only a run that settled normally is nudged: steering an interrupt (`aborted`), a provider
    // failure (`error`), a truncation, or a run with no reply of its own answers the user's cancel,
    // or a failure, with a turn nobody asked for.
    const lastReply = event.messages?.findLast((message) => message.role === "assistant");
    if (
      event.willContinue === true ||
      lastReply?.stopReason !== "stop" ||
      shuttingDown ||
      !context.hasUI ||
      id === "" ||
      legionManaged(id) ||
      // The host's live id, never the module's `sessionID`: a fresh TUI mints its id after
      // `session_start`, so the two disagree until the registration heartbeat heals the drift
      // (up to `ENVOY_HEARTBEAT_MS`, 120 s by default) and a brand-new terminal went unchecked
      // for that whole window. The period already carries the id that armed it, and a session
      // change resets the period to 0 and bumps the generation, so this is the discriminator —
      // `before_agent_start` and `tool_result` read the live id for the same reason. A `task`
      // subagent arms no period at all (it returns there), so it never reaches the check either.
      askAwareness.session_id !== id ||
      askAwareness.period === 0 ||
      !askAwareness.check_due ||
      askAwareness.checks >= ASK_CHECKS_PER_PERIOD ||
      askAwareness.baseline_as_of === null ||
      askCheckInFlight ||
      askEphemeral === undefined
    ) {
      return;
    }
    const period = askAwareness.period;
    const generation = awarenessGeneration;
    askCheckInFlight = true;
    try {
      // Both awaits below are windows in which a new user turn can arm another period, a
      // session change can move the session, or the agent's own next run can open the ask this
      // check is about — which clears the check it owed. The re-checks read one list, so they
      // cannot drift.
      const stale = (): boolean =>
        shuttingDown ||
        generation !== awarenessGeneration ||
        // Only the host's live id: see the guard above on the module's `sessionID`.
        context.sessionManager.getSessionId() !== id ||
        legionManaged(id) ||
        askAwareness.session_id !== id ||
        askAwareness.period !== period ||
        !askAwareness.check_due;
      let open: { readonly snapshot: OpenAsksResponse; readonly url: string } | null;
      try {
        open = await queryOpenAsks(id, askAwareness.baseline_as_of);
      } catch (error) {
        if (generation === awarenessGeneration) warnAskAvailability(context, error);
        return;
      }
      if (open === null || stale()) return;
      if (open.snapshot.count > 0 || open.snapshot.opened_since) {
        // Dispatch knows this session is waiting, or knows it asked since the window opened, so
        // there is nothing for the model to tell anyone: the settle is silent and no check is
        // spent. The window moves to this snapshot, which is what stops one ask from silencing
        // the rest of the period: `opened_since` counts every ask authored after `since`,
        // answered or not, so a window pinned to the arming turn stays true forever.
        askAwareness = {
          ...askAwareness,
          check_due: false,
          baseline_as_of: open.snapshot.as_of,
        };
        return;
      }
      // The self-check: one hidden, tool-free model call over a snapshot of this conversation
      // (`pi.askEphemeral`, the channel a targeted Dispatch BTW already uses), which adds nothing
      // to the transcript and which the user never sees. Only a WAITING verdict — the agent
      // saying it stopped on a human it has not asked — buys the visible turn. PROCEEDING, any
      // other answer, a timeout, and a failure are all silent, so an ordinary settle costs one
      // hidden call and shows nothing.
      //
      // The bound is this handler's own clock, not the host's: the signal is still passed, so a
      // host that honours it stops paying for an answer nobody will read, but the await always
      // settles at `selfCheckTimeoutMs` whatever the host does. A host that ignored the signal
      // would otherwise hold `askCheckInFlight` — and with it every later check — for as long as
      // its call hung. A late answer is dropped; its rejection is already handled, so it can
      // never surface as an unhandled one.
      const abort = new AbortController();
      askCheckAbort = abort;
      let answered: Promise<string | undefined>;
      try {
        answered = askEphemeral({ prompt: ASK_SELF_CHECK_PROMPT, signal: abort.signal }).then(
          (reply): string | undefined => reply.replyText,
          (error: unknown): string | undefined => {
            logSelfCheckFailure(id, error);
            return undefined;
          }
        );
      } catch (error) {
        // A host initialised without the capability installs a stub that throws synchronously
        // rather than rejecting, so `.then(onRejected)` never sees it. Left to escape, the
        // throw would leave the check unspent and every later settle would pay another Dispatch
        // round trip and throw again, with the cap never engaging.
        logSelfCheckFailure(id, error);
        answered = Promise.resolve(undefined);
      }
      const expiry = Promise.withResolvers<undefined>();
      const expire = setTimeout(() => {
        const timedOut = new Error(`self-check timed out after ${selfCheckTimeoutMs} ms`);
        abort.abort(timedOut);
        logSelfCheckFailure(id, timedOut);
        expiry.resolve(undefined);
      }, selfCheckTimeoutMs);
      let verdict: string | undefined;
      try {
        verdict = await Promise.race([answered, expiry.promise]);
      } finally {
        clearTimeout(expire);
        if (askCheckAbort === abort) askCheckAbort = undefined;
      }
      if (stale()) {
        // Whatever invalidated this check aborted it already if it could reach it; a host that
        // is still working on an answer nobody will read stops paying here.
        abort.abort(new Error("the self-check was superseded before its verdict arrived"));
        return;
      }
      // The check ran: it spends the period's outstanding one, counts against the cap, and
      // moves the window on, whatever came back. Only work re-arms it.
      askAwareness = {
        ...askAwareness,
        check_due: false,
        checks: askAwareness.checks + 1,
        baseline_as_of: open.snapshot.as_of,
      };
      if (verdict === undefined || !isWaitingVerdict(verdict.trim())) return;
      // `deliverAs: "nextTurn"` with `triggerTurn` is the host's documented form for a message
      // sent during prompt teardown; on the pin this steer produced exactly one continuation in
      // every interactive and RPC run, so it stays the channel `deliver` already uses.
      pi.sendMessage(
        { customType: ASK_REMINDER_MESSAGE, content: UNASKED_WAIT_REMINDER, display: false },
        { deliverAs: "steer", triggerTurn: true }
      );
    } finally {
      askCheckInFlight = false;
    }
  });

  // A write follows the ask it touched; nothing subscribes the session to the whole
  // issue (that is the agent's own envoy_subscribe). The host does not let a
  // tool_result handler amend the result the model already saw, so the notice goes
  // through the same steer channel `deliver` uses for inbound envelopes.
  const announceFollow = createFollowAnnouncer((text) => {
    pi.sendMessage(
      { customType: "envoy-message", content: text, display: true, details: LOCAL_ENVOY_NOTICE },
      { deliverAs: "steer", triggerTurn: false }
    );
  });
  pi.on("tool_result", async (event, context) => {
    if (event.isError) return;
    // The host's live id, never the module's `sessionID`, for the reason the stop guard reads
    // it: in a fresh TUI's drift window the two disagree, and the period is armed against the
    // live one, so a comparison against the module copy matched nothing for up to a heartbeat —
    // the window this nudge was opened up for.
    if (
      askAwareness.session_id === context.sessionManager.getSessionId() &&
      askAwareness.period > 0
    ) {
      if (ASK_OPENING_TOOLS.includes(event.toolName)) {
        // The agent asked the humans itself, so this stop has nothing left for the nudge to
        // say: it spends the check the period owed rather than ending the period. The ask is
        // still what keeps later settles silent — Dispatch is re-read at every one of them.
        askAwareness = { ...askAwareness, check_due: false };
      } else if (!event.toolName.startsWith(DISPATCH_TOOL_PREFIX)) {
        // Real work: it owes the period another check, the way finishing a step re-arms the
        // host's todo reminder. A Dispatch write is the agent talking to the humans this nudge
        // is about, not work, so it owes nothing — and a turn that only replies calls no tool
        // at all, which is what stops the nudge's own continuation from re-arming itself.
        askAwareness = { ...askAwareness, check_due: true };
      }
    }
    announceFollow(event.details);
  });

  async function execute(
    operation: EnvoyToolOperation,
    rawParameters: Record<string, unknown>
  ): Promise<ToolResult> {
    try {
      // The host hands raw arguments through (lenientArgValidation); this is the one
      // refusal, naming every problem, before anything reaches the listener.
      const parameters: Record<string, unknown> = parseEnvoyToolArguments(operation, rawParameters);
      switch (operation) {
        case EnvoyToolOperation.subscribe: {
          const added: string[] = [];
          const already: string[] = [];
          for (const topic of topicsFor(parameters)) {
            ((await subscribe(topic)) ? added : already).push(topic);
          }
          let warnings: readonly string[] | undefined;
          let registrationError: string | undefined;
          if (added.length > 0) {
            try {
              warnings = (await registerSession()).warnings;
            } catch (error) {
              registrationError = messageFor(error);
            }
          }
          const subscribed = `Subscribed: ${added.join(", ") || "(none new)"}`;
          return toolSuccess(
            warnings === undefined || warnings.length === 0
              ? subscribed
              : `${subscribed}\nWarnings: ${warnings.join("; ")}`,
            {
              added,
              already,
              ...(warnings === undefined || warnings.length === 0 ? {} : { warnings }),
              ...(registrationError === undefined ? {} : { registrationError }),
            }
          );
        }
        case EnvoyToolOperation.unsubscribe: {
          // The session's own inbox is not a subscription the tool manages.
          // Role topics can be ordinary local NATS subscriptions too, but their
          // listener interest represents the independently-held role claim: close
          // the local subscription without unregistering that interest.
          const inbox = agentSubject(sessionID);
          const targets = topicsFor(parameters, [...subscriptions.keys()]).filter(
            (topic) => topic !== inbox
          );
          const removed = targets.filter(closeIntentionally);
          const registryRemoved = removed.filter((topic) => !topic.startsWith(ROLE_TOPIC_PREFIX));
          const registrationError =
            registryRemoved.length === 0
              ? undefined
              : await client
                  .unsubscribe({ sessionID, topics: registryRemoved })
                  .then(registerSession)
                  .then(() => undefined, messageFor);
          return toolSuccess(`Unsubscribed: ${removed.join(", ") || "(none)"}`, {
            removed,
            ...(registrationError === undefined ? {} : { registrationError }),
          });
        }
        case EnvoyToolOperation.listInterests: {
          const registry = await client.getInterest(sessionID);
          const interests = mergeInterestSources(registry.topics, [...subscriptions.keys()]);
          return toolSuccess(
            JSON.stringify({ ...registry, topics: [...interests.keys()] }, null, 2),
            {
              interests: [...interests].map(([topic, source]) => ({ topic, source })),
            }
          );
        }
        case EnvoyToolOperation.inbox:
          return toolSuccess(JSON.stringify(inbox, null, 2), { count: inbox.length });
        case EnvoyToolOperation.send: {
          const targetSessionID = stringFor(parameters, "session_id");
          const result = await client.send({
            sourceSessionID: sessionID,
            targetSessionID,
            message: stringFor(parameters, "message"),
            ...toMessageMetadata(parameters as MessageMetadataArguments),
          });
          return toolSuccess(sendConfirmationText(result), {
            event_id: result.envelope.event_id,
            recipient: result.recipient,
            confirmed: result.confirmed,
          });
        }
        case EnvoyToolOperation.publish: {
          const topic = stringFor(parameters, "topic");
          const result = await client.publish({
            sourceSessionID: sessionID,
            topic,
            message: stringFor(parameters, "message"),
            ...toMessageMetadata(parameters as MessageMetadataArguments),
          });
          return toolSuccess(
            result.holder === undefined
              ? `published ${result.envelope.event_id}`
              : `published ${result.envelope.event_id}; holder ${result.holder}`,
            {
              event_id: result.envelope.event_id,
              topic,
              ...(result.holder === undefined ? {} : { holder: result.holder }),
            }
          );
        }
        case EnvoyToolOperation.setRole: {
          const role = stringFor(parameters, "role");
          await setEnvoyRole(role);
          return toolSuccess(`Now holding role: ${role}`, { role });
        }
        case EnvoyToolOperation.getRole: {
          const role = await client.getRole(stringFor(parameters, "role"));
          return toolSuccess(JSON.stringify(role, null, 2), role);
        }
        case EnvoyToolOperation.whoami: {
          return toolSuccess(
            JSON.stringify(
              { session_id: sessionID, machine_id: machineID(), dir: sessionDirectory },
              null,
              2
            ),
            {
              sessionID,
              topics: [...subscriptions.keys()],
            }
          );
        }
        case EnvoyToolOperation.listSessions: {
          const machine = optionalStringFor(parameters, "machine");
          const directory = optionalStringFor(parameters, "dir");
          const title = optionalStringFor(parameters, "title");
          const sessions = await client.listSessions({ directory, title });
          const result =
            machine === undefined
              ? sessions
              : sessions.filter((session) => session.machine_id === machine);
          return toolSuccess(JSON.stringify(result, null, 2), {
            count: result.length,
            machine,
            dir: directory,
            title,
          });
        }
      }
    } catch (error) {
      return toolFailure(error);
    }
  }
}

function schemaFor(pi: PiApi, operation: EnvoyToolOperation): unknown {
  const spec = envoyToolSpecs.find((candidate) => candidate.operation === operation);
  if (spec === undefined) throw new Error(`missing Envoy tool specification for ${operation}`);
  return pi.zod.object(spec.arguments(zodSchemaApi(pi.zod)));
}

function stringFor(parameters: Record<string, unknown>, key: string): string {
  const value = parameters[key];
  if (typeof value !== "string") throw new TypeError(`${key} must be a string`);
  return value;
}

function optionalStringFor(parameters: Record<string, unknown>, key: string): string | undefined {
  const value = parameters[key];
  if (value === undefined) return undefined;
  if (typeof value !== "string") throw new TypeError(`${key} must be a string`);
  return value;
}

function isStringArray(value: unknown): value is readonly string[] {
  return Array.isArray(value) && value.every((entry: unknown) => typeof entry === "string");
}

function topicsFor(
  parameters: Record<string, unknown>,
  fallback: readonly string[] = []
): readonly string[] {
  const value = parameters.topics;
  if (value === undefined) return fallback;
  if (!isStringArray(value)) throw new TypeError("topics must be strings");
  return expandSubscriptionTopics(value);
}
