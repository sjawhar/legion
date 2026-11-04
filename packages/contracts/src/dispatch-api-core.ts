import { z } from "zod";
import type { ASK_URGENCIES, DOC_EDIT_OPS } from "./dispatch-tools";

/**
 * Who writes, what a write points at, and the records a project is made of: the shapes every
 * other area of the Dispatch API is built from.
 */

/**
 * A stored text read as the ProseMirror document a reader is shown (`pmdoc.ReadForDisplay`),
 * carried beside the markdown itself so a page renders what the server read rather than parsing
 * the markdown again. A response carries one only for a request that sent `Dispatch-Trees: 1`,
 * and every `<field>_tree` is absent otherwise; a `/ws/live` event frame carries its trees
 * always, since a browser can set no header on a socket.
 */
export type DisplayTree = {
  readonly type: string;
  readonly [key: string]: unknown;
};

/** `DisplayTree` for a schema that must hold one: a ProseMirror node, read as whatever its own
 *  node type names, so nothing the server's schema grows has to be re-declared here. */
export const DisplayTreeSchema: z.ZodType<DisplayTree> = z.looseObject({ type: z.string() });

export type AskUrgency = (typeof ASK_URGENCIES)[number];
export type DocEditOp = (typeof DOC_EDIT_OPS)[number];

export const ActorOriginSchema = z
  .strictObject({
    host: z.string().optional(),
    machine: z.string().optional(),
    cwd: z.string().optional(),
    tmux: z.string().optional(),
    pane: z.string().optional(),
    session_title: z.string().optional(),
  })
  .readonly();

export type ActorOrigin = z.output<typeof ActorOriginSchema>;

/** The OAuth scopes a Dispatch credential can hold, mirroring Go's `model.ScopePerson` and
 *  `model.ScopeApprove` and the `agent_tokens.scopes` check that stores them: `person` acts as
 *  the credential's own person on every route a person reaches through the dashboard, and
 *  `approve` settles what a person settles — a document review, the answer of an approval ask, a
 *  machine-login decision or revoke, and minting, changing or revoking a personal token. A
 *  credential holding `approve` holds `person` too. */
export const DISPATCH_SCOPES = ["person", "approve"] as const;
export type DispatchScope = (typeof DISPATCH_SCOPES)[number];

/** The agent that made an act for the person it acts for: the session a broker-issued token
 *  proved, and the personal token presented, by id and name. It is absent wherever the person
 *  acted themselves, which is every act made at a browser.
 *
 *  `scopes` is the set that credential held at the act, recorded with it and never read back
 *  from the token: a person who narrows a token afterwards narrows what it may do next, and
 *  does not un-approve what it approved while it held `approve`. */
export const ActorActSchema = z
  .strictObject({
    session: z.string().optional(),
    token_id: z.string().optional(),
    token_name: z.string().optional(),
    scopes: z.array(z.enum(DISPATCH_SCOPES)).optional(),
  })
  .readonly();

export type ActorAct = z.output<typeof ActorActSchema>;

export const ActorSchema = z.union([
  z
    .strictObject({
      kind: z.literal("user"),
      id: z.string(),
      act: ActorActSchema.optional(),
    })
    .readonly(),
  z
    .strictObject({
      kind: z.literal("session"),
      id: z.string(),
      origin: ActorOriginSchema.optional(),
      owner: z.string().optional(),
      /** The subject of the verified service token that authenticated the write
       *  (`system:serviceaccount:<namespace>:<name>`); absent under any other bearer. */
      service: z.string().optional(),
      /** The one project key the write's credential may write. The server reads it from an
       *  access token the secrets broker issued for that project and never from a body, as it
       *  reads `service` from a service token; it is absent under every credential that names
       *  none, a browser cookie, a personal token and a broker token scoped to its person
       *  included. */
      project: z.string().optional(),
    })
    .readonly(),
  /** A server-owned writer, such as the architecture importer
   *  (`{kind: "system", id: "architecture-importer"}`). */
  z.strictObject({ kind: z.literal("system"), id: z.string() }).readonly(),
]);

export type Actor = z.output<typeof ActorSchema>;

