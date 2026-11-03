import type {
  Ask,
  AskRead,
  AskUrgency,
  CreateAskInput,
  EditAskInput,
  OpenAsk,
} from "@legion/contracts";
import { ASK_QUESTION_MAX, ASK_URGENCIES } from "@legion/contracts";
import { textHead } from "../ask-answer";
import { type DispatchClient, DispatchServiceError } from "../dispatch-http";
import { issueTopic, type Owner, parseDispatchRef } from "../dispatch-owner";
import { relatedCheckLines } from "../related-check";
import { ToolInputError } from "../tool-input-errors";
import { otherAdviceLines, renderAdvice } from "./advice";
import { askQuestionWithRef } from "./check";
import {
  asObject,
  type CommandContext,
  type DispatchToolResult,
  optionalBoolean,
  optionalString,
  type ResolvedArtifact,
  type SessionContext,
  stringArg,
  type ToolArguments,
} from "./context";
import { anchor, resolveArtifact } from "./document-targets";
import { askId, idPrefixPattern, normalizeUUID, refuseItemOffRef, resolveIdPrefix } from "./refs";
import { formatOpenAsksSummary } from "./render-item";
import { askOwnerDetails, documentTopic, followedAskDetails, followsAsk } from "./results";

function askUrgency(args: ToolArguments): AskUrgency | undefined {
  const value = args.urgency;
  return ASK_URGENCIES.find((urgency) => urgency === value);
}
const askHintLimit = 8;

function askIDInputProblem(asks: readonly Pick<Ask, "id" | "question">[], scope: string): string {
  const hints = asks
    .slice(0, askHintLimit)
    .map((ask) => `${ask.id.slice(0, 8)}… ${textHead(ask.question)}`);
  return `ask IDs are UUIDs; use the full ask ID; ${scope} open asks: ${hints.length === 0 ? "none" : hints.join(", ")}`;
}

/** The line a reply on an approval request a newer version moved prints: no reply hands such a
 *  request back (the server's approvalMoved), only `request-approval` at the current version does.
 *  Undefined when the ask is anything else or its read fails, so the reply reports as before. */
export async function movedApprovalLine(
  client: DispatchClient,
  askID: string
): Promise<string | undefined> {
  const ask = (await client.getAsk(askID).catch(() => undefined))?.ask;
  const approval = ask?.approval;
  if (ask?.kind !== "approval" || approval === undefined) return undefined;
  if (approval.requested_version >= approval.version) return undefined;
  const owner =
    ask.issue_key === null
      ? `--project ${ask.approval_artifact?.project ?? "<project>"} --artifact ${approval.artifact_id}`
      : ask.approval_artifact?.primary === true
        ? `--issue ${ask.issue_key}`
        : `--issue ${ask.issue_key} --artifact ${approval.artifact_id}`;
  return (
    `The reply did not hand the ask to the human: it is the approval request for ${approval.name} ` +
    `at version ${approval.requested_version}, and ${approval.name} is now version ${approval.version}. ` +
    "No reply hands a moved approval request back. Once the human has agreed to every point in " +
    `version ${approval.version}, run: dispatch request-approval ${owner} --summary ` +
    `'<what changed since version ${approval.requested_version}>'`
  );
}

