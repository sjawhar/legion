import { z } from "zod";
import { askFields } from "./dispatch-api-asks";
import {
  ActorSchema,
  AnchorSchema,
  anchorFields,
  anchorPositionFields,
  InboundActorSchema,
} from "./dispatch-api-core";
import type { GraphNode } from "./dispatch-api-graph";
import { ASK_TURNS, type AskTurn, DELIVERY_CAPABILITIES } from "./dispatch-api-limits";
import {
  InboundReplyBlockSchema,
  ReplyBlockSchema,
  referenceChangesFields,
  threadLoginsFields,
} from "./dispatch-api-refs";

/**
 * The comment: the row, who it mentions, how each mention was delivered, and the payloads of
 * every `comment.*` and `suggestion.*` event.
 */

export const commentMentionFields = {
  target: z.string(),
  delivery: z.enum(DELIVERY_CAPABILITIES),
  session_id: z.string().nullable(),
};

export const CommentMentionSchema = z.strictObject(commentMentionFields).readonly();

export type CommentMention = z.output<typeof CommentMentionSchema>;

/** A mention as a bus frame carries it: the same three fields, with anything else dropped. */
export const InboundCommentMentionSchema = z.object(commentMentionFields);

export const commentDeliveryFields = {
  comment_id: z.string(),
  target: z.string(),
  attempt: z.number().int().positive(),
  delivery: z.enum(DELIVERY_CAPABILITIES),
  session_id: z.string().nullable(),
  envelope_id: z.string().nullable(),
  /** The stream already held this message when the attempt was sent: an earlier attempt landed,
   *  so this one is recognised as a repeat and not handed to the mentioned session again
   *  (`DELIVERY_DUPLICATE_WINDOW_MS` states where, and the limits). `envelope_id` is then null.
   *  Absent on a row written before the field existed, which reads as false. */
  duplicate: z.boolean().optional(),
  state: z.enum(["pending", "sent", "failed"]),
  error: z.string().nullable(),
  resolve_error: z.string().nullable(),
  reply_id: z.string().nullable(),
  created_at: z.string(),
};

export const CommentDeliverySchema = z.strictObject(commentDeliveryFields).readonly();

export type CommentDelivery = z.output<typeof CommentDeliverySchema>;

/** One delivery attempt as a bus frame carries it. A stripping object: without `duplicate`
 *  declared the flag is dropped from every agent-bound comment frame, since this schema is
 *  nested inside the passthrough payload schemas. */
export const InboundCommentDeliverySchema = z.object(commentDeliveryFields);

export const suggestionFields = {
  replace_with: z.string(),
  accepted: z.boolean().nullable(),
};

export const SuggestionSchema = z.strictObject(suggestionFields).readonly();

export type Suggestion = z.output<typeof SuggestionSchema>;

export const commentFields = {
  ...anchorPositionFields,
  id: z.string(),
  issue_key: z.string().nullable(),
  artifact_id: z.string().nullable().optional(),
  author: ActorSchema,
  body: z.string(),
  anchor: AnchorSchema.nullable(),
  reply_to: z.string().nullable(),
  ask_id: z.string().nullable(),
  /** On a reply to an open ask, who holds the turn after it; null under a closed ask (nothing is
   *  waiting) and on every other comment. A human's reply is always `agent`; a session's is
   *  `human` unless posted as a progress note. */
  turn: z.enum(ASK_TURNS).nullable(),
  resolved: z.boolean(),
  resolved_by: ActorSchema.nullable(),
  resolved_at: z.string().nullable(),
  edited_at: z.string().nullable(),
  suggestion: SuggestionSchema.nullable(),
  created_at: z.string(),
  mentions: z.array(CommentMentionSchema),
  deliveries: z.array(CommentDeliverySchema),
  /** The block this reply answers (`reply_to_block`): a block of the comment `reply_to` names,
   *  or, with `ask_id`, a part of that ask; null on every other comment, and absent from a
   *  Dispatch older than the field. */
  reply_block: ReplyBlockSchema.nullable().optional(),
};

