import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type {
  AnswerAskInput,
  Ask,
  AskRead,
  AskResolution,
  Comment,
  CreateCommentInput,
} from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  borderDefault,
  calloutSuccessBg,
  calloutSuccessBodyText,
  calloutSuccessBorder,
  calloutSuccessText,
  calloutSuccessTimestampText,
  card,
  cardHoverBorder,
  inlineWarningText,
  inputClasses,
  linkHoverText,
  linkText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  quoteAccentBorder,
  quoteBodyText,
  successQuoteAccentBorder,
  textMutedOnSurface,
  textPrimaryOnSuccessCallout,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { actorLabel, describeAskResolution } from "../refs/actor";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { AskOptionList } from "./AskOptionList";
import { AskThread, type AskThreadProps } from "./AskThread";

const answerAsk = (id: string, input: AnswerAskInput): Promise<Ask> => api.answerAsk(id, input);

export interface AskCardProps {
  artifactSlug?: string;
  ask: Ask;
  thread?: "inline" | "collapsed";
  answerAsk?: (id: string, input: AnswerAskInput) => Promise<Ask>;
  /** Reply-thread fetch/write seams for tests; default to the real API. */
  getAskThread?: (id: string) => Promise<AskRead>;
  createReply?: (issueKey: string, input: CreateCommentInput) => Promise<Comment>;
}

const URGENCY_STYLES: Record<Ask["urgency"], { bg: string; text: string }> = {
  blocking: badgeBlocking,
  high: badgeHigh,
  low: badgeLow,
  med: badgeMed,
};

const URGENCY_LABELS: Record<Ask["urgency"], string> = {
  blocking: "Blocking",
  high: "High",
  low: "Low",
  med: "Medium",
};

function OrphanedAnchorNotice({
  artifactSlug,
  ask,
}: {
  artifactSlug: string | undefined;
  ask: Ask;
}): ReactNode {
  const anchor = ask.anchor;
  if (anchor === null || !anchor.orphaned || artifactSlug === undefined) {
    return null;
  }
  return (
    <p className={`mb-2 text-xs font-medium ${inlineWarningText}`}>
      Text changed.{" "}
      <Link
        className="underline"
        to={`${buildIssuePath({
          key: ask.issue_key,
          kind: "artifact",
          slug: artifactSlug,
          version: anchor.version,
        })}&ask=${ask.id}`}
      >
        View original text
      </Link>
    </p>
  );
}

function AnsweredAsk({
  artifactSlug,
  ask,
}: {
  artifactSlug: string | undefined;
  ask: Ask;
}): ReactNode {
  const { answer } = ask;
  return (
    <article
      className={`rounded-xl p-4 text-sm ${calloutSuccessBorder} ${calloutSuccessBg} ${calloutSuccessText}`}
      data-testid={`ask-${ask.id}`}
    >
      {ask.anchor === null ? null : (
        <blockquote
          className={`mb-2 border-l-2 pl-3 ${successQuoteAccentBorder} ${calloutSuccessBodyText}`}
        >
          {ask.anchor.quote}
        </blockquote>
      )}
      <OrphanedAnchorNotice artifactSlug={artifactSlug} ask={ask} />
      <p className={`font-medium ${textPrimaryOnSuccessCallout}`}>{ask.question}</p>
      <p className={`mt-1 text-xs ${calloutSuccessTimestampText}`}>{actorLabel(ask.author)}</p>
      <AskOptionList options={ask.options} selected={answer?.selected ?? []} />
      {answer === null || answer.text === null || answer.text === "" ? null : (
        <p className="mt-1 whitespace-pre-wrap">{answer.text}</p>
      )}
      {answer === null ? (
        <p className={`mt-2 text-xs ${calloutSuccessTimestampText}`}>
          Asked <Timestamp at={ask.created_at} />
        </p>
      ) : (
        <p className={`mt-2 text-xs ${calloutSuccessTimestampText}`}>
          Asked <Timestamp at={ask.created_at} /> · Answered by{" "}
          <span className="font-semibold">{answer.user}</span> <Timestamp at={answer.at} />
        </p>
      )}
    </article>
  );
}

