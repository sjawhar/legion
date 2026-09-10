import { resolve as resolvePath } from "node:path";
import type {
  Actor,
  Artifact,
  Ask,
  AskUrgency,
  Comment,
  CommentRead,
  CreateAskInput,
  EditOp,
  Event,
  IssueDetails,
} from "@legion/contracts";
import {
  ASK_URGENCIES,
  dispatchIssueSubject,
  dispatchToolSchema,
  dispatchToolSpecs,
  zodSchemaApi,
} from "@legion/contracts";
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

interface ParsedDispatchRef {
  readonly issue: string;
  readonly kind: "issue" | "spec" | "log" | "children" | "artifact" | "ask" | "comment";
  readonly id: string;
  readonly version?: number;
}

interface ResolvedArtifact {
  readonly issue: IssueDetails;
  readonly artifact: Artifact;
}

const nativeIssueKeyPattern = /^[A-Z][A-Z0-9]{1,9}-[0-9]+$/;
const externalIssueRefPattern = /^([^/\s]+)\/([^/\s#]+)#([1-9][0-9]*)$/;
const bareIssueNumberPattern = /^[1-9][0-9]*$/;

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

function askUrgency(args: ToolArguments): AskUrgency | undefined {
  const value = args.urgency;
  return ASK_URGENCIES.find((urgency) => urgency === value);
}

function parseDispatchRef(ref: string): ParsedDispatchRef | null {
  const match = ref.match(
    /^dispatch:\/\/([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)(?:\/(spec)|\/(log)|\/(children)|\/artifact\/([^/@]+)(?:@v(\d+))?|\/ask\/([^/]+)|\/comment\/([^/]+))?$/
  );
  if (!match) return null;
  const [, issue, spec, log, children, artifact, version, ask, comment] = match;
  if (!issue || (version !== undefined && Number(version) < 1)) return null;
  if (spec) return { issue, kind: "spec", id: spec };
  if (log) return { issue, kind: "log", id: log };
  if (children) return { issue, kind: "children", id: children };
  if (artifact) {
    return {
      issue,
      kind: "artifact",
      id: artifact,
      ...(version === undefined ? {} : { version: Number(version) }),
    };
  }
  if (ask) return { issue, kind: "ask", id: ask };
  if (comment) return { issue, kind: "comment", id: comment };
  return { issue, kind: "issue", id: issue };
}

function toolSchema(tool: string): z.ZodType {
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === tool);
  if (!spec) throw new Error(`Unknown Dispatch tool: ${tool}`);
  return dispatchToolSchema(spec, zodSchemaApi(z), { strict: true });
}

async function resolveIssueArguments(
  tool: string,
  args: ToolArguments,
  cwd: string,
  env: ExecutorEnvironment,
  exec: ExecFn
): Promise<{ args: ToolArguments; ref: ParsedDispatchRef | null }> {
  if (tool === "dispatch_issue") return { args, ref: null };
  const refArgument = args.ref;
  const ref =
    typeof refArgument === "string"
      ? (parseDispatchRef(refArgument) ??
        (() => {
          throw new Error("ref must be a valid dispatch:// reference");
        })())
      : null;
  const issueArgument = args.issue;
  const artifactArgument = args.artifact;
  const versionArgument = args.version;
  if (issueArgument !== undefined || ref !== null) {
    return {
      args: {
        ...args,
        ...(issueArgument === undefined && ref?.issue !== undefined ? { issue: ref.issue } : {}),
        ...(artifactArgument === undefined && (ref?.kind === "spec" || ref?.kind === "artifact")
          ? { artifact: ref.id }
          : {}),
        ...(versionArgument === undefined && ref?.version !== undefined
          ? { version: ref.version }
          : {}),
      },
      ref,
    };
  }
  const legionIssue = env.LEGION_ISSUE;
  if (!legionIssue) throw new Error("issue is required; supply issue or set LEGION_ISSUE");
  if (nativeIssueKeyPattern.test(legionIssue) || externalIssueRefPattern.test(legionIssue)) {
    return { args: { ...args, issue: legionIssue }, ref: null };
  }
  if (!bareIssueNumberPattern.test(legionIssue)) {
    throw new Error(
      "LEGION_ISSUE must be a native issue key (e.g. LEGION-3), an external owner/repo#n reference, or a bare positive issue number"
    );
  }
  const repo = await resolveCwdRepo(cwd, exec);
  if (!repo) throw new Error("issue is required; LEGION_ISSUE needs a GitHub repository in cwd");
  return { args: { ...args, issue: `${repo}#${legionIssue}` }, ref: null };
}

async function resolveArtifact(
  client: DispatchClient,
  issueReference: string,
  artifactReference: string | undefined
): Promise<ResolvedArtifact> {
  const issue = await client.getIssue(issueReference);
  const artifact =
    artifactReference === undefined || artifactReference === "spec"
      ? issue.artifacts.find(
          (candidate) => candidate.primary || candidate.id === issue.primary_artifact_id
        )
      : issue.artifacts.find(
          (candidate) => candidate.id === artifactReference || candidate.slug === artifactReference
        );
  if (!artifact) {
    throw new Error(`artifact ${artifactReference ?? "spec"} was not found on issue ${issue.key}`);
  }
  return { issue, artifact };
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

function issueSummary(issue: IssueDetails, events: readonly Event[]): string {
  const asks = issue.open_asks;
  return [
    `Title: ${issue.title}`,
    `Key: ${issue.key}`,
    `Status: ${issue.status}`,
    `Route: ${issue.route ?? "none"}`,
    "Open asks:",
    ...(asks.length === 0 ? ["- none"] : asks.map((ask) => `- ${ask.id}: ${ask.question}`)),
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

function askSummary(ask: Ask): string {
  const answer = ask.answer;
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
  const marks = resolved.issue.open_asks
    .filter((ask) => ask.state === "open" && ask.anchor?.artifact_id === resolved.artifact.id)
    .map((ask) => `ask ${ask.id}`);
  let comments: Comment[];
  try {
    comments = await client.getComments(resolved.issue.key, resolved.artifact.id);
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
  if (!input.config.enabled || !input.config.url || !input.config.token) {
    throw new Error("Dispatch is disabled; resolve both DISPATCH_URL and DISPATCH_TOKEN");
  }
  const env = input.env ?? process.env;
  const exec = input.exec ?? defaultExec;
  const issueArguments = await resolveIssueArguments(input.tool, input.args, input.cwd, env, exec);
  // The tool's strict Zod schema has validated every argument by the time it is
  // read below: unknown keys and wrong types are rejected here, so a structured
  // argument only needs the contract's shape named when it is forwarded.
  const args = toolSchema(input.tool).parse(issueArguments.args) as ToolArguments;
  const actor = toolActor(await resolveOrigin(env, exec, input.cwd), input);
  const client = new DispatchClient(input.config.url, input.config.token, input.fetchImpl);
  const issueKey =
    input.tool === "dispatch_issue"
      ? null
      : await ensureIssue(client, stringArg(args, "issue"), actor);
  const issue = () => {
    if (issueKey === null) throw new Error("issue is required");
    return issueKey;
  };

  switch (input.tool) {
    case "dispatch_issue": {
      const parent = optionalString(args, "parent");
      const external = optionalString(args, "external");
      const spec = optionalString(args, "spec");
      const created = await client.issue({
        project: stringArg(args, "project"),
        title: stringArg(args, "title"),
        ...(parent === undefined ? {} : { parent }),
        ...(external === undefined ? {} : { external }),
        ...(spec === undefined ? {} : { spec }),
        actor,
      });
      return {
        text: `Created ${created.key}: ${created.title}`,
        details: { issue: created.key, topic: dispatchIssueSubject(created.key, ">") },
      };
    }
    case "dispatch_ask": {
      const anchorArgs = asObject(args.anchor);
      const resolved = anchorArgs
        ? await resolveArtifact(client, issue(), stringArg(anchorArgs, "artifact"))
        : undefined;
      const options = args.options;
      const multiple = optionalBoolean(args, "multiple");
      const urgency = askUrgency(args);
      const anchored = anchorArgs && resolved ? anchor(resolved.artifact, anchorArgs) : undefined;
      const ask = await client.ask(issue(), {
        question: stringArg(args, "question"),
        ...(Array.isArray(options)
          ? { options: options as NonNullable<CreateAskInput["options"]> }
          : {}),
        ...(multiple === undefined ? {} : { multiple }),
        ...(urgency === undefined ? {} : { urgency }),
        ...(anchored === undefined ? {} : { anchor: anchored }),
        actor,
      });
      return {
        text: `Opened ask ${ask.id}: ${ask.question}`,
        details: {
          issue: ask.issue_key,
          topic: dispatchIssueSubject(ask.issue_key, ">"),
          ask: ask.id,
        },
      };
    }
    case "dispatch_comment": {
      const artifactReference = optionalString(args, "artifact");
      if (optionalString(args, "quote") !== undefined && artifactReference === undefined) {
        throw new Error("artifact is required when quote is supplied");
      }
      const resolved = artifactReference
        ? await resolveArtifact(client, issue(), artifactReference)
        : undefined;
      const anchored = resolved ? anchor(resolved.artifact, args) : undefined;
      const replyTo = optionalString(args, "reply_to");
      const comment = await client.comment(issue(), {
        body: stringArg(args, "body"),
        ...(anchored === undefined ? {} : { anchor: anchored }),
        ...(replyTo === undefined ? {} : { reply_to: replyTo }),
        actor,
      });
      return {
        text: `Posted comment ${comment.id}`,
        details: {
          issue: comment.issue_key,
          topic: dispatchIssueSubject(comment.issue_key, ">"),
          comment: comment.id,
        },
      };
    }
    case "dispatch_suggest": {
      const resolved = await resolveArtifact(client, issue(), stringArg(args, "artifact"));
      const anchored = anchor(resolved.artifact, args);
      if (anchored === undefined) throw new Error("quote is required");
      const body = optionalString(args, "body");
      const comment = await client.suggest(issue(), {
        ...(body === undefined ? {} : { body }),
        anchor: anchored,
        replace_with: stringArg(args, "replace_with"),
        actor,
      });
      return {
        text: `Posted suggestion ${comment.id}`,
        details: {
          issue: comment.issue_key,
          topic: dispatchIssueSubject(comment.issue_key, ">"),
          comment: comment.id,
        },
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
      const resolved = await resolveArtifact(client, issue(), stringArg(args, "artifact"));
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
        details: {
          issue: resolved.issue.key,
          topic: dispatchIssueSubject(resolved.issue.key, ">"),
          applied: edited.applied,
          ...(edited.version === null ? {} : { version: edited.version.number }),
        },
      };
    }
    case "dispatch_doc_read": {
      const artifactReference =
        optionalString(args, "artifact") ??
        (issueArguments.ref?.kind === "spec" || issueArguments.ref?.kind === "artifact"
          ? issueArguments.ref.id
          : undefined);
      const resolved = await resolveArtifact(client, issue(), artifactReference);
      const version = optionalNumber(args, "version") ?? issueArguments.ref?.version;
      const document = await client.docRead(resolved.artifact.id, version);
      const marks = await openArtifactMarks(client, resolved);
      return {
        text:
          marks.length === 0
            ? document.markdown
            : `${document.markdown}\n\nOpen anchored asks/comments: ${marks.join(", ")}`,
        details: {
          issue: resolved.issue.key,
          topic: dispatchIssueSubject(resolved.issue.key, ">"),
        },
      };
    }
    case "dispatch_artifact": {
      const primary = optionalBoolean(args, "primary");
      const summary = optionalString(args, "summary");
      const name = stringArg(args, "name");
      const content = optionalString(args, "content");
      const result = await client.artifact(
        issue(),
        content === undefined
          ? {
              name,
              file: Bun.file(resolvePath(input.cwd, stringArg(args, "path"))),
              ...(primary === undefined ? {} : { primary }),
              ...(summary === undefined ? {} : { summary }),
              actor,
            }
          : {
              name,
              content,
              ...(primary === undefined ? {} : { primary }),
              ...(summary === undefined ? {} : { summary }),
              actor,
            }
      );
      return {
        text: `Uploaded ${result.artifact.name} as version ${result.version.number}`,
        details: {
          issue: result.artifact.issue_key,
          topic: dispatchIssueSubject(result.artifact.issue_key, ">"),
          artifact: result.artifact.id,
          version: result.version.number,
        },
      };
    }
    case "dispatch_read": {
      if (issueArguments.ref?.kind === "ask") {
        const ask = await client.getAsk(issueArguments.ref.id);
        return {
          text: askSummary(ask),
          details: { issue: ask.issue_key, topic: dispatchIssueSubject(ask.issue_key, ">") },
        };
      }
      if (issueArguments.ref?.kind === "comment") {
        const comment = await client.getComment(issueArguments.ref.id);
        return {
          text: commentSummary(comment),
          details: {
            issue: comment.comment.issue_key,
            topic: dispatchIssueSubject(comment.comment.issue_key, ">"),
          },
        };
      }
      const read = await client.read(issue());
      if (issueArguments.ref?.kind === "log") {
        return {
          text: logSummary(read.issue, read.events),
          details: { issue: read.issue.key, topic: dispatchIssueSubject(read.issue.key, ">") },
        };
      }
      if (issueArguments.ref?.kind === "children") {
        return {
          text: childrenSummary(read.issue),
          details: { issue: read.issue.key, topic: dispatchIssueSubject(read.issue.key, ">") },
        };
      }
      return {
        text: issueSummary(read.issue, read.events),
        details: { issue: read.issue.key, topic: dispatchIssueSubject(read.issue.key, ">") },
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
        `repository ${repository} is not mapped in DISPATCH_REPO_PROJECTS and no DISPATCH_DEFAULT_PROJECT is configured`
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
