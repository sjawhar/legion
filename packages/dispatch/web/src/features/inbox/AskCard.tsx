import {
  type ReactNode,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { api } from "../../api/client";
import type { AnswerAskInput, Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import { QueryError } from "../../components/QueryError";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
import {
  askUrgencyAccent,
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
  surfaceBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { actorLabel } from "../refs/actor";
import { CopyRefButton } from "../refs/CopyRefButton";
import { MarkdownBody } from "../refs/MarkdownBody";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference, itemRoute } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { AskAnchorHeader } from "./AskAnchorLink";
import { AskBlockLink } from "./AskBlockLink";
import {
  AskCompletionCard,
  AskEditHistory,
  type AskFrame,
  OrphanedAnchorNotice,
} from "./AskCompletionCard";
import { AskChoiceRow } from "./AskOptionRow";
import { AskRecipients } from "./AskRecipients";
import { AskThread } from "./AskThread";
import { AskThreadDisclosure } from "./AskThreadDisclosure";
import { askTurnLabel } from "./ask-turn";
import { URGENCY_LABELS } from "./ask-urgency";
import { useAskAnswerForm } from "./useAskAnswerForm";

const answerAsk = (id: string, input: AnswerAskInput): Promise<Ask> => api.answerAsk(id, input);
const getAskThread = (id: string): Promise<AskRead> => api.getAsk(id);
const createReply = (issueKey: string, input: CreateCommentInput): Promise<Comment> =>
  api.createComment(issueKey, input);

export interface AskCardProps {
  artifactSlug?: string;
  ask: Ask;
  /** The project document that owns an ask whose read lacks `document` (only Inbox rows carry
   *  it), so the card can still name the ask's `dispatch://` reference. */
  owner?: { project: string; slug: string };
  thread?: "inline" | "collapsed";
  /** `compact` (the margin and the review sheet) renders the same option rows as `full`, but
   *  folds the free-text field and its Answer / Ask back actions behind an **Add a note or answer
   *  in your own words** disclosure, showing a lone Answer button once an option is picked; the
   *  disclosure opens itself when the pick needs words (Other, or an option requiring a reason).
   *  The article also takes tighter padding (`px-3 pt-4 pb-3` against `px-4 pt-5 pb-4`). Nothing
   *  else depends on the variant. */
  variant?: "compact" | "full";
  /** `card` (the default) draws the card: frame, urgency accent and notch, the question, and the
   *  link to a block ask's document. `block` is the card hosted inside its own decision block
   *  (`features/doc/AskBlockCard.tsx`): the block shell already draws the frame and accent, the
   *  editor shows the question, and the block is the link's target, so the card renders only
   *  what the block lacks — who asked and when, the exchange, and the composer or the record. */
  frame?: AskFrame;
  answerAsk?: (id: string, input: AnswerAskInput) => Promise<Ask>;
  /** Reply-thread fetch/write seams for tests; default to the real API. */
  getAskThread?: (id: string) => Promise<AskRead>;
  /** Data the Inbox response already hydrated for the first card render. */
  initialThread?: AskRead;
  /** Timestamp of the Inbox snapshot that supplied initialThread. */
  initialThreadUpdatedAt?: number;
  createReply?: (issueKey: string, input: CreateCommentInput) => Promise<Comment>;
  /** Called once the server has recorded the reader's answer from this card. */
  onAnswered?: (id: string) => void;
}

/** The urgency notch's text colour, matching the urgency badge of the same level. */
const URGENCY_NOTCH_TEXT: Record<Ask["urgency"], string> = {
  blocking: badgeBlocking.text,
  high: badgeHigh.text,
  low: badgeLow.text,
  med: badgeMed.text,
};

/** Moves focus from inside the form to the nearest focusable ancestor (in the Inbox, the row, so
 *  j/k/Escape still start from the same place) instead of letting it fall to the document body. */
function handOffFocus(form: HTMLElement): void {
  if (!form.contains(document.activeElement)) return;
  form.parentElement?.closest<HTMLElement>("[tabindex]")?.focus({ preventScroll: true });
}

/** The form's callback ref. When the form leaves while a control inside it has focus - the ask
 *  was answered or resolved elsewhere and the card swaps to its record - focus is handed off
 *  without any event; React detaches a ref before removing its node, so the form is still in the
 *  document when this cleanup runs. The form is also kept in `formRef` for the disabled-control
 *  hand-off below: in the compact variant the answer field is unmounted while its disclosure is
 *  closed, so the field's `.form` is not a reliable way to reach it. The callback must be stable
 *  across renders: React re-runs a changed callback ref's cleanup on every commit, and that
 *  cleanup moves focus. */
function useFormRef(): [
  { current: HTMLFormElement | null },
  (form: HTMLFormElement | null) => (() => void) | undefined,
] {
  const formRef = useRef<HTMLFormElement | null>(null);
  const track = useCallback((form: HTMLFormElement | null) => {
    formRef.current = form;
    if (form === null) return undefined;
    return () => {
      formRef.current = null;
      handOffFocus(form);
    };
  }, []);
  return [formRef, track];
}

export function AskCard({
  artifactSlug,
  ask,
  owner,
  thread = "inline",
  variant = "full",
  frame = "card",
  answerAsk: answer = answerAsk,
  createReply: reply = createReply,
  getAskThread: getThread = getAskThread,
  initialThread,
  initialThreadUpdatedAt,
  onAnswered,
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
    initialThread,
    initialThreadUpdatedAt,
    onAnswered,
  });
  const [ownWordsOpen, setOwnWordsOpen] = useState(false);
  const [referencesOpen, setReferencesOpen] = useState(false);
  // One page can host the same ask twice (a decision block and the margin sheet), so the
  // panel each control names is this instance's.
  const referencesPanelId = useId();
  const isCompact = variant === "compact";
  const inBlock = frame === "block";
  const reference = itemRoute("ask", displayedAsk, displayedAsk.document ?? owner);
  const answerFieldRef = useRef<HTMLTextAreaElement>(null);
  // Picking Request changes moves the person straight to the field the server insists
  // on. Keyed on the field actually being mounted, not just on the option: the compact variant
  // unmounts the field when its disclosure closes, and reopening it must refocus.
  const reasonFieldShown = reasonRequired && (!isCompact || ownWordsOpen);
  useEffect(() => {
    if (reasonFieldShown) {
      answerFieldRef.current?.focus();
    }
  }, [reasonFieldShown]);
  // A control that disables itself under the reader's focus - Ask back while sending and once
  // its text is sent - would have the browser drop focus to the document body (in the Inbox, out
  // of the row, which then lets the row go) with nothing the card could act on afterwards; the
  // same hand-off as a vanishing form, in the commit that disables it, before the browser looks.
  const [formRef, trackForm] = useFormRef();
  useLayoutEffect(() => {
    const form = formRef.current;
    const active = document.activeElement;
    if (form !== null && active instanceof HTMLElement && active.matches(":disabled")) {
      handOffFocus(form);
    }
  });
  const submitHintId = `${answerFieldId}-hint`;
  const authorLabel = actorLabel(displayedAsk.author);
  const sessionAuthor = displayedAsk.author.kind === "session" ? displayedAsk.author : undefined;
  const sessionTitle = sessionAuthor?.origin?.session_title?.trim();
  const tmuxTarget = sessionAuthor?.origin?.tmux;
  const hasUrgencyNotch =
    !inBlock && (displayedAsk.urgency === "blocking" || displayedAsk.urgency === "high");
  // The thread's own "still open?" wording must track the post-answer ask, not the possibly
  // stale prop passed to this instance: `completed` renders before an invalidated `ask` prop
  // round-trips down from the parent.
  const currentAsk = completed ?? displayedAsk;
  // The count comes from `displayedAsk`, never from `completed`: answering an ask does not move
  // a backlink count, and the answer response is the one ask shape that carries no count.
  const referencedByCount = displayedAsk.referenced_by_count ?? 0;
  const turnLabel = askTurnLabel(currentAsk, threadQuery.data?.replies.at(-1));
  const referencedByNode =
    reference === undefined || referencedByCount === 0 ? null : (
      <div className="mt-3">
        <ReferencedByToggle
          controls={referencesPanelId}
          count={referencedByCount}
          expanded={referencesOpen}
          onToggle={() => setReferencesOpen((open) => !open)}
        />
        {referencesOpen ? (
          <ReferencedBy
            className="mt-2"
            id={referencesPanelId}
            reference={buildDispatchReference(reference)}
          />
        ) : null}
      </div>
    );
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
        <AskCompletionCard
          artifactSlug={artifactSlug}
          ask={completed}
          edits={edits}
          frame={frame}
        />
        {referencedByNode}
        {threadNode}
      </>
    );
  }

  return (
    <article
      aria-label={`Urgency: ${URGENCY_LABELS[displayedAsk.urgency]}`}
      className={
        inBlock
          ? "relative mt-3"
          : `relative rounded-xl border-l-4 shadow-sm ${isCompact ? "px-3 pt-4 pb-3" : "px-4 pt-5 pb-4"} ${hasUrgencyNotch ? "mt-3" : ""} ${card} ${askUrgencyAccent[displayedAsk.urgency]}`
      }
      data-testid={`ask-${displayedAsk.id}`}
    >
      {hasUrgencyNotch ? (
        <span
          aria-hidden="true"
          className={`absolute -top-2 left-3 rounded px-1.5 text-[10px] leading-4 font-semibold tracking-wide uppercase ${surfaceBg} ${URGENCY_NOTCH_TEXT[displayedAsk.urgency]}`}
        >
          {URGENCY_LABELS[displayedAsk.urgency].toUpperCase()}
        </span>
      ) : null}
      <AskAnchorHeader ask={displayedAsk} inBlock={inBlock} quoteSpacing="roomy" tone="surface" />
      <OrphanedAnchorNotice artifactSlug={artifactSlug} ask={displayedAsk} />
      {inBlock ||
      displayedAsk.block_id === undefined ||
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
        ) : null}
        {inBlock ? null : (
          <div className={`text-sm leading-relaxed font-medium ${textPrimaryOnSurface}`}>
            <MarkdownBody markdown={displayedAsk.question} />
          </div>
        )}
        <p className={`mt-1 flex flex-wrap items-center gap-x-1 text-sm ${textMutedOnSurface}`}>
          {/* A block ask the server indexed with no pending author has an empty label; it still
              says when it was asked rather than opening with a dangling separator. */}
          <span>{authorLabel === "" ? "asked" : authorLabel}</span>
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
            <Timestamp at={displayedAsk.created_at} />
          </span>
          {tmuxTarget === undefined ? null : (
            <span className="inline-flex items-center gap-x-1">
              <span aria-hidden="true">·</span>
              <CopyButton value={tmuxTarget} what="tmux target">
                {tmuxTarget}
              </CopyButton>
            </span>
          )}
          {reference === undefined ? null : <CopyRefButton route={reference} />}
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
        <AskRecipients
          askId={displayedAsk.id}
          followers={threadQuery.data?.followers ?? []}
          owner={displayedAsk}
        />
        {referencedByNode}
      </div>
      {askChanged ? (
        <p className={`mt-3 text-sm font-medium ${inlineWarningText}`}>
          This question changed while you were answering. Review the latest wording and confirm your
          answer again; your draft text is still here.
        </p>
      ) : null}
      {threadNode}
      <form className="mt-4 space-y-3" onSubmit={submit} ref={trackForm}>
        {displayedAsk.options.length === 0 ? null : (
          <fieldset className="min-w-0 space-y-2">
            <legend className="sr-only">Answer options</legend>
            {displayedAsk.options.map((option) => (
              <AskChoiceRow
                checked={selected.includes(option.label)}
                disabled={isSubmitting}
                hotkey
                key={option.label}
                multiple={displayedAsk.multiple}
                name={`ask-${displayedAsk.id}`}
                onChange={() => {
                  selectRealOption(option.label);
                  if (isCompact)
                    setOwnWordsOpen(requiresReason(option.label) || trimmedAnswer !== "");
                }}
                option={option}
              />
            ))}
            {isApproval ? null : (
              <AskChoiceRow
                checked={otherSelected}
                disabled={isSubmitting}
                multiple={displayedAsk.multiple}
                name={`ask-${displayedAsk.id}`}
                onChange={() => {
                  toggleOther();
                  if (isCompact && !otherSelected) setOwnWordsOpen(true);
                }}
                option={{ label: "Other" }}
              />
            )}
          </fieldset>
        )}
        {isCompact ? (
          <>
            {selected.length === 0 || ownWordsOpen ? null : (
              <div className="space-y-2">
                {submitButton}
                {submitHintNode}
              </div>
            )}
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
          </>
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
