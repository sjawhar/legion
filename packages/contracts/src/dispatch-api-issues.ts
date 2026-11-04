import { z } from "zod";
import {
  type Actor,
  ActorSchema,
  type DisplayTree,
  type ExternalLink,
  ExternalLinkSchema,
} from "./dispatch-api-core";
import type { Artifact } from "./dispatch-api-documents";
import { referenceChangesFields } from "./dispatch-api-refs";
import { ISSUE_COMPONENTS_MODES, type ISSUE_ROUTE_STATUSES } from "./dispatch-tools";

/**
 * The issue: the row, what resolves on each read of it, and the payloads of every `issue.*` and
 * `child.*` event. Each payload is `z.output` of the schema beside it, so the row and the event
 * that reports it are written once.
 */

/** An issue's coarse priority, `0` (`P0`, highest) through `3`. */
export const IssuePrioritySchema = z.union([
  z.literal(0),
  z.literal(1),
  z.literal(2),
  z.literal(3),
]);

export type IssuePriority = z.output<typeof IssuePrioritySchema>;

export type IssueComponentsMode = (typeof ISSUE_COMPONENTS_MODES)[number];

/**
 * An issue's effective component attachment, resolved on read: the nearest issue on its
 * parent chain (itself first) with its own attachment decides, and `inherited_from` names it
 * when that is an ancestor. Mode `inherit` with `inherited_from` null means no ancestor chose
 * (unassigned). `ids` is the effective set of live component ids; `unknown` the effective ids
 * a re-import has since retired; `reason` is set for mode `none`.
 */
export const IssueComponentsSchema = z
  .strictObject({
    mode: z.enum(ISSUE_COMPONENTS_MODES),
    ids: z.array(z.string()),
    unknown: z.array(z.string()),
    reason: z.string().nullable(),
    inherited_from: z.string().nullable(),
  })
  .readonly();

export type IssueComponents = z.output<typeof IssueComponentsSchema>;

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

/**
 * The session (or human) working an issue: who claimed it and when. At most one claim exists
 * at a time, and it is not the issue's `route` (where messages go) or its `assignee` (the
 * human who answers its asks). A claim and the issue's status are separate records: claiming
 * never moves the status, and no status change claims.
 */
export const IssueClaimSchema = z
  .strictObject({
    actor: ActorSchema,
    /** RFC3339 timestamp of the claim. */
    at: z.string(),
  })
  .readonly();

export type IssueClaim = z.output<typeof IssueClaimSchema>;

/** Done of total: the task items of a spec, or the direct children of an issue; `done` ≤ `total`. */
export interface ProgressCount {
  readonly done: number;
  readonly total: number;
}

/**
 * An issue's progress, counted as GitHub counts an issue's. `tasks` is the task-list items
 * (`- [ ]` / `- [x]`, nested lists and callouts included) of the issue's primary document as its
 * latest version renders, stored on the issue whenever a version of that document is written and
 * `null` when it holds none. It is also `null` for a spec the server has not counted yet, which
 * the reconciliation the server runs at start and every five minutes closes, so an uncounted
 * window lasts at most that long after a deploy. Counts follow the document's versions: a browser
 * edit shows once it settles, within seconds. `children` is the issue's direct children, every
 * status (icebox included), `done` being those whose status is `done`, computed from the
 * children's statuses on each read and `null` when it has none; it is a count, where
 * `IssueDetails.children` beside it on the same object is the list of child issues. It is on
 * every issue of the read, the list and the pinned list, never on an event payload. The check-in
 * collector reads `tasks` and `children` here, so the shape is a contract.
 */
export interface IssueProgress {
  readonly tasks: ProgressCount | null;
  readonly children: ProgressCount | null;
}

