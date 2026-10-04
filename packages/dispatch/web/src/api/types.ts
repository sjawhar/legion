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
  ArtifactRebuildReport,
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

// Dispatch-UI-only DTOs mirroring the credential broker's JSON verbatim (its UI routes, in the
// broker's HTTP API reference: https://sjawhar.github.io/legion/broker/reference/api/). These have
// no reason to live in the shared @legion/contracts package, which is for Envoy event contracts.
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
  /** A pod enrollment's slot: one of several independent identities in one pod, chosen by the
   *  launcher that enrolled it. Null for every enrollment without one. */
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

interface CredentialGrantFields {
  grant_id: string;
  enrollment: CredentialEnrollment;
  names: string[];
  expires_at: string;
  created_at: string;
}
/** A grant the policy gave a session the viewer operates without asking: no one approved it, so it
 *  has no approver and rests on no record. */
export interface AutomaticCredentialGrant extends CredentialGrantFields {
  granted: "automatic";
  approver: null;
  record_id: null;
}
/** A grant someone approved: `approver` names the login that did (another person's, on a session
 *  the viewer operates), and `record_id` the record the approval rests on. */
export interface ApprovedCredentialGrant extends CredentialGrantFields {
  granted: "approval";
  approver: string;
  record_id: string;
}
/** A live grant on `GET /api/v1/credential-grants?approver=me`: one on a session the viewer
 *  operates, given automatically or approved by anyone, or one the viewer approved on anyone's
 *  session. */
export type CredentialGrant = AutomaticCredentialGrant | ApprovedCredentialGrant;
export interface CredentialGrantsResponse {
  grants: CredentialGrant[];
}

/** One of the machine logins the viewer approved on `GET /api/v1/machine-logins`: the launcher
 *  credential it minted, for one of the viewer's machines or for a service, not revoked, and
 *  either unexpired or expired with a session it started still running. */
export interface MachineLogin {
  credential_id: string;
  host: string;
  /** The service a service's login is for (`legion-daemon`), whose sessions are its worker pods;
   *  null for the viewer's own machine. */
  service: string | null;
  issued_at: string;
  expires_at: string;
  /** True once `expires_at` has passed: the login starts no more sessions, but sessions it
   *  started renew with their own keys and still run until they end or it is revoked. */
  expired: boolean;
}
export interface MachineLoginsResponse {
  credentials: MachineLogin[];
}
