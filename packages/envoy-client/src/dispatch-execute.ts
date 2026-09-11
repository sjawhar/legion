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
  DuplicateCandidate,
  EditAskInput,
  EditOp,
  Event,
  IssueDetails,
  IssueReferences,
  SearchResult,
} from "@legion/contracts";
import {
  ASK_URGENCIES,
  dispatchDocumentSubject,
  dispatchIssueSubject,
  dispatchToolSchema,
  dispatchToolSpecs,
  snippetText,
  zodSchemaApi,
} from "@legion/contracts";
import { canonicalRepo } from "@legion/contracts/repo";
import { z } from "zod";
import type { DispatchConfigResolution } from "./dispatch-config";
import {
  type DispatchHost,
  type DispatchOrigin,
  defaultExec,
  type ExecFn,
  resolveCwdRepo,
  resolveOrigin,
} from "./dispatch-cwd";
import { DispatchClient, DispatchServiceError } from "./dispatch-http";

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
  readonly kind: "issue" | "spec" | "log" | "children" | "artifact" | "ask" | "comment";
  readonly id: string;
  readonly version?: number;
}

interface ResolvedArtifact {
  readonly owner: Owner;
  readonly issue?: IssueDetails;
  readonly artifact: Artifact;
}

function documentResultDetails(artifact: Artifact): Record<string, unknown> {
  return {
    project: artifact.project,
    artifact: artifact.id,
    document: `${artifact.project}/${artifact.slug}`,
    topic: dispatchDocumentSubject(artifact.project, artifact.slug, ">"),
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
  return {
    issue: resolved.issue.key,
    topic: dispatchIssueSubject(resolved.issue.key, ">"),
    ...fields,
  };
}

async function askResultDetails(
  client: DispatchClient,
  ask: Pick<Ask, "id" | "issue_key" | "artifact_id">,
  resolved?: ResolvedArtifact
) {
  if (ask.issue_key !== null) {
    return {
      issue: ask.issue_key,
      topic: dispatchIssueSubject(ask.issue_key, ">"),
      ask: ask.id,
    };
  }
  if (ask.artifact_id === undefined || ask.artifact_id === null) {
    throw new Error("document ask is missing its artifact ID");
  }
  const artifact = resolved?.artifact ?? (await client.getArtifact(ask.artifact_id));
  return { ...documentResultDetails(artifact), ask: ask.id };
}

const nativeIssueKeyPattern = /^[A-Z][A-Z0-9]{1,9}-[0-9]+$/;
const externalIssueRefPattern = /^([^/\s]+)\/([^/\s#]+)#([1-9][0-9]*)$/;
const bareIssueNumberPattern = /^[1-9][0-9]*$/;

const issueFreeTools: Readonly<Record<string, true>> = {
  dispatch_issue: true,
  dispatch_edit_ask: true,
  dispatch_resolve_ask: true,
  dispatch_search: true,
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
  const artifactName = result.artifact ? ` ${result.artifact.name}` : "";
  const label = `${result.issue.key} [${result.issue.status}] ${result.issue.title} - ${result.kind}${artifactName}`;
  const href = new URL(result.href, baseUrl).toString();
  return `${label}: ${snippetText(result.snippet)} -> ${href}`;
}

function askUrgency(args: ToolArguments): AskUrgency | undefined {
  const value = args.urgency;
  return ASK_URGENCIES.find((urgency) => urgency === value);
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
    };
  }

  const issueReference = ref.match(
    /^dispatch:\/\/([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?:\/(spec)|\/(log)|\/(children)|\/artifact\/([^/@]+)(?:@v(\d+))?|\/ask\/([^/]+)|\/comment\/([^/]+))?$/
  );
  if (!issueReference) return null;
  const [, issue, spec, log, children, artifact, version, ask, comment] = issueReference;
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
  return { owner, kind: "issue", id: issue };
}

function askId(args: ToolArguments): string {
  const ask = stringArg(args, "ask");
  if (!ask.startsWith("dispatch://")) return ask;
  const reference = parseDispatchRef(ask);
  if (reference?.kind !== "ask") {
    throw new Error("ask must be a bare ask id or a dispatch://.../ask/<id> reference");
  }
  return reference.id;
}

function toolSchema(tool: string): z.ZodType {
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === tool);
  if (!spec) throw new Error(`Unknown Dispatch tool: ${tool}`);
  return dispatchToolSchema(spec, zodSchemaApi(z), { strict: true });
}

async function resolveOwnerArguments(
  tool: string,
  args: ToolArguments,
  cwd: string,
  env: ExecutorEnvironment,
  exec: ExecFn
): Promise<{ args: ToolArguments; ref: ParsedDispatchRef | null; owner: Owner | null }> {
  if (issueFreeTools[tool] === true) return { args, ref: null, owner: null };
  const refArgument = args.ref;
  const ref =
    typeof refArgument === "string"
      ? (parseDispatchRef(refArgument) ??
        (() => {
          throw new Error(
            "ref must be a valid dispatch:// reference such as dispatch://KEY-1, " +
              "dispatch://KEY-1/ask/<uuid>, dispatch://KEY-1/comment/<uuid>, " +
              "dispatch://KEY-1/artifact/<slug>, or dispatch://PROJECT/artifact/<slug>"
          );
        })())
      : null;
  const issueArgument = args.issue;
  const projectArgument = args.project;
  const artifactArgument = args.artifact;
  const versionArgument = args.version;

  if (issueArgument !== undefined && projectArgument !== undefined) {
    throw new Error("exactly one of issue and project is required");
  }
  if (typeof projectArgument === "string") {
    if (!/^[A-Z][A-Z0-9]{1,9}$/.test(projectArgument)) {
      throw new Error("project must be a project key such as CORE");
    }
    if (
      ref?.owner.kind === "project" &&
      (ref.owner.project !== projectArgument ||
        (artifactArgument !== undefined && artifactArgument !== ref.id))
    ) {
      throw new Error("project and ref must name the same document");
    }
    return {
      args: {
        ...args,
        ...(artifactArgument === undefined && ref?.owner.kind === "project"
          ? { artifact: ref.id }
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
        ...(artifactArgument === undefined ? { artifact: ref.id } : {}),
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
  if (!legionIssue) throw new Error("issue is required; supply issue or set LEGION_ISSUE");
  if (nativeIssueKeyPattern.test(legionIssue) || externalIssueRefPattern.test(legionIssue)) {
    const issue = canonicalExternalIssueRef(legionIssue);
    return { args: { ...args, issue }, ref: null, owner: { kind: "issue", issue } };
  }
  if (!bareIssueNumberPattern.test(legionIssue)) {
    throw new Error(
      "LEGION_ISSUE must be a native issue key (e.g. LEGION-3), an external owner/repo#n reference, or a bare positive issue number"
    );
  }
  const repo = await resolveCwdRepo(cwd, exec);
  if (!repo) throw new Error("issue is required; LEGION_ISSUE needs a GitHub repository in cwd");
  const issue = `${repo}#${legionIssue}`;
  return { args: { ...args, issue }, ref: null, owner: { kind: "issue", issue } };
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
      // The project artifact route resolves only by slug; a caller that supplied the
      // filename (as shown in the dispatch_artifact upload result) falls back to a
      // name match against the project's artifact list.
      if (!(error instanceof DispatchServiceError) || error.status !== 404) throw error;
      const artifacts = await client.listProjectArtifacts(owner.project);
      const artifact = artifacts.find((candidate) => candidate.name === artifactReference);
      if (!artifact) throw error;
      return { owner, artifact };
    }
  }
  const issue = await client.getIssue(owner.issue);
  const artifact =
    artifactReference === undefined || artifactReference === "spec"
      ? issue.artifacts.find(
          (candidate) => candidate.primary || candidate.id === issue.primary_artifact_id
        )
      : issue.artifacts.find(
          (candidate) =>
            candidate.id === artifactReference ||
            candidate.slug === artifactReference ||
            candidate.name === artifactReference
        );
  if (!artifact) {
    throw new Error(`artifact ${artifactReference ?? "spec"} was not found on issue ${issue.key}`);
  }
  return { owner, issue, artifact };
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

function issueSummary(
  issue: IssueDetails,
  events: readonly Event[],
  references: IssueReferences | string
): string {
  const asks = issue.open_asks;
  return [
    `Title: ${issue.title}`,
    `Key: ${issue.key}`,
    `Status: ${issue.status}`,
    `Route: ${issue.route ?? "none"}`,
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
    ...(events.length === 0
      ? ["- none"]
      : events.map(
          (event) =>
            `- #${event.seq} ${event.type} · ${event.actor.kind} ${event.actor.id} · ${event.created_at}`
        )),
  ].join("\n");
}

function logSummary(issue: IssueDetails, events: readonly Event[]): string {
  return [
    `Key: ${issue.key}`,
    "Events:",
    ...(events.length === 0
      ? ["- none"]
      : events.map(
          (event) =>
            `- #${event.seq} ${event.type} · ${event.actor.kind} ${event.actor.id} · ${event.created_at}`
        )),
  ].join("\n");
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

function askSummary({ ask, replies }: AskRead): string {
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
  ].join("\n");
}

function commentSummary({ comment, replies }: CommentRead): string {
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
  return ["Comment:", ...root, "Reply chain:", ...(chain.length === 0 ? ["- none"] : chain)].join(
    "\n"
  );
}

async function openArtifactMarks(
  client: DispatchClient,
  resolved: ResolvedArtifact
): Promise<string[]> {
  const asks =
    resolved.owner.kind === "project"
      ? await client.getArtifactAsks(resolved.artifact.id)
      : (resolved.issue?.open_asks ?? []);
  const marks = asks
    .filter((ask) => ask.state === "open" && ask.anchor?.artifact_id === resolved.artifact.id)
    .map((ask) => `ask ${ask.id}`);
  let comments: Comment[];
  try {
    comments =
      resolved.owner.kind === "project"
        ? await client.getArtifactComments(resolved.artifact.id)
        : await client.getComments(resolved.issue?.key ?? "", resolved.artifact.id);
  } catch (error) {
    if (error instanceof DispatchServiceError && error.status === 404) return marks;
    throw error;
  }
  return [
    ...marks,
    ...comments
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
  const env = input.env ?? process.env;
  const exec = input.exec ?? defaultExec;
  const ownerArguments = await resolveOwnerArguments(input.tool, input.args, input.cwd, env, exec);
  // The tool's strict Zod schema has validated every argument by the time it is
  // read below: unknown keys and wrong types are rejected here, so a structured
  // argument only needs the contract's shape named when it is forwarded.
  const args = toolSchema(input.tool).parse(ownerArguments.args) as ToolArguments;
  const actor = toolActor(await resolveOrigin(env, exec, input.cwd), input);
  const client = new DispatchClient(configUrl, configToken, input.fetchImpl);
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
      try {
        const created = await client.issue({
          project,
          title,
          ...(parent === undefined ? {} : { parent }),
          ...(external === undefined ? {} : { external }),
          ...(force === undefined ? {} : { force }),
          ...(spec === undefined ? {} : { spec }),
          actor,
        });
        return {
          text: `Created ${created.key}: ${created.title}`,
          details: { issue: created.key, topic: dispatchIssueSubject(created.key, ">") },
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
    case "dispatch_resolve_ask": {
      const kind = stringArg(args, "kind");
      if (kind !== "retracted" && kind !== "resolved")
        throw new Error("kind must be retracted or resolved");
      const ask = await client.resolveAsk(stringArg(args, "ask"), {
        kind,
        reason: stringArg(args, "reason"),
        actor,
      });
      if (ask.resolution === undefined) throw new Error("resolved ask is missing its resolution");
      return {
        text: `${kind === "retracted" ? "Retracted" : "Resolved"} ask ${ask.id}: ${ask.resolution.reason}`,
        details: await askResultDetails(client, ask),
      };
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
        question: stringArg(args, "question"),
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
      return {
        text: `Opened ask ${ask.id}: ${ask.question}`,
        details: await askResultDetails(client, ask, resolved),
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
        details: await askResultDetails(client, ask),
      };
    }
    case "dispatch_comment": {
      const artifactReference = optionalString(args, "artifact");
      if (optionalString(args, "quote") !== undefined && artifactReference === undefined) {
        throw new Error("artifact is required when quote is supplied");
      }
      const owner = documentOwner();
      const resolved =
        owner.kind === "project" || artifactReference === undefined
          ? owner.kind === "project"
            ? await resolveArtifact(client, owner, artifactReference)
            : undefined
          : await resolveArtifact(client, owner, artifactReference);
      const anchored = resolved ? anchor(resolved.artifact, args) : undefined;
      const replyTo = optionalString(args, "reply_to");
      const replyToAsk = optionalString(args, "reply_to_ask");
      if (replyTo !== undefined && replyToAsk !== undefined) {
        throw new Error("reply_to and reply_to_ask cannot both be set");
      }
      const commentInput = {
        body: stringArg(args, "body"),
        ...(anchored === undefined ? {} : { anchor: anchored }),
        ...(replyTo === undefined ? {} : { reply_to: replyTo }),
        ...(replyToAsk === undefined ? {} : { ask_id: replyToAsk }),
        actor,
      };
      const comment =
        resolved?.owner.kind === "project"
          ? await client.artifactComment(resolved.artifact.id, commentInput)
          : await client.comment(issue(), commentInput);
      return {
        text: `Posted comment ${comment.id}`,
        details:
          resolved === undefined
            ? {
                issue: comment.issue_key,
                topic: dispatchIssueSubject(issue(), ">"),
                comment: comment.id,
              }
            : writeResultDetails(resolved, { comment: comment.id }),
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
        text: `Posted suggestion ${comment.id}`,
        details: writeResultDetails(resolved, { comment: comment.id }),
      };
    }
    case "dispatch_message": {
      const message = await client.message(issue(), { body: stringArg(args, "body"), actor });
      return {
        text: `Posted message ${message.id}`,
        details: {
          issue: message.issue_key,
          topic: dispatchIssueSubject(message.issue_key, ">"),
          message: message.id,
        },
      };
    }
    case "dispatch_doc_edit": {
      const resolved = await resolveArtifact(client, documentOwner(), stringArg(args, "artifact"));
      const ops = args.ops as EditOp[];
      const summary = optionalString(args, "summary");
      const edited = await client.docEdit(resolved.artifact.id, {
        ops,
        ...(summary === undefined ? {} : { summary }),
        actor,
      });
      return {
        text:
          edited.version === null
            ? `Applied ${edited.applied} ops (no new version)`
            : `Applied ${edited.applied} ops (version ${edited.version.number})`,
        details: writeResultDetails(resolved, {
          applied: edited.applied,
          ...(edited.version === null ? {} : { version: edited.version.number }),
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
      const document = await client.docRead(resolved.artifact.id, version);
      const marks = await openArtifactMarks(client, resolved);
      return {
        text:
          marks.length === 0
            ? document.markdown
            : `${document.markdown}\n\nOpen anchored asks/comments: ${marks.join(", ")}`,
        details:
          resolved.owner.kind === "project"
            ? {
                project: resolved.artifact.project,
                document: `${resolved.artifact.project}/${resolved.artifact.slug}`,
              }
            : { issue: resolved.issue?.key },
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
      const artifactRef =
        artifactOwner.kind === "project"
          ? `dispatch://${artifactOwner.project}/artifact/${result.artifact.slug}`
          : `dispatch://${issue()}/artifact/${result.artifact.slug}`;
      return {
        text: `Uploaded ${result.artifact.name} as version ${result.version.number} (artifact slug ${result.artifact.slug}; ${artifactRef})`,
        details:
          artifactOwner.kind === "project"
            ? {
                ...documentResultDetails(result.artifact),
                version: result.version.number,
              }
            : {
                issue: issue(),
                topic: dispatchIssueSubject(issue(), ">"),
                artifact: result.artifact.id,
                version: result.version.number,
              },
      };
    }
    // Reads report their owner but no `topic`: only a write subscribes the session.
    case "dispatch_read": {
      if (ownerArguments.ref?.kind === "ask") {
        const askRead = await client.getAsk(ownerArguments.ref.id);
        return {
          text: askSummary(askRead),
          details:
            ownerArguments.ref.owner.kind === "project"
              ? { project: ownerArguments.ref.owner.project }
              : { issue: ownerArguments.ref.owner.issue },
        };
      }
      if (ownerArguments.ref?.kind === "comment") {
        const comment = await client.getComment(ownerArguments.ref.id);
        return {
          text: commentSummary(comment),
          details:
            ownerArguments.ref.owner.kind === "project"
              ? { project: ownerArguments.ref.owner.project }
              : { issue: comment.comment.issue_key },
        };
      }
      if (documentOwner().kind === "project") {
        const resolved = await resolveArtifact(
          client,
          documentOwner(),
          stringArg(args, "artifact")
        );
        return {
          text: [
            `Document: ${resolved.artifact.project} / ${resolved.artifact.name}`,
            `Reference: dispatch://${resolved.artifact.project}/artifact/${resolved.artifact.slug}`,
            `Versions: ${resolved.artifact.versions.length}`,
          ].join("\n"),
          details: {
            project: resolved.artifact.project,
            document: `${resolved.artifact.project}/${resolved.artifact.slug}`,
          },
        };
      }
      const read = await client.read(issue());
      if (ownerArguments.ref?.kind === "log") {
        return {
          text: logSummary(read.issue, read.events),
          details: { issue: read.issue.key },
        };
      }
      if (ownerArguments.ref?.kind === "children") {
        return {
          text: childrenSummary(read.issue),
          details: { issue: read.issue.key },
        };
      }
      let references: IssueReferences | string;
      try {
        references = await client.getIssueReferences(read.issue.key);
      } catch (error) {
        references =
          error instanceof DispatchServiceError && error.status === 404
            ? "unavailable"
            : `unavailable: ${error instanceof Error ? error.message : String(error)}`;
      }
      return {
        text: issueSummary(read.issue, read.events, references),
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
