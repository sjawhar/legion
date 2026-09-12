import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type {
  AnswerAskInput,
  Ask,
  AskEdit,
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
import { actorLabel, describeAskResolutionActor } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { AskOptionList } from "./AskOptionList";
import { AskThread, type AskThreadQuery } from "./AskThread";
import { isQuestionShapedAnswer } from "./question-shaped-answer";

const answerAsk = (id: string, input: AnswerAskInput): Promise<Ask> => api.answerAsk(id, input);
const getAskThread = (id: string): Promise<AskRead> => api.getAsk(id);
const createReply = (issueKey: string, input: CreateCommentInput): Promise<Comment> =>
  api.createComment(issueKey, input);

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

function AskEditHistory({ ask, edits }: { ask: Ask; edits: AskEdit[] }): ReactNode {
  if (ask.edited_at === null) {
    return null;
  }
  return (
    <div className={`mt-2 text-xs ${textMutedOnSurface}`}>
      <p>
        Edited <Timestamp at={ask.edited_at} />
      </p>
      {edits.length === 0 ? null : (
        <details className="mt-1">
          <summary className={`cursor-pointer font-medium ${linkText} ${linkHoverText}`}>
            {edits.length === 1
              ? "Show 1 previous version"
              : `Show ${edits.length} previous versions`}
          </summary>
          <div className="mt-2 space-y-3">
            {edits.map((edit) => (
              <div key={edit.at}>
                <div className={`font-medium ${textPrimaryOnSurface}`}>
                  <MarkdownBody markdown={edit.previous.question} />
                </div>
                <AskOptionList options={edit.previous.options} selected={[]} />
                <p className="mt-1">
                  Reworded by {actorLabel(edit.edited_by)} · <Timestamp at={edit.at} />
                </p>
              </div>
            ))}
          </div>
        </details>
      )}
    </div>
  );
}

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
      {ask.issue_key === null ? null : (
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
      )}
    </p>
  );
}

