import { z } from "zod";
import {
  type Actor,
  ActorSchema,
  type DisplayTree,
  DisplayTreeSchema,
  type DocEditOp,
  InboundWriterSchema,
} from "./dispatch-api-core";
import { ASK_TURNS } from "./dispatch-api-limits";
import { referenceChangesFields } from "./dispatch-api-refs";

/**
 * A document, a file or an image: the row, its versions and reviews, the blocks a reader
 * addresses it by, and the payloads of every `artifact.*` and `block.*` event.
 */

export const versionFields = {
  number: z.number().int(),
  named: z.boolean(),
  summary: z.string().nullable(),
  authors: z.array(ActorSchema),
  created_at: z.string(),
  /** How many bytes the version stores: a document's markdown (the rendering the version
   *  recorded), or a file's or an image's bytes. The server reads it from the stored row without
   *  rendering the document, so a list can name a document near the size cap without opening it.
   *  Absent on a document version from a server that predates it. */
  size: z.number().int().optional(),
  mime: z.string().optional(),
  sha256: z.string().optional(),
};

export const VersionSchema = z.strictObject(versionFields).readonly();

export type Version = z.output<typeof VersionSchema>;

export type ArtifactReviewState = "approved" | "changes_requested";

/** One human review of a document, pinned to the version it was given on. A review is given one
 *  of three ways: by answering an approval request (`ask_id` names the request), from the
 *  document header (`ask_id` null unless a request was open, which the header answers), or
 *  carried by Dispatch to a later version. A carry follows a human's answer to a decision block,
 *  when that answer is the whole change since the version he approved (`reason`: `Carried forward
 *  from the approval of version <N>: the answer to a decision block was the whole change since
 *  it.`), or a version whose text is byte for byte that of the version the document's newest
 *  review approved and whose every decision block is as its stored row has it (nobody decided one
 *  since), such as an accidental edit undone (`reason`: `Carried forward from the
 *  approval of version <N>: this version's text is that version's, unchanged.`). A carried review
 *  is `approved` in the approver's name, and its `ask_id` is null unless a request was open at the
 *  carry, which it answers `Approve` in his name. */
export interface ArtifactReview {
  readonly id: string;
  readonly artifact_id: string;
  readonly version: number;
  readonly state: ArtifactReviewState;
  readonly actor: Actor;
  readonly reason: string | null;
  /** The display tree of `reason`, absent unless the request sent `Dispatch-Trees: 1`. */
  readonly reason_tree?: DisplayTree;
  readonly ask_id: string | null;
  readonly created_at: string;
}

/** The document's approval as of its latest version: `draft` has never been reviewed or
 *  requested; `awaiting` has an open approval ask; `approved` is approved at the latest version;
 *  `stale` is approved at an older version: a later version leaves the approval there unless
 *  Dispatch carries it to that version (`ArtifactReview`); `changes_requested` carries the
 *  reviewer's reason. */
export const ArtifactApprovalSchema = z
  .strictObject({
    state: z.enum(["draft", "awaiting", "approved", "stale", "changes_requested"]),
    latest_version: z.number().int(),
    version: z.number().int().optional(),
    by: ActorSchema.optional(),
    /** The latest review's `created_at`, which its `artifact.*` event shares. */
    at: z.string().optional(),
    /** The latest review's event sequence on the document's owner, which orders the review among
     *  the owner's events as they commit. Absent for a review recorded before Dispatch kept it, and
     *  on every read the release before that serves (during a rollout or a rollback), so a missing
     *  `seq` does not prove an older review. It orders; it does not say which versions the review
     *  covers, which is `version`: a carried approval's `seq` can be below its version's own
     *  `artifact.version`. */
    seq: z.number().int().optional(),
    reason: z.string().nullable().optional(),
    /** The display tree of `reason`, absent unless the request sent `Dispatch-Trees: 1`. */
    reason_tree: DisplayTreeSchema.optional(),
    ask_id: z.string().nullable().optional(),
    requested_by: ActorSchema.optional(),
    /** Whose move the open request waits on (`Ask.waiting_on`), on `awaiting` alone: `agent` once a
     *  version moved it past the one its agent last handed to the human, or a reply left it with the
     *  agent; `human` otherwise. */
    waiting_on: z.enum(ASK_TURNS).optional(),
  })
  .readonly();

export type ArtifactApproval = z.output<typeof ArtifactApprovalSchema>;

export const artifactFields = {
  id: z.string(),
  issue_key: z.string().nullable(),
  /** `""` for an artifact an agent's conversation owns. */
  project: z.string(),
  /** The Envoy session whose conversation on the Agents page owns the artifact (a picture a person
   *  or the agent sent there), with `issue_key` null and `project` empty; null for an issue's or a
   *  project's artifact. Absent from a Dispatch older than conversation-owned artifacts. */
  session_id: z.string().nullable().optional(),
  /** `<issue key or project key>/<slug>`, or `agent/<session id>/<slug>` for a conversation's. */
  ref_key: z.string().optional(),
  slug: z.string(),
  name: z.string(),
  kind: z.enum(["doc", "image", "file"]),
  primary: z.boolean(),
  created_by: ActorSchema,
  created_at: z.string(),
  versions: z.array(VersionSchema),
  /** Document approval, derived from version-pinned reviews; absent for non-document artifacts. */
  approval: ArtifactApprovalSchema.optional(),
};

