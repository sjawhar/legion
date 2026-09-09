import { agentSubject, EnvelopeSchema } from "@legion/contracts";
import { encode } from "@toon-format/toon";
import { z } from "zod";

const KNOWN_SOURCES: Readonly<Record<string, unknown>> = EnvelopeSchema.shape.source.enum;
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

// Dispatch event payloads follow the wire contract: issue.* carries the Issue,
// ask.* the Ask, message.created the Message, comment.*/suggestion.* the Comment
// plus artifact_name, artifact.created {artifact}, artifact.version
// {artifact_id, name, version, diff?}, and child.status {child_key, from, to}.
// Bus frames are untrusted, so each schema below declares exactly the fields
// this renderer surfaces and a frame that disagrees renders without them.
const DispatchEventSchema = z.object({
  issue_key: z.string(),
  type: z.string(),
  actor: z.object({ kind: z.string(), id: z.string().optional() }).passthrough(),
  payload: z.object({}).passthrough(),
});

type DispatchEvent = z.infer<typeof DispatchEventSchema>;

const IssuePayloadSchema = z.object({
  title: z.string().optional(),
  status: z.string().optional(),
  route: z.string().nullish(),
});

const ArtifactCreatedPayloadSchema = z.object({
  artifact: z.object({ name: z.string().optional() }).optional(),
});

const ArtifactVersionPayloadSchema = z.object({
  name: z.string().optional(),
  version: z.object({ number: z.number().optional(), summary: z.string().nullish() }).optional(),
  diff: z.string().optional(),
});

const AskPayloadSchema = z.object({
  // A nil Go slice marshals to null, so every list here is nullable.
  question: z.string().optional(),
  options: z.array(z.object({ label: z.string().optional() })).nullish(),
  answer: z
    .object({ selected: z.array(z.string()).nullish(), text: z.string().nullish() })
    .nullish(),
});

const CommentPayloadSchema = z.object({
  artifact_name: z.string().optional(),
  body: z.string().optional(),
  reply_to: z.string().nullish(),
  anchor: z.object({ quote: z.string().optional() }).nullish(),
  suggestion: z.object({ replace_with: z.string().optional() }).nullish(),
});

const MessagePayloadSchema = z.object({ body: z.string().optional() });

const ChildStatusPayloadSchema = z.object({
  child_key: z.string().optional(),
  from: z.string().optional(),
  to: z.string().optional(),
});

// Each Dispatch event type validates its payload with the schema that matches the
// wire contract documented above. A frame whose payload disagrees with its type's
// schema keeps the raw (already-parsed) payload object rather than dropping data.
const DISPATCH_PAYLOAD_SCHEMAS: Readonly<Record<string, z.ZodType>> = {
  "issue.created": IssuePayloadSchema,
  "issue.updated": IssuePayloadSchema,
  "issue.closed": IssuePayloadSchema,
  "artifact.created": ArtifactCreatedPayloadSchema,
  "artifact.version": ArtifactVersionPayloadSchema,
  "ask.opened": AskPayloadSchema,
  "ask.answered": AskPayloadSchema,
  "comment.created": CommentPayloadSchema,
  "comment.resolved": CommentPayloadSchema,
  "suggestion.accepted": CommentPayloadSchema,
  "suggestion.rejected": CommentPayloadSchema,
  "message.created": MessagePayloadSchema,
  "child.status": ChildStatusPayloadSchema,
};

function dispatchPayload(event: DispatchEvent): unknown {
  const schema = DISPATCH_PAYLOAD_SCHEMAS[event.type];
  if (schema === undefined) return event.payload;
  const parsed = schema.safeParse(event.payload);
  return parsed.success ? parsed.data : event.payload;
}

// A Dispatch bus frame's JSON payload either matches the wire contract documented
// above (`event`) or it doesn't — invalid JSON, or a JSON value that disagrees with
// `DispatchEventSchema` — in which case the raw parsed value (object or string) is
// kept (`raw`) so no data is dropped and nothing renders as a hand-built text
// template.
type DispatchFrame = { readonly event: DispatchEvent } | { readonly raw: unknown };

function parseDispatchFrame(rawPayload: string): DispatchFrame {
  let value: unknown;
  try {
    value = JSON.parse(rawPayload);
  } catch {
    return { raw: rawPayload };
  }
  const parsed = DispatchEventSchema.safeParse(value);
  return parsed.success ? { event: parsed.data } : { raw: value };
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
  let dispatchEvent: unknown;
  let dispatchIssue: string | undefined;
  const dispatchRendered = envelope.source === "dispatch" && envelope.payload !== undefined;
  if (envelope.source === "dispatch") {
    if (envelope.payload === undefined) {
      dispatchIssue = "payload";
    } else {
      const frame = parseDispatchFrame(envelope.payload);
      if ("event" in frame) {
        if (frame.event.actor.kind === "session" && frame.event.actor.id === sessionID) {
          return { skip: true, content: "", envelope };
        }
        dispatchEvent = {
          issue_key: frame.event.issue_key,
          type: frame.event.type,
          actor: frame.event.actor,
          payload: dispatchPayload(frame.event),
        };
      } else {
        dispatchEvent = frame.raw;
      }
    }
  }

  let parsedPayload: unknown;
  if (
    !dispatchRendered &&
    envelope.payload !== undefined &&
    (envelope.source === "github" || envelope.payload !== envelope.payload_summary)
  ) {
    try {
      parsedPayload = JSON.parse(envelope.payload);
    } catch {
      parsedPayload = envelope.payload;
    }
  }

  const payloadSummary = envelope.payload_summary ?? "unknown";
  const message =
    !dispatchRendered &&
    envelope.payload !== undefined &&
    envelope.payload !== envelope.payload_summary
      ? parsedPayload
      : undefined;
  const summaryIsHead =
    typeof message === "string" &&
    envelope.payload_summary !== undefined &&
    message.startsWith(
      envelope.payload_summary.endsWith("…")
        ? envelope.payload_summary.slice(0, -1)
        : envelope.payload_summary
    );
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
    [
      ...parseIssues,
      ...(sourceIssue === undefined ? [] : [sourceIssue]),
      ...(dispatchIssue === undefined ? [] : [dispatchIssue]),
    ].join(", ") || undefined;
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
    ...(dispatchRendered
      ? { dispatch: dispatchEvent }
      : {
          ...(summaryIsHead ? {} : { summary: payloadSummary }),
          ...(message === undefined ? {} : { message }),
        }),
    ...(foreignSession === undefined
      ? {}
      : {
          note: `body names session ${foreignSession}; the sender is ${envelope.source_session ?? "unknown"}`,
        }),
    ...(unrecognised === undefined ? {} : { unrecognised }),
  };

  return { skip: false, content: encode({ envoy: rendered }), envelope };
}
