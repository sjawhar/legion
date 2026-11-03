import type { IssueAskRow } from "./dispatch-api-asks";
import type { Actor, DisplayTree } from "./dispatch-api-core";
import type { Artifact } from "./dispatch-api-documents";

/**
 * The reference graph: what points at what, read by `GET /api/v1/references` and
 * `GET /api/v1/block-links`, and the document lists built on it.
 */

export interface ReferencedBy {
  readonly kind: "ask" | "comment" | "message" | "artifact";
  readonly id: string;
  readonly issue_key: string | null;
  readonly project: string;
  readonly excerpt: string;
  readonly ref_key?: string;
}

export interface ReferenceVia {
  readonly kind: "ask" | "comment" | "message" | "artifact";
  readonly id: string;
}

export interface ReferenceMember {
  readonly artifact: Artifact;
  readonly depth: number;
  readonly via: ReferenceVia;
}

// IssueReferences is the answer of GET /api/v1/issues/{key}/references. With `?versions=latest`
// each member's `artifact.versions` holds only its latest version.
export interface IssueReferences {
  readonly members: ReferenceMember[];
  readonly truncated: boolean;
}

// OutgoingReference is one Dispatch mention a document writes, by its target's kind and id.
// `artifact` is the target document when it resolves; a block target (`kind: "block"`, `to_id`
// `<document ref_key>#<block id>`) carries its document, and is listed only while a current
// version of that document holds the block.
export interface OutgoingReference {
  readonly kind: string;
  readonly to_id: string;
  readonly artifact?: Artifact;
}

export interface ArtifactReferences {
  readonly outgoing: OutgoingReference[];
  readonly referenced_by: ReferencedBy[];
}

// GraphEdgeKind is an edge's kind: one of the structural kinds, or the name of a typed block's
// `block[]` attribute (BlockAttributeSchema), whose pointers are edges of that kind. The block
// schema decides those names (Go's pmdoc.LinkKinds), so any other string is admitted beside the
// structural ones, which keep their completion; none of them is ever an attribute's name.
export type GraphEdgeKind =
  | "mentions"
  | "child_of"
  | "blocked_by"
  | "attached_to"
  | "anchored_to"
  | "owned_by"
  | "replies_to"
  | "followed_by"
  | "part_of"
  | "depends_on"
  | "affects"
  | (string & {});

// GraphNode is one end of a reference-graph edge. `ref` is its dispatch:// address; a
// session node has none. A component node's id is `<project>/<component id>`.
//
// Every node carries six facts: `made_by` and `made_at` (who made it and when), `changed_by`
// and `changed_at` (who last changed its own text or fields and when; the maker when nothing
// changed it since), `parent` (the node it belongs to) and `parent_version` (the version of the
// parent it belongs to). An actor list Dispatch never recorded is `[]`, any other fact `null`.
// They are optional because a server older than them omits them.
//
// A block node (`kind: "block"`, id `<document ref_key>#<block id>`) also carries `block_id`,
// its document's reference as `document`, and `changed_version`, the document version in which
// its text last changed (its `changed_at` is that version's time). A block of a comment, an issue
// message or an ask (id `comment/<uuid>#<block id>`, `message/<uuid>#<block id>` or
// `ask/<ask id>#<part id>`, a part being the ask's question, one of its options or its answer)
// carries no `document`: its `parent` is the comment, message or ask, and a comment block's or an
// ask part's `changed_version` is the comment revision or the ask's part revision in which its
// text last changed (a message block has none). `text` and `pin` are set only on the node a
// reference read names: `text` is the block's whole current text, and `pin` answers a reference
// pinned to a version (or a comment's or an ask's revision).
export interface GraphNode {
  readonly kind:
    | "issue"
    | "artifact"
    | "block"
    | "ask"
    | "comment"
    | "message"
    | "session"
    | "component";
  readonly id: string;
  readonly block_id?: string;
  readonly issue_key?: string;
  readonly project?: string;
  readonly ref?: string;
  readonly document?: string;
  readonly changed_version?: number;
  readonly made_by?: Actor[];
  readonly made_at?: string | null;
  readonly changed_by?: Actor[];
  readonly changed_at?: string | null;
  readonly parent?: GraphNodeParent | null;
  readonly parent_version?: number | null;
  readonly text?: string;
  /** The display tree of `text`, absent unless the request sent `Dispatch-Trees: 1`. */
  readonly text_tree?: DisplayTree;
  readonly pin?: BlockPin;
}

// GraphNodeParent is the node a graph node belongs to: `id` is the parent node's own `id` (an
// artifact's uuid, an issue's key, a component's `<project>/<id>`) and `ref` its dispatch://
// reference, absent when it has none (a session).
export interface GraphNodeParent {
  readonly kind: GraphNode["kind"];
  readonly id: string;
  readonly ref?: string;
}

