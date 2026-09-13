import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { AnswerAskInput, Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  borderDefault,
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
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { actorLabel } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";
import { AskBlockLink } from "./AskBlockLink";
import { AskCompletionCard, AskEditHistory, OrphanedAnchorNotice } from "./AskCompletionCard";
import { AskThread } from "./AskThread";
import { AskThreadDisclosure } from "./AskThreadDisclosure";
import { formatAskAge } from "./ask-age";
import { useAskAnswerForm } from "./useAskAnswerForm";

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

export function AskCard({
  artifactSlug,
  ask,
  thread = "inline",
  answerAsk: answer = answerAsk,
  createReply: reply = createReply,
  getAskThread: getThread = getAskThread,
}: AskCardProps): ReactNode {
  const {
    answerFieldId,
    answerPlaceholder,
    answerText,
    askChanged,
    canAnswer,
    clarification,
    completed,
    displayedAsk,
    edits,
    isAction,
    isApproval,
    isSubmitting,
    mutation,
    questionChoice,
    selectRealOption,
    selected,
    sendAnswer,
    sendClarification,
    setAnswerText,
    setQuestionChoice,
    submit,
    submitFromKeyboard,
    submitGuard,
    threadQuery,
    trimmedAnswer,
  } = useAskAnswerForm({
    answer,
    ask,
    createReply: reply,
    getAskThread: getThread,
  });
  const tmuxTarget =
    displayedAsk.author.kind === "session" ? displayedAsk.author.origin?.tmux : undefined;
  // The thread's own "still open?" wording must track the post-answer ask, not the possibly
  // stale prop passed to this instance: `completed` renders before an invalidated `ask` prop
  // round-trips down from the parent.
  const currentAsk = completed ?? displayedAsk;
  const threadNode =
    thread === "collapsed" ? (
      <AskThreadDisclosure
        ask={currentAsk}
        createReply={reply}
        embedded={completed === null}
        thread={threadQuery}
      />
    ) : (
      <AskThread
        ask={currentAsk}
        createReply={reply}
        embedded={completed === null}
        showResolution={false}
        thread={threadQuery}
      />
    );

  if (completed !== null) {
    return (
      <>
        <AskCompletionCard artifactSlug={artifactSlug} ask={completed} edits={edits} />
        {threadNode}
      </>
    );
  }

  return (
    <article className={`rounded-xl p-4 shadow-sm ${card}`} data-testid={`ask-${displayedAsk.id}`}>
      {displayedAsk.anchor === null ? null : (
        <blockquote
          className={`mb-3 border-l-2 pl-3 text-sm ${quoteAccentBorder} ${quoteBodyText}`}
        >
          {displayedAsk.anchor.quote}
        </blockquote>
      )}
      <OrphanedAnchorNotice artifactSlug={artifactSlug} ask={displayedAsk} />
      {displayedAsk.block_id === undefined ||
      displayedAsk.block_id === null ||
      displayedAsk.block_artifact === undefined ? null : (
        <p className={`mt-2 text-sm ${linkText} ${linkHoverText}`}>
          <AskBlockLink ask={displayedAsk} />
        </p>
      )}
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          {isApproval ? (
            <p className={`text-xs font-semibold uppercase tracking-wide ${textMutedOnSurface}`}>
              Approval requested
            </p>
          ) : isAction ? (
            <span
              className={`inline-block rounded-full px-2 py-0.5 text-xs font-semibold ${badgeMed.bg} ${badgeMed.text}`}
            >
              Action
            </span>
          ) : null}
          <div className={`font-medium ${textPrimaryOnSurface}`}>
            <MarkdownBody markdown={displayedAsk.question} />
          </div>
          <p className={`mt-1 text-sm ${textMutedOnSurface}`}>
            {actorLabel(displayedAsk.author)} ·{" "}
            {isAction ? (
              <time dateTime={displayedAsk.created_at}>
                {formatAskAge(displayedAsk.created_at)}
              </time>
            ) : (
              <Timestamp at={displayedAsk.created_at} />
            )}
          </p>
          <AskEditHistory ask={displayedAsk} edits={edits} />
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
        </div>
        <span
          className={`rounded-full px-2.5 py-1 text-xs font-medium ${URGENCY_STYLES[displayedAsk.urgency].bg} ${URGENCY_STYLES[displayedAsk.urgency].text}`}
        >
          {URGENCY_LABELS[displayedAsk.urgency]}
        </span>
      </div>
      {askChanged ? (
        <p className={`mt-3 text-sm font-medium ${inlineWarningText}`}>
          This question changed while you were answering. Review the latest wording and confirm your
          answer again; your draft text is still here.
        </p>
      ) : null}
      {threadNode}
      <form className="mt-4 space-y-3" onSubmit={submit}>
        {displayedAsk.options.length === 0 ? null : (
          <fieldset className="space-y-2">
            <legend className="sr-only">Answer options</legend>
            {displayedAsk.options.map((option) => {
              const checked = selected.includes(option.label);
              return (
                <label
                  className={`flex cursor-pointer items-start gap-3 rounded-lg border px-3 py-2 ${borderDefault} ${cardHoverBorder}`}
                  key={option.label}
                >
                  <input
                    checked={checked}
                    disabled={isSubmitting}
                    name={`ask-${displayedAsk.id}`}
                    onChange={() => selectRealOption(option.label)}
                    type={displayedAsk.multiple ? "checkbox" : "radio"}
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
          </fieldset>
        )}
        <label className="block" htmlFor={answerFieldId}>
          <span className="sr-only">{isApproval ? "Reason" : "Your answer"}</span>
          <textarea
            className={`block w-full rounded-lg px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
            disabled={isSubmitting}
            id={answerFieldId}
            onChange={(event) => {
              setAnswerText(event.target.value);
              setQuestionChoice(false);
            }}
            onKeyDown={submitFromKeyboard}
            placeholder={answerPlaceholder}
            rows={2}
            value={answerText}
          />
        </label>
        {questionChoice ? (
          <fieldset aria-label="Question-shaped answer" className="flex gap-2">
            <button
              className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={isSubmitting}
              onClick={sendClarification}
              ref={(node) => node?.focus()}
              type="button"
            >
              {clarification.isPending ? "Sending…" : "Ask back instead"}
            </button>
            <button
              className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`}
              disabled={isSubmitting}
              onClick={() => sendAnswer(answerText)}
              type="button"
            >
              Answer with it anyway
            </button>
          </fieldset>
        ) : (
          <div className="flex gap-2">
            <button
              className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
              disabled={!canAnswer || isSubmitting}
              type="submit"
            >
              {mutation.isPending ? "Answering…" : "Answer"}
            </button>
            <button
              className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`}
              disabled={trimmedAnswer === "" || isSubmitting}
              onClick={sendClarification}
              type="button"
            >
              {clarification.isPending ? "Sending…" : "Ask back"}
            </button>
          </div>
        )}
        {mutation.isError ? (
          <QueryError
            message="Could not save your answer."
            onRetry={() => submitGuard.retryLast(mutation)}
            retrying={mutation.isPending}
          />
        ) : null}
        {clarification.isError ? (
          <QueryError
            message="Could not send your clarification."
            onRetry={() => submitGuard.retryLast(clarification)}
            retrying={clarification.isPending}
          />
        ) : null}
      </form>
    </article>
  );
}