export const ArtifactSchema = z.strictObject(artifactFields).readonly();

export type Artifact = z.output<typeof ArtifactSchema>;

export interface ArtifactBlock {
  readonly id: string;
  readonly type: string;
  readonly from: number;
  readonly to: number;
  /** Opaque SHA-256 token for this block's full Proof state, including inline marks, when served by a precondition-aware Dispatch server. */
  readonly token?: string;
  readonly references: {
    readonly comments: number;
    readonly asks: number;
  };
  /** A table block's rows and cells; absent on every other block, and on a table from a Dispatch
   *  server older than the listing. */
  readonly table?: BlockTable;
}

/** A table block's rows in child order, the header row first, each row and cell placed as
 *  `GET /api/v1/artifacts/{id}/blocks/{block_id}` places it, from the same live tree. */
export interface BlockTable {
  readonly rows: readonly BlockTableRow[];
}

/** One row of a table: its block id ("" for a row the live document has not stamped yet), its
 *  index among the table's rows (0 is the header row, the index `delete_row` takes) and its cells
 *  in child order. */
export interface BlockTableRow {
  readonly id: string;
  readonly index: number;
  readonly cells: readonly BlockTableCell[];
}

/** One cell of a table row: its block id ("" for a cell the live document has not stamped yet),
 *  its row index, its column (its index in its row, the index `delete_column` takes, so a cell
 *  spanning columns counts once), the opening words of the header cell drawn above it ("" where
 *  none is) and its own opening words. */
export interface BlockTableCell {
  readonly id: string;
  readonly row: number;
  readonly column: number;
  readonly header: string;
  readonly text: string;
}

/** An opaque SHA-256 token for one stable block's full Proof state, including inline marks. */
export interface EditBlockPrecondition {
  readonly id: string;
  readonly token: string;
}

/** Select either an exact live document or the identified blocks an edit depends on. */
export type EditPrecondition =
  | { readonly document: string; readonly blocks?: never }
  | { readonly document?: never; readonly blocks: readonly EditBlockPrecondition[] };

export interface EditPreconditionMismatch {
  readonly scope: "document" | "block";
  readonly block_id?: string;
  readonly expected: string;
  /** Null when the expected block no longer exists. */
  readonly current: string | null;
}

export interface EditPreconditionCurrent {
  readonly document: string;
  readonly blocks: readonly EditBlockPrecondition[];
}

export interface EditOp {
  readonly op: DocEditOp;
  readonly find?: string;
  readonly with?: string;
  readonly occurrence?: number;
  readonly markdown?: string;
  readonly after?: string;
  readonly before?: string;
  readonly block?: string;
  readonly index?: number;
  readonly type?: string;
  readonly attributes?: Readonly<Record<string, unknown>>;
}

// DocumentRead is GET and PUT /api/v1/me/documents/{id}/read: one person's last read version of a
// document, `version` and `read_at` null when they have read none.
export interface DocumentRead {
  readonly artifact_id: string;
  readonly version: number | null;
  readonly read_at: string | null;
}

export const ArtifactCreatedEventPayloadSchema = z
  .strictObject({ artifact: ArtifactSchema, ...referenceChangesFields })
  .readonly();

export type ArtifactCreatedEventPayload = z.output<typeof ArtifactCreatedEventPayloadSchema>;

/** What a reader of the bus makes of an `artifact.created` payload: the three fields of the
 *  document a renderer names it by, each optional, and everything else dropped. */
export const InboundArtifactCreatedEventPayloadSchema = z.object({
  artifact: z
    .object({
      id: artifactFields.id.optional(),
      slug: artifactFields.slug.optional(),
      name: artifactFields.name.optional(),
    })
    .optional(),
});

export const artifactVersionEventPayloadFields = {
  artifact_id: z.string(),
  name: z.string(),
  version: VersionSchema,
  diff: z.string().optional(),
};

export const ArtifactVersionEventPayloadSchema = z
  .strictObject({ ...artifactVersionEventPayloadFields, ...referenceChangesFields })
  .readonly();

export type ArtifactVersionEventPayload = z.output<typeof ArtifactVersionEventPayloadSchema>;

/** What a reader of the bus makes of an `artifact.version` payload: the fields a renderer shows,
 *  each optional, and of the version only its number and summary. The version number is read as
 *  the stored field, so a fractional one is refused; no writer emits one. */
export const InboundArtifactVersionEventPayloadSchema = z.object({
  artifact_id: artifactVersionEventPayloadFields.artifact_id.optional(),
  name: artifactVersionEventPayloadFields.name.optional(),
  version: z
    .object({ number: versionFields.number.optional(), summary: versionFields.summary.optional() })
    .optional(),
  diff: artifactVersionEventPayloadFields.diff,
});

