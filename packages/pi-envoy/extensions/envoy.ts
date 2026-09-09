import { existsSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import {
  agentSubject,
  dispatchToolSpecs,
  ROLE_TOPIC_PREFIX,
  zodSchemaApi,
} from "@legion/contracts";
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults";
import { inboundTimestamp, renderInbound, senderLabel } from "@legion/envoy-client/delivery";
import { resolveDispatchConfig } from "@legion/envoy-client/dispatch-config";
import { executeDispatchTool } from "@legion/envoy-client/dispatch-execute";
import { dispatchSubscriptionTopic } from "@legion/envoy-client/dispatch-subscribe";
import { messageFor } from "@legion/envoy-client/errors";
import { machineID } from "@legion/envoy-client/machine";
import {
  EnvoyToolOperation,
  envoyToolSpecs,
  type MessageMetadataArguments,
  toMessageMetadata,
} from "@legion/envoy-client/tool-contract";
import { createEnvoyClient, expandSubscriptionTopics } from "@legion/envoy-client/transport";
import { logger } from "@oh-my-pi/pi-utils";
import { encode } from "@toon-format/toon";
import { connect, type NatsConnection, StringCodec, type Subscription } from "nats";
import type { PiApi, SessionContext, SessionSwitchReason, ToolResult } from "../src/pi-types";
import { toolFailure, toolSuccess } from "../src/tool-result";
import { registerEnvoyMessageRenderer } from "./envoy-message-renderer";
import { registerEnvoyWhoamiCommand } from "./envoy-whoami-command";

const codec = StringCodec();
const NATS_RETRY_INTERVAL_MS = 15_000;

type LegionRoleClaim = (sessionID: string, role: string, context?: SessionContext) => Promise<void>;

type LegionRoleClaimReady = {
  readonly promise: Promise<LegionRoleClaim>;
  readonly resolve: (claim: LegionRoleClaim | PromiseLike<LegionRoleClaim>) => void;
};

type LegionRoleClaimBridge = {
  claim: LegionRoleClaim | undefined;
  readonly ready: LegionRoleClaimReady;
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

  const createdBridge = {
    claim: undefined,
    ready: Promise.withResolvers<LegionRoleClaim>(),
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

  const deliver = async (subject: string, raw: string, reply: string): Promise<void> => {
    const rendered = renderInbound(raw, sessionID, subject);
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
        pi.sendMessage(
          { customType: "envoy-message", content: rendered.content, display: true },
          { deliverAs: "steer", triggerTurn: true }
        );
      } catch (error) {
        console.warn(`[envoy] failed to inject envelope ${envelope?.event_id ?? "unknown"}`, error);
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
      driving: false,
      selfSubscribed: true,
    });

  const ensureHeartbeat = (context: SessionContext): void => {
    if (heartbeatRegistered) return;
    // Never let a heartbeat tick reject unhandled: OMP treats unhandled
    // rejections as fatal (postmortem exitAfterFatal), so a registry blip
    // would kill a live session. Warn once per outage; registration
    // self-heals on the next successful tick.
    let heartbeatOutageNotified = false;
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
      void (drifted ? establishSession(context) : registerSession())
        .then(() => {
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
    const registry = await client.getInterest(sessionID).catch(() => undefined);
    if (registry === undefined) return;
    for (const topic of registry.topics) {
      if (topic === agentSubject(sessionID) || topic.startsWith(ROLE_TOPIC_PREFIX)) continue;
      await subscribe(topic);
    }
  };

  const establishSession = async (context: SessionContext): Promise<void> => {
    const previousTopic = sessionID === "" ? undefined : agentSubject(sessionID);
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
    if ((context.sessionManager.getBranch?.() ?? []).length > 0) {
      await recoverRegisteredInterests();
    }
    // Every session subscribes to its own agent subject, so every session is
    // registered; envoy_send treats an unregistered id as dead.
    await registerSession();
    ensureHeartbeat(context);
  };

  const setEnvoyRole = async (role: string): Promise<void> => {
    const topic = ROLE_TOPIC_PREFIX + role;
    const previousTopic = claimedRoleTopic;
    await client.setRole({ sessionID, role });
    claimedRoleTopic = topic;
    if (activeSessionContext !== undefined) ensureHeartbeat(activeSessionContext);
    if (previousTopic !== undefined && previousTopic !== topic) {
      await client.unsubscribe({ sessionID, topics: [previousTopic] });
    }
  };

  const bridge = legionRoleClaimBridge();
  const claim: LegionRoleClaim = async (targetSessionID, role, callerContext) => {
    const context = callerContext ?? activeSessionContext;
    if (context === undefined || context.sessionManager.getSessionId() !== targetSessionID) {
      throw new Error(`Envoy has no active session for Legion role claim: ${targetSessionID}`);
    }
    if (sessionID !== targetSessionID) await establishSession(context);
    await setEnvoyRole(role);
    await registerSession();
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
      await establishSession(context);
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
        parameters: pi.zod.object(spec.arguments(zodSchemaApi(pi.zod))),
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
      if (await subscribe(topic)) await registerSession();
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
          if (claimedRoleTopic !== undefined && removed.includes(claimedRoleTopic)) {
            claimedRoleTopic = undefined;
          }
          const registrationError =
            removed.length === 0
              ? undefined
              : await client
                  .unsubscribe({ sessionID, topics: removed })
                  .then(registerSession)
                  .then(() => undefined, messageFor);
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
