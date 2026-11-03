import { z } from "zod";
import {
  ActorSchema,
  AnchorSchema,
  anchorFields,
  anchorPositionFields,
  InboundBySchema,
  InboundWriterSchema,
} from "./dispatch-api-core";
import { type Issue, type IssueRouteReach, issueFields } from "./dispatch-api-issues";
import { ASK_TURNS } from "./dispatch-api-limits";
import { referenceChangesFields, threadLoginsFields } from "./dispatch-api-refs";
import { ASK_URGENCIES } from "./dispatch-tools";

/**
 * The ask: the row every ask read answers with, the lists it appears in, and the payloads of
 * every `ask.*` event. Each payload is `z.output` of the schema beside it, and each inbound
 * reading is that schema with the keys, the optionality and the open strings a bus frame needs.
 */

export const AskKindSchema = z.enum(["question", "approval"]);

export type AskKind = z.output<typeof AskKindSchema>;

export const askOptionFields = {
  /** The part this option is stored as: the block `dispatch://<key>/ask/<ask id>#b-<id>` names,
   *  pinned to the ask's `revision` with `@v<N>`. Read-only, and carried by every read that shows
   *  an ask's options; absent on a write's own answer, and while the ask's parts are not indexed
   *  yet. A write that sends it back has it ignored: no caller mints or moves a part id. */
  id: z.string().optional(),
  label: z.string(),
  description: z.string().optional(),
};

export const AskOptionSchema = z.strictObject(askOptionFields).readonly();

export type AskOption = z.output<typeof AskOptionSchema>;

export const askAnswerFields = {
  /** The part the ask's current answer is stored as, read as an option's `id` is and never
   *  written by a caller; a superseded answer, which no part of the ask holds, carries none. */
  id: z.string().optional(),
  user: z.string(),
  selected: z.array(z.string()),
  text: z.string().nullable(),
  at: z.string(),
};

export const AskAnswerSchema = z.strictObject(askAnswerFields).readonly();

export type AskAnswer = z.output<typeof AskAnswerSchema>;

export const askResolutionFields = {
  /** `withdrawn` is written by the server alone, when the ask's issue is closed; no caller can
   *  resolve an ask with it, and reopening the issue opens the ask again. */
  kind: z.enum(["retracted", "resolved", "withdrawn"]),
  reason: z.string(),
  actor: ActorSchema,
  at: z.string(),
};

export const AskResolutionSchema = z.strictObject(askResolutionFields).readonly();

export type AskResolution = z.output<typeof AskResolutionSchema>;

export const askEditPreviousFields = {
  question: z.string(),
  options: z.array(AskOptionSchema),
  multiple: z.boolean(),
  urgency: z.enum(ASK_URGENCIES),
};

export const AskEditPreviousSchema = z.strictObject(askEditPreviousFields).readonly();

export type AskEditPrevious = z.output<typeof AskEditPreviousSchema>;

/** One recorded rewording of an ask: what the question was before this edit, who edited, when. */
export const AskEditSchema = z
  .strictObject({ previous: AskEditPreviousSchema, edited_by: ActorSchema, at: z.string() })
  .readonly();

export type AskEdit = z.output<typeof AskEditSchema>;

export const askAnchorArtifactFields = {
  project: z.string(),
  slug: z.string(),
  name: z.string(),
  primary: z.boolean(),
};

export const AskAnchorArtifactSchema = z.strictObject(askAnchorArtifactFields).readonly();

export type AskAnchorArtifact = z.output<typeof AskAnchorArtifactSchema>;

export const AskBlockArtifactSchema = z
  .strictObject({ id: z.string(), slug: z.string(), primary: z.boolean() })
  .readonly();

export type AskBlockArtifact = z.output<typeof AskBlockArtifactSchema>;

export const AskLastReplySchema = z
  .strictObject({ author: ActorSchema, created_at: z.string() })
  .readonly();

export type AskLastReply = z.output<typeof AskLastReplySchema>;

export const InboxDocumentSchema = z
  .strictObject({ project: z.string(), slug: z.string(), name: z.string() })
  .readonly();

export type InboxDocument = z.output<typeof InboxDocumentSchema>;

/** The approval an approval request follows: the version a human approved, when (RFC 3339 UTC,
 *  the review's `created_at`) and who. */
export const AskApprovalApprovedSchema = z
  .strictObject({ version: z.number().int(), at: z.string(), by: ActorSchema })
  .readonly();

