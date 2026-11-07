// The Dispatch API's wire shapes are generated from the Go route table through
// `schemas/dispatch-openapi.json`: `dispatch-api.generated.ts` is the one definition of every
// HTTP request and response body of that table, and this module re-exports it whole.
//
// Hand-written here is only what the route table does not hold:
//   - the Dispatch event payloads and the event envelope, which travel the message bus;
//   - the `GET /ws/live` socket's frames, a route the table does not carry, held against Go by
//     `fixtures/live/` and the server's `delivery_live_diff_test.go`;
//   - the closed string enums the OpenAPI document widens to `string`, with the `const` arrays
//     they are read from and the limits and header names `scripts/gen-go.ts` renders into Go;
//   - the helpers and client-side composites no route answers (`Advised`, `IssueRead`,
//     `ArtifactVersionContent`, `CreateArtifactInput`, …).
import { z } from "zod";
import type { AgentStreamResponder } from "./agent-stream";
import type {
  Actor,
  ArchitectureSource,
  Artifact,
  ArtifactVersionText,
  Ask,
  AskAnswer,
  AskEditPrevious,
  AskResolution,
  AskThread,
  Comment,
  DeliveryMeasuresResponse,
  DeliveryPipelineResponse,
  DeliveryTimelineWithMeasures,
  DocumentWriteAdvice,
  DuplicateCandidate,
  EditPreconditionCurrent,
  EditPreconditionMismatch,
  Issue,
  IssueAskRow,
  IssueClaim,
  IssueDetails,
  Message,
  Project,
  ProjectDefaultAssignee,
  RepoProject,
  SearchResponse,
  TargetCandidate,
  UserIssueState,
  Version,
  WhoamiAgent,
  WhoamiUser,
  WriteAdvice,
} from "./dispatch-api.generated";
import type { ASK_URGENCIES, ISSUE_COMPONENTS_MODES, ISSUE_ROUTE_STATUSES } from "./dispatch-tools";

export * from "./dispatch-api.generated";

export type AskUrgency = (typeof ASK_URGENCIES)[number];

/** The OAuth scopes a Dispatch credential can hold, mirroring Go's `model.ScopePerson` and
 *  `model.ScopeApprove` and the `agent_tokens.scopes` check that stores them: `person` acts as
 *  the credential's own person on every route a person reaches through the dashboard, and
 *  `approve` settles what a person settles — a document review, the answer of an approval ask, a
 *  machine-login decision or revoke, and minting, changing or revoking a personal token. A
 *  credential holding `approve` holds `person` too. */
export const DISPATCH_SCOPES = ["person", "approve"] as const;

export type DispatchScope = (typeof DISPATCH_SCOPES)[number];

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

export type IssuePriority = 0 | 1 | 2 | 3;

export type IssueComponentsMode = (typeof ISSUE_COMPONENTS_MODES)[number];

/** Response-only; never on an event payload. The advice an issue's write carries, which reports
 *  the issue's status, the session's writes since the last human event and its open asks. */
export type Advised<T> = T & { readonly advice?: WriteAdvice };

/** Response-only. The advice a **project** document's write carries: a project document stands
 *  under no issue, so there is no issue status, no writes-since-human count and no issue-scoped
 *  open-ask list to report, and the server sends the document's block report alone
 *  (`withRawAdvice`, the server's `documentWriteAdvice`). `Advised` would promise fields that
 *  write never sends. */
export type DocumentAdvised<T> = T & { readonly advice?: DocumentWriteAdvice };

/**
 * How long an issue claim is held for a holder the Envoy registry calls `unresponsive`: a
 * session past the listener's live window but inside its gone horizon has missed heartbeats, not
 * ended, and under box load that is ordinary. Dispatch refuses another agent's claim or release
 * while the holder was last seen no longer ago than this, so a stalled session keeps the issue
 * it is working and two sessions never write the same branch; a person still forces the claim
 * (`force`), which is the escape this grace leaves open.
 *
 * Twenty minutes is four times the listener's live window (`ENVOY_SESSION_LIVE_WINDOW`, 5m) and
 * well inside its gone horizon (`ENVOY_SESSION_GONE_HORIZON`, 1h), so a session that is merely
 * slow keeps its work while a box that really lost one frees it long before the horizon.
 * `scripts/gen-go.ts` emits it as `contracts.IssueClaimUnresponsiveGrace`, so the refusal the
 * server writes and the wait a client predicts are one number.
 */
export const ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS = 20 * 60 * 1000;

/**
 * The state of the session holding an issue's claim, as an issue read reports it beside the
 * claim: `live`, `unresponsive` (past the live window, inside the gone horizon - stalled, not
 * gone, and still the holder), `gone` (past the horizon, or no registration at all), or
 * `unknown` when the Envoy listener gave the read no list to judge it by.
 */
export type IssueClaimHolderState = "live" | "unresponsive" | "gone" | "unknown";

/**
 * Whether an issue's route reaches a running session right now: `live` when a live session
 * holds the role or the routed session is live, `no_holder` when nobody live holds the role (it
 * is unclaimed or its holder's session lapsed) or the session is not live, `unknown` when the
 * Envoy listener gave no list to judge it by (never read as live, nor as `no_holder`).
 */
export type IssueRouteStatus = (typeof ISSUE_ROUTE_STATUSES)[number];

/** Maximum entries in an issue's blocked_by input, before duplicate keys are removed. */
export const MAX_ISSUE_BLOCKERS = 20;

/**
 * The longest issue title, in UTF-16 units, counted after the trim `POST /api/v1/issues` and
 * `PATCH /api/v1/issues/{key}` apply; a longer one is `400 CAP_EXCEEDED`. Production's longest
 * title was 326 characters when this was set. It bounds the duplicate-title check, which compares
 * the new title with every title's lexemes in the project on each creation, and the
 * `POSSIBLE_DUPLICATE` message, which quotes a candidate's whole title. Generated into Go as
 * `contracts.IssueTitleMax`, which the server enforces, and the dashboard's title fields take it as
 * their `maxLength`, so the two agree.
 */
export const ISSUE_TITLE_MAX = 1000;

/**
 * The longest project name, in UTF-16 units, counted after the trim `POST /api/v1/projects`
 * applies; a longer one is `400 CAP_EXCEEDED`. A name shows beside its key in the sidebar's
 * 20rem rail and in every project picker's `KEY · Name` option, any bearer may write one, and no
 * route renames a project, so it is held to about two lines of that rail; production's longest name
 * was 25 characters when this was set. Generated into Go as `contracts.ProjectNameMax`, which the
 * server enforces, and the Settings name field and `dispatch project` take it as their maximum.
 */
export const PROJECT_NAME_MAX = 80;

/**
 * The most issues one page of `GET /api/v1/issues` holds: `limit` is 1 to this. Generated into Go
 * as `contracts.MaxIssuePageLimit`, which the server enforces, and the `dispatch_issues` tool's
 * `limit` takes the same bound, so the two ends agree.
 */
export const MAX_ISSUE_PAGE_LIMIT = 250;

/**
 * The page size of `GET /api/v1/issues` when a caller pages with `offset` alone, and of
 * `dispatch_issues` when it names no `limit`. Generated into Go as `contracts.DefaultIssuePageLimit`.
 */
export const DEFAULT_ISSUE_PAGE_LIMIT = 50;

/** The part an issue's document plays beside its spec. */
export type DocumentRole = "verification" | "plan";

/** Who gave a review: `person` for one a person gave, `plan_review` for a plan review's approval
 *  of a plan version. A review Dispatch carried forward to a later version keeps the kind of the
 *  review it carried, so a plan version restored to its approved text is still `plan_review`.
 *  Absent from a Dispatch older than review kinds, which wrote people's reviews alone. */
export type ArtifactReviewKind = "person" | "plan_review";

/** Who holds the turn on an open ask after a reply: `human` when the human needs to act,
 *  `agent` when the asking agent still owes the next move (a progress note). */
export const ASK_TURNS = ["human", "agent"] as const;

export type AskTurn = (typeof ASK_TURNS)[number];

/** One ask of `GET /api/v1/issues/{key}/asks?thread=1`: the row above, plus the thread that
 *  renders the card whole without a fetch per ask. A reader asks for this shape by asking for
 *  the thread, so the thread is always there. */
export interface IssueAskThreadRow extends IssueAskRow {
  /** The card's initial thread; `["ask-thread", id]` remains its targeted live refresh. */
  readonly thread: AskThread;
}

/** What a `SessionAnswer` is to the session Dispatch delivered it to, one snake_case wire value per
 *  relation, each spelling the phrase a reader prints: a person answered the session's ask,
 *  replied in its ask's thread, replied in a comment thread it wrote in, commented on a document it
 *  wrote, commented on an issue routed to it, mentioned it, sent it a message (direct, on an issue,
 *  or a broadcast's copy), or replied to its message. Mirrored in Go as the
 *  `model.SessionAnswerRelation*` constants. */
