// scripts/kind-smoke/envoy-bridge.ts — SMOKE_GITHUB_INGRESS=envoy: relay the sandbox repository's
// GitHub subjects from production NATS (read-only) into a kind smoke instance's own NATS. It
// subscribes upstream to exactly `notifications.github.<owner>.<repo>.>` and republishes each
// message unchanged downstream; it never publishes upstream, and it never touches Dispatch issue
// subjects — the instance's scratch Dispatch server publishes its own, and two rigs on one issue
// stream admitted each other's issues (docs/solutions/legion).
import { connect, type NatsConnection, type Subscription } from "nats";
import { EnvelopeSchema } from "../../packages/contracts/src/envelope";

export const DEFAULT_UPSTREAM_NATS_URL = "nats://envoy-nats.tailb86685.ts.net:4222";

const requiredEnvelopeFields = [
  "event_id",
  "source",
  "source_event_id",
  "topic",
  "dedupe_key",
  "issued_at",
  "payload_summary",
  "trace_id",
] as const;

const envelopeFields: Record<string, true> = {
  event_id: true,
  source: true,
  source_event_id: true,
  source_session: true,
  topic: true,
  dedupe_key: true,
  issued_at: true,
  expires_at: true,
  payload_summary: true,
  payload: true,
  payload_ref: true,
  trace_id: true,
  sender: true,
  in_reply_to: true,
  supersedes: true,
  urgency: true,
  expects_reply: true,
};

export interface BridgeConfig {
  repository: string;
  subjects: string[];
  upstreamUrl: string;
  downstreamUrl: string;
}

export type EnvelopeValidation =
  | { ok: true; shape: "current" }
  | { ok: false; missing: string[]; extra: string[]; errors: string[] };

export function bridgeConfigFromEnvironment(
  environment: Record<string, string | undefined>
): BridgeConfig {
  const repository = environment.SMOKE_REPO?.trim() ?? "";
  if (!/^[A-Za-z0-9][A-Za-z0-9.-]*\/[A-Za-z0-9][A-Za-z0-9._-]*$/.test(repository)) {
    throw new Error("SMOKE_REPO must be a literal <owner>/<repo> repository");
  }
  const downstreamUrl = environment.SMOKE_RIG_NATS?.trim() ?? "";
  if (!downstreamUrl) throw new Error("SMOKE_RIG_NATS is required");
  return {
    repository,
    subjects: [`notifications.github.${repository.replace("/", ".")}.>`],
    upstreamUrl: environment.SMOKE_UPSTREAM_NATS?.trim() || DEFAULT_UPSTREAM_NATS_URL,
    downstreamUrl,
  };
}

export function envelopeValidation(data: string | Uint8Array): EnvelopeValidation {
  const text = typeof data === "string" ? data : new TextDecoder().decode(data);
  let candidate: unknown;
  try {
    candidate = JSON.parse(text);
  } catch (error) {
    return {
      ok: false,
      missing: [],
      extra: [],
      errors: [`invalid JSON: ${error instanceof Error ? error.message : String(error)}`],
    };
  }
  if (typeof candidate !== "object" || candidate === null || Array.isArray(candidate)) {
    return {
      ok: false,
      missing: [...requiredEnvelopeFields],
      extra: [],
      errors: ["envelope must be a JSON object"],
    };
  }
  const envelope = candidate as Record<string, unknown>;
  const missing = requiredEnvelopeFields.filter((field) => !(field in envelope));
  const extra = Object.keys(envelope)
    .filter((field) => !(field in envelopeFields))
    .sort();
  const parsed = EnvelopeSchema.safeParse(envelope);
  const errors = parsed.success
    ? []
    : parsed.error.issues.map((issue) => `${issue.path.join(".") || "envelope"}: ${issue.message}`);
  if (missing.length === 0 && extra.length === 0 && errors.length === 0) {
    return { ok: true, shape: "current" };
  }
  return { ok: false, missing, extra, errors };
}

function validationFailure(validation: Exclude<EnvelopeValidation, { ok: true }>): string {
  return `missing=[${validation.missing.join(",")}] extra=[${validation.extra.join(",")}] errors=[${validation.errors.join("; ")}]`;
}

async function forwardMessages(
  subscription: Subscription,
  subject: string,
  downstream: NatsConnection
): Promise<void> {
  const subjectPrefix = subject.slice(0, -1);
  let firstMessage = true;
  for await (const message of subscription) {
    if (!message.subject.startsWith(subjectPrefix)) {
      throw new Error(`refusing out-of-scope upstream subject ${message.subject}`);
    }
    if (firstMessage) {
      firstMessage = false;
      const validation = envelopeValidation(message.data);
      if (!validation.ok) {
        const reason = validationFailure(validation);
        console.error(`BRIDGE UNHEALTHY first-envelope subject=${message.subject} ${reason}`);
        throw new Error(`first bridged envelope is incompatible: ${reason}`);
      }
      console.log(
        `BRIDGE VALIDATION shape=${validation.shape} subject=${message.subject} bytes=${message.data.byteLength}`
      );
    }
    downstream.publishMessage(message);
    await downstream.flush();
    console.log(`BRIDGED subject=${message.subject} bytes=${message.data.byteLength}`);
  }
}

export async function runBridge(config: BridgeConfig): Promise<void> {
  const upstream = await connect({
    servers: config.upstreamUrl,
    name: `legion-kind-smoke-bridge-upstream-${config.repository}`,
    reconnect: true,
    maxReconnectAttempts: -1,
    reconnectTimeWait: 2_000,
  });
  let downstream: NatsConnection | undefined;
  try {
    downstream = await connect({
      servers: config.downstreamUrl,
      name: `legion-kind-smoke-bridge-downstream-${config.repository}`,
      reconnect: true,
      maxReconnectAttempts: -1,
      reconnectTimeWait: 2_000,
    });
    const activeDownstream = downstream;
    const subscriptions = config.subjects.map((subject) => upstream.subscribe(subject));
    const stop = () => {
      for (const subscription of subscriptions) subscription.unsubscribe();
    };
    process.once("SIGINT", stop);
    process.once("SIGTERM", stop);
    try {
      await upstream.flush();
      await downstream.flush();
      console.log(
        `BRIDGE READY subjects=${config.subjects.join(",")} upstream=${config.upstreamUrl} downstream=${config.downstreamUrl}`
      );
      await Promise.all(
        subscriptions.map((subscription, index) =>
          forwardMessages(subscription, config.subjects[index] as string, activeDownstream)
        )
      );
    } finally {
      process.off("SIGINT", stop);
      process.off("SIGTERM", stop);
    }
  } finally {
    await Promise.allSettled([upstream.drain(), ...(downstream ? [downstream.drain()] : [])]);
  }
}

if (import.meta.main) {
  await runBridge(bridgeConfigFromEnvironment(process.env)).catch((error) => {
    console.error(`BRIDGE UNHEALTHY ${error instanceof Error ? error.message : String(error)}`);
    process.exitCode = 1;
  });
}
