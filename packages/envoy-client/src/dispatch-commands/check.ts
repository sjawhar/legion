import {
  ASK_QUESTION_MAX,
  dispatchToolSchema,
  dispatchToolSpecs,
  overCapMessage,
  PROJECT_KEY_PATTERN,
  PROJECT_KEY_RULE,
  zodSchemaApi,
} from "@legion/contracts";
import { canonicalRepo } from "@legion/contracts/repo";
import { z } from "zod";
import { commandName } from "../dispatch-command";
import type { ActiveDispatchConfig } from "../dispatch-config";
import { defaultExec, type ExecFn, resolveCwdRepo } from "../dispatch-cwd";
import { type ParsedDispatchRef, parseDispatchRef } from "../dispatch-owner";
import { localPictures, parseAgentArtifactRef } from "../dispatch-picture-tools";
import { formatZodIssues } from "../tool-input-errors";
import {
  asObject,
  type CheckedCall,
  type DispatchCallCheckInput,
  type ExecutorEnvironment,
  type OwnerResolution,
  optionalBoolean,
  optionalNumber,
  optionalString,
  stringArg,
  type ToolArguments,
} from "./context";
import {
  askIdProblem,
  commentIdProblem,
  dispatchRefFromUrl,
  innerRefProblem,
  itemRefProblem,
  messageIdOf,
  messageIdProblem,
  messageInReplyTo,
  refOwnerName,
  replyBlockProblem,
} from "./refs";

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
  dispatch_related: true,
  dispatch_issues: true,
  dispatch_project: true,
  dispatch_projects: true,
  dispatch_open_asks: true,
  dispatch_answers: true,
  dispatch_whoami: true,
  dispatch_inbox: true,
  dispatch_architecture_sync: true,
};

function canonicalExternalIssueRef(value: string): string {
  const match = value.trim().match(externalIssueRefPattern);
  return match ? `${canonicalRepo(match[1] ?? "", match[2] ?? "")}#${match[3]}` : value;
}
/** The question text sent to Dispatch: the ref is appended unless the question already cites it. */
function questionWithRef(question: string, ref: string | undefined): string {
  return ref === undefined || question.includes(ref) ? question : `${question}\n\nRef: ${ref}`;
}

/** The validated question plus ref; `argumentProblems` has already refused an over-cap pair. */
export function askQuestionWithRef(args: ToolArguments): string {
  return questionWithRef(stringArg(args, "question"), optionalString(args, "ref"));
}

function askQuestionProblem(withRef: string): string | undefined {
  return withRef.length > ASK_QUESTION_MAX
    ? `question plus ref ${overCapMessage(withRef.length, ASK_QUESTION_MAX)}; shorten the question or drop the ref`
    : undefined;
}

/** The tools whose `images` argument sends pictures. */
const pictureSendingTools: Readonly<Record<string, true>> = {
  dispatch_ask: true,
  dispatch_comment: true,
  dispatch_message: true,
};
const refGrammarProblem =
  "ref must be a valid dispatch:// reference such as dispatch://KEY-1, " +
  "dispatch://KEY-1/ask/<uuid>, dispatch://KEY-1/comment/<uuid>, " +
  "dispatch://KEY-1/message/<uuid>, dispatch://KEY-1/artifact/<slug>, " +
  "dispatch://PROJECT/artifact/<document-ref> (an artifact id, slug, or filename), " +
  "dispatch://KEY-1/spec#b-<block id>, dispatch://KEY-1/artifact/<slug>#b-<block id> or " +
  "dispatch://PROJECT/artifact/<slug>#b-<block id> (one block of a document, its id " +
  "percent-encoded, optionally pinned to a version of the document with @v<N> after it; for " +
  "dispatch read), dispatch://KEY-1/ask/<uuid>#b-<part id> or " +
  "dispatch://PROJECT/artifact/<slug>/ask/<uuid>#b-<part id> (one part of an ask: its question, " +
  "one of its options or its answer, optionally pinned to a revision of the ask's parts with " +
  "@v<N> after it; for dispatch read), dispatch://KEY-1/comment/<uuid>#b-<block id>, " +
  "dispatch://PROJECT/artifact/<slug>/comment/<uuid>#b-<block id> or " +
  "dispatch://KEY-1/message/<uuid>#b-<block id> (one block of a comment or message, a " +
  "comment's optionally pinned to a revision of the comment with @v<N> after it; for dispatch " +
  "read), or " +
  "dispatch://agent/<session id>/artifact/<slug> (a picture in a conversation on the Agents page, " +
  "for dispatch doc-read); a dashboard URL on this Dispatch server is accepted too";