export const issueFields = {
  key: z.string(),
  project: z.string(),
  number: z.number().int(),
  title: z.string(),
  status: z.string(),
  priority: IssuePrioritySchema.nullable(),
  rank: z.string(),
  labels: z.array(z.string()),
  parent: z.string().nullable(),
  /** Issue keys in the same project this issue waits on, sorted. */
  blocked_by: z.array(z.string()),
  /** Lowercase email of the person who answers this issue's asks; null when unassigned. */
  assignee: z.string().nullable(),
  /** The session or human working this issue, or null when nobody has claimed it. */
  claim: IssueClaimSchema.nullable(),
  components: IssueComponentsSchema,
  external_links: z.array(ExternalLinkSchema),
  route: z.string().nullable(),
  /** The issue's own repository, owner/name, a mapping of its project; null when it inherits. */
  repo: z.string().nullable(),
  /** The repository Dispatch resolves for the issue: its own, else its nearest ancestor's, else the
   *  project's default mapping, else null. Legion records it when it first records the issue. */
  resolved_repo: z.string().nullable(),
  /** Null for issues created before Dispatch recorded their creator. */
  created_by: ActorSchema.nullable(),
  created_at: z.string(),
  updated_at: z.string(),
  closed_at: z.string().nullable(),
  primary_artifact_id: z.string(),
  /** The issue's Verification document, the one created as `verification.md`, and its Plan, the
   *  one created as `plan.md` (`Artifact.role`); `""` when the issue holds none. Absent from a
   *  Dispatch older than document roles. */
  verification_artifact_id: z.string().optional(),
  plan_artifact_id: z.string().optional(),
  last_seq: z.number().int(),
};

export const IssueSchema = z.strictObject(issueFields).readonly();

export type Issue = z.output<typeof IssueSchema>;

/**
 * Whether an issue's route reaches a running session right now: `live` when a live session
 * holds the role or the routed session is live, `no_holder` when nobody live holds the role (it
 * is unclaimed or its holder's session lapsed) or the session is not live, `unknown` when the
 * Envoy listener did not answer (never read as live).
 */
export type IssueRouteStatus = (typeof ISSUE_ROUTE_STATUSES)[number];

/**
 * Every issue read (the detail, the list, the pinned list) and every ask read (`AskReach`, for
 * the ask's issue's route) resolves this from the server's copy of the listener's session list,
 * at most 5 s old. It is stored in no table and is never on an event payload, which is why it is
 * not a field of `Issue`. Both are null when there is no route.
 */
export interface IssueRouteReach {
  readonly route_status: IssueRouteStatus | null;
  /** The live session the route reaches; set only when `route_status` is `live`. */
  readonly route_holder: string | null;
}

/**
 * What both claim routes answer: the issue, flat, with whether the Legion daemon owns its
 * status. `legion_managed_status` is true when the issue carries the `legion` label in any case,
 * or sits under an issue that does; on such an issue only the Legion daemon writes the status,
 * and a status anyone else sets is undone. It is resolved on read from the issue's labels and
 * its parents', stored nowhere, and never on an event payload, which is why it is not a field of
 * `Issue`.
 */
