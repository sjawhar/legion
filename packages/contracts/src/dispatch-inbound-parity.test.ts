import { describe, expect, test } from "bun:test";
import { z } from "zod";
import {
  ASK_TURNS,
  AskEditedEventPayloadSchema,
  DELIVERY_CAPABILITIES,
  DispatchEventSchema,
  DispatchTargetedCommentPayloadSchema,
  DispatchTargetedDeliverySchema,
  DispatchTargetedMessagePayloadSchema,
  InboundArtifactCreatedEventPayloadSchema,
  InboundArtifactHumanEditedEventPayloadSchema,
  InboundArtifactReviewEventPayloadSchema,
  InboundArtifactVersionEventPayloadSchema,
  InboundAskEventPayloadSchema,
  InboundAskFollowerEventPayloadSchema,
  InboundChildStatusEventPayloadSchema,
  InboundCommentEventPayloadSchema,
  InboundIssueEventPayloadSchema,
  InboundMessageDeliveryEventPayloadSchema,
  InboundMessageEventPayloadSchema,
  InboundReplyBlockSchema,
  InboundSubscriptionRemovedEventPayloadSchema,
} from "./dispatch-api";

/**
 * What the inbound parsers accepted, refused and kept on the day the payload types were still
 * hand-written beside them, recorded frame by frame.
 *
 * The schemas below this comment are a copy of those parsers as they stood then, taken verbatim
 * from `dispatch-api.ts`. Every sample is parsed twice - once by the copy, once by the parser the
 * package exports today - and the two answers are compared: the same verdict, and, where both
 * accept, the same value. A bus frame is untrusted and retained for 72 hours, so a parser that
 * starts refusing a frame it used to keep, or that stops keeping a field, silently drops events
 * that are already on the wire; this test is what makes that a failure rather than a surprise.
 *
 * It is deliberately a copy and not an import: the point is to hold the old behaviour still while
 * the exported parsers are rewritten against the exact payload schemas.
 */

const OldDispatchEventSchema = z.object({
  issue_key: z.string().nullable(),
  artifact_id: z.string().nullish(),
  project: z.string().optional(),
  type: z.string(),
  actor: z.object({ kind: z.string(), id: z.string().optional() }).passthrough(),
  payload: z.object({}).passthrough(),
});

const OldIssueEventPayloadSchema = z.object({
  title: z.string().optional(),
  labels: z.array(z.string()).optional(),
  status: z.string().optional(),
  priority: z.number().int().min(0).max(3).nullable().optional(),
  rank: z.string().optional(),
  route: z.string().nullish(),
  changed: z.array(z.string()).optional(),
  ancestors: z.array(z.string()).optional(),
  thread_logins: z.array(z.string()).optional(),
});

const OldArtifactCreatedEventPayloadSchema = z.object({
  artifact: z
    .object({ id: z.string().optional(), slug: z.string().optional(), name: z.string().optional() })
    .optional(),
});

const OldArtifactVersionEventPayloadSchema = z.object({
  artifact_id: z.string().optional(),
  name: z.string().optional(),
  version: z.object({ number: z.number().optional(), summary: z.string().nullish() }).optional(),
  diff: z.string().optional(),
});

const OldArtifactHumanEditedEventPayloadSchema = z.object({
  artifact_id: z.string().optional(),
  name: z.string().optional(),
  from_version: z.number().int().optional(),
  to_version: z.number().int().optional(),
  editors: z.array(z.string()).optional(),
  diff: z.string().optional(),
  diff_truncated: z.boolean().optional(),
});

const OldArtifactReviewEventPayloadSchema = z.object({
  artifact_id: z.string().optional(),
  name: z.string().optional(),
  version: z.number().int().optional(),
  actor: z.object({ kind: z.string(), id: z.string() }).passthrough().optional(),
  reason: z.string().nullish(),
  ask_id: z.string().nullish(),
});

const oldAskEventPayloadFields = {
  id: z.string().optional(),
  opened_event_id: z.number().int().positive(),
  kind: z.string().optional(),
  question: z.string().optional(),
  options: z
    .array(z.object({ id: z.string().optional(), label: z.string().optional() }))
    .optional(),
  revision: z.number().int().positive().optional(),
  answer: z
    .object({
      id: z.string().optional(),
      selected: z.array(z.string()).nullish(),
      text: z.string().nullish(),
    })
    .nullish(),
  previous_answer: z
    .object({
      user: z.string().optional(),
      selected: z.array(z.string()).nullish(),
      text: z.string().nullish(),
      at: z.string().optional(),
    })
    .nullish(),
  restored: z.boolean().optional(),
  anchor: z
    .object({
      quote: z.string().optional(),
      mark_id: z.string().optional(),
      block_id: z.string().nullable().optional(),
    })
    .passthrough()
    .nullish(),
  anchor_artifact: z
    .object({
      project: z.string().optional(),
      slug: z.string().optional(),
      name: z.string().optional(),
      primary: z.boolean().optional(),
    })
    .optional(),
  resolution: z
    .object({
      kind: z.enum(["retracted", "resolved", "withdrawn"]).optional(),
      reason: z.string().optional(),
      actor: z
        .object({ kind: z.string().optional(), id: z.string().optional() })
        .passthrough()
        .optional(),
      at: z.string().optional(),
    })
    .nullish(),
  thread_logins: z.array(z.string()).optional(),
};

