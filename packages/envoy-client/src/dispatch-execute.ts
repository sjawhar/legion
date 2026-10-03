import { resolve as resolvePath } from "node:path";
import type {
  Actor,
  Advised,
  Artifact,
  Ask,
  AskRead,
  AskUrgency,
  BlockPath,
  Comment,
  CommentRead,
  CreateAskInput,
  CreateCommentInput,
  DuplicateCandidate,
  EditAskInput,
  EditOp,
  EditPrecondition,
  Event,
  GraphEdge,
  Issue,
  IssueClaim,
  IssueComponents,
  IssueComponentsInput,
  IssueComponentsMode,
  IssueDetails,
  IssuePriority,
  IssueReferences,
  IssueRouteReach,
  IssueRouteStatus,
  MessageRead,
  OpenAsk,
  OpenAsksResponse,
  SearchResult,
  WriteAdvice,
} from "@legion/contracts";
import {
  ASK_QUESTION_MAX,
  ASK_URGENCIES,
  actorLabel,
  claimHolds,
  DEFAULT_ISSUE_PAGE_LIMIT,
  dispatchToolSchema,
  dispatchToolSpecs,
  itemFromSearch,
  overCapMessage,
  PROJECT_KEY_PATTERN,
  serviceSubjectLabel,
  snippetText,
  zodSchemaApi,
} from "@legion/contracts";
import { canonicalRepo } from "@legion/contracts/repo";
import { z } from "zod";
import { askAnswerText, textHead } from "./ask-answer";
import type { DispatchConfigResolution } from "./dispatch-config";
import {
  type DispatchHost,
  type DispatchOrigin,
  defaultExec,
  type ExecFn,
  resolveCwdRepo,
  resolveOrigin,
} from "./dispatch-cwd";
import {
  DispatchClient,
  DispatchGatewayError,
  DispatchServiceError,
  type GraphReferencesQuery,
} from "./dispatch-http";
import {
  dispatchChildRef,
  dispatchDocumentRef,
  dispatchIssueRef,
  documentLabel,
  documentTopicOf,
  issueTopic,
  type OwnerTopic,
} from "./dispatch-owner";
import { messageFor } from "./errors";
import { formatZodIssues, ToolInputError } from "./tool-input-errors";

/**
 * Tool arguments as the model supplied them. The tool's Zod schema validates
 * them before use; the named keys are the ones the executor inspects itself,
 * everything else is forwarded to the API unchanged.
 */
type ToolArguments = {
  readonly urgency?: unknown;
  readonly ref?: unknown;
  readonly issue?: unknown;
  readonly project?: unknown;
  readonly artifact?: unknown;
  readonly version?: unknown;
  readonly anchor?: unknown;
  readonly options?: unknown;
  readonly labels?: unknown;
  readonly blocked_by?: unknown;
  readonly external_links?: unknown;
  readonly ops?: unknown;
  readonly in_reply_to?: unknown;
  readonly message?: unknown;
} & Record<string, unknown>;

type ExecutorEnvironment = {
  readonly LEGION_ISSUE?: string;
} & Record<string, string | undefined>;

export interface ExecuteDispatchToolInput {
  readonly tool: string;
  readonly args: ToolArguments;
  readonly cwd: string;
  readonly host: DispatchHost;
  readonly sessionId?: string;
  readonly sessionTitle?: string;
  readonly config: DispatchConfigResolution;
  /** Host cancellation for the currently executing tool call. */
  readonly signal?: AbortSignal;
  readonly env?: ExecutorEnvironment;
  readonly fetchImpl?: typeof fetch;
  readonly exec?: ExecFn;
}

export interface DispatchToolResult {
  readonly text: string;
  readonly details: Record<string, unknown>;
}

type Owner =
  | { readonly kind: "issue"; readonly issue: string }
  | {
      readonly kind: "project";
      readonly project: string;
    };

interface ParsedDispatchRef {
  readonly owner: Owner;
  readonly kind: "issue" | "spec" | "log" | "children" | "artifact" | "ask" | "comment" | "message";
  readonly id: string;
  readonly version?: number;
  /** The document slug a project-owned ask or comment ref names. */
  readonly artifact?: string;
}

interface ResolvedArtifact {
  readonly owner: Owner;
  readonly issue?: IssueDetails;
  readonly artifact: Artifact;
}

function documentTopic(artifact: Artifact): OwnerTopic {
  return documentTopicOf(artifact.project, artifact.slug);
}

function resolvedTopic(resolved: ResolvedArtifact): OwnerTopic {
  if (resolved.owner.kind === "project") return documentTopic(resolved.artifact);
  if (resolved.issue === undefined) throw new Error("issue document is missing its issue");
  return issueTopic(resolved.issue.key);
}

function notSubscribed(owner: OwnerTopic): string {
  return `(not subscribed to ${owner.label}; envoy_subscribe ${owner.topic} for every event on it)`;
}

function followsAsk(owner: OwnerTopic): string {
  return `You follow this ask: its answer and replies reach you directly. For every event on ${owner.label}: envoy_subscribe ${owner.topic}`;
}
const triageAdviceShown = new Set<string>();

export function resetAdviceMemory(): void {
  triageAdviceShown.clear();
}

function renderAdvice(
  tool: string,
  key: string,
  advice: WriteAdvice | undefined,
  opts: {
    isAskReply?: boolean;
    isPrimarySpec?: boolean;
    setsStatus?: boolean;
    replyToOwnAsk?: boolean;
  }
): string[] {
  if (advice === undefined) return [];

  const hasIssueAdvice = advice.issue_status !== undefined;
  const openAsks = advice.your_open_asks;
  const writesSinceHuman = advice.session_writes_since_human;
  const lines: string[] = [];
  const unparsed = advice.unparsed_openers;
  if (
    unparsed !== undefined &&
    unparsed.count > 0 &&
    (tool === "dispatch_issue" || tool === "dispatch_artifact")
  ) {
    const quoted = unparsed.examples.map((example) => JSON.stringify(example)).join(", ");
    const subject =
      unparsed.count === 1
        ? "1 typed-block opening in this document is text, not a block"
        : `${unparsed.count} typed-block openings in this document are text, not blocks`;
    lines.push(
      `${subject}: ${quoted}. An opening like \`:::ask{…}\` makes a block only as a line of its own, so as text it asks nobody. Mentioning the syntax on purpose? Put it in code. See the \`dispatch\` skill, "Decision blocks".`
    );
  }
  if (
    advice.decision_blocks === 0 &&
    opts.isPrimarySpec === true &&
    (tool === "dispatch_issue" || tool === "dispatch_artifact")
  ) {
    lines.push(
      'This spec holds no ask blocks, so nothing here reaches a human\'s inbox. Want human feedback? See the `dispatch` skill, "Decision blocks".'
    );
  }

  if (
    hasIssueAdvice &&
    writesSinceHuman !== undefined &&
    writesSinceHuman >= 3 &&
    (tool === "dispatch_message" ||
      tool === "dispatch_ask" ||
      (tool === "dispatch_comment" && opts.isAskReply !== true))
  ) {
    const middle =
      writesSinceHuman >= 6
        ? "Stop posting here until a human replies."
        : "Progress ledger or scratchpad? If so, stop.";
    lines.push(
      `You've sent ${writesSinceHuman} messages on ${key} with no human response. ${middle} See the \`dispatch\` skill, "Structure over stream".`
    );
  }

  if (
    advice.issue_status === "triage" &&
    tool !== "dispatch_issue" &&
    !(tool === "dispatch_issue_update" && opts.setsStatus === true) &&
    !triageAdviceShown.has(key)
  ) {
    triageAdviceShown.add(key);
    lines.push(
      `${key} is still in triage — nobody can see its development status. See the \`dispatch\` skill, "Issue status is yours to move".`
    );
  }

  if (
    hasIssueAdvice &&
    openAsks !== undefined &&
    openAsks.length > 0 &&
    (tool === "dispatch_message" ||
      tool === "dispatch_doc_edit" ||
      tool === "dispatch_issue_update" ||
      (tool === "dispatch_comment" && opts.replyToOwnAsk !== true))
  ) {
    const askCount = Math.min(openAsks.length, 2);
    for (let index = 0; index < askCount; index += 1) {
      const ask = openAsks[index];
      if (ask === undefined) break;
      lines.push(
        `You still have an open ask on ${key}: "${ask.question.slice(0, 80)}" (${ask.id}). Still needed? See the \`dispatch\` skill, "Close what you opened".`
      );
    }
  }

  return lines;
}

function documentResultDetails(artifact: Artifact): Record<string, unknown> {
  return {
    project: artifact.project,
    artifact: artifact.id,
    document: documentLabel(artifact.project, artifact.slug),
  };
}

function writeResultDetails(
  resolved: ResolvedArtifact,
  fields: Record<string, unknown>
): Record<string, unknown> {
  if (resolved.owner.kind === "project") {
    return { ...documentResultDetails(resolved.artifact), ...fields };
  }
  if (resolved.issue === undefined) throw new Error("issue document is missing its issue");
  return { issue: resolved.issue.key, ...fields };
}

/** Owner and ask ids for a result about one ask; no claim about following. */
async function askOwnerDetails(
  client: DispatchClient,
  ask: Pick<Ask, "id" | "issue_key" | "artifact_id">,
  artifact?: Artifact
) {
  if (ask.issue_key !== null) {
    return { issue: ask.issue_key, ask: ask.id };
  }
  if (ask.artifact_id === undefined || ask.artifact_id === null) {
    throw new Error("document ask is missing its artifact ID");
  }
  const owner = artifact ?? (await client.getArtifact(ask.artifact_id));
  return { ...documentResultDetails(owner), ask: ask.id };
}

/** Owner label and details for a result about one comment; the artifact is fetched when not supplied. */
async function commentOwnerResult(
  client: DispatchClient,
  comment: Pick<Comment, "id" | "issue_key" | "artifact_id">,
  artifact?: Artifact
): Promise<{ readonly label: string; readonly details: Record<string, unknown> }> {
  if (comment.issue_key !== null) {
    return { label: comment.issue_key, details: { issue: comment.issue_key, comment: comment.id } };
  }
  if (comment.artifact_id === undefined || comment.artifact_id === null) {
    throw new Error("document comment is missing its artifact ID");
  }
  const owner = artifact ?? (await client.getArtifact(comment.artifact_id));
  return {
    label: documentLabel(owner.project, owner.slug),
    details: { ...documentResultDetails(owner), comment: comment.id },
  };
}

/**
 * Details for a write that made the calling session a follower of the ask (opened it,
 * requested approval through it, followed it): `follows.ask` tells the host to say so once.
 * Editing or resolving an ask records no follower, so those results use askOwnerDetails.
 */
async function followedAskDetails(
  client: DispatchClient,
  ask: Pick<Ask, "id" | "issue_key" | "artifact_id">,
  artifact?: Artifact
) {
  return { ...(await askOwnerDetails(client, ask, artifact)), follows: { ask: ask.id } };
}

