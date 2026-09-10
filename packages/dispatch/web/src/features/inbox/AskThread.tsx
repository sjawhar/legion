import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { api } from "../../api/client";
import type { Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
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
  textSecondaryOnSurfaceMuted,
} from "../../theme/classes";
import { actorLabel, describeAskResolution } from "../refs/actor";
import { Timestamp } from "../refs/Timestamp";

const getAskThread = (id: string): Promise<AskRead> => api.getAsk(id);
const createReply = (issueKey: string, input: CreateCommentInput): Promise<Comment> =>
  api.createComment(issueKey, input);

function replyAuthorLabel(comment: Comment): string {
  return actorLabel(comment.author);
}

function resolutionLine(ask: Ask): string | null {
  if (ask.state !== "resolved") return null;
  if (ask.resolution === undefined) throw new Error("resolved ask is missing its resolution");
  return describeAskResolution(ask.resolution);
}

interface UseAskThreadResult {
  replies: Comment[];
  body: string;
  setBody: (value: string) => void;
  submitReply: (event: FormEvent<HTMLFormElement>) => void;
  isPending: boolean;
  isError: boolean;
  retry: () => void;
}

/** Query + reply mutation for an ask's thread, kept separate from AskThread's markup so it can
 * be reasoned about (and, if ever needed, reused) independently of the JSX. */
function useAskThread(
  ask: Ask,
  createReply: (issueKey: string, input: CreateCommentInput) => Promise<Comment>,
  getAskThread: (id: string) => Promise<AskRead>
): UseAskThreadResult {
  const queryClient = useQueryClient();
  const [body, setBody] = useState("");
  // A distinct queryKey from ["asks", issueKey] (the issue-wide asks list
  // useAnsweredAsks queries): the two consumers expect incompatible shapes
  // (Ask[] vs. {ask, replies}), and react-query caches by key alone, so
  // sharing a key would silently corrupt whichever read second.
  const thread = useQuery({
    queryKey: ["ask-thread", ask.id],
    queryFn: () => getAskThread(ask.id),
  });
  const submit = useMutation({
    mutationFn: (text: string) => createReply(ask.issue_key, { ask_id: ask.id, body: text }),
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

  return {
    replies: thread.data?.replies ?? [],
    body,
    setBody,
    submitReply,
    isPending: submit.isPending,
    isError: submit.isError,
    retry: () => {
      if (submit.variables !== undefined) {
        submit.mutate(submit.variables);
      }
    },
  };
}
export interface AskThreadProps {
  ask: Ask;
  createReply?: (issueKey: string, input: CreateCommentInput) => Promise<Comment>;
  getAskThread?: (id: string) => Promise<AskRead>;
}

/**
 * The reply thread under a question: every comment that replies directly to
 * the ask (Comment.ask_id) plus their own reply chains, oldest first. Open
 * and answered asks keep an inline reply form; a resolved ask keeps its
 * history and recorded resolution without a composer.
 */
export function AskThread({
  ask,
  createReply: reply = createReply,
  getAskThread: getThread = getAskThread,
}: AskThreadProps): ReactNode {
  // The same ask can render simultaneously in more than one place (the issue
  // board and the margin both show open anchored asks) - useId keeps this
  // instance's reply field id/label pairing unique across those mounts,
  // instead of colliding on a shared `ask.id`-derived id.
  const replyFieldId = `${useId()}-reply`;
  const { replies, body, setBody, submitReply, isPending, isError, retry } = useAskThread(
    ask,
    reply,
    getThread
  );
  const resolution = resolutionLine(ask);

  return (
    <section
      aria-label="Replies"
      className={`mt-4 space-y-3 border-t pt-4 ${borderDefault}`}
      data-testid={`thread-${ask.id}`}
    >
      {resolution === null ? null : (
        <p
          className={`rounded-lg px-3 py-2 text-sm ${surfaceMutedBg} ${textSecondaryOnSurfaceMuted}`}
        >
          {resolution}
        </p>
      )}
      {replies.length === 0 ? null : (
        <ul className="space-y-2">
          {replies.map((comment) => (
            <li className={`rounded-lg p-2 text-sm ${surfaceMutedBg}`} key={comment.id}>
              <p className={`whitespace-pre-wrap ${textPrimaryOnSurfaceMuted}`}>{comment.body}</p>
              <p className={`mt-1 text-xs ${textMutedOnSurfaceMuted}`}>
                {replyAuthorLabel(comment)} · <Timestamp at={comment.created_at} />
              </p>
            </li>
          ))}
        </ul>
      )}
      {resolution === null ? (
        <form className="flex flex-col gap-2" onSubmit={submitReply}>
          <label
            className={`block text-sm font-medium ${textSecondaryOnCanvas}`}
            htmlFor={replyFieldId}
          >
            Reply
            <textarea
              className={`mt-1 block w-full rounded-lg px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
              disabled={isPending}
              id={replyFieldId}
              onChange={(event) => setBody(event.target.value)}
              value={body}
            />
          </label>
          <button
            className={`self-start rounded-lg border px-3 py-1.5 text-sm font-medium ${borderStrong} ${textSecondaryOnSurface} ${enabledCardHoverBorder} disabled:cursor-not-allowed disabled:opacity-50`}
            disabled={body.trim() === "" || isPending}
            type="submit"
          >
            {isPending ? "Replying…" : "Reply"}
          </button>
          {isError ? (
            <QueryError message="Could not post your reply." onRetry={retry} retrying={isPending} />
          ) : null}
        </form>
      ) : null}
    </section>
  );
}