const OldAskEventPayloadSchema = z.intersection(
  z.object(oldAskEventPayloadFields),
  z.object({
    previous: z.never().optional(),
    edited_by: z.never().optional(),
  })
);

const OldAskEditedEventPayloadSchema = z.object({
  ...oldAskEventPayloadFields,
  multiple: z.boolean(),
  urgency: z.string(),
  edited_at: z.string().nullish(),
  previous: z.object({
    question: z.string(),
    options: z.array(z.object({ label: z.string().optional() })),
    multiple: z.boolean(),
    urgency: z.string(),
  }),
  edited_by: z.object({ kind: z.string(), id: z.string() }).passthrough(),
});

const OldCommentMentionSchema = z.object({
  target: z.string(),
  delivery: z.enum(DELIVERY_CAPABILITIES),
  session_id: z.string().nullable(),
});

const OldCommentDeliverySchema = z.object({
  comment_id: z.string(),
  target: z.string(),
  attempt: z.number().int().positive(),
  delivery: z.enum(DELIVERY_CAPABILITIES),
  session_id: z.string().nullable(),
  envelope_id: z.string().nullable(),
  duplicate: z.boolean().optional(),
  state: z.enum(["pending", "sent", "failed"]),
  error: z.string().nullable(),
  resolve_error: z.string().nullable(),
  reply_id: z.string().nullable(),
  created_at: z.string(),
});

const OldReplyBlockSchema = z.object({
  node: z.string(),
  ref: z.string().optional(),
  block_id: z.string(),
  text: z.string(),
  removed: z.boolean(),
});

const OldCommentEventPayloadSchema = z
  .object({
    id: z.string().optional(),
    artifact_name: z.string().optional(),
    project_key: z.string().optional(),
    artifact_slug: z.string().optional(),
    body: z.string().optional(),
    reply_to: z.string().nullish(),
    ask_id: z.string().nullish(),
    ask_question: z.string().optional(),
    ask_state: z.enum(["open", "answered", "resolved"]).optional(),
    ask_waiting_on: z.enum(ASK_TURNS).optional(),
    turn: z.enum(ASK_TURNS).nullish(),
    anchor: z
      .object({
        artifact_id: z.string().optional(),
        block_id: z.string().nullable().optional(),
        mark_id: z.string().optional(),
        orphaned: z.boolean().optional(),
        quote: z.string().optional(),
        version: z.number().int().optional(),
      })
      .nullish(),
    suggestion: z.object({ replace_with: z.string().optional() }).nullish(),
    author: z.object({ kind: z.string(), id: z.string() }).optional(),
    created_at: z.string().optional(),
    mentions: z.array(OldCommentMentionSchema).optional(),
    deliveries: z.array(OldCommentDeliverySchema).optional(),
    reply_block: OldReplyBlockSchema.nullish(),
    thread_logins: z.array(z.string()).optional(),
  })
  .passthrough();

const OldMessageEventPayloadSchema = z.object({
  id: z.string().optional(),
  body: z.string().optional(),
  target: z.string().nullish(),
  in_reply_to: z.string().nullish(),
  reply_body: z.string().optional(),
  thread_target: z.string().optional(),
  author: z.object({ kind: z.string(), id: z.string() }).optional(),
  reply_block: OldReplyBlockSchema.nullish(),
  thread_logins: z.array(z.string()).optional(),
});

const OldDispatchTargetedResourceIDSchema = z.uuid();

const OldDispatchTargetedMessagePayloadSchema = OldMessageEventPayloadSchema.extend({
  id: OldDispatchTargetedResourceIDSchema,
  issue_key: z.string().nullable(),
  author: z.object({ kind: z.string(), id: z.string() }),
  body: z.string(),
  target: z.string(),
  in_reply_to: z.string().nullable(),
  broadcast_id: z.string().nullish(),
  deliveries: z.array(z.unknown()),
  created_at: z.string(),
});

const OldDispatchTargetedCommentPayloadSchema = OldCommentEventPayloadSchema.extend({
  id: OldDispatchTargetedResourceIDSchema,
  issue_key: z.string().nullable(),
  artifact_id: z.string().nullable(),
  author: z.object({ kind: z.string(), id: z.string() }),
  body: z.string(),
  reply_to: z.string().nullable(),
  ask_id: z.string().nullable(),
  mentions: z.array(OldCommentMentionSchema),
  deliveries: z.array(OldCommentDeliverySchema),
  created_at: z.string(),
  artifact_name: z.string(),
});

const OldDispatchTargetedMessageDeliverySchema = z.object({
  attempt: z.number().int().positive(),
  mode: z.enum(DELIVERY_CAPABILITIES),
});

const OldDispatchTargetedCommentDeliverySchema = OldDispatchTargetedMessageDeliverySchema.extend({
  comment_id: OldDispatchTargetedResourceIDSchema,
  target: z.string(),
});

const OldDispatchTargetedDeliverySchema = z.union([
  OldDispatchTargetedCommentDeliverySchema,
  OldDispatchTargetedMessageDeliverySchema,
]);