const nativeIssueKeyPattern = /^[A-Z][A-Z0-9]{1,9}-[0-9]+$/;
const externalIssueRefPattern = /^([^/\s]+)\/([^/\s#]+)#([1-9][0-9]*)$/;
const bareIssueNumberPattern = /^[1-9][0-9]*$/;

const issueFreeTools: Readonly<Record<string, true>> = {
  dispatch_issue: true,
  dispatch_edit_ask: true,
  dispatch_resolve_ask: true,
  dispatch_resolve_comment: true,
  dispatch_follow: true,
  dispatch_search: true,
  dispatch_issues: true,
  dispatch_open_asks: true,
  dispatch_whoami: true,
  dispatch_architecture_sync: true,
};

function canonicalExternalIssueRef(value: string): string {
  const match = value.trim().match(externalIssueRefPattern);
  return match ? `${canonicalRepo(match[1] ?? "", match[2] ?? "")}#${match[3]}` : value;
}
function stringArg(args: Record<string, unknown>, name: string): string {
  const value = args[name];
  if (typeof value !== "string") throw new Error(`${name} is required`);
  return value;
}

function optionalString(args: Record<string, unknown>, name: string): string | undefined {
  const value = args[name];
  return typeof value === "string" ? value : undefined;
}

function optionalBoolean(args: Record<string, unknown>, name: string): boolean | undefined {
  const value = args[name];
  return typeof value === "boolean" ? value : undefined;
}

function optionalNumber(args: Record<string, unknown>, name: string): number | undefined {
  const value = args[name];
  return typeof value === "number" ? value : undefined;
}

/** `priority` as the server's tri-state: absent leaves it, null clears it, 0–3 set it. */
function optionalPriority(
  args: Record<string, unknown>,
  name: string
): IssuePriority | null | undefined {
  const value = args[name];
  if (value === null) return null;
  // The zod spec already refused anything but an integer 0–3.
  return typeof value === "number" ? (value as IssuePriority) : undefined;
}

/** A `priority` filter list as the issue list takes it: null, meaning no priority, is `none`. */
function optionalPriorityFilter(
  args: Record<string, unknown>,
  name: string
): (IssuePriority | "none")[] | undefined {
  const value = args[name];
  // The zod spec already refused anything but a list of integers 0–3 and null.
  return Array.isArray(value)
    ? (value as (IssuePriority | null)[]).map((item) => item ?? "none")
    : undefined;
}

/** The `components` argument as the server takes it; the zod spec already checked its shape. */
function optionalComponents(
  args: Record<string, unknown>,
  name: string
): IssueComponentsInput | undefined {
  const value = args[name];
  if (typeof value !== "object" || value === null) return undefined;
  const input = value as { mode: IssueComponentsMode; ids?: string[]; reason?: string };
  return {
    mode: input.mode,
    ...(input.ids === undefined ? {} : { ids: input.ids }),
    ...(input.reason === undefined ? {} : { reason: input.reason }),
  };
}

/** The change line after a components write: what the issue's own attachment became. */
function componentsChange(input: IssueComponentsInput, after: IssueComponents): string {
  switch (input.mode) {
    case "explicit":
      return `components -> explicit [${after.ids.join(", ")}]`;
    case "none":
      return `components -> none (${after.reason ?? input.reason ?? ""})`;
    case "inherit":
      return "components -> inherit";
  }
}

const architectureComponentsAction =
  'Review the `dispatch` skill, "Architecture components", to attach it to the parts it changes or mark it as non-architectural with a reason.';

/**
 * Guidance only: creation already succeeded, so an unavailable source lookup must not turn this
 * into a failed write.
 */
async function architectureGuidance(
  client: DispatchClient,
  project: string,
  components: IssueComponents
): Promise<string | undefined> {
  if (components.mode === "none" || components.ids.length > 0) return undefined;

  try {
    if ((await client.getArchitectureSource(project)) === null) {
      return undefined;
    }
    return `Project ${project} has an architecture model, but this issue is not linked to any of its current components. ${architectureComponentsAction}`;
  } catch (error) {
    return `Could not check whether project ${project} has an architecture model: ${messageFor(error)}. ${architectureComponentsAction}`;
  }
}

interface DuplicateCandidateShape {
  readonly key?: unknown;
  readonly title?: unknown;
  readonly status?: unknown;
  readonly snippet?: unknown;
  readonly shared_terms?: unknown;
  readonly href?: unknown;
}

function isDuplicateCandidate(value: unknown): value is DuplicateCandidate {
  if (typeof value !== "object" || value === null) return false;
  const candidate = value as DuplicateCandidateShape;
  return (
    typeof candidate.key === "string" &&
    typeof candidate.title === "string" &&
    typeof candidate.status === "string" &&
    typeof candidate.snippet === "string" &&
    typeof candidate.shared_terms === "number" &&
    typeof candidate.href === "string"
  );
}

function duplicateCandidates(error: DispatchServiceError): DuplicateCandidate[] {
  if (error.candidates === undefined || !error.candidates.every(isDuplicateCandidate)) throw error;
  return error.candidates;
}

function searchResultLine(result: SearchResult, baseUrl: string): string {
  const href = new URL(result.href, baseUrl).toString();
  const { owner } = result;
  if (owner.kind === "document") {
    const reference = dispatchDocumentRef(owner.project, owner.slug);
    return `${reference} [document] ${owner.name} - ${result.kind}: ${snippetText(result.snippet)} -> ${href}`;
  }
  const artifactName = result.artifact ? ` ${result.artifact.name}` : "";
  const label = `${owner.key} [${owner.status}] ${owner.title} - ${result.kind}${artifactName}`;
  return `${label}: ${snippetText(result.snippet)} -> ${href}`;
}

function askUrgency(args: ToolArguments): AskUrgency | undefined {
  const value = args.urgency;
  return ASK_URGENCIES.find((urgency) => urgency === value);
}

/** The question text sent to Dispatch: the ref is appended unless the question already cites it. */
function questionWithRef(question: string, ref: string | undefined): string {
  return ref === undefined || question.includes(ref) ? question : `${question}\n\nRef: ${ref}`;
}

/** The validated question plus ref; `argumentProblems` has already refused an over-cap pair. */
function askQuestionWithRef(args: ToolArguments): string {
  return questionWithRef(stringArg(args, "question"), optionalString(args, "ref"));
}

function askQuestionProblem(withRef: string): string | undefined {
  return withRef.length > ASK_QUESTION_MAX
    ? `question plus ref ${overCapMessage(withRef.length, ASK_QUESTION_MAX)}; shorten the question or drop the ref`
    : undefined;
}

function parseDispatchRef(ref: string): ParsedDispatchRef | null {
  const projectDocument = ref.match(
    /^dispatch:\/\/([A-Z][A-Z0-9]{1,9})\/artifact\/([^/@]+)(?:@v(\d+))?(?:\/(ask|comment)\/([^/]+))?$/
  );
  if (projectDocument) {
    const [, project, artifact, version, targetKind, targetID] = projectDocument;
    if (
      project === undefined ||
      artifact === undefined ||
      (version !== undefined && Number(version) < 1)
    ) {
      return null;
    }
    if (targetKind === undefined) {
      return {
        owner: { kind: "project", project },
        kind: "artifact",
        id: artifact,
        ...(version === undefined ? {} : { version: Number(version) }),
      };
    }
    if (targetID === undefined || (targetKind !== "ask" && targetKind !== "comment")) return null;
    return {
      owner: { kind: "project", project },
      kind: targetKind,
      id: targetID,
      artifact,
    };
  }

  const issueReference = ref.match(
    /^dispatch:\/\/([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?:\/(spec)|\/(log)|\/(children)|\/artifact\/([^/@]+)(?:@v(\d+))?|\/ask\/([^/]+)|\/comment\/([^/]+)|\/message\/([^/]+))?$/
  );
  if (!issueReference) return null;
  const [, issue, spec, log, children, artifact, version, ask, comment, message] = issueReference;
  if (!issue || (version !== undefined && Number(version) < 1)) return null;
  const owner: Owner = { kind: "issue", issue };
  if (spec) return { owner, kind: "spec", id: spec };
  if (log) return { owner, kind: "log", id: log };
  if (children) return { owner, kind: "children", id: children };
  if (artifact) {
    return {
      owner,
      kind: "artifact",
      id: artifact,
      ...(version === undefined ? {} : { version: Number(version) }),
    };
  }
  if (ask) return { owner, kind: "ask", id: ask };
  if (comment) return { owner, kind: "comment", id: comment };
  if (message) return { owner, kind: "message", id: message };
  return { owner, kind: "issue", id: issue };
}

/** The address of the ask or comment `id` under a parsed ref's owner (a project-owned ref names its document). */
function refTarget(ref: ParsedDispatchRef, kind: "ask" | "comment", id: string): string {
  const ownerRef =
    ref.owner.kind === "issue"
      ? dispatchIssueRef(ref.owner.issue)
      : dispatchDocumentRef(ref.owner.project, `${ref.artifact}`);
  return dispatchChildRef(ownerRef, kind, id);
}

const canonicalUUIDPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const compactUUIDPattern = /^[0-9a-f]{32}$/i;

/** Canonicalizes every UUID spelling accepted by github.com/google/uuid.Parse. */
function normalizeUUID(value: string): string | undefined {
  let compact = value;
  if (value.slice(0, 9).toLowerCase() === "urn:uuid:") {
    compact = value.slice(9);
  } else if (value.length === 38 && value.startsWith("{") && value.endsWith("}")) {
    compact = value.slice(1, -1);
  }
  if (canonicalUUIDPattern.test(compact)) compact = compact.replaceAll("-", "");
  if (!compactUUIDPattern.test(compact)) return undefined;
  return `${compact.slice(0, 8)}-${compact.slice(8, 12)}-${compact.slice(12, 16)}-${compact.slice(16, 20)}-${compact.slice(20)}`.toLowerCase();
}

const askIdProblem = "ask must be a bare ask id or a dispatch://.../ask/<id> reference";
const commentIdProblem =
  "comment must be a bare comment id or a dispatch://.../comment/<id> reference";
const messageIdProblem =
  "in_reply_to must be a full message id (uuid) or a dispatch://KEY/message/<id> reference";

/** The ask id a bare id or dispatch://.../ask/<id> names; `argumentProblems` refused anything else. */
function askId(args: ToolArguments): string {
  const ask = stringArg(args, "ask");
  return ask.startsWith("dispatch://") ? (parseDispatchRef(ask)?.id ?? ask) : ask;
}

/** The message uuid a bare id or dispatch://KEY/message/<id> names, or undefined for anything else. */
function messageIdOf(value: string): string | undefined {
  const id = value.startsWith("dispatch://")
    ? (() => {
        const reference = parseDispatchRef(value);
        return reference?.kind === "message" ? reference.id : undefined;
      })()
    : value;
  return id === undefined ? undefined : normalizeUUID(id);
}

/** A short id: 8+ hex characters (hyphens allowed) that is not a full uuid. */
const idPrefixPattern = /^[0-9a-f][0-9a-f-]{7,}$/i;

/** How a refusal names the owner whose asks or comments a prefix was matched against. */
function refOwnerName(ref: ParsedDispatchRef): string {
  return ref.owner.kind === "issue" ? ref.owner.issue : `${ref.owner.project}/${ref.artifact}`;
}

/**
 * The full id an ask or comment reference names. A full uuid passes through; a unique prefix
 * is resolved against `list()`, the candidate set `ownerName` names; anything else is refused.
 */
async function resolveIdPrefix(
  tool: string,
  kind: "ask" | "comment",
  id: string,
  ownerName: string,
  list: () => Promise<readonly { readonly id: string }[]>
): Promise<string> {
  const fullID = normalizeUUID(id);
  if (fullID !== undefined) return fullID;
  if (!idPrefixPattern.test(id)) {
    throw new ToolInputError(tool, [
      `${kind} id ${id} must be a full uuid or a prefix of at least 8 hex characters`,
    ]);
  }
  const prefix = id.toLowerCase();
  const matches = (await list()).filter((item) => item.id.toLowerCase().startsWith(prefix));
  if (matches.length === 1 && matches[0] !== undefined) return matches[0].id;
  throw new ToolInputError(tool, [
    matches.length === 0
      ? `${kind} id ${id} matches none of the ${kind}s on ${ownerName}; use the full id`
      : `${kind} id ${id} matches ${matches.length} ${kind}s on ${ownerName}; use the full id`,
  ]);
}

const askIdShapeProblem = "ask ids are uuids (a prefix of at least 8 hex characters works)";

/** The open asks this session authored: the read `dispatch_open_asks` reports. */
async function sessionOpenAsks(
  client: DispatchClient,
  sessionId: string | undefined
): Promise<readonly Pick<OpenAsk, "id" | "question">[]> {
  const session = sessionId?.trim();
  if (!session) throw new Error("host session id is required to read this session's open asks");
  return (await client.openAsks(session)).asks;
}

/** The asks a refused ask id can be corrected to, in the server's own hint shape. */
function openAskHints(asks: readonly Pick<OpenAsk, "id" | "question">[]): string {
  if (asks.length === 0) return "you have no open asks";
  const hints = asks
    .slice(0, askHintLimit)
    .map((ask) => `${ask.id.slice(0, 8)} — ${textHead(ask.question)}`);
  return `your open asks: ${hints.join("; ")}`;
}

/**
 * The ask uuid `dispatch_edit_ask`, `dispatch_resolve_ask`, and `dispatch_follow` write to.
 * A full uuid passes through unread; an 8+ hex prefix resolves against this session's own
 * open asks; anything else is refused before the write, naming those asks so the next call
 * carries a real id. An open-asks read that fails never masks that rule.
 */
async function resolveAskArgument(
  tool: string,
  args: ToolArguments,
  client: DispatchClient,
  sessionId: string | undefined
): Promise<string> {
  const id = askId(args);
  if (normalizeUUID(id) === undefined && !idPrefixPattern.test(id)) {
    const asks = await sessionOpenAsks(client, sessionId).catch(() => undefined);
    throw new ToolInputError(tool, [
      asks === undefined ? askIdShapeProblem : `${askIdShapeProblem}; ${openAskHints(asks)}`,
    ]);
  }
  return resolveIdPrefix(tool, "ask", id, "this session", () => sessionOpenAsks(client, sessionId));
}

/** The validated reply target; `argumentProblems` refused anything that is not a message id. */
function messageInReplyTo(args: ToolArguments): string | undefined {
  const inReplyTo = optionalString(args, "in_reply_to");
  return inReplyTo === undefined ? undefined : messageIdOf(inReplyTo);
}

/**
 * Cross-field checks the schema cannot express, run as predicates over the (possibly
 * unparsed) arguments so every problem is reported in the same refusal.
 */
function argumentProblems(tool: string, args: ToolArguments): string[] {
  const problems: string[] = [];
  switch (tool) {
    case "dispatch_ask": {
      // The schema already caps the bare question; this covers the ref the tool appends.
      const question = optionalString(args, "question");
      const ref = optionalString(args, "ref");
      if (question !== undefined && ref !== undefined && question.length <= ASK_QUESTION_MAX) {
        const problem = askQuestionProblem(questionWithRef(question, ref));
        if (problem !== undefined) problems.push(problem);
      }
      break;
    }
    case "dispatch_edit_ask":
    case "dispatch_resolve_ask":
    case "dispatch_follow": {
      const ask = optionalString(args, "ask");
      if (ask?.startsWith("dispatch://") && parseDispatchRef(ask)?.kind !== "ask") {
        problems.push(askIdProblem);
      }
      break;
    }
    case "dispatch_resolve_comment": {
      const comment = optionalString(args, "comment");
      if (comment?.startsWith("dispatch://") && parseDispatchRef(comment)?.kind !== "comment") {
        problems.push(commentIdProblem);
      }
      break;
    }
    case "dispatch_comment": {
      if (
        optionalString(args, "quote") !== undefined &&
        optionalString(args, "artifact") === undefined
      ) {
        problems.push("artifact is required when quote is supplied");
      }
      if (
        optionalString(args, "reply_to") !== undefined &&
        optionalString(args, "reply_to_ask") !== undefined
      ) {
        problems.push("reply_to and reply_to_ask cannot both be set");
      }
      break;
    }
    case "dispatch_message": {
      const inReplyTo = optionalString(args, "in_reply_to");
      if (inReplyTo !== undefined && messageIdOf(inReplyTo) === undefined) {
        problems.push(messageIdProblem);
      }
      break;
    }
    case "dispatch_read": {
      const message = optionalString(args, "message");
      if (message !== undefined && messageIdOf(message) === undefined) {
        problems.push(
          "message must be a full message id (uuid) or a dispatch://KEY/message/<id> reference"
        );
      }
      break;
    }
  }
  return problems;
}

/**
 * The dispatch:// form of a dashboard URL on the configured server, or undefined when the
 * value is not such a URL. Accepts the issue, spec, artifact, ask, comment, and log pages
 * plus project document pages (with their ?ask= / ?comment= deep links).
 */
export function dispatchRefFromUrl(value: string, serverUrl: string): string | undefined {
  let url: URL;
  let origin: string;
  try {
    url = new URL(value);
    origin = new URL(serverUrl).origin;
  } catch {
    return undefined;
  }
  if (url.origin !== origin) return undefined;
  // The SPA's version selector: `?v=N` on issue pages, `?version=N` on project document pages.
  const versionOf = (name: string): string => {
    const value = url.searchParams.get(name);
    return value !== null && /^[1-9][0-9]*$/.test(value) ? `@v${value}` : "";
  };
  const issuePage = url.pathname.match(
    /^\/issues\/([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?:\/(spec|log|conversation|children)|\/artifacts\/([^/]+)|\/asks\/([^/]+)|\/comments\/([^/]+)|\/messages\/([^/]+))?\/?$/
  );
  if (issuePage) {
    const [, key, page, artifact, ask, comment, message] = issuePage;
    const version = versionOf("v");
    // A document page carrying `?comment=`/`?ask=` names that item, not the document: that is
    // the href `dispatch_search` returns for an anchored comment or ask on an issue's document.
    if (page === "spec" || artifact !== undefined) {
      const item = itemFromSearch(url.search);
      if (item === null) return undefined;
      if (item !== undefined) return `dispatch://${key}/${item.kind}/${item.id}`;
    }
    if (page === "spec" && version !== "") return `dispatch://${key}/artifact/spec${version}`;
    if (page !== undefined) return `dispatch://${key}/${page === "conversation" ? "log" : page}`;
    if (artifact !== undefined) {
      return `dispatch://${key}/artifact/${decodeURIComponent(artifact)}${version}`;
    }
    if (ask !== undefined) return `dispatch://${key}/ask/${ask}`;
    if (comment !== undefined) return `dispatch://${key}/comment/${comment}`;
    if (message !== undefined) return `dispatch://${key}/message/${message}`;
    return `dispatch://${key}`;
  }
  const documentPage = url.pathname.match(
    /^\/projects\/([A-Z][A-Z0-9]{1,9})\/documents\/([^/]+)\/?$/
  );
  if (!documentPage) return undefined;
  const [, project, slug] = documentPage;
  const document = `dispatch://${project}/artifact/${decodeURIComponent(slug ?? "")}${versionOf("version")}`;
  const item = itemFromSearch(url.search);
  if (item === null) return undefined;
  return item === undefined ? document : `${document}/${item.kind}/${item.id}`;
}

const refGrammarProblem =
  "ref must be a valid dispatch:// reference such as dispatch://KEY-1, " +
  "dispatch://KEY-1/ask/<uuid>, dispatch://KEY-1/comment/<uuid>, " +
  "dispatch://KEY-1/message/<uuid>, dispatch://KEY-1/artifact/<slug>, or " +
  "dispatch://PROJECT/artifact/<document-ref> (an artifact id, slug, or filename); " +
  "a dashboard URL on this Dispatch server is accepted too";

const ownerRequiredProblem = "issue is required; supply issue or set LEGION_ISSUE";

// The specs are frozen module constants, so each tool's schema is built once and shared;
// parsing and issue formatting only read it.
const toolSchemas = new Map<string, z.ZodType>();

function toolSchema(tool: string): z.ZodType {
  const cached = toolSchemas.get(tool);
  if (cached !== undefined) return cached;
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === tool);
  if (!spec) throw new Error(`Unknown Dispatch tool: ${tool}`);
  const schema = dispatchToolSchema(spec, zodSchemaApi(z), { strict: true });
  toolSchemas.set(tool, schema);
  return schema;
}

interface OwnerResolution {
  readonly args: ToolArguments;
  readonly ref: ParsedDispatchRef | null;
  readonly owner: Owner | null;
}

/**
 * Fills the owner (issue or project) from the arguments, the ref, or LEGION_ISSUE, pushing
 * every defect onto `problems` and continuing so the caller reports them all at once.
 * A `ref` given as a dashboard URL on `serverUrl` is rewritten to its dispatch:// form.
 */
async function resolveOwnerArguments(
  tool: string,
  input: ToolArguments,
  cwd: string,
  env: ExecutorEnvironment,
  exec: ExecFn,
  serverUrl: string,
  problems: string[]
): Promise<OwnerResolution> {
  if (issueFreeTools[tool] === true) return { args: input, ref: null, owner: null };
  const refArgument = input.ref;
  let ref: ParsedDispatchRef | null = null;
  let args = input;
  if (typeof refArgument === "string") {
    const refText = dispatchRefFromUrl(refArgument, serverUrl) ?? refArgument;
    if (refText !== refArgument) args = { ...input, ref: refText };
    ref = parseDispatchRef(refText);
    if (ref === null) {
      problems.push(
        tool === "dispatch_ask" && !refText.startsWith("dispatch://")
          ? "ref must be a dispatch:// reference"
          : refGrammarProblem
      );
    }
  }
  const issueArgument = args.issue;
  const projectArgument = args.project;
  const artifactArgument = args.artifact;
  const versionArgument = args.version;

  if (issueArgument !== undefined && projectArgument !== undefined) {
    problems.push("exactly one of issue and project is required");
  }
  if (typeof projectArgument === "string") {
    if (!PROJECT_KEY_PATTERN.test(projectArgument)) {
      problems.push("project must be a project key such as CORE");
    }
    const refDocument = ref?.owner.kind === "project" ? (ref.artifact ?? ref.id) : undefined;
    if (
      ref?.owner.kind === "project" &&
      (ref.owner.project !== projectArgument ||
        (artifactArgument !== undefined && artifactArgument !== refDocument))
    ) {
      problems.push("project and ref must name the same document");
    }
    return {
      args: {
        ...args,
        ...(artifactArgument === undefined && refDocument !== undefined
          ? { artifact: refDocument }
          : {}),
        ...(versionArgument === undefined && ref?.version !== undefined
          ? { version: ref.version }
          : {}),
      },
      ref,
      owner: { kind: "project", project: projectArgument },
    };
  }
  if (ref?.owner.kind === "project") {
    return {
      args: {
        ...args,
        project: ref.owner.project,
        ...(artifactArgument === undefined ? { artifact: ref.artifact ?? ref.id } : {}),
        ...(versionArgument === undefined && ref.version !== undefined
          ? { version: ref.version }
          : {}),
      },
      ref,
      owner: ref.owner,
    };
  }
  if (issueArgument !== undefined || ref !== null) {
    const issue =
      typeof issueArgument === "string"
        ? canonicalExternalIssueRef(issueArgument)
        : ref?.owner.kind === "issue"
          ? ref.owner.issue
          : undefined;
    return {
      args: {
        ...args,
        ...(issue === undefined ? {} : { issue }),
        ...(artifactArgument === undefined && (ref?.kind === "spec" || ref?.kind === "artifact")
          ? { artifact: ref.id }
          : {}),
        ...(versionArgument === undefined && ref?.version !== undefined
          ? { version: ref.version }
          : {}),
      },
      ref,
      owner: issue === undefined ? null : { kind: "issue", issue },
    };
  }
  // A session answering a human's direct message names only the message it answers, by its bare
  // id: that conversation belongs to no issue, so there is no owner to resolve and LEGION_ISSUE
  // would attach the reply to an unrelated one. The `dispatch_message` case sends that call
  // through POST /api/v1/messages/{id}/reply, the one route that carries a reply without an
  // issue, and this is the only way it is left without an owner. A
  // `dispatch://KEY/message/<id>` names the issue its message lives on, so it is not a direct
  // message and keeps the issue path.
  const replyTarget = args.in_reply_to;
  if (
    tool === "dispatch_message" &&
    typeof replyTarget === "string" &&
    !replyTarget.startsWith("dispatch://")
  ) {
    return { args, ref, owner: null };
  }
  // A read by `message` names a conversation, which a direct message's has no issue to own; the
  // `dispatch_read` case reads it through GET /api/v1/messages/{id}.
  if (tool === "dispatch_read" && typeof args.message === "string") {
    return { args, ref, owner: null };
  }
  const legionIssue = env.LEGION_ISSUE;
  if (!legionIssue) {
    problems.push(ownerRequiredProblem);
    return { args, ref, owner: null };
  }
  if (nativeIssueKeyPattern.test(legionIssue) || externalIssueRefPattern.test(legionIssue)) {
    const issue = canonicalExternalIssueRef(legionIssue);
    return { args: { ...args, issue }, ref, owner: { kind: "issue", issue } };
  }
  if (!bareIssueNumberPattern.test(legionIssue)) {
    problems.push(
      "LEGION_ISSUE must be a native issue key (e.g. LEGION-3), an external owner/repo#n reference, or a bare positive issue number"
    );
    return { args, ref, owner: null };
  }
  const repo = await resolveCwdRepo(cwd, exec);
  if (!repo) {
    problems.push("issue is required; LEGION_ISSUE needs a GitHub repository in cwd");
    return { args, ref, owner: null };
  }
  const issue = `${repo}#${legionIssue}`;
  return { args: { ...args, issue }, ref, owner: { kind: "issue", issue } };
}

/** The document `artifactReference` names among `artifacts`. A bare reference is an id, a slug,
 * then a filename; a `canonical` one, the document part of a dispatch:// reference, is a slug by
 * definition (the address the dashboard and Dispatch's own routes use), so its slug match is the
 * document even where another document's filename is the same text. */
function artifactByReference(
  artifacts: readonly Artifact[],
  artifactReference: string,
  owner: "issue" | "project",
  canonical: boolean
): Artifact {
  const bySlug = artifacts.find((candidate) => candidate.slug === artifactReference);
  if (canonical && bySlug !== undefined) return bySlug;
  const byId = artifacts.find((candidate) => candidate.id === artifactReference);
  if (byId !== undefined) return byId;
  const byName = artifacts.filter((candidate) => candidate.name === artifactReference);
  // Dispatch suffixes a slug two documents would share, so one document's filename can be
  // another's slug; a bare reference both answer to names two documents, and only an id tells
  // them apart. Only a document answers to its filename there, as in Dispatch's own fallback, so
  // an image or file of that name names no second document.
  if (bySlug !== undefined) {
    const named = byName.filter(
      (candidate) => candidate.id !== bySlug.id && candidate.kind === "doc"
    );
    if (named.length > 0) {
      throw new Error(documentReferenceProblem(artifactReference, [bySlug, ...named], owner, "id"));
    }
    return bySlug;
  }
  if (byName.length > 1) {
    throw new Error(documentReferenceProblem(artifactReference, byName, owner, "slug"));
  }
  const [artifact] = byName;
  if (artifact === undefined) {
    throw new Error(documentReferenceProblem(artifactReference, artifacts, owner));
  }
  return artifact;
}

async function resolveArtifact(
  client: DispatchClient,
  owner: Owner,
  artifactReference: string | undefined,
  { canonical = false }: { readonly canonical?: boolean } = {}
): Promise<ResolvedArtifact> {
  if (owner.kind === "project") {
    if (artifactReference === undefined) {
      throw new Error("artifact is required for a project document");
    }
    // Project artifact routes resolve a slug, then a filename no other document shares, which is
    // what a dispatch:// reference means.
    const routed = await client
      .getProjectArtifact(owner.project, artifactReference)
      .catch((error: unknown) => {
        if (!dispatchAnswered(error, 404)) throw error;
        return undefined;
      });
    if (canonical && routed !== undefined) return { owner, artifact: routed };
    // A bare reference is matched as an issue's is, over the project's unlinked documents with the
    // route's answer among them: an id outranks another document's slug, and a slug another
    // document's filename also answers to is refused. The unlinked-only collection keeps an
    // issue-attached artifact of the same name from becoming the document owner.
    const unlinked = await client.listProjectArtifacts(owner.project, true);
    const artifacts =
      routed === undefined
        ? unlinked
        : [routed, ...unlinked.filter((candidate) => candidate.id !== routed.id)];
    return {
      owner,
      artifact: artifactByReference(artifacts, artifactReference, "project", canonical),
    };
  }
  const issue = await client.getIssue(owner.issue);
  let artifact: Artifact | undefined;
  if (artifactReference === undefined || artifactReference === "spec") {
    artifact = issue.artifacts.find(
      (candidate) => candidate.primary || candidate.id === issue.primary_artifact_id
    );
  } else {
    artifact = artifactByReference(issue.artifacts, artifactReference, "issue", canonical);
  }
  if (!artifact) {
    throw new Error(
      documentReferenceProblem(artifactReference ?? "spec", issue.artifacts, "issue")
    );
  }
  return { owner, issue, artifact };
}

/** The id of the document an issue carries under `reference`: its artifact id, slug, or filename,
 * or `spec` for its primary document, resolved as the Dispatch tools resolve an issue's `artifact`
 * argument. A reference that names no document, or a filename two documents share, throws the
 * same hint the Dispatch tools give. */
export async function resolveIssueDocumentId(
  client: DispatchClient,
  issue: string,
  reference: string
): Promise<string> {
  return (await resolveArtifact(client, { kind: "issue", issue }, reference)).artifact.id;
}

const documentHintLimit = 8;

/** The refusal of a document reference: none matched, or several did, in which case `use` names
 * what tells them apart (a slug, when several share a filename; the id, when one's slug is
 * another's filename). */
function documentReferenceProblem(
  reference: string,
  documents: readonly Pick<Artifact, "id" | "slug" | "name">[],
  owner: "issue" | "project",
  use?: "slug" | "id"
): string {
  const hints = documents
    .slice(0, documentHintLimit)
    .map((document) =>
      use === "id"
        ? `${document.id} (${document.slug}, ${document.name})`
        : `${document.slug} (${document.name})`
    );
  const list = hints.length === 0 ? "none" : hints.join(", ");
  return use === undefined
    ? `document "${reference}" not found by slug; this ${owner}'s documents: ${list}`
    : `"${reference}" names ${documents.length} documents on this ${owner}; use ${use === "id" ? "the id" : "a slug"}: ${list}`;
}

const askHintLimit = 8;

function askIDInputProblem(asks: readonly Pick<Ask, "id" | "question">[], scope: string): string {
  const hints = asks
    .slice(0, askHintLimit)
    .map((ask) => `${ask.id.slice(0, 8)}… ${textHead(ask.question)}`);
  return `ask IDs are UUIDs; use the full ask ID; ${scope} open asks: ${hints.length === 0 ? "none" : hints.join(", ")}`;
}

async function invalidReplyToAskProblem(
  client: DispatchClient,
  owner: Owner,
  resolved: ResolvedArtifact | undefined
): Promise<string> {
  if (owner.kind === "issue") {
    const issue = resolved?.issue ?? (await client.getIssue(owner.issue));
    return askIDInputProblem(issue.open_asks, "this issue's");
  }
  if (resolved === undefined) throw new Error("project document is missing its resolved artifact");
  return askIDInputProblem(
    await client.getArtifactAsks(resolved.artifact.id, "open"),
    "this document's"
  );
}

function anchor(
  artifact: Artifact,
  args: Record<string, unknown>
): { artifact: string; quote: string; occurrence?: number } | undefined {
  const quote = optionalString(args, "quote");
  if (quote === undefined) return undefined;
  const occurrence = optionalNumber(args, "occurrence");
  return { artifact: artifact.id, quote, ...(occurrence === undefined ? {} : { occurrence }) };
}

function toolActor(origin: DispatchOrigin, input: ExecuteDispatchToolInput): Actor {
  return {
    kind: "session",
    id: input.sessionId ?? "unknown",
    origin: {
      ...origin,
      host: input.host,
      ...(input.sessionTitle === undefined ? {} : { session_title: input.sessionTitle }),
    },
  };
}

/** One line describing a document's approval, or undefined for a draft nobody has asked about. An
 *  awaiting approval says whom its request waits on: the agent once a version moved it, the
 *  agent's own revision included, which sends that agent no event. */
function approvalLine(artifact: Pick<Artifact, "approval">): string | undefined {
  const approval = artifact.approval;
  if (approval === undefined || approval.state === "draft") return undefined;
  switch (approval.state) {
    case "awaiting": {
      const turn = approval.waiting_on === undefined ? "" : `, waiting on ${approval.waiting_on}`;
      return `Approval: awaiting${turn} (requested by ${approval.requested_by?.id ?? "unknown"}, ask ${approval.ask_id ?? "?"})`;
    }
    case "approved":
      return `Approval: approved v${approval.version} by ${approval.by?.id ?? "unknown"}`;
    case "stale":
      return `Approval: approved v${approval.version} by ${approval.by?.id ?? "unknown"}, edited since (now v${approval.latest_version}) - request approval again once the human has agreed to every point in this version`;
    case "changes_requested":
      return `Approval: changes requested on v${approval.version} by ${approval.by?.id ?? "unknown"}: ${approval.reason ?? ""}`;
  }
}

/** The Components line of an issue read: the effective set with where it came from. */
function componentsLine(components: IssueComponents): string {
  const inherited =
    components.inherited_from === null ? "" : ` (inherited from ${components.inherited_from})`;
  const retired =
    components.unknown.length === 0 ? "" : ` (retired: ${components.unknown.join(", ")})`;
  switch (components.mode) {
    case "explicit":
      return `Components: ${components.ids.length === 0 ? "none live" : components.ids.join(", ")}${inherited}${retired}`;
    case "none":
      return `Components: none — ${components.reason ?? ""}${inherited}`;
    case "inherit":
      return "Components: unassigned";
  }
}

/**
 * How every agent surface names a claim's holder and says whether the claim still holds. The
 * name is `actorLabel` from `@legion/contracts`, the same function the dashboard's header, List
 * rows and Board cards call, so a session reading `dispatch_read` and a human reading the issue
 * page see one name for one holder; without a registry it falls back to the title the session
 * stamped on the claim, as the dashboard's does. Whether it holds is `claimHolds`, the judgement
 * the dashboard's claim chip makes too: a session the loaded registry does not list reads
 * "· not running", as the chip does, and its issue is free to claim; with no registry (`titles`
 * undefined) a session's claim reads "· liveness unknown", since nothing can say whether that
 * session runs. A person's claim, and a session the registry lists, carry no marker.
 */
function claimText(claim: IssueClaim, titles: ReadonlyMap<string, string> | undefined): string {
  const holding = claimHolds(claim, titles);
  const marker =
    holding === "unknown" ? " · liveness unknown" : holding === "lapsed" ? " · not running" : "";
  return `${actorLabel(claim.actor, titles)} since ${claim.at}${marker}`;
}

/**
 * The live agent registry behind one piece of output, as session titles by id: at most one
 * `GET /api/v1/agents` per tool call, never per row, and none at all unless a session holds
 * something being rendered — the same gate the dashboard applies with `useAgents(holdsSession)`.
 *
 * `undefined` is no registry: none was needed, or the agents request failed. Names then fall back
 * to the title each session stamped on its write, and a session's claim reads "liveness unknown"
 * (`claimText`) rather than running or not running. A holder the registry does not list is the
 * ordinary case, a session that has ended, whose claim the next agent may take.
 */
async function liveSessionTitles(
  client: DispatchClient,
  needed: boolean
): Promise<ReadonlyMap<string, string> | undefined> {
  if (!needed) {
    return undefined;
  }
  try {
    const agents = await client.listAgents();
    return new Map(agents.map((agent) => [agent.session_id, agent.title]));
  } catch {
    return undefined;
  }
}

/** Whether a claim needs the live registry to be named: only a session has a title that moves. */
function holdsSession(claim: IssueClaim | null | undefined): boolean {
  return claim?.actor.kind === "session";
}

type RoutedIssue = Pick<Issue, "route"> & Partial<IssueRouteReach>;

/** Whether the route is a role a live session holds, which the live registry names. */
function routeHeldBySession(issue: RoutedIssue): boolean {
  return issue.route_status === "live" && issue.route?.startsWith("role:") === true;
}

/**
 * An issue's route as every agent surface reads it: where its messages go and whether that
 * reaches anyone, from the `route_status` the server resolved on this read. A route to a role
 * nobody holds, or to a session that is not running, reaches nobody at the moment of the read,
 * so it says so - and says "right now", because a restarting session is absent for minutes.
 */
function routeText(issue: RoutedIssue, titles?: ReadonlyMap<string, string>): string {
  if (issue.route === null) return "none";
  const holder = issue.route_holder ?? null;
  const reach: Record<IssueRouteStatus, string> = {
    live:
      routeHeldBySession(issue) && holder !== null
        ? ` (held by ${titles?.get(holder) ?? holder})`
        : "",
    no_holder: issue.route.startsWith("role:")
      ? " (nobody holds it right now)"
      : " (that session is not running right now)",
    unknown: " (the Envoy listener did not answer, so whether it reaches anyone is unknown)",
  };
  return issue.route + (issue.route_status == null ? "" : reach[issue.route_status]);
}

function issueSummary(
  issue: IssueDetails,
  events: readonly Event[],
  references: IssueReferences | string,
  graph: readonly string[],
  titles?: ReadonlyMap<string, string>
): string {
  const asks = issue.open_asks;
  const spec = issue.artifacts?.find((artifact) => artifact.primary);
  const specApproval = spec === undefined ? undefined : approvalLine(spec);
  if (issue.priority === undefined) throw new Error("Dispatch issue is missing priority");
  if (issue.assignee === undefined) throw new Error("Dispatch issue is missing assignee");
  if (issue.components === undefined) throw new Error("Dispatch issue is missing components");
  if (issue.claim === undefined) throw new Error("Dispatch issue is missing claim");
  if (issue.external_links === undefined) {
    throw new Error("Dispatch issue is missing external_links");
  }
  return [
    `Title: ${issue.title}`,
    `Key: ${issue.key}`,
    `Status: ${issue.status}`,
    `Assignee: ${issue.assignee ?? "unassigned"}`,
    `Claimed by: ${issue.claim === null ? "nobody" : claimText(issue.claim, titles)}`,
    ...(issue.priority === null ? [] : [`Priority: P${issue.priority}`]),
    `Labels: ${issue.labels.length === 0 ? "none" : issue.labels.join(", ")}`,
    componentsLine(issue.components),
    `Route: ${routeText(issue, titles)}`,
    ...(specApproval === undefined
      ? []
      : [`Spec ${specApproval.replace(/^Approval/, "approval")}`]),
    // The links a person or an agent put on the issue — the pull request that delivers it among
    // them — which the issue page renders with their state; the reference graph below carries
    // only links between Dispatch nodes.
    "External links:",
    ...(issue.external_links.length === 0
      ? ["- none"]
      : issue.external_links.map(
          (link) => `- ${link.url}${link.kind === undefined ? "" : ` (${link.kind})`}`
        )),
    "Open asks:",
    ...(asks.length === 0 ? ["- none"] : asks.map((ask) => `- ${ask.id}: ${ask.question}`)),
    "References:",
    ...(typeof references === "string"
      ? [`- ${references}`]
      : references.members.length === 0
        ? ["- none"]
        : references.members.map(
            ({ artifact, depth, via }) =>
              `- ${artifact.project}/${artifact.slug} · depth ${depth} via ${via.kind} ${via.id}`
          )),
    ...(typeof references === "string" || !references.truncated
      ? []
      : ["- more references beyond 8 hops"]),
    "Events:",
    ...(events.length === 0 ? ["- none"] : events.map(eventLine)),
    ...graph,
  ].join("\n");
}

/** One graph edge as a read shows it: edge type, the other node and its address, excerpt, when. */
function referenceLines(edges: readonly GraphEdge[] | string): string[] {
  if (typeof edges === "string") return [`- ${edges}`];
  if (edges.length === 0) return ["- none"];
  return edges.map((edge) => {
    const excerpt = edge.excerpt === undefined ? "" : `${textHead(edge.excerpt.text)} · `;
    return `- ${edge.kind} ${edge.node.kind} ${edge.node.ref ?? edge.node.id} (${excerpt}${edge.created_at})`;
  });
}

/**
 * How a read degrades a graph or closure section the server cannot serve. Dispatch's own 404 is a
 * server without the route; any other failure, a gateway's 404 page included, is named.
 */
function unavailableReason(error: unknown): string {
  return dispatchAnswered(error, 404) ? "unavailable" : `unavailable: ${messageFor(error)}`;
}

async function graphEdges(
  client: DispatchClient,
  query: GraphReferencesQuery
): Promise<GraphEdge[] | string> {
  try {
    return (await client.getReferences(query)).edges;
  } catch (error) {
    return unavailableReason(error);
  }
}

async function issueReferencesOrUnavailable(
  client: DispatchClient,
  key: string
): Promise<IssueReferences | string> {
  try {
    return await client.getIssueReferences(key);
  } catch (error) {
    return unavailableReason(error);
  }
}

/**
 * The two sections every read ends with: `Referenced by:` (edges pointing at the node, mentions
 * and structure alike) and `Links:` (edges it writes), read from the reference graph in both
 * directions. A graph the server cannot serve degrades to one "unavailable" row, like the
 * closure section, so the read itself still answers.
 */
async function graphSections(client: DispatchClient, ref: string): Promise<string[]> {
  const [incoming, outgoing] = await Promise.all([
    graphEdges(client, { to: ref }),
    graphEdges(client, { from: ref }),
  ]);
  return ["Referenced by:", ...referenceLines(incoming), "Links:", ...referenceLines(outgoing)];
}

function logSummary(issue: IssueDetails, events: readonly Event[]): string {
  return [
    `Key: ${issue.key}`,
    "Events:",
    ...(events.length === 0 ? ["- none"] : events.map(eventLine)),
  ].join("\n");
}

/** The head of the text an event carries, so a log reads without opening each item. */
function eventHead(event: Event): string | undefined {
  switch (event.type) {
    case "ask.opened":
    case "ask.anchor_refreshed":
    case "ask.edited":
    case "ask.handed_back":
    case "ask.resolved":
      return textHead(event.payload.question);
    case "ask.answered":
      return `${textHead(event.payload.question)} -> ${textHead(askAnswerText(event.payload.answer))}`;
    case "comment.created":
    case "comment.anchor_refreshed":
    case "comment.edited":
    case "comment.resolved":
    case "comment.reopened":
    case "suggestion.accepted":
    case "suggestion.rejected":
    case "message.created":
    case "message.answered":
      return textHead(event.payload.body);
    case "artifact.version":
      return textHead(
        `${event.payload.name} v${event.payload.version.number}${event.payload.version.summary ? `: ${event.payload.version.summary}` : ""}`
      );
    case "issue.created":
    case "issue.updated":
    case "issue.closed":
      return `status ${event.payload.status}`;
    default:
      return undefined;
  }
}

/** How an event, comment, message or ask author reads in the tools: `<kind> <id>`, plus
 *  ` (as <namespace>/<name>)` — the verified service token's subject through
 *  `serviceSubjectLabel`, which keeps the namespace because every namespace has a `default`
 *  service account — when a service token authenticated the write. A claim's holder is named by
 *  `claimText` instead, which follows the dashboard's session label. */
function actorText(actor: Actor): string {
  const service = actor.kind === "session" ? actor.service : undefined;
  if (service === undefined) {
    return `${actor.kind} ${actor.id}`;
  }
  return `${actor.kind} ${actor.id} (as ${serviceSubjectLabel(service)})`;
}

function eventLine(event: Event): string {
  const head = eventHead(event);
  return `- #${event.seq} ${event.type} · ${actorText(event.actor)} · ${event.created_at}${head === undefined || head === "" ? "" : ` · ${head}`}`;
}

function childrenSummary(issue: IssueDetails): string {
  return [
    `Key: ${issue.key}`,
    "Children:",
    ...(issue.children.length === 0
      ? ["- none"]
      : issue.children.map((child) => `- ${child.key}: ${child.title} (${child.status})`)),
  ].join("\n");
}

function askSummary({ ask, replies }: AskRead, graph: readonly string[]): string {
  const answer = ask.answer;
  const chain = replies.flatMap((reply) => [
    `${reply.id} · ${actorText(reply.author)}`,
    `Body: ${reply.body}`,
  ]);
  return [
    `Question: ${ask.question}`,
    ...anchorLines(ask),
    "Options:",
    ...(ask.options.length === 0
      ? ["- none"]
      : ask.options.map(
          (option) => `- ${option.label}${option.description ? ` — ${option.description}` : ""}`
        )),
    `State: ${ask.state}`,
    "Answer:",
    ...(answer === null
      ? ["- none"]
      : [
          `- By: ${answer.user}`,
          `- Selected: ${answer.selected.length === 0 ? "none" : answer.selected.join(", ")}`,
          ...(answer.text === null ? [] : [`- Text: ${answer.text}`]),
        ]),
    ...(ask.resolution === undefined
      ? []
      : [
          "Resolution:",
          `- By: ${ask.resolution.actor.id}`,
          `- Kind: ${ask.resolution.kind}`,
          `- Reason: ${ask.resolution.reason}`,
        ]),
    "Replies:",
    ...(chain.length === 0 ? ["- none"] : chain),
    ...graph,
  ].join("\n");
}

function openAskAge(ageSeconds: number): string {
  const seconds = Math.max(0, Math.floor(ageSeconds));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m${seconds % 60 === 0 ? "" : ` ${seconds % 60}s`}`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h${minutes % 60 === 0 ? "" : ` ${minutes % 60}m`}`;
  const days = Math.floor(hours / 24);
  return `${days}d${hours % 24 === 0 ? "" : ` ${hours % 24}h`}`;
}

function openAskOwner(ask: OpenAsk): string {
  return "issue" in ask.owner
    ? `${ask.owner.issue.key}: ${ask.owner.issue.title}`
    : `${ask.owner.document.project} / ${ask.owner.document.name}`;
}

function openAskLine(ask: OpenAsk, baseUrl: string): string {
  const priority = ask.priority === null ? "" : `P${ask.priority} · `;
  return `- ${openAskAge(ask.age_seconds)} · ${priority}${openAskOwner(ask)} · ${ask.question} · ${new URL(ask.ref, baseUrl).toString()}`;
}

export function formatOpenAsksSummary(response: OpenAsksResponse, baseUrl: string): string {
  const scope = "active open asks you authored on open issues and project documents";
  if (response.count === 0) {
    return ["There are no unanswered asks for this session.", `Scope: ${scope}.`].join("\n");
  }
  const waitingOnHuman = response.asks.filter((ask) => ask.waiting_on === "human");
  const waitingOnAgent = response.asks.filter((ask) => ask.waiting_on === "agent");
  return [
    `${response.count} unanswered ${response.count === 1 ? "ask" : "asks"} you authored on active issues and project documents.`,
    "",
    `Waiting on human (${waitingOnHuman.length}):`,
    ...waitingOnHuman.map((ask) => openAskLine(ask, baseUrl)),
    "",
    `Waiting on agent (${waitingOnAgent.length}):`,
    ...waitingOnAgent.map((ask) => openAskLine(ask, baseUrl)),
  ].join("\n");
}

function commentSummary({ comment, replies }: CommentRead, graph: readonly string[]): string {
  const root = [
    `${comment.id} · ${actorText(comment.author)}`,
    ...anchorLines(comment),
    `Body: ${comment.body}`,
  ];
  const chain = replies.flatMap((reply) => [
    `${reply.id} · ${actorText(reply.author)}`,
    ...anchorLines(reply),
    `Body: ${reply.body}`,
  ]);
  return [
    "Comment:",
    ...root,
    "Reply chain:",
    ...(chain.length === 0 ? ["- none"] : chain),
    ...graph,
  ].join("\n");
}

/** An anchored record's quote, then where its block stands: the position, or why the read could
 *  not place it. A record without an anchor prints neither. */
function anchorLines(
  record: Pick<Comment, "anchor" | "anchor_block" | "anchor_block_error">
): string[] {
  return [
    ...(record.anchor?.quote === undefined ? [] : [`> ${record.anchor.quote}`]),
    ...(record.anchor_block === undefined
      ? []
      : [`Position: ${positionText(record.anchor_block)}`]),
    ...(record.anchor_block_error === undefined
      ? []
      : [`Position: unavailable (${record.anchor_block_error})`]),
  ];
}

/** Where an anchor's block stands. Every node from the top-level block down reads `type[index]`;
 *  in a table the row and cell read instead as `row 5 (Red-teamer loop), column Due`: the row's
 *  index (0 is the header), labelled by its cells before the anchored column — blank cells and
 *  bare numbers dropped, since a `#` column repeats the index — and the column's header, or its
 *  index where no header cell covers it. */
export function positionText(block: BlockPath): string {
  const { table, path } = block;
  const segments = path.map((entry) => `${entry.type}[${entry.index}]`);
  if (table === undefined || table.row === null) return segments.join(" › ");
  const tableAt = path.findIndex((entry) => entry.type === "table");
  const cells = table.cells ?? [];
  const label = (table.column === null ? cells : cells.slice(0, table.column))
    .map((cell) => cell.trim())
    .filter((cell) => cell !== "" && !/^\d+$/.test(cell))
    .join(" · ");
  const row = label === "" ? `row ${table.row}` : `row ${table.row} (${label})`;
  const header = table.header === null || table.header === "" ? String(table.column) : table.header;
  return [
    ...segments.slice(0, tableAt + 1),
    table.column === null ? row : `${row}, column ${header}`,
  ].join(" › ");
}

function messageSummary({ message, replies }: MessageRead, graph: readonly string[]): string {
  const root = [`${message.id} · ${actorText(message.author)}`, `Body: ${message.body}`];
  const chain = replies.flatMap((reply) => [
    `${reply.id} · ${actorText(reply.author)}`,
    `Body: ${reply.body}`,
  ]);
  return [
    "Message:",
    ...root,
    "Reply chain:",
    ...(chain.length === 0 ? ["- none"] : chain),
    ...graph,
  ].join("\n");
}

async function openArtifactMarks(
  client: DispatchClient,
  resolved: ResolvedArtifact
): Promise<string[]> {
  const asksPromise =
    resolved.owner.kind === "project"
      ? client.getArtifactAsks(resolved.artifact.id)
      : Promise.resolve(resolved.issue?.open_asks ?? []);
  const commentsPromise =
    resolved.owner.kind === "project"
      ? client.getArtifactComments(resolved.artifact.id)
      : client.getComments(resolved.issue?.key ?? "", resolved.artifact.id);
  const commentsResultPromise = commentsPromise.then(
    (value) => ({ status: "fulfilled" as const, value }),
    (reason) => ({ status: "rejected" as const, reason })
  );
  // An ask error takes precedence over a comment error. Capture a concurrent comment failure
  // so it is handled without delaying an ask failure.
  const asks = await asksPromise;
  const marks = asks
    .filter((ask) => ask.state === "open" && ask.anchor?.artifact_id === resolved.artifact.id)
    .map((ask) => `ask ${ask.id}`);
  const commentsResult = await commentsResultPromise;
  if (commentsResult.status === "rejected") {
    if (dispatchAnswered(commentsResult.reason, 404)) return marks;
    throw commentsResult.reason;
  }
  return [
    ...marks,
    ...commentsResult.value
      .filter(
        (comment) => !comment.resolved && comment.anchor?.artifact_id === resolved.artifact.id
      )
      .map((comment) => `comment ${comment.id}`),
  ];
}

/** The asks of the document's `ask` blocks. An issue lists them under the issue, since the artifact
 * route refuses an issue document; an unlinked document lists its own. */
async function blockAsks(
  client: DispatchClient,
  resolved: ResolvedArtifact,
  state?: "all" | "open" | "answered"
): Promise<Array<Ask & { readonly block_id: string }>> {
  const asks = await (resolved.issue === undefined
    ? client.getArtifactAsks(resolved.artifact.id, state)
    : client.listIssueAsks(resolved.issue.key, state));
  return asks.filter(
    (ask): ask is Ask & { readonly block_id: string } =>
      typeof ask.block_id === "string" && ask.block_artifact?.id === resolved.artifact.id
  );
}

/**
 * Refuses an approval request while the document holds an open decision block. A request names
 * the latest version, and a new version moves it to that version and leaves it waiting on its
 * agent, so a request over a block the human has yet to answer leaves their turn the moment they
 * answer it. The live document's `ask` blocks are judged by the latest version, the one the
 * request would name: a block that version shows open counts as open even when its ask is already
 * answered or closed, since that answer reaches a version only when the document settles, about
 * two seconds later, or with the next edit (the agent's fold of the answer into the text). A block
 * not yet in that version counts as open too. A block removed from the document is not judged
 * here; `refuseRemovingOpenDecisionBlocks` keeps one whose ask is open in it. A document already
 * approved at its latest version is left to the server, which answers with that approval.
 */
async function refuseOpenDecisionBlocks(
  client: DispatchClient,
  tool: string,
  resolved: ResolvedArtifact
): Promise<void> {
  const artifact = resolved.artifact;
  const latest = artifact.approval?.latest_version;
  // No approval state means no document version to approve: the server's own refusal says so.
  if (latest === undefined || latest < 1 || artifact.approval?.state === "approved") return;
  const blocks = (await client.artifactBlocks(artifact.id)).filter((block) => block.type === "ask");
  if (blocks.length === 0) return;
  const [documentAsks, version] = await Promise.all([
    blockAsks(client, resolved),
    client.docRead(artifact.id, latest),
  ]);
  const asks = new Map(documentAsks.map((ask) => [ask.block_id, ask]));
  const lines = version.markdown.split("\n");
  const open = blocks.flatMap((block) => {
    const ask = asks.get(block.id);
    const named =
      ask === undefined
        ? `block ${block.id}`
        : `${JSON.stringify(ask.question)} (block ${block.id}, ask ${ask.id})`;
    // The block's opening lines, `:::ask{#<id> … state="…"}`. Every match counts, so a line that
    // quotes the opener (in code, say) can add an open block but never hide one; a block none of
    // whose lines carries a state is open.
    const states = lines
      .filter((line) => line.includes(`ask{#${block.id} `) || line.includes(`ask{#${block.id}}`))
      .map((line) => /\bstate="(\w+)"/.exec(line)?.[1]);
    if (states.length === 0) return [`${named}, which version ${latest} does not hold yet`];
    if (!states.includes("open") && states.some((state) => state !== undefined)) return [];
    if (ask === undefined) return [`${named}, whose ask Dispatch has not opened yet`];
    if (ask.state === "open") return [named];
    // An answered block's answer is folded in; a waived one (resolved) gets the decision written in.
    const next =
      ask.state === "answered"
        ? "fold the answer into the text"
        : "write the decision into the text";
    return [
      `${named}, ${ask.state} but still open in version ${latest}: ${next} with dispatch_doc_edit, which writes a version that carries it`,
    ];
  });
  if (open.length === 0) return;
  const count = open.length === 1 ? "1 open decision block" : `${open.length} open decision blocks`;
  throw new Error(
    [
      `${tool} was not called: ${artifact.name} (version ${latest}) has ${count}. Answering one writes a new version, which would move this request to that version and leave it waiting on you.`,
      ...open.map((line) => `- ${line}`),
      "Do not request approval over an open block, even when a human asked for it. Tell the human which block is open and ask them to answer it or to waive it. Once it is answered, fold the answer into the text with dispatch_doc_edit. If they waive it, close the block with dispatch_resolve_ask (kind resolved, their words as the reason) and write their decision into the text with dispatch_doc_edit. Then request approval again once the human has agreed to every point in the new version: the call opens the request, or hands an open one back to the human.",
    ].join("\n")
  );
}

/**
 * Refuses a document edit that would take a decision block out of the document while its ask is
 * open: a `delete` of the block or of a block holding it, or a `retype` of it into another type.
 * The edit writes its version at once; about two seconds later settlement retracts the ask in the
 * system's name and writes no version, so the human's question leaves the Inbox unanswered and an
 * approval request made after it names a version with no open block for
 * `refuseOpenDecisionBlocks` to find. An `insert` in the same batch that carries the block's id
 * does not exempt it: telling a block written back from an opener quoted in code needs the
 * server's parser, so the executor fails closed, as `refuseOpenDecisionBlocks` does for a quoted
 * opener. An open block is reworded with `replace`, moved with `move`, or, when this session asked
 * it, has its question, options, urgency or multiple changed with `dispatch_edit_ask`; each keeps
 * it. Costs nothing for an edit with no such operation, then one
 * `GET /artifacts/{id}/blocks`, and the owner's asks only when an operation reaches an `ask` block.
 */
async function refuseRemovingOpenDecisionBlocks(
  client: DispatchClient,
  tool: string,
  resolved: ResolvedArtifact,
  ops: readonly EditOp[]
): Promise<void> {
  const removing = ops.filter(
    (operation) =>
      operation.block !== undefined &&
      (operation.op === "delete" || (operation.op === "retype" && operation.type !== "ask"))
  );
  if (removing.length === 0) return;
  const artifact = resolved.artifact;
  const blocks = await client.artifactBlocks(artifact.id);
  const askBlocks = blocks.filter((block) => block.type === "ask");
  const removed = new Set<string>();
  for (const operation of removing) {
    const target = blocks.find((block) => block.id === operation.block);
    if (target === undefined) continue;
    // A delete takes every block inside the one it names; a retype changes only that block.
    for (const block of askBlocks) {
      if (
        block.id === target.id ||
        (operation.op === "delete" && block.from >= target.from && block.to <= target.to)
      ) {
        removed.add(block.id);
      }
    }
  }
  if (removed.size === 0) return;
  const asks = await blockAsks(client, resolved, "open");
  const open = asks.filter((ask) => removed.has(ask.block_id));
  if (open.length === 0) return;
  const [what, question] =
    open.length === 1
      ? ["a decision block whose ask is", "question"]
      : [`${open.length} decision blocks whose asks are`, "questions"];
  throw new Error(
    [
      `${tool} was not called: it would remove ${what} still open, and the human's ${question} would leave their Inbox unanswered.`,
      ...open.map(
        (ask) => `- ${JSON.stringify(ask.question)} (block ${ask.block_id}, ask ${ask.id})`
      ),
      "A decision block leaves the document once its ask is answered or resolved. Until then, reword it with replace, relocate it with move, or change its question, options, urgency or multiple with dispatch_edit_ask if you asked it; each keeps it.",
    ].join("\n")
  );
}

/**
 * A Dispatch refusal carrying its own code in the message the host shows: the code
 * (ISSUE_CLAIMED, CLAIM_CONTENDED, EXTERNAL_LINK_TAKEN, ...) is the part an agent acts on, and
 * the prose alone hides it. Only the message changes: every other field of the refusal
 * (`candidates`, `current`, `mismatches`) rides along, since a caller reads them off the error
 * it catches. An error that is not a refusal is returned as it is, so a caller's
 * `throw refusalWithCode(error)` rethrows it untouched. This returns rather than throws: a
 * helper that never returns leaves its switch case with no visible terminator, which Biome's
 * noFallthroughSwitchClause rejects.
 *
 * A gateway's answer stays a `DispatchGatewayError`, and the caller's account of what its call did
 * joins it in one sentence. Where the request may have reached Dispatch (`mayHaveReachedDispatch`),
 * the client's advice could judge only from the method (a write is told to check whether it took
 * effect), so that account, which knows what its call did, takes its place and must say what to do;
 * otherwise the request never reached Dispatch, and the account follows the client's advice.
 */
function refusalWithCode(error: unknown, ...clauses: string[]): unknown {
  const suffix = clauses.filter((clause) => clause !== "").join("; ");
  const joined = suffix === "" ? "" : `; ${suffix}`;
  if (error instanceof DispatchGatewayError) {
    let told = error.message;
    if (joined !== "") {
      told = error.mayHaveReachedDispatch
        ? `${error.answer}${joined}`
        : `${error.answer}, so ${error.advice}${joined}`;
    }
    return new DispatchGatewayError(
      error.status,
      error.answer,
      error.advice,
      `${error.code}: ${told}`
    );
  }
  if (!(error instanceof DispatchServiceError)) return error;
  return new DispatchServiceError(
    error.code,
    error.status,
    `${error.code}: ${error.message}${joined}`,
    error.candidates,
    error.current,
    error.mismatches
  );
}

/** Whether Dispatch itself answered `status`; a gateway's answer of that status says nothing it decided. */
function dispatchAnswered(error: unknown, status: number): error is DispatchServiceError {
  return error instanceof DispatchServiceError && error.fromDispatch && error.status === status;
}

/**
 * Whether a write that failed with `error` may have taken effect anyway. Only an answer sent
 * before the write could apply proves it did not: Dispatch's own refusal (a 4xx), or a gateway's
 * answer that never reached Dispatch (`mayHaveReachedDispatch`). Dispatch's own 5xx can come after
 * it committed, and a timeout or a transport error says nothing either way.
 */
function writeMayHaveLanded(error: unknown): boolean {
  if (error instanceof DispatchGatewayError) return error.mayHaveReachedDispatch;
  return !(error instanceof DispatchServiceError) || error.status >= 500;
}

/**
 * An error that is no refusal (a timeout, a transport error) with `account`, the caller's clause
 * on what its call did, after its message: joined to it with "; ", or as a sentence of its own
 * after a message that ends one ("The operation timed out.", "… able to access the url?").
 */
function withAccount(error: unknown, account: string): Error {
  const message = messageFor(error);
  const told = /[.!?]$/.test(message)
    ? `${message} ${account.charAt(0).toUpperCase()}${account.slice(1)}`
    : `${message}; ${account}`;
  return new Error(told, { cause: error });
}

/** Validate and execute one native Dispatch tool against the JSON HTTP API. */
export async function executeDispatchTool(
  input: ExecuteDispatchToolInput
): Promise<DispatchToolResult> {
  const configUrl = input.config.url;
  const configToken = input.config.token;
  if (!input.config.enabled || !configUrl || !configToken) {
    throw new Error("Dispatch is disabled; resolve both DISPATCH_URL and DISPATCH_TOKEN");
  }
  const baseFetch = input.fetchImpl ?? fetch;
  const fetchImpl = Object.assign(
    async (...args: Parameters<typeof fetch>): Promise<Response> => {
      try {
        return await baseFetch(...args);
      } catch (error) {
        if (error instanceof TypeError) {
          throw new Error(
            `Dispatch at ${configUrl} is unreachable: ${error.message}. If the Dispatch URL changed, restart this agent process so it picks up the new configuration.`
          );
        }
        throw error;
      }
    },
    { preconnect: baseFetch.preconnect }
  );
  const env = input.env ?? process.env;
  const exec = input.exec ?? defaultExec;
  // One validation pass: owner resolution, the strict schema, and the cross-field hand
  // checks all push onto `problems`; the call is refused once with every problem listed,
  // or proceeds with arguments the schema has fully validated (unknown keys and wrong
  // types are impossible below, so structured arguments only need the contract's shape named).
  const problems: string[] = [];
  const ownerArguments = await resolveOwnerArguments(
    input.tool,
    input.args,
    input.cwd,
    env,
    exec,
    configUrl,
    problems
  );
  // Every owner-missing exit (no issue, bad LEGION_ISSUE, no repo in cwd) leaves owner null;
  // the schema's own "issue is required" would only restate the actionable line already pushed.
  const ownerMissing = issueFreeTools[input.tool] !== true && ownerArguments.owner === null;
  const schema = toolSchema(input.tool);
  const parsed = schema.safeParse(ownerArguments.args, { reportInput: true });
  if (!parsed.success) {
    // When the owner is already reported missing, the schema's own "issue is required"
    // (a required `issue` field, or the owner refine) restates it; keep the actionable line.
    const issues = parsed.error.issues.filter(
      (issue) =>
        !ownerMissing ||
        !(
          (issue.code === "invalid_type" &&
            issue.path.length === 1 &&
            issue.path[0] === "issue" &&
            issue.input === undefined) ||
          (issue.code === "custom" &&
            issue.path.length === 0 &&
            (issue.message.startsWith("Exactly one of issue and project is required") ||
              issue.message.startsWith("issue is required unless in_reply_to")))
        )
    );
    problems.push(...formatZodIssues(issues, schema));
  }
  problems.push(...argumentProblems(input.tool, ownerArguments.args));
  if (problems.length > 0) throw new ToolInputError(input.tool, problems);
  // A factory, not one instance: the constructor starts the request deadline, and the main
  // path resolves the origin (a subprocess) before it needs a client.
  const dispatchClient = (): DispatchClient =>
    new DispatchClient(configUrl, configToken, fetchImpl, input.signal);
  if (input.tool === "dispatch_open_asks") {
    const client = dispatchClient();
    const project = optionalString(ownerArguments.args, "project");
    if (project !== undefined) {
      const response = await client.openAsksForProject(project);
      return { text: formatOpenAsksSummary(response, configUrl), details: { ...response } };
    }
    const sessionId = input.sessionId?.trim();
    if (!sessionId) throw new Error("host session id is required for dispatch_open_asks");
    const response = await client.openAsks(sessionId);
    return { text: formatOpenAsksSummary(response, configUrl), details: { ...response } };
  }
  if (input.tool === "dispatch_whoami") {
    const sessionId = input.sessionId?.trim();
    if (!sessionId) throw new Error("host session id is required for dispatch_whoami");
    const client = dispatchClient();
    const identity = await client.whoami();
    const owner = identity.kind === "agent" ? identity.owner : identity.login.toLowerCase();
    // A Dispatch that predates verified service tokens omits the field entirely.
    const service = identity.kind === "agent" ? (identity.service ?? null) : null;
    const unowned =
      "no owner, so issues you create without an assignee are unassigned (or inherit their parent's).";
    return {
      text:
        owner !== null
          ? `Session ${sessionId} acts for ${owner}: issues you create without an assignee are assigned to ${owner}.`
          : service !== null
            ? // The text names the identity the way every other surface does; details
              // keeps the subject whole, because that is the persisted value.
              `Session ${sessionId} runs as service ${serviceSubjectLabel(service)}: ${unowned}`
            : `Session ${sessionId} runs under the shared token: ${unowned}`,
      details: { session: sessionId, owner, service },
    };
  }
  const args = (parsed.success ? parsed.data : ownerArguments.args) as ToolArguments;
  const actor = toolActor(await resolveOrigin(env, exec, input.cwd), input);
  const client = dispatchClient();
  // The document part of the call's dispatch:// reference is a slug by definition; a document
  // argument the reference does not supply is a bare one.
  const refDocument =
    ownerArguments.ref?.kind === "artifact" ? ownerArguments.ref.id : ownerArguments.ref?.artifact;
  const resolveDocument = (documentOwner: Owner, reference: string | undefined) =>
    resolveArtifact(client, documentOwner, reference, {
      canonical: reference !== undefined && reference === refDocument,
    });
  const owner =
    ownerArguments.owner?.kind === "issue"
      ? {
          kind: "issue" as const,
          issue: await resolveExistingIssue(client, ownerArguments.owner.issue),
        }
      : ownerArguments.owner;
  const issue = () => {
    if (owner?.kind !== "issue") throw new Error("issue is required");
    return owner.issue;
  };
  const documentOwner = () => {
    if (owner === null) throw new Error("issue or project is required");
    return owner;
  };

  switch (input.tool) {
    case "dispatch_issue": {
      const project = stringArg(args, "project");
      const title = stringArg(args, "title");
      const parent = optionalString(args, "parent");
      const external = optionalString(args, "external");
      const force = optionalBoolean(args, "force");
      const spec = optionalString(args, "spec");
      const priority = optionalPriority(args, "priority");
      const assignee = optionalString(args, "assignee");
      const components = optionalComponents(args, "components");
      const labels = args.labels;
      const blockedBy = args.blocked_by;
      try {
        const created = await client.issue({
          project,
          title,
          ...(parent === undefined ? {} : { parent }),
          ...(Array.isArray(blockedBy) ? { blocked_by: blockedBy as string[] } : {}),
          ...(external === undefined ? {} : { external }),
          ...(force === undefined ? {} : { force }),
          ...(spec === undefined ? {} : { spec }),
          ...(priority === undefined ? {} : { priority }),
          ...(assignee === undefined ? {} : { assignee }),
          ...(components === undefined ? {} : { components }),
          ...(Array.isArray(labels) ? { labels: labels as string[] } : {}),
          actor,
        });
        const componentGuidance = await architectureGuidance(
          client,
          created.project,
          created.components
        );
        const adviceLines = [
          ...renderAdvice(input.tool, created.key, created.advice, {
            isPrimarySpec: spec !== undefined,
          }),
          ...(componentGuidance === undefined ? [] : [componentGuidance]),
        ];
        return {
          text: [
            `Created ${created.key}: ${created.title} ${notSubscribed(issueTopic(created.key))}`,
            ...adviceLines,
          ].join("\n"),
          details: {
            issue: created.key,
            ...(created.advice === undefined ? {} : { advice: created.advice }),
          },
        };
      } catch (error) {
        if (!(error instanceof DispatchServiceError) || error.code !== "POSSIBLE_DUPLICATE") {
          throw error;
        }
        const candidates = duplicateCandidates(error);
        return {
          text: [
            `Not created: "${title}" looks like a duplicate.`,
            ...candidates.map((candidate) => {
              const href = new URL(candidate.href, configUrl).toString();
              return `${candidate.key} [${candidate.status}] ${candidate.title} → ${href}`;
            }),
            "Reference the existing issue, or call dispatch_issue again with force: true after reading it.",
          ].join("\n"),
          details: { duplicates: candidates },
        };
      }
    }
    case "dispatch_issue_update": {
      const issueKey = issue();
      const status = optionalString(args, "status");
      const reason = optionalString(args, "reason");
      const title = optionalString(args, "title");
      const route = optionalString(args, "route");
      const parent = optionalString(args, "parent");
      const components = optionalComponents(args, "components");
      const priority = optionalPriority(args, "priority");
      const labels = Array.isArray(args.labels) ? (args.labels as string[]) : undefined;
      const blockedBy = Array.isArray(args.blocked_by) ? (args.blocked_by as string[]) : undefined;
      const requestedLinks = Array.isArray(args.external_links)
        ? [...new Set(args.external_links as string[])]
        : undefined;
      let before: IssueDetails;
      try {
        before = await client.getIssue(issueKey);
      } catch (error) {
        throw refusalWithCode(error);
      }
      // The schema admits reason only beside status done. A closed issue refuses messages,
      // comments, and artifacts, so the reason is posted first and the close waits on it.
      let closingNote: { readonly id: string; readonly ref: string } | undefined;
      if (reason !== undefined) {
        try {
          const message = await client.message(issueKey, { body: reason, actor });
          closingNote = {
            id: message.id,
            ref: dispatchChildRef(dispatchIssueRef(issueKey), "message", message.id),
          };
        } catch (error) {
          // The close's PATCH below follows the same rule: only an answer sent before the reason
          // could be stored proves it was not posted.
          const told = writeMayHaveLanded(error)
            ? "the reason may or may not have been posted, and the close was not sent: read the issue's messages before retrying, since retrying this call posts its reason again"
            : "the reason was not posted, so the close was not sent";
          if (error instanceof DispatchServiceError) throw refusalWithCode(error, told);
          throw withAccount(error, told);
        }
      }
      // The server replaces the whole link set; the common call is "link the pull request
      // I just opened", so merge by URL and keep every existing link (and its kind).
      const linked = before.external_links.map((link) => link.url);
      const newLinks = requestedLinks?.filter((url) => !linked.includes(url)) ?? [];
      let after: Advised<Issue>;
      try {
        after = await client.updateIssue(issueKey, {
          ...(status === undefined ? {} : { status }),
          ...(title === undefined ? {} : { title }),
          ...(labels === undefined ? {} : { labels }),
          ...(priority === undefined ? {} : { priority }),
          ...(route === undefined ? {} : { route }),
          ...(parent === undefined ? {} : { parent: parent === "" ? null : parent }),
          ...(blockedBy === undefined ? {} : { blocked_by: blockedBy }),
          ...(components === undefined ? {} : { components }),
          ...(requestedLinks === undefined
            ? {}
            : { external_links: [...before.external_links, ...newLinks.map((url) => ({ url }))] }),
          actor,
        });
      } catch (error) {
        // A URL links exactly one issue. A server from before EXTERNAL_LINK_TAKEN answers the
        // unique-index violation with 500 INTERNAL, which names nothing; say what it means. A
        // gateway's 500 page is not that answer, and gets no such reading.
        const taken =
          dispatchAnswered(error, 500) && newLinks.length > 0
            ? `one of ${newLinks.join(", ")} may already be linked from another issue (a URL links exactly one issue)`
            : "";
        if (closingNote === undefined) throw refusalWithCode(error, taken);
        // The reason is on the issue, so a blind retry would post it a second time: the error
        // says where the first one is, and whether the close may have landed anyway. A gateway's
        // timeout or rate limit refused nothing there is to fix.
        const posted = `the reason already landed as message ${closingNote.id} (${closingNote.ref})`;
        const fix =
          error instanceof DispatchGatewayError && error.transient
            ? ""
            : "fix what refused the close, then ";
        const landed = writeMayHaveLanded(error)
          ? `${posted}, and the close may or may not have taken effect. Read the issue's status before retrying: done means it closed; otherwise retry with a reason that points at message ${closingNote.id}, since retrying this call posts its reason again`
          : `${posted} but the issue did not close. Retrying this call posts its reason again, so ${fix}retry with a reason that points at message ${closingNote.id}`;
        if (error instanceof DispatchServiceError) throw refusalWithCode(error, taken, landed);
        throw withAccount(error, landed);
      }
      const linkCount = `(${after.external_links.length} ${after.external_links.length === 1 ? "link" : "links"})`;
      const changes = [
        ...(closingNote === undefined
          ? []
          : [`reason posted as message ${closingNote.id} (${closingNote.ref})`]),
        ...(status === undefined ? [] : [`status ${before.status} -> ${after.status}`]),
        ...(title === undefined ? [] : [`title "${after.title}"`]),
        ...(labels === undefined
          ? []
          : [after.labels.length === 0 ? "labels cleared" : `labels ${after.labels.join(", ")}`]),
        ...(priority === undefined
          ? []
          : [after.priority === null ? "priority cleared" : `priority -> P${after.priority}`]),
        ...(requestedLinks === undefined
          ? []
          : [
              newLinks.length === 0
                ? `already linked ${requestedLinks.join(", ")} ${linkCount}`
                : `linked ${newLinks.join(", ")} ${linkCount}`,
            ]),
        ...(route === undefined
          ? []
          : [after.route === null ? "route cleared" : `route ${after.route}`]),
        ...(parent === undefined
          ? []
          : [after.parent === null ? "parent cleared" : `parent -> ${after.parent}`]),
        ...(blockedBy === undefined
          ? []
          : [
              after.blocked_by.length === 0
                ? "blocked_by cleared"
                : `blocked by ${after.blocked_by.join(", ")}`,
            ]),
        ...(components === undefined ? [] : [componentsChange(components, after.components)]),
      ];
      const adviceLines = renderAdvice(input.tool, after.key, after.advice, {
        setsStatus: status !== undefined,
      });
      return {
        text: [
          `${after.key}: ${changes.join("; ")} ${notSubscribed(issueTopic(after.key))}`,
          ...adviceLines,
        ].join("\n"),
        details: {
          issue: after.key,
          status: after.status,
          external_links: after.external_links.map((link) => link.url),
          ...(closingNote === undefined ? {} : { message: closingNote.id }),
          ...(after.advice === undefined ? {} : { advice: after.advice }),
        },
      };
    }
    case "dispatch_claim": {
      const issueKey = issue();
      const release = optionalBoolean(args, "release") ?? false;
      let after: Issue;
      try {
        after = release
          ? await client.releaseIssueClaim(issueKey, { actor })
          : await client.claimIssue(issueKey, { actor });
      } catch (error) {
        // ISSUE_CLAIMED and CLAIM_CONTENDED are two different refusals with two different
        // answers, and the tool description and the skill both name the codes.
        throw refusalWithCode(error);
      }
      const held = after.claim;
      // A release answers with the claim cleared or it does not answer at all: the server
      // refuses a release the caller may not make (409 ISSUE_CLAIMED, naming the live holder),
      // and loads the issue inside the releasing transaction. So there is no "released but
      // still claimed" state to render here.
      let text: string;
      if (release) {
        text = `${issueKey}: claim released; nobody is working it now. Its status is still ${after.status} — move it yourself if that is no longer where the work is.`;
      } else {
        if (held === null) {
          // A successful claim always answers with the claim in place; anything else is a
          // server that no longer matches this contract, worth saying rather than papering over.
          throw new Error(`Dispatch claimed ${issueKey} but answered with no claim`);
        }
        text = `${issueKey}: claimed by you since ${held.at}. Its status is ${after.status}; a claim moves nothing, so move it to in_progress with dispatch_issue_update when you start, and release the claim when you stop.`;
      }
      return {
        text: [text, notSubscribed(issueTopic(issueKey))].join("\n"),
        details: { issue: issueKey, status: after.status, claim: held },
      };
    }
    case "dispatch_search": {
      const query = stringArg(args, "query");
      const project = optionalString(args, "project");
      const limit = optionalNumber(args, "limit");
      const search = await client.search(query, {
        ...(project === undefined ? {} : { project }),
        ...(limit === undefined ? {} : { limit }),
      });
      const results = search.results;
      const count = results.length;
      return {
        text:
          count === 0
            ? `No results for "${query}".`
            : [
                `${count} ${count === 1 ? "result" : "results"} for "${query}" (${search.took_ms} ms)`,
                ...results.map((result) => searchResultLine(result, configUrl)),
              ].join("\n"),
        details: { query, results },
      };
    }
    case "dispatch_issues": {
      const project = stringArg(args, "project");
      const status = optionalString(args, "status");
      const parent = optionalString(args, "parent");
      const label = optionalString(args, "label");
      const priority = optionalPriorityFilter(args, "priority");
      const updatedSince = optionalString(args, "updated_since");
      // The zod spec already refused anything but one of ISSUE_ROUTE_STATUSES.
      const routeStatus = optionalString(args, "route_status") as IssueRouteStatus | undefined;
      const page = await client.listIssuePage(
        {
          project,
          ...(status === undefined ? {} : { status }),
          ...(parent === undefined ? {} : { parent }),
          ...(label === undefined ? {} : { label }),
          ...(priority === undefined ? {} : { priority }),
          ...(updatedSince === undefined ? {} : { updated_since: updatedSince }),
          ...(routeStatus === undefined ? {} : { route_status: routeStatus }),
        },
        // The tool's schema has already refused a limit outside 1..MAX_ISSUE_PAGE_LIMIT and a
        // negative or fractional offset.
        {
          limit: optionalNumber(args, "limit") ?? DEFAULT_ISSUE_PAGE_LIMIT,
          offset: optionalNumber(args, "offset") ?? 0,
        }
      );
      const { total, limit, offset } = page;
      const rows = page.issues.map((row) => ({
        key: row.key,
        title: row.title,
        status: row.status,
        priority: row.priority,
        parent: row.parent,
        labels: row.labels ?? [],
        open_asks: row.open_asks,
        claim: row.claim ?? null,
        route: row.route ?? null,
        route_status: row.route_status ?? null,
        route_holder: row.route_holder ?? null,
        updated_at: row.updated_at,
      }));
      const titles = await liveSessionTitles(
        client,
        rows.some((row) => holdsSession(row.claim))
      );
      const isPartial = offset !== 0 || rows.length !== total;
      const showing = !isPartial
        ? ""
        : rows.length === 0
          ? `showing 0-0 of ${total}`
          : `showing ${offset + 1}-${offset + rows.length} of ${total}`;
      return {
        text:
          rows.length === 0
            ? `No issues in ${project}.${isPartial ? ` (${showing})` : ""}`
            : [
                `${rows.length} ${rows.length === 1 ? "issue" : "issues"} in ${project}` +
                  (isPartial ? ` (${showing})` : ""),
                ...rows.map(
                  (row) =>
                    `${row.key} [${row.status}]${row.priority === null ? "" : ` P${row.priority}`} ${row.title}` +
                    (row.open_asks === 0
                      ? ""
                      : ` · ${row.open_asks} open ${row.open_asks === 1 ? "ask" : "asks"}`) +
                    (row.claim === null ? "" : ` · claimed by ${claimText(row.claim, titles)}`) +
                    // A route that reaches a live session changes nothing about the row; one
                    // that reaches nobody, or cannot be judged, is what the owner audit reads.
                    (row.route === null || row.route_status === "live" || row.route_status === null
                      ? ""
                      : ` · route ${routeText(row)}`)
                ),
              ].join("\n"),
        details: { issues: rows, total, offset, limit },
      };
    }
    case "dispatch_architecture_sync": {
      const project = stringArg(args, "project");
      const source = await client.syncArchitectureSource(project);
      const at = source.last_sync_at ?? "unknown time";
      return {
        text:
          source.last_error === null
            ? `Synced ${project} architecture from ${source.repo}@${source.branch}: commit ${source.last_commit ?? "unknown"} (${at}).`
            : [
                `Sync failed for ${project} (${source.repo}@${source.branch}): ${source.last_error}`,
                source.last_commit === null
                  ? "No model has ever imported for this project."
                  : `The previous model stays up (commit ${source.last_commit}).`,
              ].join("\n"),
        details: {
          project,
          repo: source.repo,
          branch: source.branch,
          commit: source.last_commit,
          error: source.last_error,
        },
      };
    }
    case "dispatch_resolve_ask": {
      const kind = stringArg(args, "kind") as "retracted" | "resolved";
      const id = await resolveAskArgument(input.tool, args, client, input.sessionId);
      const ask = await client.resolveAsk(id, {
        kind,
        reason: stringArg(args, "reason"),
        actor,
      });
      if (ask.resolution === undefined) throw new Error("resolved ask is missing its resolution");
      return {
        text: `${kind === "retracted" ? "Retracted" : "Resolved"} ask ${ask.id}: ${ask.resolution.reason}`,
        details: await askOwnerDetails(client, ask),
      };
    }
    case "dispatch_resolve_comment": {
      const reference = stringArg(args, "comment");
      // `argumentProblems` refused any dispatch:// value that is not a comment reference.
      const ref = reference.startsWith("dispatch://") ? parseDispatchRef(reference) : null;
      let id = reference;
      let document: Artifact | undefined;
      if (ref?.owner.kind === "issue") {
        const issueKey = ref.owner.issue;
        id = await resolveIdPrefix(input.tool, "comment", ref.id, refOwnerName(ref), () =>
          client.getComments(issueKey)
        );
      } else if (ref !== null) {
        const artifact = (
          await resolveArtifact(client, ref.owner, ref.artifact, { canonical: true })
        ).artifact;
        document = artifact;
        id = await resolveIdPrefix(input.tool, "comment", ref.id, refOwnerName(ref), () =>
          client.getArtifactComments(artifact.id)
        );
      }
      const comment = await client.resolveComment(id, actor);
      const { label, details } = await commentOwnerResult(client, comment, document);
      return { text: `Resolved comment ${comment.id} on ${label}.`, details };
    }
    case "dispatch_ask": {
      const anchorArgs = asObject(args.anchor);
      const owner = documentOwner();
      const artifactReference =
        optionalString(args, "artifact") ??
        (anchorArgs === null ? undefined : optionalString(anchorArgs, "artifact"));
      const resolved =
        owner.kind === "project" || artifactReference === undefined
          ? owner.kind === "project"
            ? await resolveDocument(owner, artifactReference)
            : undefined
          : await resolveDocument(owner, artifactReference);
      const options = args.options;
      const multiple = optionalBoolean(args, "multiple");
      const urgency = askUrgency(args);
      const anchored = anchorArgs && resolved ? anchor(resolved.artifact, anchorArgs) : undefined;
      const askInput = {
        question: askQuestionWithRef(args),
        ...(Array.isArray(options)
          ? { options: options as NonNullable<CreateAskInput["options"]> }
          : {}),
        ...(multiple === undefined ? {} : { multiple }),
        ...(urgency === undefined ? {} : { urgency }),
        ...(anchored === undefined ? {} : { anchor: anchored }),
        actor,
      };
      const ask =
        resolved?.owner.kind === "project"
          ? await client.artifactAsk(resolved.artifact.id, askInput)
          : await client.ask(issue(), askInput);
      const askOwner =
        ask.issue_key !== null
          ? issueTopic(ask.issue_key)
          : resolved === undefined
            ? issueTopic(issue())
            : documentTopic(resolved.artifact);
      const adviceLines = renderAdvice(input.tool, askOwner.label, ask.advice, {});
      return {
        text: [
          `Asked ${ask.id} on ${askOwner.label} (urgency ${ask.urgency}): ${ask.question}\n${followsAsk(askOwner)}`,
          ...adviceLines,
        ].join("\n"),
        details: {
          ...(await followedAskDetails(client, ask, resolved?.artifact)),
          ...(ask.advice === undefined ? {} : { advice: ask.advice }),
        },
      };
    }
    case "dispatch_edit_ask": {
      const question = optionalString(args, "question");
      const options = args.options;
      const multiple = optionalBoolean(args, "multiple");
      const urgency = askUrgency(args);
      const id = await resolveAskArgument(input.tool, args, client, input.sessionId);
      const ask = await client.editAsk(id, {
        ...(question === undefined ? {} : { question }),
        ...(Array.isArray(options)
          ? { options: options as NonNullable<EditAskInput["options"]> }
          : {}),
        ...(multiple === undefined ? {} : { multiple }),
        ...(urgency === undefined ? {} : { urgency }),
        actor,
      });
      return {
        text: `Ask edited: ${ask.question}`,
        details: await askOwnerDetails(client, ask),
      };
    }
    case "dispatch_comment": {
      const artifactReference = optionalString(args, "artifact");
      const owner = documentOwner();
      const resolved =
        owner.kind === "project" || artifactReference === undefined
          ? owner.kind === "project"
            ? await resolveDocument(owner, artifactReference)
            : undefined
          : await resolveDocument(owner, artifactReference);
      const anchored = resolved ? anchor(resolved.artifact, args) : undefined;
      const replyTo = optionalString(args, "reply_to");
      const replyToAskReference = optionalString(args, "reply_to_ask");
      const replyToAsk =
        replyToAskReference === undefined ? undefined : normalizeUUID(replyToAskReference);
      if (replyToAskReference !== undefined && replyToAsk === undefined) {
        throw new ToolInputError(input.tool, [
          await invalidReplyToAskProblem(client, owner, resolved),
        ]);
      }
      // The schema already refused reply_to alongside reply_to_ask, turn without reply_to_ask,
      // and a turn outside agent|human, so the value is the contract's shape.
      const requestedTurn = optionalString(args, "turn") as CreateCommentInput["turn"];
      const commentInput: CreateCommentInput = {
        body: stringArg(args, "body"),
        ...(anchored === undefined ? {} : { anchor: anchored }),
        ...(replyTo === undefined ? {} : { reply_to: replyTo }),
        ...(replyToAsk === undefined ? {} : { ask_id: replyToAsk }),
        ...(requestedTurn === undefined ? {} : { turn: requestedTurn }),
        actor,
      };
      const comment =
        resolved?.owner.kind === "project"
          ? await client.artifactComment(resolved.artifact.id, commentInput)
          : await client.comment(issue(), commentInput);
      const commentOwner = resolved === undefined ? issueTopic(issue()) : resolvedTopic(resolved);
      const commentDetails =
        resolved === undefined
          ? { issue: comment.issue_key, comment: comment.id }
          : writeResultDetails(resolved, { comment: comment.id });
      const adviceLines = renderAdvice(input.tool, commentOwner.label, comment.advice, {
        isAskReply: replyToAsk !== undefined,
        replyToOwnAsk:
          replyToAsk !== undefined &&
          (comment.advice?.your_open_asks?.some((ask) => ask.id === replyToAsk) ?? false),
      });
      if (replyToAsk !== undefined) {
        // The server answers whom the ask waits on now that this comment is its newest reply. It
        // differs from this comment's turn for an agent reply on a moved approval request.
        const askState =
          comment.ask_waiting_on === undefined
            ? ""
            : `; ask now waiting on ${comment.ask_waiting_on}`;
        return {
          text: [
            `Replied on ask ${replyToAsk} (comment ${comment.id}${askState}). ${followsAsk(commentOwner)}`,
            ...adviceLines,
          ].join("\n"),
          details: {
            ...commentDetails,
            ask: replyToAsk,
            follows: { ask: replyToAsk },
            ...(comment.ask_waiting_on === undefined
              ? {}
              : { ask_waiting_on: comment.ask_waiting_on }),
            ...(comment.advice === undefined ? {} : { advice: comment.advice }),
          },
        };
      }
      return {
        text: [`Posted comment ${comment.id} ${notSubscribed(commentOwner)}`, ...adviceLines].join(
          "\n"
        ),
        details: {
          ...commentDetails,
          ...(comment.advice === undefined ? {} : { advice: comment.advice }),
        },
      };
    }
    case "dispatch_suggest": {
      const resolved = await resolveDocument(documentOwner(), stringArg(args, "artifact"));
      const anchored = anchor(resolved.artifact, args);
      if (anchored === undefined) throw new Error("quote is required");
      const body = optionalString(args, "body");
      const suggestionInput = {
        ...(body === undefined ? {} : { body }),
        anchor: anchored,
        replace_with: stringArg(args, "replace_with"),
        actor,
      };
      const comment =
        resolved.owner.kind === "project"
          ? await client.artifactSuggest(resolved.artifact.id, suggestionInput)
          : await client.suggest(issue(), suggestionInput);
      return {
        text: `Posted suggestion ${comment.id} ${notSubscribed(resolvedTopic(resolved))}`,
        details: writeResultDetails(resolved, { comment: comment.id }),
      };
    }
    case "dispatch_message": {
      const inReplyTo = messageInReplyTo(args);
      const body = stringArg(args, "body");
      if (owner === null && inReplyTo !== undefined) {
        // No owner beside an `in_reply_to`: `resolveOwnerArguments` left it that way because the
        // caller named only the message it answers, which is a human's direct message to this
        // session - a conversation with no issue to post into. `POST /messages/{id}/reply` is
        // the route for it, and it names the delivery attempt being answered. A direct message
        // is delivered as attempt 1 when it is created, and a later retry never retires that
        // row, so 1 is the attempt this session was handed. Dispatch takes the reply as proof
        // the message arrived whatever that attempt's receipt says.
        // A model calling dispatch_message means to post, so it asks to follow up: once the
        // attempt is answered, Dispatch posts new text as this session's follow-up, threaded
        // under its first reply. The host's automatic BTW answer never asks (delivery.ts), so a
        // frame handed to the session twice cannot post a second answer.
        const reply = await client.messageReply(
          inReplyTo,
          { body, attempt: 1, actor },
          { followUp: true }
        );
        // Text this session already posted in the conversation (its first reply or an earlier
        // follow-up) posts nothing, and Dispatch says so, so the session is not told it sent
        // something new.
        if (reply.duplicate === true && reply.body === body) {
          return {
            text:
              `Dispatch already has this exact text in the conversation (message ${reply.id}); ` +
              "nothing new was posted. Send different text if you have more to say.",
            details: { message: reply.id, in_reply_to: inReplyTo, posted: false, duplicate: true },
          };
        }
        // A Dispatch that predates follow-ups ignores `?follow_up=true` and keeps one reply per
        // attempt: it answers any second call with the stored reply - often the host's own
        // automatic BTW answer, sent before the model got here - and posts nothing. The stored
        // body is how that reads apart from a send, so this says what happened instead of
        // reporting a send that did not occur.
        if (reply.body !== body) {
          return {
            text:
              `Message ${inReplyTo} was already answered by message ${reply.id}; Dispatch kept ` +
              "that reply and posted nothing. Wait for their next message rather than answering " +
              "this one again.",
            details: { message: reply.id, in_reply_to: inReplyTo, posted: false },
          };
        }
        const readBack = `dispatch_read({message: "${inReplyTo}"}) reads the conversation back.`;
        const parent = reply.in_reply_to ?? undefined;
        const follows = parent === inReplyTo ? undefined : parent;
        return {
          text:
            follows === undefined
              ? `Replied to message ${inReplyTo} with message ${reply.id}. ${readBack}`
              : `Replied to message ${inReplyTo} with message ${reply.id}, a follow-up threaded ` +
                `under your reply ${follows}. ${readBack}`,
          details: {
            message: reply.id,
            in_reply_to: inReplyTo,
            posted: true,
            ...(follows === undefined ? {} : { follows }),
          },
        };
      }
      const issueKey = issue();
      const message = await client.message(issueKey, {
        body,
        ...(inReplyTo === undefined ? {} : { in_reply_to: inReplyTo }),
        actor,
      });
      const messageRef = dispatchChildRef(dispatchIssueRef(issueKey), "message", message.id);
      const adviceLines = renderAdvice(input.tool, issueKey, message.advice, {});
      return {
        text: [
          `Posted message ${message.id} (${messageRef}) ${notSubscribed(issueTopic(issueKey))}`,
          ...adviceLines,
        ].join("\n"),
        details: {
          issue: issueKey,
          message: message.id,
          ...(message.advice === undefined ? {} : { advice: message.advice }),
        },
      };
    }
    case "dispatch_doc_edit": {
      const resolved = await resolveDocument(documentOwner(), stringArg(args, "artifact"));
      const ops = args.ops as EditOp[];
      const summary = optionalString(args, "summary");
      const { precondition: rawPrecondition } = args;
      const precondition = rawPrecondition as EditPrecondition | undefined;
      await refuseRemovingOpenDecisionBlocks(client, input.tool, resolved, ops);
      const edited = await client.docEdit(resolved.artifact.id, {
        ops,
        ...(summary === undefined ? {} : { summary }),
        ...(precondition === undefined ? {} : { precondition }),
        actor,
      });
      const retyped = ops.filter((operation) => operation.op === "retype").length;
      const versionText =
        edited.version === null ? "no new version" : `version ${edited.version.number}`;
      const retypedText =
        retyped === 0 ? "" : `; retyped ${retyped} block${retyped === 1 ? "" : "s"}`;
      // A batch that left the document as it was mints no version: say so, and name the
      // operations that did nothing, so the next attempt is aimed at the quote, not the version.
      const nothingChanged = edited.changed === false;
      const head = nothingChanged
        ? `Applied ${edited.applied} ops${retypedText}; nothing changed (${versionText})`
        : `Applied ${edited.applied} ops${retypedText} (${versionText})`;
      const unchangedOps = edited.unchanged_ops ?? [];
      const unchangedText =
        unchangedOps.length === 0
          ? ""
          : `; ${unchangedOps.length === 1 ? "operation" : "operations"} ${unchangedOps.join(", ")} changed nothing`;
      // A change the live document no longer carries: a browser deletion that landed after this
      // edit's version was rendered and before it reached the room, which is past undoing, so the
      // version records text the live document does not have (LEGION-269). `null` is a check that
      // reached no verdict - the room is reloading, or holds a tree past the schema's depth bound,
      // which a re-read answers DOC_SCHEMA for; an older server omits the field and reads as it
      // always did.
      const lostOps = edited.lost_ops;
      const lostText =
        lostOps === undefined || (lostOps !== null && lostOps.length === 0)
          ? ""
          : lostOps === null
            ? "; could not confirm this edit survived, because the live document is being reloaded or holds a tree too deep to read — re-read it"
            : `; ${versionText} carries text the live document no longer has: a concurrent change removed what ${lostOps.length === 1 ? "operation" : "operations"} ${lostOps.join(", ")} wrote — re-read the document`;
      const applied = `${head}${unchangedText}${lostText}`;
      const adviceLines = renderAdvice(
        input.tool,
        resolvedTopic(resolved).label,
        edited.advice,
        {}
      );
      // The token of the document this edit produced, rendered as dispatch_doc_read renders it:
      // the next guarded edit passes it as `precondition.document` with no read in between. A
      // Dispatch server predating it returns none, and the result reads as it always did.
      const tokenTrailer = edited.token === undefined ? [] : [`Document token: ${edited.token}`];
      return {
        text: [
          `${applied} ${notSubscribed(resolvedTopic(resolved))}`,
          ...tokenTrailer,
          ...adviceLines,
        ].join("\n"),
        details: writeResultDetails(resolved, {
          applied: edited.applied,
          ...(edited.version === null ? {} : { version: edited.version.number }),
          ...(nothingChanged ? { changed: false } : {}),
          ...(lostOps === undefined ? {} : { lost_ops: lostOps }),
          ...(edited.token === undefined ? {} : { token: edited.token }),
          ...(edited.advice === undefined ? {} : { advice: edited.advice }),
        }),
      };
    }
    case "dispatch_doc_read": {
      const artifactReference =
        optionalString(args, "artifact") ??
        (ownerArguments.ref?.kind === "spec" || ownerArguments.ref?.kind === "artifact"
          ? ownerArguments.ref.id
          : undefined);
      const resolved = await resolveDocument(documentOwner(), artifactReference);
      const version = optionalNumber(args, "version") ?? ownerArguments.ref?.version;
      const documentPromise = client.docRead(resolved.artifact.id, version);
      const marksPromise = openArtifactMarks(client, resolved);
      const marksResultPromise = marksPromise.then(
        (value) => ({ status: "fulfilled" as const, value }),
        (reason) => ({ status: "rejected" as const, reason })
      );
      // A document error takes precedence over a mark error. Capture the concurrent mark failure
      // so it is handled without delaying a document failure.
      const document = await documentPromise;
      const marksResult = await marksResultPromise;
      if (marksResult.status === "rejected") throw marksResult.reason;
      const marks = marksResult.value;
      const approval = approvalLine(resolved.artifact);
      const trailer = [
        ...("token" in document && document.token !== undefined
          ? [`Document token: ${document.token}`]
          : []),
        ...(marks.length === 0 ? [] : [`Open anchored asks/comments: ${marks.join(", ")}`]),
        ...(approval === undefined ? [] : [approval]),
      ];
      return {
        text:
          trailer.length === 0
            ? document.markdown
            : `${document.markdown}\n\n${trailer.join("\n")}`,
        details:
          resolved.owner.kind === "project"
            ? {
                project: resolved.artifact.project,
                document: `${resolved.artifact.project}/${resolved.artifact.slug}`,
              }
            : { issue: resolved.issue?.key },
      };
    }
    case "dispatch_request_approval": {
      const artifactReference =
        optionalString(args, "artifact") ??
        (ownerArguments.ref?.kind === "spec" || ownerArguments.ref?.kind === "artifact"
          ? ownerArguments.ref.id
          : undefined);
      const resolved = await resolveDocument(documentOwner(), artifactReference);
      await refuseOpenDecisionBlocks(client, input.tool, resolved);
      const result = await client.requestApproval(resolved.artifact.id, {
        actor,
        summary: stringArg(args, "summary"),
      });
      if (result.ask === null) {
        return {
          text: `${resolved.artifact.name} (document id ${resolved.artifact.id}) is already approved at version ${result.version} by ${result.approval.by?.id ?? "unknown"}; no new request was opened. An edit after approval makes it stale, so request again only for a new version, once the human has agreed to every point in it.`,
          details: {
            ...(resolved.owner.kind === "project"
              ? documentResultDetails(resolved.artifact)
              : { issue: resolved.issue?.key }),
            artifact: resolved.artifact.id,
            version: result.version,
          },
        };
      }
      const details = await followedAskDetails(client, result.ask, resolved.artifact);
      // A call that opened, reworded or handed back the request says so; one that found it already
      // waiting on the human says nothing changed, so a retry never reads as a fresh hand-back.
      const outcome = result.recorded
        ? `Approval requested for ${resolved.artifact.name} (document id ${resolved.artifact.id}) at version ${result.version} (ask ${result.ask.id}).`
        : `The approval request for ${resolved.artifact.name} (document id ${resolved.artifact.id}) at version ${result.version} (ask ${result.ask.id}) already waits on the human, so this call changed nothing: nothing since it last reached the human (a newer version, a human's reply in its thread, or your progress note) left it waiting on you.`;
      return {
        text: `${outcome} The human's Inbox asks: ${JSON.stringify(result.ask.question)}. The answer arrives as artifact.approved or artifact.changes_requested. An edit before the answer moves this request to the new version and leaves it waiting on you, and an edit after approval makes the approval stale: either way, request again for the new version once the human has agreed to every point in it, which hands this request back or opens a new one.`,
        details: { ...details, artifact: resolved.artifact.id, version: result.version },
      };
    }
    case "dispatch_artifact": {
      const summary = optionalString(args, "summary");
      const name = stringArg(args, "name");
      const content = optionalString(args, "content");
      const artifactInput =
        content === undefined
          ? {
              name,
              file: Bun.file(resolvePath(input.cwd, stringArg(args, "path"))),
              ...(summary === undefined ? {} : { summary }),
              actor,
            }
          : {
              name,
              content,
              ...(summary === undefined ? {} : { summary }),
              actor,
            };
      const artifactOwner = documentOwner();
      const result =
        artifactOwner.kind === "project"
          ? await client.projectArtifact(artifactOwner.project, artifactInput)
          : await client.artifact(issue(), artifactInput);
      const artifactRef = dispatchDocumentRef(
        artifactOwner.kind === "project" ? artifactOwner.project : issue(),
        result.artifact.slug
      );
      const uploadOwner =
        artifactOwner.kind === "project" ? documentTopic(result.artifact) : issueTopic(issue());
      const adviceLines = renderAdvice(input.tool, uploadOwner.label, result.advice, {
        isPrimarySpec: result.artifact.primary || result.artifact.name === "spec.md",
      });
      return {
        text: [
          `Uploaded ${result.artifact.name} as version ${result.version.number} (artifact slug ${result.artifact.slug}; ${artifactRef}) ${notSubscribed(uploadOwner)}`,
          ...adviceLines,
        ].join("\n"),
        details:
          artifactOwner.kind === "project"
            ? {
                ...documentResultDetails(result.artifact),
                version: result.version.number,
                ...(result.advice === undefined ? {} : { advice: result.advice }),
              }
            : {
                issue: issue(),
                artifact: result.artifact.id,
                version: result.version.number,
                ...(result.advice === undefined ? {} : { advice: result.advice }),
              },
      };
    }
    case "dispatch_follow": {
      const sessionId = input.sessionId?.trim();
      if (!sessionId) throw new Error("host session id is required for dispatch_follow");
      const ask = await resolveAskArgument(input.tool, args, client, sessionId);
      const action = stringArg(args, "action");
      if (action === "unfollow") {
        await client.unfollowAsk(ask, sessionId, actor);
        return { text: `Unfollowed ask ${ask}.`, details: { ask } };
      }
      const read = await client.getAsk(ask);
      await client.followAsk(ask, sessionId, actor);
      return {
        text: `Following ask ${ask}: its answer and replies reach this session directly.`,
        details: await followedAskDetails(client, read.ask),
      };
    }
    // Reads report their owner and follow nothing; no result subscribes the session.
    case "dispatch_read": {
      const message = optionalString(args, "message");
      if (message !== undefined) {
        const sessionId = input.sessionId?.trim();
        if (!sessionId) throw new Error("host session id is required for dispatch_read({message})");
        const thread = await client.getMessageThread(messageIdOf(message) as string, sessionId);
        const issueKey = thread.message.issue_key;
        return {
          text: messageSummary(
            thread,
            issueKey === null
              ? []
              : await graphSections(
                  client,
                  dispatchChildRef(dispatchIssueRef(issueKey), "message", thread.message.id)
                )
          ),
          details: {
            message: thread.message.id,
            ...(issueKey === null ? {} : { issue: issueKey }),
          },
        };
      }
      if (ownerArguments.ref?.kind === "ask") {
        const ref = ownerArguments.ref;
        const id = await resolveIdPrefix(input.tool, "ask", ref.id, refOwnerName(ref), async () =>
          ref.owner.kind === "issue"
            ? client.listIssueAsks(ref.owner.issue)
            : client.getArtifactAsks(
                (await resolveDocument(ref.owner, ref.artifact)).artifact.id,
                "all"
              )
        );
        const askRead = await client.getAsk(id);
        const askRef = refTarget(ref, "ask", id);
        return {
          text: askSummary(askRead, await graphSections(client, askRef)),
          details:
            ref.owner.kind === "project"
              ? { project: ref.owner.project }
              : { issue: ref.owner.issue },
        };
      }
      if (ownerArguments.ref?.kind === "comment") {
        const ref = ownerArguments.ref;
        const id = await resolveIdPrefix(
          input.tool,
          "comment",
          ref.id,
          refOwnerName(ref),
          async () =>
            ref.owner.kind === "issue"
              ? client.getComments(ref.owner.issue)
              : client.getArtifactComments(
                  (await resolveDocument(ref.owner, ref.artifact)).artifact.id
                )
        );
        const comment = await client.getComment(id);
        const commentRef = refTarget(ref, "comment", id);
        return {
          text: commentSummary(comment, await graphSections(client, commentRef)),
          details:
            ref.owner.kind === "project"
              ? { project: ref.owner.project }
              : { issue: comment.comment.issue_key },
        };
      }
      if (ownerArguments.ref?.kind === "message") {
        if (ownerArguments.ref.owner.kind !== "issue") {
          throw new Error("message references are issue-scoped");
        }
        const messageRead = await client.getMessage(
          ownerArguments.ref.owner.issue,
          ownerArguments.ref.id
        );
        const messageRef = dispatchChildRef(
          dispatchIssueRef(ownerArguments.ref.owner.issue),
          "message",
          ownerArguments.ref.id
        );
        return {
          text: messageSummary(messageRead, await graphSections(client, messageRef)),
          details: { issue: messageRead.message.issue_key },
        };
      }
      if (documentOwner().kind === "project") {
        const resolved = await resolveDocument(documentOwner(), stringArg(args, "artifact"));
        const documentRef = dispatchDocumentRef(resolved.artifact.project, resolved.artifact.slug);
        return {
          text: [
            `Document: ${resolved.artifact.project} / ${resolved.artifact.name}`,
            `Reference: ${documentRef}`,
            `Versions: ${resolved.artifact.versions.length}`,
            ...(approvalLine(resolved.artifact) === undefined
              ? []
              : [approvalLine(resolved.artifact) as string]),
            ...(await graphSections(client, documentRef)),
          ].join("\n"),
          details: {
            project: resolved.artifact.project,
            document: documentLabel(resolved.artifact.project, resolved.artifact.slug),
          },
        };
      }
      const issueKey = issue();
      if (ownerArguments.ref?.kind === "log") {
        const read = await client.read(issueKey);
        return {
          text: logSummary(read.issue, read.events),
          details: { issue: read.issue.key },
        };
      }
      if (ownerArguments.ref?.kind === "children") {
        const read = await client.read(issueKey);
        return {
          text: childrenSummary(read.issue),
          details: { issue: read.issue.key },
        };
      }
      const readPromise = client.read(issueKey);
      // Start the independent, non-fatal closure lookup with the issue read. `read` retains
      // its internal issue-then-events order because event pagination starts at issue.last_seq.
      const referencesPromise = issueReferencesOrUnavailable(client, issueKey);
      const read = await readPromise;
      // The claim's holder must read the same here as on the issue page, so the label comes
      // from the live registry — asked for only when a session holds this issue.
      const [references, graph, titles] = await Promise.all([
        referencesPromise,
        graphSections(client, dispatchIssueRef(read.issue.key)),
        liveSessionTitles(client, holdsSession(read.issue.claim) || routeHeldBySession(read.issue)),
      ]);
      return {
        text: issueSummary(read.issue, read.events, references, graph, titles),
        details: { issue: read.issue.key },
      };
    }
    default:
      throw new Error(`Unknown Dispatch tool: ${input.tool}`);
  }
}

async function resolveExistingIssue(
  client: DispatchClient,
  issueReference: string
): Promise<string> {
  try {
    return await client.resolveIssue(issueReference);
  } catch (error) {
    if (dispatchAnswered(error, 404)) {
      throw new Error(
        `no Dispatch issue is linked to ${issueReference}; create it first with ` +
          `dispatch_issue({ external: "${issueReference}", ... })`
      );
    }
    throw error;
  }
}
function asObject(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}