// BlockPin is a block reference pinned to a version of its document (`#b-<id>@v<N>`): the
// pinned reference, the block's whole text at that version, and how the block stands now.
// `state` is "unchanged" when its current text equals `text`, "changed" when it differs, and
// "removed" when no current version holds the block; `removed_version` is then the version that
// deleted it.
export interface BlockPin {
  readonly version: number;
  readonly ref: string;
  readonly text: string;
  /** The display tree of `text`, absent unless the request sent `Dispatch-Trees: 1`. */
  readonly text_tree?: DisplayTree;
  readonly state: "unchanged" | "changed" | "removed";
  readonly removed_version?: number;
}

// GraphExcerpt is the text shown beside an edge: the block containing a document mention
// (with its block id) or the head of the other node's own text.
export interface GraphExcerpt {
  readonly block_id?: string;
  readonly text: string;
}

// GraphEdge is one typed, timestamped edge seen from the queried node: `node` is the other
// end, `direction` is "in" for edges pointing at the queried node and "out" for edges it
// writes, and `source_seq` is the events.id that introduced a mention (null for structure).
// `block`, on a document's read, names the document's block at the queried end: the block an
// inbound edge points at, or the block that holds an outbound link.
export interface GraphEdge {
  readonly kind: GraphEdgeKind;
  readonly direction: "in" | "out";
  readonly node: GraphNode;
  readonly block?: GraphNode;
  readonly excerpt?: GraphExcerpt;
  readonly created_at: string;
  readonly source_seq: number | null;
}

// GraphReferences is GET /api/v1/references: the queried node and its edges, newest first.
export interface GraphReferences {
  readonly node: GraphNode;
  readonly edges: GraphEdge[];
}

// BlockLink is one link from a block of one document to a block of a document: its edge kind,
// both block nodes, and when and in which version of its document the source block first held
// it.
export interface BlockLink {
  readonly kind: GraphEdgeKind;
  readonly from: GraphNode;
  readonly to: GraphNode;
  readonly created_at: string;
  readonly created_version: number;
}

// BlockLinks is GET /api/v1/block-links?from=<document ref>&to=<document ref>: the two documents
// and the links from blocks of `from` to blocks of `to`, newest first, then by `from.id`, then by
// `to.id`. A link is listed only while both of its blocks are current blocks of their documents.
export interface BlockLinks {
  readonly from: GraphNode;
  readonly to: GraphNode;
  readonly links: BlockLink[];
}

// DecisionKeep is one person's agreement with a decision: the decision's heading block, the
// version its text stood at when they pressed Keep, and who did when. What tells a kept decision
// from one waiting for review again is the sum of its text, which the server records beside the
// row and serves as `waiting_review`: a decision's `changed_version` is the greatest of the
// blocks it holds now, so it does not move when a block is removed from the decision or moved
// into or out of it.
export interface DecisionKeep {
  readonly artifact_id: string;
  readonly block_id: string;
  readonly changed_version: number;
  readonly kept_by: string;
  readonly kept_at: string;
}

// DecisionLink is one link from a block of a decision to a block of the issue's spec: the
// decision's own block that holds it, the spec block it names, when and in which version of
// decisions.md the link was first written, and whether that spec block is out of date - the
// decision changed after it first linked the block, and the block has not changed since.
export interface DecisionLink {
  readonly from_block: string;
  readonly to: GraphNode;
  readonly created_version: number;
  readonly created_at: string;
  readonly stale: boolean;
}

// Decision is one decision of an issue's `decisions.md`: its heading block and every block after
// it up to the next heading of its level or higher. `kind` is "person" when its blocks mention an
// ask (a person's decision links the answered ask) and "agent" otherwise; `unsure` is set by a
// paragraph that starts `Unsure:`; `replaced` by sitting under the section headed exactly
// `## Replaced`. `changed_version` and `changed_at` are the greatest of its blocks'.
// `text_sha256` is the sum of `text`, which Keep carries back to say which text it agreed with.
// `waiting_review` is an agent's decision, not replaced, whose text nobody has kept as it
// stands.
export interface Decision {
  readonly block_id: string;
  readonly heading: string;
  readonly ref: string;
  readonly text: string;
  readonly text_sha256: string;
  readonly kind: "agent" | "person";
  readonly unsure: boolean;
  readonly replaced: boolean;
  readonly asks: string[];
  readonly links: DecisionLink[];
  readonly changed_version: number;
  readonly changed_at: string | null;
  readonly kept: DecisionKeep | null;
  readonly waiting_review: boolean;
}

// IssueDecisions is GET /api/v1/issues/{key}/decisions: the issue's decisions document, the open
// questions its spec still holds (oldest first) and each decision of that document in document
// order. `document` is null, with no decisions, when the issue has no `decisions.md`.
export interface IssueDecisions {
  readonly document: string | null;
  readonly open_questions: IssueAskRow[];
  readonly decisions: Decision[];
}