const OldMessageDeliveryEventPayloadSchema = z.object({
  message_id: z.string().optional(),
  attempt: z.number().int().positive().optional(),
  delivery: z.enum(DELIVERY_CAPABILITIES).optional(),
  session_id: z.string().optional(),
  target: z.string().optional(),
  title: z.string().optional(),
  state: z.enum(["sent", "failed"]).optional(),
  duplicate: z.boolean().optional(),
  error: z.string().optional(),
});

const OldChildStatusEventPayloadSchema = z.object({
  child_key: z.string().optional(),
  from: z.string().optional(),
  to: z.string().optional(),
});

const OldSubscriptionRemovedEventPayloadSchema = z.object({
  session_id: z.string().optional(),
  by: z.object({ kind: z.string(), id: z.string().optional() }).passthrough().optional(),
  topics: z.array(z.string()).optional(),
  pending: z.boolean().optional(),
  request_event_id: z.number().int().positive().optional(),
});

const OldAskFollowerEventPayloadSchema = z.object({
  ask_id: z.string().optional(),
  session_id: z.string().optional(),
  by: z.object({ kind: z.string(), id: z.string().optional() }).passthrough().optional(),
});

/** The parsers as they stood when the payload types were hand-written beside them. */
const OLD = {
  artifactCreated: OldArtifactCreatedEventPayloadSchema,
  artifactHumanEdited: OldArtifactHumanEditedEventPayloadSchema,
  artifactReview: OldArtifactReviewEventPayloadSchema,
  artifactVersion: OldArtifactVersionEventPayloadSchema,
  ask: OldAskEventPayloadSchema,
  askEdited: OldAskEditedEventPayloadSchema,
  askFollower: OldAskFollowerEventPayloadSchema,
  childStatus: OldChildStatusEventPayloadSchema,
  comment: OldCommentEventPayloadSchema,
  event: OldDispatchEventSchema,
  issue: OldIssueEventPayloadSchema,
  message: OldMessageEventPayloadSchema,
  messageDelivery: OldMessageDeliveryEventPayloadSchema,
  replyBlock: OldReplyBlockSchema,
  subscriptionRemoved: OldSubscriptionRemovedEventPayloadSchema,
  targetedComment: OldDispatchTargetedCommentPayloadSchema,
  targetedDelivery: OldDispatchTargetedDeliverySchema,
  targetedMessage: OldDispatchTargetedMessagePayloadSchema,
} as const;

/** The readings the package exports today, each beside the exact schema it is derived from. */
const NEW: Readonly<Record<keyof typeof OLD, z.ZodType>> = {
  artifactCreated: InboundArtifactCreatedEventPayloadSchema,
  artifactHumanEdited: InboundArtifactHumanEditedEventPayloadSchema,
  artifactReview: InboundArtifactReviewEventPayloadSchema,
  artifactVersion: InboundArtifactVersionEventPayloadSchema,
  ask: InboundAskEventPayloadSchema,
  askEdited: AskEditedEventPayloadSchema,
  askFollower: InboundAskFollowerEventPayloadSchema,
  childStatus: InboundChildStatusEventPayloadSchema,
  comment: InboundCommentEventPayloadSchema,
  event: DispatchEventSchema,
  issue: InboundIssueEventPayloadSchema,
  message: InboundMessageEventPayloadSchema,
  messageDelivery: InboundMessageDeliveryEventPayloadSchema,
  replyBlock: InboundReplyBlockSchema,
  subscriptionRemoved: InboundSubscriptionRemovedEventPayloadSchema,
  targetedComment: DispatchTargetedCommentPayloadSchema,
  targetedDelivery: DispatchTargetedDeliverySchema,
  targetedMessage: DispatchTargetedMessagePayloadSchema,
};

type ParserName = keyof typeof OLD;

interface Sample {
  readonly parser: ParserName;
  readonly name: string;
  readonly frame: unknown;
}

type Frame = Record<string, unknown>;

const targetedDeliveryFixture = JSON.parse(
  await Bun.file(new URL("../fixtures/dispatch-targeted-delivery.json", import.meta.url)).text()
) as { readonly event: Frame; readonly delivery: Frame };

/** The actor every recorded frame below was written by. */
const USER = { kind: "user", id: "alice" };
const SESSION = { kind: "session", id: "session-1" };

/**
 * The frames the package's own tests read today: the targeted-delivery fixture and the literals
 * of `dispatch-api.test.ts`, each under the parser that test feeds it to.
 */
