import type { UseQueryResult } from "@tanstack/react-query";
import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useState } from "react";

import { type ArtifactOwner, api, type WriteBounds } from "../../api/client";
import type { Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { useSending } from "../../hooks/useSending";
import {
  borderDefault,
  linkHoverText,
  linkText,
  surfaceMutedBg,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurfaceMuted,
} from "../../theme/classes";
import { AgentName } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import { MarkdownBody } from "../refs/MarkdownBody";
import { itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import type { DwellRef } from "../shell/useDwell";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { AskAnswerArea, draftTargets, EMPTY_DRAFT_REFUSAL } from "./AskAnswerArea";
import { clearReplyDraft, useReplyDraft } from "./reply-drafts";

export type CreateReply = (
  issueKey: string,
  input: CreateCommentInput,
  bounds: WriteBounds
) => Promise<Comment>;

const createReply: CreateReply = (issueKey, input, bounds) =>
  api.createComment(issueKey, input, bounds);

/** What one Reply puts on the wire: the words, and the targets picked in them, frozen when the
 *  press starts so a keystroke after it cannot change what is being sent. */
interface ReplyInput {
  readonly body: string;
  readonly mentions: readonly string[];
}

function AskReply({
  comment,
  current = false,
  markRef,
  owner,
}: {
  comment: Comment;
  current?: boolean;
  markRef?: DwellRef;
  /** The project document that owns a reply with no issue, so the reply can name its own
   *  `dispatch://` reference (`itemRoute`). */
  owner?: { project: string; slug: string };
}): ReactNode {
  const reference = itemRoute("comment", comment, owner);
  return (
    <li
      aria-current={current ? "true" : undefined}
      className={`rounded-lg p-2 text-sm ${surfaceMutedBg}`}
      data-reply=""
      ref={markRef}
    >
      <div className={textPrimaryOnSurfaceMuted}>
        <MarkdownBody markdown={comment.body} />
      </div>
      <p
        className={`mt-1 flex flex-wrap items-center gap-x-1.5 text-xs ${textMutedOnSurfaceMuted}`}
      >
        <AgentName actor={comment.author} inline />
        <span aria-hidden="true">·</span>
        <Timestamp at={comment.created_at} />
        {/* A reply is a node like any other, so it carries the one copy control for its own
            reference (`dispatch://<KEY>/comment/<id>`), as a margin thread's comments do. */}
        {reference === undefined ? null : <CopyRefButton route={reference} />}
      </p>
    </li>
  );
}

/** AskCard owns the `["ask-thread", ask.id]` query, initializing it from an Inbox row when
 *  available; every card renders the shared thread without a duplicate read. */
export type AskThreadQuery = UseQueryResult<AskRead, Error>;

/**
 * The reply box under an answered ask, which is the same answer area as the open card's
 * (`AskAnswerArea.tsx`): the Conversation tab's field and `@` picker, with Reply as the one
 * button it sends by. It posts to the ask's issue, or to its document for a project-document
 * ask, then invalidates the shared thread so the reply shows first. A picked session rides with
 * the comment as a `session:` mention and gets it. Its draft is the ask's (`reply-drafts.ts`),
 * and its send is the ask's too: while any composer's reply to this ask is out, every composer
 * for it, including one mounted since, shows the draft disabled. A file pasted or dropped into
 * the field uploads to `uploadOwner` and its text joins the draft.
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
  const [draft, setDraft] = useReplyDraft(ask.id);
  // Keyed by the ask, so the send outlives the composer that started it and any composer for the
  // ask sees it in flight.
  const mutationKey = ["ask-reply", ask.id];
  const { sending } = useSending(mutationKey);
  // The form shows a refused reply itself, with Retry, under the draft it kept.
  const submit = useKeyedWrite({
    write: ({ body, mentions }: ReplyInput, bounds) => {
      const input: CreateCommentInput = {
        ask_id: ask.id,
        body,
        ...(mentions.length === 0 ? {} : { mentions: mentions.map((target) => ({ target })) }),
      };
      if (ask.issue_key === null) {
        if (ask.artifact_id === null || ask.artifact_id === undefined) {
          throw new Error("document ask is missing its artifact id");
        }
        return api.createArtifactComment(ask.artifact_id, input, bounds);
      }
      return reply(ask.issue_key, input, bounds);
    },
    inline: true,
    label: () => `Reply on “${ask.question}”`,
    mutationKey,
    onSuccess: (_comment, input) => {
      clearReplyDraft(ask.id, input.body);
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
    },
  });
  /** Why Reply refuses the draft as it stands: it posts what the box holds. */
  const refusal = draft.text.trim() === "" ? EMPTY_DRAFT_REFUSAL : null;

  // The field's own Ctrl/Cmd+Enter stands down while a picture is still uploading
  // (`AskAnswerArea`), and Reply says the upload is what holds it, so neither way in sends a
  // reply the picture would miss.
  const sendReply = () => {
    if (refusal !== null || sending) return;
    submit.mutate({
      body: draft.text.trim(),
      mentions: draftTargets(draft).map((mention) => mention.target),
    });
  };

  return (
    <form
      className="flex flex-col gap-2"
      // Ctrl/Cmd+Enter submits the form without passing through the button that would have
      // swallowed a refused press, so the form owes the same refusal (`RefusableButton`).
      onSubmit={(event) => {
        event.preventDefault();
        sendReply();
      }}
    >
      <AskAnswerArea
        actions={({ reasonId, uploading }) => (
          <RefusableButton
            busy={sending ? "Replying…" : uploading ? "Reply" : undefined}
            className="self-start"
            look="secondary-compact"
            refusal={refusal}
            refusalShownBy={reasonId.reply}
            type="submit"
          >
            Reply
          </RefusableButton>
        )}
        disabled={sending}
        draft={draft}
        label="Reply"
        onDraftChange={setDraft}
        refusals={{ reply: refusal }}
        uploadOwner={uploadOwner}
      />
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
  /** The Replies page's mark for the ask (`AskCard`, `useMarkThreadRead`), placed on the newest
   *  reply's own element when that reply is the newest write the card shows. */
  markNewest?: DwellRef;
  /** The reply a deep link named, which only the Conversation passes: that reply carries
   *  `aria-current`, and the fold over the older replies opens when it is one of them. */
  currentReplyId?: string;
  /** The project document that owns an ask with no issue, so each reply can name its own
   *  `dispatch://` reference (`AskReply`, `itemRoute`). */
  owner?: { project: string; slug: string };
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
  markNewest,
  currentReplyId,
  owner,
}: AskThreadProps): ReactNode {
  const olderRepliesId = useId();
  const replies = thread.data?.replies ?? [];
  const [showAllReplies, setShowAllReplies] = useState<boolean>();
  const hiddenReplyCount = Math.max(replies.length - 2, 0);
  const recentReplies = replies.slice(-2).reverse();
  const newestReply = replies.at(-1);
  const older = replies.slice(0, -2);
  // A link to a reply older than the two the card keeps visible seeds the control open, so the
  // reply it names is on screen rather than behind a control the reader must find. Seeding, not
  // overriding: once the reader uses the control, their choice wins.
  const showAll = showAllReplies ?? older.some((comment) => comment.id === currentReplyId);
  const olderReplies = showAll ? [...older].reverse() : [];

  return (
    <section
      aria-label="Replies"
      className={embedded ? "space-y-3" : `mt-4 space-y-3 border-t pt-4 ${borderDefault}`}
      data-testid={`thread-${ask.id}`}
    >
      {recentReplies.length === 0 ? null : (
        <ul className="space-y-2">
          {recentReplies.map((comment) => (
            <AskReply
              comment={comment}
              current={comment.id === currentReplyId}
              key={comment.id}
              markRef={comment === newestReply ? markNewest : undefined}
              owner={owner}
            />
          ))}
        </ul>
      )}
      {hiddenReplyCount === 0 ? null : (
        <button
          aria-controls={olderRepliesId}
          aria-expanded={showAll}
          className={`min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
          onClick={() => setShowAllReplies(!showAll)}
          type="button"
        >
          {showAll
            ? "Show fewer replies"
            : `Show ${hiddenReplyCount} more ${hiddenReplyCount === 1 ? "reply" : "replies"}`}
        </button>
      )}
      {olderReplies.length === 0 ? null : (
        <ul className="space-y-2" id={olderRepliesId}>
          {olderReplies.map((comment) => (
            <AskReply
              comment={comment}
              current={comment.id === currentReplyId}
              key={comment.id}
              owner={owner}
            />
          ))}
        </ul>
      )}
      {showComposer && ask.state === "answered" ? (
        <AskReplyComposer ask={ask} createReply={reply} uploadOwner={uploadOwner} />
      ) : null}
    </section>
  );
}
