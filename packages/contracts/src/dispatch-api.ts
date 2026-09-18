import { z } from "zod";
import type { ASK_URGENCIES, DOC_EDIT_OPS, ISSUE_COMPONENTS_MODES } from "./dispatch-tools";

export type AskUrgency = (typeof ASK_URGENCIES)[number];
export type DocEditOp = (typeof DOC_EDIT_OPS)[number];

export interface ActorOrigin {
  readonly host?: string;
  readonly machine?: string;
  readonly cwd?: string;
  readonly tmux?: string;
  readonly pane?: string;
  readonly session_title?: string;
}

export type Actor =
  | { readonly kind: "user"; readonly id: string }
  | {
      readonly kind: "session";
      readonly id: string;
      readonly origin?: ActorOrigin;
      readonly owner?: string;
    }
  /** A server-owned writer, such as the architecture importer
   *  (`{kind: "system", id: "architecture-importer"}`). */
  | { readonly kind: "system"; readonly id: string };

export type BlockAttributeKind = "string" | "bool" | "enum" | "string[]" | "actor" | "timestamp";

export type BlockContentRule = "paragraph+" | "block+" | "paragraph+ bullet_list?";

export interface BlockAttributeSchema {
  readonly kind: BlockAttributeKind;
  readonly choices?: readonly string[];
  readonly default?: string | boolean | readonly string[];
  readonly server?: boolean;
}

export interface BlockTypeSchema {
  readonly name: string;
  readonly content: BlockContentRule;
  readonly render: "host";
  readonly attributes: Readonly<Record<string, BlockAttributeSchema>>;
}

export interface BlockSchema {
  readonly version: number;
  readonly types: readonly BlockTypeSchema[];
}

export interface AgentToken {
  readonly id: string;
  readonly name: string;
  readonly prefix: string;
  readonly created_at: string;
  readonly last_used_at: string | null;
  readonly revoked_at: string | null;
}

export interface CreatedAgentToken extends AgentToken {
  readonly token: string;
}

export interface Anchor {
  readonly artifact_id: string;
  /** Stable owner block for new anchors; null for legacy rows awaiting backfill. */
  readonly block_id: string | null;
  readonly mark_id: string;
  readonly version: number;
  readonly quote: string;
  readonly orphaned: boolean;
}

export type AnchorInput =
  | {
      readonly artifact: string;
      readonly quote: string;
      readonly occurrence?: number;
      readonly mark_id?: never;
    }
  | {
      readonly artifact: string;
      readonly mark_id: string;
      readonly quote?: never;
      readonly occurrence?: never;
    };

export interface Project {
  readonly key: string;
  readonly name: string;
  readonly open_asks?: number;
  readonly created_at: string;
}

export interface RepoProject {
  readonly repo: string;
  readonly project: string;
  readonly created_by: Actor;
  readonly created_at: string;
}

/**
 * The repository and branch a project's architecture documents are imported
 * from (directory fixed at .dispatch/architecture/). The GitHub App
 * installation id backing the access check is a server detail and never
 * appears here. The sync fields stay null until the importer runs.
 */
export interface ArchitectureSource {
  readonly project: string;
  readonly repo: string;
  readonly branch: string;
  readonly enabled: boolean;
  readonly created_by: Actor;
  readonly created_at: string;
  readonly last_sync_at: string | null;
  readonly last_commit: string | null;
  readonly last_error: string | null;
}

export interface ExternalLink {
  readonly url: string;
  readonly kind?: string;
}

export type IssuePriority = 0 | 1 | 2 | 3;

export type IssueComponentsMode = (typeof ISSUE_COMPONENTS_MODES)[number];

/**
 * An issue's effective component attachment, resolved on read: the nearest issue on its
 * parent chain (itself first) with its own attachment decides, and `inherited_from` names it
 * when that is an ancestor. Mode `inherit` with `inherited_from` null means no ancestor chose
 * (unassigned). `ids` is the effective set of live component ids; `unknown` the effective ids
 * a re-import has since retired; `reason` is set for mode `none`.
 */
export interface IssueComponents {
  readonly mode: IssueComponentsMode;
  readonly ids: string[];
  readonly unknown: string[];
  readonly reason: string | null;
  readonly inherited_from: string | null;
}

/**
 * The `components` field of an issue write. `inherit` deletes the issue's own attachment
 * (back to its ancestors'); `explicit` replaces it with `ids`, bare component ids of the
 * issue's project that are not external; `none` records `reason` the issue is not
 * architectural.
 */
export interface IssueComponentsInput {
  readonly mode: IssueComponentsMode;
  readonly ids?: string[];
  readonly reason?: string;
}

export interface Issue {
  readonly key: string;
  readonly project: string;
  readonly number: number;
  readonly title: string;
  readonly status: string;
  readonly priority: IssuePriority | null;
  readonly rank: string;
  readonly labels: string[];
  readonly parent: string | null;
  /** Lowercase GitHub login of the human who answers this issue's asks; null when unassigned. */
  readonly assignee: string | null;
  readonly components: IssueComponents;
  readonly external_links: ExternalLink[];
  readonly route: string | null;
  readonly created_by: Actor;
  readonly created_at: string;
  readonly updated_at: string;
  readonly closed_at: string | null;
  readonly primary_artifact_id: string;
  readonly last_seq: number;
}