export async function invalidReplyToAskProblem(
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
const askIdShapeProblem = "ask ids are uuids (a prefix of at least 8 hex characters works)";

/**
 * The ask `dispatch read` of `dispatch://…/ask/<id>` names, read. An ask in a document has two
 * ids: its own uuid and the id of its `:::ask{#<id>}` block, which can look like a uuid too.
 * Either names it, whole or as an 8+ hex prefix exactly one of `list()`, the owner's asks, carries.
 * A full uuid is read as an ask id first; the list is read only when no ask has that id.
 */
export async function readAskRef(
  tool: string,
  id: string,
  ownerName: string,
  client: DispatchClient,
  list: () => Promise<readonly Pick<Ask, "id" | "block_id">[]>
): Promise<AskRead> {
  const fullID = normalizeUUID(id);
  if (fullID !== undefined) {
    try {
      return await client.getAsk(fullID);
    } catch (error) {
      if (!(error instanceof DispatchServiceError) || error.status !== 404) throw error;
      const blocks = (await list()).filter((ask) => ask.block_id?.toLowerCase() === fullID);
      if (blocks.length !== 1 || blocks[0] === undefined) throw error;
      return client.getAsk(blocks[0].id);
    }
  }
  const asks = await list();
  let matches = asks.filter((ask) => ask.block_id === id);
  if (matches.length === 0) {
    if (!idPrefixPattern.test(id)) {
      throw new ToolInputError(tool, [
        `ask id ${id} must be a full uuid, a prefix of at least 8 hex characters, or the id of an :::ask block on ${ownerName}`,
      ]);
    }
    const prefix = id.toLowerCase();
    matches = asks.filter(
      (ask) =>
        ask.id.toLowerCase().startsWith(prefix) ||
        (ask.block_id?.toLowerCase().startsWith(prefix) ?? false)
    );
  }
  if (matches.length === 1 && matches[0] !== undefined) return client.getAsk(matches[0].id);
  throw new ToolInputError(tool, [
    matches.length === 0
      ? `ask id ${id} matches none of the asks on ${ownerName}; use the full id`
      : `ask id ${id} matches ${matches.length} asks on ${ownerName}; use the full id`,
  ]);
}

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
 * A full uuid passes through; an 8+ hex prefix resolves against this session's own open asks;
 * anything else is refused before the write, naming those asks so the next call carries a real
 * id. An open-asks read that fails never masks that rule. An ask named by a dispatch:// reference
 * is read first and refused unless it is on the issue or document the reference names.
 */
async function resolveAskArgument(
  tool: string,
  args: ToolArguments,
  client: DispatchClient,
  sessionId: string | undefined
): Promise<{ readonly id: string; readonly read?: AskRead }> {
  const id = askId(args);
  if (normalizeUUID(id) === undefined && !idPrefixPattern.test(id)) {
    const asks = await sessionOpenAsks(client, sessionId).catch(() => undefined);
    throw new ToolInputError(tool, [
      asks === undefined ? askIdShapeProblem : `${askIdShapeProblem}; ${openAskHints(asks)}`,
    ]);
  }
  const resolved = await resolveIdPrefix(tool, "ask", id, "this session", () =>
    sessionOpenAsks(client, sessionId)
  );
  const reference = stringArg(args, "ask");
  // `argumentProblems` refused a dispatch:// value that is not an ask reference.
  const ref = reference.startsWith("dispatch://") ? parseDispatchRef(reference) : null;
  if (ref === null) return { id: resolved };
  const read = await client.getAsk(resolved);
  await refuseItemOffRef(tool, "ask", ref, reference, read.ask, async () => {
    const document = await resolveArtifact(client, ref.owner, ref.artifact, { canonical: true });
    return document.artifact.id;
  });
  return { id: resolved, read };
}

/** `dispatch open-asks`: the open asks this session, or one project, is waiting on. */
export async function openAsksCommand(context: SessionContext): Promise<DispatchToolResult> {
  const { input, linkBase, ownerArguments, newClient: dispatchClient } = context;
  const client = dispatchClient();
  const project = optionalString(ownerArguments.args, "project");
  if (project !== undefined) {
    const response = await client.openAsksForProject(project);
    return { text: formatOpenAsksSummary(response, linkBase), details: { ...response } };
  }
  const sessionId = input.sessionId?.trim();
  if (!sessionId) throw new Error("host session id is required for dispatch open-asks");
  const response = await client.openAsks(sessionId);
  return { text: formatOpenAsksSummary(response, linkBase), details: { ...response } };
}

/** `dispatch ask`: open an ask on the issue, or on a passage of a document. */
export async function askCommand(context: CommandContext): Promise<DispatchToolResult> {
  const {
    args,
    client,
    actor,
    input,
    linkBase,
    documentOwner,
    resolveDocument,
    writePictures,
    issue,
  } = context;
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
  const withRef = askQuestionWithRef(args);
  const refAppended = withRef !== stringArg(args, "question");
  const question = await writePictures(resolved, withRef, {
    cap: ASK_QUESTION_MAX,
    text: refAppended ? "question plus ref" : "question",
    shorten: refAppended ? "shorten the question, drop the ref" : "shorten the question",
  });
  const askInput = {
    question,
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
  // The check's form sits right under the line naming the ask; renderAdvice's cadence line
  // must stay the result's last line.
  const checkLines = relatedCheckLines(ask.advice?.suggestions, {
    project: resolved?.artifact.project ?? (ask.issue_key ?? "").replace(/-[0-9]+$/, ""),
    text: stringArg(args, "question"),
    configUrl: linkBase,
  });
  const adviceLines = await otherAdviceLines(() =>
    renderAdvice(input.tool, askOwner.label, ask.advice, {})
  );
  return {
    text: [
      `Asked ${ask.id} on ${askOwner.label} (urgency ${ask.urgency}): ${ask.question}`,
      ...checkLines,
      await followsAsk(askOwner, input.subscriptions),
      ...adviceLines,
    ].join("\n"),
    details: {
      ...(await followedAskDetails(client, ask, resolved?.artifact)),
      ...(ask.advice === undefined ? {} : { advice: ask.advice }),
    },
  };
}

/** `dispatch edit-ask`: reword an open ask, or change its options, urgency or multiple. */
export async function editAskCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input } = context;
  const question = optionalString(args, "question");
  const options = args.options;
  const multiple = optionalBoolean(args, "multiple");
  const urgency = askUrgency(args);
  const { id } = await resolveAskArgument(input.tool, args, client, input.sessionId);
  const ask = await client.editAsk(id, {
    ...(question === undefined ? {} : { question }),
    ...(Array.isArray(options) ? { options: options as NonNullable<EditAskInput["options"]> } : {}),
    ...(multiple === undefined ? {} : { multiple }),
    ...(urgency === undefined ? {} : { urgency }),
    actor,
  });
  return {
    text: `Ask edited: ${ask.question}`,
    details: await askOwnerDetails(client, ask),
  };
}

/** `dispatch resolve-ask`: close an ask nobody needs to answer, with the reason. */
export async function resolveAskCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input } = context;
  const kind = stringArg(args, "kind") as "retracted" | "resolved";
  const { id } = await resolveAskArgument(input.tool, args, client, input.sessionId);
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

/** `dispatch follow`: have an ask's answer and replies reach this session, or stop them. */
export async function followCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input } = context;
  const sessionId = input.sessionId?.trim();
  if (!sessionId) throw new Error("host session id is required for dispatch follow");
  const { id: ask, read: refRead } = await resolveAskArgument(input.tool, args, client, sessionId);
  const action = stringArg(args, "action");
  if (action === "unfollow") {
    await client.unfollowAsk(ask, sessionId, actor);
    return { text: `Unfollowed ask ${ask}.`, details: { ask } };
  }
  const read = refRead ?? (await client.getAsk(ask));
  await client.followAsk(ask, sessionId, actor);
  return {
    text: `Following ask ${ask}: its answer and replies reach this session directly.`,
    details: await followedAskDetails(client, read.ask),
  };
}