export type AskApprovalApproved = z.output<typeof AskApprovalApprovedSchema>;

/** The document and version an approval ask names. `requested_version` is the version the agent
 *  last handed to the human; a lower value means the agent is revising the moved request.
 *  `approved` names the document's newest approval when the request was opened, so a request to
 *  approve again says which approval it follows; it is absent on a request for a document nobody
 *  has approved. A version move re-encodes the whole approval, so it survives unchanged. */
export const AskApprovalSchema = z
  .strictObject({
    artifact_id: z.string(),
    name: z.string(),
    version: z.number().int(),
    requested_version: z.number().int(),
    approved: AskApprovalApprovedSchema.optional(),
  })
  .readonly();

export type AskApproval = z.output<typeof AskApprovalSchema>;

export const askFields = {
  ...anchorPositionFields,
  id: z.string(),
  issue_key: z.string().nullable(),
  artifact_id: z.string().nullable().optional(),
  /** The source typed block; null for asks created outside a document. */
  block_id: z.string().nullable().optional(),
  /** The document named by `anchor`, present on ask reads and ask events. */
  anchor_artifact: AskAnchorArtifactSchema.optional(),
  /** The document that contains a typed ask block. */
  block_artifact: AskBlockArtifactSchema.optional(),
  author: ActorSchema,
  /** `approval` asks are server-created document reviews; a `question`'s asker chooses its
   *  options. */
  kind: AskKindSchema,
  question: z.string(),
  options: z.array(AskOptionSchema),
  multiple: z.boolean(),
  urgency: z.enum(ASK_URGENCIES),
  anchor: AnchorSchema.nullable(),
  state: z.enum(["open", "answered", "resolved"]),
  answer: AskAnswerSchema.nullable(),
  resolution: AskResolutionSchema.optional(),
  /** The canonical event ID of this ask's opening turn. */
  opened_event_id: z.number().int().positive(),
  created_at: z.string(),
  /** Batches the current inbound graph-edge count for ask-list surfaces. */
  referenced_by_count: z.number().int().optional(),
  /** The current revision of the ask's parts: the number a reference to one of them pins
   *  (`dispatch://<key>/ask/<id>#b-<part>@v<N>`). Batched beside the part ids of the options and
   *  the answer, so it is absent on a write's own answer and while the ask's parts are not indexed
   *  yet, where no revision can be pinned. */
  revision: z.number().int().positive().optional(),
  issue: z
    .strictObject({
      key: issueFields.key,
      title: issueFields.title,
      assignee: issueFields.assignee,
    })
    .readonly()
    .optional(),
  document: InboxDocumentSchema.optional(),
  /** Inbox rows, the issue detail's `open_asks` and the issue ask list: the newest reply in the
   *  ask's thread, or null when nobody has replied. Who spoke last; whose turn it is comes from
   *  `waiting_on`. Absent on every other ask read. */
  last_reply: AskLastReplySchema.nullable().optional(),
  /** Whose reply an open ask needs next. A moved approval request stays with its agent while
   *  `requested_version` is below `version`; a hand-back (`ask.handed_back`) returns it to the
   *  human until a reply newer than the one it answered decides it by its `turn`. Present on every
   *  open-ask read (inbox rows, ask lists, the ask detail, the issue detail's `open_asks`); absent
   *  on closed asks and on `ask.*` event payloads. */
  waiting_on: z.enum(ASK_TURNS).optional(),
  edited_at: z.string().nullable(),
  /** Present only on a server-created approval ask. */
  approval: AskApprovalSchema.optional(),
  /** The document `approval.artifact_id` names, hydrated live like `anchor_artifact` and
   *  `block_artifact`: its current slug and primary flag, not a snapshot from when the request
   *  opened. Present only alongside `approval`. */
  approval_artifact: AskAnchorArtifactSchema.optional(),
};

export const AskSchema = z.strictObject(askFields).readonly();

export type Ask = z.output<typeof AskSchema>;

/** Whom besides the followers a person's answer and replies on an ask reach, as every ask read
 *  carries it: the single-ask read (`AskRead`) and each list row's thread (`AskThread`).
 *
 *  The inherited `route_status` and `route_holder` are the issue reads' `IssueRouteReach` for
 *  `route`, resolved per read from one listener read and stored nowhere: `route_holder` is the
 *  live session `route` reaches right now (the routed session while the Envoy listener lists it,
 *  or the session holding the role), so a reader can count each session once (the claimant may be
 *  the same session). `route_status` tells a route nobody holds (`no_holder`) from one Dispatch
 *  cannot judge because the listener is unconfigured or did not answer (`unknown`); both leave
 *  `route_holder` null. Both are null when there is no route, and on a project document's ask. */