export interface IssueSummary
  extends Pick<
    Issue,
    | "key"
    | "title"
    | "status"
    | "priority"
    | "rank"
    | "parent"
    | "assignee"
    | "components"
    | "updated_at"
    | "last_seq"
  > {
  readonly labels?: string[];
  readonly open_asks: number;
}

export interface IssueChild {
  readonly key: string;
  readonly title: string;
  readonly status: string;
  /** Issues in the child's subtree (itself included, every status) with status `done`. */
  readonly subtree_done: number;
  /** Issues in the child's subtree, the child itself included. */
  readonly subtree_total: number;
  /** The newest `updated_at` anywhere in the child's subtree (RFC3339). */
  readonly active_at: string;
  readonly external_links: ExternalLink[];
}

/**
 * GET /api/v1/projects/{key}/architecture: the project's component model with the work
 * attached to it. Counting rule: a component counts every distinct issue whose effective set
 * names it or any component it contains (transitively over `parent`), parents and icebox
 * included, each once; `own_*` keeps only the issues naming this component itself. History
 * is not served.
 */
export interface ArchitectureTree {
  readonly source: ArchitectureTreeSource;
  readonly totals: ArchitectureTreeTotals;
  readonly components: ArchitectureTreeComponent[];
  /** Issues no ancestor chain attached. */
  readonly unassigned: ArchitectureTreeIssueRef[];
  /** Issues resolving to a `none` row, their own or an ancestor's. */
  readonly not_architectural: ArchitectureTreeNone[];
  /** Issues whose effective set names components a re-import retired. */
  readonly retired_links: ArchitectureTreeRetired[];
}

export interface ArchitectureTreeSource {
  readonly repo: string;
  readonly branch: string;
  readonly last_commit: string | null;
  readonly last_sync_at: string | null;
  readonly last_error: string | null;
}

export interface ArchitectureTreeTotals {
  /** Every filed issue in the project, icebox and closed included. */
  readonly issues_done: number;
  readonly issues_total: number;
  readonly unassigned: number;
  readonly not_architectural: number;
  /** Non-external components whose `total` is 0. */
  readonly components_without_work: number;
  readonly retired_links: number;
}

export interface ArchitectureTreeComponent {
  readonly id: string;
  readonly title: string;
  readonly parent: string | null;
  readonly depends_on: string[];
  readonly paths: string[];
  readonly external: boolean;
  readonly prose: string;
  readonly done: number;
  readonly total: number;
  readonly own_done: number;
  readonly own_total: number;
  readonly issues: ArchitectureTreeIssue[];
}

/** How an issue counts for a component; the strongest wins when several apply. */
export type ArchitectureTreeAttachment = "direct" | "inherited" | "contained";

export interface ArchitectureTreeIssue {
  readonly key: string;
  readonly title: string;
  readonly status: string;
  readonly priority: IssuePriority | null;
  readonly parent: string | null;
  readonly external_links: ExternalLink[];
  readonly updated_at: string;
  readonly attached: ArchitectureTreeAttachment;
  /** For `contained`: the descendant component the issue's set names. */
  readonly via?: string;
}

export interface ArchitectureTreeIssueRef {
  readonly key: string;
  readonly title: string;
  readonly status: string;
}

export interface ArchitectureTreeNone extends ArchitectureTreeIssueRef {
  readonly reason: string;
  readonly inherited_from: string | null;
}

export interface ArchitectureTreeRetired extends ArchitectureTreeIssueRef {
  readonly ids: string[];
}

export interface Artifact {
  readonly id: string;
  readonly issue_key: string | null;
  readonly project: string;
  readonly ref_key?: string;
  readonly slug: string;
  readonly name: string;
  readonly kind: "doc" | "image" | "file";
  readonly primary: boolean;
  readonly created_by: Actor;
  readonly created_at: string;
  readonly versions: Version[];
  readonly referenced_by?: ReferencedBy[];
  /** Document approval, derived from version-pinned reviews; absent for non-document artifacts. */
  readonly approval?: ArtifactApproval;
}

export type ArtifactReviewState = "approved" | "changes_requested";

/** One human review of a document, pinned to the version it was given on. */
export interface ArtifactReview {
  readonly id: string;
  readonly artifact_id: string;
  readonly version: number;
  readonly state: ArtifactReviewState;
  readonly actor: Actor;
  readonly reason: string | null;
  readonly ask_id: string | null;
  readonly created_at: string;
}

/** The document's approval as of its latest version: `draft` has never been reviewed or
 *  requested; `awaiting` has an open approval ask; `approved` is approved at the latest version;
 *  `stale` is approved at an older version; `changes_requested` carries the reviewer's reason. */
export interface ArtifactApproval {
  readonly state: "draft" | "awaiting" | "approved" | "stale" | "changes_requested";
  readonly latest_version: number;
  readonly version?: number;
  readonly by?: Actor;
  readonly at?: string;
  readonly reason?: string | null;
  readonly ask_id?: string | null;
  readonly requested_by?: Actor;
}