export const CommentSchema = z.strictObject(commentFields).readonly();

export type Comment = z.output<typeof CommentSchema>;

/** What `GET /api/v1/comments/{id}/blocks` and `GET /api/v1/messages/{id}/blocks` answer: the
 *  comment or message as a graph node, the comment's revision (absent for a message, which is
 *  never edited), whether its blocks are indexed from the body it stores, and its current blocks
 *  in body order. While the item is owed an index, or was indexed from another body than it
 *  stores, `indexed` is false and `blocks` is empty (the read writes nothing): an editor then
 *  edits the comment without carrying block ids. */
export interface ItemBlocks {
  readonly item: GraphNode;
  readonly revision?: number;
  readonly indexed: boolean;
  readonly blocks: readonly ItemBlock[];
}

/** One current block of a comment or message: its id (`b2`), its type, its whole text as its
 *  author wrote it, its reference (absent for a block of a direct message, which has no address),
 *  and `from`/`to`, where it starts and ends in the stored body in UTF-16 code units (a JavaScript
 *  string index). A comment block also carries the revision its text last changed in and that
 *  revision's time; a message block has neither. */
export interface ItemBlock {
  readonly id: string;
  readonly type: string;
  readonly text: string;
  readonly ref?: string;
  readonly from: number;
  readonly to: number;
  readonly changed_revision?: number;
  readonly changed_at?: string;
}

/** What a route that writes a comment answers (`POST /api/v1/issues/{key}/comments`,
 *  `POST /api/v1/artifacts/{id}/comments`, and the delivery callback
 *  `POST /api/v1/comments/{id}/reply`): the comment row and, on a reply to an open ask, whom that
 *  ask waits on now that the reply is its newest. It is the value the comment's event carries
 *  under the same name, and may differ from `turn`: an agent's reply on a moved approval request
 *  leaves that request waiting on the agent. A replayed delivery callback, which writes nothing,
 *  answers the stored reply without it. */
export interface CommentWriteResponse extends Comment {
  readonly ask_waiting_on?: AskTurn;
}

/**
 * `POST /api/v1/comments/{id}/accept` answers the comment plus whether the live document already
 * lacks the text the accept wrote: a concurrent browser change removed it after the accept's
 * version was rendered and before the write reached the room, which is past undoing (LEGION-269).
 * A loss inside the earlier window is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` instead, and leaves the
 * suggestion open. `null` is a check that reached no verdict, which is not the same statement as
 * `false`; `undefined` is a Dispatch server predating the check.
 */
export interface AcceptSuggestionResponse extends Comment {
  readonly lost?: boolean | null;
}

/**
 * The payload of every `comment.*` event. It is the comment row as it stood when the event was
 * written, and events are retained verbatim, so the row's later additions are optional here:
 * `mentions` and `deliveries` were added on 2026-09-18 (#1188), and every comment event recorded
 * before that day carries neither. A read row (`Comment`) always has both. `thread_logins` (the
 * comment's ask, else its root) is carried by `comment.created` and `comment.answered`.
 */
export const commentEventPayloadFields = {
  ...commentFields,
  mentions: z.array(CommentMentionSchema).optional(),
  deliveries: z.array(CommentDeliverySchema).optional(),
  artifact_name: z.string(),
  /** The document owner for an artifact comment, including direct author routes. */
  project_key: z.string().optional(),
  artifact_slug: z.string().optional(),
  /** The question text of the ask this comment replies to (Comment.ask_id); empty otherwise. */
  ask_question: z.string().optional(),
  /** The state of that ask when the comment was posted. A human reply while it is still `open`
   *  is a request for clarification: the asker answers in the thread or rewords the question. */
  ask_state: askFields.state.optional(),
  /** The open ask's `waiting_on` once this comment is its newest reply, so a stream consumer can
   *  move the ask between "waiting on you" and "waiting on the agent" without re-reading it.
   *  Absent when the comment does not reply to an open ask. */
  ask_waiting_on: z.enum(ASK_TURNS).optional(),
  ...referenceChangesFields,
  ...threadLoginsFields,
};