export interface AskReach extends IssueRouteReach {
  /** The session holding the claim on the ask's issue: a person's answer and replies on the ask
   *  reach it (Dispatch's claimant route) as well as the route and the followers. Null while
   *  nobody or a person holds the claim, and on a project document's ask, which has no issue. */
  readonly claimant: string | null;
  /** The ask's issue's route (`session:<id>` or `role:<name>`): a person's answer and replies on
   *  the ask reach that session or role holder as well as the claimant and the followers. Null
   *  when the issue has no route, and on a project document's ask. */
  readonly route: string | null;
}

/** One ask of `GET /api/v1/issues/{key}/asks`. The row is an `Ask` plus the one field a list
 *  surface cannot compute for itself: `last_reply`, the sort key for ordering a page by its
 *  newest reply. It carries no thread: the list is unpaged, and the readers that render the ask
 *  alone - a document's decision blocks and the review margin - would pay for every reply body,
 *  follower and answer on the issue to draw none of them. */
export interface IssueAskRow extends Ask {
  /** The newest reply in this ask's thread, or null when nobody has replied. */
  readonly last_reply: AskLastReply | null;
}

/** What `PUT /api/v1/me/asks/{id}/snooze` answers: the stored moment the row returns. */
export interface AskSnooze {
  readonly snoozed_until: string;
}

export type OpenAskOwner =
  | { readonly issue: Pick<Issue, "key" | "title"> }
  | { readonly document: InboxDocument };

/**
 * A session that receives an ask's answer, edits, resolution, and replies on its own
 * agent topic: the asker, every session that replied, and any session a human added.
 * A session may leave (or rejoin) only itself; a human may add or remove any session.
 */
export const AskFollowerSchema = z
  .strictObject({ session_id: z.string(), since: z.string() })
  .readonly();

export type AskFollower = z.output<typeof AskFollowerSchema>;

/** Every `ask.*` event payload is the ask row, flat, plus what the write moved in the reference
 *  graph. `restored` is true on the `ask.opened` or `ask.answered` settlement appends when an edit
 *  restores a decision block it retracted: the ask returns to the state it had, and no one answered
 *  anything new. A restore the release before Dispatch kept the mark writes - every one already in
 *  the event log, and one written during a rollout or an image rollback - carries no mark, so a
 *  missing `restored` does not prove an event is a new answer. `thread_logins` (the ask's own
 *  thread) is carried by `ask.opened`, `ask.answered` and `ask.resolved`. */
export const askEventPayloadFields = {
  ...askFields,
  ...referenceChangesFields,
  ...threadLoginsFields,
  restored: z.boolean().optional(),
};

export const AskEventPayloadSchema = z.strictObject(askEventPayloadFields).readonly();

export type AskEventPayload = z.output<typeof AskEventPayloadSchema>;

/** An `ask.answered` payload. `previous_answer` is present when the answer replaced an earlier
 *  one; the asker and every follower receive it as they received the first. */
export const AskAnsweredEventPayloadSchema = z
  .strictObject({ ...askEventPayloadFields, previous_answer: AskAnswerSchema.optional() })
  .readonly();

export type AskAnsweredEventPayload = z.output<typeof AskAnsweredEventPayloadSchema>;

export const AskEditEventPayloadSchema = z
  .strictObject({
    ...askEventPayloadFields,
    previous: AskEditPreviousSchema,
    edited_by: ActorSchema,
    /** A version move of an approval request that follows an earlier move since the request was
     *  opened or last handed back: before this move, its `requested_version` was already below the
     *  version it named. The payload's `approval.version` is the version after the move, so the
     *  first move satisfies `requested_version < version` too and is not quiet. A quiet move changes
     *  only the version the request names, so its event, like a human's unnamed `artifact.version`,
     *  carries `notify: false` and reaches no follower. Absent on every other edit, the first move
     *  after an opening or a hand-back included, even when a thread reply left the request waiting
     *  on its agent. */
    quiet: z.literal(true).optional(),
  })
  .readonly();

export type AskEditEventPayload = z.output<typeof AskEditEventPayloadSchema>;