export interface ArtifactBlock {
  readonly id: string;
  readonly type: string;
  readonly from: number;
  readonly to: number;
  readonly references: {
    readonly comments: number;
    readonly asks: number;
  };
}

export interface Version {
  readonly number: number;
  readonly named: boolean;
  readonly summary: string | null;
  readonly authors: Actor[];
  readonly created_at: string;
  readonly size?: number;
  readonly mime?: string;
  readonly sha256?: string;
}

export type AskKind = "question" | "approval";

export interface Ask {
  readonly id: string;
  readonly issue_key: string | null;
  readonly artifact_id?: string | null;
  /** The source typed block; null for asks created outside a document. */
  readonly block_id?: string | null;
  /** The document named by `anchor`, present on ask reads and ask events. */
  readonly anchor_artifact?: AskAnchorArtifact;
  /** The document that contains a typed ask block. */
  readonly block_artifact?: AskBlockArtifact;
  readonly author: Actor;
  /** `approval` asks are server-created document reviews; a `question`'s asker chooses its options. */
  readonly kind: AskKind;
  readonly question: string;
  readonly options: AskOption[];
  readonly multiple: boolean;
  readonly urgency: AskUrgency;
  readonly anchor: Anchor | null;
  readonly state: "open" | "answered" | "resolved";
  readonly answer: AskAnswer | null;
  readonly resolution?: AskResolution;
  /** The canonical event ID of this ask's opening turn. */
  readonly opened_event_id: number;
  readonly created_at: string;
  readonly issue?: Pick<Issue, "key" | "title" | "assignee">;
  readonly document?: InboxDocument;
  /** Inbox rows and the issue detail's `open_asks`: the newest reply in the ask's thread, or
   *  null when nobody has replied. Who spoke last; whose turn it is comes from `waiting_on`.
   *  Absent on every other ask read. */
  readonly last_reply?: AskLastReply | null;
  /** Whose reply an open ask needs next: the `turn` of its newest reply, `human` when nobody
   *  has replied. Present on every open-ask read (inbox rows, ask lists, the ask detail, the
   *  issue detail's `open_asks`); absent on closed asks and on `ask.*` event payloads. */
  readonly waiting_on?: AskTurn;
  readonly edited_at: string | null;
}

/** Who holds the turn on an open ask after a reply: `human` when the human needs to act,
 *  `agent` when the asking agent still owes the next move (a progress note). */
export const ASK_TURNS = ["human", "agent"] as const;
export type AskTurn = (typeof ASK_TURNS)[number];

/** The complete data an Inbox card needs before its first targeted thread refresh. */
export interface InboxThread {
  readonly replies: Comment[];
  /** Every rewording of the question, oldest first; empty when never edited. */
  readonly edits: AskEdit[];
  /** The sessions this ask's answer and replies reach directly, oldest first. */
  readonly followers: AskFollower[];
}

/** An open ask as returned by the human Inbox. */
export interface InboxRow extends Ask {
  /** The owning issue's coarse priority, or null for an unset or unassigned issue. */
  readonly priority: IssuePriority | null;
  /** The card's initial thread; `["ask-thread", id]` remains its targeted live refresh. */
  readonly thread: InboxThread;
}

export interface AskAnchorArtifact {
  readonly project: string;
  readonly slug: string;
  readonly name: string;
  readonly primary: boolean;
}

export interface AskBlockArtifact {
  readonly id: string;
  readonly slug: string;
  readonly primary: boolean;
}

export interface AskLastReply {
  readonly author: Actor;
  readonly created_at: string;
}

export interface InboxDocument {
  readonly project: string;
  readonly slug: string;
  readonly name: string;
}

export type OpenAskOwner =
  | { readonly issue: Pick<Issue, "key" | "title"> }
  | { readonly document: InboxDocument };

export interface OpenAsk {
  readonly id: string;
  /** Relative Dispatch deep link to the ask's owner. */
  readonly ref: string;
  readonly question: string;
  readonly kind: AskKind;
  readonly urgency: AskUrgency;
  readonly created_at: string;
  readonly age_seconds: number;
  readonly priority: IssuePriority | null;
  readonly owner: OpenAskOwner;
  /** True when any human has replied, even if the agent spoke last. */
  readonly human_replied: boolean;
  readonly last_reply: AskLastReply | null;
  /** Whose reply would move the currently open ask forward. */
  readonly waiting_on: AskTurn;
}

export interface OpenAsksResponse {
  readonly session_id: string;
  readonly as_of: string;
  /** An ask was opened by this session at or after the supplied server baseline. */
  readonly opened_since: boolean;
  readonly count: number;
  readonly waiting_on_human: number;
  readonly waiting_on_agent: number;
  readonly asks: OpenAsk[];
}

export interface AskOption {
  readonly label: string;
  readonly description?: string;
}

export interface AskAnswer {
  readonly user: string;
  readonly selected: string[];
  readonly text: string | null;
  readonly at: string;
}

export interface AskResolution {
  readonly kind: "retracted" | "resolved";
  readonly reason: string;
  readonly actor: Actor;
  readonly at: string;
}

export interface AskEditPrevious {
  readonly question: string;
  readonly options: AskOption[];
  readonly multiple: boolean;
  readonly urgency: AskUrgency;
}

