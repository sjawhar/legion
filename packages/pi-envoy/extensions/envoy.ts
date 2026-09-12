import { existsSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  agentSubject,
  dispatchToolSchema,
  dispatchToolSpecs,
  ROLE_TOPIC_PREFIX,
  zodSchemaApi,
} from "@legion/contracts";
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults";
import {
  inboundTimestamp,
  renderInbound,
  senderLabel,
  type DispatchDelivery,
} from "@legion/envoy-client/delivery";
import { resolveDispatchConfig } from "@legion/envoy-client/dispatch-config";
import { executeDispatchTool } from "@legion/envoy-client/dispatch-execute";
import {
  dispatchSubscriptionTopic,
  dispatchTopicLabel,
  subscriptionRemovedTopics,
} from "@legion/envoy-client/dispatch-subscribe";
import { messageFor } from "@legion/envoy-client/errors";
import { machineID } from "@legion/envoy-client/machine";
import {
  EnvoyToolOperation,
  envoyToolSpecs,
  type MessageMetadataArguments,
  toMessageMetadata,
} from "@legion/envoy-client/tool-contract";
import {
  createEnvoyClient,
  EnvoyApiError,
  expandSubscriptionTopics,
} from "@legion/envoy-client/transport";
import { logger } from "@oh-my-pi/pi-utils";
import { encode } from "@toon-format/toon";
import { connect, type NatsConnection, StringCodec, type Subscription } from "nats";
import type { PiApi, SessionContext, SessionSwitchReason, ToolResult } from "../src/pi-types";
import { toolFailure, toolSuccess } from "../src/tool-result";
import { registerEnvoyMessageRenderer } from "./envoy-message-renderer";
import { registerEnvoyWhoamiCommand } from "./envoy-whoami-command";

const codec = StringCodec();
const NATS_RETRY_INTERVAL_MS = 15_000;

/**
 * Transcript entry recording the role this session holds. Written on every
 * claim (`{ role }`) and release (`{ role: null }`); the last one on the branch
 * is the truth a resumed process uses when deciding what to reclaim.
 */
const ROLE_CLAIM_ENTRY = "envoy-role-claim";

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

type LegionRoleClaim = (sessionID: string, role: string, context?: SessionContext) => Promise<void>;

/**
 * Why the heartbeat decided the listener had lost sight of this session's role: `"reclaimed"` —
 * the listener no longer named this session and a soft claim landed; `"reregistered"` — the
 * claim itself survived, but this session had been unreachable (a registry outage), so the
 * listener may have answered "no holder" for it meanwhile.
 */
export type RoleRegainReason = "reclaimed" | "reregistered";

type LegionRoleRegained = (role: string, reason: RoleRegainReason) => Promise<void>;

type LegionRoleClaimReady = {
  readonly promise: Promise<LegionRoleClaim>;
  readonly resolve: (claim: LegionRoleClaim | PromiseLike<LegionRoleClaim>) => void;
};

type LegionRoleClaimBridge = {
  claim: LegionRoleClaim | undefined;
  readonly ready: LegionRoleClaimReady;
  /**
   * legion.ts's regain hook. One slot, like `claim`, but several legion.ts instances share a
   * process: OMP re-binds every extension factory for each in-process `task` subagent. Only an
   * instance that has established a Legion identity registers here, so the slot always holds
   * the identity-bearing instance's listener.
   */
  regained: LegionRoleRegained | undefined;
};

interface GlobalLegionRoleClaimBridgeStore {
  [key: symbol]: LegionRoleClaimBridge | undefined;
}

// A process-wide symbol bridges legion.ts's `claimEnvoyRole` import to the
// one envoyExtension(pi) instance OMP actually ran, since each manifest entry
// loads as its own module instance with its own module-scope state.
const LEGION_ROLE_CLAIM_BRIDGE = Symbol.for("legion.pi-envoy.role-claim-bridge");

function legionRoleClaimBridge(): LegionRoleClaimBridge {
  const store = globalThis as typeof globalThis & GlobalLegionRoleClaimBridgeStore;
  const bridge = store[LEGION_ROLE_CLAIM_BRIDGE];
  if (bridge) return bridge;

  const createdBridge: LegionRoleClaimBridge = {
    claim: undefined,
    ready: Promise.withResolvers<LegionRoleClaim>(),
    regained: undefined,
  };
  store[LEGION_ROLE_CLAIM_BRIDGE] = createdBridge;
  return createdBridge;
}