const RECORDED: readonly Sample[] = [
  { parser: "event", name: "the targeted-delivery fixture", frame: targetedDeliveryFixture.event },
  {
    parser: "targetedMessage",
    name: "the targeted-delivery fixture's payload",
    frame: targetedDeliveryFixture.event.payload,
  },
  {
    parser: "targetedDelivery",
    name: "the targeted-delivery fixture's delivery",
    frame: targetedDeliveryFixture.delivery,
  },
  {
    parser: "event",
    name: "a typed artifact version event",
    frame: {
      actor: SESSION,
      created_at: "2026-09-09T00:00:00Z",
      id: 42,
      issue_key: "DSP-1",
      notify: true,
      payload: {
        artifact_id: "artifact-1",
        name: "spec.md",
        version: { number: 2, summary: null },
      },
      seq: 7,
      type: "artifact.version",
    },
  },
  {
    parser: "event",
    name: "a document event with an unlinked owner",
    frame: {
      id: 12,
      issue_key: null,
      artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      project: "CORE",
      seq: 4,
      type: "artifact.version",
      actor: SESSION,
      notify: true,
      created_at: "2026-09-11T04:05:29Z",
      payload: {},
    },
  },
  {
    parser: "event",
    name: "an artifact-owned ask edit event",
    frame: {
      id: 72,
      issue_key: null,
      artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
      project: "CORE",
      seq: 4,
      type: "ask.edited",
      actor: SESSION,
      notify: false,
      created_at: "2026-09-11T04:05:29Z",
      payload: {
        id: "ask-1",
        issue_key: null,
        artifact_id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
        author: SESSION,
        kind: "question",
        question: "The document is ready for readers. How should we publish it?",
        options: [],
        multiple: false,
        urgency: "med",
        anchor: null,
        state: "open",
        answer: null,
        opened_event_id: 3,
        created_at: "2026-09-11T04:00:00Z",
        edited_at: "2026-09-11T04:05:29Z",
        previous: {
          question: "The document needs a publication decision. How should we proceed?",
          options: [{ label: "Publish for readers" }],
          multiple: false,
          urgency: "med",
        },
        edited_by: SESSION,
      },
    },
  },
  {
    parser: "event",
    name: "an invalid ask block event",
    frame: {
      id: 73,
      issue_key: "DOC-1",
      seq: 5,
      type: "block.invalid",
      actor: SESSION,
      notify: false,
      created_at: "2026-09-13T16:30:00Z",
      payload: {
        block_id: "ask-block",
        version: 3,
        reason: 'ask block "ask-block" has an option without a label',
        disturbed_by: SESSION,
      },
    },
  },
  {
    parser: "event",
    name: "a read mark's user_thread_state event",
    frame: {
      id: 1,
      issue_key: null,
      project: "CORE",
      seq: 3,
      actor: USER,
      notify: false,
      created_at: "2026-10-12T00:02:00Z",
      type: "user_thread_state.updated",
      payload: { login: "alice", thread_id: "comment-1" },
    },
  },
  {
    parser: "event",
    name: "a snooze's user_state event",
    frame: {
      id: 2,
      issue_key: null,
      project: "CORE",
      seq: 4,
      actor: USER,
      notify: false,
      created_at: "2026-10-12T00:03:00Z",
      type: "user_state.updated",
      payload: { login: "alice", changed: ["snoozed"] },
    },
  },
  { parser: "issue", name: "labels alone", frame: { labels: ["frontend", "urgent"] } },
  { parser: "issue", name: "the highest priority", frame: { priority: 0 } },
  { parser: "issue", name: "no priority", frame: { priority: null } },
  {
    parser: "issue",
    name: "what a write changed, its ancestors and its thread logins",
    frame: {
      status: "done",
      changed: ["status"],
      ancestors: ["CORE-2", "CORE-1"],
      thread_logins: ["alice@example.com"],
    },
  },
  {
    parser: "issue",
    name: "an event older than the change fields",
    frame: { title: "Older event" },
  },
  {
    parser: "comment",
    name: "thread logins",
    frame: { id: "comment-1", thread_logins: ["alice@example.com", "bob@example.com"] },
  },
  {
    parser: "message",
    name: "thread logins",
    frame: { id: "message-1", thread_logins: ["alice@example.com", "bob@example.com"] },
  },
  {
    parser: "ask",
    name: "thread logins",
    frame: { opened_event_id: 3, thread_logins: ["alice@example.com", "bob@example.com"] },
  },
  { parser: "message", name: "a message with no thread logins", frame: { id: "message-2" } },
  { parser: "ask", name: "no opened event id", frame: { question: "Q" } },
  {
    parser: "ask",
    name: "an opened event id of zero",
    frame: { opened_event_id: 0, question: "Q" },
  },
  {
    parser: "ask",
    name: "a positive opened event id",
    frame: { opened_event_id: 12, question: "Q" },
  },
  {
    parser: "ask",
    name: "a replaced answer",
    frame: {
      opened_event_id: 12,
      question: "Ship it?",
      answer: { selected: ["Ship"], text: null },
      previous_answer: {
        user: "alice@example.com",
        selected: ["Hold"],
        text: "Wait for review.",
        at: "2026-10-08T00:00:00Z",
      },
    },
  },
  {
    parser: "ask",
    name: "an ask the close of its issue withdrew",
    frame: {
      opened_event_id: 12,
      question: "Ship it?",
      resolution: {
        kind: "withdrawn",
        reason: "Withdrawn by the close of DSP-1.",
        actor: { kind: "session", id: "legion-daemon:DSP" },
        at: "2026-10-25T00:00:00Z",
      },
    },
  },
  {
    parser: "ask",
    name: "a block-pinned anchor",
    frame: {
      opened_event_id: 12,
      anchor: { block_id: "block-1", mark_id: "mark-1", quote: "Selected text" },
    },
  },
  {
    parser: "ask",
    name: "a legacy anchor",
    frame: { opened_event_id: 13, anchor: { mark_id: "mark-2", quote: "Older text" } },
  },
  {
    parser: "ask",
    name: "an anchor document",
    frame: {
      opened_event_id: 14,
      anchor_artifact: { name: "Spec", primary: true, project: "CORE", slug: "spec" },
    },
  },
  {
    parser: "ask",
    name: "edit history on a non-edit ask event",
    frame: {
      opened_event_id: 7,
      question: "Which API?",
      previous: {
        question: "Which transport?",
        options: [{ label: "HTTP" }],
        multiple: false,
        urgency: "med",
      },
    },
  },
  {
    parser: "askEdited",
    name: "an ask edit's history",
    frame: {
      opened_event_id: 7,
      question: "Which transport should we implement?",
      options: [{ label: "REST" }, { label: "gRPC" }],
      multiple: true,
      urgency: "high",
      answer: null,
      edited_at: "2026-09-11T03:26:00Z",
      previous: {
        question: "Which implementation?",
        options: [{ label: "HTTP" }, { label: "MCP" }],
        multiple: false,
        urgency: "low",
      },
      edited_by: SESSION,
    },
  },
  {
    parser: "askEdited",
    name: "an ask edit missing its previous wording",
    frame: {
      opened_event_id: 7,
      question: "Which API?",
      options: [],
      multiple: false,
      urgency: "med",
      edited_at: "2026-09-11T03:26:00Z",
      edited_by: SESSION,
    },
  },
  {
    parser: "comment",
    name: "a document named in references_changed",
    frame: {
      id: "comment-1",
      references_changed: [
        { kind: "issue", id: "CORE-1" },
        {
          kind: "artifact",
          id: "a4cf7999-cab2-4326-939d-cb1e76733cc3",
          ref: "dispatch://CORE/artifact/runbook",
        },
      ],
    },
  },
  {
    parser: "comment",
    name: "a truncated write naming the kinds it cut",
    frame: {
      id: "comment-1",
      references_changed: [{ kind: "issue", id: "CORE-1" }],
      references_changed_truncated: true,
      references_changed_truncated_kinds: ["artifact"],
    },
  },
  {
    parser: "comment",
    name: "a hydrated mention delivery",
    frame: {
      id: "comment-mention-1",
      issue_key: "DSP-1",
      artifact_id: null,
      author: USER,
      body: "Please review this.",
      anchor: null,
      reply_to: null,
      ask_id: null,
      turn: null,
      resolved: false,
      resolved_by: null,
      resolved_at: null,
      edited_at: null,
      suggestion: null,
      created_at: "2026-09-18T00:00:00Z",
      mentions: [{ target: "session:session-1", delivery: "aside", session_id: "session-1" }],
      deliveries: [
        {
          comment_id: "comment-mention-1",
          target: "session:session-1",
          attempt: 1,
          delivery: "aside",
          session_id: "session-1",
          envelope_id: "envelope-1",
          state: "sent",
          error: null,
          resolve_error: null,
          reply_id: null,
          created_at: "2026-09-18T00:00:01Z",
        },
      ],
      artifact_name: "spec.md",
    },
  },
  {
    parser: "comment",
    name: "a whole comment row",
    frame: {
      anchor: null,
      author: USER,
      body: "Please update this.",
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "comment-1",
      issue_key: "DSP-1",
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      ask_id: null,
      turn: null,
      mentions: [],
      deliveries: [],
      artifact_name: "spec.md",
    },
  },
  {
    parser: "comment",
    name: "a reply naming the block it answers",
    frame: {
      anchor: null,
      ask_id: null,
      author: USER,
      body: "On the second paragraph.",
      created_at: "2026-09-10T00:00:01Z",
      deliveries: [],
      edited_at: null,
      id: "comment-2",
      issue_key: "DSP-1",
      mentions: [],
      reply_block: {
        block_id: "b2",
        node: "comment/comment-1#b2",
        ref: "dispatch://DSP-1/comment/comment-1#b-b2",
        removed: false,
        text: "Beta.",
      },
      reply_to: "comment-1",
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: null,
    },
  },
  {
    parser: "comment",
    name: "a comment recorded before replies named blocks",
    frame: { id: "comment-3", author: USER, body: "Older.", reply_to: "comment-1" },
  },
  {
    parser: "message",
    name: "a message with its author and body",
    frame: {
      id: "message-1",
      issue_key: "DSP-1",
      author: USER,
      body: "The build is green.",
      created_at: "2026-09-10T00:00:00Z",
    },
  },
  {
    parser: "message",
    name: "a reply's in_reply_to and preview",
    frame: {
      id: "message-2",
      issue_key: "DSP-1",
      author: USER,
      body: "Sounds good.",
      in_reply_to: "message-1",
      reply_body: "The build is green.",
      created_at: "2026-09-10T00:00:01Z",
    },
  },
  {
    parser: "message",
    name: "a reply's thread target",
    frame: {
      id: "message-3",
      issue_key: "DSP-1",
      author: USER,
      body: "Noted.",
      in_reply_to: "message-2",
      thread_target: "session:s1",
      created_at: "2026-09-10T00:00:02Z",
    },
  },
  {
    parser: "message",
    name: "a root message",
    frame: { id: "message-1", author: USER, body: "Root message." },
  },
  {
    parser: "message",
    name: "a direct message's block",
    frame: {
      author: USER,
      body: "On two.",
      created_at: "2026-09-10T00:00:01Z",
      deliveries: [],
      id: "message-2",
      in_reply_to: "message-1",
      issue_key: null,
      reply_block: { block_id: "b2", node: "message/message-1#b2", removed: true, text: "Two." },
      target: null,
    },
  },
  {
    parser: "message",
    name: "a null reply block",
    frame: {
      author: USER,
      body: "On two.",
      created_at: "2026-09-10T00:00:01Z",
      deliveries: [],
      id: "message-2",
      in_reply_to: "message-1",
      issue_key: null,
      reply_block: null,
      target: null,
    },
  },
  {
    parser: "message",
    name: "a reply recorded before replies named blocks",
    frame: { id: "message-3", author: USER, in_reply_to: "message-1" },
  },
  {
    parser: "replyBlock",
    name: "a comment block with its address",
    frame: {
      block_id: "b2",
      node: "comment/comment-1#b2",
      ref: "dispatch://DSP-1/comment/comment-1#b-b2",
      removed: false,
      text: "Beta.",
    },
  },
  {
    parser: "replyBlock",
    name: "a direct message's block, which has no address",
    frame: { block_id: "b2", node: "message/message-1#b2", removed: true, text: "Two." },
  },
  {
    parser: "replyBlock",
    name: "a block without its text or removal",
    frame: { block_id: "b2", node: "comment/c#b2" },
  },
  ...DELIVERY_CAPABILITIES.map(
    (mode): Sample => ({
      parser: "messageDelivery",
      name: `the ${mode} capability`,
      frame: { delivery: mode },
    })
  ),
  { parser: "messageDelivery", name: "a mode that is no capability", frame: { delivery: "shout" } },
  {
    parser: "targetedMessage",
    name: "a complete targeted message",
    frame: {
      id: "11111111-1111-4111-8111-111111111111",
      issue_key: null,
      author: USER,
      body: "Can this ship?",
      target: "session:ses_target",
      in_reply_to: null,
      deliveries: [],
      created_at: "2026-09-12T00:00:00Z",
    },
  },
  {
    parser: "targetedMessage",
    name: "a targeted message carrying a field from a later release",
    frame: {
      id: "11111111-1111-4111-8111-111111111111",
      issue_key: null,
      author: USER,
      body: "Can this ship?",
      target: "session:ses_target",
      in_reply_to: null,
      deliveries: [],
      created_at: "2026-09-12T00:00:00Z",
      future_field: true,
    },
  },
  {
    parser: "targetedComment",
    name: "a complete targeted comment",
    frame: {
      id: "22222222-2222-4222-8222-222222222222",
      issue_key: null,
      artifact_id: "artifact-1",
      author: USER,
      body: "Please review this section.",
      reply_to: null,
      ask_id: null,
      deliveries: [],
      mentions: [],
      created_at: "2026-09-12T00:00:00Z",
      artifact_name: "spec.md",
      project_key: "CORE",
      artifact_slug: "spec",
    },
  },
];

