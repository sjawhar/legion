import { EnvelopeSchema } from "../../packages/contracts/src/envelope";
import { connect, type NatsConnection } from "nats";

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
};

export type BridgeConfig = {
  repository: string;
  subject: string;
  upstreamUrl: string;
  downstreamUrl: string;
};

export type EnvelopeValidation =
  | { valid: true; shape: "current" }
  | { valid: false; missing: string[]; extra: string[]; errors: string[] };

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
    subject: `notifications.github.${repository.replace("/", ".")}.>`,
    upstreamUrl: environment.SMOKE_UPSTREAM_NATS?.trim() || DEFAULT_UPSTREAM_NATS_URL,
    downstreamUrl,
  };
}

export function envelopeValidation(data: string): EnvelopeValidation {
  let candidate: unknown;
  try {
    candidate = JSON.parse(data);
  } catch (error) {
    return {
      valid: false,
      missing: [],
      extra: [],
      errors: [`invalid JSON: ${error instanceof Error ? error.message : String(error)}`],
    };
  }

  if (typeof candidate !== "object" || candidate === null || Array.isArray(candidate)) {
    return {
      valid: false,
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
    return { valid: true, shape: "current" };
  }
  return { valid: false, missing, extra, errors };
}

function validationFailure(validation: Exclude<EnvelopeValidation, { valid: true }>): string {
  return `missing=[${validation.missing.join(",")}] extra=[${validation.extra.join(",")}] errors=[${validation.errors.join("; ")}]`;
}


export async function runBridge(config: BridgeConfig): Promise<void> {
  const upstream = await connect({
    servers: config.upstreamUrl,
    name: `legion-smoke-envoy-bridge-upstream-${config.repository}`,
    reconnect: true,
    maxReconnectAttempts: -1,
    reconnectTimeWait: 2_000,
  });
  let downstream: NatsConnection | undefined;
  try {
    downstream = await connect({
      servers: config.downstreamUrl,
      name: `legion-smoke-envoy-bridge-downstream-${config.repository}`,
      reconnect: true,
      maxReconnectAttempts: -1,
      reconnectTimeWait: 2_000,
    });
    const subscription = upstream.subscribe(config.subject);
    const stop = () => subscription.unsubscribe();
    process.once("SIGINT", stop);
    process.once("SIGTERM", stop);

    try {
      await upstream.flush();
      await downstream.flush();
      console.log(
        `BRIDGE READY subject=${config.subject} upstream=${config.upstreamUrl} downstream=${config.downstreamUrl}`
      );

      let firstMessage = true;
      for await (const message of subscription) {
        if (!message.subject.startsWith(config.subject.slice(0, -1))) {
          throw new Error(`refusing out-of-scope upstream subject ${message.subject}`);
        }
        if (firstMessage) {
          firstMessage = false;
          const validation = envelopeValidation(new TextDecoder().decode(message.data));
          if (!validation.valid) {
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
    } finally {
      process.off("SIGINT", stop);
      process.off("SIGTERM", stop);
    }
  } finally {
    await Promise.allSettled([
      upstream.drain(),
      ...(downstream ? [downstream.drain()] : []),
    ]);
  }
}

if (import.meta.main) {
  await runBridge(bridgeConfigFromEnvironment(process.env)).catch((error) => {
    console.error(`BRIDGE UNHEALTHY ${error instanceof Error ? error.message : String(error)}`);
    process.exitCode = 1;
  });
}
