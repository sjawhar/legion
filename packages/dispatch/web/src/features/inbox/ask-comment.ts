import { api, type WriteBounds } from "../../api/client";
import type { Ask, Comment, CreateCommentInput } from "../../api/types";
import { type Draft, draftTargets } from "./draft-store";

/**
 * The comment an ask sends, and where it goes. One place, because the open card's Ask back
 * (`useAskAnswerForm.ts`) and the answered ask's Reply (`AskThread.tsx`) are the same write: the
 * same wire shape, the same fallback to the ask's document, and the same contract error where
 * that document has no artifact. Written twice, a change to either would have to land twice.
 */

/** A document ask's artifact; the server always sets it, so its absence is a contract error. */
export function askArtifactId(ask: Pick<Ask, "artifact_id">): string {
  if (ask.artifact_id === null || ask.artifact_id === undefined) {
    throw new Error("document ask is missing its artifact id");
  }
  return ask.artifact_id;
}

/**
 * What one Reply or Ask back puts on the wire: the words the box holds, and the sessions picked
 * in them, which ride with the comment so each one gets it. Both are read when the press starts,
 * so a keystroke after it cannot change what is being sent.
 */
export function askCommentInput(askId: string, draft: Draft): CreateCommentInput {
  const mentions = draftTargets(draft).map((mention) => ({ target: mention.target }));
  return {
    ask_id: askId,
    body: draft.text.trim(),
    ...(mentions.length === 0 ? {} : { mentions }),
  };
}

/** Posts `input` where `ask` lives: its issue, through `createReply`, or the document it was
 *  asked on. */
export function postAskComment(
  ask: Pick<Ask, "artifact_id" | "issue_key">,
  input: CreateCommentInput,
  bounds: WriteBounds,
  createReply: (
    issueKey: string,
    input: CreateCommentInput,
    bounds: WriteBounds
  ) => Promise<Comment>
): Promise<Comment> {
  if (ask.issue_key === null) {
    return api.createArtifactComment(askArtifactId(ask), input, bounds);
  }
  return createReply(ask.issue_key, input, bounds);
}
