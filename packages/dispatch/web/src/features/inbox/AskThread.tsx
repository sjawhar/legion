import type { UseQueryResult } from "@tanstack/react-query";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { type ArtifactOwner, api, type WriteBounds } from "../../api/client";
import type { Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import { useSending } from "../../hooks/useSending";
import {
  borderDefault,
  borderStrong,
  enabledCardHoverBorder,
  inputClasses,
  surfaceMutedBg,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurfaceMuted,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { appendToDraft, DraftUploadStatus, useDraftUpload } from "../artifacts/ArtifactUpload";
import { AgentName } from "../refs/AgentName";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { Unfurl } from "../refs/Unfurl";
import type { DwellRef } from "../shell/useDwell";
import { EarlierReplies } from "../threads/EarlierReplies";
import { threadLayout } from "../threads/thread-layout";
import type { ThreadOrder } from "../threads/thread-order";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { clearReplyDraft, useReplyDraft } from "./reply-drafts";

export type CreateReply = (
  issueKey: string,
  input: CreateCommentInput,
  bounds: WriteBounds
) => Promise<Comment>;

const createReply: CreateReply = (issueKey, input, bounds) =>
  api.createComment(issueKey, input, bounds);

function AskReply({
  comment,
  current = false,
  markRef,
}: {
  comment: Comment;
  current?: boolean;
  markRef?: DwellRef;
}): ReactNode {
  return (
    <li
      aria-current={current ? "true" : undefined}
      className={`rounded-lg p-2 text-sm ${surfaceMutedBg}`}
      data-reply=""
      ref={markRef}
    >
      <div className={textPrimaryOnSurfaceMuted}>
        <MarkdownBody markdown={comment.body} source={textSourceOf("comment", comment.id)} />
      </div>
      <Unfurl body={comment.body} source={textSourceOf("comment", comment.id)} />
      <p className={`mt-1 text-xs ${textMutedOnSurfaceMuted}`}>
        <AgentName actor={comment.author} inline /> · <Timestamp at={comment.created_at} />
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
  // The form shows a refused reply itself, with Retry, under the draft it kept.
  const submit = useKeyedWrite({
    write: (text: string, bounds) => {
      if (ask.issue_key === null) {
        if (ask.artifact_id === null || ask.artifact_id === undefined) {
          throw new Error("document ask is missing its artifact id");
        }
        return api.createArtifactComment(ask.artifact_id, { ask_id: ask.id, body: text }, bounds);
      }
      return reply(ask.issue_key, { ask_id: ask.id, body: text }, bounds);
    },
    inline: true,
    label: () => `Reply on “${ask.question}”`,
    mutationKey,
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
  /** The shared thread read mark for the ask (`AskCard`, `useMarkThreadRead`), placed on the
   *  newest reply's own element when that reply is the newest write the card shows. */
  markNewest?: DwellRef;
  /** The reply a deep link named, which only the Conversation passes: that reply carries
   *  `aria-current`, and the fold over the older replies opens when it is one of them. */
  currentReplyId?: string;
  /** The order the thread reads in: the reader's own choice, which the card holds
   *  (`threads/thread-order.ts`). */
  order: ThreadOrder;
  /** The ids of the replies the reader has not read (`unreadWrites` in
   *  `threads/thread-layout.ts`): an unread reply is never folded away. */
  unread?: ReadonlySet<string>;
}

/**
 * The reply thread under a question: every comment that replies directly to the ask
 * (Comment.ask_id) plus their own reply chains, in the reader's thread order, with the replies
 * outside the newest two folded behind **Show N earlier replies** (`threads/thread-layout.ts`,
 * drawn by `threads/EarlierReplies.tsx`, the fold control the Inbox's thread body shares).
 * Answered asks keep a plain reply composer, at the end of that order; open asks reserve their
 * single composer for answering or asking back, and a resolved ask keeps its history without a
 * composer.
 */
export function AskThread({
  ask,
  thread,
  createReply: reply = createReply,
  showComposer = true,
  embedded = false,
  uploadOwner,
  markNewest,
  currentReplyId,
  order,
  unread,
}: AskThreadProps): ReactNode {
  const replies = thread.data?.replies ?? [];
  const [showAllReplies, setShowAllReplies] = useState<boolean>();
  const { composerFirst, folded, shown } = threadLayout({
    collapse: true,
    order,
    replies,
    unread,
  });
  const newestReply = replies.at(-1);
  // A link to a reply the fold holds seeds the control open, so the reply it names is on screen
  // rather than behind a control the reader must find. Seeding, not overriding: once the reader
  // uses the control, their choice wins.
  const showAll = showAllReplies ?? folded.some((comment) => comment.id === currentReplyId);

  const replyList =
    shown.length === 0 ? null : (
      <ul className="space-y-2">
        {shown.map((comment) => (
          <AskReply
            comment={comment}
            current={comment.id === currentReplyId}
            key={comment.id}
            markRef={comment === newestReply ? markNewest : undefined}
          />
        ))}
      </ul>
    );
  // The fold and what it holds are one block, drawn furthest from the newest message: under the
  // question in Oldest first, at the foot of the thread in Newest first.
  const fold = (
    <EarlierReplies
      count={folded.length}
      onToggle={() => setShowAllReplies(!showAll)}
      open={showAll}
      replies={(id) => (
        <ul className="space-y-2" id={id}>
          {folded.map((comment) => (
            <AskReply comment={comment} current={comment.id === currentReplyId} key={comment.id} />
          ))}
        </ul>
      )}
    />
  );
  const composer =
    showComposer && ask.state === "answered" ? (
      <AskReplyComposer ask={ask} createReply={reply} uploadOwner={uploadOwner} />
    ) : null;

  return (
    <section
      aria-label="Replies"
      className={embedded ? "space-y-3" : `mt-4 space-y-3 border-t pt-4 ${borderDefault}`}
      data-testid={`thread-${ask.id}`}
    >
      {/* Three slots, so a change of order moves the ends and leaves the replies where they
          are: the box the reader writes in sits next to the newest message, the fold over the
          earlier replies furthest from it. */}
      {composerFirst ? composer : fold}
      {replyList}
      {composerFirst ? fold : composer}
    </section>
  );
}