export const CommentEventPayloadSchema = z.strictObject(commentEventPayloadFields).readonly();

export type CommentEventPayload = z.output<typeof CommentEventPayloadSchema>;

/**
 * What a reader of the bus makes of a `comment.*` payload: the fields Dispatch clients are known
 * to read, each optional because an event retained from before a field existed carries none, and
 * the author's `kind` an open string.
 *
 * Bus frames are untrusted and this schema declares only the fields Dispatch clients are known to
 * read today, but the wire payload always carries every field the server model has.
 * `.passthrough()` keeps a field this schema hasn't caught up to riding along instead of silently
 * vanishing when a consumer that reads it is added later, as `ask_waiting_on` and `turn` would
 * vanish from a `comment.created` reply with no test to catch it.
 */
export const InboundCommentEventPayloadSchema = z
  .object({
    id: commentFields.id.optional(),
    artifact_name: commentEventPayloadFields.artifact_name.optional(),
    project_key: commentEventPayloadFields.project_key,
    artifact_slug: commentEventPayloadFields.artifact_slug,
    body: commentFields.body.optional(),
    reply_to: commentFields.reply_to.optional(),
    ask_id: commentFields.ask_id.optional(),
    ask_question: commentEventPayloadFields.ask_question,
    ask_state: commentEventPayloadFields.ask_state,
    ask_waiting_on: commentEventPayloadFields.ask_waiting_on,
    turn: commentFields.turn.optional(),
    anchor: z
      .object({
        artifact_id: anchorFields.artifact_id.optional(),
        block_id: anchorFields.block_id.optional(),
        mark_id: anchorFields.mark_id.optional(),
        orphaned: anchorFields.orphaned.optional(),
        quote: anchorFields.quote.optional(),
        version: anchorFields.version.optional(),
      })
      .nullish(),
    suggestion: z.object({ replace_with: suggestionFields.replace_with.optional() }).nullish(),
    author: InboundActorSchema.optional(),
    created_at: commentFields.created_at.optional(),
    mentions: z.array(InboundCommentMentionSchema).optional(),
    deliveries: z.array(InboundCommentDeliverySchema).optional(),
    reply_block: InboundReplyBlockSchema.nullish(),
    thread_logins: threadLoginsFields.thread_logins,
  })
  .passthrough();

/** The durable result of one mention-delivery attempt in the issue event log. */
export const CommentDeliveryEventPayloadSchema = z
  .strictObject({
    comment_id: z.string(),
    /** The ask whose thread holds the comment, so a consumer keyed on the ask sees a receipt
     *  that changes the thread it renders; null when the comment belongs to no ask, and absent
     *  on a receipt a server older than the field wrote. */
    ask_id: z.string().nullable().optional(),
    target: z.string(),
    attempt: z.number().int().positive(),
    delivery: z.enum(DELIVERY_CAPABILITIES),
    session_id: z.string().nullable(),
    /** A receipt is only ever appended with a settled outcome: the sender that records it holds
     *  the attempt's claim, and both reply handlers settle the row in the statement that appends
     *  theirs. The attempt row itself reads `pending` between its commit and that outcome. */
    state: z.enum(["sent", "failed"]),
    /** The stream already held this message: an earlier attempt landed, so this one is recognised
     *  as a repeat and not handed to the mentioned session again
     *  (`DELIVERY_DUPLICATE_WINDOW_MS`). Absent means false. */
    duplicate: z.boolean().optional(),
    error: z.string().optional(),
    reply_id: z.string().nullable(),
  })
  .readonly();

export type CommentDeliveryEventPayload = z.output<typeof CommentDeliveryEventPayloadSchema>;
