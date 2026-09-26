/**
 * The upstream types this package's public surface names.
 *
 * `src/lib.ts` re-exports `StoredMark` to Dispatch, which builds mark records against it, and
 * names `HeatMapMode` in `CreateProofEditorOptions`. The copied tree reaches upstream through
 * `proof-sdk-upstream/src/…`, a specifier no tsc program resolves (packages/proof-editor/
 * AGENTS.md § The upstream boundary), so a type that arrived that way would reach a consumer as
 * `any` — `kind: "replaced"` and `heatMapMode: "hiden"` would both type-check. These
 * declarations are copied verbatim from proof-sdk at
 * 24a5fc94915cda5704897c497af6312c140db40d, each region naming the file it came from, so the
 * consumer sees the real shape; `tests/upstream-pin.test.ts` reads those regions and fails when
 * the pinned files stop agreeing with them. This file is checked by tsc; it carries no
 * `@ts-nocheck`.
 */

/* --- copied from proof-sdk src/editor/plugins/heatmap-decorations.ts @ 24a5fc94 --- */

export type HeatMapMode = 'hidden' | 'subtle' | 'background' | 'full';

/* --- end copy --- */

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
