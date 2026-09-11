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
  | { readonly kind: "session"; readonly id: string; readonly origin?: ActorOrigin };

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
  extends Pick<Issue, "key" | "title" | "status" | "parent" | "updated_at" | "last_seq"> {
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
  readonly edited_at: string | null;
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
}

export interface Message {
  readonly id: string;
  readonly issue_key: string;
  readonly author: Actor;
  readonly body: string;
  readonly created_at: string;
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

export interface SearchResult {
  readonly kind: SearchResultKind;
  readonly issue: SearchIssueRef;
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
  readonly capabilities: string[];
  readonly last_seen: number;
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

export interface ChildStatusEventPayload {
  readonly child_key: string;
  readonly from: string;
  readonly to: string;
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
  | (DispatchEventBase & { readonly type: "message.created"; readonly payload: Message })
  | (DispatchEventBase & {
      readonly type: "child.status";
      readonly payload: ChildStatusEventPayload;
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

export interface UpdateIssueInput {
  readonly title?: string;
  readonly status?: string;
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
  route: z.string().nullish(),
});

export const ArtifactCreatedEventPayloadSchema = z.object({
  artifact: z.object({ name: z.string().optional() }).optional(),
});

export const ArtifactVersionEventPayloadSchema = z.object({
  name: z.string().optional(),
  version: z.object({ number: z.number().optional(), summary: z.string().nullish() }).optional(),
  diff: z.string().optional(),
});

const askEventPayloadFields = {
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
  artifact_name: z.string().optional(),
  body: z.string().optional(),
  reply_to: z.string().nullish(),
  ask_id: z.string().nullish(),
  ask_question: z.string().optional(),
  anchor: z.object({ quote: z.string().optional() }).nullish(),
  suggestion: z.object({ replace_with: z.string().optional() }).nullish(),
});

export const MessageEventPayloadSchema = z.object({ body: z.string().optional() });

export const ChildStatusEventPayloadSchema = z.object({
  child_key: z.string().optional(),
  from: z.string().optional(),
  to: z.string().optional(),
});
