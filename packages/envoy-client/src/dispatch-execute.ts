import { resolve as resolvePath } from "node:path";
import type {
  Actor,
  Artifact,
  Ask,
  AskRead,
  AskUrgency,
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
  IssueComponents,
  IssueComponentsInput,
  IssueComponentsMode,
  IssueDetails,
  IssuePriority,
  IssueReferences,
  MessageRead,
  OpenAsk,
  OpenAsksResponse,
  SearchResult,
  WriteAdvice,
} from "@legion/contracts";
import {
  ASK_QUESTION_MAX,
  ASK_URGENCIES,
  dispatchToolSchema,
  dispatchToolSpecs,
  overCapMessage,
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
import { DispatchClient, DispatchServiceError, type GraphReferencesQuery } from "./dispatch-http";
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
  readonly external_links?: unknown;
  readonly ops?: unknown;
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
  if (
    advice.decision_blocks === 0 &&
    opts.isPrimarySpec === true &&
    (tool === "dispatch_issue" || tool === "dispatch_artifact")
  ) {
    lines.push(
      'No decision blocks in this spec — nothing here reaches a human\'s inbox. Want human feedback? See the `dispatch` skill, "Decision blocks".'
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

/**
 * The full id an ask or comment ref names. A full uuid passes through; a unique prefix is
 * resolved against `list()` (the owner's asks or comments); anything else is refused.
 */
async function resolveIdPrefix(
  tool: string,
  kind: "ask" | "comment",
  ref: ParsedDispatchRef,
  list: () => Promise<readonly { readonly id: string }[]>
): Promise<string> {
  const fullID = normalizeUUID(ref.id);
  if (fullID !== undefined) return fullID;
  const ownerName =
    ref.owner.kind === "issue" ? ref.owner.issue : `${ref.owner.project}/${ref.artifact}`;
  if (!idPrefixPattern.test(ref.id)) {
    throw new ToolInputError(tool, [
      `${kind} id ${ref.id} must be a full uuid or a prefix of at least 8 hex characters`,
    ]);
  }
  const prefix = ref.id.toLowerCase();
  const matches = (await list()).filter((item) => item.id.toLowerCase().startsWith(prefix));
  if (matches.length === 1 && matches[0] !== undefined) return matches[0].id;
  throw new ToolInputError(tool, [
    matches.length === 0
      ? `${kind} id ${ref.id} matches none of the ${kind}s on ${ownerName}; use the full id`
      : `${kind} id ${ref.id} matches ${matches.length} ${kind}s on ${ownerName}; use the full id`,
  ]);
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
    case "dispatch_resolve_ask": {
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
  }
  return problems;
}

/**
 * The dispatch:// form of a dashboard URL on the configured server, or undefined when the
 * value is not such a URL. Accepts the issue, spec, artifact, ask, comment, and log pages
 * plus project document pages (with their ?ask= / ?comment= deep links).
 */
function dispatchRefFromUrl(value: string, serverUrl: string): string | undefined {
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
  const ask = url.searchParams.get("ask");
  if (ask !== null) return `${document}/ask/${ask}`;
  const comment = url.searchParams.get("comment");
  if (comment !== null) return `${document}/comment/${comment}`;
  return document;
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
    if (!/^[A-Z][A-Z0-9]{1,9}$/.test(projectArgument)) {
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

function artifactByReference(
  artifacts: readonly Artifact[],
  artifactReference: string,
  owner: "issue" | "project"
): Artifact {
  const byIdOrSlug =
    artifacts.find((candidate) => candidate.id === artifactReference) ??
    artifacts.find((candidate) => candidate.slug === artifactReference);
  if (byIdOrSlug !== undefined) return byIdOrSlug;
  const byName = artifacts.filter((candidate) => candidate.name === artifactReference);
  if (byName.length > 1) {
    throw new Error(documentReferenceProblem(artifactReference, byName, owner, true));
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
  artifactReference: string | undefined
): Promise<ResolvedArtifact> {
  if (owner.kind === "project") {
    if (artifactReference === undefined) {
      throw new Error("artifact is required for a project document");
    }
    try {
      return {
        owner,
        artifact: await client.getProjectArtifact(owner.project, artifactReference),
      };
    } catch (error) {
      // Project artifact routes resolve slugs. The unlinked-only collection gives project
      // documents the same id, slug, then filename resolution as issue artifacts without
      // allowing an issue-attached artifact of the same name to become the document owner.
      if (!(error instanceof DispatchServiceError) || error.status !== 404) throw error;
      const artifacts = await client.listProjectArtifacts(owner.project, true);
      return {
        owner,
        artifact: artifactByReference(artifacts, artifactReference, "project"),
      };
    }
  }
  const issue = await client.getIssue(owner.issue);
  let artifact: Artifact | undefined;
  if (artifactReference === undefined || artifactReference === "spec") {
    artifact = issue.artifacts.find(
      (candidate) => candidate.primary || candidate.id === issue.primary_artifact_id
    );
  } else {
    artifact = artifactByReference(issue.artifacts, artifactReference, "issue");
  }
  if (!artifact) {
    throw new Error(
      documentReferenceProblem(artifactReference ?? "spec", issue.artifacts, "issue")
    );
  }
  return { owner, issue, artifact };
}

const documentHintLimit = 8;

function documentReferenceProblem(
  reference: string,
  documents: readonly Pick<Artifact, "slug" | "name">[],
  owner: "issue" | "project",
  ambiguous = false
): string {
  const hints = documents
    .slice(0, documentHintLimit)
    .map((document) => `${document.slug} (${document.name})`);
  const list = hints.length === 0 ? "none" : hints.join(", ");
  return ambiguous
    ? `"${reference}" names ${documents.length} documents on this ${owner}; use a slug: ${list}`
    : `document "${reference}" not found by slug; this ${owner}'s documents: ${list}`;
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

/** One line describing a document's approval, or undefined for a draft nobody has asked about. */
function approvalLine(artifact: Pick<Artifact, "approval">): string | undefined {
  const approval = artifact.approval;
  if (approval === undefined || approval.state === "draft") return undefined;
  switch (approval.state) {
    case "awaiting":
      return `Approval: awaiting (requested by ${approval.requested_by?.id ?? "unknown"}, ask ${approval.ask_id ?? "?"})`;
    case "approved":
      return `Approval: approved v${approval.version} by ${approval.by?.id ?? "unknown"}`;
    case "stale":
      return `Approval: approved v${approval.version} by ${approval.by?.id ?? "unknown"}, edited since (now v${approval.latest_version}) - request approval again`;
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

function issueSummary(
  issue: IssueDetails,
  events: readonly Event[],
  references: IssueReferences | string,
  graph: readonly string[]
): string {
  const asks = issue.open_asks;
  const spec = issue.artifacts?.find((artifact) => artifact.primary);
  const specApproval = spec === undefined ? undefined : approvalLine(spec);
  if (issue.priority === undefined) throw new Error("Dispatch issue is missing priority");
  if (issue.assignee === undefined) throw new Error("Dispatch issue is missing assignee");
  if (issue.components === undefined) throw new Error("Dispatch issue is missing components");
  return [
    `Title: ${issue.title}`,
    `Key: ${issue.key}`,
    `Status: ${issue.status}`,
    `Assignee: ${issue.assignee ?? "unassigned"}`,
    ...(issue.priority === null ? [] : [`Priority: P${issue.priority}`]),
    `Labels: ${issue.labels.length === 0 ? "none" : issue.labels.join(", ")}`,
    componentsLine(issue.components),
    `Route: ${issue.route ?? "none"}`,
    ...(specApproval === undefined
      ? []
      : [`Spec ${specApproval.replace(/^Approval/, "approval")}`]),
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

/** How a read degrades a graph or closure section the server cannot serve. */
function unavailableReason(error: unknown): string {
  return error instanceof DispatchServiceError && error.status === 404
    ? "unavailable"
    : `unavailable: ${messageFor(error)}`;
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

function eventLine(event: Event): string {
  const head = eventHead(event);
  return `- #${event.seq} ${event.type} · ${event.actor.kind} ${event.actor.id} · ${event.created_at}${head === undefined || head === "" ? "" : ` · ${head}`}`;
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
    `${reply.id} · ${reply.author.kind} ${reply.author.id}`,
    `Body: ${reply.body}`,
  ]);
  return [
    `Question: ${ask.question}`,
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
    `${comment.id} · ${comment.author.kind} ${comment.author.id}`,
    ...(comment.anchor?.quote === undefined ? [] : [`> ${comment.anchor.quote}`]),
    `Body: ${comment.body}`,
  ];
  const chain = replies.flatMap((reply) => [
    `${reply.id} · ${reply.author.kind} ${reply.author.id}`,
    ...(reply.anchor?.quote === undefined ? [] : [`> ${reply.anchor.quote}`]),
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

function messageSummary({ message, replies }: MessageRead, graph: readonly string[]): string {
  const root = [
    `${message.id} · ${message.author.kind} ${message.author.id}`,
    `Body: ${message.body}`,
  ];
  const chain = replies.flatMap((reply) => [
    `${reply.id} · ${reply.author.kind} ${reply.author.id}`,
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
    if (
      commentsResult.reason instanceof DispatchServiceError &&
      commentsResult.reason.status === 404
    ) {
      return marks;
    }
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
            issue.message.startsWith("Exactly one of issue and project is required"))
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
    return {
      text:
        owner === null
          ? `Session ${sessionId} runs under the shared token: no owner, so issues you create without an assignee are unassigned (or inherit their parent's).`
          : `Session ${sessionId} acts for ${owner}: issues you create without an assignee are assigned to ${owner}.`,
      details: { session: sessionId, owner },
    };
  }
  const args = (parsed.success ? parsed.data : ownerArguments.args) as ToolArguments;
  const actor = toolActor(await resolveOrigin(env, exec, input.cwd), input);
  const client = dispatchClient();
  const owner =
    ownerArguments.owner?.kind === "issue"
      ? {
          kind: "issue" as const,
          issue: await ensureIssue(client, ownerArguments.owner.issue, actor),
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
      const priority = optionalNumber(args, "priority");
      const assignee = optionalString(args, "assignee");
      const components = optionalComponents(args, "components");
      const labels = args.labels;
      try {
        const created = await client.issue({
          project,
          title,
          ...(parent === undefined ? {} : { parent }),
          ...(external === undefined ? {} : { external }),
          ...(force === undefined ? {} : { force }),
          ...(spec === undefined ? {} : { spec }),
          ...(priority === undefined ? {} : { priority: priority as IssuePriority }),
          ...(assignee === undefined ? {} : { assignee }),
          ...(components === undefined ? {} : { components }),
          ...(Array.isArray(labels) ? { labels: labels as string[] } : {}),
          actor,
        });
        const adviceLines = renderAdvice(input.tool, created.key, created.advice, {
          isPrimarySpec: spec !== undefined,
        });
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
      const title = optionalString(args, "title");
      const route = optionalString(args, "route");
      const parent = optionalString(args, "parent");
      const components = optionalComponents(args, "components");
      const labels = Array.isArray(args.labels) ? (args.labels as string[]) : undefined;
      const requestedLinks = Array.isArray(args.external_links)
        ? [...new Set(args.external_links as string[])]
        : undefined;
      // The server replaces the whole link set; the common call is "link the pull request
      // I just opened", so merge by URL and keep every existing link (and its kind).
      let newLinks: string[] = [];
      try {
        const before = await client.getIssue(issueKey);
        const linked = before.external_links.map((link) => link.url);
        newLinks = requestedLinks?.filter((url) => !linked.includes(url)) ?? [];
        const after = await client.updateIssue(issueKey, {
          ...(status === undefined ? {} : { status }),
          ...(title === undefined ? {} : { title }),
          ...(labels === undefined ? {} : { labels }),
          ...(route === undefined ? {} : { route }),
          ...(parent === undefined ? {} : { parent: parent === "" ? null : parent }),
          ...(components === undefined ? {} : { components }),
          ...(requestedLinks === undefined
            ? {}
            : { external_links: [...before.external_links, ...newLinks.map((url) => ({ url }))] }),
          actor,
        });
        const linkCount = `(${after.external_links.length} ${after.external_links.length === 1 ? "link" : "links"})`;
        const changes = [
          ...(status === undefined ? [] : [`status ${before.status} -> ${after.status}`]),
          ...(title === undefined ? [] : [`title "${after.title}"`]),
          ...(labels === undefined
            ? []
            : [after.labels.length === 0 ? "labels cleared" : `labels ${after.labels.join(", ")}`]),
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
            ...(after.advice === undefined ? {} : { advice: after.advice }),
          },
        };
      } catch (error) {
        // The server's code (INVALID_STATUS, ISSUE_CLOSED, EXTERNAL_LINK_TAKEN, ...) is the
        // part an agent acts on; keep it in the message the host shows.
        if (!(error instanceof DispatchServiceError)) throw error;
        // A URL links exactly one issue. A server from before EXTERNAL_LINK_TAKEN answers the
        // unique-index violation with 500 INTERNAL, which names nothing; say what it means.
        const taken =
          error.status === 500 && newLinks.length > 0
            ? `; one of ${newLinks.join(", ")} may already be linked from another issue (a URL links exactly one issue)`
            : "";
        throw new DispatchServiceError(
          error.code,
          error.status,
          `${error.code}: ${error.message}${taken}`,
          error.candidates
        );
      }
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
      const updatedSince = optionalString(args, "updated_since");
      const limit = Math.min(Math.max(optionalNumber(args, "limit") ?? 50, 1), 250);
      const issues = await client.listIssues({
        project,
        ...(status === undefined ? {} : { status }),
        ...(parent === undefined ? {} : { parent }),
        ...(label === undefined ? {} : { label }),
        ...(updatedSince === undefined ? {} : { updated_since: updatedSince }),
      });
      const rows = issues.slice(0, limit).map((row) => ({
        key: row.key,
        title: row.title,
        status: row.status,
        priority: row.priority,
        parent: row.parent,
        labels: row.labels ?? [],
        open_asks: row.open_asks,
        updated_at: row.updated_at,
      }));
      return {
        text:
          rows.length === 0
            ? `No issues in ${project}.`
            : [
                `${rows.length} ${rows.length === 1 ? "issue" : "issues"} in ${project}` +
                  (issues.length > rows.length
                    ? ` (showing ${rows.length} of ${issues.length})`
                    : ""),
                ...rows.map(
                  (row) =>
                    `${row.key} [${row.status}]${row.priority === null ? "" : ` P${row.priority}`} ${row.title}` +
                    (row.open_asks === 0
                      ? ""
                      : ` · ${row.open_asks} open ${row.open_asks === 1 ? "ask" : "asks"}`)
                ),
              ].join("\n"),
        details: { issues: rows },
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
      const ask = await client.resolveAsk(stringArg(args, "ask"), {
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
        id = await resolveIdPrefix(input.tool, "comment", ref, () => client.getComments(issueKey));
      } else if (ref !== null) {
        const artifact = (await resolveArtifact(client, ref.owner, ref.artifact)).artifact;
        document = artifact;
        id = await resolveIdPrefix(input.tool, "comment", ref, () =>
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
            ? await resolveArtifact(client, owner, artifactReference)
            : undefined
          : await resolveArtifact(client, owner, artifactReference);
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
      const ask = await client.editAsk(askId(args), {
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
            ? await resolveArtifact(client, owner, artifactReference)
            : undefined
          : await resolveArtifact(client, owner, artifactReference);
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
          (comment.advice?.your_open_asks.some((ask) => ask.id === replyToAsk) ?? false),
      });
      if (replyToAsk !== undefined) {
        // The server records turn only on a reply to an open ask, so a non-null turn is exactly
        // "the ask is open and now waits on <turn>"; a reply under a closed ask reports no state.
        const askState = comment.turn === null ? "" : `; ask now waiting on ${comment.turn}`;
        return {
          text: [
            `Replied on ask ${replyToAsk} (comment ${comment.id}${askState}). ${followsAsk(commentOwner)}`,
            ...adviceLines,
          ].join("\n"),
          details: {
            ...commentDetails,
            ask: replyToAsk,
            follows: { ask: replyToAsk },
            ...(comment.turn === null ? {} : { ask_waiting_on: comment.turn }),
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
      const resolved = await resolveArtifact(client, documentOwner(), stringArg(args, "artifact"));
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
      const issueKey = issue();
      const message = await client.message(issueKey, {
        body: stringArg(args, "body"),
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
      const resolved = await resolveArtifact(client, documentOwner(), stringArg(args, "artifact"));
      const ops = args.ops as EditOp[];
      const summary = optionalString(args, "summary");
      const { precondition: rawPrecondition } = args;
      const precondition = rawPrecondition as EditPrecondition | undefined;
      const edited = await client.docEdit(resolved.artifact.id, {
        ops,
        ...(summary === undefined ? {} : { summary }),
        ...(precondition === undefined ? {} : { precondition }),
        actor,
      });
      const retyped = ops.filter((operation) => operation.op === "retype").length;
      const versionText =
        edited.version === null ? "no new version" : `version ${edited.version.number}`;
      const applied =
        retyped === 0
          ? `Applied ${edited.applied} ops (${versionText})`
          : `Applied ${edited.applied} ops; retyped ${retyped} block${retyped === 1 ? "" : "s"} (${versionText})`;
      const adviceLines = renderAdvice(
        input.tool,
        resolvedTopic(resolved).label,
        edited.advice,
        {}
      );
      return {
        text: [`${applied} ${notSubscribed(resolvedTopic(resolved))}`, ...adviceLines].join("\n"),
        details: writeResultDetails(resolved, {
          applied: edited.applied,
          ...(edited.version === null ? {} : { version: edited.version.number }),
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
      const resolved = await resolveArtifact(client, documentOwner(), artifactReference);
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
      const resolved = await resolveArtifact(client, documentOwner(), artifactReference);
      const result = await client.requestApproval(resolved.artifact.id, { actor });
      if (result.ask === null) {
        return {
          text: `${resolved.artifact.name} (document id ${resolved.artifact.id}) is already approved at version ${result.version} by ${result.approval.by?.id ?? "unknown"}; no new request was opened. An edit after approval makes it stale, so request again only for a new version.`,
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
      return {
        text: `Approval requested for ${resolved.artifact.name} (document id ${resolved.artifact.id}) at version ${result.version} (ask ${result.ask.id}). The answer arrives as artifact.approved or artifact.changes_requested; an edit after approval makes it stale, so request again for the new version.`,
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
      const ask = askId(args);
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
      if (ownerArguments.ref?.kind === "ask") {
        const ref = ownerArguments.ref;
        const id = await resolveIdPrefix(input.tool, "ask", ref, async () =>
          ref.owner.kind === "issue"
            ? client.listIssueAsks(ref.owner.issue)
            : client.getArtifactAsks(
                (await resolveArtifact(client, ref.owner, ref.artifact)).artifact.id,
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
        const id = await resolveIdPrefix(input.tool, "comment", ref, async () =>
          ref.owner.kind === "issue"
            ? client.getComments(ref.owner.issue)
            : client.getArtifactComments(
                (await resolveArtifact(client, ref.owner, ref.artifact)).artifact.id
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
        const resolved = await resolveArtifact(
          client,
          documentOwner(),
          stringArg(args, "artifact")
        );
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
      const [references, graph] = await Promise.all([
        referencesPromise,
        graphSections(client, dispatchIssueRef(read.issue.key)),
      ]);
      return {
        text: issueSummary(read.issue, read.events, references, graph),
        details: { issue: read.issue.key },
      };
    }
    default:
      throw new Error(`Unknown Dispatch tool: ${input.tool}`);
  }
}

async function ensureIssue(
  client: DispatchClient,
  issueReference: string,
  actor: Actor
): Promise<string> {
  try {
    return await client.ensureIssue(issueReference, actor);
  } catch (error) {
    if (error instanceof DispatchServiceError && error.code === "PROJECT_UNMAPPED") {
      const repository = issueReference.slice(0, issueReference.lastIndexOf("#"));
      throw new Error(
        `repository ${repository} is not mapped in repository settings and no DISPATCH_DEFAULT_PROJECT is configured`
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
