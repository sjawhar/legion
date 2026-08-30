import * as os from "node:os";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { agentSubject, EnvelopeSchema, ROLE_TOPIC_PREFIX } from "@legion/contracts";
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults";
import { EnvoyToolOperation, envoyToolSpecs } from "@legion/envoy-client/tool-contract";
import { createEnvoyClient } from "@legion/envoy-client/transport";
import { encode } from "@toon-format/toon";
import { connect, type NatsConnection, StringCodec, type Subscription } from "nats";
import { registerEnvoyWhoamiCommand } from "./envoy-whoami-command";

type ToolParameters = Readonly<Record<string, string | readonly string[] | undefined>>;

type ToolResult = {
  readonly content: readonly { readonly type: "text"; readonly text: string }[];
  readonly details: Readonly<Record<string, unknown>>;
  readonly isError?: boolean;
};


export interface EnvoySessionContext {
  readonly cwd: string;
  readonly sessionManager: {
    readonly getSessionId: () => string;
    /** Live display title; assigned by omp after the first turn, so often undefined at session_start. */
    readonly getSessionName?: () => string | undefined;
  };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  readonly ui: { readonly notify: (message: string, level: "warning") => void };
}

type SessionContext = EnvoySessionContext;

type PiApi = {
  readonly zod: {
    readonly object: (shape: Readonly<Record<string, unknown>>) => unknown;
    readonly string: () => { readonly optional: () => unknown };
    readonly array: (item: unknown) => { readonly optional: () => unknown };
  };
  readonly registerTool: (tool: {
    readonly name: string;
    readonly label: string;
    readonly description: string;
    readonly parameters: unknown;
    readonly execute: (id: string, parameters: ToolParameters) => Promise<ToolResult>;
  }) => void;
  readonly registerCommand: (
    name: string,
    command: {
      readonly description: string;
      readonly handler: (
        args: string,
        context: {
          readonly ui: { readonly notify: (message: string, level: "info" | "warning") => void };
        }
      ) => Promise<void>;
    }
  ) => void;
  readonly on: (
    event: "resources_discover" | "session_start" | "session_switch" | "session_branch" | "session_tree" | "session_shutdown",
    handler: (event: unknown, context: SessionContext) => Promise<unknown>,
  ) => void;
  readonly sendMessage: (
    message: { readonly customType: string; readonly content: string; readonly display: boolean },
    options: { readonly deliverAs: "steer"; readonly triggerTurn: boolean },
  ) => void;
};

const codec = StringCodec();
const NATS_RETRY_INTERVAL_MS = 15_000;

type LegionRoleClaim = (
  sessionID: string,
  role: string,
  context?: EnvoySessionContext
) => Promise<void>;

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
  context?: EnvoySessionContext
): Promise<void> {
  const bridge = legionRoleClaimBridge();
  await (bridge.claim ?? (await bridge.ready.promise))(sessionID, role, context);
}

export async function deleteEnvoyInterest(baseUrl: string, sessionID: string): Promise<void> {
  const response = await fetch(`${baseUrl}/v1/interests/${encodeURIComponent(sessionID)}`, {
    method: "DELETE",
  });
  const responseBody = await response.text();
  if (!response.ok) {
    throw new Error(
      `DELETE /v1/interests/${sessionID} failed with ${response.status}: ${responseBody}`
    );
  }
}
const SKILLS_DIRECTORY = resolve(dirname(fileURLToPath(import.meta.url)), "../../../skills");