const ownerRequiredProblem = "issue is required; supply issue or set LEGION_ISSUE";
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
        problems.push(itemRefProblem(ask) ?? askIdProblem);
      }
      break;
    }
    case "dispatch_resolve_comment": {
      const comment = optionalString(args, "comment");
      if (comment?.startsWith("dispatch://") && parseDispatchRef(comment)?.kind !== "comment") {
        problems.push(itemRefProblem(comment) ?? commentIdProblem);
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
      const replyTo = optionalString(args, "reply_to");
      const replyToAsk = optionalString(args, "reply_to_ask");
      if (replyTo !== undefined && replyToAsk !== undefined) {
        problems.push("reply_to and reply_to_ask cannot both be set");
      }
      for (const problem of [
        replyTo === undefined ? undefined : replyBlockProblem("reply_to", "comment", replyTo),
        replyToAsk === undefined ? undefined : replyBlockProblem("reply_to_ask", "ask", replyToAsk),
      ]) {
        if (problem !== undefined) problems.push(problem);
      }
      break;
    }
    case "dispatch_message": {
      const inReplyTo = optionalString(args, "in_reply_to");
      if (inReplyTo !== undefined) {
        const problem =
          replyBlockProblem("in_reply_to", "message", inReplyTo) ??
          (messageInReplyTo(args) === undefined ? messageIdProblem : undefined);
        if (problem !== undefined) problems.push(problem);
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
      const events = optionalNumber(args, "events");
      const allEvents = optionalBoolean(args, "all_events") === true;
      if (events !== undefined && allEvents) {
        problems.push(
          "--events and --all-events both say how many events the read lists: name one, --events N for the last N or --all-events for every one"
        );
      }
      const count = events !== undefined ? "--events" : allEvents ? "--all-events" : undefined;
      const eventless = count === undefined ? undefined : readWithoutEvents(args);
      if (eventless !== undefined) {
        problems.push(
          `${count} lists an issue's events, which dispatch read prints only for an issue or its dispatch://KEY/log, not for ${eventless}: drop ${count}`
        );
      }
      break;
    }
    case "dispatch_doc_read": {
      if (
        optionalBoolean(args, "blocks") === true &&
        optionalNumber(args, "version") !== undefined
      ) {
        problems.push(
          "blocks lists the live document, the one dispatch doc-edit changes; name no version"
        );
      }
      break;
    }
    case "dispatch_issues": {
      if (optionalBoolean(args, "all") === true) {
        const paging = [
          ...(optionalNumber(args, "limit") === undefined ? [] : ["--limit"]),
          ...(optionalNumber(args, "offset") === undefined ? [] : ["--offset"]),
        ];
        if (paging.length > 0) {
          problems.push(
            `--all takes every matching issue in one unpaged read, so it cannot be combined with ${paging.join(" or ")}: name --all on its own, or page with --limit and --offset`
          );
        }
      }
      break;
    }
  }
  return problems;
}

/** What a `dispatch_read` of the owner-resolved `args` prints in place of an issue's events, or
 *  undefined for an issue read or its `dispatch://KEY/log`, the two reads that list them. The
 *  ref's kind comes before `artifact` and `project`: a project document's ask or comment ref
 *  fills both, and the read prints the ask or comment. */
function readWithoutEvents(args: ToolArguments): string | undefined {
  if (optionalString(args, "message") !== undefined) return "a message's conversation";
  const ref = optionalString(args, "ref");
  switch (ref === undefined ? undefined : parseDispatchRef(ref)?.kind) {
    case "ask":
      return "an ask";
    case "comment":
      return "a comment";
    case "message":
      return "a message's conversation";
    case "children":
      return "the issue's children";
    // An artifact beside a log ref is refused by `ownerConflictProblems`, which says to drop the
    // artifact; the log lists events, so the count stays.
    case "log":
      return undefined;
  }
  if (
    optionalString(args, "artifact") !== undefined ||
    optionalString(args, "project") !== undefined
  ) {
    return "a document summary";
  }
  return undefined;
}
/**
 * The owner arguments of one call name one thing, and the command uses every one it is given:
 * an argument another one overrides, or one the command has no use for, would otherwise be
 * dropped without a word. Each problem names the spelling that does what the caller meant.
 */