/**
 * One whole frame per parser: every field the parser declares, filled with a value it accepts.
 * The generated samples below are read off these, so a field added to a parser without a value
 * here is a field no generated sample covers.
 */
const WHOLE: Readonly<Record<ParserName, Frame>> = {
  artifactCreated: { artifact: { id: "artifact-1", slug: "spec", name: "spec.md" } },
  artifactHumanEdited: {
    artifact_id: "artifact-1",
    name: "spec.md",
    from_version: 3,
    to_version: 5,
    editors: ["alice@example.com"],
    diff: "@@ -1 +1 @@\n-a\n+b\n",
    diff_truncated: false,
  },
  artifactReview: {
    artifact_id: "artifact-1",
    name: "spec.md",
    version: 4,
    actor: USER,
    reason: "Looks right.",
    ask_id: "ask-1",
  },
  artifactVersion: {
    artifact_id: "artifact-1",
    name: "spec.md",
    version: { number: 2, summary: "Second draft" },
    diff: "@@ -1 +1 @@\n-a\n+b\n",
  },
  ask: {
    id: "ask-1",
    opened_event_id: 7,
    kind: "question",
    question: "Ship it?",
    options: [{ id: "b1", label: "Ship" }],
    revision: 2,
    answer: { id: "b3", selected: ["Ship"], text: "Yes." },
    previous_answer: {
      user: "alice@example.com",
      selected: ["Hold"],
      text: "Wait.",
      at: "2026-10-08T00:00:00Z",
    },
    restored: false,
    anchor: { quote: "Selected text", mark_id: "mark-1", block_id: "block-1" },
    anchor_artifact: { project: "CORE", slug: "spec", name: "Spec", primary: true },
    resolution: {
      kind: "resolved",
      reason: "Shipped.",
      actor: USER,
      at: "2026-10-25T00:00:00Z",
    },
    thread_logins: ["alice@example.com"],
  },
  askEdited: {
    id: "ask-1",
    opened_event_id: 7,
    kind: "question",
    question: "Which transport?",
    options: [{ id: "b1", label: "REST" }],
    revision: 2,
    answer: null,
    previous_answer: null,
    restored: false,
    anchor: null,
    anchor_artifact: { project: "CORE", slug: "spec", name: "Spec", primary: true },
    resolution: null,
    thread_logins: ["alice@example.com"],
    multiple: false,
    urgency: "med",
    edited_at: "2026-09-11T03:26:00Z",
    previous: {
      question: "Which implementation?",
      options: [{ label: "HTTP" }],
      multiple: false,
      urgency: "low",
    },
    edited_by: SESSION,
  },
  askFollower: { ask_id: "ask-1", session_id: "session-1", by: USER },
  childStatus: { child_key: "CORE-2", from: "triage", to: "doing" },
  comment: {
    id: "comment-1",
    artifact_name: "spec.md",
    project_key: "CORE",
    artifact_slug: "spec",
    body: "Please review this.",
    reply_to: "comment-0",
    ask_id: "ask-1",
    ask_question: "Ship it?",
    ask_state: "open",
    ask_waiting_on: "human",
    turn: "agent",
    anchor: {
      artifact_id: "artifact-1",
      block_id: "block-1",
      mark_id: "mark-1",
      orphaned: false,
      quote: "Selected text",
      version: 3,
    },
    suggestion: { replace_with: "Replacement" },
    author: USER,
    created_at: "2026-09-18T00:00:00Z",
    mentions: [{ target: "session:session-1", delivery: "aside", session_id: "session-1" }],
    deliveries: [
      {
        comment_id: "comment-1",
        target: "session:session-1",
        attempt: 1,
        delivery: "aside",
        session_id: "session-1",
        envelope_id: "envelope-1",
        duplicate: false,
        state: "sent",
        error: null,
        resolve_error: null,
        reply_id: null,
        created_at: "2026-09-18T00:00:01Z",
      },
    ],
    reply_block: {
      node: "comment/comment-0#b2",
      ref: "dispatch://DSP-1/comment/comment-0#b-b2",
      block_id: "b2",
      text: "Beta.",
      removed: false,
    },
    thread_logins: ["alice@example.com"],
  },
  event: {
    issue_key: "CORE-1",
    artifact_id: null,
    project: "CORE",
    type: "comment.created",
    actor: USER,
    payload: { id: "comment-1" },
  },
  issue: {
    title: "Release",
    labels: ["frontend"],
    status: "doing",
    priority: 1,
    rank: "0|hzzzzz:",
    route: "role:reviewer",
    changed: ["status"],
    ancestors: ["CORE-1"],
    thread_logins: ["alice@example.com"],
  },
  message: {
    id: "message-1",
    body: "The build is green.",
    target: "session:ses_target",
    in_reply_to: "message-0",
    reply_body: "Is it green?",
    thread_target: "session:ses_target",
    author: USER,
    reply_block: {
      node: "message/message-0#b2",
      ref: "dispatch://DSP-1/message/message-0#b-b2",
      block_id: "b2",
      text: "Two.",
      removed: false,
    },
    thread_logins: ["alice@example.com"],
  },
  messageDelivery: {
    message_id: "message-1",
    attempt: 1,
    delivery: "btw",
    session_id: "session-1",
    target: "session:session-1",
    title: "Planner",
    state: "sent",
    duplicate: false,
    error: "",
  },
  replyBlock: {
    node: "comment/comment-1#b2",
    ref: "dispatch://DSP-1/comment/comment-1#b-b2",
    block_id: "b2",
    text: "Beta.",
    removed: false,
  },
  subscriptionRemoved: {
    session_id: "session-1",
    by: USER,
    topics: ["notifications.dispatch.issue.CORE-1.>"],
    pending: true,
    request_event_id: 12,
  },
  targetedComment: {
    id: "22222222-2222-4222-8222-222222222222",
    issue_key: "CORE-1",
    artifact_id: "artifact-1",
    author: USER,
    body: "Please review this section.",
    reply_to: null,
    ask_id: null,
    mentions: [],
    deliveries: [],
    created_at: "2026-09-12T00:00:00Z",
    artifact_name: "spec.md",
  },
  targetedDelivery: { attempt: 1, mode: "btw" },
  targetedMessage: {
    id: "11111111-1111-4111-8111-111111111111",
    issue_key: "CORE-1",
    author: USER,
    body: "Can this ship?",
    target: "session:ses_target",
    in_reply_to: null,
    broadcast_id: null,
    deliveries: [],
    created_at: "2026-09-12T00:00:00Z",
  },
};