export const SESSION_ANSWER_RELATIONS = [
  "answered_your_ask",
  "replied_on_your_ask",
  "replied_in_a_comment_thread",
  "commented_on_a_document",
  "commented_on_an_issue",
  "mentioned_you",
  "sent_you_a_message",
  "replied_to_your_message",
] as const;

export type SessionAnswerRelation = (typeof SESSION_ANSWER_RELATIONS)[number];

/** The reference-change pair every event that reports a graph write carries, mirroring the
 *  server's embedded `model.ReferenceChangesPayload`: one shape, so a new payload family
 *  extends it rather than re-typing the fields. */
export interface ReferenceChangesPayload {
  /** The nodes whose inbound reference edges this write moved, named so a reader carrying
   *  their batched backlink counts refreshes exactly those rows. Absent when the write moved
   *  no counted node, and on every event recorded before the field existed. */
  readonly references_changed?: ChangedReference[];
  /** The write moved more counted nodes of some kind than an event carries (`refs.CountedChangeLimit`
   *  of each kind): a reader that does not read `references_changed_truncated_kinds` refreshes
   *  every list that holds a count. */
  readonly references_changed_truncated?: boolean;
  /** The kinds whose targets the write cut from `references_changed`, set with
   *  `references_changed_truncated`; the targets of every other kind are still named. A reader
   *  refreshes every read of each kind listed, and only those. Absent from an event written
   *  before the field existed, whose truncation said nothing of its kinds. */
  readonly references_changed_truncated_kinds?: ChangedReference["kind"][];
}

/** Whose Replies count a write can have moved, mirroring the server's `ThreadLogins` payload
 *  field: every login with a Replies row (the server's `user_threads`) on the write's thread once
 *  the write is in, sorted, `[]` when nobody has one. Each payload family names the event types
 *  that carry it; it is absent from every other type and from every event written before the
 *  field existed, which a reader takes as "anyone's count may have moved". */
export interface ThreadLoginsPayload {
  readonly thread_logins?: string[];
}

/** What of an item an event carries, on the `comment.*`, `suggestion.*` and `message.*` payloads.
 *
 *  `body_truncated` is that `body` is the first part of the item's body and not all of it. A
 *  comment, comment edit, message or broadcast body is bounded block by block
 *  (`DISPATCH_BLOCK_UNITS` on each block, `DISPATCH_ITEM_BLOCKS` on how many it holds), so one
 *  body can be far longer than the `DISPATCH_EVENT_BODY_MAX` UTF-16 units an event carries, and
 *  the rest is one read away: `GET /api/v1/comments/{id}` or `GET /api/v1/messages/{id}`. Envoy's
 *  webhook normalizer writes a field of the same name, a string (`"true"`), on the payload of a
 *  GitHub review whose body it cut; that is another payload and another field, and a reader must
 *  not take one for the other.
 *
 *  `payload_omitted` is that the message bus refused this event whole, so Dispatch's outbox
 *  published a stand-in in its place: the payload holds the original's top-level fields that are
 *  short enough to send and nothing else, so `body` and every other long field is absent. The
 *  item itself answers the rest. */
export interface CarriedBodyPayload {
  readonly body_truncated?: boolean;
  readonly payload_omitted?: boolean;
}

/** What an `issue.created`, `issue.updated` or `issue.closed` write changed, read in its own
 *  transaction. Each field is absent from every other `issue.*` type and from an event written
 *  before the fields existed, which a reader takes as "anything may have changed". */
export interface IssueEventChanges {
  /** Each field whose value the write moved, of `title`, `status`, `closed_at` (for a close or a
   *  reopen), `rank`, `priority`, `assignee`, `parent`, `labels`, `route`, `repo`, `components`,
   *  `blocked_by` and `external_links`; for `issue.created`, every field the new issue holds a
   *  value in. */
  readonly changed?: string[];
  /** The issue's parent chain after the write, parent first up to the root; `[]` for a root. */
  readonly ancestors?: string[];
  /** Present only when the status moved: for a close or a reopen every login with a Replies row
   *  on one of the issue's open asks, for any other status move `[]`. */
  readonly thread_logins?: string[];
}

/** Every `issue.*` event payload is the issue row, flat, plus what the write moved in the
 *  reference graph: a new issue's spec text is indexed in the creating transaction. */
export type IssueEventPayload = Issue & ReferenceChangesPayload & IssueEventChanges;

/** Every `ask.*` event payload is the ask row, flat, plus what the write moved in the reference
 *  graph. `restored` is true on the `ask.opened` or `ask.answered` settlement appends when an edit
 *  restores a decision block it retracted: the ask returns to the state it had, and no one answered
 *  anything new. A restore the release before Dispatch kept the mark writes - every one already in
 *  the event log, and one written during a rollout or an image rollback - carries no mark, so a
 *  missing `restored` does not prove an event is a new answer. `thread_logins` (the ask's own
 *  thread) is carried by `ask.opened`, `ask.answered` and `ask.resolved`. */
export type AskEventPayload = Ask &
  ReferenceChangesPayload &
  ThreadLoginsPayload & { readonly restored?: boolean };

/** An `ask.answered` payload. `previous_answer` is present when the answer replaced an earlier
 *  one; the asker and every follower receive it as they received the first. */
export type AskAnsweredEventPayload = AskEventPayload & {
  readonly previous_answer?: AskAnswer;
};

export type AskEditEventPayload = AskEventPayload & {
  readonly previous: AskEditPrevious;
  readonly edited_by: Actor;
  /** A version move of an approval request that follows an earlier move since the request was
   *  opened or last handed back: before this move, its `requested_version` was already below the
   *  version it named. The payload's `approval.version` is the version after the move, so the
   *  first move satisfies `requested_version < version` too and is not quiet. A quiet move changes
   *  only the version the request names, so its event, like a human's unnamed `artifact.version`,
   *  carries `notify: false` and reaches no follower. Absent on every other edit, the first move
   *  after an opening or a hand-back included, even when a thread reply left the request waiting
   *  on its agent. */
  readonly quiet?: true;
};

/** The durable result of one mention-delivery attempt in the issue event log. */
export interface CommentDeliveryEventPayload {
  readonly comment_id: string;
  /** The ask whose thread holds the comment, so a consumer keyed on the ask sees a receipt
   *  that changes the thread it renders; null when the comment belongs to no ask, and absent
   *  on a receipt a server older than the field wrote. */
  readonly ask_id?: string | null;
  readonly target: string;
  readonly attempt: number;
  readonly delivery: DeliveryCapability;
  readonly session_id: string | null;
  /** A receipt is only ever appended with a settled outcome: the sender that records it holds
   *  the attempt's claim, and both reply handlers settle the row in the statement that appends
   *  theirs. The attempt row itself reads `pending` between its commit and that outcome. */
  readonly state: "sent" | "failed";
  /** The stream already held this message: an earlier attempt landed, so this one is recognised as
   *  a repeat and not handed to the mentioned session again (`DELIVERY_DUPLICATE_WINDOW_MS`).
   *  Absent means false. */
  readonly duplicate?: boolean;
  readonly error?: string;
  readonly reply_id: string | null;
}

/**
 * The payload of every `comment.*` event. It is the comment row as it stood when the event was
 * written, and events are retained verbatim, so the row's later additions are optional here:
 * `mentions` and `deliveries` were added on 2026-09-18 (#1188), and every comment event recorded
 * before that day carries neither. A read row (`Comment`) always has both. `thread_logins` (the
 * comment's ask, else its root) is carried by `comment.created` and `comment.answered`.
 */
