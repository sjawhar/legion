import {
  ArtifactCreatedEventPayloadSchema as ArtifactCreatedPayloadSchema,
  ArtifactVersionEventPayloadSchema as ArtifactVersionPayloadSchema,
  AskEditedEventPayloadSchema as AskEditedPayloadSchema,
  AskFollowerEventPayloadSchema as AskFollowerPayloadSchema,
  AskEventPayloadSchema as AskPayloadSchema,
  agentSubject,
  ChildStatusEventPayloadSchema as ChildStatusPayloadSchema,
  CommentEventPayloadSchema as CommentPayloadSchema,
  type DeliveryCapability,
  DISPATCH_DOCUMENT_TOPIC_PREFIX,
  DISPATCH_TOPIC_PREFIX,
  type InboundDispatchEvent as DispatchEvent,
  DispatchEventSchema,
  type DispatchTargetedCommentDeliverySchema,
  DispatchTargetedCommentPayloadSchema,
  DispatchTargetedDeliverySchema,
  DispatchTargetedMessagePayloadSchema,
  DispatchTargetedResourceIDSchema,
  EnvelopeSchema,
  IssueEventPayloadSchema as IssuePayloadSchema,
  MessageDeliveryEventPayloadSchema as MessageDeliveryPayloadSchema,
  MessageEventPayloadSchema as MessagePayloadSchema,
  SubscriptionRemovedEventPayloadSchema as SubscriptionRemovedPayloadSchema,
} from "@legion/contracts";
import { encode } from "@toon-format/toon";
import { z } from "zod";
import { askAnswerText, textHead } from "./ask-answer";
import { dispatchChildRef, dispatchDocumentRef, dispatchIssueRef } from "./dispatch-owner";

const KNOWN_SOURCES: Readonly<Record<string, unknown>> = EnvelopeSchema.shape.source.enum;
const FOREIGN_SESSION_ID = /\b01a0[0-9a-f]{4}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}\b/g;
const AskAuthorPayloadSchema = z.object({
  author: z.object({ kind: z.string(), id: z.string() }).passthrough(),
});

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
export type DispatchDelivery = {
  readonly resource: "message" | "comment";
  readonly id: string;
  readonly attempt: number;
  readonly mode: DeliveryCapability;
  readonly replyPath: string;
  readonly replyFields: Readonly<Record<string, string>>;
  readonly issueKey: string | null;
  readonly body: string;
};

function dispatchReplyPath(resource: DispatchDelivery["resource"], id: string): string {
  const collection = resource === "message" ? "messages" : "comments";
  return `/api/v1/${collection}/${encodeURIComponent(id)}/reply`;
}

/**
 * Answers a targeted Dispatch delivery on the sender's behalf: the reply body when the
 * host delivered it, or the error when it could not (so the sender sees the attempt fail
 * instead of waiting on a reply that never comes). A plain fetch with no deadline of its own.
 */
export async function postDeliveryReply(
  config: { readonly url: string; readonly token: string },
  sessionId: string,
  delivery: DispatchDelivery,
  result: { readonly body?: string; readonly error?: string }
): Promise<void> {
  const response = await fetch(
    `${config.url}${dispatchReplyPath(delivery.resource, delivery.id)}`,
    {
      method: "POST",
      headers: { Authorization: `Bearer ${config.token}`, "Content-Type": "application/json" },
      body: JSON.stringify({
        actor: { kind: "session", id: sessionId },
        attempt: delivery.attempt,
        ...delivery.replyFields,
        ...result,
      }),
    }
  );
  if (!response.ok) {
    throw new Error(`Dispatch reply failed: ${response.status} ${await response.text()}`);
  }
}

/**
 * Whether an inbound frame is a forwarded role lane waiting for a receipt on `reply`. The
 * listener forwards a role-lane event to the holder's direct subject as a core request with
 * the envelope's `topic` still the role topic, and reports a missing receipt as a failed
 * delivery. Every other frame on the direct subject — a Dispatch author route, a peer
 * envoy_send — is a JetStream publish whose reply inbox belongs to the server's PubAck; an
 * empty receipt there fails the publisher with `nats: invalid jetstream publish response`.
 * `reply` is absent as `""` (nats.js's getter) or `undefined` (a host that omits the field).
 */
