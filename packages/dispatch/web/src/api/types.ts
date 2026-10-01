export type {
  Actor,
  ActorOrigin,
  Agent,
  AgentToken,
  Anchor,
  AnchorInput,
  AnswerAskInput,
  ArchitectureSource,
  ArchitectureTree,
  ArchitectureTreeComponent,
  ArchitectureTreeIssue,
  ArchitectureTreeIssueRef,
  ArchitectureTreeNone,
  ArchitectureTreeRetired,
  ArchitectureTreeSource,
  Artifact,
  ArtifactApproval,
  ArtifactBlock,
  ArtifactReview,
  ArtifactReviewEventPayload,
  ArtifactReviewState,
  ArtifactText,
  ArtifactUploadResponse,
  ArtifactVersionBlob,
  ArtifactVersionContent,
  ArtifactVersionText,
  Ask,
  AskAnchorArtifact,
  AskAnswer,
  AskBlockArtifact,
  AskEdit,
  AskEditPrevious,
  AskFollower,
  AskLastReply,
  AskOption,
  AskRead,
  AskResolution,
  AskSnooze,
  AskTurn,
  AskUrgency,
  AuthenticatedUser,
  BlockAttributeKind,
  BlockAttributeSchema,
  BlockContentRule,
  BlockSchema,
  BlockTypeSchema,
  Broadcast,
  BroadcastCreated,
  BroadcastExclusion,
  BroadcastRead,
  BroadcastRecipient,
  BroadcastSummary,
  ChangedReference,
  ChildStatusEventPayload,
  Comment,
  CommentDelivery,
  CommentDeliveryEventPayload,
  CommentEventPayload,
  CommentMention,
  CommentRead,
  CreateAgentMessageInput,
  CreateArtifactInput,
  CreateAskInput,
  CreateBroadcastInput,
  CreateCommentInput,
  CreatedAgentToken,
  CreateIssueInput,
  CreateMessageInput,
  CreateProjectInput,
  CreateVersionInput,
  DeliveryCapability,
  DispatchEvent,
  DispatchUser,
  DocEditOp,
  DuplicateCandidate,
  EditAskInput,
  EditCommentInput,
  EditOp,
  Event,
  EventType,
  ExternalLink,
  GraphEdge,
  GraphReferences,
  InboxDocument,
  InboxRow,
  Issue,
  IssueChild,
  IssueClaim,
  IssueClaimEventPayload,
  IssueComponents,
  IssueComponentsInput,
  IssueDetails,
  IssuePriority,
  IssueRankInput,
  IssueRead,
  IssueReferences,
  IssueRouteReach,
  IssueSummary,
  ListUsersResponse,
  Message,
  MessageDelivery,
  MessageDeliveryEventPayload,
  MessageDeliveryMode,
  MessageEventPayload,
  MessageRead,
  Project,
  RepoProject,
  SearchArtifactRef,
  SearchOwner,
  SearchResponse,
  SearchResult,
  SearchResultKind,
  Subscriber,
  SubscriptionRemovedEventPayload,
  Suggestion,
  TargetCandidate,
  UpdateIssueInput,
  UserAgentState,
  UserAgentStateInput,
  UserAgentStates,
  UserIssueState,
  UserState,
  Version,
} from "@legion/contracts";

// Dispatch-UI-only DTOs mirroring the credential broker's JSON verbatim (contract v9's "UI
// routes" section). These have no reason to live in the shared @legion/contracts package,
// which is for Envoy event contracts.
export type CredentialRequestKind = "agent_secret" | "launcher_credential";
export type CredentialRequestState =
  | "pending"
  | "approved"
  | "denied"
  | "expired"
  | "cancelled"
  | "revoked";

export interface CredentialPendingRow {
  record_id: string;
  kind: CredentialRequestKind;
  identifiers: string[]; // secret rule keys, or [hostname] for a launcher_credential
  requested_at: string; // RFC3339
}
export interface CredentialPendingResponse {
  pending: CredentialPendingRow[];
}

export interface CredentialEnrollment {
  kind: string;
  runtime_id: string;
  operator: string | null;
  /** A pod enrollment's slot: one of several independent identities in one pod, which Legion
   *  names `<role>-g<generation>`. Null for every enrollment without one. */
  slot: string | null;
}
export interface CredentialDecisionEvent {
  event: string; // "approved" | "denied" | "expired" | "cancelled" | "revoked"
  at: string;
  credential_id: string | null;
}
export interface CredentialRecord {
  record_id: string;
  kind: CredentialRequestKind;
  state: CredentialRequestState;
  approver: string;
  enrollment: CredentialEnrollment | null;
  identifiers: string[];
  service: string | null;
  reason: string;
  lifetime_seconds: number;
  rules_version: string;
  expires_at: string;
  requested_at: string;
  decided: CredentialDecisionEvent | null;
}

export interface CredentialApproval {
  state: "approved";
  grant_id: string | null;
  credential_id: string | null;
}
export interface CredentialDenial {
  state: "denied";
}
/** What approve and deny answer, relayed verbatim from the broker: an approval names the grant or
 *  the launcher credential it made, and a denial is `{state: "denied"}` alone. */
export type CredentialDecisionResponse = CredentialApproval | CredentialDenial;

/** A live grant on `GET /api/v1/credential-grants?approver=me`: one the viewer approved, or one on
 *  an enrollment the viewer operates, which `approver` may name as another login. */
export interface CredentialGrant {
  grant_id: string;
  record_id: string;
  enrollment: CredentialEnrollment;
  names: string[];
  approver: string;
  expires_at: string;
  created_at: string;
}
export interface CredentialGrantsResponse {
  grants: CredentialGrant[];
}
