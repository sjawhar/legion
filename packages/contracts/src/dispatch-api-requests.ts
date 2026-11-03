import type { Ask, AskOption } from "./dispatch-api-asks";
import type { Actor, AnchorInput, AskUrgency, ExternalLink } from "./dispatch-api-core";
import type {
  Artifact,
  EditOp,
  EditPrecondition,
  EditPreconditionCurrent,
  EditPreconditionMismatch,
  Version,
} from "./dispatch-api-documents";
import type { Event } from "./dispatch-api-events";
import type {
  Issue,
  IssueChild,
  IssueComponentsInput,
  IssuePriority,
  IssueProgress,
  IssueRouteReach,
} from "./dispatch-api-issues";
import type { AskTurn, DeliveryCapability, MessageDeliveryMode } from "./dispatch-api-limits";
import type { DecisionBlockCheck, DuplicateCandidate, Suggestions } from "./dispatch-api-search";
import type { IssueStatus } from "./dispatch-tools";

/**
 * What a caller sends and what Dispatch answers: the write bodies of every route, the advice a
 * write comes back with, and the read shapes assembled from several areas at once.
 */

export interface WriteAdvice {
  /** Issue-scoped fields are omitted on project-document writes. */
  readonly issue_status?: IssueStatus;
  /** The calling session's own messages, comments and asks on the issue since the last human event. */
  readonly session_writes_since_human?: number;
  readonly your_open_asks?: Array<{ id: string; question: string }>;
  /** The ask blocks in the document an upload or issue creation stored, answered ones included. */
  readonly decision_blocks?: number;
  /**
   * A document edit's ask blocks the document before it did not hold, as the server's parser reads
   * the edited document: one inserted or retyped into an ask counts wherever it lands, a blockquote
   * or a list item included, and an opener quoted in code counts nothing. On every edit's advice;
   * absent from a Dispatch server predating it.
   */
  readonly decision_blocks_added?: number;
  /**
   * The related-item check each of a document edit's new decision blocks got, in
   * document order; a block past the fifth in one edit answers `not_made`. Omitted when the edit
   * added none, and absent from a Dispatch server predating the check, which only counts them in
   * `decision_blocks_added`.
   */
  readonly decision_block_checks?: DecisionBlockCheck[];
  /**
   * The typed block openings (`:::ask{…}`) the written document holds as text rather than as
   * blocks, outside code: written inside a line, or escaped. `examples` quotes the first few with a
   * little of the text before each. Omitted when there are none.
   */
  readonly unparsed_openers?: { readonly count: number; readonly examples: readonly string[] };
  /**
   * The approval a document write superseded: the version it stored replaced the one a person
   * had approved, so that approval is stale until someone approves the new version or a later
   * write stores the approved version's text again exactly with no decision block in it decided
   * since, which carries the approval to that write's version. A Legion design gate is such an
   * approval, so this write closed it. Set by a document edit and by an upload that replaced an
   * existing document, and only on the write that superseded the approval: a write on a document
   * nobody approved, one whose approval was already stale, and one whose version carried that
   * approval forward (the approval carry: an answer to a decision block, or the approved version's
   * text stored again) each omit it.
   */
  readonly superseded_approval?: {
    /** The version a person approved, the one this write superseded. */
    readonly approved_version: number;
    /** The version this write stored. */
    readonly version: number;
    /** Who gave that approval. */
    readonly approved_by: string;
  };
  /**
   * The related-item check (LEGION-550), set on an issue or ask creation: the items
   * most like what was just filed, any past decision that matches, and what the check did
   * (`outcome`). Never refuses the write; it waits at most five seconds, and a check that did not
   * search to the end says why in `missing`.
   */
  readonly suggestions?: Suggestions;
  /**
   * True when the issue carries the `legion` label in any case, or sits under an issue that
   * does, so only the Legion daemon writes its status and a status anyone else sets is undone.
   * Resolved only where a line depends on it: a session's write on an issue whose status is
   * `triage`, where it decides whether the "still in triage" advice is printed. Absent
   * everywhere else — on a write to an issue past triage, and on a write whose actor is not a
   * session, which is answered without running any advice query.
   */
  readonly legion_managed_status?: boolean;
}

/** Response-only; never on an event payload. */
export type Advised<T> = T & { readonly advice?: WriteAdvice };

export interface CreateProjectInput {
  readonly key: string;
  readonly name: string;
  readonly actor?: Actor;
}

export interface CreateIssueInput {
  readonly project: string;
  readonly title: string;
  readonly parent?: string;
  /** Issue keys in the same project this issue waits on. */
  readonly blocked_by?: string[];
  readonly external?: string;
  readonly spec?: string;
  readonly force?: boolean;
  readonly labels?: string[];
  readonly priority?: IssuePriority | null;
  /** The email of a person who has signed in; omitted, the server picks the creating person, the
   *  personal token's owner, the parent's assignee, or the project's default assignee when that
   *  still names a person who has signed in. */
  readonly assignee?: string;
  /** The issue's own component attachment; omitted, it inherits its parent's. */
  readonly components?: IssueComponentsInput;
  /** The repository the issue targets, owner/name, mapped to its project; omitted, it inherits its
   *  parent's, else the project's default. */
  readonly repo?: string;