export default function envoyExtension(pi: PiApi): void {
  const defaults = envoyDefaultsFromEnvironment(process.env);
  const client = createEnvoyClient({ baseUrl: defaults.envoyUrl, fetch });
  const subscriptions = new Map<string, Subscription>();
  const dedupeKeys = new Set<string>();
  const machineID = os.hostname();
  let connection: NatsConnection | undefined;
  let sessionDirectory = "";
  let sessionID = "";
  let heartbeatRegistered = false;
  let claimedRoleTopic: string | undefined;
  let activeSessionContext: SessionContext | undefined;

  pi.on("resources_discover", async () => ({ skillPaths: [SKILLS_DIRECTORY] }));

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
    let payloadText = raw;
    let displayTopic = subject;
    let duplicate = false;
    try {
      const envelope = EnvelopeSchema.parse(JSON.parse(raw));
      duplicate = dedupeKeys.has(envelope.dedupe_key);
      if (!duplicate) {
        dedupeKeys.add(envelope.dedupe_key);
        if (dedupeKeys.size > 1000) {
          const oldest = dedupeKeys.values().next();
          if (!oldest.done) dedupeKeys.delete(oldest.value);
        }
        displayTopic = envelope.topic;
        if (envelope.payload === undefined) {
          payloadText = encode({ summary: envelope.payload_summary });
        } else {
          try {
            payloadText = encode(JSON.parse(envelope.payload));
          } catch {
            payloadText = envelope.payload;
          }
        }
      }
    } catch {
      payloadText = raw;
    }
    // Steering: mid-turn the message is injected at the next tool boundary
    // instead of waiting for the turn to finish; idle it still starts a turn
    // (triggerTurn), so wake-on-message behavior is unchanged.
    if (!duplicate) {
      pi.sendMessage(
        { customType: "envoy-message", content: `[ENVOY ${displayTopic}]\n${payloadText}`, display: true },
        { deliverAs: "steer", triggerTurn: true },
      );
    }
    if (reply !== "" && subject === agentSubject(sessionID)) (await ensureConnection()).publish(reply);
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
          // A single failed injection (e.g. sendMessage during compaction)
          // must not tear down the subscription; drop the message and keep
          // pumping.
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
    if (subscriptions.has(topic)) return false;
    const subscription = (await ensureConnection()).subscribe(topic);
    // A fresh subscription supersedes any earlier deliberate close of this topic.
    intentionallyClosed.delete(topic);
    subscriptions.set(topic, subscription);
    void pump(topic, subscription);
    return true;
  };

  const registerSession = async (): Promise<void> => {
    await client.subscribe({
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
  };

  const registrationRequired = (): boolean =>
    process.env.ENVOY_REGISTER_SESSION === "1" || claimedRoleTopic !== undefined;

  const ensureHeartbeat = (context: SessionContext): void => {
    if (heartbeatRegistered) return;
    // Never let a heartbeat tick reject unhandled: OMP treats unhandled
    // rejections as fatal (postmortem exitAfterFatal), so a registry blip
    // would kill a live session. Warn once per outage; registration
    // self-heals on the next successful tick.
    let heartbeatOutageNotified = false;
    let healing = false;
    context.setInterval(() => {
      if (!registrationRequired()) return;
      // Sessions can be created lazily after session_start (a fresh TUI has no
      // session yet), and the ID this closure registered with goes stale. Heal
      // on drift instead of heartbeating a dead identity forever.
      const liveSessionID = context.sessionManager.getSessionId();
      const drifted = liveSessionID !== "" && liveSessionID !== sessionID;
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
            "warning",
          );
        })
        .finally(() => {
          healing = false;
        });
    }, defaults.heartbeatMs);
    heartbeatRegistered = true;
  };

  const establishSession = async (context: SessionContext): Promise<void> => {
    const previousTopic = sessionID === "" ? undefined : agentSubject(sessionID);
    sessionDirectory = context.cwd;
    sessionID = context.sessionManager.getSessionId();
    activeSessionContext = context;
    const currentTopic = agentSubject(sessionID);
    if (previousTopic !== undefined && previousTopic !== currentTopic) {
      closeIntentionally(previousTopic);
    }
    await ensureConnection();
    await subscribe(currentTopic);
    if (registrationRequired()) {
      await registerSession();
      ensureHeartbeat(context);
    }
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
    if (defaults.natsUrls.length === 0) {
      context.ui.notify(
        "envoy: ENVOY_NATS_URL is not set; inbound envoy messages are disabled (outbound tools still work)",
        "warning",
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
        "warning",
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

  const rebind = async (_event: unknown, context: SessionContext): Promise<void> => {
    if (defaults.natsUrls.length === 0) return;
    try {
      await establishSession(context);
    } catch (error) {
      // A switch during a network outage must degrade, not fail the handler;
      // the next switch or the NATS client's own reconnect re-establishes.
      context.ui.notify(`envoy: rebind failed (${messageFor(error)}); will recover on reconnect`, "warning");
    }
  };

  pi.on("session_switch", rebind);
  pi.on("session_branch", rebind);
  pi.on("session_tree", rebind);

  pi.on("session_shutdown", async () => {
    shuttingDown = true;
    try {
      // Bound the drain: on a dead connection it can hang past OMP's 2s
      // shutdown-handler budget and the flush is best-effort anyway.
      const drain = connection?.drain();
      if (drain) await Promise.race([drain, new Promise((resolve) => setTimeout(resolve, 1_000))]);
    } catch {
      // A failed drain on shutdown is not actionable.
    } finally {
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

  registerEnvoyWhoamiCommand(pi, () => sessionID);

  async function execute(operation: EnvoyToolOperation, parameters: ToolParameters): Promise<ToolResult> {
    try {
      switch (operation) {
        case EnvoyToolOperation.subscribe: {
          const added: string[] = [];
          const already: string[] = [];
          for (const topic of topicsFor(parameters)) (await subscribe(topic) ? added : already).push(topic);
          const registrationError =
            added.length === 0 ? undefined : await registerSession().then(() => undefined, messageFor);
          return success(`Subscribed: ${added.join(", ") || "(none new)"}`, {
            added,
            already,
            ...(registrationError === undefined ? {} : { registrationError }),
          });
        }
        case EnvoyToolOperation.unsubscribe: {
          const targets = topicsFor(parameters, [
            ...subscriptions.keys(),
            ...(claimedRoleTopic === undefined ? [] : [claimedRoleTopic]),
          ]);
          const removed = targets.filter((topic) => closeIntentionally(topic) || topic === claimedRoleTopic);
          if (claimedRoleTopic !== undefined && removed.includes(claimedRoleTopic)) claimedRoleTopic = undefined;
          const registrationError =
            removed.length === 0
              ? undefined
              : await client
                  .unsubscribe({ sessionID, topics: removed })
                  .then(registerSession)
                  .then(() => undefined, messageFor);
          return success(`Unsubscribed: ${removed.join(", ") || "(none)"}`, {
            removed,
            ...(registrationError === undefined ? {} : { registrationError }),
          });
        }
        case EnvoyToolOperation.listInterests: {
          const registry = await client.getInterest(sessionID);
          const interests = new Map<string, "registry" | "live" | "both">();
          for (const topic of registry.topics) interests.set(topic, subscriptions.has(topic) ? "both" : "registry");
          for (const topic of subscriptions.keys()) {
            if (!interests.has(topic)) interests.set(topic, "live");
          }
          return success(JSON.stringify({ ...registry, topics: [...interests.keys()] }, null, 2), {
            interests: [...interests].map(([topic, source]) => ({ topic, source })),
          });
        }
        case EnvoyToolOperation.send: {
          const targetSessionID = stringFor(parameters, "session_id");
          await client.send({ sourceSessionID: sessionID, targetSessionID, message: stringFor(parameters, "message") });
          return success(`Sent to ${targetSessionID}`, { target: targetSessionID });
        }
        case EnvoyToolOperation.publish: {
          const topic = stringFor(parameters, "topic");
          await client.publish({ sourceSessionID: sessionID, topic, message: stringFor(parameters, "message") });
          return success(`Published to ${topic}`, { topic });
        }
        case EnvoyToolOperation.setRole: {
          const role = stringFor(parameters, "role");
          await setEnvoyRole(role);
          return success(`Now holding role: ${role}`, { role });
        }
        case EnvoyToolOperation.whoami: {
          return success(JSON.stringify({ session_id: sessionID, machine_id: machineID, dir: sessionDirectory }, null, 2), {
            sessionID,
            topics: [...subscriptions.keys()],
          });
        }
        case EnvoyToolOperation.listSessions: {
          const machine = parameters.machine;
          const sessions = await client.listSessions();
          const result = typeof machine === "string" ? sessions.filter((session) => session.machine_id === machine) : sessions;
          return success(JSON.stringify(result, null, 2), { count: result.length, machine });
        }
      }
    } catch (error) {
      return failure(messageFor(error));
    }
  }
}

function schemaFor(pi: PiApi, operation: EnvoyToolOperation): unknown {
  const z = pi.zod;
  switch (operation) {
    case EnvoyToolOperation.subscribe:
      return z.object({ topics: z.array(z.string()) });
    case EnvoyToolOperation.unsubscribe:
      return z.object({ topics: z.array(z.string()).optional() });
    case EnvoyToolOperation.send:
      return z.object({ session_id: z.string(), message: z.string() });
    case EnvoyToolOperation.publish:
      return z.object({ topic: z.string(), message: z.string() });
    case EnvoyToolOperation.setRole:
      return z.object({ role: z.string() });
    case EnvoyToolOperation.listSessions:
      return z.object({ machine: z.string().optional() });
    case EnvoyToolOperation.listInterests:
    case EnvoyToolOperation.whoami:
      return z.object({});
  }
}

function stringFor(parameters: ToolParameters, key: string): string {
  const value = parameters[key];
  if (typeof value !== "string") throw new TypeError(`${key} must be a string`);
  return value;
}

function topicsFor(parameters: ToolParameters, fallback: readonly string[] = []): readonly string[] {
  const value = parameters.topics;
  if (value === undefined) return fallback;
  if (!Array.isArray(value) || value.some((topic) => typeof topic !== "string")) throw new TypeError("topics must be strings");
  return value;
}

function success(text: string, details: Readonly<Record<string, unknown>> = {}): ToolResult {
  return { content: [{ type: "text", text }], details };
}

function failure(text: string): ToolResult {
  return { content: [{ type: "text", text }], details: {}, isError: true };
}

function messageFor(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