export function expectsLaneReceipt<
  Frame extends {
    readonly subject: string;
    readonly directSubject: string;
    readonly envelopeTopic: string | undefined;
    readonly reply: string | undefined;
  },
>(frame: Frame): frame is Frame & { readonly reply: string } {
  return (
    frame.reply !== undefined &&
    frame.reply !== "" &&
    frame.subject === frame.directSubject &&
    frame.envelopeTopic !== undefined &&
    frame.envelopeTopic !== frame.directSubject
  );
}

/** Remembers `key` in a dedupe set, dropping the oldest entry once the set exceeds `limit`. */
export function rememberBounded(seen: Set<string>, key: string, limit: number): void {
  seen.add(key);
  if (seen.size > limit) {
    const oldest = seen.values().next();
    if (!oldest.done) seen.delete(oldest.value);
  }
}

function isCommentTargetedDelivery(
  delivery: z.infer<typeof DispatchTargetedDeliverySchema>
): delivery is z.infer<typeof DispatchTargetedCommentDeliverySchema> {
  return "comment_id" in delivery;
}

const DispatchTargetedFrameSchema = z
  .object({
    event: DispatchEventSchema,
    delivery: DispatchTargetedDeliverySchema,
  })
  .superRefine(({ event, delivery }, context) => {
    if (
      event.type === "message.created" &&
      !isCommentTargetedDelivery(delivery) &&
      DispatchTargetedMessagePayloadSchema.safeParse(event.payload).success
    ) {
      return;
    }
    if (
      event.type === "comment.created" &&
      isCommentTargetedDelivery(delivery) &&
      DispatchTargetedCommentPayloadSchema.safeParse(event.payload).success
    ) {
      return;
    }
    context.addIssue({
      code: "custom",
      message:
        event.type === "comment.created"
          ? "targeted delivery requires a complete comment.created event"
          : "targeted delivery requires a complete message.created event",
    });
  });

const RecoverableDispatchDeliveryFailureSchema = z
  .object({
    event: z
      .object({
        type: z.enum(["message.created", "comment.created"]),
        issue_key: z.string().nullable(),
        payload: z.object({ id: z.string(), body: z.string().optional() }).passthrough(),
      })
      .passthrough(),
    delivery: DispatchTargetedDeliverySchema,
  })
  .refine(
    ({ event, delivery }) =>
      event.type === "comment.created"
        ? isCommentTargetedDelivery(delivery)
        : !isCommentTargetedDelivery(delivery) &&
          DispatchTargetedResourceIDSchema.safeParse(event.payload.id).success,
    { message: "delivery resource must match its event type" }
  );

export type DeliveryEnvelope = Pick<
  InboundEnvelope,
  "source" | "source_session" | "payload" | "sender" | "event_id"
>;

export type RenderInboundResult = {
  readonly skip: boolean;
  readonly content: string;
  readonly envelope?: InboundEnvelope;
  /** Actor of a Dispatch event that passed the wire schema. */
  readonly dispatchActor?: DispatchEvent["actor"];
  readonly delivery?: DispatchDelivery;
  readonly rejectedDelivery?: DispatchDelivery;
  readonly malformedDelivery?: true;
};

export function senderLabel(envelope: DeliveryEnvelope): string {
  const sender = envelope.source_session ?? envelope.source ?? "unknown";
  return envelope.sender?.title === undefined ? sender : `${sender} (${envelope.sender.title})`;
}

/** A ready-to-issue tool call any host can act on (OMP devices and the Claude bridge's MCP tools share these names). */
export interface ReplyHint {
  readonly tool: string;
  readonly args: Readonly<Record<string, string>>;
}

export function replyWith(envelope: DeliveryEnvelope): ReplyHint | undefined {
  if (envelope.source !== "agent" || envelope.source_session === undefined) return undefined;
  return {
    tool: "envoy_send",
    args: {
      session_id: envelope.source_session,
      ...(envelope.event_id === undefined ? {} : { in_reply_to: envelope.event_id }),
      message: "...",
    },
  };
}