/**
 * An actor as a bus frame carries it, in the three readings the inbound parsers need.
 *
 * `kind` is an open string in each: JetStream retains a frame for 72 hours, so a reader may still
 * see an actor kind the server no longer writes, and a renderer that refuses the frame over it
 * drops an event it could have shown. `InboundActorSchema` is a comment's or a message's author,
 * whose extra fields no reader of these frames carries; `InboundWriterSchema` keeps the fields it
 * does not declare, as the payloads holding it do; `InboundBySchema` is the writer of a record
 * whose id a frame may omit.
 */
export const InboundActorSchema = z.object({ kind: z.string(), id: z.string() });

export const InboundWriterSchema = InboundActorSchema.passthrough();

export const InboundBySchema = z
  .object({ kind: z.string(), id: z.string().optional() })
  .passthrough();

/** How a verified service token's subject reads wherever an actor is shown.
 *
 *  A Kubernetes subject — exactly `system:serviceaccount:<namespace>:<name>` —
 *  renders `<namespace>/<name>`, because the namespace is the half that
 *  disambiguates: every namespace has a `default` service account, so the name
 *  alone names no one once a second namespace authenticates. Every other
 *  subject renders whole. Truncating a subject whose shape is unknown is the
 *  mistake this rule exists to undo, so the fallback shows all of it.
 *
 *  The persisted actor always carries the full subject; this is presentation. */
export function serviceSubjectLabel(subject: string): string {
  const parts = subject.split(":");
  if (
    parts.length === 4 &&
    parts[0] === "system" &&
    parts[1] === "serviceaccount" &&
    parts[2] !== "" &&
    parts[3] !== ""
  ) {
    return `${parts[2]}/${parts[3]}`;
  }
  return subject;
}

export interface AgentToken {
  readonly id: string;
  readonly name: string;
  readonly prefix: string;
  /** The OAuth scopes the token holds (`DISPATCH_SCOPES`). A token holding `approve` holds
   *  `person` too; `[]` is a token that acts as nobody. */
  readonly scopes: DispatchScope[];
  readonly created_at: string;
  readonly last_used_at: string | null;
  readonly revoked_at: string | null;
}

export interface CreatedAgentToken extends AgentToken {
  readonly token: string;
}

/** `POST /api/v1/me/agent-tokens`: the new token's name, and the scopes it is to hold. Omitting
 *  `scopes` gives it the set the minting credential holds itself; `[]` gives it none. A scope the
 *  minter does not hold is 403 OAUTH_SCOPE_REQUIRED, and `approve` without `person` is 400
 *  TOKEN_SCOPES_INPUT. The answer is the one time the token's plaintext is shown. */
export interface CreateAgentTokenInput {
  readonly name: string;
  readonly scopes?: DispatchScope[];
}

/** `PATCH /api/v1/me/agent-tokens/{id}`: the whole set the token is to hold, with the mint's
 *  refusals. It is required, since it replaces the set rather than adding to it. */
export interface UpdateAgentTokenScopesInput {
  readonly scopes: DispatchScope[];
}

/** What a credential request's kind is, as the secrets broker names it: a secret an agent asked
 *  its person for, or a machine login a launcher asked to enrol. */
export type CredentialRequestKind = "agent_secret" | "launcher_credential";

/** A credential request's two possibly-differing session ids, read independently by the broker:
 *  `request` is the id the request itself stated (an unsigned override in its body at create
 *  time), `enrollment` is the id its requesting enrollment stated when it enrolled. Either is
 *  null when that id was never set; a machine login (`launcher_credential`, which has no request
 *  row and no requesting enrollment) always answers both null. Optional so an older broker's
 *  response, with no `session` field at all, resolves the same as a request that names neither
 *  id. */
export interface CredentialSession {
  readonly request: string | null;
  readonly enrollment: string | null;
}

/** One row of `GET /api/v1/credential-requests?approver=me`: a request the caller may decide,
 *  as the dashboard's Inbox and `dispatch inbox` both read it. */