export type AskEditEventPayload = Ask & {
  readonly previous: AskEditPrevious;
  readonly edited_by: Actor;
};

/** One recorded rewording of an ask: what the question was before this edit, who edited, when. */
export interface AskEdit {
  readonly previous: AskEditPrevious;
  readonly edited_by: Actor;
  readonly at: string;
}

export interface CommentMention {
  readonly target: string;
  readonly delivery: DeliveryCapability;
  readonly session_id: string | null;
}

export interface CommentDelivery {
  readonly comment_id: string;
  readonly target: string;
  readonly attempt: number;
  readonly delivery: DeliveryCapability;
  readonly session_id: string | null;
  readonly envelope_id: string | null;
  readonly state: "pending" | "sent" | "failed";
  readonly error: string | null;
  readonly resolve_error: string | null;
  readonly reply_id: string | null;
  readonly created_at: string;
}

/** The durable result of one mention-delivery attempt in the issue event log. */
export interface CommentDeliveryEventPayload {
  readonly comment_id: string;
  readonly target: string;
  readonly attempt: number;
  readonly delivery: DeliveryCapability;
  readonly session_id: string | null;
  readonly state: "pending" | "sent" | "failed";
  readonly error?: string;
  readonly reply_id: string | null;
}
export interface Comment {
  readonly id: string;
  readonly issue_key: string | null;
  readonly artifact_id?: string | null;
  readonly author: Actor;
  readonly body: string;
  readonly anchor: Anchor | null;
  readonly reply_to: string | null;
  readonly ask_id: string | null;
  /** On a reply to an open ask, who holds the turn after it; null under a closed ask (nothing is
   *  waiting) and on every other comment. A human's reply is always `agent`; a session's is
   *  `human` unless posted as a progress note. */
  readonly turn: AskTurn | null;
  readonly resolved: boolean;
  readonly resolved_by: Actor | null;
  readonly resolved_at: string | null;
  readonly edited_at: string | null;
  readonly suggestion: Suggestion | null;
  readonly created_at: string;
  readonly mentions: CommentMention[];
  readonly deliveries: CommentDelivery[];
}

export interface Suggestion {
  readonly replace_with: string;
  readonly accepted: boolean | null;
}

export interface CommentEventPayload extends Comment {
  readonly artifact_name: string;
  /** The document owner for an artifact comment, including direct author routes. */
  readonly project_key?: string;
  readonly artifact_slug?: string;
  /** The question text of the ask this comment replies to (Comment.ask_id); empty otherwise. */
  readonly ask_question?: string;
  /** The state of that ask when the comment was posted. A human reply while it is still `open`
   *  is a request for clarification: the asker answers in the thread or rewords the question. */
  readonly ask_state?: Ask["state"];
  /** The open ask's `waiting_on` once this comment is its newest reply, so a stream consumer can
   *  move the ask between "waiting on you" and "waiting on the agent" without re-reading it.
   *  Absent when the comment does not reply to an open ask. */
  readonly ask_waiting_on?: AskTurn;
}

/**
 * The closed list of targeted-delivery modes a session can advertise as
 * capabilities on the Envoy listener (`/v1/interests/subscribe`) and that
 * Dispatch can request for a message. Every other spelling of this set —
 * `MessageDeliveryMode`, the delivery event payload schema, the envoy-client
 * targeted-frame schema — derives from this constant.
 */
export const DELIVERY_CAPABILITIES = ["aside", "btw", "steer"] as const;

export type DeliveryCapability = (typeof DELIVERY_CAPABILITIES)[number];

export type MessageDeliveryMode = DeliveryCapability;

export interface MessageDelivery {
  readonly message_id: string;
  readonly attempt: number;
  readonly delivery: MessageDeliveryMode;
  readonly session_id: string;
  readonly envelope_id: string | null;
  readonly state: "sent" | "failed";
  readonly error: string | null;
  readonly reply_id: string | null;
  readonly created_at: string;
}

export interface Message {
  readonly id: string;
  readonly issue_key: string | null;
  readonly author: Actor;
  readonly body: string;
  readonly target: string | null;
  readonly in_reply_to: string | null;
  readonly deliveries: MessageDelivery[];
  readonly created_at: string;
}

export interface MessageEventPayload extends Message {
  /** First 160 characters of the reply target's body; empty otherwise. */
  readonly reply_body?: string;
}

export interface MessageDeliveryEventPayload {
  readonly message_id: string;
  readonly attempt: number;
  readonly delivery: MessageDeliveryMode;
  readonly session_id: string;
  readonly target?: string;
  readonly title: string;
  readonly state: "sent" | "failed";
  readonly error?: string;
}

export type SearchResultKind = "issue" | "document" | "comment" | "ask" | "message";

export interface SearchArtifactRef {
  readonly slug: string;
  readonly name: string;
}

export type SearchOwner =
  | {
      readonly kind: "issue";
      readonly key: string;
      readonly title: string;
      readonly status: string;
    }
  | {
      readonly kind: "document";
      readonly project: string;
      readonly slug: string;
      readonly artifact_id: string;
      readonly name: string;
    };

