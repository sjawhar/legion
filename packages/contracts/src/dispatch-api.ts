import { z } from "zod";
import type { ASK_URGENCIES, DOC_EDIT_OPS } from "./dispatch-tools";

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
    };

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

export interface ExternalLink {
  readonly url: string;
  readonly kind?: string;
}

export interface Issue {
  readonly key: string;
  readonly project: string;
  readonly number: number;
  readonly title: string;
  readonly status: string;
  readonly rank: string;
  readonly labels: string[];
  readonly parent: string | null;
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
  extends Pick<Issue, "key" | "title" | "status" | "rank" | "parent" | "updated_at" | "last_seq"> {
  readonly labels?: string[];
  readonly open_asks: number;
}

export interface IssueChild {
  readonly key: string;
  readonly title: string;
  readonly status: string;
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

export interface Ask {
  readonly id: string;
  readonly issue_key: string | null;
  readonly artifact_id?: string | null;
  readonly author: Actor;
  /** `approval` asks are opened by an approval request; their options are fixed
   *  (`Approve`, `Request changes`) and their answer writes a document review. */
  readonly kind: "question" | "approval";
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
  readonly issue?: Pick<Issue, "key" | "title">;
  readonly document?: InboxDocument;
  /** Inbox rows only: the newest reply in the ask's thread, or null when nobody has replied.
   *  A human reply on an open ask means the asker owes the next turn (a clarification). */
  readonly last_reply?: AskLastReply | null;
  readonly edited_at: string | null;
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

export interface Comment {
  readonly id: string;
  readonly issue_key: string | null;
  readonly artifact_id?: string | null;
  readonly author: Actor;
  readonly body: string;
  readonly anchor: Anchor | null;
  readonly reply_to: string | null;
  readonly ask_id: string | null;
  readonly resolved: boolean;
  readonly resolved_by: Actor | null;
  readonly resolved_at: string | null;
  readonly edited_at: string | null;
  readonly suggestion: Suggestion | null;
  readonly created_at: string;
}

export interface Suggestion {
  readonly replace_with: string;
  readonly accepted: boolean | null;
}

export interface CommentEventPayload extends Comment {
  readonly artifact_name: string;
  /** The question text of the ask this comment replies to (Comment.ask_id); empty otherwise. */
  readonly ask_question?: string;
  /** The state of that ask when the comment was posted. A human reply while it is still `open`
   *  is a request for clarification: the asker answers in the thread or rewords the question. */
  readonly ask_state?: Ask["state"];
}

export interface Message {
  readonly id: string;
  readonly issue_key: string;
  readonly author: Actor;
  readonly body: string;
  readonly reply_to: string | null;
  readonly created_at: string;
}

export interface MessageEventPayload extends Message {
  /** First 160 characters of the reply target's body (Message.reply_to); empty otherwise. */
  readonly reply_body?: string;
}

export type SearchResultKind = "issue" | "document" | "comment" | "ask" | "message";

export interface SearchIssueRef {
  readonly key: string;
  readonly title: string;
  readonly status: string;
}

export interface SearchArtifactRef {
  readonly slug: string;
  readonly name: string;
}

export type SearchOwner =
  | { readonly kind: "issue"; readonly key: string }
  | {
      readonly kind: "document";
      readonly project: string;
      readonly slug: string;
      readonly artifact_id: string;
      readonly name: string;
    };

export interface SearchResult {
  readonly kind: SearchResultKind;
  /**
   * Who owns the hit. Servers before project-document search omit it; `searchOwnerOf` derives
   * the issue owner from `issue` in that case.
   */
  readonly owner?: SearchOwner;
  /** Legacy issue-shaped display metadata, retained for existing consumers. */
  readonly issue: SearchIssueRef;
  readonly artifact?: SearchArtifactRef;
  readonly id: string;
  readonly snippet: string;
  readonly rank: number;
  readonly href: string;
}

/** The hit's owner, falling back to the issue-shaped fields an older server sends. */
export function searchOwnerOf(result: Pick<SearchResult, "owner" | "issue">): SearchOwner {
  return result.owner ?? { kind: "issue", key: result.issue.key };
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
  readonly capabilities: string[];
  readonly last_seen: number;
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

export interface ChildStatusEventPayload {
  readonly child_key: string;
  readonly from: string;
  readonly to: string;
}

// SubscriptionRemovedEventPayload is the wire payload of subscription.removed:
// a human unsubscribed session_id from topics on this issue or document, and
// this record is both the audit trail and the direct notice routed to that
// session's own agent topic regardless of the issue's route.
export interface SubscriptionRemovedEventPayload {
  readonly session_id: string;
  readonly by: Actor;
  readonly topics: readonly string[];
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
      readonly type: "ask.edited";
      readonly payload: AskEditEventPayload;
    })
  | (DispatchEventBase & { readonly type: "ask.answered"; readonly payload: Ask })
  | (DispatchEventBase & {
      readonly type: "ask.resolved";
      readonly payload: Ask & { readonly state: "resolved"; readonly resolution: AskResolution };
    })
  | (DispatchEventBase & {
      readonly type: "comment.created";
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
      readonly type: "child.status";
      readonly payload: ChildStatusEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "subscription.removed";
      readonly payload: SubscriptionRemovedEventPayload;
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
  readonly route?: string | null;
  readonly external_links?: ExternalLink[];
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
  readonly suggestion?: { readonly replace_with: string };
  readonly actor?: Actor;
}

export interface EditCommentInput {
  readonly body: string;
}

export interface CreateMessageInput {
  readonly body: string;
  readonly reply_to?: string;
  readonly actor?: Actor;
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
  status: z.string().optional(),
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
  question: z.string().optional(),
  options: z.array(z.object({ label: z.string().optional() })).nullish(),
  answer: z
    .object({ selected: z.array(z.string()).nullish(), text: z.string().nullish() })
    .nullish(),
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

export const CommentEventPayloadSchema = z.object({
  id: z.string().optional(),
  artifact_name: z.string().optional(),
  body: z.string().optional(),
  reply_to: z.string().nullish(),
  ask_id: z.string().nullish(),
  ask_question: z.string().optional(),
  ask_state: z.enum(["open", "answered", "resolved"]).optional(),
  anchor: z.object({ quote: z.string().optional() }).nullish(),
  suggestion: z.object({ replace_with: z.string().optional() }).nullish(),
  author: z.object({ kind: z.string(), id: z.string() }).optional(),
  created_at: z.string().optional(),
});

export const MessageEventPayloadSchema = z.object({
  id: z.string().optional(),
  body: z.string().optional(),
  reply_to: z.string().nullish(),
  reply_body: z.string().optional(),
  author: z.object({ kind: z.string(), id: z.string() }).optional(),
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
});
