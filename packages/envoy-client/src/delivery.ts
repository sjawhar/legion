import { agentSubject, EnvelopeSchema } from "@legion/contracts";
import { encode } from "@toon-format/toon";
import { z } from "zod";
import type { Event } from "./dispatch-http";

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

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function asDispatchEvent(value: unknown): Event | null {
  const event = asRecord(value);
  if (
    event === null ||
    typeof event.issue_key !== "string" ||
    typeof event.type !== "string" ||
    typeof event.actor !== "object" ||
    event.actor === null ||
    typeof event.payload !== "object" ||
    event.payload === null
  ) {
    return null;
  }
  return event as unknown as Event;
}

function dispatchActorLabel(actor: Event["actor"]): string {
  return `${actor.kind} ${actor.id ?? "unknown"}`;
}

function dispatchBody(event: Event): string[] {
  const payload = asRecord(event.payload);
  if (payload === null) return [];
  const lines: string[] = [];
  const text = (name: string): string | undefined =>
    typeof payload[name] === "string" ? (payload[name] as string) : undefined;

  switch (event.type) {
    case "issue.created":
    case "issue.updated":
    case "issue.closed": {
      const title = text("title");
      const status = text("status");
      const route = text("route");
      if (title) lines.push(`Title: ${title}`);
      if (status) lines.push(`Status: ${status}`);
      if (route) lines.push(`Route: ${route}`);
      break;
    }
    case "artifact.created": {
      const name = text("name");
      if (name) lines.push(`Artifact: ${name}`);
      break;
    }
    case "artifact.version": {
      const name = text("name");
      const version = asRecord(payload.version);
      if (name) lines.push(`Artifact: ${name}`);
      if (typeof version?.number === "number") lines.push(`Version: ${version.number}`);
      if (typeof version?.summary === "string") lines.push(`Summary: ${version.summary}`);
      const diff = text("diff");
      if (diff) lines.push("Diff:", diff);
      break;
    }
    case "ask.opened":
    case "ask.answered": {
      const question = text("question");
      if (question) lines.push(`Question: ${question}`);
      if (event.type === "ask.opened") {
        const options = Array.isArray(payload.options)
          ? payload.options
              .map((option) => asRecord(option)?.label)
              .filter((label): label is string => typeof label === "string")
          : [];
        if (options.length > 0) lines.push(`Options: ${options.join(", ")}`);
      } else {
        const answer = asRecord(payload.answer);
        const selected = Array.isArray(answer?.selected)
          ? answer.selected.filter((choice): choice is string => typeof choice === "string")
          : [];
        lines.push(`Selected: ${selected.join(", ") || "none"}`);
        if (typeof answer?.text === "string") lines.push(`Text: ${answer.text}`);
      }
      break;
    }
    case "comment.created":
    case "comment.resolved": {
      const artifactName = text("artifact_name");
      const anchor = asRecord(payload.anchor);
      const replyTo = text("reply_to");
      const body = text("body");
      if (artifactName) lines.push(`Artifact: ${artifactName}`);
      if (typeof anchor?.quote === "string") lines.push(`> ${anchor.quote}`);
      if (replyTo) lines.push(`Reply chain: ${replyTo}`);
      if (body) lines.push(`Body: ${body}`);
      break;
    }
    case "suggestion.accepted":
    case "suggestion.rejected": {
      const artifactName = text("artifact_name");
      const quote = asRecord(payload.anchor)?.quote;
      const replacement = asRecord(payload.suggestion)?.replace_with;
      if (artifactName) lines.push(`Artifact: ${artifactName}`);
      if (typeof quote === "string") lines.push(`> ${quote}`);
      if (typeof quote === "string" && typeof replacement === "string") {
        lines.push(`${quote} → ${replacement}`);
      }
      break;
    }
    case "message.created": {
      const body = text("body");
      if (body) lines.push(`Body: ${body}`);
      break;
    }
    case "child.status": {
      const child = text("child_key");
      const from = text("from");
      const to = text("to");
      if (child && from && to) lines.push(`Child: ${child} ${from} → ${to}`);
      break;
    }
  }
  return lines;
}

function renderDispatch(envelope: InboundEnvelope, sessionID: string): RenderInboundResult | null {
  if (envelope.payload === undefined) return null;
  let rawEvent: unknown;
  try {
    rawEvent = JSON.parse(envelope.payload);
  } catch {
    return null;
  }
  const event = asDispatchEvent(rawEvent);
  if (event === null) return null;
  if (event.actor.kind === "session" && event.actor.id === sessionID) {
    return { skip: true, content: "", envelope };
  }
  return {
    skip: false,
    content: [
      `dispatch ${event.issue_key} · ${event.type} · ${dispatchActorLabel(event.actor)}`,
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
