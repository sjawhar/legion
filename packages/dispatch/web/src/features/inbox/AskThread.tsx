import type { UseQueryResult } from "@tanstack/react-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useLayoutEffect, useRef, useState } from "react";

import { api } from "../../api/client";
import type { Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
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
import { actorLabel } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";

type CreateReply = (issueKey: string, input: CreateCommentInput) => Promise<Comment>;

const createReply: CreateReply = (issueKey, input) => api.createComment(issueKey, input);

/** Unsent reply drafts, by ask id. A draft belongs to the ask, not to the card showing it: the
 *  margin remounts an answered ask's card when its ask list moves it out of Needs you, often while
 *  the reader is still typing. Sending the reply, or emptying the field, drops the draft. */
const replyDrafts = new Map<string, string>();

/** The caret of a focused reply field whose card unmounted in the current commit, by ask id. A
 *  composer for the same ask that mounts in that commit takes the focus and the caret back. */
const replyFocusHandoffs = new Map<string, { end: number; start: number }>();

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
 * project-document ask, then invalidates the shared thread so the reply shows first. Its draft and
 * focus are keyed by the ask, so they survive the card remounting elsewhere.
 */
export function AskReplyComposer({
  ask,
  createReply: reply = createReply,
}: {
  ask: Ask;
  createReply?: CreateReply;
}): ReactNode {
  const queryClient = useQueryClient();
  // Each composer owns its field id so transient duplicate mounts during a responsive
  // transition cannot share an ask-id-derived id.
  const fieldId = `${useId()}-reply`;
  const fieldRef = useRef<HTMLTextAreaElement>(null);
  const [body, setBodyState] = useState(() => replyDrafts.get(ask.id) ?? "");
  const setBody = (text: string) => {
    setBodyState(text);
    if (text === "") replyDrafts.delete(ask.id);
    else replyDrafts.set(ask.id, text);
  };
  useLayoutEffect(() => {
    const field = fieldRef.current;
    if (field === null) return;
    const caret = replyFocusHandoffs.get(ask.id);
    if (caret !== undefined) {
      replyFocusHandoffs.delete(ask.id);
      field.focus();
      field.setSelectionRange(caret.start, caret.end);
    }
    return () => {
      // React runs this before it detaches the field, so a field being typed in is still focused.
      if (document.activeElement !== field) return;
      const handoff = { end: field.selectionEnd, start: field.selectionStart };
      replyFocusHandoffs.set(ask.id, handoff);
      queueMicrotask(() => {
        if (replyFocusHandoffs.get(ask.id) === handoff) replyFocusHandoffs.delete(ask.id);
      });
    };
  }, [ask.id]);
  const submit = useMutation({
    mutationFn: (text: string) => {
      if (ask.issue_key === null) {
        if (ask.artifact_id === null || ask.artifact_id === undefined) {
          throw new Error("document ask is missing its artifact id");
        }
        return api.createArtifactComment(ask.artifact_id, { ask_id: ask.id, body: text });
      }
      return reply(ask.issue_key, { ask_id: ask.id, body: text });
    },
    onSuccess: () => {
      setBody("");
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
    },
  });

  const submitReply = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const text = body.trim();
    if (text === "" || submit.isPending) {
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
          disabled={submit.isPending}
          id={fieldId}
          onChange={(event) => setBody(event.target.value)}
          onKeyDown={(event) => submitOnModifiedEnter(event)}
          ref={fieldRef}
          value={body}
        />
      </label>
      <button
        className={`self-start rounded-lg border px-3 py-1.5 text-sm font-medium ${borderStrong} ${textSecondaryOnSurface} ${enabledCardHoverBorder} disabled:cursor-not-allowed disabled:opacity-50`}
        disabled={body.trim() === "" || submit.isPending}
        type="submit"
      >
        {submit.isPending ? "Replying…" : "Reply"}
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
  /** Compact cards render the answered-ask composer themselves, after their Reply control. */
  showComposer?: boolean;
  /** A thread inside an open ask card inherits the card's compact flow. */
  embedded?: boolean;
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
        <AskReplyComposer ask={ask} createReply={reply} />
      ) : null}
    </section>
  );
}