// Dispatch event payloads follow the wire contract: issue.* carries the Issue,
// ask.opened/answered/resolved the Ask, ask.edited the edited Ask plus its prior
// fields and editor, message.created the Message, comment.*/suggestion.* the Comment
// plus artifact_name, artifact.created {artifact}, artifact.version
// {artifact_id, name, version, diff?}, and child.status {child_key, from, to}.
// Bus frames are untrusted, so each schema below declares exactly the fields
// this renderer surfaces and a frame that disagrees renders without them.

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
  "ask.edited": AskEditedPayloadSchema,
  "ask.answered": AskPayloadSchema,
  "ask.resolved": AskPayloadSchema,
  "comment.created": CommentPayloadSchema,
  "comment.resolved": CommentPayloadSchema,
  "comment.reopened": CommentPayloadSchema,
  "comment.edited": CommentPayloadSchema,
  "suggestion.accepted": CommentPayloadSchema,
  "suggestion.rejected": CommentPayloadSchema,
  "message.created": MessagePayloadSchema,
  "message.delivery": MessageDeliveryPayloadSchema,
  "message.answered": MessagePayloadSchema,
  "child.status": ChildStatusPayloadSchema,
};
function topicDocument(
  topic: string | undefined
): { readonly project: string; readonly slug: string } | undefined {
  if (topic?.startsWith(DISPATCH_DOCUMENT_TOPIC_PREFIX) !== true) return undefined;
  const [project, slug] = topic.slice(DISPATCH_DOCUMENT_TOPIC_PREFIX.length).split(".", 3);
  return project !== undefined && project !== "" && slug !== undefined && slug !== ""
    ? { project, slug }
    : undefined;
}

function dispatchOwner(event: DispatchEvent, topic: string | undefined): string {
  if (event.issue_key !== null) return event.issue_key;
  const document = topicDocument(topic);
  if (document !== undefined) return `${document.project} / ${document.slug}`;
  return event.artifact_id ?? "unknown";
}

type CommentPayload = z.infer<typeof CommentPayloadSchema>;

function dispatchCommentReplyWith(
  event: DispatchEvent,
  topic: string | undefined,
  comment: CommentPayload | undefined
): ReplyHint | undefined {
  if (comment === undefined || comment.id === undefined) return undefined;

  const reply = comment.ask_id;
  const thread =
    reply === undefined || reply === null || reply === "" ? "reply_to" : "reply_to_ask";
  const threadID = thread === "reply_to" ? comment.id : String(reply);
  if (event.issue_key !== null) {
    return {
      tool: "dispatch_comment",
      args: { issue: event.issue_key, [thread]: threadID, body: "..." },
    };
  }

  let project = comment.project_key;
  let artifact = comment.artifact_slug;
  if (project === undefined || project === "" || artifact === undefined || artifact === "") {
    [project, artifact] = dispatchOwner(event, topic).split(" / ", 2);
  }
  if (project === undefined || project === "" || artifact === undefined || artifact === "")
    return undefined;
  return {
    tool: "dispatch_comment",
    args: { project, artifact, [thread]: threadID, body: "..." },
  };
}

// An answered ask reads answer-first: the question head and the answer rendering sit
// directly under `dispatch:`, before the payload.
function dispatchAskAnswer(
  event: DispatchEvent
): { readonly question: string; readonly answer: string } | undefined {
  if (event.type !== "ask.answered") return undefined;
  const ask = AskPayloadSchema.safeParse(event.payload);
  if (!ask.success) return undefined;
  return {
    question: textHead(ask.data.question ?? ""),
    answer: askAnswerText(ask.data.answer),
  };
}

function dispatchPayload(event: DispatchEvent): unknown {
  const schema = DISPATCH_PAYLOAD_SCHEMAS[event.type];
  if (schema === undefined) return event.payload;
  const parsed = schema.safeParse(event.payload);
  return parsed.success ? parsed.data : event.payload;
}