/** A frame like `frame` without `key`. */
function without(frame: Frame, key: string): Frame {
  const { [key]: _dropped, ...rest } = frame;
  return rest;
}

/**
 * Every generated sample: for each field of each parser, the frame without it and the frame
 * carrying a value of the wrong shape in it (an object where a scalar belongs, and a boolean,
 * which is the wrong shape for every field no parser declares as a boolean); for each object a
 * frame carrying a field no parser declares; and, for the fields the parsers deliberately read
 * more loosely than the payload's own type, a frame whose value the parser takes and the exact
 * type refuses.
 */
function generated(): Sample[] {
  const samples: Sample[] = [];
  for (const parser of Object.keys(WHOLE) as ParserName[]) {
    const whole = WHOLE[parser];
    samples.push({ parser, name: "every field it declares", frame: whole });
    samples.push({
      parser,
      name: "a field no parser declares",
      frame: { ...whole, a_field_from_a_later_release: "kept or dropped, never a refusal" },
    });
    for (const key of Object.keys(whole)) {
      samples.push({ parser, name: `without ${key}`, frame: without(whole, key) });
      samples.push({
        parser,
        name: `${key} as an object`,
        frame: { ...whole, [key]: { malformed: true } },
      });
      samples.push({ parser, name: `${key} as a boolean`, frame: { ...whole, [key]: false } });
      const value = whole[key];
      if (value !== null && typeof value === "object" && !Array.isArray(value)) {
        samples.push({
          parser,
          name: `${key} carrying a field no parser declares`,
          frame: { ...whole, [key]: { ...value, a_field_from_a_later_release: 1 } },
        });
      }
    }
  }
  return samples;
}

