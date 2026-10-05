import type { UseQueryResult } from "@tanstack/react-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { type ArtifactOwner, api } from "../../api/client";
import type { Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useSending } from "../../hooks/useSending";
import {
  borderDefault,
  borderStrong,
  enabledCardHoverBorder,
  inputClasses,
  linkHoverText,
  linkText,
  surfaceMutedBg,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { appendToDraft, DraftUploadStatus, useDraftUpload } from "../artifacts/ArtifactUpload";
import { actorLabel } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";
import { clearReplyDraft, useReplyDraft } from "./reply-drafts";

export type CreateReply = (issueKey: string, input: CreateCommentInput) => Promise<Comment>;

const createReply: CreateReply = (issueKey, input) => api.createComment(issueKey, input);

function AskReply({ comment }: { comment: Comment }): ReactNode {
  return (
    <li className={`rounded-lg p-2 text-sm ${surfaceMutedBg}`}>
      <div className={textPrimaryOnSurfaceMuted}>
        <MarkdownBody markdown={comment.body} />
      </div>
      <p className={`mt-1 text-xs ${textMutedOnSurfaceMuted}`}>
        {actorLabel(comment.author)} · <Timestamp at={comment.created_at} />
      </p>
    </li>
  );
}

/** AskCard owns the `["ask-thread", ask.id]` query, initializing it from an Inbox row when
 *  available; every card renders the shared thread without a duplicate read. */
export type AskThreadQuery = UseQueryResult<AskRead, Error>;

/**
 * The reply form under an answered ask. It posts to the ask's issue, or to its document for a
 * project-document ask, then invalidates the shared thread so the reply shows first. Its draft is
 * the ask's (`reply-drafts.ts`), and its send is the ask's too: while any composer's reply to this
 * ask is out, every composer for it, including one mounted since, shows the draft disabled. A file
 * pasted or dropped into the field uploads to `uploadOwner` and its text joins the draft.
 */
export function AskReplyComposer({
  ask,
  createReply: reply = createReply,
  uploadOwner,
}: {
  ask: Ask;
  createReply?: CreateReply;
  /** Where a file pasted or dropped into the reply goes: the ask's issue, or its document's
   *  project. Without one the field takes a paste as text. */
  uploadOwner?: ArtifactOwner;
}): ReactNode {
  const queryClient = useQueryClient();
  // Each composer owns its field id so transient duplicate mounts during a responsive
  // transition cannot share an ask-id-derived id.
  const fieldId = `${useId()}-reply`;
  const [body, setBody] = useReplyDraft(ask.id);
  // Keyed by the ask, so the send outlives the composer that started it and any composer for the
  // ask sees it in flight.
  const mutationKey = ["ask-reply", ask.id];
  const { sending } = useSending(mutationKey);
  const upload = useDraftUpload((text) => setBody((current) => appendToDraft(current, text)));
  const uploading = upload.pending > 0;
  const submit = useMutation({
    mutationKey,
    mutationFn: (text: string) => {
      if (ask.issue_key === null) {
        if (ask.artifact_id === null || ask.artifact_id === undefined) {
          throw new Error("document ask is missing its artifact id");
        }
        return api.createArtifactComment(ask.artifact_id, { ask_id: ask.id, body: text });
      }
      return reply(ask.issue_key, { ask_id: ask.id, body: text });
    },
    onSuccess: (_comment, text) => {
      clearReplyDraft(ask.id, text);
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
    },
  });

  const submitReply = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const text = body.trim();
    // A reply sent while a picture is still uploading would go without it.
    if (text === "" || sending || uploading) {
      return;
    }
    submit.mutate(text);
  };

  return (
    <form className="flex flex-col gap-2" onSubmit={submitReply}>
      <label className={`block text-sm font-medium ${textSecondaryOnCanvas}`} htmlFor={fieldId}>
        Reply
        <textarea
          className={`mt-1 block w-full rounded-lg px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
          disabled={sending}
          id={fieldId}
          onChange={(event) => setBody(event.target.value)}
          onDrop={
            uploadOwner === undefined ? undefined : (event) => upload.drop(event, uploadOwner)
          }
          onKeyDown={(event) => submitOnModifiedEnter(event, { disabled: uploading })}
          onPaste={
            uploadOwner === undefined ? undefined : (event) => upload.paste(event, uploadOwner)
          }
          value={body}
        />
      </label>
      <DraftUploadStatus upload={upload} />
      <button
        className={`self-start rounded-lg border px-3 py-1.5 text-sm font-medium ${borderStrong} ${textSecondaryOnSurface} ${enabledCardHoverBorder} disabled:cursor-not-allowed disabled:opacity-50`}
        disabled={body.trim() === "" || sending || uploading}
        type="submit"
      >
        {sending ? "Replying…" : "Reply"}
      </button>
      {submit.isError ? (
        <QueryError
          message="Could not post your reply."
          onRetry={() => {
            if (submit.variables !== undefined) {
              submit.mutate(submit.variables);
            }
          }}
          retrying={submit.isPending}
        />
      ) : null}
    </form>
  );
}

export interface AskThreadProps {
  ask: Ask;
  /** The shared ask-thread query AskCard owns; this component never requests thread data itself. */
  thread: AskThreadQuery;
  createReply?: CreateReply;
  /** False where the caller renders the answered-ask composer itself: a collapsed thread
   *  (Conversation, decision blocks) puts it after its own Write a reply control. */
  showComposer?: boolean;
  /** A thread inside an open ask card inherits the card's compact flow. */
  embedded?: boolean;
  /** Where a file pasted or dropped into the reply goes (`AskReplyComposer`). */
  uploadOwner?: ArtifactOwner;
}

/**
 * The reply thread under a question: every comment that replies directly to
 * the ask (Comment.ask_id) plus their own reply chains, newest first. The newest two replies stay
 * visible; a card-local control reveals the remaining older replies. Answered asks keep a plain
 * reply composer; open asks reserve their single composer for answering or asking back, and a
 * resolved ask keeps its history without a composer.
 */
export function AskThread({
  ask,
  thread,
  createReply: reply = createReply,
  showComposer = true,
  embedded = false,
  uploadOwner,
}: AskThreadProps): ReactNode {
  const olderRepliesId = useId();
  const replies = thread.data?.replies ?? [];
  const [showAllReplies, setShowAllReplies] = useState(false);
  const hiddenReplyCount = Math.max(replies.length - 2, 0);
  const recentReplies = replies.slice(-2).reverse();
  const olderReplies = showAllReplies ? replies.slice(0, -2).reverse() : [];

  return (
    <section
      aria-label="Replies"
      className={embedded ? "space-y-3" : `mt-4 space-y-3 border-t pt-4 ${borderDefault}`}
      data-testid={`thread-${ask.id}`}
    >
      {recentReplies.length === 0 ? null : (
        <ul className="space-y-2">
          {recentReplies.map((comment) => (
            <AskReply comment={comment} key={comment.id} />
          ))}
        </ul>
      )}
      {hiddenReplyCount === 0 ? null : (
        <button
          aria-controls={olderRepliesId}
          aria-expanded={showAllReplies}
          className={`min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
          onClick={() => setShowAllReplies((showing) => !showing)}
          type="button"
        >
          {showAllReplies
            ? "Show fewer replies"
            : `Show ${hiddenReplyCount} more ${hiddenReplyCount === 1 ? "reply" : "replies"}`}
        </button>
      )}
      {olderReplies.length === 0 ? null : (
        <ul className="space-y-2" id={olderRepliesId}>
          {olderReplies.map((comment) => (
            <AskReply comment={comment} key={comment.id} />
          ))}
        </ul>
      )}
      {showComposer && ask.state === "answered" ? (
        <AskReplyComposer ask={ask} createReply={reply} uploadOwner={uploadOwner} />
      ) : null}
    </section>
  );
}
