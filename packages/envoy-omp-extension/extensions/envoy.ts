import * as os from "node:os";
import { agentSubject } from "@legion/contracts";
import { envoyDefaultsFromEnvironment } from "@legion/envoy-client/defaults";
import { toEnvelopeDisplay } from "@legion/envoy-client/display";
import { EnvoyToolOperation, envoyToolSpecs } from "@legion/envoy-client/tool-contract";
import { createEnvoyClient } from "@legion/envoy-client/transport";
import { connect, type NatsConnection, StringCodec, type Subscription } from "nats";

type ToolParameters = Readonly<Record<string, string | readonly string[] | undefined>>;

type ToolResult = {
  readonly content: readonly { readonly type: "text"; readonly text: string }[];
  readonly details: Readonly<Record<string, unknown>>;
  readonly isError?: boolean;
};

type SessionContext = {
  readonly cwd: string;
  readonly sessionManager: { readonly getSessionId: () => string };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  readonly ui: { readonly notify: (message: string, level: "warning") => void };
};

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
  readonly on: (
    event: "session_start" | "session_shutdown",
    handler: (event: unknown, context: SessionContext) => Promise<void>,
  ) => void;
  readonly sendMessage: (
    message: { readonly customType: string; readonly content: string; readonly display: boolean },
    options: { readonly deliverAs: "followUp"; readonly triggerTurn: boolean },
  ) => void;
};

const codec = StringCodec();
const NATS_RETRY_INTERVAL_MS = 15_000;

export default function envoyExtension(pi: PiApi): void {
  const defaults = envoyDefaultsFromEnvironment(process.env);
  const client = createEnvoyClient({ baseUrl: defaults.envoyUrl, fetch });
  const subscriptions = new Map<string, Subscription>();
  const dedupeKeys = new Set<string>();
  const machineID = os.hostname();
  let connection: NatsConnection | undefined;
  let sessionDirectory = "";
  let sessionID = "";

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

  const deliver = (subject: string, raw: string): void => {
    let summary = raw;
    let source = "";
    try {
      const display = toEnvelopeDisplay(JSON.parse(raw));
      if (dedupeKeys.has(display.dedupeKey)) return;
      dedupeKeys.add(display.dedupeKey);
      if (dedupeKeys.size > 1000) {
        const oldest = dedupeKeys.values().next();
        if (!oldest.done) dedupeKeys.delete(oldest.value);
      }
      summary = display.summary;
      source = display.sourceSessionID === undefined ? "" : ` from ${display.sourceSessionID}`;
    } catch {
      summary = raw;
    }
    pi.sendMessage(
      { customType: "envoy-message", content: `[ENVOY message on topic "${subject}"${source}]\n${summary}`, display: true },
      { deliverAs: "followUp", triggerTurn: true },
    );
  };

  const subscribe = async (topic: string): Promise<boolean> => {
    if (subscriptions.has(topic)) return false;
    const subscription = (await ensureConnection()).subscribe(topic);
    subscriptions.set(topic, subscription);
    void (async () => {
      try {
        for await (const message of subscription) deliver(message.subject, codec.decode(message.data));
      } finally {
        subscriptions.delete(topic);
      }
    })();
    return true;
  };

  const registerSession = async (context: SessionContext): Promise<void> => {
    await client.subscribe({
      sessionID,
      directory: context.cwd,
      topics: [agentSubject(sessionID)],
      port: 0,
      title: "",
      driving: false,
      selfSubscribed: true,
    });
  };

  pi.on("session_start", async (_event, context) => {
    sessionDirectory = context.cwd;
    sessionID = context.sessionManager.getSessionId();
    if (defaults.natsUrls.length === 0) {
      context.ui.notify(
        "envoy: ENVOY_NATS_URL is not set; inbound envoy messages are disabled (outbound tools still work)",
        "warning",
      );
      return;
    }
    let established = false;
    const establish = async (): Promise<void> => {
      await ensureConnection();
      await subscribe(agentSubject(sessionID));
      if (process.env.ENVOY_REGISTER_SESSION === "1") {
        await registerSession(context);
        context.setInterval(() => void registerSession(context), defaults.heartbeatMs);
      }
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

  pi.on("session_shutdown", async () => {
    try {
      await connection?.drain();
    } finally {
      connection = undefined;
      subscriptions.clear();
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

  async function execute(operation: EnvoyToolOperation, parameters: ToolParameters): Promise<ToolResult> {
    try {
      switch (operation) {
        case EnvoyToolOperation.subscribe: {
          const added: string[] = [];
          const already: string[] = [];
          for (const topic of topicsFor(parameters)) (await subscribe(topic) ? added : already).push(topic);
          return success(`Subscribed: ${added.join(", ") || "(none new)"}`, { added, already });
        }
        case EnvoyToolOperation.unsubscribe: {
          const targets = topicsFor(parameters, [...subscriptions.keys()]);
          const removed = targets.filter((topic) => {
            const subscription = subscriptions.get(topic);
            if (subscription === undefined) return false;
            subscription.unsubscribe();
            subscriptions.delete(topic);
            return true;
          });
          return success(`Unsubscribed: ${removed.join(", ") || "(none)"}`, { removed });
        }
        case EnvoyToolOperation.listInterests:
          return success(JSON.stringify(await client.getInterest(sessionID), null, 2));
        case EnvoyToolOperation.send: {
          const targetSessionID = stringFor(parameters, "target_session");
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
          await client.setRole({ sessionID, role });
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
      return z.object({ target_session: z.string(), message: z.string() });
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
