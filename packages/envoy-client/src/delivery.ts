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
// this renderer prints and a frame that disagrees renders without them.
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

function dispatchActorLabel(actor: DispatchEvent["actor"]): string {
  return `${actor.kind} ${actor.id ?? "unknown"}`;
}

function dispatchBody(event: DispatchEvent): string[] {
  const lines: string[] = [];

  switch (event.type) {
    case "issue.created":
    case "issue.updated":
    case "issue.closed": {
      const parsed = IssuePayloadSchema.safeParse(event.payload);
      if (!parsed.success) break;
      if (parsed.data.title) lines.push(`Title: ${parsed.data.title}`);
      if (parsed.data.status) lines.push(`Status: ${parsed.data.status}`);
      if (parsed.data.route) lines.push(`Route: ${parsed.data.route}`);
      break;
    }
    case "artifact.created": {
      const parsed = ArtifactCreatedPayloadSchema.safeParse(event.payload);
      const name = parsed.success ? parsed.data.artifact?.name : undefined;
      if (name) lines.push(`Artifact: ${name}`);
      break;
    }
    case "artifact.version": {
      const parsed = ArtifactVersionPayloadSchema.safeParse(event.payload);
      if (!parsed.success) break;
      const version = parsed.data.version;
      if (parsed.data.name) lines.push(`Artifact: ${parsed.data.name}`);
      if (version?.number !== undefined) lines.push(`Version: ${version.number}`);
      if (version?.summary) lines.push(`Summary: ${version.summary}`);
      if (parsed.data.diff) lines.push("Diff:", parsed.data.diff);
      break;
    }
    case "ask.opened":
    case "ask.answered": {
      const parsed = AskPayloadSchema.safeParse(event.payload);
      if (!parsed.success) break;
      if (parsed.data.question) lines.push(`Question: ${parsed.data.question}`);
      if (event.type === "ask.opened") {
        const options = (parsed.data.options ?? [])
          .map((option) => option.label)
          .filter((label): label is string => label !== undefined);
        if (options.length > 0) lines.push(`Options: ${options.join(", ")}`);
        break;
      }
      const answer = parsed.data.answer;
      lines.push(`Selected: ${answer?.selected?.join(", ") || "none"}`);
      if (answer?.text) lines.push(`Text: ${answer.text}`);
      break;
    }
    case "comment.created":
    case "comment.resolved": {
      const parsed = CommentPayloadSchema.safeParse(event.payload);
      if (!parsed.success) break;
      const quote = parsed.data.anchor?.quote;
      if (parsed.data.artifact_name) lines.push(`Artifact: ${parsed.data.artifact_name}`);
      if (quote) lines.push(`> ${quote}`);
      if (parsed.data.reply_to) lines.push(`Reply chain: ${parsed.data.reply_to}`);
      if (parsed.data.body) lines.push(`Body: ${parsed.data.body}`);
      break;
    }
    case "suggestion.accepted":
    case "suggestion.rejected": {
      const parsed = CommentPayloadSchema.safeParse(event.payload);
      if (!parsed.success) break;
      const quote = parsed.data.anchor?.quote;
      const replacement = parsed.data.suggestion?.replace_with;
      if (parsed.data.artifact_name) lines.push(`Artifact: ${parsed.data.artifact_name}`);
      if (quote) lines.push(`> ${quote}`);
      if (quote && replacement) lines.push(`${quote} → ${replacement}`);
      break;
    }
    case "message.created": {
      const parsed = MessagePayloadSchema.safeParse(event.payload);
      const body = parsed.success ? parsed.data.body : undefined;
      if (body) lines.push(`Body: ${body}`);
      break;
    }
    case "child.status": {
      const parsed = ChildStatusPayloadSchema.safeParse(event.payload);
      if (!parsed.success) break;
      const { child_key: child, from, to } = parsed.data;
      if (child && from && to) lines.push(`Child: ${child} ${from} → ${to}`);
      break;
    }
  }
  return lines;
}

function renderDispatch(envelope: InboundEnvelope, sessionID: string): RenderInboundResult {
  const invalid = (): RenderInboundResult => ({
    skip: false,
    content: "dispatch: invalid event",
    envelope,
  });
  if (envelope.payload === undefined) return invalid();
  let rawEvent: unknown;
  try {
    rawEvent = JSON.parse(envelope.payload);
  } catch {
    return invalid();
  }
  const parsed = DispatchEventSchema.safeParse(rawEvent);
  if (!parsed.success) return invalid();
  const event = parsed.data;
  if (event.actor.kind === "session" && event.actor.id === sessionID) {
    return { skip: true, content: "", envelope };
  }
  return {
    skip: false,
    content: [
      `dispatch ${event.issue_key} · ${event.type} · by ${dispatchActorLabel(event.actor)}`,
      ...dispatchBody(event),
    ].join("\n"),
    envelope,
  };
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
  if (envelope.source === "dispatch") {
    const renderedDispatch = renderDispatch(envelope, sessionID);
    if (renderedDispatch !== null) return renderedDispatch;
  }

  let parsedPayload: unknown;
  if (
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
    envelope.payload !== undefined && envelope.payload !== envelope.payload_summary
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
    ...(summaryIsHead ? {} : { summary: payloadSummary }),
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
