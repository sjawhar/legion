import { dispatchToolSpecs, zodSchemaApi } from "@legion/contracts";
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
import {
  type Actor,
  type Artifact,
  type AskInput,
  type Comment,
  type CommentInput,
  DispatchClient,
  DispatchServiceError,
  type EditOperation,
  type Event,
  type IssueDetails,
} from "./dispatch-http";

export interface ExecuteDispatchToolInput {
  readonly tool: string;
  readonly args: Record<string, unknown>;
  readonly cwd: string;
  readonly host: DispatchHost;
  readonly sessionId?: string;
  readonly sessionTitle?: string;
  readonly config: DispatchConfigResolution;
  readonly env?: Record<string, string | undefined>;
  readonly fetchImpl?: typeof fetch;
  readonly exec?: ExecFn;
}

export interface DispatchToolResult {
  readonly text: string;
  readonly details: Record<string, unknown>;
}

interface ParsedDispatchRef {
  readonly issue: string;
  readonly artifact?: string;
  readonly version?: number;
}

interface ResolvedArtifact {
  readonly issue: IssueDetails;
  readonly artifact: Artifact;
}

function dispatchTopic(issue: string): string {
  return `notifications.dispatch.issue.${issue}.>`;
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

function parseDispatchRef(ref: string): ParsedDispatchRef | null {
  const match = ref.match(/^dispatch:\/\/([^/]+)(?:\/(?:spec|artifacts\/([^/@]+)(?:@v(\d+))?))?$/);
  if (!match) return null;
  const [, issue, artifact, version] = match;
  if (!issue) return null;
  return {
    issue,
    ...(artifact === undefined ? {} : { artifact }),
    ...(version === undefined ? {} : { version: Number(version) }),
  };
}

function toolSchema(tool: string): z.ZodObject<z.ZodRawShape> {
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === tool);
  if (!spec) throw new Error(`Unknown Dispatch tool: ${tool}`);
  return z.object(spec.arguments(zodSchemaApi(z)) as z.ZodRawShape).strict();
}