/**
 * The fields a reader of the bus makes of an `ask.*` payload: the ones Dispatch clients render,
 * each optional because an event retained from before a field existed carries none.
 *
 * `kind` stays an open string: JetStream retains `ask.*` envelopes for 72 h, so a renderer may
 * still see a kind the server no longer writes (the removed `action`). The outbound `AskKind`
 * stays narrow.
 */
const inboundAskEventPayloadFields = {
  id: askFields.id.optional(),
  opened_event_id: askFields.opened_event_id,
  kind: z.string().optional(),
  question: askFields.question.optional(),
  // An option and the answer carry the part id of the block each is stored as, and the payload the
  // ask's part revision, as every other read of the ask carries them; a payload the ask has moved
  // past carries no id, and one of an ask whose parts are not indexed yet no revision either.
  options: z
    .array(z.object({ id: askOptionFields.id, label: askOptionFields.label.optional() }))
    .optional(),
  revision: askFields.revision,
  answer: z
    .object({
      id: askAnswerFields.id,
      selected: askAnswerFields.selected.nullish(),
      text: askAnswerFields.text.optional(),
    })
    .nullish(),
  previous_answer: z
    .object({
      user: askAnswerFields.user.optional(),
      selected: askAnswerFields.selected.nullish(),
      text: askAnswerFields.text.optional(),
      at: askAnswerFields.at.optional(),
    })
    .nullish(),
  restored: askEventPayloadFields.restored,
  anchor: z
    .object({
      quote: anchorFields.quote.optional(),
      mark_id: anchorFields.mark_id.optional(),
      block_id: anchorFields.block_id.optional(),
    })
    .passthrough()
    .nullish(),
  anchor_artifact: z
    .object({
      project: askAnchorArtifactFields.project.optional(),
      slug: askAnchorArtifactFields.slug.optional(),
      name: askAnchorArtifactFields.name.optional(),
      primary: askAnchorArtifactFields.primary.optional(),
    })
    .optional(),
  resolution: z
    .object({
      kind: askResolutionFields.kind.optional(),
      reason: askResolutionFields.reason.optional(),
      // The resolver of a frame retained from an earlier release may name neither kind nor id.
      actor: z
        .object({ kind: z.string().optional(), id: z.string().optional() })
        .passthrough()
        .optional(),
      at: askResolutionFields.at.optional(),
    })
    .nullish(),
  thread_logins: threadLoginsFields.thread_logins,
};

export const InboundAskEventPayloadSchema = z.intersection(
  z.object(inboundAskEventPayloadFields),
  z.object({
    previous: z.never().optional(),
    edited_by: z.never().optional(),
  })
);

/**
 * What a reader of the bus makes of an `ask.edited` payload: the ask fields above plus the
 * rewording, with `urgency` an open string for the same reason `kind` is one.
 */
export const InboundAskEditEventPayloadSchema = z.object({
  ...inboundAskEventPayloadFields,
  multiple: askFields.multiple,
  urgency: z.string(),
  edited_at: askFields.edited_at.optional(),
  previous: z.object({
    question: askEditPreviousFields.question,
    options: z.array(z.object({ label: askOptionFields.label.optional() })),
    multiple: askEditPreviousFields.multiple,
    urgency: z.string(),
  }),
  edited_by: InboundWriterSchema,
});

/** `ask.edited` carries an `AskEditEventPayload`, under the name the event type spells: one
 *  payload, two spellings. This alias is the name every reader of the bus already imports. */
export const AskEditedEventPayloadSchema = InboundAskEditEventPayloadSchema;

// AskFollowerEventPayload is the wire payload of ask.follower_added and
// ask.follower_removed: which session joined or left which ask, and who did it. The
// removed event also reaches the removed session's own topic directly.
export const askFollowerEventPayloadFields = {
  ask_id: z.string(),
  session_id: z.string(),
  by: ActorSchema,
};

export const AskFollowerEventPayloadSchema = z
  .strictObject(askFollowerEventPayloadFields)
  .readonly();

export type AskFollowerEventPayload = z.output<typeof AskFollowerEventPayloadSchema>;

/** What a reader of the bus makes of an `ask.follower_added` or `ask.follower_removed` payload:
 *  every field optional, and the writer read as a frame may carry it. */
export const InboundAskFollowerEventPayloadSchema = z.object({
  ask_id: askFollowerEventPayloadFields.ask_id.optional(),
  session_id: askFollowerEventPayloadFields.session_id.optional(),
  by: InboundBySchema.optional(),
});