  readonly actor?: Actor;
}

export interface IssueRankInput {
  readonly before?: string;
  readonly after?: string;
}

export interface UpdateIssueInput {
  readonly title?: string;
  readonly status?: string;
  readonly rank?: IssueRankInput;
  readonly labels?: string[];
  readonly priority?: IssuePriority | null;
  readonly route?: string | null;
  readonly external_links?: ExternalLink[];
  /** The email of a person who has signed in, or null to unassign; omitted leaves the assignee
   *  alone. */
  readonly assignee?: string | null;
  /** A parent issue key in the same project, or null to clear; omitted leaves it alone. */
  readonly parent?: string | null;
  /** Replaces every issue key this issue waits on; [] clears it. Omitted leaves it alone. */
  readonly blocked_by?: string[];
  /** The issue's own component attachment; null (or mode `inherit`) deletes it so the issue
   *  inherits again; omitted leaves it alone. Allowed on a closed issue. */
  readonly components?: IssueComponentsInput | null;
  /** The issue's own repository, owner/name, mapped to its project, or null to clear it so the
   *  issue inherits again; omitted leaves it alone. Refused on a closed issue. */
  readonly repo?: string | null;
  readonly actor?: Actor;
}

export interface CreateAskInput {
  readonly question: string;
  readonly options?: AskOption[];
  readonly multiple?: boolean;
  readonly urgency?: AskUrgency;
  readonly anchor?: AnchorInput;
  readonly actor?: Actor;
}

export interface EditAskInput {
  readonly question?: string;
  readonly options?: AskOption[];
  readonly multiple?: boolean;
  readonly urgency?: AskUrgency;
  readonly actor?: Actor;
}

export interface AnswerAskInput {
  readonly selected: string[];
  readonly text?: string;
  /** The ask's `edited_at` value when the human reviewed the current wording. */
  readonly expected_edited_at: string | null;
  /** The current answer's `at` when its answerer changes it; absent for a first answer. */
  readonly expected_answer_at?: string | null;
}

export interface ResolveAskInput {
  readonly kind: "retracted" | "resolved";
  readonly reason: string;
  readonly actor?: Actor;
}

export interface CreateCommentInput {
  readonly body: string;
  readonly anchor?: AnchorInput;
  readonly reply_to?: string;
  /** The id of the block the reply answers (`b2`): a block of the comment `reply_to` names,
   *  checked against that comment before `reply_to` climbs to its thread's head, in an ask's
   *  thread as anywhere else, or, with `ask_id`, a part of that ask. `400 INVALID_COMMENT`
   *  without `reply_to` or `ask_id`, and for a block the item it is checked against does not
   *  hold now. */
  readonly reply_to_block?: string;
  readonly ask_id?: string;
  readonly mentions?: Array<{ readonly target: string }>;
  readonly delivery?: DeliveryCapability;
  /** Only with `ask_id`: `agent` marks a progress note that keeps the ask waiting on its asker;
   *  `human` (the default for a session) hands the turn to the human. Ignored for a human author,
   *  whose reply always hands the turn to the agent, and under a closed ask, where no turn is
   *  recorded. `400 TURN_REQUIRES_ASK` without `ask_id`. */
  readonly turn?: AskTurn;
  readonly suggestion?: { readonly replace_with: string };
  readonly actor?: Actor;
}

export interface EditCommentInput {
  readonly body: string;
  /** The comment's revision the editor read its blocks at. Required with `blocks`; when it is not
   *  the comment's current revision (another edit saved first), `blocks` is ignored and the
   *  server matches the new body's blocks to the comment's by their text. */
  readonly base_revision?: number;
  /** Each block id the editor carried through the edit, in body order, with `from`, where the
   *  block's span starts in `body`, in UTF-16 code units (a JavaScript string index). The new
   *  body's block holding a carried `from` keeps that id. `400 INVALID_COMMENT` naming the field
   *  for an id that is not a block of the comment at `base_revision`, an id twice, a `from`
   *  outside `body` or before the one ahead of it, or `blocks` without `base_revision`. */
  readonly blocks?: readonly { readonly id: string; readonly from: number }[];
}

export interface CreateMessageInput {
  readonly body: string;
  readonly in_reply_to?: string;
  /** The id of the block of the message `in_reply_to` names that the reply answers (`b2`).
   *  `400 INVALID_MESSAGE` without `in_reply_to` and for a block that message does not hold. */
  readonly in_reply_to_block?: string;
  readonly target?: string;
  readonly delivery?: MessageDeliveryMode;
  readonly urgency?: "low" | "med" | "high" | "blocking";
  readonly actor?: Actor;
}