export interface CommentEventPayload
  extends Omit<Comment, "mentions" | "deliveries">,
    CarriedBodyPayload,
    ReferenceChangesPayload,
    ThreadLoginsPayload {
  readonly mentions?: Comment["mentions"];
  readonly deliveries?: Comment["deliveries"];
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

/**
 * The most sessions one broadcast sends to (`POST /api/v1/broadcasts`'s `session_ids`, after the
 * duplicates are dropped). A runaway guard, not a product limit: selection is a human ticking
 * boxes, but select-all makes a large send one click. Generated into Go as
 * `contracts.MaxBroadcastRecipients`, which the server enforces, so the dashboard's refusal and
 * the server's are one number.
 */
export const MAX_BROADCAST_RECIPIENTS = 100;

/**
 * The request header that names one write's intent: the same on every retry of that write, and
 * different for every new press. Dispatch accepts it, optionally, on every `POST` and `PATCH`
 * route (`PUT` and `DELETE` are idempotent already): a repeat with the same key and the same
 * request, once the first has finished, is answered with the first's stored status and body and
 * `IDEMPOTENCY_REPLAYED_HEADER`, and runs nothing again; the same key with a different request is
 * `422 IDEMPOTENCY_KEY_REUSED`; the same key while the first is still running (within
 * `IDEMPOTENCY_LEASE_MS`) is `409 IDEMPOTENCY_KEY_IN_FLIGHT` with `Retry-After: 1`, which a client
 * asks again after. A 5xx, 401, 408 or 429 is not stored, so the next attempt runs the request
 * again - unless the first attempt's write committed before its answer was lost, which a repeat
 * is answered `409 IDEMPOTENCY_KEY_COMMITTED` for. Keys belong to the signed-in login that sent
 * them and are kept `IDEMPOTENCY_KEY_TTL_MS`.
 * Five routes answer a repeat from the record they made rather than the stored body: `POST
 * /api/v1/agents/{session_id}/messages`, `POST /api/v1/broadcasts`, `POST
 * /api/v1/issues/{key}/messages`, `POST /api/v1/issues/{key}/comments` and `POST
 * /api/v1/artifacts/{id}/comments` resume any delivery still owed and answer `201` with
 * `IDEMPOTENCY_REPLAYED_HEADER` and the resource read afresh in the first answer's shape (a
 * broadcast with the first answer's `excluded`), so a recipient whose attempt a live sender holds
 * can still read `pending`, and a repeat after a lost answer is that replay, never
 * `409 IDEMPOTENCY_KEY_COMMITTED`. A write without the header is run as it always was, with no
 * replay (the IETF `Idempotency-Key` draft; Google AIP-155).
 */
export const IDEMPOTENCY_KEY_HEADER = "Idempotency-Key";

/** The response header (`?1`, a structured-field boolean) on an answer Dispatch replayed from
 *  the first request under the key rather than ran again. */
export const IDEMPOTENCY_REPLAYED_HEADER = "Idempotency-Replayed";

/** The longest `IDEMPOTENCY_KEY_HEADER` value, in characters. */
export const IDEMPOTENCY_KEY_MAX = 128;

/** A whole `IDEMPOTENCY_KEY_HEADER` value: letters, digits, `.`, `_`, `:` and `-`, from one to
 *  `IDEMPOTENCY_KEY_MAX` characters. Any other is `400 KEY_INVALID` (`IDEMPOTENCY_KEY_INVALID`). */
export const IDEMPOTENCY_KEY_PATTERN = new RegExp(`^[A-Za-z0-9._:-]{1,${IDEMPOTENCY_KEY_MAX}}$`);

/** The code of the `422` for a key already used for a different request (another method,
 *  route, path value or body); that request was not run. */
export const IDEMPOTENCY_KEY_REUSED = "KEY_REUSED";

/** The code of the `409` (with `Retry-After: 1`) for a key whose first request is still running;
 *  nothing was run, and the client asks again under the same key. */
export const IDEMPOTENCY_KEY_IN_FLIGHT = "KEY_IN_FLIGHT";

/** The code of the `409` for a key whose write landed before its answer was lost: Dispatch will
 *  not run it again and has no answer to replay. The write is done; a client treats it as landed
 *  and reads again what it wrote (the event feed carries the change too). */
export const IDEMPOTENCY_KEY_COMMITTED = "KEY_COMMITTED";

/** The code of the `400` for an `IDEMPOTENCY_KEY_HEADER` that does not match
 *  `IDEMPOTENCY_KEY_PATTERN`, or is sent twice: named as `KEY_REUSED`, `KEY_IN_FLIGHT` and
 *  `KEY_COMMITTED` are. */
export const IDEMPOTENCY_KEY_INVALID = "KEY_INVALID";

/** The code a replay's body carries when the first answer was larger than Dispatch keeps (1 MiB):
 *  the replay has the first answer's status and this body instead of the first's. A 2xx with it
 *  means the write ran; the client reads the resource again rather than this body. */
export const IDEMPOTENCY_ANSWER_NOT_KEPT = "ANSWER_NOT_KEPT";

/**
 * The request header a page puts on every write it makes (`POST`, `PATCH`, `PUT`, `DELETE`): the
 * login of the person who pressed it. Dispatch answers `409 DISPATCH_LOGIN_CHANGED` before
 * anything runs, its key untouched, when the request's caller is another person or no person at
 * all, so a write one person pressed never runs under another's session - another tab signs in
 * in place, and this tab's retry goes out on the new cookie before it learns of it. A caller that
 * resolves to nobody (a lapsed cookie) is answered by the route's own `401`. A write without the
 * header (the `dispatch` command, an agent) is not checked.
 */
export const DISPATCH_LOGIN_HEADER = "Dispatch-Login";

/** The code of the `409` for a write whose `DISPATCH_LOGIN_HEADER` names a login that is not its
 *  caller's: nothing ran. The page drops every write that login pressed. */
export const DISPATCH_LOGIN_CHANGED = "LOGIN_CHANGED";

/**
 * The code of the `403` for a write by an access token the secrets broker issued that names a
 * session and a service but no person: the machine holding it has signed nobody in, so Dispatch
 * has nobody to record the write under. Its reads are unaffected. The message names
 * `agent-secrets login`, the one command that makes the write possible, because whoever reads
 * the refusal is whoever must run it.
 */
export const DISPATCH_SIGN_IN_REQUIRED = "SIGN_IN_REQUIRED";

/** The codes of a write Dispatch made and has no answer to give for: `409 KEY_COMMITTED`, or a
 *  replayed 2xx reading `ANSWER_NOT_KEPT`. A client treats either as landed. */
export type IdempotencyLandedCode =
  | typeof IDEMPOTENCY_KEY_COMMITTED
  | typeof IDEMPOTENCY_ANSWER_NOT_KEPT;

/**
 * Whether an attempt Dispatch answered with `status` (and `code`, its error body's) ended without
 * Dispatch's verdict on the key, so a client asks again under the same key: a 5xx, a 408 or a 429
 * (none of which Dispatch stores, `IDEMPOTENCY_KEY_HEADER`), or `409 IDEMPOTENCY_KEY_IN_FLIGHT`
 * (the first request under the key is still running). Any other answer is Dispatch's verdict, and
 * the same key would be answered with it again. The page and the `dispatch` command both retry by
 * this one rule.
 */
export function retriesUnderKey(status: number, code: string | undefined): boolean {
  return (
    status >= 500 ||
    status === 408 ||
    status === 429 ||
    (status === 409 && code === IDEMPOTENCY_KEY_IN_FLIGHT)
  );
}

/**
 * Whether an answer of `status`, `replayed` (it carried `IDEMPOTENCY_REPLAYED_HEADER`) and parsed
 * `body` is a replay whose first answer Dispatch did not keep: a replayed 2xx whose body is
 * `{ code: IDEMPOTENCY_ANSWER_NOT_KEPT }`. The write ran; the client reads the resource again
 * rather than taking this body for it. An answer that is not a replay is the resource itself,
 * whatever it holds.
 */
export function answerNotKept(status: number, replayed: boolean, body: unknown): boolean {
  return (
    replayed &&
    status >= 200 &&
    status < 300 &&
    typeof body === "object" &&
    body !== null &&
    Reflect.get(body, "code") === IDEMPOTENCY_ANSWER_NOT_KEPT
  );
}

/** How long Dispatch keeps a key's answer: 24 hours. */
export const IDEMPOTENCY_KEY_TTL_MS = 24 * 60 * 60 * 1000;

/** How long a key's first request holds the key: a repeat within this is
 *  `409 IDEMPOTENCY_KEY_IN_FLIGHT`. One after it, the first one's process presumed dead, is
 *  answered `409 IDEMPOTENCY_KEY_COMMITTED` when that first run's write committed, is answered
 *  from the record on the five routes that keep one (`IDEMPOTENCY_KEY_HEADER`), and otherwise
 *  takes the key over and runs the request again. 60 seconds. */
export const IDEMPOTENCY_LEASE_MS = 60 * 1000;

export type DeliveryCapability = (typeof DELIVERY_CAPABILITIES)[number];

/**
 * How long a repeated delivery is recognised, in milliseconds: the window behind the dashboard's
 * promise that a same-mode Retry "won't deliver it twice". This comment is where that promise is
 * stated; every other place that relies on it points here.
 *
 * Dispatch keys a send by message and mode (`<message>:<mode>`, `<comment>:<target>:<mode>`), the
 * same for every attempt, and the listener makes that key, scoped to the recipient
 * (`agent.<session>.<key>`), the envelope's `dedupe_key`. A same-mode Retry of a send that already
 * landed therefore repeats that frame's dedupe key, and only that: the listener mints a new
 * `event_id` for every send. Two things recognise the repeat inside this window, both only for a
 * key that names its event (`dedupeKeyNamesItsEvent` in `envelope.ts`, which every Dispatch key
 * does), and a recipient is protected by whichever of them sits between it and the bus:
 *
 * - The notification stream stores such a frame under a MsgId of its dedupe key and topic, so it
 *   keeps one copy and answers the repeat as a duplicate, which the attempt records as
 *   `duplicate`. That protects a session the listener pushes to from the stream. It does not
 *   protect a session that subscribes over core NATS: JetStream's check applies to the stream's
 *   copy only, and a core subscriber is handed every publish, repeats included.
 * - Each host that subscribes over core NATS (the Oh My Pi extension, the Claude Code channel)
 *   hands its agent at most one frame per such key inside this window, through
 *   `createDeliveryDedupe` in `@legion/envoy-client/delivery`. A frame the host answered with an
 *   error instead (a BTW whose side turn failed) was never handed over, so the host releases its
 *   key and a same-mode re-send of it reaches the agent.
 *
 * It fails in three places. A host's `createDeliveryDedupe` holds its keys in memory, so a host
 * that keeps nothing else and restarts after the first frame landed has forgotten it, and a Retry
 * reaches that agent a second time; that is ordinary, not rare: `claude --resume` starts a new
 * channel process on the same conversation, and a Sandbox pod resumes its session in a new
 * process. pi-envoy and claude-envoy, from the releases that carry `@legion/envoy-client/replay`,
 * also keep a ledger of the frames their session was handed (pi-envoy in the session's
 * transcript, claude-envoy in a file per session id) and drop a repeat it holds after a restart,
 * for 72 hours and up to 20,000 frames; only a crash between the hand-over and the ledger write
 * still lets that Retry through. Older plugins still forget, and the dashboard cannot tell which
 * one a session runs. The stream stores a re-send of an attempt the session answered with an
 * error no more than any other repeat, so a session the listener pushes to never receives it, and
 * its attempt reads as a duplicate. The dashboard therefore offers no same-mode Retry for such an
 * attempt and points at a mode change, which is a new key; sending that Retry under a new key is
 * LEGION-431. And a host keeps at most `DELIVERY_DEDUPE_KEY_LIMIT` keys, forgetting the oldest
 * past it, so a re-send of a key it was handed before that many later ones reaches the agent
 * again. The limit is two and a half times what the whole stream stored in
 * one window when it was set, so a host reaches it only once traffic grows past that or a
 * producer floods a topic it follows.
 *
 * Past the window neither remembers the key, and a same-mode retry is a second delivery, so the
 * dashboard makes the promise only inside it, and names the restart
 * (`packages/dispatch/web/src/features/conversation/delivery.ts`). `scripts/gen-go.ts` emits this
 * as `contracts.DeliveryDuplicateWindow`, which `bus/stream.go` uses for both the stream's
 * duplicate window and its retention.
 *
 * It also bounds webhook redelivery dedupe: a GitHub, Slack or Ghost Wispr envelope is stored
 * under a MsgId of its delivery id, and GitHub redelivers deliveries up to three days old, so a
 * window shorter than that lets a GitHub redelivery be stored, and handed to a host, twice.
 *
 * This window bounds a *delivery* retry, not a repeated *write*: a write's `Idempotency-Key`
 * (`IDEMPOTENCY_KEY_HEADER`) is recognised for `IDEMPOTENCY_KEY_TTL_MS`, however late within
 * that the repeat arrives.
 */
export const DELIVERY_DUPLICATE_WINDOW_MS = 72 * 60 * 60 * 1000;

/**
 * The cause a delivery attempt records when the listener never answered its send. The listener
 * publishes before it answers, so this is the one failure that may already have reached the
 * recipient — which is what makes re-sending in another mode a genuine second delivery, and what
 * the dashboard keys that wording on. Generated into Go as `contracts.ReceiptTimeoutCause`, so
 * the string the server writes and the string the dashboard recognises are one literal.
 *
 * It is the CAUSE only: the advice that follows from it expires with
 * `DELIVERY_DUPLICATE_WINDOW_MS`, so it is composed at render time and never stored.
 */
export const RECEIPT_TIMEOUT_CAUSE =
  "The listener didn't answer within the send window; the message may already have been delivered.";

export type MessageDeliveryMode = DeliveryCapability;

/**
 * A node whose inbound reference edges one write moved, addressed the way the reads that show
 * it are keyed: an issue by key, an ask by id together with the issue or document whose ask list
 * holds its row, and a document or file (`artifact`) by its uuid (`id`, the node id its
 * `GET /api/v1/references` read reports) and its `ref`, the dispatch:// address its Referenced by
 * panel is opened on (`dispatch://CORE/artifact/runbook`, `dispatch://CORE-1/spec`,
 * `dispatch://CORE-1/artifact/notes-md`). An `artifact` entry carries neither `issue_key` nor
 * `artifact_id`; `ref` is absent for one whose owner names no address. A block of a document
 * (`block`) is named by its node id (`<document ref_key>#<block id>`, as its read reports it), its
 * `ref` (`dispatch://CORE-1/spec#b-intro`) and its document's uuid as `artifact_id`, whose panel
 * lists the edges at its blocks; a write names one whose inbound edges or whose text it moved. A
 * citation of a comment or message changes no row read by name and the graph query covers it. An
 * event written before `artifact` or `block` entries existed names none.
 */
export interface ChangedReference {
  readonly kind: "issue" | "ask" | "artifact" | "block";
  readonly id: string;
  readonly issue_key?: string | null;
  readonly artifact_id?: string | null;
  readonly ref?: string;
}

/** `thread_logins` (the message's thread root, up `in_reply_to`) is carried by an issue message's
 *  `message.created` and `message.answered`; a direct (issue-less) conversation has no Replies
 *  thread, so its messages carry none. */
export interface MessageEventPayload
  extends Message,
    CarriedBodyPayload,
    ReferenceChangesPayload,
    ThreadLoginsPayload {
  /** First 160 characters of the reply target's body, or of the block it names (`reply_block`);
   *  empty otherwise. */
  readonly reply_body?: string;
  /** The target of a reply's thread root: the conversation the reply lands in, for a consumer
   *  that groups a thread under its root. A session replying to a message aimed at that
   *  session inherits no target of its own, so its `target` names no conversation. Absent on a
   *  root message, whose `target` already names one, and under an untargeted thread. It is the
   *  root's `target` column verbatim, so it is whatever a target may be - `session:<id>` or
   *  the `role:<role>` route the thread was aimed at - and never resolved to a session. */
  readonly thread_target?: string;
}

export interface MessageDeliveryEventPayload {
  readonly message_id: string;
  readonly attempt: number;
  readonly delivery: MessageDeliveryMode;
  readonly session_id: string;
  readonly target?: string;
  readonly title: string;
  readonly state: "sent" | "failed";
  /** The stream already held this message: an earlier attempt landed, so this one is recognised as
   *  a repeat and not handed to the recipient again (`DELIVERY_DUPLICATE_WINDOW_MS`). Absent means
   *  false. */
  readonly duplicate?: boolean;
  readonly error?: string;
}

/** `message.accepted`: the session an attempt went to took the message as its user's own turn.
 *  It is never a second `message.delivery` receipt; the send still appends its own. `target` is
 *  the message's own, `session:<recipient>` for a direct message. */
export interface MessageAcceptedEventPayload {
  readonly message_id: string;
  readonly attempt: number;
  readonly session_id: string;
  readonly accepted_as: "user_turn";
  readonly target: string;
}

/** The kinds a search result and a write suggestion are one of. The route table carries `kind`
 *  as a plain string, and this is the closed list it means. */
export const SEARCH_RESULT_KINDS = ["issue", "document", "comment", "ask", "message"] as const;

export type SearchResultKind = (typeof SEARCH_RESULT_KINDS)[number];

/** The four paging fields a Dispatch that supports search paging always answers together:
 *  `total` is every match of every kind, and `reachable` is how many of `total` the pages can
 *  return — each kind lists only its best `SEARCH_KIND_DEPTH` (`dispatch-tools.ts`), so an
 *  offset at or past it returns no results. */
export type SearchResultsPage = Readonly<
  Pick<SearchResponse, "total" | "reachable" | "limit" | "offset">
>;

/** `SearchResultsPage` with every field retyped to `V`, always optional: the shape of "none of
 * these fields" for a sentinel `V` (`undefined` for the wire type below, `never` for
 * `fake-search-response.ts`'s stricter test-only override, which forbids supplying a concrete
 * value without forbidding an explicit `undefined` — TypeScript widens every optional property to
 * admit one regardless of its declared type unless the whole project turns on
 * `exactOptionalPropertyTypes`). */
export type SearchResultsPageAbsentAs<V> = { readonly [K in keyof SearchResultsPage]?: V };

/** `SearchResponse.degraded`'s one value today: meaning search could not run for this request (no
 * embedder configured, the query's own embedding timed out, or the model service answered an
 * error) and search fell back to keyword-only ranking. Generated into Go as
 * `contracts.SearchDegradedEmbedderUnavailable` so the string Dispatch writes and the string
 * `search-answer.ts` compares against cannot drift apart. */
export const SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE = "embedder_unavailable";

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
  | "answers"
  | "followed_by"
  | "part_of"
  | "depends_on"
  | "affects"
  | (string & {});

// DecisionKeptEventPayload is the payload of `decision.kept`, which a person's Keep appends on
// the decision's issue: the document, the decision's heading block and the version its text stood
// at. It wakes nobody - a Keep records that a person agreed, which asks its agent for nothing.
export interface DecisionKeptEventPayload {
  readonly artifact_id: string;
  readonly block_id: string;
  readonly changed_version: number;
}

export interface ArtifactCreatedEventPayload extends ReferenceChangesPayload {
  readonly artifact: Artifact;
}

export interface ArtifactVersionEventPayload extends ReferenceChangesPayload {
  readonly artifact_id: string;
  readonly name: string;
  readonly version: Version;
  readonly diff?: string;
}

/** `artifact.human_edited`: a person's browser edits of a document, sent once the document has
 *  been quiet for 30 seconds after the last of them. `from_version` is the version before the
 *  quiet window's first version by a person, `to_version` the window's last one; `editors` names
 *  each person who wrote one, and the event's actor is the one who edited last. `diff` is the
 *  unified diff of the two versions' stored markdown, cut at its last line end within 4,096 bytes,
 *  `diff_truncated` then true; it is empty when a version is no longer stored. The event reaches
 *  each session the document credits and the issue's route, never a person. */
export interface ArtifactHumanEditedEventPayload {
  readonly artifact_id: string;
  readonly name: string;
  readonly from_version: number;
  readonly to_version: number;
  readonly editors: readonly string[];
  readonly diff: string;
  readonly diff_truncated: boolean;
}

/** `artifact.approved` and `artifact.changes_requested`: a human review of a document,
 *  pinned to `version`; `reason` is required for changes requested; `ask_id` names the
 *  approval ask the review answered, null when given from the document header with no
 *  approval ask open at that version. A third provenance is the carry: Dispatch records an
 *  `artifact.approved` in a human's name at the version his answer to a decision block writes,
 *  when that answer is the whole change since the version he approved, and at a version whose
 *  text is byte for byte the approved version's with no decision block in it decided since; its
 *  `reason` names the carry and the version it follows, and its `ask_id` is null unless a request
 *  was open at the carry, which it answers (see `ArtifactReview`). */
export interface ArtifactReviewEventPayload {
  readonly artifact_id: string;
  readonly name: string;
  readonly version: number;
  /** Who gave the review; absent from a Dispatch older than review kinds. */
  readonly kind?: ArtifactReviewKind;
  readonly actor: Actor;
  readonly reason: string | null;
  readonly ask_id: string | null;
  readonly content_sha256?: string;
  readonly content_sha256_source?: "insert" | "backfill";
}

/** Whether a recorded result still speaks for the work as it stands: `current` at the commit the
 *  work stands at and the words the result proved, `older` at an earlier commit or against words
 *  that have changed since, and `unknown` when the check's issue resolves to no repository or
 *  Dispatch's GitHub App cannot read it. */
export type CheckResultCurrency = "current" | "older" | "unknown";

/** When a check runs: before the merge, after it, or against the default branch. The closed
 *  enum `pmdoc/schema/blocks.json` declares under the `when` attribute's `choices`. */
export type CheckWhen = "before_merge" | "after_merge" | "default_branch";

/** `result.recorded`: one result recorded against a check, appended on the **check's** issue and
 *  naming in `issue` the issue it was recorded on. */
export interface CheckResultEventPayload {
  readonly result_id: string;
  readonly issue: string;
  readonly check_issue: string;
  readonly check_artifact_id: string;
  readonly check_block_id: string;
  readonly check_version: number;
  readonly when: CheckWhen;
  readonly verdict: "pass" | "fail";
  readonly role: string;
  readonly commit_sha: string;
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

/** `settings.project_default_assignee.updated`: a human set or cleared a project's default
 *  assignee. The payload is the stored row, so a clear carries `assignee: null`. */
export type ProjectDefaultAssigneeUpdatedEventPayload = ProjectDefaultAssignee;

export interface ArchitectureSourceUpdatedEventPayload {
  readonly source: ArchitectureSource;
  readonly deleted: boolean;
}

/** `architecture.synced`: the importer replaced one source's part of the project's model.
 *  `repo` and `branch` name that source; events retained from before projects had several
 *  sources lack them. */
export interface ArchitectureSyncedEventPayload {
  readonly repo?: string;
  readonly branch?: string;
  readonly commit: string;
  readonly components: number;
}

/** `architecture.sync_failed`: one source's import was rejected or failed; that source's
 *  previous components stay up and its row carries the same text in `last_error`. `repo` and
 *  `branch` name that source; events retained from before projects had several sources lack
 *  them. */
export interface ArchitectureSyncFailedEventPayload {
  readonly repo?: string;
  readonly branch?: string;
  readonly error: string;
}

export interface UserStateUpdatedEventPayload {
  readonly login: string;
  /** The viewer's row for one issue as the write left it. Absent from a snooze's event, which
   *  every signed-in feed carries and so names no issue of the writer's. */
  readonly state?: UserIssueState;
  /** Which of `pinned`, `last_read_seq`, `dismissed` and `snoozed` the write moved (`[]` when it
   *  moved none), so another tab refreshes its pinned list for a pin, its whole Inbox for a snooze
   *  or an un-snooze (`PUT`/`DELETE /api/v1/me/asks/{id}/snooze`, whose event carries only `login`
   *  and `["snoozed"]`), and neither for a read mark. Absent from an event written before the
   *  field existed, which a reader takes as "any of pinned, last_read_seq and dismissed may have
   *  moved". */
  readonly changed?: string[];
}

/** `user_agent_state.updated`: one viewer's Clear or read mark on one session's conversation
 *  changed. It carries no state; the viewer's other tabs and devices refetch theirs. */
export interface UserAgentStateUpdatedEventPayload {
  readonly login: string;
  readonly session_id: string;
}

/** `user_thread_state.updated`: one viewer's read mark on one thread of the Replies page moved.
 *  It carries no state; the viewer's other tabs and devices refetch their replies. */
export interface UserThreadStateUpdatedEventPayload {
  readonly login: string;
  readonly thread_id: string;
}

/**
 * `issue.claimed` and `issue.released`: who is working the issue now, whose claim this one
 * replaced or cleared, and why. `previous_claim` is present on a takeover (the holder's
 * session was no longer live), a release, and a close; the previous claimant's own agent
 * topic receives the event so it learns it no longer holds the work.
 */
export interface IssueClaimEventPayload {
  readonly key: string;
  readonly status: string;
  /** The issue's `repo` and `resolved_repo`, as on every issue event. */
  readonly repo: string | null;
  readonly resolved_repo: string | null;
  readonly claim: IssueClaim | null;
  readonly previous_claim?: IssueClaim;
  readonly reason: "claimed" | "takeover" | "forced" | "released" | "closed";
}

/**
 * `issue.released` always says whose claim it cleared: the server appends the event only with a
 * claim in hand, whether the holder released it, a human did, or a close did. So readers of a
 * release never have to handle a missing previous claim, and must not pretend to.
 */
export type IssueReleasedEventPayload = IssueClaimEventPayload & {
  readonly previous_claim: IssueClaim;
};

/**
 * `issue.deployed`: the delivery timeline's word that the issue's pull request reached production,
 * written once per pull request the moment the timeline first knows the production run that
 * shipped it (by intake or by the reconcile), with the system actor `delivery-timeline`, and
 * published on the issue's subject (`notifications.dispatch.issue.<KEY>.issue.deployed`) as every
 * issue event is. `repo` and `number` name the pull request, `run_id` the production run, and
 * `completed_at` when that run's production job completed, in UTC. The Legion daemon no longer
 * reads it: nothing a deployment publishes wakes a production check. Strict: the Go writer
 * (`packages/envoy/internal/dispatch/delivery/deployed.go`) emits exactly these four fields.
 */
export const IssueDeployedEventPayloadSchema = z.strictObject({
  repo: z.string(),
  number: z.number().int().positive(),
  run_id: z.number().int().positive(),
  completed_at: z.iso.datetime(),
});

export type IssueDeployedEventPayload = z.infer<typeof IssueDeployedEventPayloadSchema>;

/**
 * `handoff.recorded`: a session recorded version `version` of the issue's `phase` handoff, the
 * record `handoff_id`. It never carries the body; a reader fetches the record by id. Appended in
 * the write's own transaction and published on the issue's subject as every issue event is.
 * Strict: the Go writer (`HandoffRecordedEventPayload` in
 * `packages/envoy/internal/dispatch/model`) emits exactly these three fields.
 */
export const HandoffRecordedPayloadSchema = z.strictObject({
  handoff_id: z.string(),
  phase: z.string(),
  version: z.number().int().positive(),
});

export type HandoffRecordedPayload = z.infer<typeof HandoffRecordedPayloadSchema>;

/**
 * The subject Dispatch announces its server fleet's build on, directly on the bus rather than
 * through the outbox (it is no issue's, document's or project's event), as an Envoy envelope with
 * `source` `dispatch` whose payload is `DispatchServerStartedPayload`. One envelope each time the
 * commit every live Dispatch server process serves changes: a rolling update is announced once its
 * last process of the build before has gone, a restart on the announced build is not announced, and
 * a rollback to an earlier build is announced again. Its event id, source event id and dedupe key
 * are `dispatch-server-started-<commit>-<seq>`, `seq` being the announcement's own number. Mirrored
 * by `startup.Subject` in `packages/envoy/internal/dispatch/startup`. The Legion daemon's intake
 * matches no subject to it: an envelope on it is no fact.
 */
export const DISPATCH_SERVER_STARTED_SUBJECT = "notifications.dispatch.server.started";

/**
 * The payload of a `server.started` envelope: the build commit every live Dispatch server process
 * serves (a fleet any of whose processes carries no stamped commit is never announced) and when
 * the fleet was found serving it, by the database's clock, in UTC. The field is named `started_at`
 * because the Go writer carries that name. The Legion daemon reads none of
 * it: nothing a Dispatch build announces wakes a production check. Strict: the Go writer emits
 * exactly these two fields.
 */
export const DispatchServerStartedPayloadSchema = z.strictObject({
  commit: z.string().min(1),
  started_at: z.iso.datetime(),
});

export type DispatchServerStartedPayload = z.infer<typeof DispatchServerStartedPayloadSchema>;

interface DispatchEventBase {
  readonly id: number;
  readonly issue_key: string | null;
  readonly artifact_id?: string | null;
  readonly project?: string;
  readonly seq: number;
  readonly actor: Actor;
  /** Whether the event wakes the issue's route, claimant and authors (`events.Notify`), so an
   *  agent's own event is not a wake. A reader following an owner topic on the bus skips on the
   *  published copy's `quiet` instead (`PublishedDispatchEvent`). */
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
      readonly type: "settings.project_default_assignee.updated";
      readonly payload: ProjectDefaultAssigneeUpdatedEventPayload;
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
  | (DispatchEventBase & {
      readonly type: "user_agent_state.updated";
      readonly payload: UserAgentStateUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "user_thread_state.updated";
      readonly payload: UserThreadStateUpdatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "decision.kept";
      readonly payload: DecisionKeptEventPayload;
    })
  | (DispatchEventBase & { readonly type: "issue.created"; readonly payload: IssueEventPayload })
  | (DispatchEventBase & { readonly type: "issue.updated"; readonly payload: IssueEventPayload })
  | (DispatchEventBase & { readonly type: "issue.closed"; readonly payload: IssueEventPayload })
  | (DispatchEventBase & {
      readonly type: "issue.claimed";
      readonly payload: IssueClaimEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "issue.released";
      readonly payload: IssueReleasedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "issue.deployed";
      readonly payload: IssueDeployedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "handoff.recorded";
      readonly payload: HandoffRecordedPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.created";
      readonly payload: ArtifactCreatedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.version";
      readonly payload: ArtifactVersionEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.human_edited";
      readonly payload: ArtifactHumanEditedEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.approved";
      readonly payload: ArtifactReviewEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "artifact.changes_requested";
      readonly payload: ArtifactReviewEventPayload;
    })
  | (DispatchEventBase & { readonly type: "ask.opened"; readonly payload: AskEventPayload })
  | (DispatchEventBase & {
      readonly type: "ask.anchor_refreshed";
      readonly payload: AskEventPayload;
    })
  /** Every rewording: a PATCH edit, a block ask's new text, and an approval request's move to a new
   *  version or new summary. Each one stamps the ask's `edited_at`. */
  | (DispatchEventBase & {
      readonly type: "ask.edited";
      readonly payload: AskEditEventPayload;
    })
  /** An agent handed its approval request back to the human: `approval.requested_version` is the
   *  version it handed back. It rewords nothing and leaves `edited_at` as it was; a hand-back
   *  with a new summary is an `ask.edited` followed by this event. */
  | (DispatchEventBase & {
      readonly type: "ask.handed_back";
      readonly payload: AskEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.answered";
      readonly payload: AskAnsweredEventPayload;
    })
  | (DispatchEventBase & {
      readonly type: "ask.resolved";
      readonly payload: AskEventPayload & {
        readonly state: "resolved";
        readonly resolution: AskResolution;
      };
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
      readonly type: "result.recorded";
      readonly payload: CheckResultEventPayload;
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
      readonly type: "message.accepted";
      readonly payload: MessageAcceptedEventPayload;
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

export type EventType = DispatchEvent["type"];

/** The code of the `401` Dispatch itself answers a caller it cannot resolve to a person - no
 *  session cookie, one it cannot read, one whose generation it has revoked, or a membership the
 *  sign-in issuer no longer confirms (`identity.WriteError`). A `401` without it came from
 *  something in front of Dispatch, an authenticating proxy among them, and says nothing about
 *  the session: a sign-out answered that way did not go through. */
export const AUTH_NO_IDENTITY = "NO_IDENTITY";

/** `GET /auth/whoami`'s answer for a person signed in to the dashboard. */
export interface AuthenticatedUser {
  readonly kind: "user";
  readonly login: string;
  /** The decimal id at or below which every event is final (committed or rolled back for good)
   *  when Dispatch answered. Every read the page makes after this answer holds every event at or
   *  below it, so the event feed resumes from it on its first
   *  subscription and replays exactly what committed since. Always answered: a server that cannot
   *  answer the head answers a 503 rather than a whoami without one (`EVENT_HEAD_UNAVAILABLE` while
   *  no event id is proven final yet, which ends on its own; `EVENT_HEAD_READ_FAILED` when its
   *  database does not answer), and a
   *  server older than the field serves no `/ws/live` either, so no page of this release ever
   *  reads one of its answers. */
  readonly event_head: string;
}

/** GET /api/v1/whoami: a person by lowercase email (`login`), or an agent with its personal
 *  token's owner (a lowercase email) — null under the shared token or a verified service token —
 *  and that service token's subject, null unless one authenticated the request. `session` is the
 *  session the credential itself proved, which only an access token the secrets broker issued
 *  does, and `project` the one project key that credential lets the caller write, null where it
 *  names none. Both are absent from a Dispatch older than broker-issued tokens, which a client
 *  reads as an older build rather than as null.
 *
 *  `scopes` is the OAuth scopes the caller's own credential holds, which decide the routes it may
 *  call: both of them for a person at a browser, the set its row names for a personal token, and
 *  `[]` for the shared token and a verified service token, which act as nobody. The kind stays
 *  `agent` for a personal token holding `person`, because the kind names the credential and not
 *  the act; the person it acts for is `owner`. */
export type WhoamiResponse = WhoamiUser | WhoamiAgent;

interface CreateArtifactOptions {
  readonly name: string;
  readonly summary?: string;
  readonly actor?: Actor;
}

export type CreateArtifactInput = CreateArtifactOptions &
  ({ readonly file: Blob } | { readonly content: string });

/** What `DispatchClient.read` composes from `GET /api/v1/issues/{key}` and that issue's events
 *  page: no route answers it. The events are `DispatchEvent`, whose payload this package types
 *  per event type; the route table's own `Event` leaves `payload` as `unknown`. */
export interface IssueRead {
  readonly issue: IssueDetails;
  readonly events: DispatchEvent[];
}

/**
 * How many of an issue's events `dispatch_read` lists, the newest ones, when it names neither
 * `events` nor `all_events`: what `DispatchClient.read` returns by default.
 */
export const DEFAULT_ISSUE_READ_EVENTS = 10;

/**
 * The close code the document websocket (`/ws/doc/{room}`) refuses a room with when the tree it
 * holds is outside the Proof schema: it completes the upgrade and closes with this code, and
 * `DOCUMENT_SCHEMA_CLOSE_REASON`, before any sync, so a browser editor never receives a tree it
 * would normalize and write back. It is in the private 4000-4999 range beside Hocuspocus's own 4401
 * and 4403, and the dashboard reads it as the document's repair state, never as a dropped
 * connection to retry. Generated into Go as `contracts.DocumentSchemaCloseCode`.
 */
export const DOCUMENT_SCHEMA_CLOSE_CODE = 4409;

/** The reason the document websocket closes with `DOCUMENT_SCHEMA_CLOSE_CODE`: the code reads of
 *  that document answer (`409 DOC_SCHEMA`). Generated into Go as
 *  `contracts.DocumentSchemaCloseReason`. */
export const DOCUMENT_SCHEMA_CLOSE_REASON = "DOC_SCHEMA";

export interface ArtifactVersionBlob {
  readonly blob: Blob;
  readonly mime: string;
  readonly version: number;
}

export type ArtifactVersionContent = ArtifactVersionText | ArtifactVersionBlob;

/**
 * The code of the `409` Dispatch answers an id written as a prefix two items or more carry: a
 * page address, a `dispatch://` reference or a dashboard address in text, or a route that takes
 * an id in its path (INFRA-869). `candidates` then lists the items it names, at most ten, in id
 * order, for the caller to write more of the id or to choose one. Generated into Go as
 * `contracts.IDAmbiguous`.
 */
export const ID_AMBIGUOUS = "ID_AMBIGUOUS";

/**
 * One item an ambiguous id prefix names (`ID_AMBIGUOUS`). `id` is the whole id the prefix
 * shortens - the item's uuid, or the block id a document block is unique under - and `ref` its
 * whole `dispatch://` reference, empty for an item that has no address (a direct message, a
 * document of an agent's conversation). At most one of `issue_key` and `document` names its
 * owner: the issue that owns it, or the reference of the project document it is written on.
 * `first_line` is the head of what tells two candidates apart, absent for a direct message a
 * caller may not read.
 */
export interface IDCandidate {
  readonly kind: "ask" | "comment" | "message" | "artifact" | "block";
  readonly id: string;
  readonly ref: string;
  readonly issue_key?: string;
  readonly document?: string;
  readonly first_line?: string;
}

export interface DispatchServiceErrorShape {
  readonly error?: string;
  readonly code?: string;
  readonly candidates?: TargetCandidate[] | DuplicateCandidate[] | IDCandidate[];
  readonly current?: EditPreconditionCurrent;
  readonly mismatches?: EditPreconditionMismatch[];
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

/**
 * One event as Dispatch's outbox publishes it to NATS: the event this API serves, plus `quiet`.
 *
 * `quiet` is whether the event carries nothing for a reader following its owner topic
 * (`events.Quiet` in envoy): a delivery receipt, an anchor refresh, settings or per-viewer
 * bookkeeping, a targeted message its session already had, an ask edit that only moved a version,
 * or a version nobody named. That is what such a reader skips on, where `notify` answers only
 * whether the event wakes the issue's route, claimant and authors - false for every event an agent
 * wrote, so a reader skipping on it drops the activity a session subscribed to the issue asked
 * for. It is optional because a frame published before Dispatch stated it carries none, and such a
 * frame is judged by `notify` instead. The API's own event shape carries no `quiet`: the outbox
 * adds it to the copy it publishes.
 */
export type PublishedDispatchEvent = DispatchEvent & { readonly quiet?: boolean };

export const IssueEventPayloadSchema = z.object({
  title: z.string().optional(),
  labels: z.array(z.string()).optional(),
  status: z.string().optional(),
  priority: z.number().int().min(0).max(3).nullable().optional(),
  rank: z.string().optional(),
  route: z.string().nullish(),
  changed: z.array(z.string()).optional(),
  ancestors: z.array(z.string()).optional(),
  thread_logins: z.array(z.string()).optional(),
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

export const ArtifactHumanEditedEventPayloadSchema = z.object({
  artifact_id: z.string().optional(),
  name: z.string().optional(),
  from_version: z.number().int().optional(),
  to_version: z.number().int().optional(),
  editors: z.array(z.string()).optional(),
  diff: z.string().optional(),
  diff_truncated: z.boolean().optional(),
});

export const ArtifactReviewEventPayloadSchema = z.object({
  artifact_id: z.string().optional(),
  name: z.string().optional(),
  version: z.number().int().optional(),
  actor: z.object({ kind: z.string(), id: z.string() }).passthrough().optional(),
  reason: z.string().nullish(),
  ask_id: z.string().nullish(),
  content_sha256: z.string().optional(),
  content_sha256_source: z.enum(["insert", "backfill"]).optional(),
});

const askEventPayloadFields = {
  id: z.string().optional(),
  opened_event_id: z.number().int().positive(),
  // Inbound only: JetStream retains ask.* envelopes for 72 h, so a renderer may still see a
  // kind the server no longer writes (the removed `action`). The outbound `AskKind` stays narrow.
  kind: z.string().optional(),
  question: z.string().optional(),
  // An option and the answer carry the part id of the block each is stored as, and the payload the
  // ask's part revision, as every other read of the ask carries them; a payload the ask has moved
  // past carries no id, and one of an ask whose parts are not indexed yet no revision either.
  options: z
    .array(z.object({ id: z.string().optional(), label: z.string().optional() }))
    .optional(),
  revision: z.number().int().positive().optional(),
  answer: z
    .object({
      id: z.string().optional(),
      selected: z.array(z.string()).nullish(),
      text: z.string().nullish(),
    })
    .nullish(),
  previous_answer: z
    .object({
      user: z.string().optional(),
      selected: z.array(z.string()).nullish(),
      text: z.string().nullish(),
      at: z.string().optional(),
    })
    .nullish(),
  restored: z.boolean().optional(),
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
      kind: z.enum(["retracted", "resolved", "withdrawn"]).optional(),
      reason: z.string().optional(),
      actor: z
        .object({ kind: z.string().optional(), id: z.string().optional() })
        .passthrough()
        .optional(),
      at: z.string().optional(),
    })
    .nullish(),
  thread_logins: z.array(z.string()).optional(),
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
  // A stripping object: without this line the flag is dropped from every agent-bound comment
  // frame, since this schema is nested inside the passthrough payload schemas.
  duplicate: z.boolean().optional(),
  state: z.enum(["pending", "sent", "failed"]),
  error: z.string().nullable(),
  resolve_error: z.string().nullable(),
  reply_id: z.string().nullable(),
  created_at: z.string(),
});

/** The block a reply answers, as a comment or message event payload carries it (`ReplyBlock`).
 *  An event recorded before the field, or by a Dispatch older than it, carries none. */
export const ReplyBlockSchema = z.object({
  node: z.string(),
  ref: z.string().optional(),
  block_id: z.string(),
  text: z.string(),
  removed: z.boolean(),
});

export const CommentEventPayloadSchema = z
  .object({
    id: z.string().optional(),
    artifact_name: z.string().optional(),
    project_key: z.string().optional(),
    artifact_slug: z.string().optional(),
    body: z.string().optional(),
    // `CarriedBodyPayload` states what these two mean and who answers the rest.
    body_truncated: z.boolean().optional(),
    payload_omitted: z.boolean().optional(),
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
    reply_block: ReplyBlockSchema.nullish(),
    thread_logins: z.array(z.string()).optional(),
  })
  // Bus frames are untrusted and this schema declares only the fields Dispatch
  // clients are known to read today, but the wire payload always carries every
  // field the server model has. .passthrough() keeps a field this schema hasn't
  // caught up to riding along instead of silently vanishing when a consumer that
  // reads it is added later, as ask_waiting_on and turn would vanish from a
  // comment.created reply with no test to catch it.
  .passthrough();

export const MessageEventPayloadSchema = z.object({
  id: z.string().optional(),
  body: z.string().optional(),
  // `CarriedBodyPayload` states what these two mean and who answers the rest.
  body_truncated: z.boolean().optional(),
  payload_omitted: z.boolean().optional(),
  target: z.string().nullish(),
  in_reply_to: z.string().nullish(),
  reply_body: z.string().optional(),
  thread_target: z.string().optional(),
  author: z.object({ kind: z.string(), id: z.string() }).optional(),
  reply_block: ReplyBlockSchema.nullish(),
  thread_logins: z.array(z.string()).optional(),
});

export const DispatchTargetedResourceIDSchema = z.uuid();

export const DispatchTargetedMessagePayloadSchema = MessageEventPayloadSchema.extend({
  id: DispatchTargetedResourceIDSchema,
  issue_key: z.string().nullable(),
  author: z.object({ kind: z.string(), id: z.string() }),
  body: z.string(),
  target: z.string(),
  in_reply_to: z.string().nullable(),
  // The broadcast a frame's message is one recipient's copy of, as the frame claims it: a frame is
  // untrusted, so neither its value nor its absence proves anything.
  broadcast_id: z.string().nullish(),
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
  // A stripping object: without this line the flag is dropped from every agent-bound
  // message.delivery frame.
  duplicate: z.boolean().optional(),
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

/** The timeline's facets, as the query parameters that select them and the `facet_counts` keys
 *  that count them. */
export type DeliveryFacet =
  | "repo"
  | "parent_agent"
  | "session"
  | "issue"
  | "priority"
  | "component"
  | "author"
  | "rework"
  | "deployed";

/** The facets the page colours merges by, as the `color_counts` keys that count them. */
export type DeliveryColorFacet = "repo" | "author" | "priority" | "component" | "parent_agent";

/** A window by preset, whose end stays at the present, or by the exact `from`/`to` of a custom range. */
export type DeliveryLiveWindow =
  | { readonly preset: 7 | 14 | 28 | 84 }
  | { readonly from: string; readonly to: string };

/** One view a page shows. `query` is the query string the page would send to the matching GET route
 *  without `from`/`to` (search and every facet), so the live answer and the GET answer are one. */
export type DeliveryLiveView =
  | { readonly kind: "timeline"; readonly window: DeliveryLiveWindow; readonly query: string }
  | { readonly kind: "measures"; readonly window: DeliveryLiveWindow; readonly query: string }
  | { readonly kind: "pipeline"; readonly at?: string };

/** The timeline's lists sent item by item, by id. */
export type DeliveryLiveList = "prs" | "runs" | "session_groups";

/** The timeline's maps sent entry by entry, by key. */
export type DeliveryLiveMap =
  | "issue_titles"
  | "session_titles"
  | "session_pr_counts"
  | "components";

/** The timeline's two-level count maps, sent count by count: `{facet: {value: count}}`. */
export type DeliveryLiveCounts = "facet_counts" | "color_counts";

/** What a page holds, as revisions (16 hex digits each) in the frame's own sections. */
export interface DeliveryLiveRevs {
  readonly items: Readonly<Partial<Record<DeliveryLiveList, Readonly<Record<string, string>>>>>;
  readonly maps: Readonly<Partial<Record<DeliveryLiveMap, Readonly<Record<string, string>>>>>;
  readonly counts: Readonly<
    Partial<Record<DeliveryLiveCounts, Readonly<Record<string, Readonly<Record<string, string>>>>>>
  >;
  /** Every field sent whole, by its name. */
  readonly fields: Readonly<Record<string, string>>;
}

/** A delta's revision changes, in DeliveryLiveRevs' shape; null for a key that left. */
export interface DeliveryLiveRevChanges {
  readonly items?: Readonly<
    Partial<Record<DeliveryLiveList, Readonly<Record<string, string | null>>>>
  >;
  readonly maps?: Readonly<
    Partial<Record<DeliveryLiveMap, Readonly<Record<string, string | null>>>>
  >;
  readonly counts?: Readonly<
    Partial<
      Record<DeliveryLiveCounts, Readonly<Record<string, Readonly<Record<string, string | null>>>>>
    >
  >;
  readonly fields?: Readonly<Record<string, string>>;
}

/** A delivery view's resume: the page's last applied change and what it holds; Dispatch answers
 *  the delta. */
export interface DeliveryLiveResume {
  readonly seq: string;
  readonly revs: DeliveryLiveRevs;
}

/** The close code Dispatch ends a live socket with once the socket reaches its age, a random point
 *  in [LIVE_SOCKET_MIN_AGE_MS, LIVE_SOCKET_MAX_AGE_MS). It is in the private 4000-4999 range, apart
 *  from the 4000 a page closes a silent socket with: the page reopens at once and resumes every
 *  subscription, showing no pill and refreshing nothing. */
export const LIVE_SOCKET_MAX_AGE_CLOSE = 4001;

/** The youngest age at which Dispatch ends a live socket: 30 minutes. */
export const LIVE_SOCKET_MIN_AGE_MS = 1_800_000;

/** The age before which Dispatch ends every live socket: 60 minutes. */
export const LIVE_SOCKET_MAX_AGE_MS = 3_600_000;

/** The most subscriptions one live socket holds at once; a subscribe past it is refused with a
 *  `TOO_MANY_SUBSCRIPTIONS` error frame. */
export const LIVE_SOCKET_MAX_SUBSCRIPTIONS = 64;

/** The Dispatch event feed, event by event in id order. */
export interface EventsLiveView {
  readonly kind: "events";
}

/** One session's agent stream: whether it is responding, its replay, and each frame it publishes. */
export interface AgentLiveView {
  readonly kind: "agent";
  /** The session's id, a UUID. */
  readonly session_id: string;
}

/** One view a live socket subscription shows. */
export type LiveView = DeliveryLiveView | EventsLiveView | AgentLiveView;

/** An events view's resume: the decimal id of the last event the page applied. */
export interface EventsLiveResume {
  readonly since: string;
}

/** A subscribe frame for a view of type `V`, whose resume is an `R`. */
interface LiveSubscribeTo<V extends LiveView, R> {
  readonly type: "subscribe";
  /** The page's own id for this subscription, echoed on every frame for it. */
  readonly sub: string;
  readonly view: V;
  /** A reconnecting page's resume point, of its view's kind. */
  readonly resume?: R;
}

/** Starts the subscription `sub` names. Its resume is its view's kind's: a `DeliveryLiveResume`
 *  for a delivery view, an `EventsLiveResume` for the events view, and none for an agent view,
 *  which starts from the session's replay. Dispatch refuses any other pairing with an
 *  `INVALID_FRAME` error frame, so the type admits none. */
export type LiveSubscribe =
  | LiveSubscribeTo<DeliveryLiveView, DeliveryLiveResume>
  | LiveSubscribeTo<EventsLiveView, EventsLiveResume>
  | LiveSubscribeTo<AgentLiveView, never>;

export interface LiveUnsubscribe {
  readonly type: "unsubscribe";
  readonly sub: string;
}

export type LiveClientFrame = LiveSubscribe | LiveUnsubscribe;

export type DeliveryLiveBody =
  | DeliveryTimelineWithMeasures
  | DeliveryMeasuresResponse
  | DeliveryPipelineResponse;

export interface DeliveryLiveSnapshot {
  readonly type: "snapshot";
  readonly sub: string;
  /** The change log position this answer reflects; a decimal string, since it is a bigint. */
  readonly seq: string;
  /** When the newest change it reflects was recorded; null when the log is empty. */
  readonly changed_at: string | null;
  /** Exactly the matching GET route's body for the resolved window. */
  readonly body: DeliveryLiveBody;
  readonly revs: DeliveryLiveRevs;
}

/** One list's changes: its items that are new or changed, whole, and the ids of those that left. */
export interface DeliveryLiveItemChanges<L extends DeliveryLiveList> {
  readonly upsert: NonNullable<DeliveryTimelineWithMeasures[L]>;
  readonly remove: readonly string[];
}

export interface DeliveryLiveDelta {
  readonly type: "delta";
  readonly sub: string;
  readonly seq: string;
  readonly changed_at: string | null;
  /** Fields replaced whole. `window` is here whenever it moved, and never alone. */
  readonly set: Readonly<Record<string, unknown>>;
  readonly items: { readonly [L in DeliveryLiveList]?: DeliveryLiveItemChanges<L> };
  /** A key's new value, or null when it left. */
  readonly maps: {
    readonly [M in DeliveryLiveMap]?: Readonly<
      Record<string, NonNullable<DeliveryTimelineWithMeasures[M]>[string] | null>
    >;
  };
  /** A count's new value, or null when it left. */
  readonly counts: Readonly<
    Partial<
      Record<DeliveryLiveCounts, Readonly<Record<string, Readonly<Record<string, number | null>>>>>
    >
  >;
  readonly revs: DeliveryLiveRevChanges;
}

/** Sent to every subscriber when the freshness row's verdict changes, and to a subscription right
 *  after its first answer, so a page that was not subscribed at a change still learns it; never
 *  otherwise. */
export interface DeliveryLiveFreshness {
  readonly type: "freshness";
  readonly freshness: DeliveryTimelineWithMeasures["freshness"];
  /** The sources past their threshold: "prs", "runs", "ci", "events". */
  readonly stale: readonly string[];
}

/** Sent once at the start of every events subscription, before any `event`: the id it starts
 *  after, which is the page's `since` clamped to the feed's head, or the head when the page sent
 *  none. A page holding no cursor adopts it, so a page that has applied no event still resumes
 *  without a gap. */
export interface LiveEventsStart {
  readonly type: "events_start";
  readonly sub: string;
  /** A decimal event id. */
  readonly since: string;
  /** True when the page resumed from an id at or below the feed's head: the subscription replays
   *  every event after it before it goes live, so the page missed nothing and owes no refresh for
   *  the gap. False with no resume, and for a resume above the head (an id of a log since
   *  replaced), which start at the head and replay nothing the page missed. */
  readonly replay_complete: boolean;
}

/** One event of the feed. */
export interface LiveEventFrame {
  readonly type: "event";
  readonly sub: string;
  /** The event's decimal id, which an `EventsLiveResume` names as `since`. */
  readonly id: string;
  readonly event: DispatchEvent;
}

interface LiveAgentEvent<E extends string, D> {
  readonly type: "agent";
  readonly sub: string;
  readonly event: E;
  readonly data: D;
}

/** One message of a session's agent stream; `event` names what `data` is. The responder verdict
 *  is Dispatch's own. A replay's or a frame's data is the session's own JSON, which Dispatch
 *  relays and never parses, so it is `unknown` here: meant to be an `AgentStreamReplay` or an
 *  `AgentStreamFrame`, it is whatever the session published, and a page checks it before it
 *  renders it. */
export type LiveAgentFrame =
  | LiveAgentEvent<"responder", AgentStreamResponder>
  | LiveAgentEvent<"replay", unknown>
  | LiveAgentEvent<"frame", unknown>;

/** Dispatch ended this one subscription: it fell behind (`behind`), or the relay dropped the
 *  viewer (`dropped`). The page subscribes it again at once. */
export interface LiveEnded {
  readonly type: "ended";
  readonly sub: string;
  readonly reason: "behind" | "dropped";
}

/** A refusal: of the subscription `sub` names, or, without `sub`, of a frame Dispatch could not
 *  read. */
export interface LiveError {
  readonly type: "error";
  readonly sub?: string;
  readonly code: string;
  readonly message: string;
}

/** Sent every heartbeat interval beside the socket's own ping, carrying nothing: a browser never
 *  shows a ping to the page, so this is how the page tells a quiet socket from a dead one. */
export interface LiveHeartbeat {
  readonly type: "heartbeat";
}

export type LiveServerFrame =
  | DeliveryLiveSnapshot
  | DeliveryLiveDelta
  | DeliveryLiveFreshness
  | LiveEventsStart
  | LiveEventFrame
  | LiveAgentFrame
  | LiveEnded
  | LiveError
  | LiveHeartbeat;