export interface SearchResult {
  readonly kind: SearchResultKind;
  /** Who owns the hit: the issue (with its title and status) or the standalone project document. */
  readonly owner: SearchOwner;
  /**
   * Mirrors `owner` for issue-owned hits so agent clients built before the owner-only shape
   * (#1119) keep rendering; remove once no installed pi-legion-envoy / opencode-legion-envoy /
   * claude-envoy-bridge predates it. Clients built after #1119 read `owner`, never this.
   */
  readonly issue?: {
    readonly key: string;
    readonly title: string;
    readonly status: string;
  };
  readonly artifact?: SearchArtifactRef;
  readonly id: string;
  readonly snippet: string;
  readonly rank: number;
  readonly href: string;
}

export interface SearchResponse {
  readonly results: SearchResult[];
  readonly took_ms: number;
}

export interface DuplicateCandidate {
  readonly key: string;
  readonly title: string;
  readonly status: string;
  readonly snippet: string;
  readonly shared_terms: number;
  readonly href: string;
}

export interface Agent {
  readonly session_id: string;
  readonly title: string;
  readonly dir: string;
  readonly machine_id: string;
  readonly roles: string[];
  /**
   * Wire value from the Envoy registry: an open string list. The known values
   * are `DELIVERY_CAPABILITIES`; unknown strings persist and never match a mode.
   */
  readonly capabilities: string[];
  readonly last_seen: number;
  readonly open_asks: number;
  readonly last_activity: string | null;
}

// Subscriber is a session whose persisted Envoy interests include an issue's
// or unlinked document's topic family, enriched with its live registry state
// when the session is currently connected. Removable is false when the only
// matching topic is broader than this owner (e.g. "notifications.dispatch.>"):
// removing it would silently unsubscribe the session from every other issue
// and document too, so the server refuses to remove it; `via` then names one
// such topic.
export interface Subscriber {
  readonly session_id: string;
  readonly title: string;
  readonly live: boolean;
  readonly last_seen: number;
  readonly topics: readonly string[];
  readonly removable: boolean;
  readonly via?: string;
}

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

export interface IssueReferences {
  readonly members: ReferenceMember[];
  readonly truncated: boolean;
}

export interface OutgoingReference {
  readonly kind: string;
  readonly to_id: string;
  readonly artifact?: Artifact;
}

export interface ArtifactReferences {
  readonly outgoing: OutgoingReference[];
  readonly referenced_by: ReferencedBy[];
}

export type GraphEdgeKind =
  | "mentions"
  | "child_of"
  | "attached_to"
  | "anchored_to"
  | "owned_by"
  | "replies_to"
  | "followed_by"
  | "part_of"
  | "depends_on"
  | "affects";