export async function claimEnvoyRole(
  sessionID: string,
  role: string,
  context?: SessionContext
): Promise<void> {
  const bridge = legionRoleClaimBridge();
  await (bridge.claim ?? (await bridge.ready.promise))(sessionID, role, context);
}

/**
 * Registers the hook the heartbeat fires after it re-establishes this session as `role`'s live
 * holder (see `reassertRole`). legion.ts re-runs the role's daemon ready call from it. Last
 * registration wins, exactly like `claimEnvoyRole`'s bridge slot — so legion.ts calls this only
 * from the paths that establish a Legion identity, never at extension setup, or a `task`
 * subagent's identity-less instance would replace the holder's listener.
 */
export function onEnvoyRoleRegained(
  listener: (role: string, reason: RoleRegainReason) => Promise<void>
): void {
  legionRoleClaimBridge().regained = listener;
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

export default function envoyExtension(pi: PiApi): void {
  logger.debug("extension instance loaded", { extension: import.meta.url });
  const defaults = envoyDefaultsFromEnvironment(process.env);
  // One loader for the shared envoy.json contract: the dispatch tool is
  // registered only where it names a service, and an invalid file is reported
  // at session start, not silently treated as off.
  const dispatchConfig = resolveDispatchConfig(process.env, { cwd: process.cwd() });
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
    if (!dispatchConfig.enabled || dispatchConfig.url === null || dispatchConfig.token === null) {
      throw new Error("Dispatch reply endpoint is not configured");
    }
    const response = await fetch(`${dispatchConfig.url}/api/v1/messages/${delivery.messageID}/reply`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${dispatchConfig.token}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        actor: { kind: "session", id: sessionID },
        attempt: delivery.attempt,
        ...result,
      }),
    });
    if (!response.ok) {
      throw new Error(`Dispatch reply failed: ${response.status} ${await response.text()}`);
    }
  };

  const deliver = async (subject: string, raw: string, reply: string): Promise<void> => {
    const rendered = renderInbound(raw, sessionID, subject);
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
            `[envoy] rejecting malformed Dispatch targeted delivery ${rendered.rejectedDelivery.messageID}`
          );
          await postDispatchReply(rendered.rejectedDelivery, {
            error: "Invalid Dispatch targeted delivery frame",
          });
        } else if (rendered.malformedDelivery === true) {
          console.warn("[envoy] dropping malformed Dispatch targeted delivery without a reply address");
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
        console.warn(`[envoy] failed to deliver envelope ${envelope?.event_id ?? "unknown"}`, error);
        throw error;
      }
      if (dedupeKey !== undefined) {
        dedupeKeys.add(dedupeKey);
        if (dedupeKeys.size > 1000) {
          const oldest = dedupeKeys.values().next();
          if (!oldest.done) dedupeKeys.delete(oldest.value);
        }
      }
    }
    if (reply !== "" && subject === agentSubject(sessionID)) {
      (await ensureConnection()).publish(reply);
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
      capabilities: typeof pi.askEphemeral === "function" ? ["aside", "btw"] : ["aside"],
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
    // Both captured at entry: a session rebind or an explicit envoy_role_set/envoy_unsubscribe
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

  const releaseClaimedRole = (): void => {
    claimedRoleTopic = undefined;
    pi.appendEntry(ROLE_CLAIM_ENTRY, { role: null });
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
    options: EstablishSessionOptions = {}
  ): Promise<void> => {
    const previousSessionID = sessionID;
    const previousTopic = sessionID === "" ? undefined : agentSubject(sessionID);
    if (options.carryPreviousSessionRole === false) claimedRoleTopic = undefined;
    sessionDirectory = context.cwd;
    sessionID = context.sessionManager.getSessionId();
    activeSessionContext = context;
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
    // A resumed session (non-empty branch — the host's documented resume
    // signal) may hold registered interests from its previous life.
    const branch = context.sessionManager.getBranch?.() ?? [];
    const resumed = branch.length > 0;
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
    options: EstablishSessionOptions = {}
  ): Promise<void> => {
    const run = sessionEstablishment.then(
      () => establishSessionNow(context, options),
      () => establishSessionNow(context, options)
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
    if (sessionID !== targetSessionID) await establishSession(context);
    // The listener rejects a claim from a session it does not currently know
    // (its registration may have expired), so register before claiming.
    await registerSession();
    await setEnvoyRole(role);
  };
  bridge.claim = claim;
  bridge.ready.resolve(claim);

  pi.on("session_start", async (_event, context) => {
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
      await establishSession(context);
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
    if (defaults.natsUrls.length === 0) return;
    const previousID = sessionID;
    try {
      const carryPreviousSessionRole = reason !== "new" && reason !== "resume";
      await establishSession(context, { carryPreviousSessionRole });
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

  for (const spec of envoyToolSpecs) {
    pi.registerTool({
      name: spec.name,
      label: spec.name,
      description: spec.description,
      parameters: schemaFor(pi, spec.operation),
      execute: async (_id, parameters) => execute(spec.operation, parameters),
    });
  }

  if (dispatchConfig.enabled) {
    for (const spec of dispatchToolSpecs) {
      pi.registerTool({
        name: spec.name,
        label: spec.name,
        description: spec.description,
        parameters: dispatchToolSchema(spec, zodSchemaApi(pi.zod)),
        execute: async (_id, params, _signal, _onUpdate, context) => {
          try {
            const result = await executeDispatchTool({
              tool: spec.name,
              args: params,
              cwd: context.cwd,
              host: "omp",
              sessionId: context.sessionManager.getSessionId(),
              sessionTitle: context.sessionManager.getSessionName?.(),
              config: dispatchConfig,
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

  pi.on("tool_result", async (event) => {
    if (event.isError) return;
    const topic = dispatchSubscriptionTopic(event.details);
    if (topic === null) return;
    try {
      const isNew = await subscribe(topic);
      if (isNew) await registerSession();
      // Silent subscription is the one thing Sami ruled out: a write must tell
      // the agent it now gets every event on this issue. Already-subscribed is
      // not news, so it stays silent rather than repeating itself every write.
      // The host does not let a tool_result handler amend the result the model
      // already saw, so this goes through the same steer channel `deliver`
      // uses for inbound envelopes instead of a UI-only notification, which
      // the model never sees.
      if (isNew) {
        pi.sendMessage(
          {
            customType: "envoy-message",
            content: `Subscribed to ${dispatchTopicLabel(topic)} (every event on this issue reaches you; envoy_unsubscribe ${topic} to stop).`,
            display: true,
          },
          { deliverAs: "steer", triggerTurn: false }
        );
      }
    } catch (error) {
      activeSessionContext?.ui.notify(
        `envoy: dispatch reply auto-subscribe failed (${messageFor(error)}); run envoy_subscribe ${topic}`,
        "warning"
      );
    }
  });

  async function execute(
    operation: EnvoyToolOperation,
    parameters: Record<string, unknown>
  ): Promise<ToolResult> {
    try {
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
          // The session's own inbox is not a subscription the tool manages:
          // "remove all" and an explicit request alike leave it in place, or
          // the session stays registered but deaf to direct messages.
          const inbox = agentSubject(sessionID);
          const targets = topicsFor(parameters, [
            ...subscriptions.keys(),
            ...(claimedRoleTopic === undefined ? [] : [claimedRoleTopic]),
          ]).filter((topic) => topic !== inbox);
          const removed = targets.filter(
            (topic) => closeIntentionally(topic) || topic === claimedRoleTopic
          );
          const releasingRole =
            claimedRoleTopic !== undefined && removed.includes(claimedRoleTopic);
          const registrationError =
            removed.length === 0
              ? undefined
              : await client
                  .unsubscribe({ sessionID, topics: removed })
                  .then(registerSession)
                  .then(() => undefined, messageFor);
          // The transcript release is final for automatic reclaim, so it is
          // recorded only once the listener has actually let the role go; a
          // failed release leaves the claim intact on both sides.
          if (releasingRole && registrationError === undefined) releaseClaimedRole();
          return toolSuccess(`Unsubscribed: ${removed.join(", ") || "(none)"}`, {
            removed,
            ...(registrationError === undefined ? {} : { registrationError }),
          });
        }
        case EnvoyToolOperation.listInterests: {
          const registry = await client.getInterest(sessionID);
          const interests = new Map<string, "registry" | "live" | "both">();
          for (const topic of registry.topics) {
            interests.set(topic, subscriptions.has(topic) ? "both" : "registry");
          }
          for (const topic of subscriptions.keys()) {
            if (!interests.has(topic)) interests.set(topic, "live");
          }
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
          const confirmation = result.confirmed ? "" : " (recipient unconfirmed by listener)";
          return toolSuccess(
            `sent ${result.envelope.event_id} to ${result.recipient}${confirmation}`,
            {
              event_id: result.envelope.event_id,
              recipient: result.recipient,
              confirmed: result.confirmed,
            }
          );
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