export function ownerConflictProblems(
  tool: string,
  args: ToolArguments,
  parsedRef: ParsedDispatchRef | null,
  refText: string | undefined
): string[] {
  const problems: string[] = [];
  const command = `dispatch ${commandName(tool)}`;
  const issue = optionalString(args, "issue");
  const project = optionalString(args, "project");
  const artifact = optionalString(args, "artifact");
  const version = optionalNumber(args, "version");
  // A dispatch ask beside an issue or project cites its ref and takes nothing else from it, so
  // the ref may name any other issue or document.
  const ref =
    tool === "dispatch_ask" && (issue !== undefined || project !== undefined) ? null : parsedRef;
  // The document a ref names: an issue's spec or artifact, the one a document block is in, or the
  // project document it is on. A block of an issue's comment or message names none.
  const refDocument =
    ref === null
      ? undefined
      : ref.owner.kind === "project"
        ? (ref.artifact ?? ref.id)
        : ref.kind === "spec" ||
            ref.kind === "artifact" ||
            (ref.kind === "block" && ref.item === undefined)
          ? ref.id
          : undefined;
  if (ref !== null && refText !== undefined) {
    const refOwner = refOwnerName(ref);
    // An external issue reference is compared once Dispatch has resolved it to its key.
    if (
      issue !== undefined &&
      (ref.owner.kind !== "issue" ||
        (nativeIssueKeyPattern.test(issue) && issue !== ref.owner.issue))
    ) {
      problems.push(
        `issue ${issue} and ref ${refText} name two owners; --ref ${refText} alone acts on ${refOwner}, and --issue ${issue} alone acts on ${issue}`
      );
    }
    if (project !== undefined && ref.owner.kind === "issue") {
      problems.push(
        `project ${project} and ref ${refText} name two owners; --ref ${refText} alone acts on ${refOwner}, and --project ${project}${artifact === undefined ? "" : ` --artifact ${artifact}`} alone acts on that project document`
      );
    }
    // A citation is all `dispatch ask` takes from its ref, so its artifact is the anchor's; a
    // project argument beside a project ref is held to the ref's document where the owner is
    // filled.
    if (
      tool !== "dispatch_ask" &&
      project === undefined &&
      artifact !== undefined &&
      refDocument !== undefined &&
      artifact !== refDocument
    ) {
      const owner =
        ref.owner.kind === "issue"
          ? `--issue ${ref.owner.issue}`
          : `--project ${ref.owner.project}`;
      problems.push(
        `artifact ${artifact} and ref ${refText} name two documents; --ref ${refText} alone names ${refDocument}, and ${owner} --artifact ${artifact} names ${artifact}`
      );
    }
    if (
      tool === "dispatch_read" &&
      project === undefined &&
      artifact !== undefined &&
      ref.owner.kind === "issue" &&
      ref.kind !== "issue" &&
      refDocument === undefined
    ) {
      problems.push(
        `ref ${refText} names no document, so dispatch read would read it and drop artifact ${artifact}: drop --artifact to read the ref, or read the document with --issue ${ref.owner.issue} --artifact ${artifact}`
      );
    }
    if (version !== undefined && ref.version !== undefined && version !== ref.version) {
      problems.push(
        `version ${version} and ref ${refText} name two versions; name the version once`
      );
    }
    if (
      ref.version !== undefined &&
      (tool === "dispatch_comment" || tool === "dispatch_suggest" || tool === "dispatch_doc_edit")
    ) {
      problems.push(
        `${command} acts on the live document, so the version ref ${refText} names would be dropped: drop its @v${ref.version}`
      );
    }
    if (ref.version !== undefined && tool === "dispatch_read") {
      problems.push(
        `dispatch read prints the live document's summary, so the version ref ${refText} names would be dropped: read that version with dispatch doc-read --ref ${refText}`
      );
    }
  }
  // A comment or an ask on an issue lands on one of its documents only when it quotes it; the
  // document a call names without a quote would be dropped. A block ref is refused whole
  // (`innerRefProblem`), so its document is not named again here.
  const projectOwned = project !== undefined || ref?.owner.kind === "project";
  if (tool === "dispatch_comment" && !projectOwned && optionalString(args, "quote") === undefined) {
    const document = artifact ?? (ref?.kind === "block" ? undefined : refDocument);
    if (document !== undefined) {
      problems.push(
        `${command} posts on the issue unless quote anchors it in a document, so document ${document} would be dropped: add --quote to comment on a passage of it, or name only the issue`
      );
    }
  }
  if (tool === "dispatch_ask") {
    const anchor = asObject(args.anchor);
    const anchorArtifact = anchor === null ? undefined : optionalString(anchor, "artifact");
    if (!projectOwned && artifact !== undefined && anchor === null) {
      problems.push(
        `${command} opens the ask on the issue unless anchor quotes a document, so artifact ${artifact} would be dropped: add --anchor-json '${JSON.stringify({ artifact, quote: "<exact text>" })}' to ask about a passage of it, or drop --artifact`
      );
    }
    if (artifact !== undefined && anchorArtifact !== undefined && anchorArtifact !== artifact) {
      problems.push(
        `artifact ${artifact} and anchor.artifact ${anchorArtifact} name two documents; name the document once`
      );
    }
  }
  return problems;
}

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
/**
 * Fills the owner (issue or project) from the arguments, the ref, or LEGION_ISSUE, pushing
 * every defect onto `problems` and continuing so the caller reports them all at once.
 * A `ref` given as a dashboard URL on any of `dashboardUrls` (the API's own address, then the
 * public dashboard when there is one) is rewritten to its dispatch:// form.
 */
