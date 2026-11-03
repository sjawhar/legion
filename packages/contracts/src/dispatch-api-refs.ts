import { z } from "zod";

/**
 * The shapes several payload families share: what a write moved in the reference graph, whose
 * Replies count it moved, and the block a reply answers. Each is one schema, and each family
 * spreads its fields into its own payload rather than re-typing them.
 */

export const changedReferenceFields = {
  kind: z.enum(["issue", "ask", "artifact", "block"]),
  id: z.string(),
  issue_key: z.string().nullable().optional(),
  artifact_id: z.string().nullable().optional(),
  ref: z.string().optional(),
};

export const ChangedReferenceSchema = z.strictObject(changedReferenceFields).readonly();

/**
 * A node whose inbound reference edges one write moved, addressed the way the reads that show
 * it are keyed: an issue by key, an ask by id together with the issue or document whose ask list
 * holds its row, and a document or file (`artifact`) by its uuid (`id`, the node id its
 * `GET /api/v1/references` read reports) and its `ref`, the dispatch:// address its Referenced by
 * panel is opened on (`dispatch://CORE/artifact/runbook`, `dispatch://CORE-1/spec`,
 * `dispatch://CORE-1/artifact/notes-md`). An `artifact` entry carries neither `issue_key` nor
 * `artifact_id`; `ref` is absent for one whose owner names no address. A block of a document
 * (`block`) is named by its node id (`<document ref_key>#<block id>`, as its read reports it), its
 * `ref` (`dispatch://CORE-1/spec#b-intro`) and its document's uuid as `artifact_id`, whose panel
 * lists the edges at its blocks; a write names one whose inbound edges or whose text it moved. A
 * citation of a comment or message changes no row read by name and the graph query covers it. An
 * event written before `artifact` or `block` entries existed names none.
 */
export type ChangedReference = z.output<typeof ChangedReferenceSchema>;

/**
 * The reference-change pair every event that reports a graph write carries, mirroring the
 * server's embedded `model.ReferenceChangesPayload`: one shape, so a new payload family
 * spreads these fields rather than re-typing them.
 */
export const referenceChangesFields = {
  /** The nodes whose inbound reference edges this write moved, named so a reader carrying
   *  their batched backlink counts refreshes exactly those rows. Absent when the write moved
   *  no counted node, and on every event recorded before the field existed. */
  references_changed: z.array(ChangedReferenceSchema).optional(),
  /** The write moved more counted nodes of some kind than an event carries (`refs.CountedChangeLimit`
   *  of each kind): a reader that does not read `references_changed_truncated_kinds` refreshes
   *  every list that holds a count. */
  references_changed_truncated: z.boolean().optional(),
  /** The kinds whose targets the write cut from `references_changed`, set with
   *  `references_changed_truncated`; the targets of every other kind are still named. A reader
   *  refreshes every read of each kind listed, and only those. Absent from an event written
   *  before the field existed, whose truncation said nothing of its kinds. */
  references_changed_truncated_kinds: z.array(changedReferenceFields.kind).optional(),
};

export const ReferenceChangesPayloadSchema = z.strictObject(referenceChangesFields).readonly();

export type ReferenceChangesPayload = z.output<typeof ReferenceChangesPayloadSchema>;

/** Whose Replies count a write can have moved, mirroring the server's `ThreadLogins` payload
 *  field: every login with a Replies row (the server's `user_threads`) on the write's thread once
 *  the write is in, sorted, `[]` when nobody has one. Each payload family names the event types
 *  that carry it; it is absent from every other type and from every event written before the
 *  field existed, which a reader takes as "anyone's count may have moved". */
export const threadLoginsFields = {
  thread_logins: z.array(z.string()).optional(),
};

export const ThreadLoginsPayloadSchema = z.strictObject(threadLoginsFields).readonly();

export type ThreadLoginsPayload = z.output<typeof ThreadLoginsPayloadSchema>;

export const replyBlockFields = {
  node: z.string(),
  ref: z.string().optional(),
  block_id: z.string(),
  text: z.string(),
  removed: z.boolean(),
};

/** The block a reply answers: its node id (`comment/<uuid>#b2`, or `ask/<uuid>#b2` for a part of
 *  the ask the reply names), its reference (absent for a block of a direct message, which has no
 *  address), its id, its text cut to 480 characters (the last text it held once removed), and
 *  whether its item's current revision no longer holds it. */
export const ReplyBlockSchema = z.strictObject(replyBlockFields).readonly();

export type ReplyBlock = z.output<typeof ReplyBlockSchema>;

/** The block a reply answers, as a comment or message event payload carries it (`ReplyBlock`).
 *  An event recorded before the field, or by a Dispatch older than it, carries none. A bus frame
 *  may carry a field this release does not know, so the inbound reading strips rather than
 *  refuses. */
export const InboundReplyBlockSchema = z.object(replyBlockFields);
