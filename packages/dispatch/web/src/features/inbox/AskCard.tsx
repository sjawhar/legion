import { type ReactNode, useEffect, useRef, useState } from "react";

import { api } from "../../api/client";
import type { AnswerAskInput, Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import { Pill } from "../../components/Pill";
import { QueryError } from "../../components/QueryError";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import {
  askUrgencyAccent,
  badgeBlocking,
  badgeHigh,
  badgeLow,
  badgeMed,
  borderDefault,
  borderTransparent,
  card,
  cardHoverBorder,
  groupFocusVisibleBorder,
  groupHoverCardBorder,
  inlineWarningText,
  inputClasses,
  linkHoverText,
  linkText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  quoteAccentBorder,
  quoteBodyText,
  surfaceBg,
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
import { askTurnLabel } from "./ask-turn";
import { useAskAnswerForm } from "./useAskAnswerForm";

const answerAsk = (id: string, input: AnswerAskInput): Promise<Ask> => api.answerAsk(id, input);
const getAskThread = (id: string): Promise<AskRead> => api.getAsk(id);
const createReply = (issueKey: string, input: CreateCommentInput): Promise<Comment> =>
  api.createComment(issueKey, input);

export interface AskCardProps {
  artifactSlug?: string;
  ask: Ask;
  thread?: "inline" | "collapsed";
  /** `compact` keeps the answer controls appropriate for a margin or review sheet. */
  variant?: "compact" | "full";
  answerAsk?: (id: string, input: AnswerAskInput) => Promise<Ask>;
  /** Reply-thread fetch/write seams for tests; default to the real API. */
  getAskThread?: (id: string) => Promise<AskRead>;
  createReply?: (issueKey: string, input: CreateCommentInput) => Promise<Comment>;
}

const URGENCY_STYLES: Record<Ask["urgency"], { text: string }> = {
  blocking: { text: badgeBlocking.text },
  high: { text: badgeHigh.text },
  low: { text: badgeLow.text },
  med: { text: badgeMed.text },
};

const URGENCY_LABELS: Record<Ask["urgency"], string> = {
  blocking: "Blocking",
  high: "High",
  low: "Low",
  med: "Medium",
};

/** A compact quick-answer chip, styled like `PinButton`'s `quiet` variant: the 44 px button is
 *  invisible, the pill inside is the glyph, and only that pill shows a hover/focus outline —
 *  so a row of chips is not a row of boxes larger than their labels. `Pill` is `shrink-0
 *  whitespace-nowrap`; `max-w-full` still clamps it to the button (max-width binds a flex item
 *  whatever its shrink), and the description span alone wraps, so a long hint folds inside the
 *  pill instead of running past the margin card's edge. */
const quietChipButton =
  "group inline-flex min-h-11 max-w-full items-center focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50";
const quietChipPill = `max-w-full gap-1.5 border ${borderTransparent} ${groupHoverCardBorder} ${groupFocusVisibleBorder}`;
const quietChipDescription = "text-left font-normal whitespace-normal";

export function AskCard({
  artifactSlug,
  ask,
  thread = "inline",
  variant = "full",
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
    otherSelected,
    questionChoice,
    reasonRequired,
    requiresReason,
    selectRealOption,
    selected,
    sendAnswer,
    sendClarification,
    setAnswerText,
    setQuestionChoice,
    submit,
    submitHint,
    toggleOther,
    submitGuard,
    threadQuery,
    trimmedAnswer,
  } = useAskAnswerForm({
    answer,
    ask,
    createReply: reply,
    getAskThread: getThread,
  });
  const [ownWordsOpen, setOwnWordsOpen] = useState(false);
  const isCompact = variant === "compact";
  const answerFieldRef = useRef<HTMLTextAreaElement>(null);
  // Picking Can't / Request changes moves the person straight to the field the server insists
  // on. Keyed on the field actually being mounted, not just on the option: the compact variant
  // unmounts the field when its disclosure closes, and re-picking the same option must refocus.
  const reasonFieldShown = reasonRequired && (!isCompact || ownWordsOpen);
  useEffect(() => {
    if (reasonFieldShown) {
      answerFieldRef.current?.focus();
    }
  }, [reasonFieldShown]);
  const submitHintId = `${answerFieldId}-hint`;
  const sessionAuthor = displayedAsk.author.kind === "session" ? displayedAsk.author : undefined;
  const sessionTitle = sessionAuthor?.origin?.session_title?.trim();
  const tmuxTarget = sessionAuthor?.origin?.tmux;
  const hasUrgencyNotch = displayedAsk.urgency === "blocking" || displayedAsk.urgency === "high";
  // The thread's own "still open?" wording must track the post-answer ask, not the possibly
  // stale prop passed to this instance: `completed` renders before an invalidated `ask` prop
  // round-trips down from the parent.
  const currentAsk = completed ?? displayedAsk;
  const turnLabel = askTurnLabel(currentAsk, threadQuery.data?.replies.at(-1));
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

  const answerField = (
    <label className="block" htmlFor={answerFieldId}>
      <span
        className={
          reasonRequired ? `mb-1 block text-sm font-medium ${textSecondaryOnSurface}` : "sr-only"
        }
      >
        {reasonRequired ? "Reason (required)" : isApproval ? "Reason" : "Your answer"}
      </span>
      <textarea
        className={`block w-full rounded-lg px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
        data-ask-answer=""
        disabled={isSubmitting}
        id={answerFieldId}
        onChange={(event) => {
          setAnswerText(event.target.value);
          setQuestionChoice(false);
        }}
        onKeyDown={(event) => submitOnModifiedEnter(event)}
        placeholder={answerPlaceholder}
        ref={answerFieldRef}
        rows={2}
        value={answerText}
      />
    </label>
  );
  const submitButton = (
    <button
      aria-describedby={submitHint === undefined ? undefined : submitHintId}
      className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
      disabled={!canAnswer || isSubmitting}
      title={submitHint}
      type="submit"
    >
      {mutation.isPending ? "Answering…" : "Answer"}
    </button>
  );
  const submitHintNode =
    submitHint === undefined ? null : (
      <p className={`text-xs ${textMutedOnSurface}`} id={submitHintId}>
        {submitHint}
      </p>
    );
  const answerActions = questionChoice ? (
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
    <div className="space-y-2">
      <div className="flex gap-2">
        {submitButton}
        <button
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder}`}
          disabled={trimmedAnswer === "" || isSubmitting}
          onClick={sendClarification}
          type="button"
        >
          {clarification.isPending ? "Sending…" : "Ask back"}
        </button>
      </div>
      {submitHintNode}
    </div>
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
    <article
      aria-label={`Urgency: ${URGENCY_LABELS[displayedAsk.urgency]}`}
      className={`relative rounded-xl border-l-4 shadow-sm ${isCompact ? "px-3 pt-4 pb-3" : "px-4 pt-5 pb-4"} ${hasUrgencyNotch ? "mt-3" : ""} ${card} ${askUrgencyAccent[displayedAsk.urgency]}`}
      data-testid={`ask-${displayedAsk.id}`}
    >
      {hasUrgencyNotch ? (
        <span
          aria-hidden="true"
          className={`absolute -top-2 left-3 rounded px-1.5 text-[10px] leading-4 font-semibold tracking-wide uppercase ${surfaceBg} ${URGENCY_STYLES[displayedAsk.urgency].text}`}
        >
          {URGENCY_LABELS[displayedAsk.urgency].toUpperCase()}
        </span>
      ) : null}
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
      <div>
        {isApproval ? (
          <p className={`text-xs font-semibold uppercase tracking-wide ${textMutedOnSurface}`}>
            Approval requested
          </p>
        ) : isAction ? (
          <Pill>Action</Pill>
        ) : null}
        <div className={`text-sm leading-relaxed font-medium ${textPrimaryOnSurface}`}>
          <MarkdownBody markdown={displayedAsk.question} />
        </div>
        <p className={`mt-1 flex flex-wrap items-center gap-x-1 text-sm ${textMutedOnSurface}`}>
          <span>{actorLabel(displayedAsk.author)}</span>
          {sessionAuthor === undefined ? null : (
            <CopyButton value={sessionAuthor.id} what="session ID">
              ID
            </CopyButton>
          )}
          {sessionTitle === undefined || sessionTitle === "" ? null : (
            <CopyButton value={sessionTitle} what="session title">
              title
            </CopyButton>
          )}
          <span className="inline-flex items-center gap-x-1">
            <span aria-hidden="true">·</span>
            {isAction ? (
              <time dateTime={displayedAsk.created_at}>
                {formatAskAge(displayedAsk.created_at)}
              </time>
            ) : (
              <Timestamp at={displayedAsk.created_at} />
            )}
          </span>
          {tmuxTarget === undefined ? null : (
            <span className="inline-flex items-center gap-x-1">
              <span aria-hidden="true">·</span>
              <CopyButton value={tmuxTarget} what="tmux target">
                {tmuxTarget}
              </CopyButton>
            </span>
          )}
        </p>
        {turnLabel === null ? null : (
          <p
            className={`mt-1 text-xs font-medium ${textSecondaryOnSurface}`}
            data-testid={`turn-${displayedAsk.id}`}
          >
            {turnLabel}
          </p>
        )}
        <AskEditHistory ask={displayedAsk} edits={edits} />
      </div>
      {askChanged ? (
        <p className={`mt-3 text-sm font-medium ${inlineWarningText}`}>
          This question changed while you were answering. Review the latest wording and confirm your
          answer again; your draft text is still here.
        </p>
      ) : null}
      {threadNode}
      <form className="mt-4 space-y-3" onSubmit={submit}>
        {displayedAsk.options.length === 0 ? null : isCompact ? (
          <fieldset className="space-y-2">
            <legend className="sr-only">Quick answers</legend>
            <div className="flex flex-wrap gap-2">
              {displayedAsk.options.map((option) => {
                const checked = selected.includes(option.label);
                return (
                  <button
                    aria-pressed={checked}
                    className={quietChipButton}
                    disabled={isSubmitting}
                    key={option.label}
                    onClick={() => {
                      selectRealOption(option.label);
                      setOwnWordsOpen(requiresReason(option.label));
                    }}
                    type="button"
                  >
                    <Pill className={quietChipPill} tone={checked ? "selected-label" : "label"}>
                      <MarkdownBody markdown={option.label} variant="inline" />
                      {option.description === undefined ? null : (
                        <span className={quietChipDescription}>
                          <MarkdownBody markdown={option.description} variant="inline" />
                        </span>
                      )}
                    </Pill>
                  </button>
                );
              })}
              {isApproval || isAction ? null : (
                <button
                  aria-pressed={otherSelected}
                  className={quietChipButton}
                  disabled={isSubmitting}
                  onClick={() => {
                    toggleOther();
                    setOwnWordsOpen(true);
                  }}
                  type="button"
                >
                  <Pill className={quietChipPill} tone={otherSelected ? "selected-label" : "label"}>
                    Other
                  </Pill>
                </button>
              )}
            </div>
            {selected.length === 0 || ownWordsOpen ? null : (
              <div className="space-y-2">
                {submitButton}
                {submitHintNode}
              </div>
            )}
          </fieldset>
        ) : (
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
                    data-ask-option=""
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
            {isApproval || isAction ? null : (
              <label
                className={`flex cursor-pointer items-start gap-3 rounded-lg border px-3 py-2 ${borderDefault} ${cardHoverBorder}`}
              >
                <input
                  checked={otherSelected}
                  disabled={isSubmitting}
                  name={`ask-${displayedAsk.id}`}
                  onChange={toggleOther}
                  type={displayedAsk.multiple ? "checkbox" : "radio"}
                />
                <span className={`font-medium ${textPrimaryOnSurface}`}>Other</span>
              </label>
            )}
          </fieldset>
        )}
        {isCompact ? (
          <div>
            <button
              aria-controls={`${answerFieldId}-disclosure`}
              aria-expanded={ownWordsOpen}
              className={`min-h-11 text-sm font-medium ${textSecondaryOnSurface}`}
              onClick={() => setOwnWordsOpen((open) => !open)}
              type="button"
            >
              Add a note or answer in your own words
            </button>
            {ownWordsOpen ? (
              <div className="space-y-3 pt-3" id={`${answerFieldId}-disclosure`}>
                {answerField}
                {answerActions}
              </div>
            ) : null}
          </div>
        ) : (
          <>
            {answerField}
            {answerActions}
          </>
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
