import type { Artifact, CreateCommentInput } from "@legion/contracts";
import { DISPATCH_BODY_MAX } from "@legion/contracts";
import { commandLine } from "../dispatch-command";
import {
  dispatchChildRef,
  dispatchIssueRef,
  issueTopic,
  parseDispatchRef,
} from "../dispatch-owner";
import type { PictureTextLimit } from "../dispatch-picture-tools";
import { ToolInputError } from "../tool-input-errors";
import { renderAdvice } from "./advice";
import { invalidReplyToAskProblem, movedApprovalLine } from "./asks";
import { type CommandContext, type DispatchToolResult, optionalString, stringArg } from "./context";
import { anchor, resolveArtifact } from "./document-targets";
import {
  itemReplyTo,
  messageInReplyTo,
  normalizeUUID,
  refOwnerName,
  refuseItemOffRef,
  resolveIdPrefix,
} from "./refs";
import {
  commentOwnerResult,
  followsAsk,
  notSubscribed,
  resolvedTopic,
  writeResultDetails,
} from "./results";

/** `dispatch resolve-comment`: close a comment thread on an issue or a document. */
export async function resolveCommentCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input } = context;
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
    const artifact = (await resolveArtifact(client, ref.owner, ref.artifact, { canonical: true }))
      .artifact;
    document = artifact;
    id = await resolveIdPrefix(input.tool, "comment", ref.id, refOwnerName(ref), () =>
      client.getArtifactComments(artifact.id)
    );
  }
  // A full id is resolved as it is, so the comment is read first and held to the owner the
  // reference names before it is resolved.
  if (ref !== null) {
    const read = await client.getComment(id);
    await refuseItemOffRef(input.tool, "comment", ref, reference, read.comment, async () =>
      document === undefined ? "" : document.id
    );
  }
  const comment = await client.resolveComment(id, actor);
  const { label, details } = await commentOwnerResult(client, comment, document);
  return { text: `Resolved comment ${comment.id} on ${label}.`, details };
}

/** `dispatch comment`: post a comment, a reply in a thread, or a reply under an ask. */
export async function commentCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, documentOwner, resolveDocument, writePictures, issue } =
    context;
  const artifactReference = optionalString(args, "artifact");
  const owner = documentOwner();
  const resolved =
    owner.kind === "project" || artifactReference === undefined
      ? owner.kind === "project"
        ? await resolveDocument(owner, artifactReference)
        : undefined
      : await resolveDocument(owner, artifactReference);
  const anchored = resolved ? anchor(resolved.artifact, args) : undefined;
  const replyTo = itemReplyTo(args, "reply_to");
  const replyToAskTarget = itemReplyTo(args, "reply_to_ask");
  const replyToAsk =
    replyToAskTarget === undefined ? undefined : normalizeUUID(replyToAskTarget.id);
  if (replyToAskTarget !== undefined && replyToAsk === undefined) {
    throw new ToolInputError(input.tool, [await invalidReplyToAskProblem(client, owner, resolved)]);
  }
  // reply_to and reply_to_ask are mutually exclusive, so at most one names a block: a block
  // of the comment reply_to answers, or the part of the ask reply_to_ask threads under.
  const replyToBlock = replyTo?.block ?? replyToAskTarget?.block;
  // The schema already refused reply_to alongside reply_to_ask, turn without reply_to_ask,
  // and a turn outside agent|human, so the value is the contract's shape.
  const requestedTurn = optionalString(args, "turn") as CreateCommentInput["turn"];
  const body = await writePictures(resolved, stringArg(args, "body"), {
    cap: DISPATCH_BODY_MAX,
    text: "body",
    shorten: "shorten the body",
  });
  const commentInput: CreateCommentInput = {
    body,
    ...(anchored === undefined ? {} : { anchor: anchored }),
    ...(replyTo === undefined ? {} : { reply_to: replyTo.id }),
    ...(replyToBlock === undefined ? {} : { reply_to_block: replyToBlock }),
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
      comment.ask_waiting_on === undefined ? "" : `; ask now waiting on ${comment.ask_waiting_on}`;
    // A reply meant for the human that leaves the ask with the agent landed on an approval
    // request a newer version moved: say why, and name the call that hands it back.
    const moved =
      requestedTurn !== "agent" && comment.ask_waiting_on === "agent"
        ? await movedApprovalLine(client, replyToAsk)
        : undefined;
    return {
      text: [
        `Replied on ask ${replyToAsk} (comment ${comment.id}${askState}). ${await followsAsk(commentOwner, input.subscriptions)}`,
        ...(moved === undefined ? [] : [moved]),
        ...adviceLines,
      ].join("\n"),
      details: {
        ...commentDetails,
        ask: replyToAsk,
        follows: { ask: replyToAsk },
        ...(comment.ask_waiting_on === undefined ? {} : { ask_waiting_on: comment.ask_waiting_on }),
        ...(comment.advice === undefined ? {} : { advice: comment.advice }),
      },
    };
  }
  return {
    text: [
      `Posted comment ${comment.id}${await notSubscribed(commentOwner, input.subscriptions)}`,
      ...adviceLines,
    ].join("\n"),
    details: {
      ...commentDetails,
      ...(comment.advice === undefined ? {} : { advice: comment.advice }),
    },
  };
}

/** `dispatch suggest`: propose a replacement for a passage of a document. */
export async function suggestCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, documentOwner, resolveDocument, issue } = context;
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
    text: `Posted suggestion ${comment.id}${await notSubscribed(resolvedTopic(resolved), input.subscriptions)}`,
    details: writeResultDetails(resolved, { comment: comment.id }),
  };
}

/** `dispatch message`: post on the issue's conversation, or answer a direct message. */
export async function messageCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, owner, pictures, postWithPictures, issue } = context;
  const target = messageInReplyTo(args);
  const inReplyTo = target?.id;
  const written = stringArg(args, "body");
  const bodyLimit: PictureTextLimit = {
    cap: DISPATCH_BODY_MAX,
    text: "body",
    shorten: "shorten the body",
  };
  if (owner === null && inReplyTo !== undefined) {
    // A direct message's conversation belongs to no issue, so its pictures belong to this
    // session's own conversation, where the person reads the reply.
    let body = written;
    if (pictures.length > 0) {
      const sessionId = input.sessionId?.trim();
      if (!sessionId) {
        throw new Error("host session id is required to send pictures in a direct-message reply");
      }
      body = await postWithPictures(written, bodyLimit, `agent/${sessionId}`, (upload) =>
        client.agentArtifact(sessionId, upload)
      );
    }
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
    const readBack = `${commandLine("dispatch_read", { message: inReplyTo })} reads the conversation back.`;
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
  const body = await postWithPictures(written, bodyLimit, issueKey, (upload) =>
    client.artifact(issueKey, upload)
  );
  const message = await client.message(issueKey, {
    body,
    ...(inReplyTo === undefined ? {} : { in_reply_to: inReplyTo }),
    ...(target?.block === undefined ? {} : { in_reply_to_block: target.block }),
    actor,
  });
  const messageRef = dispatchChildRef(dispatchIssueRef(issueKey), "message", message.id);
  const adviceLines = renderAdvice(input.tool, issueKey, message.advice, {});
  return {
    text: [
      `Posted message ${message.id} (${messageRef})${await notSubscribed(issueTopic(issueKey), input.subscriptions)}`,
      ...adviceLines,
    ].join("\n"),
    details: {
      issue: issueKey,
      message: message.id,
      ...(message.advice === undefined ? {} : { advice: message.advice }),
    },
  };
}
