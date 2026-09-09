export interface ActorOrigin {
  host?: "omp" | "opencode" | "claude";
  machine?: string;
  cwd?: string;
  tmux?: string;
  pane?: string;
  session_title?: string;
}

export type Actor =
  | { kind: "user"; id: string }
  | { kind: "session"; id: string; origin?: ActorOrigin };

export interface Anchor {
  artifact_id: string;
  version: number;
  quote: string;
  from: number;
  to: number;
  orphaned: boolean;
}

export type AnchorInput =
  | { artifact: string; quote: string; occurrence?: number }
  | { artifact: string; from: number; to: number };

export interface Project {
  key: string;
  name: string;
}

export interface ExternalLink {
  url: string;
  kind?: "github_issue" | "github_pr" | "url";
}

export interface Issue {
  key: string;
  project: string;
  number: number;
  title: string;
  status: string;
  labels: string[];
  parent: string | null;
  external_links: ExternalLink[];
  route: string | null;
  created_by: Actor;
  created_at: string;
  updated_at: string;
  closed_at: string | null;
  primary_artifact_id: string;
  last_seq: number;
  artifacts?: Artifact[];
  open_asks?: Ask[];
  children?: IssueChild[];
}

export interface IssueChild {
  key: string;
  title: string;
  status: string;
}

export interface IssueSummary {
  key: string;
  title: string;
  status: string;
  parent: string | null;
  updated_at: string;
  open_asks: number;
}

export interface Version {
  number: number;
  named: boolean;
  summary: string | null;
  authors: Actor[];
  created_at: string;
  size?: number;
  mime?: string;
  sha256?: string;
}

export interface Artifact {
  id: string;
  issue_key: string;
  slug: string;
  name: string;
  kind: "doc" | "image" | "file";
  primary: boolean;
  created_by: Actor;
  created_at: string;
  versions: Version[];
  referenced_by?: ReferencedBy[];
}

export interface ReferencedBy {
  kind: "ask" | "comment" | "message";
  id: string;
  issue_key: string;
  excerpt: string;
}

export interface AskOption {
  label: string;
  description?: string;
}

export interface AskAnswer {
  user: string;
  selected: string[];
  text: string | null;
  at: string;
}

export interface Ask {
  id: string;
  issue_key: string;
  author: Actor;
  question: string;
  options: AskOption[];
  multiple: boolean;
  custom: boolean;
  urgency: "low" | "med" | "high" | "blocking";
  anchor: Anchor | null;
  state: "open" | "answered";
  answer: AskAnswer | null;
  created_at: string;
  issue?: Pick<Issue, "key" | "title">;
}

export interface Suggestion {
  replace_with: string;
  accepted: boolean | null;
}

export interface Comment {
  id: string;
  issue_key: string;
  author: Actor;
  body: string;
  anchor: Anchor | null;
  reply_to: string | null;
  resolved: boolean;
  suggestion: Suggestion | null;
  created_at: string;
}

export interface Message {
  id: string;
  issue_key: string;
  author: Actor;
  body: string;
  created_at: string;
}

export type EventType =
  | "issue.created"
  | "issue.updated"
  | "issue.closed"
  | "artifact.created"
  | "artifact.version"
  | "ask.opened"
  | "ask.answered"
  | "comment.created"
  | "comment.resolved"
  | "suggestion.accepted"
  | "suggestion.rejected"
  | "message.created"
  | "child.status";

export interface Event {
  id: number;
  issue_key: string;
  seq: number;
  type: EventType;
  actor: Actor;
  notify: boolean;
  created_at: string;
  payload: Record<string, unknown>;
}

export interface EditOp {
  op: "replace" | "delete" | "insert";
  find?: string;
  with?: string;
  occurrence?: number;
  markdown?: string;
  after?: string;
  before?: string;
}

export interface AuthenticatedUser {
  login: string;
}

export interface UserIssueState {
  pinned: boolean;
  last_read_seq: number;
  dismissed: string[];
}

export type UserState = Record<string, UserIssueState>;

export interface CreateIssueInput {
  project: string;
  title: string;
  parent?: string;
  /** External issue reference as owner/repo#n; the server links it on create. */
  external?: string;
  spec?: string;
  actor?: Actor;
}

export interface UpdateIssueInput {
  title?: string;
  status?: string;
  labels?: string[];
  route?: string | null;
  external_links?: ExternalLink[];
  actor?: Actor;
}

export interface CreateAskInput {
  question: string;
  options?: AskOption[];
  multiple?: boolean;
  custom?: boolean;
  urgency?: Ask["urgency"];
  anchor?: AnchorInput;
  actor?: Actor;
}

export interface AnswerAskInput {
  selected: string[];
  text?: string;
}

export interface CreateCommentInput {
  body: string;
  anchor?: AnchorInput;
  reply_to?: string;
  suggestion?: { replace_with: string };
  actor?: Actor;
}

export interface CreateMessageInput {
  body: string;
  actor?: Actor;
}

export interface CreateArtifactInput {
  name: string;
  primary?: boolean;
  summary?: string;
  actor?: Actor;
  file: File;
}

export interface CreateVersionInput {
  summary: string;
  actor?: Actor;
}

export interface EditArtifactInput {
  ops: EditOp[];
  summary?: string;
  actor?: Actor;
}

export interface ArtifactVersionText {
  markdown: string;
  version: number | null;
}

export interface ArtifactVersionBlob {
  blob: Blob;
  mime: string;
  version: number;
}

export type ArtifactVersionContent = ArtifactVersionText | ArtifactVersionBlob;