export async function resolveOwnerArguments(
  tool: string,
  input: ToolArguments,
  cwd: string,
  env: ExecutorEnvironment,
  exec: ExecFn,
  dashboardUrls: readonly string[],
  problems: string[]
): Promise<OwnerResolution> {
  if (issueFreeTools[tool] === true) return { args: input, ref: null, owner: null };
  const refArgument = input.ref;
  let ref: ParsedDispatchRef | null = null;
  let args = input;
  if (typeof refArgument === "string") {
    const refText =
      dashboardUrls
        .map((dashboard) => dispatchRefFromUrl(refArgument, dashboard))
        .find((ref) => ref !== undefined) ?? refArgument;
    if (refText !== refArgument) args = { ...input, ref: refText };
    const agentArtifact = parseAgentArtifactRef(refText);
    if (agentArtifact !== null) {
      // A conversation's upload belongs to no issue or project a write could post into, so only a
      // read names one, and by the ref alone.
      if (tool !== "dispatch_doc_read") {
        problems.push(
          `ref ${refText} names a picture in a conversation on the Agents page; only dispatch doc-read reads one`
        );
      }
      if (args.issue !== undefined || args.project !== undefined || args.artifact !== undefined) {
        problems.push(
          "a dispatch://agent/... ref names its upload alone: name no issue, project, or artifact with it"
        );
      }
      return { args, ref: null, owner: null, agentArtifact };
    }
    ref = parseDispatchRef(refText);
    if (ref === null) {
      problems.push(
        itemRefProblem(refText) ??
          (tool === "dispatch_ask" && !refText.startsWith("dispatch://")
            ? "ref must be a dispatch:// reference"
            : refGrammarProblem)
      );
    } else {
      const inner = innerRefProblem(tool, ref, refText);
      if (inner !== undefined) problems.push(inner);
    }
  }
  const issueArgument = args.issue;
  const projectArgument = args.project;
  const artifactArgument = args.artifact;
  // Only doc-read reads a version from the ref; every other tool reads or acts on the live
  // document, and `ownerConflictProblems` refuses a versioned ref one would write through.
  const versionFromRef =
    tool === "dispatch_doc_read" && args.version === undefined ? ref?.version : undefined;
  const refVersion = versionFromRef === undefined ? {} : { version: versionFromRef };
  // A dispatch ask beside an issue or project cites its ref and takes nothing else from it.
  const ownerRef =
    tool === "dispatch_ask" && (issueArgument !== undefined || projectArgument !== undefined)
      ? null
      : ref;

  if (issueArgument !== undefined && projectArgument !== undefined) {
    problems.push("exactly one of issue and project is required");
  }
  if (typeof projectArgument === "string") {
    if (!PROJECT_KEY_PATTERN.test(projectArgument)) {
      problems.push(`project is not a project key: ${PROJECT_KEY_RULE}`);
    }
    const refDocument =
      ownerRef?.owner.kind === "project" ? (ownerRef.artifact ?? ownerRef.id) : undefined;
    if (
      ownerRef?.owner.kind === "project" &&
      (ownerRef.owner.project !== projectArgument ||
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
        ...refVersion,
      },
      ref,
      owner: { kind: "project", project: projectArgument },
    };
  }
  if (ownerRef?.owner.kind === "project") {
    return {
      args: {
        ...args,
        project: ownerRef.owner.project,
        ...(artifactArgument === undefined ? { artifact: ownerRef.artifact ?? ownerRef.id } : {}),
        ...refVersion,
      },
      ref,
      owner: ownerRef.owner,
    };
  }
  if (issueArgument !== undefined || ownerRef !== null) {
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
        ...(artifactArgument === undefined &&
        tool !== "dispatch_ask" &&
        (ref?.kind === "spec" || ref?.kind === "artifact")
          ? { artifact: ref.id }
          : {}),
        ...refVersion,
      },
      ref,
      owner: issue === undefined ? null : { kind: "issue", issue },
    };
  }
  // A ref that names nothing is already refused above. It was meant to name the owner, so
  // LEGION_ISSUE never stands in for it, and a missing issue would only restate that refusal.
  if (typeof args.ref === "string") return { args, ref, owner: null };
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
/** The schema's own "issue is required": a required `issue` field left out, or the owner refine. */
function restatesMissingOwner(issue: z.core.$ZodIssue): boolean {
  return (
    (issue.code === "invalid_type" &&
      issue.path.length === 1 &&
      issue.path[0] === "issue" &&
      issue.input === undefined) ||
    (issue.code === "custom" &&
      issue.path.length === 0 &&
      (issue.message.startsWith("Exactly one of issue and project is required") ||
        issue.message.startsWith("issue is required unless in_reply_to")))
  );
}
/** The addresses `config` reaches Dispatch at, the API's and the dashboard's: what the call and
 *  its dry run both read a dashboard-URL `ref` against. */