export interface CredentialPendingRow {
  readonly record_id: string;
  readonly kind: CredentialRequestKind;
  /** Secret rule keys, or `[hostname]` for a `launcher_credential`. */
  readonly identifiers: string[];
  /** RFC3339. */
  readonly requested_at: string;
  readonly session?: CredentialSession;
  /** An `agent_secret` row's page on the secrets broker, where a person signed in to the broker
   *  decides it (`<BROKER_PUBLIC_URL>/approve/<record>`). Absent on a machine login's row, and on
   *  every row from a broker older than that page. */
  readonly approve_url?: string;
}

/** `GET /api/v1/credential-requests?approver=me`, which answers null where the deployment has no
 *  secrets broker. */
export interface CredentialPendingResponse {
  readonly pending: CredentialPendingRow[];
}

export const anchorFields = {
  artifact_id: z.string(),
  /** Stable owner block for new anchors; null for legacy rows awaiting backfill. */
  block_id: z.string().nullable(),
  mark_id: z.string(),
  version: z.number().int(),
  quote: z.string(),
  orphaned: z.boolean(),
};

export const AnchorSchema = z.strictObject(anchorFields).readonly();

export type Anchor = z.output<typeof AnchorSchema>;

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

/** One node on a block's path from the document's top-level block down to the block: its type,
 *  its block id ("" for a node the live document has not stamped yet) and its index among its
 *  parent's children. */
export const BlockPathEntrySchema = z
  .strictObject({ type: z.string(), id: z.string(), index: z.number().int() })
  .readonly();

export type BlockPathEntry = z.output<typeof BlockPathEntrySchema>;

/** Where a block stands in the table holding it. `row` is the row's index (0 is the header row)
 *  and `column` the cell's index in its row — the indexes `delete_row` and `delete_column` take.
 *  `row` is null for the table block itself; `column` and `header` are null for the table and for
 *  a row block. `header` is the text of the header cell drawn above the cell's column once colspans
 *  and rowspans take their places, "" where no header cell covers it. `cells` is each cell of the
 *  row as its opening words, by child index; null for the table block. */
export const TablePositionSchema = z
  .strictObject({
    id: z.string(),
    row: z.number().int().nullable(),
    column: z.number().int().nullable(),
    header: z.string().nullable(),
    cells: z.array(z.string()).readonly().nullable(),
  })
  .readonly();

export type TablePosition = z.output<typeof TablePositionSchema>;

/** Where a block stands in its document: `GET /api/v1/artifacts/{id}/blocks/{block_id}`'s answer,
 *  and a comment's or ask's `anchor_block`. `table` is present for a table block and for every
 *  block inside one. */
export const BlockPathSchema = z
  .strictObject({
    id: z.string(),
    type: z.string(),
    path: z.array(BlockPathEntrySchema).readonly(),
    table: TablePositionSchema.optional(),
  })
  .readonly();

export type BlockPath = z.output<typeof BlockPathSchema>;

/** Where a comment's or an ask's anchor's block stands in the live document. Set on the
 *  single-record reads alone (`GET /api/v1/comments/{id}`, `GET /api/v1/asks/{id}`); lists and
 *  events never carry it. */
export const anchorPositionFields = {
  /** Absent when the anchor names no block, the block has left the document, the document could
   *  not be read (`anchor_block_error` then says why), or the server predates it. */
  anchor_block: BlockPathSchema.optional(),
  /** Why `anchor_block` is absent although the anchor names a block: the read could not read its
   *  document, named by the code the API answers that error with elsewhere. The read itself still
   *  answers. */
  anchor_block_error: z
    .enum(["DOC_SERVICE_UNAVAILABLE", "DOC_SCHEMA", "DOCUMENT_UNLOADABLE", "INTERNAL"])
    .optional(),
};

export const AnchorPositionSchema = z.strictObject(anchorPositionFields).readonly();

export type AnchorPosition = z.output<typeof AnchorPositionSchema>;

export const ExternalLinkSchema = z
  .strictObject({ url: z.string(), kind: z.string().optional() })
  .readonly();

export type ExternalLink = z.output<typeof ExternalLinkSchema>;

/** `project.created` and `project.updated` carry the project row itself. */
export const ProjectSchema = z
  .strictObject({
    key: z.string(),
    name: z.string(),
    open_asks: z.number().int().optional(),
    created_at: z.string(),
  })
  .readonly();

export type Project = z.output<typeof ProjectSchema>;

