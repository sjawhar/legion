/**
 * The upstream types this package re-exports on its public surface.
 *
 * `src/lib.ts` re-exports `StoredMark` to Dispatch, which builds mark records against it. The
 * rest of the copied tree reaches upstream through `proof-sdk-upstream/src/…`, a specifier no
 * tsc program resolves (packages/proof-editor/AGENTS.md § The upstream boundary), so a type
 * re-exported from there arrives at a consumer as `any` — `kind: "replaced"` type-checks. These
 * declarations are copied verbatim from proof-sdk `src/formats/marks.ts` at
 * 24a5fc94915cda5704897c497af6312c140db40d, so the consumer sees the real shape;
 * `tests/upstream-types-match-the-pin.test.ts` fails when the pinned file stops agreeing with
 * them. This file is checked by tsc; it carries no `@ts-nocheck`.
 *
 * Upstream types that reach a consumer any other way are still `any`. `HeatMapMode`, which
 * `CreateProofEditorOptions.heatMapMode` names, is the one that does: `lib.ts` imports it
 * alongside two values in a single statement, and rewriting that statement is more than the
 * import-specifier edit the copy allows.
 */

/* --- copied from proof-sdk src/formats/marks.ts @ 24a5fc94 --- */

export type MarkKind =
  | 'authored'    // Who created this content (replaces provenance)
  | 'approved'    // Content signed off
  | 'flagged'     // Needs attention
  | 'comment'     // Discussion thread
  | 'insert'      // Proposed addition
  | 'delete'      // Proposed removal
  | 'replace';    // Proposed replacement

export type SuggestionStatus = 'pending' | 'accepted' | 'rejected';

export interface MarkRange {
  from: number;
  to: number;
}

export interface CommentReply {
  by: string;
  text: string;
  at: string;
}

export interface StoredMark {
  kind?: MarkKind;
  by?: string;
  createdAt?: string;
  range?: MarkRange;
  /** Relative anchor start (char-offset form: `char:<offset>`). */
  startRel?: string;
  /** Relative anchor end (char-offset form: `char:<offset>`). */
  endRel?: string;
  text?: string;
  thread?: string | CommentReply[];
  threadId?: string;
  replies?: CommentReply[];
  resolved?: boolean;
  content?: string;
  status?: SuggestionStatus;
  note?: string;
  runId?: string;
  focusAreaId?: string;
  focusAreaName?: string;
  agentId?: string;
  proposalId?: string;
  provisional?: boolean;
  orchestrator?: boolean;
  debugAutoFixedQuotes?: boolean;
  debugAutoFixedQuotesReason?: string;
  /** Quote text for remote sync — allows recreating ProseMirror anchors on remote clients */
  quote?: string;
}

/* --- end copy --- */