/**
 * Frames whose value the parsers take and the payload's exact type refuses: an ask kind or an
 * urgency the server no longer writes (JetStream keeps 72 hours of frames, so a renderer still
 * sees them), an actor kind that is none of the three, and an actor with no id. Each is the
 * reason its field stays an open string rather than an enum.
 */
const LOOSER: readonly Sample[] = [
  {
    parser: "ask",
    name: "an ask kind the server no longer writes",
    frame: { opened_event_id: 7, kind: "action" },
  },
  {
    parser: "askEdited",
    name: "an urgency that is no urgency",
    frame: {
      opened_event_id: 7,
      multiple: false,
      urgency: "whenever",
      previous: { question: "Q", options: [], multiple: false, urgency: "whenever" },
      edited_by: SESSION,
    },
  },
  {
    parser: "askEdited",
    name: "an editor whose kind is none of the three",
    frame: {
      opened_event_id: 7,
      multiple: false,
      urgency: "med",
      previous: { question: "Q", options: [], multiple: false, urgency: "med" },
      edited_by: { kind: "robot", id: "r1" },
    },
  },
  {
    parser: "comment",
    name: "an author whose kind is none of the three",
    frame: { id: "comment-1", author: { kind: "robot", id: "r1" } },
  },
  {
    parser: "message",
    name: "an author whose kind is none of the three",
    frame: { id: "message-1", author: { kind: "robot", id: "r1" } },
  },
  {
    parser: "artifactReview",
    name: "a reviewer whose kind is none of the three",
    frame: { artifact_id: "artifact-1", actor: { kind: "robot", id: "r1" } },
  },
  {
    parser: "event",
    name: "an actor with no id",
    frame: { issue_key: null, type: "comment.created", actor: { kind: "user" }, payload: {} },
  },
  {
    parser: "event",
    name: "an actor whose kind is none of the three",
    frame: {
      issue_key: null,
      type: "comment.created",
      actor: { kind: "robot", id: "r1" },
      payload: {},
    },
  },
  {
    parser: "subscriptionRemoved",
    name: "a remover with no id",
    frame: { session_id: "session-1", by: { kind: "user" } },
  },
  {
    parser: "askFollower",
    name: "a follower writer with no id",
    frame: { ask_id: "ask-1", session_id: "session-1", by: { kind: "user" } },
  },
];