/**
 * A project's default assignee: the last step of an issue create's assignee chain, reached when
 * the create named nobody, the session acts for nobody, and no parent carries one. `assignee` is
 * a person's lowercase email, or null for a project that names nobody — which
 * `GET /api/v1/settings/project-default-assignees` leaves out of its list entirely.
 */
export const ProjectDefaultAssigneeSchema = z
  .strictObject({ project: z.string(), assignee: z.string().nullable() })
  .readonly();

export type ProjectDefaultAssignee = z.output<typeof ProjectDefaultAssigneeSchema>;

/** The body of `PUT /api/v1/projects/{key}/default-assignee`: the person the project's unowned
 *  work lands on, or null to clear the default. */
export interface ProjectDefaultAssigneeInput {
  readonly assignee: string | null;
}

export const RepoProjectSchema = z
  .strictObject({
    repo: z.string(),
    project: z.string(),
    /** The project's default repository: an issue with no repository of its own and none on its
     *  parent chain resolves to it. At most one mapping of a project is its default. */
    default: z.boolean(),
    created_by: ActorSchema,
    created_at: z.string(),
  })
  .readonly();

export type RepoProject = z.output<typeof RepoProjectSchema>;

/**
 * One repository a project's architecture documents are imported from (directory
 * fixed at .dispatch/architecture/), on its branch. A project may have several
 * sources, one per repository, and its component model is every source's components
 * together. The GitHub App installation id backing the access check is a server
 * detail and never appears here. The sync fields stay null until the importer runs;
 * `components` is how many components this source currently projects.
 */
export const ArchitectureSourceSchema = z
  .strictObject({
    project: z.string(),
    repo: z.string(),
    branch: z.string(),
    enabled: z.boolean(),
    created_by: ActorSchema,
    created_at: z.string(),
    last_sync_at: z.string().nullable(),
    last_commit: z.string().nullable(),
    last_error: z.string().nullable(),
    components: z.number().int(),
  })
  .readonly();

export type ArchitectureSource = z.output<typeof ArchitectureSourceSchema>;

export const RepoProjectUpdatedEventPayloadSchema = z
  .strictObject({ mapping: RepoProjectSchema, deleted: z.boolean() })
  .readonly();

export type RepoProjectUpdatedEventPayload = z.output<typeof RepoProjectUpdatedEventPayloadSchema>;

/** `settings.project_default_assignee.updated`: a human set or cleared a project's default
 *  assignee. The payload is the stored row, so a clear carries `assignee: null`. */
export const ProjectDefaultAssigneeUpdatedEventPayloadSchema = ProjectDefaultAssigneeSchema;

export type ProjectDefaultAssigneeUpdatedEventPayload = z.output<
  typeof ProjectDefaultAssigneeUpdatedEventPayloadSchema
>;

export const ArchitectureSourceUpdatedEventPayloadSchema = z
  .strictObject({ source: ArchitectureSourceSchema, deleted: z.boolean() })
  .readonly();

export type ArchitectureSourceUpdatedEventPayload = z.output<
  typeof ArchitectureSourceUpdatedEventPayloadSchema
>;

/** `architecture.synced`: the importer replaced one source's part of the project's model.
 *  `repo` and `branch` name that source; events retained from before projects had several
 *  sources lack them. */
export const ArchitectureSyncedEventPayloadSchema = z
  .strictObject({
    repo: z.string().optional(),
    branch: z.string().optional(),
    commit: z.string(),
    components: z.number().int(),
  })
  .readonly();

export type ArchitectureSyncedEventPayload = z.output<typeof ArchitectureSyncedEventPayloadSchema>;

/** `architecture.sync_failed`: one source's import was rejected or failed; that source's
 *  previous components stay up and its row carries the same text in `last_error`. `repo` and
 *  `branch` name that source; events retained from before projects had several sources lack
 *  them. */
export const ArchitectureSyncFailedEventPayloadSchema = z
  .strictObject({
    repo: z.string().optional(),
    branch: z.string().optional(),
    error: z.string(),
  })
  .readonly();

export type ArchitectureSyncFailedEventPayload = z.output<
  typeof ArchitectureSyncFailedEventPayloadSchema
>;