/** A human-targeted message sent directly from an agent card, without an issue owner; a reply
 *  names a message of the same issue-less conversation. */
export interface CreateAgentMessageInput {
  readonly body: string;
  readonly delivery: MessageDeliveryMode;
  readonly in_reply_to?: string;
  /** The id of the block of the message `in_reply_to` names that the reply answers (`b2`). */
  readonly in_reply_to_block?: string;
}

/** The targeted session's answer to one delivery attempt of a message
 *  (`POST /api/v1/messages/{id}/reply`). It is the only route that carries a reply into an
 *  issue-less conversation - a human's direct message to a session - because every other message
 *  route hangs its message off an issue. The server accepts it only from a bearer whose `actor`
 *  is the session the attempt was delivered to, and threads the reply under the message it
 *  answers. Only the answering form is modelled here; a host that could not deliver the frame
 *  reports that failure through its own delivery reply, not through a Dispatch tool. */
export interface MessageReplyInput {
  readonly body: string;
  /** The delivery attempt being answered; attempts count from 1. */
  readonly attempt: number;
  readonly actor: Actor;
  /** The id of the block of the message answered that the reply answers (`b2`); refused on a
   *  follow-up, which is filed under the session's first reply. */
  readonly in_reply_to_block?: string;
}

interface CreateArtifactOptions {
  readonly name: string;
  readonly summary?: string;
  readonly actor?: Actor;
}

export type CreateArtifactInput = CreateArtifactOptions &
  ({ readonly file: Blob } | { readonly content: string });

export interface CreateVersionInput {
  readonly summary: string;
  readonly actor?: Actor;
}

export interface EditArtifactInput {
  readonly ops: EditOp[];
  readonly summary?: string;
  readonly precondition?: EditPrecondition;
  readonly actor?: Actor;
}

export interface IssueDetails extends Issue, IssueRouteReach {
  readonly progress: IssueProgress;
  readonly artifacts: Artifact[];
  readonly open_asks: Ask[];
  readonly children: IssueChild[];
  /** How many graph edges point at this issue, counted with the detail so the header's
   *  `Referenced by (N)` control costs no request of its own. */
  readonly referenced_by_count: number;
}

export interface IssueRead {
  readonly issue: IssueDetails;
  readonly events: Event[];
}

export interface ArtifactText {
  readonly markdown: string;
  readonly version: number | null;
  /** Opaque SHA-256 token for the full Proof document state, including inline marks, when served by a precondition-aware Dispatch server. */
  readonly token?: string;
}

export interface ArtifactRebuildReport {
  readonly head: number;
  readonly removed_checkpoints: number;
  readonly removed_snapshots: number;
  readonly removed_updates: number;
  /** The version whose markdown the rebuilt document holds: its latest saved version, or the
   *  version a rebuild from supplied markdown that changed the document wrote. */
  readonly source_version: number;
  readonly validation_error: string;
}

export interface ArtifactVersionText extends Version {
  readonly markdown: string;
}

export interface ArtifactVersionBlob {
  readonly blob: Blob;
  readonly mime: string;
  readonly version: number;
}

export type ArtifactVersionContent = ArtifactVersionText | ArtifactVersionBlob;

export interface ArtifactUploadResponse {
  readonly artifact: Artifact;
  readonly version: Version;
  readonly advice?: WriteAdvice;
}

export interface EditArtifactResponse {
  readonly applied: number;
  readonly version: Version | null;
  /** False when the batch left the document's canonical markdown exactly as it was, in which
   *  case no version is minted, named or not. Absent from a Dispatch server predating the check. */
  readonly changed?: boolean;
  /** Zero-based index of each operation that left the document as it found it. */
  readonly unchanged_ops?: number[];
  /** Zero-based index of each operation whose text the live document no longer carried once this
   *  edit reached it: a concurrent browser change removed it after the version was rendered and
   *  before the write was published, which is past undoing, so the version records text the live
   *  document does not have. An empty array is the ordinary outcome. `null` is a check that
   *  reached no verdict, which is not the same statement; `undefined` is a Dispatch server
   *  predating the check (LEGION-269). A loss inside the earlier window is refused outright with
   *  `409 EDIT_LOST_TO_CONCURRENT_CHANGE`, not reported here. */
  readonly lost_ops?: number[] | null;
  /** Opaque SHA-256 token for the full Proof document state this edit produced, including inline
   *  marks: the document precondition for the caller's next edit, with no read in between. Absent
   *  from a Dispatch server predating it. */
  readonly token?: string;
  readonly advice?: WriteAdvice;
}

/** ProseMirror positions in the live document. */
export interface TargetCandidate {
  readonly from: number;
  readonly to: number;
  readonly context: string;
}

export interface DispatchServiceErrorShape {
  readonly error?: string;
  readonly code?: string;
  readonly candidates?: TargetCandidate[] | DuplicateCandidate[];
  readonly current?: EditPreconditionCurrent;
  readonly mismatches?: EditPreconditionMismatch[];
}