// GraphNode is one end of a reference-graph edge. `ref` is its dispatch:// address; a
// session node has none. A component node's id is `<project>/<component id>`.
export interface GraphNode {
  readonly kind: "issue" | "artifact" | "ask" | "comment" | "message" | "session" | "component";
  readonly id: string;
  readonly issue_key?: string;
  readonly project?: string;
  readonly ref?: string;
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
export interface GraphEdge {
  readonly kind: GraphEdgeKind;
  readonly direction: "in" | "out";
  readonly node: GraphNode;
  readonly excerpt?: GraphExcerpt;
  readonly created_at: string;
  readonly source_seq: number | null;
}

// GraphReferences is GET /api/v1/references: the queried node and its edges, newest first.
export interface GraphReferences {
  readonly node: GraphNode;
  readonly edges: GraphEdge[];
}

export interface ArtifactCreatedEventPayload {
  readonly artifact: Artifact;
}

export interface ArtifactVersionEventPayload {
  readonly artifact_id: string;
  readonly name: string;
  readonly version: Version;
  readonly diff?: string;
}

/** `artifact.approved` and `artifact.changes_requested`: a human review of a document,
 *  pinned to `version`; `reason` is required for changes requested; `ask_id` names the
 *  approval ask the review answered, null when given from the document header. */
export interface ArtifactReviewEventPayload {
  readonly artifact_id: string;
  readonly name: string;
  readonly version: number;
  readonly actor: Actor;
  readonly reason: string | null;
  readonly ask_id: string | null;
}

/** A server-owned typed-block attribute was restored from its indexed row. */
export interface BlockRepairedEventPayload {
  readonly block_id: string;
  readonly version: number;
  readonly disturbed_by: Actor;
}

/** A browser-authored ask block is malformed and awaits repair. */
export interface BlockInvalidEventPayload {
  readonly block_id: string;
  readonly version: number;
  readonly reason: string;
  readonly disturbed_by: Actor;
}
export interface ChildStatusEventPayload {
  readonly child_key: string;
  readonly from: string;
  readonly to: string;
}

export interface ChildEventPayload {
  readonly child_key: string;
}

// SubscriptionRemovedEventPayload is the wire payload of subscription.removed:
// a human unsubscribed session_id from topics on this issue or document, and
// this record is both the audit trail and the direct notice routed to that
// session's own agent topic regardless of the issue's route.
export interface SubscriptionRemovedEventPayload {
  readonly session_id: string;
  readonly by: Actor;
  readonly topics: readonly string[];
  /** True only while Dispatch is retrying the listener-side removal. */
  readonly pending?: boolean;
  /** The remove-requested event this completion settles. */
  readonly request_event_id?: number;
}

/**
 * A session that receives an ask's answer, edits, resolution, and replies on its own
 * agent topic: the asker, every session that replied, and any session a human added.
 * A session may leave (or rejoin) only itself; a human may add or remove any session.
 */
export interface AskFollower {
  readonly session_id: string;
  readonly since: string;
}

// AskFollowerEventPayload is the wire payload of ask.follower_added and
// ask.follower_removed: which session joined or left which ask, and who did it. The
// removed event also reaches the removed session's own topic directly.
export interface AskFollowerEventPayload {
  readonly ask_id: string;
  readonly session_id: string;
  readonly by: Actor;
}

export interface RepoProjectUpdatedEventPayload {
  readonly mapping: RepoProject;
  readonly deleted: boolean;
}

export interface ArchitectureSourceUpdatedEventPayload {
  readonly source: ArchitectureSource;
  readonly deleted: boolean;
}

/** `architecture.synced`: the importer replaced the project's model. */
export interface ArchitectureSyncedEventPayload {
  readonly commit: string;
  readonly components: number;
}

/** `architecture.sync_failed`: the import was rejected or failed; the previous
 *  model stays up and the source row carries the same text in `last_error`. */
export interface ArchitectureSyncFailedEventPayload {
  readonly error: string;
}

export interface UserStateUpdatedEventPayload {
  readonly login: string;
  readonly state: UserIssueState;
}

interface DispatchEventBase {
  readonly id: number;
  readonly issue_key: string | null;
  readonly artifact_id?: string | null;
  readonly project?: string;
  readonly seq: number;
  readonly actor: Actor;
  readonly notify: boolean;
  readonly created_at: string;
}

export type DispatchEvent =
  | (DispatchEventBase & { readonly type: "project.created"; readonly payload: Project })
  | (DispatchEventBase & { readonly type: "project.updated"; readonly payload: Project })
  | (DispatchEventBase & {
      readonly type: "settings.repo_project.updated";
      readonly payload: RepoProjectUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "settings.architecture_source.updated";
      readonly payload: ArchitectureSourceUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "architecture.synced";
      readonly payload: ArchitectureSyncedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "architecture.sync_failed";
      readonly payload: ArchitectureSyncFailedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "user_state.updated";
      readonly payload: UserStateUpdatedEventPayload;
    })
  | (DispatchEventBase & { readonly type: "issue.created"; readonly payload: Issue })
  | (DispatchEventBase & { readonly type: "issue.updated"; readonly payload: Issue })
  | (DispatchEventBase & { readonly type: "issue.closed"; readonly payload: Issue })
  | (DispatchEventBase & {
      readonly type: "artifact.created";
      readonly payload: ArtifactCreatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.version";
      readonly payload: ArtifactVersionEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.approved";
      readonly payload: ArtifactReviewEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.changes_requested";
      readonly payload: ArtifactReviewEventPayload;
    })
  | (DispatchEventBase & { readonly type: "ask.opened"; readonly payload: Ask })
  | (DispatchEventBase & {
      readonly type: "ask.anchor_refreshed";
      readonly payload: Ask;
    })
  | (DispatchEventBase & {
      readonly type: "ask.edited";
      readonly payload: AskEditEventPayload;
    })
  | (DispatchEventBase & { readonly type: "ask.answered"; readonly payload: Ask })
  | (DispatchEventBase & {
      readonly type: "ask.resolved";
      readonly payload: Ask & { readonly state: "resolved"; readonly resolution: AskResolution };
    })
  | (DispatchEventBase & {
      readonly type: "block.repaired";
      readonly payload: BlockRepairedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "block.invalid";
      readonly payload: BlockInvalidEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.created";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.anchor_refreshed";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.delivery";
      readonly payload: CommentDeliveryEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.answered";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.resolved";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.reopened";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "comment.edited";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "suggestion.accepted";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "suggestion.rejected";
      readonly payload: CommentEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "message.created";
      readonly payload: MessageEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "message.delivery";
      readonly payload: MessageDeliveryEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "message.answered";
      readonly payload: MessageEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "child.status";
      readonly payload: ChildStatusEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "child.added";
      readonly payload: ChildEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "child.removed";
      readonly payload: ChildEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "subscription.remove_requested";
      readonly payload: SubscriptionRemovedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "subscription.removed";
      readonly payload: SubscriptionRemovedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.follower_added";
      readonly payload: AskFollowerEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.follower_removed";
      readonly payload: AskFollowerEventPayload;
    });

export type Event = DispatchEvent;
export type EventType = DispatchEvent["type"];

export interface EditOp {
  readonly op: DocEditOp;
  readonly find?: string;
  readonly with?: string;
  readonly occurrence?: number;
  readonly markdown?: string;
  readonly after?: string;
  readonly before?: string;
  readonly block?: string;
  readonly type?: string;
  readonly attributes?: Readonly<Record<string, unknown>>;
}

export interface AuthenticatedUser {
  readonly kind: "user";
  readonly login: string;
}

export interface UserIssueState {
  readonly pinned: boolean;
  readonly last_read_seq: number;
  readonly dismissed: string[];
}

export type UserState = Record<string, UserIssueState>;

/** A viewer's Clear on one agent's conversation: exchanges whose newest message is at or
 * before `cleared_before` (RFC3339) are hidden for that viewer only. */
export interface UserAgentState {
  readonly cleared_before: string;
}

/** `GET /api/v1/me/agents/state`: keyed by session ID. */
export type UserAgentStates = Record<string, UserAgentState>;

export interface CreateProjectInput {
  readonly key: string;
  readonly name: string;
  readonly actor?: Actor;
}

export interface CreateIssueInput {
  readonly project: string;
  readonly title: string;
  readonly parent?: string;
  readonly external?: string;
  readonly spec?: string;
  readonly force?: boolean;
  readonly labels?: string[];
  readonly priority?: IssuePriority | null;
  /** An allowlisted login; omitted, the server picks the creating human, the personal token's
   *  owner, or the parent's assignee. */
  readonly assignee?: string;
  /** The issue's own component attachment; omitted, it inherits its parent's. */
  readonly components?: IssueComponentsInput;

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
  /** An allowlisted login, or null to unassign; omitted leaves the assignee alone. */
  readonly assignee?: string | null;
  /** A parent issue key in the same project, or null to clear; omitted leaves it alone. */
  readonly parent?: string | null;
  /** The issue's own component attachment; null (or mode `inherit`) deletes it so the issue
   *  inherits again; omitted leaves it alone. Allowed on a closed issue. */
  readonly components?: IssueComponentsInput | null;
  readonly actor?: Actor;
}

/** One row of GET /api/v1/users: a login on the sign-in allowlist. */
export interface DispatchUser {
  readonly login: string;
}

export interface ListUsersResponse {
  readonly users: DispatchUser[];
}

/** GET /api/v1/whoami: a human by display-cased login, or an agent with its personal token's
 *  owner (lowercase) — null under the shared token. */
export type WhoamiResponse =
  | { readonly kind: "user"; readonly login: string }
  | { readonly kind: "agent"; readonly owner: string | null };

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
}