const SAMPLES: readonly Sample[] = [...RECORDED, ...generated(), ...LOOSER];

/** How many samples this file holds; stated so a sample lost to an edit is a failure. */
const SAMPLE_COUNT = 538;

/** Where the two parsers disagree on `sample`, in words; empty when they agree. */
function disagreement(sample: Sample): string | null {
  const old = OLD[sample.parser].safeParse(sample.frame);
  const now = NEW[sample.parser].safeParse(sample.frame);
  if (old.success !== now.success) {
    return `${sample.parser}: ${sample.name}: the recorded parser ${
      old.success ? "accepted" : "refused"
    } it and today's ${now.success ? "accepts" : "refuses"} it`;
  }
  if (!old.success || !now.success) return null;
  try {
    expect(now.data).toEqual(old.data);
  } catch {
    return `${sample.parser}: ${sample.name}: kept ${JSON.stringify(
      now.data
    )} where the recorded parser kept ${JSON.stringify(old.data)}`;
  }
  return null;
}

describe("the inbound parsers read every recorded frame as they did", () => {
  test(`holds ${SAMPLE_COUNT} samples`, () => {
    expect(SAMPLES.length).toBe(SAMPLE_COUNT);
  });

  for (const parser of Object.keys(OLD).sort() as ParserName[]) {
    const samples = SAMPLES.filter((sample) => sample.parser === parser);
    test(`${parser}, on ${samples.length} frames`, () => {
      expect(samples.length).toBeGreaterThan(0);
      expect(samples.map(disagreement).filter((found) => found !== null)).toEqual([]);
    });
  }
});