/** `artifact.human_edited`: a person's browser edits of a document, sent once the document has
 *  been quiet for 30 seconds after the last of them. `from_version` is the version before the
 *  quiet window's first version by a person, `to_version` the window's last one; `editors` names
 *  each person who wrote one, and the event's actor is the one who edited last. `diff` is the
 *  unified diff of the two versions' stored markdown, cut at its last line end within 4,096 bytes,
 *  `diff_truncated` then true; it is empty when a version is no longer stored. The event reaches
 *  each session the document credits and the issue's route, never a person. */
export const artifactHumanEditedEventPayloadFields = {
  artifact_id: z.string(),
  name: z.string(),
  from_version: z.number().int(),
  to_version: z.number().int(),
  editors: z.array(z.string()).readonly(),
  diff: z.string(),
  diff_truncated: z.boolean(),
};

export const ArtifactHumanEditedEventPayloadSchema = z
  .strictObject(artifactHumanEditedEventPayloadFields)
  .readonly();

export type ArtifactHumanEditedEventPayload = z.output<
  typeof ArtifactHumanEditedEventPayloadSchema
>;

/** What a reader of the bus makes of an `artifact.human_edited` payload: every field optional,
 *  because a frame is untrusted and a missing one must not discard the event. */
export const InboundArtifactHumanEditedEventPayloadSchema = z.object({
  artifact_id: artifactHumanEditedEventPayloadFields.artifact_id.optional(),
  name: artifactHumanEditedEventPayloadFields.name.optional(),
  from_version: artifactHumanEditedEventPayloadFields.from_version.optional(),
  to_version: artifactHumanEditedEventPayloadFields.to_version.optional(),
  editors: artifactHumanEditedEventPayloadFields.editors.optional(),
  diff: artifactHumanEditedEventPayloadFields.diff.optional(),
  diff_truncated: artifactHumanEditedEventPayloadFields.diff_truncated.optional(),
});

/** `artifact.approved` and `artifact.changes_requested`: a human review of a document,
 *  pinned to `version`; `reason` is required for changes requested; `ask_id` names the
 *  approval ask the review answered, null when given from the document header with no
 *  approval ask open at that version. A third provenance is the carry: Dispatch records an
 *  `artifact.approved` in a human's name at the version his answer to a decision block writes,
 *  when that answer is the whole change since the version he approved, and at a version whose
 *  text is byte for byte the approved version's with no decision block in it decided since; its
 *  `reason` names the carry and the version it follows, and its `ask_id` is null unless a request
 *  was open at the carry, which it answers (see `ArtifactReview`). */
export const artifactReviewEventPayloadFields = {
  artifact_id: z.string(),
  name: z.string(),
  version: z.number().int(),
  actor: ActorSchema,
  reason: z.string().nullable(),
  ask_id: z.string().nullable(),
};

export const ArtifactReviewEventPayloadSchema = z
  .strictObject(artifactReviewEventPayloadFields)
  .readonly();

export type ArtifactReviewEventPayload = z.output<typeof ArtifactReviewEventPayloadSchema>;

/** What a reader of the bus makes of an `artifact.approved` or `artifact.changes_requested`
 *  payload: every field optional, and the reviewer's `kind` an open string, since a frame
 *  retained from an earlier release may name a kind this one does not. */
export const InboundArtifactReviewEventPayloadSchema = z.object({
  artifact_id: artifactReviewEventPayloadFields.artifact_id.optional(),
  name: artifactReviewEventPayloadFields.name.optional(),
  version: artifactReviewEventPayloadFields.version.optional(),
  actor: InboundWriterSchema.optional(),
  reason: artifactReviewEventPayloadFields.reason.optional(),
  ask_id: artifactReviewEventPayloadFields.ask_id.optional(),
});

/** A server-owned typed-block attribute was restored from its indexed row. */
export const BlockRepairedEventPayloadSchema = z
  .strictObject({ block_id: z.string(), version: z.number().int(), disturbed_by: ActorSchema })
  .readonly();

export type BlockRepairedEventPayload = z.output<typeof BlockRepairedEventPayloadSchema>;

/** A browser-authored ask block is malformed and awaits repair. */
export const BlockInvalidEventPayloadSchema = z
  .strictObject({
    block_id: z.string(),
    version: z.number().int(),
    reason: z.string(),
    disturbed_by: ActorSchema,
  })
  .readonly();

export type BlockInvalidEventPayload = z.output<typeof BlockInvalidEventPayloadSchema>;

/** `decision.kept`, which a person's Keep appends on the decision's issue: the document, the
 *  decision's heading block and the version its text stood at. It wakes nobody - a Keep records
 *  that a person agreed, which asks its agent for nothing. */
export const DecisionKeptEventPayloadSchema = z
  .strictObject({
    artifact_id: z.string(),
    block_id: z.string(),
    changed_version: z.number().int(),
  })
  .readonly();

export type DecisionKeptEventPayload = z.output<typeof DecisionKeptEventPayloadSchema>;