// A comment.created reply to an ask carries the question text (ask_question) alongside
// the ask id, so the frame names the question head under `dispatch:` the way an answered
// ask does; `re:` stays the ask's ref.
function dispatchAskQuestion(comment: CommentPayload | undefined): string | undefined {
  return comment?.ask_question !== undefined && comment.ask_question !== ""
    ? textHead(comment.ask_question)
    : undefined;
}

// The thread a correlated frame continues, as a dispatch:// ref rather than the text it
// names: the ask an ask.* event or an ask reply (comment.created with ask_id) belongs to,
// or the message a message.* reply answers. A document-owned ask's ref needs the document
// slug, which only the comment payload or a document topic carries; a frame without either
// keeps the bare id Dispatch correlated it with, as does a frame of any other type.
function dispatchReplyRef(
  event: DispatchEvent,
  topic: string | undefined,
  inReplyTo: string,
  comment: CommentPayload | undefined
): string {
  let kind: "ask" | "message";
  let document = topicDocument(topic);
  if (event.type.startsWith("ask.")) {
    kind = "ask";
  } else if (event.type.startsWith("message.")) {
    kind = "message";
  } else if (event.type === "comment.created") {
    if (comment === undefined || comment.ask_id === undefined || comment.ask_id === null) {
      return inReplyTo;
    }
    kind = "ask";
    const { project_key: project, artifact_slug: slug } = comment;
    if (project !== undefined && project !== "" && slug !== undefined && slug !== "") {
      document = { project, slug };
    }
  } else {
    return inReplyTo;
  }
  if (event.issue_key !== null) {
    return dispatchChildRef(dispatchIssueRef(event.issue_key), kind, inReplyTo);
  }
  if (document === undefined) return inReplyTo;
  return dispatchChildRef(dispatchDocumentRef(document.project, document.slug), kind, inReplyTo);
}

// A Dispatch bus frame's JSON payload either matches the wire contract documented
// above (`event`) or it doesn't — invalid JSON, or a JSON value that disagrees with
// `DispatchEventSchema` — in which case the raw parsed value (object or string) is
// kept (`raw`) so no data is dropped and nothing renders as a hand-built text
// template.
type DispatchFrame =
  | { readonly event: DispatchEvent; readonly notify: unknown; readonly delivery: unknown }
  | {
      readonly raw: unknown;
      readonly rejectedDelivery?: DispatchDelivery;
      readonly malformedDelivery?: true;
    };

function isObject(value: unknown): value is Readonly<Record<string, unknown>> {
  return typeof value === "object" && value !== null;
}