export interface CreateMessageInput {
  readonly body: string;
  readonly in_reply_to?: string;
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
  readonly actor?: Actor;
}

export interface IssueDetails extends Issue {
  readonly artifacts: Artifact[];
  readonly open_asks: Ask[];
  readonly children: IssueChild[];
}

export interface ArtifactDetails extends Artifact {
  readonly referenced_by: ReferencedBy[];
}

export interface CommentRead {
  readonly comment: Comment;
  readonly replies: Comment[];
}

export interface AskRead {
  readonly ask: Ask;
  readonly replies: Comment[];
  /** Every rewording of the question, oldest first; empty when never edited. */
  readonly edits: AskEdit[];
  /** The sessions this ask's answer and replies reach directly, oldest first. */
  readonly followers: AskFollower[];
}

export interface AskFollowersRead {
  readonly followers: AskFollower[];
}

export interface MessageRead {
  readonly message: Message;
  readonly replies: Message[];
}

export interface IssueRead {
  readonly issue: IssueDetails;
  readonly events: Event[];
}

export interface ArtifactText {
  readonly markdown: string;
  readonly version: number | null;
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
}

export interface EditArtifactResponse {
  readonly applied: number;
  readonly version: Version | null;
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
}

/**
 * Tolerantly validates the Dispatch event framing delivered by Envoy. Payload
 * content is parsed per event type so malformed optional display fields do not
 * discard a valid event header.
 */
export const DispatchEventSchema = z.object({
  issue_key: z.string().nullable(),
  artifact_id: z.string().nullish(),
  project: z.string().optional(),
  type: z.string(),
  actor: z.object({ kind: z.string(), id: z.string().optional() }).passthrough(),
  payload: z.object({}).passthrough(),
});

export type InboundDispatchEvent = z.infer<typeof DispatchEventSchema>;

export const IssueEventPayloadSchema = z.object({
  title: z.string().optional(),
  labels: z.array(z.string()).optional(),
  status: z.string().optional(),
  priority: z.number().int().min(0).max(3).nullable().optional(),
  rank: z.string().optional(),
  route: z.string().nullish(),
});

export const ArtifactCreatedEventPayloadSchema = z.object({
  artifact: z
    .object({ id: z.string().optional(), slug: z.string().optional(), name: z.string().optional() })
    .optional(),
});

export const ArtifactVersionEventPayloadSchema = z.object({
  artifact_id: z.string().optional(),
  name: z.string().optional(),
  version: z.object({ number: z.number().optional(), summary: z.string().nullish() }).optional(),
  diff: z.string().optional(),
});

export const ArtifactReviewEventPayloadSchema = z.object({
  artifact_id: z.string().optional(),
  name: z.string().optional(),
  version: z.number().int().optional(),
  actor: z.object({ kind: z.string(), id: z.string() }).passthrough().optional(),
  reason: z.string().nullish(),
  ask_id: z.string().nullish(),
});