export interface ClaimedIssue extends Issue {
  /**
   * Absent when the server did not answer it: a Dispatch older than the field, or a label walk
   * that failed (the claim still stands). A client reads an absent field as not Legion-managed,
   * which is what every client said before the field existed.
   */
  readonly legion_managed_status?: boolean;
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
      | "blocked_by"
      | "assignee"
      | "claim"
      | "components"
      | "route"
      | "repo"
      | "resolved_repo"
      | "updated_at"
      | "last_seq"
      | "primary_artifact_id"
    >,
    IssueRouteReach {
  readonly labels?: string[];
  readonly open_asks: number;
  readonly progress: IssueProgress;
}

/**
 * `GET /api/v1/issues?limit=&offset=`: one page of the filtered listing in its order, with `total`,
 * how many issues the filters matched, and the `limit` and `offset` that chose the page. Without
 * either parameter the route answers the plain `IssueSummary[]`.
 */
export interface IssueSummaryPage {
  readonly issues: IssueSummary[];
  readonly total: number;
  readonly limit: number;
  readonly offset: number;
}

/** What each requirement of an issue's spec lacks, from `GET /api/v1/issues/{key}/coverage`. A
 *  requirement is a `requirement` block of the spec; it has a check when a `check` block of any
 *  document of that issue or of any descendant issue, at any status, names it in `serves`, and a
 *  plan task when a `plan_task` block of that set does. Nothing is ever refused for a gap. */
export interface IssueCoverage {
  readonly issue: string;
  /** The issue's spec document, `""` for an issue that has none. */
  readonly spec_artifact_id: string;
  /** Whether the issue has a plan document. Before it has one, a requirement with no plan task
   *  is no gap. */
  readonly has_plan: boolean;
  readonly requirements: readonly RequirementCoverage[];
  /** One line per gap, `<block id> has no check` and `<block id> has no plan task`, or the
   *  single line `the spec marks no requirements` for a spec that marks none. */
  readonly gaps: readonly string[];
}

/** One `requirement` block of an issue's spec and what serves it. */
export interface RequirementCoverage {
  readonly block_id: string;
  /** The block's reference, `dispatch://<KEY>/spec#b-<id>`. */
  readonly ref: string;
  /** The requirement's words, with every link and `dispatch://` reference dropped. */
  readonly text: string;
  readonly checks: number;
  readonly plan_tasks: number;
  /** This requirement's own gap lines, the ones `IssueCoverage.gaps` carries for it, so a
   *  reader draws them beside the requirement without parsing the sentence back. */
  readonly gaps: readonly string[];
}

/** Whether a recorded result still speaks for the work as it stands: `current` at the commit the
 *  work stands at and the words the result proved, `older` at an earlier commit or against words
 *  that have changed since, and `unknown` when the check's issue resolves to no repository or
 *  Dispatch's GitHub App cannot read it. */
export type CheckResultCurrency = "current" | "older" | "unknown";

/** One result recorded against a `check` block of an issue's Verification document.
 *  `issue_key` is the issue it was recorded on, which may be a descendant of `check_issue_key`,
 *  the issue whose Verification document holds the check. `check_version` and `check_sha256` are
 *  the block's version and digest when the result was recorded, so a reworded check makes its
 *  earlier results older. `currency` is computed when the results are read and is absent from
 *  the record's own answer. */
export interface CheckResult {
  readonly id: string;
  readonly issue_key: string;
  readonly check_issue_key: string;
  readonly check_artifact_id: string;
  readonly check_block_id: string;
  readonly check_version: number;
  readonly check_sha256: string;
  readonly role: string;
  readonly surface: string;
  readonly run: string;
  readonly observed: string;
  readonly commit_sha: string;
  readonly negative_control: string;
  readonly failed: number;
  readonly skipped: number;
  readonly excluded: number;
  readonly verdict: "pass" | "fail";
  readonly screenshots: readonly string[];
  readonly session: string;
  readonly actor: Actor;
  readonly created_at: string;
  readonly event_seq?: number;
  readonly currency?: CheckResultCurrency;
}

/** When a check runs: before the merge, after it, or against the default branch. The closed
 *  enum `pmdoc/schema/blocks.json` declares under the `when` attribute's `choices`. */
export const CheckWhenSchema = z.enum(["before_merge", "after_merge", "default_branch"]);

export type CheckWhen = z.output<typeof CheckWhenSchema>;

/** One `check` block of a Verification document with every result recorded against it, newest
 *  first. */
export interface CheckBlock {
  readonly block_id: string;
  readonly ref: string;
  readonly issue_key: string;
  readonly artifact_id: string;
  readonly text: string;
  readonly when: CheckWhen;
  readonly surface: string;
  readonly run: string;
  readonly negative_control: string;
  readonly version: number;
  readonly sha256: string;
  readonly results: readonly CheckResult[];
}

/** `GET /api/v1/issues/{key}/results`: the issue's Verification document and its checks in
 *  document order. An issue with no Verification document answers no checks. */
export interface IssueCheckResults {
  readonly issue: string;
  /** The issue's Verification document, `""` for an issue that has none. */
  readonly verification_artifact_id: string;
  readonly checks: readonly CheckBlock[];
}

/** `POST /api/v1/issues/{key}/results`: the stored result and the pull-request body line it
 *  implies, which Dispatch writes so every writer writes one grammar and which carries no link
 *  to Dispatch. */
export interface CheckResultRecorded {
  readonly result: CheckResult;
  readonly line: string;
}

/** What `POST /api/v1/issues/{key}/results` takes. Absent counts are 0; a `pass` counting any of
 *  them is refused `400 RESULT_CONTRADICTS`. */
export interface RecordCheckResultInput {
  readonly actor?: Actor;
  readonly check: string;
  readonly role: string;
  readonly verdict: "pass" | "fail";
  readonly commit: string;
  readonly surface?: string;
  readonly run?: string;
  readonly observed?: string;
  readonly negative_control?: string;
  readonly failed?: number;
  readonly skipped?: number;
  readonly excluded?: number;
  readonly screenshots?: readonly string[];
}

/**
 * One artifact of an `IssueReferenceSummary`: what a reference to an issue's document shows.
 * `latest_version` is its greatest version number, null only for an artifact with no version,
 * which no committed write leaves. `primary` marks the issue's spec, the document a reference to
 * a block of the spec (`dispatch://<issue>/spec#b-…`) names.
 */
export interface IssueReferenceSummaryDocument {
  readonly id: string;
  readonly slug: string;
  readonly name: string;
  readonly kind: Artifact["kind"];
  readonly latest_version: number | null;
  readonly primary: boolean;
}

/**
 * `GET /api/v1/issues/{key}/summary`: what a reference to an issue shows, read without the full
 * issue (`IssueDetails`). `documents` is every artifact of the issue, in the order the full read
 * lists its `artifacts`, so an issue reference and its documents' references share one read.
 * 404 `NOT_FOUND` for no such issue.
 */
export interface IssueReferenceSummary {
  readonly key: string;
  readonly project: string;
  readonly title: string;
  readonly status: string;
  readonly documents: IssueReferenceSummaryDocument[];
}

/**
 * `GET /api/v1/projects/{key}/artifacts/{slug}/summary`: what a reference to a project document
 * shows, read without its versions. The slug or unambiguous filename resolves as the project
 * document read resolves it (404 `ARTIFACT_NOT_FOUND`, 400 `ARTIFACT_REFERENCE_AMBIGUOUS`).
 */
export interface DocumentReferenceSummary extends Omit<IssueReferenceSummaryDocument, "primary"> {
  readonly project: string;
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
 * GET /api/v1/projects/{key}/architecture: the project's component model, every source's
 * components together, with the work attached to it. Counting rule: a component counts every
 * distinct issue whose effective set names it or any component it contains (transitively over
 * `parent`), parents and icebox included, each once; `own_*` keeps only the issues naming this
 * component itself. History is not served.
 */
export interface ArchitectureTree {
  /** Each source's sync bookkeeping, in the order the project added its sources. */
  readonly sources: ArchitectureTreeSource[];
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
  /** The repository of the source whose files define this component. */
  readonly source: string;
  readonly title: string;
  readonly parent: string | null;
  readonly depends_on: string[];
  readonly paths: string[];
  readonly external: boolean;
  readonly prose: string;
  /** The display tree of `prose`, absent unless the request sent `Dispatch-Trees: 1`. */
  readonly prose_tree?: DisplayTree;
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

/** What an `issue.created`, `issue.updated` or `issue.closed` write changed, read in its own
 *  transaction. Each field is absent from every other `issue.*` type and from an event written
 *  before the fields existed, which a reader takes as "anything may have changed". */
export const issueEventChangesFields = {
  /** Each field whose value the write moved, of `title`, `status`, `closed_at` (for a close or a
   *  reopen), `rank`, `priority`, `assignee`, `parent`, `labels`, `route`, `repo`, `components`,
   *  `blocked_by` and `external_links`; for `issue.created`, every field the new issue holds a
   *  value in. */
  changed: z.array(z.string()).optional(),
  /** The issue's parent chain after the write, parent first up to the root; `[]` for a root. */
  ancestors: z.array(z.string()).optional(),
  /** Present only when the status moved: for a close or a reopen every login with a Replies row
   *  on one of the issue's open asks, for any other status move `[]`. */
  thread_logins: z.array(z.string()).optional(),
};

export const IssueEventChangesSchema = z.strictObject(issueEventChangesFields).readonly();

export type IssueEventChanges = z.output<typeof IssueEventChangesSchema>;

/** Every `issue.*` event payload is the issue row, flat, plus what the write moved in the
 *  reference graph: a new issue's spec text is indexed in the creating transaction. */
export const IssueEventPayloadSchema = z
  .strictObject({ ...issueFields, ...referenceChangesFields, ...issueEventChangesFields })
  .readonly();

export type IssueEventPayload = z.output<typeof IssueEventPayloadSchema>;

/**
 * What a reader of the bus makes of an `issue.*` payload: the nine fields Dispatch clients read
 * today, each optional because an event retained from before a field existed carries none, and
 * everything else dropped. A frame is untrusted, so a malformed display field must not discard a
 * valid event header.
 */
export const InboundIssueEventPayloadSchema = z.object({
  title: issueFields.title.optional(),
  labels: issueFields.labels.optional(),
  status: issueFields.status.optional(),
  priority: issueFields.priority.optional(),
  rank: issueFields.rank.optional(),
  route: issueFields.route.optional(),
  changed: issueEventChangesFields.changed,
  ancestors: issueEventChangesFields.ancestors,
  thread_logins: issueEventChangesFields.thread_logins,
});

/**
 * `issue.claimed` and `issue.released`: who is working the issue now, whose claim this one
 * replaced or cleared, and why. `previous_claim` is present on a takeover (the holder's
 * session was no longer live), a release, and a close; the previous claimant's own agent
 * topic receives the event so it learns it no longer holds the work.
 */
export const issueClaimEventPayloadFields = {
  key: z.string(),
  status: z.string(),
  /** The issue's `repo` and `resolved_repo`, as on every issue event. */
  repo: z.string().nullable(),
  resolved_repo: z.string().nullable(),
  claim: IssueClaimSchema.nullable(),
  previous_claim: IssueClaimSchema.optional(),
  reason: z.enum(["claimed", "takeover", "forced", "released", "closed"]),
};

export const IssueClaimEventPayloadSchema = z.strictObject(issueClaimEventPayloadFields).readonly();

export type IssueClaimEventPayload = z.output<typeof IssueClaimEventPayloadSchema>;

/**
 * `issue.released` always says whose claim it cleared: the server appends the event only with a
 * claim in hand, whether the holder released it, a human did, or a close did. So readers of a
 * release never have to handle a missing previous claim, and must not pretend to.
 */
export const IssueReleasedEventPayloadSchema = z
  .strictObject({ ...issueClaimEventPayloadFields, previous_claim: IssueClaimSchema })
  .readonly();

export type IssueReleasedEventPayload = z.output<typeof IssueReleasedEventPayloadSchema>;

/**
 * `issue.deployed`: the delivery timeline's word that the issue's pull request reached production,
 * written once per pull request the moment the timeline first knows the production run that
 * shipped it (by intake or by the reconcile), with the system actor `delivery-timeline`, and
 * published on the issue's subject (`notifications.dispatch.issue.<KEY>.issue.deployed`) as every
 * issue event is. `repo` and `number` name the pull request, `run_id` the production run, and
 * `completed_at` when that run's production job completed, in UTC. The Legion daemon wakes the
 * issue's production check on it. Strict: the Go writer
 * (`packages/envoy/internal/dispatch/delivery/deployed.go`) emits exactly these four fields.
 */
export const IssueDeployedEventPayloadSchema = z.strictObject({
  repo: z.string(),
  number: z.number().int().positive(),
  run_id: z.number().int().positive(),
  completed_at: z.iso.datetime(),
});

export type IssueDeployedEventPayload = z.output<typeof IssueDeployedEventPayloadSchema>;

export const childStatusEventPayloadFields = {
  child_key: z.string(),
  from: z.string(),
  to: z.string(),
};

export const ChildStatusEventPayloadSchema = z
  .strictObject(childStatusEventPayloadFields)
  .readonly();

export type ChildStatusEventPayload = z.output<typeof ChildStatusEventPayloadSchema>;

/** What a reader of the bus makes of a `child.status` payload: each field optional, because a
 *  frame is untrusted and a missing one must not discard the event. */
export const InboundChildStatusEventPayloadSchema = z.object({
  child_key: childStatusEventPayloadFields.child_key.optional(),
  from: childStatusEventPayloadFields.from.optional(),
  to: childStatusEventPayloadFields.to.optional(),
});

export const ChildEventPayloadSchema = z.strictObject({ child_key: z.string() }).readonly();

export type ChildEventPayload = z.output<typeof ChildEventPayloadSchema>;

/** `result.recorded`: one result recorded against a check, appended on the **check's** issue and
 *  naming in `issue` the issue it was recorded on. */
export const CheckResultEventPayloadSchema = z
  .strictObject({
    result_id: z.string(),
    issue: z.string(),
    check_issue: z.string(),
    check_artifact_id: z.string(),
    check_block_id: z.string(),
    check_version: z.number().int(),
    when: CheckWhenSchema,
    verdict: z.enum(["pass", "fail"]),
    role: z.string(),
    commit_sha: z.string(),
  })
  .readonly();

export type CheckResultEventPayload = z.output<typeof CheckResultEventPayloadSchema>;