function parseDispatchFrame(rawPayload: string): DispatchFrame {
  let value: unknown;
  try {
    value = JSON.parse(rawPayload);
  } catch {
    return { raw: rawPayload };
  }

  // A targeted frame requires `delivery`, so only a frame carrying the key can be one.
  if (isObject(value) && "delivery" in value) {
    const targeted = DispatchTargetedFrameSchema.safeParse(value);
    if (targeted.success) {
      const wireFrame = value as { readonly event: DispatchEvent & { readonly notify?: unknown } };
      return {
        event: targeted.data.event,
        notify: wireFrame.event.notify,
        delivery: targeted.data.delivery,
      };
    }
    const recoverable = RecoverableDispatchDeliveryFailureSchema.safeParse(value);
    if (recoverable.success) {
      const delivery = recoverable.data.delivery;
      const commentDelivery = isCommentTargetedDelivery(delivery);
      const resource = commentDelivery ? "comment" : "message";
      const id = commentDelivery ? delivery.comment_id : recoverable.data.event.payload.id;
      return {
        raw: value,
        rejectedDelivery: {
          resource,
          id,
          attempt: delivery.attempt,
          mode: delivery.mode,
          replyPath: dispatchReplyPath(resource, id),
          replyFields: commentDelivery ? { target: delivery.target } : {},
          issueKey: recoverable.data.event.issue_key,
          body: recoverable.data.event.payload.body ?? "",
        },
        malformedDelivery: true,
      };
    }
    return { raw: value, malformedDelivery: true };
  }

  const parsed = DispatchEventSchema.safeParse(value);
  if (!parsed.success) return { raw: value };
  const wireEvent = value as DispatchEvent & { readonly notify?: unknown };
  return {
    event: parsed.data,
    notify: wireEvent.notify,
    delivery: undefined,
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
  let dispatchEvent: unknown;
  let dispatchActor: DispatchEvent["actor"] | undefined;
  let dispatchIssue: string | undefined;
  let dispatchReply: ReplyHint | undefined;
  let delivery: DispatchDelivery | undefined;
  let rejectedDelivery: DispatchDelivery | undefined;
  let malformedDelivery = false;
  let inReplyTo = envelope.in_reply_to;
  // On an ask event the payload is the ask itself, whose author is another follower's
  // session when this session merely replied; naming it is not a spoof to flag.
  let askAuthor: string | undefined;
  const dispatchRendered = envelope.source === "dispatch" && envelope.payload !== undefined;
  if (envelope.source === "dispatch") {
    const topic = subject ?? envelope.topic;
    if (envelope.payload === undefined) {
      dispatchIssue = "payload";
    } else {
      const frame = parseDispatchFrame(envelope.payload);
      if ("event" in frame) {
        if (frame.notify === false && topic?.startsWith(DISPATCH_TOPIC_PREFIX) === true) {
          return { skip: true, content: "", envelope };
        }
        if (frame.event.actor.kind === "session" && frame.event.actor.id === sessionID) {
          return { skip: true, content: "", envelope };
        }
        // A meta-notice about Envoy plumbing, not a domain event: a short plain
        // sentence is higher signal here than the generic TOON envelope below.
        // This event also reaches the issue's own topic — every other subscriber,
        // not just the removed session — so it renders only for the session it
        // names; every other receiving session sees nothing (high signal).
        if (frame.event.type === "subscription.removed") {
          const removed = SubscriptionRemovedPayloadSchema.safeParse(frame.event.payload);
          if (!removed.success || removed.data.session_id !== sessionID) {
            return { skip: true, content: "", envelope };
          }
          const who = removed.data.by?.id ?? "someone";
          return {
            skip: false,
            content: `Unsubscribed from ${dispatchOwner(frame.event, topic)} by ${who}`,
            envelope,
          };
        }
        // Same shape for a human curating an ask's followers: only the session the
        // payload names hears it (the removed session on its own topic, an added one
        // wherever it listens); every other subscriber of the owner topic sees nothing.
        if (
          frame.event.type === "ask.follower_added" ||
          frame.event.type === "ask.follower_removed"
        ) {
          const follower = AskFollowerPayloadSchema.safeParse(frame.event.payload);
          if (!follower.success || follower.data.session_id !== sessionID) {
            return { skip: true, content: "", envelope };
          }
          const who = follower.data.by?.id ?? "someone";
          const owner = dispatchOwner(frame.event, topic);
          const ask = follower.data.ask_id ?? "?";
          return {
            skip: false,
            content:
              frame.event.type === "ask.follower_added"
                ? `Now following ask ${ask} on ${owner} (added by ${who}): its answer and replies reach you directly; dispatch_follow unfollow to stop.`
                : `No longer following ask ${ask} on ${owner} (removed by ${who}).`,
            envelope,
          };
        }
        const comment =
          frame.event.type === "comment.created"
            ? CommentPayloadSchema.safeParse(frame.event.payload)
            : undefined;
        const commentPayload = comment?.success === true ? comment.data : undefined;
        const answered = dispatchAskAnswer(frame.event);
        const question = answered?.question ?? dispatchAskQuestion(commentPayload);
        if (frame.event.type.startsWith("ask.")) {
          const asked = AskAuthorPayloadSchema.safeParse(frame.event.payload);
          if (asked.success && asked.data.author.kind === "session") {
            askAuthor = asked.data.author.id;
          }
        }
        if (inReplyTo !== undefined) {
          inReplyTo = dispatchReplyRef(frame.event, topic, inReplyTo, commentPayload);
        }
        dispatchReply = dispatchCommentReplyWith(frame.event, topic, commentPayload);
        if (frame.event.type === "message.created") {
          const message = DispatchTargetedMessagePayloadSchema.safeParse(frame.event.payload);
          const requested = DispatchTargetedDeliverySchema.safeParse(frame.delivery);
          if (message.success && requested.success && !isCommentTargetedDelivery(requested.data)) {
            delivery = {
              resource: "message",
              id: message.data.id,
              attempt: requested.data.attempt,
              mode: requested.data.mode,
              replyPath: dispatchReplyPath("message", message.data.id),
              replyFields: {},
              issueKey: frame.event.issue_key,
              body: message.data.body,
            };
            if (frame.event.issue_key !== null) {
              dispatchReply = {
                tool: "dispatch_message",
                args: { issue: frame.event.issue_key, in_reply_to: message.data.id, body: "..." },
              };
            }
          }
        } else if (frame.event.type === "comment.created") {
          const comment = DispatchTargetedCommentPayloadSchema.safeParse(frame.event.payload);
          const requested = DispatchTargetedDeliverySchema.safeParse(frame.delivery);
          if (comment.success && requested.success && isCommentTargetedDelivery(requested.data)) {
            delivery = {
              resource: "comment",
              id: requested.data.comment_id,
              attempt: requested.data.attempt,
              mode: requested.data.mode,
              replyPath: dispatchReplyPath("comment", requested.data.comment_id),
              replyFields: { target: requested.data.target },
              issueKey: frame.event.issue_key,
              body: comment.data.body,
            };
          }
        }
        dispatchEvent = {
          owner: dispatchOwner(frame.event, topic),
          ...(frame.event.issue_key === null
            ? {
                ...(topic?.startsWith(DISPATCH_DOCUMENT_TOPIC_PREFIX) === true
                  ? { document: dispatchOwner(frame.event, topic).replace(" / ", "/") }
                  : {}),
                ...(frame.event.artifact_id === undefined || frame.event.artifact_id === null
                  ? {}
                  : { artifact_id: frame.event.artifact_id }),
              }
            : { issue_key: frame.event.issue_key }),
          type: frame.event.type,
          actor: frame.event.actor,
          ...(question === undefined ? {} : { question }),
          ...(answered === undefined ? {} : { answer: answered.answer }),
          payload: dispatchPayload(frame.event),
        };
        dispatchActor = frame.event.actor;
      } else {
        dispatchEvent = frame.raw;
        rejectedDelivery = frame.rejectedDelivery;
        malformedDelivery = frame.malformedDelivery === true;
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
  // The summary is scanned before the payload, in index order, and the pattern spans no
  // newline, so this sees the same matches as scanning the two joined by "\n".
  let foreignSession: string | undefined;
  for (const part of [envelope.payload_summary ?? "", envelope.payload ?? ""]) {
    for (const match of part.matchAll(FOREIGN_SESSION_ID)) {
      if (
        match[0] !== envelope.source_session &&
        match[0] !== sessionID &&
        match[0] !== askAuthor
      ) {
        foreignSession = match[0];
        break;
      }
    }
    if (foreignSession !== undefined) break;
  }
  const role = envelope.sender?.roles?.[0];
  const reply = dispatchReply ?? replyWith(envelope);
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
    ...(inReplyTo === undefined ? {} : { re: inReplyTo }),
    ...(envelope.supersedes === undefined ? {} : { supersedes: envelope.supersedes }),
    ...(reply === undefined ? {} : { reply_with: reply }),
    ...(role === undefined
      ? {}
      : {
          reply_role: {
            tool: "envoy_publish",
            args: { topic: `notifications.role.${role}`, message: "..." },
          },
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

  return {
    skip: false,
    content: encode({ envoy: rendered }),
    envelope,
    ...(dispatchActor === undefined ? {} : { dispatchActor }),
    ...(delivery === undefined ? {} : { delivery }),
    ...(rejectedDelivery === undefined ? {} : { rejectedDelivery }),
    ...(malformedDelivery ? { malformedDelivery: true as const } : {}),
  };
}