async function resolveIssueArguments(
  tool: string,
  args: Record<string, unknown>,
  cwd: string,
  env: Record<string, string | undefined>,
  exec: ExecFn
): Promise<{ args: Record<string, unknown>; ref: ParsedDispatchRef | null }> {
  if (tool === "dispatch_issue") return { args, ref: null };
  const refArgument = args["ref"];
  const ref =
    typeof refArgument === "string"
      ? (parseDispatchRef(refArgument) ??
        (() => {
          throw new Error("ref must be a valid dispatch:// reference");
        })())
      : null;
  const issueArgument = args["issue"];
  const artifactArgument = args["artifact"];
  const versionArgument = args["version"];
  if (issueArgument !== undefined || ref !== null) {
    return {
      args: {
        ...args,
        ...(issueArgument === undefined && ref?.issue !== undefined ? { issue: ref.issue } : {}),
        ...(artifactArgument === undefined && ref?.artifact !== undefined
          ? { artifact: ref.artifact }
          : {}),
        ...(versionArgument === undefined && ref?.version !== undefined
          ? { version: ref.version }
          : {}),
      },
      ref,
    };
  }
  const legionIssue = env["LEGION_ISSUE"];
  if (!legionIssue) throw new Error("issue is required; supply issue or set LEGION_ISSUE");
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
  const asks = Array.isArray(issue.open_asks) ? issue.open_asks : [];
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

async function openArtifactMarks(
  client: DispatchClient,
  resolved: ResolvedArtifact
): Promise<string[]> {
  const asks = Array.isArray(resolved.issue.open_asks) ? resolved.issue.open_asks : [];
  const marks = asks
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
  const args = toolSchema(input.tool).parse(issueArguments.args) as Record<string, unknown>;
  const actor = toolActor(await resolveOrigin(env, exec, input.cwd), input);
  const client = new DispatchClient(input.config.url, input.config.token, input.fetchImpl);
  const issue = () => stringArg(args, "issue");

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
        details: { issue: created.key, topic: dispatchTopic(created.key) },
      };
    }
    case "dispatch_ask": {
      const anchorArgs = asObject(args["anchor"]);
      const resolved = anchorArgs
        ? await resolveArtifact(client, issue(), stringArg(anchorArgs, "artifact"))
        : undefined;
      const options = args["options"];
      const multiple = optionalBoolean(args, "multiple");
      const custom = optionalBoolean(args, "custom");
      const urgency = optionalString(args, "urgency") as AskInput["urgency"] | undefined;
      const anchored = anchorArgs && resolved ? anchor(resolved.artifact, anchorArgs) : undefined;
      const ask = await client.ask(issue(), {
        question: stringArg(args, "question"),
        ...(Array.isArray(options) ? { options: options as NonNullable<AskInput["options"]> } : {}),
        ...(multiple === undefined ? {} : { multiple }),
        ...(custom === undefined ? {} : { custom }),
        ...(urgency === undefined ? {} : { urgency }),
        ...(anchored === undefined ? {} : { anchor: anchored }),
        actor,
      });
      return {
        text: `Opened ask ${ask.id}: ${ask.question}`,
        details: { issue: ask.issue_key, topic: dispatchTopic(ask.issue_key), ask: ask.id },
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
          topic: dispatchTopic(comment.issue_key),
          comment: comment.id,
        },
      };
    }
    case "dispatch_suggest": {
      const resolved = await resolveArtifact(client, issue(), stringArg(args, "artifact"));
      const comment = await client.suggest(issue(), {
        body: optionalString(args, "body") ?? "",
        anchor: anchor(resolved.artifact, args) as NonNullable<CommentInput["anchor"]>,
        replace_with: stringArg(args, "replace_with"),
        actor,
      });
      return {
        text: `Posted suggestion ${comment.id}`,
        details: {
          issue: comment.issue_key,
          topic: dispatchTopic(comment.issue_key),
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
          topic: dispatchTopic(message.issue_key),
          message: message.id,
        },
      };
    }
    case "dispatch_doc_edit": {
      const resolved = await resolveArtifact(client, issue(), stringArg(args, "artifact"));
      const ops = args["ops"] as EditOperation[];
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
          topic: dispatchTopic(resolved.issue.key),
          applied: edited.applied,
          ...(edited.version === null ? {} : { version: edited.version.number }),
        },
      };
    }
    case "dispatch_doc_read": {
      const artifactReference = optionalString(args, "artifact") ?? issueArguments.ref?.artifact;
      const resolved = await resolveArtifact(client, issue(), artifactReference);
      const version = optionalNumber(args, "version") ?? issueArguments.ref?.version;
      const document = await client.docRead(resolved.artifact.id, version);
      const marks = await openArtifactMarks(client, resolved);
      return {
        text:
          marks.length === 0
            ? document.markdown
            : `${document.markdown}\n\nOpen anchored asks/comments: ${marks.join(", ")}`,
        details: { issue: resolved.issue.key },
      };
    }
    case "dispatch_artifact": {
      const primary = optionalBoolean(args, "primary");
      const summary = optionalString(args, "summary");
      const result = await client.artifact(issue(), {
        name: stringArg(args, "name"),
        file: Bun.file(stringArg(args, "path")),
        ...(primary === undefined ? {} : { primary }),
        ...(summary === undefined ? {} : { summary }),
        actor,
      });
      return {
        text: `Uploaded ${result.artifact.name} as version ${result.version.number}`,
        details: {
          issue: result.artifact.issue_key,
          topic: dispatchTopic(result.artifact.issue_key),
          artifact: result.artifact.id,
          version: result.version.number,
        },
      };
    }
    case "dispatch_read": {
      const read = await client.read(issue());
      return { text: issueSummary(read.issue, read.events), details: { issue: read.issue.key } };
    }
    default:
      throw new Error(`Unknown Dispatch tool: ${input.tool}`);
  }
}

function asObject(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}