function ResolvedAsk({ ask }: { ask: Ask & { resolution: AskResolution } }): ReactNode {
  const { resolution } = ask;
  return (
    <article className={`rounded-xl p-4 text-sm ${card}`} data-testid={`ask-${ask.id}`}>
      {ask.anchor === null ? null : (
        <blockquote className={`mb-2 border-l-2 pl-3 ${quoteAccentBorder} ${quoteBodyText}`}>
          {ask.anchor.quote}
        </blockquote>
      )}
      <p className={`font-medium ${textPrimaryOnSurface}`}>{ask.question}</p>
      <span
        className={`mt-2 inline-block rounded-full px-2.5 py-1 text-xs font-medium ${badgeLow.bg} ${badgeLow.text}`}
        data-testid="ask-resolution-badge"
      >
        {resolution.kind === "retracted" ? "Retracted" : "Resolved"}
      </span>
      <p className={`mt-1 text-xs ${textMutedOnSurface}`}>{actorLabel(ask.author)}</p>
      <AskOptionList options={ask.options} selected={[]} />
      <p className={`mt-2 text-xs ${textMutedOnSurface}`}>
        Asked <Timestamp at={ask.created_at} /> · {describeAskResolution(resolution)}
      </p>
    </article>
  );
}

function AskThreadDisclosure(props: AskThreadProps): ReactNode {
  const [open, setOpen] = useState(false);
  const thread = useQuery({
    queryKey: ["ask-thread", props.ask.id],
    queryFn: () =>
      props.getAskThread === undefined
        ? api.getAsk(props.ask.id)
        : props.getAskThread(props.ask.id),
  });

  if (thread.isError) {
    const message =
      thread.error instanceof Error ? thread.error.message : "Unable to load replies.";
    return (
      <button
        aria-expanded="false"
        className={`mt-3 min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
        onClick={() => {
          void thread.refetch();
        }}
        title={message}
        type="button"
      >
        Replies unavailable — retry
      </button>
    );
  }

  const count = thread.data?.replies.length ?? 0;
  return (
    <>
      <button
        aria-controls={`thread-${props.ask.id}`}
        aria-expanded={open}
        className={`mt-3 min-h-11 text-sm font-medium ${linkText} ${linkHoverText}`}
        onClick={() => {
          setOpen((current) => !current);
        }}
        type="button"
      >
        {count === 0 ? "Reply" : count === 1 ? "1 reply" : `${count} replies`}
      </button>
      {open ? (
        <div id={`thread-${props.ask.id}`}>
          <AskThread {...props} showResolution={false} />
        </div>
      ) : null}
    </>
  );
}

export function AskCard({
  artifactSlug,
  ask,
  thread = "inline",
  answerAsk: answer = answerAsk,
  createReply: reply,
  getAskThread: getThread,
}: AskCardProps): ReactNode {
  const queryClient = useQueryClient();
  // Each AskCard instance owns its answer field label so cards with the same
  // ask id never collide when a responsive transition briefly renders both.
  const answerFieldId = `${useId()}-answer`;
  const [selected, setSelected] = useState<string[]>([]);
  const [answerText, setAnswerText] = useState("");
  const [justAnswered, setJustAnswered] = useState<Ask | null>(null);
  const submitGuard = useSubmitGuard();
  const mutation = useMutation({
    mutationFn: (input: AnswerAskInput) => answer(ask.id, input),
    onMutate: async () => {
      await queryClient.cancelQueries({ queryKey: ["inbox"] });
      const previous = queryClient.getQueryData<Ask[]>(["inbox"]);
      queryClient.setQueryData<Ask[]>(["inbox"], (current) =>
        current?.filter((currentAsk) => currentAsk.id !== ask.id)
      );
      return previous;
    },
    onError: (_error, _input, previous) => {
      queryClient.setQueryData(["inbox"], previous);
      void queryClient.invalidateQueries({ queryKey: ["inbox"] });
    },
    onSettled: () => {
      submitGuard.release();
    },
    onSuccess: (updatedAsk) => {
      setJustAnswered(updatedAsk);
      void queryClient.invalidateQueries({ queryKey: ["inbox"] });
      void queryClient.invalidateQueries({ queryKey: ["asks", ask.issue_key] });
      void queryClient.invalidateQueries({ queryKey: ["issue", ask.issue_key] });
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const text = answerText.trim();
    submitGuard.guard(() => mutation.mutate(text === "" ? { selected } : { selected, text }));
  };
  const canSubmit = selected.length > 0 || answerText.trim() !== "";
  const tmuxTarget = ask.author.kind === "session" ? ask.author.origin?.tmux : undefined;

  const completed = justAnswered ?? (ask.state === "open" ? null : ask);
  const threadNode =
    thread === "collapsed" ? (
      <AskThreadDisclosure ask={ask} createReply={reply} getAskThread={getThread} />
    ) : (
      <AskThread ask={ask} createReply={reply} getAskThread={getThread} showResolution={false} />
    );

  if (completed !== null) {
    if (completed.state === "resolved") {
      if (completed.resolution === undefined)
        throw new Error("resolved ask is missing its resolution");
      return (
        <>
          <ResolvedAsk ask={{ ...completed, resolution: completed.resolution }} />
          {threadNode}
        </>
      );
    }
    return (
      <>
        <AnsweredAsk artifactSlug={artifactSlug} ask={{ ...completed, answer: completed.answer }} />
        {threadNode}
      </>
    );
  }

  return (
    <>
      <article className={`rounded-xl p-4 shadow-sm ${card}`} data-testid={`ask-${ask.id}`}>
        {ask.anchor === null ? null : (
          <blockquote
            className={`mb-3 border-l-2 pl-3 text-sm ${quoteAccentBorder} ${quoteBodyText}`}
          >
            {ask.anchor.quote}
          </blockquote>
        )}
        <OrphanedAnchorNotice artifactSlug={artifactSlug} ask={ask} />
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <p className={`font-medium ${textPrimaryOnSurface}`}>{ask.question}</p>
            <p className={`mt-1 text-sm ${textMutedOnSurface}`}>
              {actorLabel(ask.author)} · <Timestamp at={ask.created_at} />
            </p>
          </div>
          <span
            className={`rounded-full px-2.5 py-1 text-xs font-medium ${URGENCY_STYLES[ask.urgency].bg} ${URGENCY_STYLES[ask.urgency].text}`}
          >
            {URGENCY_LABELS[ask.urgency]}
          </span>
        </div>
        {tmuxTarget === undefined ? null : (
          <button
            className={`mt-3 text-sm font-medium ${linkText} ${linkHoverText}`}
            onClick={() => {
              void navigator.clipboard?.writeText(tmuxTarget);
            }}
            type="button"
          >
            Copy tmux target
          </button>
        )}
        <form className="mt-4 space-y-3" onSubmit={submit}>
          {ask.options.length === 0 ? null : (
            <fieldset className="space-y-2">
              <legend className="sr-only">Answer options</legend>
              {ask.options.map((option) => {
                const checked = selected.includes(option.label);
                return (
                  <label
                    className={`flex cursor-pointer items-start gap-3 rounded-lg border px-3 py-2 ${borderDefault} ${cardHoverBorder}`}
                    key={option.label}
                  >
                    <input
                      checked={checked}
                      disabled={mutation.isPending}
                      name={`ask-${ask.id}`}
                      onChange={() => {
                        setSelected((current) => {
                          if (!ask.multiple) {
                            return [option.label];
                          }
                          return current.includes(option.label)
                            ? current.filter((label) => label !== option.label)
                            : [...current, option.label];
                        });
                      }}
                      type={ask.multiple ? "checkbox" : "radio"}
                    />
                    <span>
                      <span className={`font-medium ${textPrimaryOnSurface}`}>{option.label}</span>
                      {option.description === undefined ? null : (
                        <span className={`mt-0.5 block text-sm ${textMutedOnSurface}`}>
                          {option.description}
                        </span>
                      )}
                    </span>
                  </label>
                );
              })}
            </fieldset>
          )}
          <label
            className={`block text-sm font-medium ${textSecondaryOnSurface}`}
            htmlFor={answerFieldId}
          >
            Your answer
            <textarea
              className={`mt-1 block w-full rounded-lg px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
              disabled={mutation.isPending}
              id={answerFieldId}
              onChange={(event) => setAnswerText(event.target.value)}
              value={answerText}
            />
          </label>
          <button
            className={`rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
            disabled={!canSubmit || mutation.isPending}
            type="submit"
          >
            {mutation.isPending ? "Submitting…" : "Submit answer"}
          </button>
          {mutation.isError ? (
            <QueryError
              message="Could not save your answer."
              onRetry={() => submitGuard.retryLast(mutation)}
              retrying={mutation.isPending}
            />
          ) : null}
        </form>
      </article>
      {threadNode}
    </>
  );
}
