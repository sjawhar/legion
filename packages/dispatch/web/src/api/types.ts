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
}
export interface CredentialDecisionEvent {
  event: string; // "approved" | "denied" | "expired" | "cancelled" | "revoked"
  at: string;
  credential_id: string | null;
}
export interface CredentialChallenges {
  approve: string; // base64url
  deny: string; // base64url
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
  challenges: CredentialChallenges | null; // present only while pending, never for a machine record
}

export interface CredentialDecisionResponse {
  state: "approved" | "denied";
  grant_id: string | null;
  credential_id: string | null;
}

export interface CredentialKey {
  credential_id: string;
  aaguid: string;
  registered_at: string;
  last_used_at: string | null;
  state: "active" | "tombstoned" | "revoked";
  endorsed_by: string | null;
  seeded: boolean;
}
export interface CredentialKeysResponse {
  keys: CredentialKey[];
}

export interface CredentialGrant {
  grant_id: string;
  record_id: string;
  enrollment: CredentialEnrollment;
  names: string[];
  expires_at: string;
  created_at: string;
}
export interface CredentialGrantsResponse {
  grants: CredentialGrant[];
}

// Raw WebAuthn JSON shapes as go-webauthn's `protocol` package emits/consumes them (field names
// are load-bearing - they must match exactly what the broker parses). Kept permissive (the
// fields navigator.credentials actually needs) rather than modeling every optional extension.
export interface PublicKeyCredentialCreationOptionsJSON {
  rp: { id?: string; name: string };
  user: { id: string; name: string; displayName: string }; // id is base64url
  challenge: string; // base64url
  pubKeyCredParams: { type: "public-key"; alg: number }[];
  authenticatorSelection?: { userVerification?: string; residentKey?: string };
  attestation?: string;
  timeout?: number;
  excludeCredentials?: { id: string; type: "public-key" }[];
}
export interface PublicKeyCredentialRequestOptionsJSON {
  challenge: string; // base64url
  rpId?: string;
  allowCredentials?: { id: string; type: "public-key" }[];
  userVerification?: string;
  timeout?: number;
}
export interface AuthenticationResponseJSON {
  id: string;
  rawId: string;
  type: "public-key";
  response: {
    clientDataJSON: string;
    authenticatorData: string;
    signature: string;
    userHandle?: string;
  };
}
export interface RegistrationResponseJSON {
  id: string;
  rawId: string;
  type: "public-key";
  response: {
    clientDataJSON: string;
    attestationObject: string;
  };
}

export interface CredentialCeremonyBeginResponse {
  ceremony_id: string;
  publicKey: PublicKeyCredentialCreationOptionsJSON | PublicKeyCredentialRequestOptionsJSON;
}
export interface CredentialCeremonyFinishResponse {
  yaml: string;
}
