import type { UseQueryResult } from "@tanstack/react-query";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { api } from "../../api/client";
import type { Ask, AskRead, AskResolution, Comment, CreateCommentInput } from "../../api/types";
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
import { actorLabel, describeAskResolutionActor } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";

const createReply = (issueKey: string, input: CreateCommentInput): Promise<Comment> =>
  api.createComment(issueKey, input);

function resolvedInfo(ask: Ask): AskResolution | null {
  if (ask.state !== "resolved") return null;
  if (ask.resolution === undefined) throw new Error("resolved ask is missing its resolution");
  return ask.resolution;
}

/** The `["ask-thread", ask.id]` query AskCard fetches once and shares with its collapsed
 *  disclosure and this inline thread, so no consumer here issues its own duplicate fetch. */
export type AskThreadQuery = UseQueryResult<AskRead, Error>;

interface UseAskThreadResult {
  replies: Comment[];
  body: string;
  setBody: (value: string) => void;
  submitReply: (event: FormEvent<HTMLFormElement>) => void;
  isPending: boolean;
  isError: boolean;
  retry: () => void;
}

/** Reply mutation for an ask's thread, kept separate from AskThread's markup so it can be
 * reasoned about (and, if ever needed, reused) independently of the JSX. Reads replies from
 * the shared `thread` query AskCard owns rather than fetching its own copy. */
function useAskThread(
  ask: Ask,
  createReply: (issueKey: string, input: CreateCommentInput) => Promise<Comment>,
  thread: AskThreadQuery
): UseAskThreadResult {
  const queryClient = useQueryClient();
  const [body, setBody] = useState("");
  const submit = useMutation({
    mutationFn: (text: string) => {
      if (ask.issue_key === null) {
        throw new Error("cannot reply to an ask without an issue");
      }
      return createReply(ask.issue_key, { ask_id: ask.id, body: text });
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
  /** The shared ask-thread query AskCard already fetched; this component never fetches its
   *  own copy. */
  thread: AskThreadQuery;
  createReply?: (issueKey: string, input: CreateCommentInput) => Promise<Comment>;
  showResolution?: boolean;
}

/**
 * The reply thread under a question: every comment that replies directly to
 * the ask (Comment.ask_id) plus their own reply chains, oldest first. An open ask's composer
 * is framed as asking for clarification (replying never answers the question); an answered
 * ask keeps the plain reply composer. A resolved ask keeps its history and recorded
 * resolution without a composer.
 */
export function AskThread({
  ask,
  thread,
  createReply: reply = createReply,
  showResolution = true,
}: AskThreadProps): ReactNode {
  // Each AskThread instance owns its reply field label so transient duplicate
  // mounts during a responsive transition cannot share an ask-id-derived id.
  const replyFieldId = `${useId()}-reply`;
  const { replies, body, setBody, submitReply, isPending, isError, retry } = useAskThread(
    ask,
    reply,
    thread
  );
  const resolution = resolvedInfo(ask);
  const isOpen = ask.state === "open";

  return (
    <section
      aria-label="Replies"
      className={`mt-4 space-y-3 border-t pt-4 ${borderDefault}`}
      data-testid={`thread-${ask.id}`}
    >
      {showResolution && resolution !== null ? (
        <p
          className={`rounded-lg px-3 py-2 text-sm ${surfaceMutedBg} ${textSecondaryOnSurfaceMuted}`}
        >
          {describeAskResolutionActor(resolution)} -{" "}
          <MarkdownBody markdown={resolution.reason} variant="inline" />
        </p>
      ) : null}
      {replies.length === 0 ? null : (
        <ul className="space-y-2">
          {replies.map((comment) => (
            <li className={`rounded-lg p-2 text-sm ${surfaceMutedBg}`} key={comment.id}>
              <div className={textPrimaryOnSurfaceMuted}>
                <MarkdownBody markdown={comment.body} />
              </div>
              <p className={`mt-1 text-xs ${textMutedOnSurfaceMuted}`}>
                {actorLabel(comment.author)} · <Timestamp at={comment.created_at} />
              </p>
            </li>
          ))}
        </ul>
      )}
      {resolution === null && ask.issue_key !== null ? (
        <form className="flex flex-col gap-2" onSubmit={submitReply}>
          {isOpen ? (
            <p className={`text-xs ${textMutedOnSurfaceMuted}`}>
              Replying does not answer the question.
            </p>
          ) : null}
          <label
            className={`block text-sm font-medium ${textSecondaryOnCanvas}`}
            htmlFor={replyFieldId}
          >
            {isOpen ? "Ask for clarification" : "Reply"}
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
            {isOpen ? (isPending ? "Sending…" : "Send") : isPending ? "Replying…" : "Reply"}
          </button>
          {isError ? (
            <QueryError message="Could not post your reply." onRetry={retry} retrying={isPending} />
          ) : null}
        </form>
      ) : null}
    </section>
  );
}