export function dispatchServerUrls(
  config: Pick<ActiveDispatchConfig, "url" | "publicUrl">
): readonly string[] {
  return config.publicUrl === undefined ? [config.url] : [config.url, config.publicUrl];
}

/**
 * The check a call makes before it sends anything, the one `--dry-run` runs too: the owner (from
 * the arguments, the ref, or LEGION_ISSUE), the strict schema, the cross-field checks, and the
 * picture files `images` names. The schema sees the owner-resolved arguments, so it judges an
 * issue, artifact or version the ref supplies, and its validation sees the owner it requires.
 */
export async function checkDispatchCall(input: DispatchCallCheckInput): Promise<CheckedCall> {
  const { tool, args, cwd } = input;
  const problems: string[] = [];
  const owner = await resolveOwnerArguments(
    tool,
    args,
    cwd,
    input.env ?? process.env,
    input.exec ?? defaultExec,
    input.serverUrls,
    problems
  );
  // Judged on the caller's own arguments, before the ref filled any: the ref as resolved, read
  // back from a dashboard URL when it was one.
  const conflicts =
    issueFreeTools[tool] === true
      ? []
      : ownerConflictProblems(tool, args, owner.ref, optionalString(owner.args, "ref"));
  problems.push(...conflicts);
  // Every owner-missing exit (no issue, bad LEGION_ISSUE, no repo in cwd) leaves owner null, and
  // a conflict already names the owners; the schema's own "issue is required" would only restate
  // the actionable line already pushed.
  const ownerMissing =
    issueFreeTools[tool] !== true && (owner.owner === null || conflicts.length > 0);
  const schema = toolSchema(tool);
  const parsed = schema.safeParse(owner.args, { reportInput: true });
  if (!parsed.success) {
    const issues = ownerMissing
      ? parsed.error.issues.filter((issue) => !restatesMissingOwner(issue))
      : parsed.error.issues;
    problems.push(...formatZodIssues(issues, schema, tool));
  }
  problems.push(...argumentProblems(tool, owner.args));
  const pictures =
    pictureSendingTools[tool] === true ? await localPictures(owner.args.images, cwd, problems) : [];
  return {
    problems,
    owner,
    args: parsed.success ? (parsed.data as ToolArguments) : owner.args,
    pictures,
  };
}
