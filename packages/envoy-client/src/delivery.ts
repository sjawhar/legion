import { agentSubject } from "@legion/contracts";
import { encode } from "@toon-format/toon";
import { z } from "zod";

const KNOWN_SOURCES: Record<string, true> = {
  agent: true,
  human: true,
  envoy: true,
  github: true,
  slack: true,
  whatsapp: true,
  ghostwispr: true,
};
const FOREIGN_SESSION_ID = /\b01a0[0-9a-f]{4}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}\b/g;

const InboundSenderSchema = z.object({
  session_id: z.string().optional(),
  machine: z.string().optional(),
  cwd: z.string().optional(),
  title: z.string().optional(),
  roles: z.array(z.string()).optional(),
});

const InboundEnvelopeSchema = z
  .object({
    event_id: z.string().optional(),
    source: z.string(),
    source_session: z.string().optional(),
    topic: z.string().optional(),
    dedupe_key: z.string().optional(),
    issued_at: z.number().int().optional(),
    expires_at: z.number().int().optional(),
    payload_summary: z.string().optional(),
    payload: z.string().optional(),
    sender: InboundSenderSchema.optional(),
    in_reply_to: z.string().optional(),
    supersedes: z.string().optional(),
    urgency: z.string().optional(),
    expects_reply: z.string().optional(),
  })
  .passthrough();

function tolerantShape(shape: z.ZodRawShape): z.ZodRawShape {
  return Object.fromEntries(
    Object.entries(shape).map(([key, schema]) => [key, z.catch(schema, undefined)])
  );
}

const TolerantInboundEnvelopeSchema = z
  .object({
    ...tolerantShape(InboundEnvelopeSchema.shape),
    // `source` is required on valid envelopes but omitted/malformed frames still render as unknown.
    source: InboundEnvelopeSchema.shape.source.optional().catch(undefined),
    // A malformed sender member must not discard valid sender members.
    sender: z.object(tolerantShape(InboundSenderSchema.shape)).optional().catch(undefined),
  })
  .passthrough();

export type InboundEnvelope = z.infer<typeof InboundEnvelopeSchema>;

export type DeliveryEnvelope = Pick<
  InboundEnvelope,
  "source" | "source_session" | "payload" | "sender"
>;

export type RenderInboundResult = {
  readonly skip: boolean;
  readonly content: string;
  readonly envelope?: InboundEnvelope;
};

export function senderLabel(envelope: DeliveryEnvelope): string {
  const sender = envelope.source_session ?? envelope.source ?? "unknown";
  return envelope.sender?.title === undefined ? sender : `${sender} (${envelope.sender.title})`;
}

export function replyWith(envelope: DeliveryEnvelope): string | undefined {
  if (envelope.source !== "agent" || envelope.source_session === undefined) return undefined;
  return `envoy_send(session_id="${envelope.source_session}", message="...")`;
}

export function isOwnDispatchEcho(envelope: DeliveryEnvelope, sessionID: string): boolean {
  if (envelope.source !== "github" || envelope.payload === undefined) return false;

  try {
    const payload: unknown = JSON.parse(envelope.payload);
    return (
      typeof payload === "object" &&
      payload !== null &&
      "dispatch_session" in payload &&
      payload.dispatch_session === sessionID
    );
  } catch {
    return false;
  }
}

export function inboundTimestamp(milliseconds: number | undefined): string {
  if (milliseconds === undefined) return "unknown";
  const date = new Date(Math.floor(milliseconds / 1_000) * 1_000);
  return Number.isNaN(date.getTime()) ? "unknown" : date.toISOString().replace(".000Z", "Z");
}

export function renderInbound(
  raw: string,
  sessionID: string,
  subject?: string
): RenderInboundResult {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return {
      skip: false,
      content: encode({
        envoy: { topic: subject ?? "unknown", unrecognised: "payload was not JSON" },
      }),
    };
  }

  const parsed = InboundEnvelopeSchema.safeParse(value);
  const parseIssues = parsed.success
    ? []
    : parsed.error.issues.map((issue) => issue.path.join(".") || "envelope");
  let envelope: InboundEnvelope;
  if (parsed.success) {
    envelope = parsed.data;
  } else {
    const tolerant = TolerantInboundEnvelopeSchema.safeParse(value);
    if (!tolerant.success) {
      return {
        skip: false,
        content: encode({ envoy: { unrecognised: parseIssues.join(", ") || "envelope" } }),
      };
    }
    const data = tolerant.data as Partial<InboundEnvelope>;
    envelope = { ...data, source: data.source ?? "unknown" };
  }
  if (isOwnDispatchEcho(envelope, sessionID)) {
    return { skip: true, content: "", envelope };
  }

  const payloadSummary = envelope.payload_summary ?? "unknown";
  let message: unknown;
  if (envelope.payload !== undefined && envelope.payload !== envelope.payload_summary) {
    try {
      message = JSON.parse(envelope.payload);
    } catch {
      message = envelope.payload;
    }
  }
  const body = `${envelope.payload_summary ?? ""}\n${envelope.payload ?? ""}`;
  let foreignSession: string | undefined;
  for (const match of body.matchAll(FOREIGN_SESSION_ID)) {
    if (match[0] !== envelope.source_session && match[0] !== sessionID) {
      foreignSession = match[0];
      break;
    }
  }
  const role = envelope.sender?.roles?.[0];
  const reply = replyWith(envelope);
  const sourceIssue =
    parseIssues.includes("source") || KNOWN_SOURCES[envelope.source] !== undefined
      ? undefined
      : `source=${envelope.source}`;
  const unrecognised =
    [...parseIssues, ...(sourceIssue === undefined ? [] : [sourceIssue])].join(", ") || undefined;
  const rendered = {
    ...(envelope.topic === agentSubject(sessionID)
      ? { to: `you (${sessionID.slice(0, 4)}…)` }
      : {}),
    from: senderLabel(envelope),
    at: inboundTimestamp(envelope.issued_at),
    id: envelope.event_id ?? "unknown",
    ...(envelope.expires_at === undefined ? {} : { by: inboundTimestamp(envelope.expires_at) }),
    ...(envelope.urgency === undefined ? {} : { urgency: envelope.urgency }),
    ...(envelope.expects_reply === undefined ? {} : { expects_reply: envelope.expects_reply }),
    ...(envelope.in_reply_to === undefined ? {} : { re: envelope.in_reply_to }),
    ...(envelope.supersedes === undefined ? {} : { supersedes: envelope.supersedes }),
    ...(reply === undefined ? {} : { reply_with: reply }),
    ...(role === undefined
      ? {}
      : {
          reply_role: `envoy_publish(topic="notifications.role.${role}", message="...")`,
        }),
    summary: payloadSummary,
    ...(message === undefined ? {} : { message }),
    ...(foreignSession === undefined
      ? {}
      : {
          note: `body names session ${foreignSession}; the sender is ${envelope.source_session ?? "unknown"}`,
        }),
    ...(unrecognised === undefined ? {} : { unrecognised }),
  };

  return { skip: false, content: encode({ envoy: rendered }), envelope };
}