const askEventPayloadFields = {
  id: z.string().optional(),
  opened_event_id: z.number().int().positive(),
  // Inbound only: JetStream retains ask.* envelopes for 72 h, so a renderer may still see a
  // kind the server no longer writes (the removed `action`). The outbound `AskKind` stays narrow.
  kind: z.string().optional(),
  question: z.string().optional(),
  options: z.array(z.object({ label: z.string().optional() })).optional(),
  answer: z
    .object({ selected: z.array(z.string()).nullish(), text: z.string().nullish() })
    .nullish(),
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
      kind: z.enum(["retracted", "resolved"]).optional(),
      reason: z.string().optional(),
      actor: z
        .object({ kind: z.string().optional(), id: z.string().optional() })
        .passthrough()
        .optional(),
      at: z.string().optional(),
    })
    .nullish(),
};

export const AskEventPayloadSchema = z.intersection(
  z.object(askEventPayloadFields),
  z.object({
    previous: z.never().optional(),
    edited_by: z.never().optional(),
  })
);

export const AskEditedEventPayloadSchema = z.object({
  ...askEventPayloadFields,
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

const CommentMentionSchema = z.object({
  target: z.string(),
  delivery: z.enum(DELIVERY_CAPABILITIES),
  session_id: z.string().nullable(),
});

const CommentDeliverySchema = z.object({
  comment_id: z.string(),
  target: z.string(),
  attempt: z.number().int().positive(),
  delivery: z.enum(DELIVERY_CAPABILITIES),
  session_id: z.string().nullable(),
  envelope_id: z.string().nullable(),
  state: z.enum(["pending", "sent", "failed"]),
  error: z.string().nullable(),
  resolve_error: z.string().nullable(),
  reply_id: z.string().nullable(),
  created_at: z.string(),
});

export const CommentEventPayloadSchema = z
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
    mentions: z.array(CommentMentionSchema).optional(),
    deliveries: z.array(CommentDeliverySchema).optional(),
  })
  // Bus frames are untrusted and this schema declares only the fields Dispatch
  // clients are known to read today, but the wire payload always carries every
  // field the server model has. .passthrough() keeps a field this schema hasn't
  // caught up to riding along instead of silently vanishing when a consumer that
  // reads it is added later — the failure mode that dropped ask_waiting_on/turn
  // from a comment.created reply without any test catching it.
  .passthrough();

export const MessageEventPayloadSchema = z.object({
  id: z.string().optional(),
  body: z.string().optional(),
  target: z.string().nullish(),
  in_reply_to: z.string().nullish(),
  reply_body: z.string().optional(),
  author: z.object({ kind: z.string(), id: z.string() }).optional(),
});

export const DispatchTargetedResourceIDSchema = z.uuid();

export const DispatchTargetedMessagePayloadSchema = MessageEventPayloadSchema.extend({
  id: DispatchTargetedResourceIDSchema,
  issue_key: z.string().nullable(),
  author: z.object({ kind: z.string(), id: z.string() }),
  body: z.string(),
  target: z.string(),
  in_reply_to: z.string().nullable(),
  deliveries: z.array(z.unknown()),
  created_at: z.string(),
});

export const DispatchTargetedCommentPayloadSchema = CommentEventPayloadSchema.extend({
  id: DispatchTargetedResourceIDSchema,
  issue_key: z.string().nullable(),
  artifact_id: z.string().nullable(),
  author: z.object({ kind: z.string(), id: z.string() }),
  body: z.string(),
  reply_to: z.string().nullable(),
  ask_id: z.string().nullable(),
  mentions: z.array(CommentMentionSchema),
  deliveries: z.array(CommentDeliverySchema),
  created_at: z.string(),
  artifact_name: z.string(),
});

export const DispatchTargetedMessageDeliverySchema = z.object({
  attempt: z.number().int().positive(),
  mode: z.enum(DELIVERY_CAPABILITIES),
});

export const DispatchTargetedCommentDeliverySchema = DispatchTargetedMessageDeliverySchema.extend({
  comment_id: DispatchTargetedResourceIDSchema,
  target: z.string(),
});

export const DispatchTargetedDeliverySchema = z.union([
  DispatchTargetedCommentDeliverySchema,
  DispatchTargetedMessageDeliverySchema,
]);

export const MessageDeliveryEventPayloadSchema = z.object({
  message_id: z.string().optional(),
  attempt: z.number().int().positive().optional(),
  delivery: z.enum(DELIVERY_CAPABILITIES).optional(),
  session_id: z.string().optional(),
  target: z.string().optional(),
  title: z.string().optional(),
  state: z.enum(["sent", "failed"]).optional(),
  error: z.string().optional(),
});

export const ChildStatusEventPayloadSchema = z.object({
  child_key: z.string().optional(),
  from: z.string().optional(),
  to: z.string().optional(),
});

export const SubscriptionRemovedEventPayloadSchema = z.object({
  session_id: z.string().optional(),
  by: z.object({ kind: z.string(), id: z.string().optional() }).passthrough().optional(),
  topics: z.array(z.string()).optional(),
  pending: z.boolean().optional(),
  request_event_id: z.number().int().positive().optional(),
});

export const AskFollowerEventPayloadSchema = z.object({
  ask_id: z.string().optional(),
  session_id: z.string().optional(),
  by: z.object({ kind: z.string(), id: z.string().optional() }).passthrough().optional(),
});