function AnsweredAsk({
  artifactSlug,
  ask,
  edits,
}: {
  artifactSlug: string | undefined;
  ask: Ask;
  edits: AskEdit[];
}): ReactNode {
  const { answer } = ask;
  // A chosen "Other" answer carries no real option (answer.selected is empty) but still has
  // free text - render it under an Other label so the record reads as a chosen option, not an
  // unlabeled addendum. An ask with no options at all has no Other row to have chosen, so its
  // free text is always the plain answer instead.
  const otherText =
    ask.options.length > 0 && answer !== null && answer.selected.length === 0 ? answer.text : null;
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
      <div className={`font-medium ${textPrimaryOnSuccessCallout}`}>
        <MarkdownBody markdown={ask.question} />
      </div>
      <p className={`mt-1 text-xs ${calloutSuccessTimestampText}`}>{actorLabel(ask.author)}</p>
      <AskEditHistory ask={ask} edits={edits} />
      <AskOptionList options={ask.options} selected={answer?.selected ?? []} />
      {otherText !== null && otherText !== "" ? (
        <div className="mt-2">
          <p className={`text-sm font-medium ${textPrimaryOnSuccessCallout}`}>Other</p>
          <div className="mt-1">
            <MarkdownBody markdown={otherText} />
          </div>
        </div>
      ) : answer === null || answer.text === null || answer.text === "" ? null : (
        <div className="mt-1">
          <MarkdownBody markdown={answer.text} />
        </div>
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

function ResolvedAsk({
  ask,
  edits,
}: {
  ask: Ask & { resolution: AskResolution };
  edits: AskEdit[];
}): ReactNode {
  const { resolution } = ask;
  return (
    <article className={`rounded-xl p-4 text-sm ${card}`} data-testid={`ask-${ask.id}`}>
      {ask.anchor === null ? null : (
        <blockquote className={`mb-2 border-l-2 pl-3 ${quoteAccentBorder} ${quoteBodyText}`}>
          {ask.anchor.quote}
        </blockquote>
      )}
      <div className={`font-medium ${textPrimaryOnSurface}`}>
        <MarkdownBody markdown={ask.question} />
      </div>
      <span
        className={`mt-2 inline-block rounded-full px-2.5 py-1 text-xs font-medium ${badgeLow.bg} ${badgeLow.text}`}
        data-testid="ask-resolution-badge"
      >
        {resolution.kind === "retracted" ? "Retracted" : "Resolved"}
      </span>
      <p className={`mt-1 text-xs ${textMutedOnSurface}`}>{actorLabel(ask.author)}</p>
      <AskOptionList options={ask.options} selected={[]} />
      <AskEditHistory ask={ask} edits={edits} />
      <p className={`mt-2 text-xs ${textMutedOnSurface}`}>
        Asked <Timestamp at={ask.created_at} /> · {describeAskResolutionActor(resolution)} -{" "}
        <MarkdownBody markdown={resolution.reason} variant="inline" />
      </p>
    </article>
  );
}

function AskThreadDisclosure({
  ask,
  thread,
  createReply,
}: {
  ask: Ask;
  thread: AskThreadQuery;
  createReply?: (issueKey: string, input: CreateCommentInput) => Promise<Comment>;
}): ReactNode {
  const [open, setOpen] = useState(false);

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
        aria-controls={`thread-${ask.id}`}
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
        <div id={`thread-${ask.id}`}>
          <AskThread ask={ask} createReply={createReply} showResolution={false} thread={thread} />
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
  getAskThread: getThread = getAskThread,
}: AskCardProps): ReactNode {
  const queryClient = useQueryClient();
  // Each AskCard instance owns its answer field label so cards with the same
  // ask id never collide when a responsive transition briefly renders both.
  const answerFieldId = `${useId()}-answer`;
  const [selected, setSelected] = useState<string[]>([]);
  const [otherSelected, setOtherSelected] = useState(false);
  const [answerText, setAnswerText] = useState("");
  const [questionChoice, setQuestionChoice] = useState(false);
  const [justAnswered, setJustAnswered] = useState<Ask | null>(null);
  const submitGuard = useSubmitGuard();
  // Shared by this card, its edit-version history, its collapsed disclosure, and its inline
  // thread — one fetch instead of each consumer issuing its own.
  const threadQuery = useQuery<AskRead, Error>({
    queryKey: ["ask-thread", ask.id],
    queryFn: () => getThread(ask.id),
  });
  const edits = threadQuery.data?.edits ?? [];
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
      if (ask.issue_key === null) {
        if (ask.artifact_id === null || ask.artifact_id === undefined) {
          throw new Error("document ask is missing its artifact id");
        }
        void queryClient.invalidateQueries({ queryKey: ["artifact", ask.artifact_id] });
        void queryClient.invalidateQueries({ queryKey: ["projects"] });
        return;
      }
      void queryClient.invalidateQueries({ queryKey: ["asks", ask.issue_key] });
      void queryClient.invalidateQueries({ queryKey: ["issue", ask.issue_key] });
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  const clarification = useMutation({
    mutationFn: (text: string) => {
      if (ask.issue_key === null) {
        if (ask.artifact_id === null || ask.artifact_id === undefined) {
          throw new Error("document ask is missing its artifact id");
        }
        return api.createArtifactComment(ask.artifact_id, { ask_id: ask.id, body: text });
      }
      return (reply ?? createReply)(ask.issue_key, { ask_id: ask.id, body: text });
    },
    onSettled: () => {
      submitGuard.release();
    },
    onSuccess: () => {
      setAnswerText("");
      setQuestionChoice(false);
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
      void queryClient.invalidateQueries({ queryKey: ["inbox"] });
    },
  });

  const hasOptions = ask.options.length > 0;
  const isApproval = ask.kind === "approval";
  const isSubmitting = mutation.isPending || clarification.isPending;
  const sendAnswer = (text: string) => {
    submitGuard.guard(() => {
      if (isApproval) {
        mutation.mutate(selected.includes("Request changes") ? { selected, text } : { selected });
        return;
      }
      if (hasOptions) {
        mutation.mutate(otherSelected ? { selected, text } : { selected });
        return;
      }
      mutation.mutate(text === "" ? { selected } : { selected, text });
    });
  };

  const selectRealOption = (label: string) => {
    setQuestionChoice(false);
    if (ask.multiple) {
      setSelected((current) =>
        current.includes(label)
          ? current.filter((currentLabel) => currentLabel !== label)
          : [...current, label]
      );
      return;
    }
    setSelected([label]);
    setOtherSelected(false);
    setAnswerText("");
  };

  const toggleOther = () => {
    setQuestionChoice(false);
    if (ask.multiple) {
      setOtherSelected((current) => {
        const next = !current;
        if (!next) {
          setAnswerText("");
        }
        return next;
      });
      return;
    }
    setSelected([]);
    setOtherSelected(true);
  };

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const text = answerText.trim();
    if (!isApproval && hasOptions && otherSelected && isQuestionShapedAnswer(text)) {
      setQuestionChoice(true);
      return;
    }
    sendAnswer(text);
  };
  const sendClarification = () => {
    const text = answerText.trim();
    if (text === "") return;
    submitGuard.guard(() => clarification.mutate(text));
  };
  const canSubmit = isApproval
    ? selected.length > 0 && (!selected.includes("Request changes") || answerText.trim() !== "")
    : hasOptions
      ? selected.length > 0 || (otherSelected && answerText.trim() !== "")
      : answerText.trim() !== "";
  const showTextarea = isApproval
    ? selected.includes("Request changes")
    : !hasOptions || otherSelected;
  const tmuxTarget = ask.author.kind === "session" ? ask.author.origin?.tmux : undefined;

  const completed = justAnswered ?? (ask.state === "open" ? null : ask);
  // The thread's own "still open?" wording must track the post-answer ask, not the possibly
  // stale prop passed to this instance: `justAnswered` renders before an invalidated `ask` prop
  // round-trips down from the parent.
  const currentAsk = completed ?? ask;
  const threadNode =
    thread === "collapsed" ? (
      <AskThreadDisclosure ask={currentAsk} createReply={reply} thread={threadQuery} />
    ) : (
      <AskThread ask={currentAsk} createReply={reply} showResolution={false} thread={threadQuery} />
    );

  if (completed !== null) {
    if (completed.state === "resolved") {
      if (completed.resolution === undefined)
        throw new Error("resolved ask is missing its resolution");
      return (
        <>
          <ResolvedAsk ask={{ ...completed, resolution: completed.resolution }} edits={edits} />
          {threadNode}
        </>
      );
    }
    return (
      <>
        <AnsweredAsk
          artifactSlug={artifactSlug}
          ask={{ ...completed, answer: completed.answer }}
          edits={edits}
        />
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
            {ask.kind === "approval" ? (
              <p className={`text-xs font-semibold uppercase tracking-wide ${textMutedOnSurface}`}>
                Approval requested
              </p>
            ) : null}
            <div className={`font-medium ${textPrimaryOnSurface}`}>
              <MarkdownBody markdown={ask.question} />
            </div>
            <p className={`mt-1 text-sm ${textMutedOnSurface}`}>
              {actorLabel(ask.author)} · <Timestamp at={ask.created_at} />
            </p>
            <AskEditHistory ask={ask} edits={edits} />
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
                      disabled={isSubmitting}
                      name={`ask-${ask.id}`}
                      onChange={() => selectRealOption(option.label)}
                      type={ask.multiple ? "checkbox" : "radio"}
                    />
                    <span>
                      <span className={`font-medium ${textPrimaryOnSurface}`}>
                        <MarkdownBody markdown={option.label} variant="inline" />
                      </span>
                      {option.description === undefined ? null : (
                        <span className={`mt-0.5 block text-sm ${textMutedOnSurface}`}>
                          <MarkdownBody markdown={option.description} variant="inline" />
                        </span>
                      )}
                    </span>
                  </label>
                );
              })}
              {isApproval ? null : (
                <label
                  className={`flex cursor-pointer items-start gap-3 rounded-lg border px-3 py-2 ${borderDefault} ${cardHoverBorder}`}
                >
                  <input
                    checked={otherSelected}
                    disabled={isSubmitting}
                    name={`ask-${ask.id}`}
                    onChange={toggleOther}
                    type={ask.multiple ? "checkbox" : "radio"}
                  />
                  <span className={`font-medium ${textPrimaryOnSurface}`}>Other</span>
                </label>
              )}
            </fieldset>
          )}
          {showTextarea ? (
            <label
              className={`block text-sm font-medium ${textSecondaryOnSurface}`}
              htmlFor={answerFieldId}
            >
              {isApproval ? "Reason" : "Your answer"}
              <textarea
                className={`mt-1 block w-full rounded-lg px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
                disabled={isSubmitting}
                id={answerFieldId}
                onChange={(event) => {
                  setAnswerText(event.target.value);
                  setQuestionChoice(false);
                }}
                value={answerText}
              />
            </label>
          ) : null}
          {questionChoice ? (
            <fieldset aria-label="Question-shaped answer" className="space-y-2">
              <button
                className={`min-h-11 w-full rounded-lg border px-3 py-2 text-left text-sm font-semibold ${borderDefault} ${textPrimaryOnSurface} ${cardHoverBorder}`}
                disabled={isSubmitting}
                onClick={sendClarification}
                ref={(node) => node?.focus()}
                type="button"
              >
                This reads like a question — send as clarification (keeps the ask open)
              </button>
              <button
                className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`}
                disabled={isSubmitting}
                onClick={() => sendAnswer(answerText.trim())}
                type="button"
              >
                Answer with it anyway
              </button>
              {clarification.isError ? (
                <QueryError
                  message="Could not send your clarification."
                  onRetry={() => submitGuard.retryLast(clarification)}
                  retrying={clarification.isPending}
                />
              ) : null}
            </fieldset>
          ) : (
            <button
              className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={!canSubmit || isSubmitting}
              type="submit"
            >
              {mutation.isPending ? "Submitting…" : "Submit answer"}
            </button>
          )}
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
