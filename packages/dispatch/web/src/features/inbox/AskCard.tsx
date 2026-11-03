import {
  type ReactNode,
  type SetStateAction,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { type ArtifactOwner, api, type WriteBounds } from "../../api/client";
import type { AnswerAskInput, Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import { ChevronIcon } from "../../components/DisclosureToggle";
import { QueryError } from "../../components/QueryError";
import { RefusableButton } from "../../components/RefusableButton";
import { compareTimestamps } from "../../lib/timestamps";
import {
  askUrgencyAccent,
  borderDefault,
  card,
  inlineWarningText,
  linkHoverText,
  linkText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButton,
  textMutedHoverToSecondary,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import type { MentionFieldHandle } from "../conversation/MentionField";
import { MarkdownBody } from "../refs/MarkdownBody";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference, itemRoute } from "../refs/routes";
import { askRepliesOwner, useMarkThreadRead } from "../replies/useMarkThreadRead";
import { type AskAnswerActions, AskAnswerArea } from "./AskAnswerArea";
import { AskApprovalLink } from "./AskApprovalLink";
import { AskBlockLink } from "./AskBlockLink";
import {
  AskCompletionCard,
  AskEditHistory,
  type AskFrame,
  OrphanedAnchorNotice,
} from "./AskCompletionCard";
import { AskHeader, hasUrgencyNotch } from "./AskHeader";
import { AskChoiceRow } from "./AskOptionRow";
import { AskRecipients } from "./AskRecipients";
import { AskThread } from "./AskThread";
import { AskThreadDisclosure } from "./AskThreadDisclosure";
import { URGENCY_LABELS } from "./ask-urgency";
import type { Draft } from "./draft-store";
import { EarlierApproval } from "./EarlierApproval";
import { clearReplyDraft } from "./reply-drafts";
import { useAskAnswerForm } from "./useAskAnswerForm";

const answerAsk = (id: string, input: AnswerAskInput, bounds: WriteBounds): Promise<Ask> =>
  api.answerAsk(id, input, bounds);
const getAskThread = (id: string): Promise<AskRead> => api.getAsk(id);
const createReply = (
  issueKey: string,
  input: CreateCommentInput,
  bounds: WriteBounds
): Promise<Comment> => api.createComment(issueKey, input, bounds);

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
  answerAsk?: (id: string, input: AnswerAskInput, bounds: WriteBounds) => Promise<Ask>;
  /** Reply-thread fetch/write seams for tests; default to the real API. */
  getAskThread?: (id: string) => Promise<AskRead>;
  /** Data the Inbox response already hydrated for the first card render. */
  initialThread?: AskRead;
  /** Timestamp of the Inbox snapshot that supplied initialThread. */
  initialThreadUpdatedAt?: number;
  createReply?: (
    issueKey: string,
    input: CreateCommentInput,
    bounds: WriteBounds
  ) => Promise<Comment>;
  /** Whether an approval ask's label also links its document and version. Default `true`: the
   *  Inbox, the drawer and the Conversation name it. The margin passes `false` — the reader is
   *  already on that document, so the link would point at the page under it. */
  documentLink?: boolean;
  /** Called once the server has recorded the reader's answer from this card. */
  onAnswered?: (id: string) => void;
  /** `change` opens the seeded form when the viewer may replace the current answer. */
  mode?: "answer" | "change";
  /** Marks the ask's thread read on the Replies page once its newest write (the newest reply, or
   *  an answer given after every reply) has been on screen. Set only on the ask's own pages - its
   *  Conversation turn, its margin card, its decision block - so the Inbox, its drawer and the
   *  Answers page start no unread-set reads. */
  markRead?: boolean;
  /** The reply a deep link named: the Conversation marks that reply and opens the card's
   *  collapsed thread over it. No other surface lands on a reply. */
  currentReplyId?: string;
}

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
  documentLink = true,
  onAnswered,
  mode = "answer",
  markRead = false,
  currentReplyId,
}: AskCardProps): ReactNode {
  const {
    answerDraft,
    answerFailure,
    answerFieldId,
    answerPlaceholder,
    answerRefusal,
    answers,
    answerText,
    askChanged,
    canChangeAnswer,
    cancelChanging,
    changing,
    clarification,
    clarificationRefusal,
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
    setAnswerDraft,
    setQuestionChoice,
    startChanging,
    submit,
    toggleOther,
    submitGuard,
    threadQuery,
    trimmedAnswer,
    unsentAnswer,
  } = useAskAnswerForm({
    answer,
    ask,
    createReply: reply,
    getAskThread: getThread,
    initialThread,
    initialThreadUpdatedAt,
    initiallyChanging: mode === "change",
    onAnswered,
  });
  const [ownWordsOpen, setOwnWordsOpen] = useState(false);
  const [handlesOpen, setHandlesOpen] = useState(false);
  const [referencesOpen, setReferencesOpen] = useState(false);
  // A change seeds the current answer's note; a compact card shows it rather than folding text
  // the save would send behind its disclosure. Set while rendering the first changing render,
  // React's pattern for state derived from a prop change, so the field is never hidden a frame.
  const [seededFor, setSeededFor] = useState(false);
  if (changing !== seededFor) {
    setSeededFor(changing);
    if (changing && variant === "compact" && answerText.trim() !== "") setOwnWordsOpen(true);
  }
  // One page can host the same ask twice (a decision block and the margin sheet), so the
  // panel each control names, and the line the lone Answer over a closed disclosure is
  // described by, are this instance's.
  const referencesPanelId = useId();
  const loneAnswerReasonId = useId();
  const isCompact = variant === "compact";
  const inBlock = frame === "block";
  const reference = itemRoute("ask", displayedAsk, displayedAsk.document ?? owner);
  // Where a file pasted or dropped into the answer, or into a reply under the ask, goes: the
  // ask's issue, or the project of the document it was asked on.
  const documentProject = (displayedAsk.document ?? owner ?? displayedAsk.anchor_artifact)?.project;
  const uploadOwner: ArtifactOwner | undefined =
    displayedAsk.issue_key !== null
      ? { issue: displayedAsk.issue_key }
      : documentProject === undefined
        ? undefined
        : { project: documentProject };
  // Everything the reader puts in the box goes through here, the keystroke and the text an
  // upload lands alike: a draft the field changed is no longer the question-shaped answer the
  // card offered to ask back instead.
  const changeDraft = (next: SetStateAction<Draft>) => {
    setAnswerDraft(next);
    setQuestionChoice(false);
  };
  const answerFieldRef = useRef<MentionFieldHandle>(null);
  // Picking Request changes moves the person straight to the field the server insists
  // on. Keyed on the field actually being mounted, not just on the option: the compact variant
  // unmounts the field when its disclosure closes, and reopening it must refocus.
  const reasonFieldShown = reasonRequired && (!isCompact || ownWordsOpen);
  useEffect(() => {
    if (reasonFieldShown) {
      answerFieldRef.current?.textarea()?.focus();
    }
  }, [reasonFieldShown]);
  // Opening the disclosure is a move into the field it reveals, whoever opened it: the reader's
  // own click, and the Inbox's Enter, which presses this same control (`Inbox.tsx`). The field
  // is drawn by the render the press schedules, so the focus can only be this effect's - the
  // opener runs before React has committed the box and would focus nothing. Only an open by the
  // disclosure itself focuses, which is why the intent is recorded as the control is pressed: an
  // option that opens the disclosure for its own words, and a change that seeds the note, leave
  // the reader where they are.
  const focusNoteOnOpen = useRef(false);
  useEffect(() => {
    if (!focusNoteOnOpen.current) return;
    focusNoteOnOpen.current = false;
    if (ownWordsOpen) {
      answerFieldRef.current?.textarea()?.focus();
    }
  }, [ownWordsOpen]);
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
  // A block ask the server indexed with no pending author has an empty label and reads as
  // "asked", with no session handles to open.
  const sessionAuthor = displayedAsk.author.kind === "session" ? displayedAsk.author : undefined;
  // The copy handles' values: the title the session stamped, as the clipboard has always had it.
  const sessionTitle = sessionAuthor?.origin?.session_title?.trim();
  const tmuxTarget = sessionAuthor?.origin?.tmux;
  const notched = hasUrgencyNotch(displayedAsk, inBlock);
  // The thread's own "still open?" wording must track the post-answer ask, not the possibly
  // stale prop passed to this instance: `completed` renders before an invalidated `ask` prop
  // round-trips down from the parent.
  const currentAsk = completed ?? displayedAsk;
  // A resolved ask takes no more replies, so an unsent one has nowhere to go.
  const resolved = displayedAsk.state === "resolved" || currentAsk.state === "resolved";
  useEffect(() => {
    if (resolved) clearReplyDraft(displayedAsk.id);
  }, [displayedAsk.id, resolved]);
  // The count comes from `displayedAsk`, never from `completed`: answering an ask does not move
  // a backlink count, and the answer response is the one ask shape that carries no count.
  const referencedByCount = displayedAsk.referenced_by_count ?? 0;
  // The thread's newest write the card shows, which the Replies mark watches and reads through:
  // the newest reply, unless the record's answer came after it (an answer is a write like a reply).
  const newestReply = threadQuery.data?.replies.at(-1);
  const recordedAnswer = completed?.state === "answered" ? completed.answer : null;
  const answerIsNewest =
    recordedAnswer !== null &&
    (newestReply === undefined || compareTimestamps(recordedAnswer.at, newestReply.created_at) > 0);
  const markNewest = useMarkThreadRead({
    enabled: markRead,
    newestShownAt: answerIsNewest ? recordedAnswer?.at : newestReply?.created_at,
    owner: askRepliesOwner(displayedAsk),
    threadId: displayedAsk.id,
  });
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
            reference={buildDispatchReference(reference)}
            toggle={{ id: referencesPanelId }}
          />
        ) : null}
      </div>
    );
  // A reply's own `dispatch://` reference needs the project document behind a document ask,
  // which only an Inbox row carries on the ask itself.
  const replyOwner = displayedAsk.document ?? owner;
  const threadNode =
    thread === "collapsed" ? (
      <AskThreadDisclosure
        ask={currentAsk}
        createReply={reply}
        currentReplyId={currentReplyId}
        embedded={completed === null}
        markNewest={answerIsNewest ? undefined : markNewest}
        owner={replyOwner}
        thread={threadQuery}
        uploadOwner={uploadOwner}
      />
    ) : (
      <AskThread
        ask={currentAsk}
        createReply={reply}
        currentReplyId={currentReplyId}
        embedded={completed === null}
        markNewest={answerIsNewest ? undefined : markNewest}
        owner={replyOwner}
        thread={threadQuery}
        uploadOwner={uploadOwner}
      />
    );

  const answerLabel = changing ? "Save answer" : "Answer";
  /** Answer, in each place this card draws it: inside the answer area, and as the lone button a
   *  compact card shows over a closed note disclosure. Only its own send, or an upload the draft
   *  is waiting on, disables it; every other reason it cannot be pressed it states and refuses. */
  const submitButton = (shownBy: string | undefined, uploading: boolean) => (
    <RefusableButton
      busy={
        mutation.isPending
          ? changing
            ? "Saving…"
            : "Answering…"
          : uploading
            ? answerLabel
            : undefined
      }
      className="min-h-11"
      refusal={answerRefusal}
      refusalShownBy={shownBy}
      type="submit"
    >
      {answerLabel}
    </RefusableButton>
  );
  const answerActions = ({ reasonId, uploading }: AskAnswerActions): ReactNode =>
    questionChoice ? (
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
        <RefusableButton
          busy={mutation.isPending ? "Answering…" : undefined}
          look="secondary"
          onPress={() => sendAnswer(answerText)}
          refusal={answerRefusal}
          refusalShownBy={reasonId.anyway}
        >
          Answer with it anyway
        </RefusableButton>
      </fieldset>
    ) : (
      <div className="flex gap-2">
        {submitButton(reasonId.answer, uploading)}
        {changing ? (
          <button
            className={secondaryButton}
            disabled={isSubmitting}
            onClick={cancelChanging}
            type="button"
          >
            Cancel
          </button>
        ) : (
          <RefusableButton
            busy={clarification.isPending ? "Sending…" : uploading ? "Ask back" : undefined}
            look="secondary"
            onPress={sendClarification}
            refusal={clarificationRefusal}
            refusalShownBy={reasonId.askBack}
          >
            Ask back
          </RefusableButton>
        )}
      </div>
    );
  /** The card's half of the one answer area (`AskAnswerArea.tsx`): the field, the `@` picker,
   *  the draft and the uploads are the area's, and these submits are the card's, because Answer,
   *  Save answer and Ask back all post through `useAskAnswerForm` rather than as a comment of
   *  the box's own. */
  const answerArea = (
    <AskAnswerArea
      actions={answerActions}
      disabled={isSubmitting}
      draft={answerDraft}
      fieldRef={answerFieldRef}
      handle="data-ask-answer"
      label={reasonRequired ? "Reason (required)" : isApproval ? "Reason" : "Your answer"}
      labelHidden={!reasonRequired}
      onDraftChange={changeDraft}
      placeholder={answerPlaceholder}
      refusals={
        questionChoice
          ? { anyway: answerRefusal }
          : { answer: answerRefusal, askBack: changing ? null : clarificationRefusal }
      }
      uploadOwner={uploadOwner}
    />
  );

  if (completed !== null) {
    return (
      <>
        <AskCompletionCard
          answers={answers}
          artifactSlug={artifactSlug}
          ask={completed}
          documentLink={documentLink}
          edits={edits}
          frame={frame}
          markRef={answerIsNewest ? markNewest : undefined}
          onChangeAnswer={canChangeAnswer ? startChanging : undefined}
          owner={owner}
        />
        {unsentAnswer === "" ? null : (
          <div
            className={`mt-3 rounded-xl border p-3 ${borderDefault}`}
            data-testid="ask-unsent-answer"
          >
            <p className={`text-xs ${textMutedOnSurface}`}>
              This question closed before you sent your answer, so it was not sent. Your text is
              here to copy.
            </p>
            <p className={`mt-1 text-sm whitespace-pre-wrap ${textPrimaryOnSurface}`}>
              {unsentAnswer}
            </p>
          </div>
        )}
        {answerFailure === null ? null : <QueryError message={answerFailure.message} />}
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
          : `relative rounded-xl border-l-4 shadow-sm ${isCompact ? "px-3 pt-4 pb-3" : "px-4 pt-5 pb-4"} ${notched ? "mt-3" : ""} ${card} ${askUrgencyAccent[displayedAsk.urgency]}`
      }
      // The one handle on a card as a whole, carrying the question the card draws: a page-level
      // key that must not fire while the reader is answering an ask (the issue page's priority
      // digits, beside this card's own options) matches the bare attribute, and the Inbox's
      // answer keys find the selected question's own card by its value (`answerHand` in
      // `Inbox.tsx`). Neither asks for a test id.
      data-ask-card={displayedAsk.id}
      data-testid={`ask-${displayedAsk.id}`}
    >
      {/* One provenance line under the question, in the spec's decided conversation grammar:
          whose turn it is (the only part a reader acts on), who asked, when, and the ask's
          own reference, drawn by the one header every surface shows an ask through. The
          session's own handles - its ID, its title, its tmux target - are chrome a reader
          wants perhaps once a week, so they fold behind the author chip and open onto their
          own row. That keeps the line inside the card at 390 px and in a 280 px margin, where
          a single row could not hold five copy controls without pushing the reference out of
          reach. */}
      <AskHeader
        ask={displayedAsk}
        control={(name) => (
          <button
            aria-expanded={handlesOpen}
            className={`flex min-w-0 items-center gap-1 ${textMutedHoverToSecondary}`}
            onClick={() => setHandlesOpen((open) => !open)}
            type="button"
          >
            {name}
            <ChevronIcon expanded={handlesOpen} />
          </button>
        )}
        inBlock={inBlock}
        lastReply={threadQuery.data?.replies.at(-1)}
        owner={owner}
        place="card"
        trailing={
          !handlesOpen ? null : (
            <span className="flex basis-full flex-wrap items-center gap-x-1.5">
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
              {tmuxTarget === undefined ? null : (
                <CopyButton value={tmuxTarget} what="tmux target">
                  {tmuxTarget}
                </CopyButton>
              )}
            </span>
          )
        }
      >
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
            <p
              className={`text-[10px] font-semibold uppercase tracking-wide ${textMutedOnSurface}`}
            >
              Approval requested
            </p>
          ) : null}
          {isApproval && documentLink ? (
            <p className={`mt-1 text-sm ${linkText} ${linkHoverText}`}>
              <AskApprovalLink ask={displayedAsk} />
            </p>
          ) : null}
          {inBlock ? null : (
            <div className={`text-[15px] leading-relaxed font-medium ${textPrimaryOnSurface}`}>
              <MarkdownBody markdown={displayedAsk.question} />
            </div>
          )}
          {/* A request to approve again names the approval it follows, under the question (the
              spec's own placement), with the diff from that version behind Show diff; a first
              request carries no earlier approval, and EarlierApproval renders nothing for it. */}
          {displayedAsk.approval === undefined ? null : (
            <EarlierApproval approval={displayedAsk.approval} />
          )}
        </div>
      </AskHeader>
      <AskEditHistory ask={displayedAsk} edits={edits} />
      {referencedByNode}
      {askChanged ? (
        <p className={`mt-3 text-sm font-medium ${inlineWarningText}`}>
          This question changed while you were answering. Review the latest wording and confirm your
          answer again; your draft text is still here.
        </p>
      ) : null}
      {threadNode}
      <form className="mt-3 space-y-3" onSubmit={submit} ref={trackForm}>
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
              // Over a closed disclosure there is no box and so no area: the lone Answer still
              // says why it refuses, on a line of this card's own.
              <div className="space-y-2">
                {answerRefusal === null ? null : (
                  <p className={`text-xs ${textMutedOnSurface}`} id={loneAnswerReasonId}>
                    {answerRefusal}
                  </p>
                )}
                {submitButton(loneAnswerReasonId, false)}
              </div>
            )}
            <div>
              <button
                aria-controls={`${answerFieldId}-disclosure`}
                aria-expanded={ownWordsOpen}
                className={`min-h-11 text-sm font-medium ${textSecondaryOnSurface}`}
                // The Inbox's Enter opens this disclosure to reach the answer box behind it,
                // and finds the control by this handle, as it finds the box by
                // `data-ask-answer` and an option by `data-ask-option` (`Inbox.tsx`).
                data-ask-note-disclosure=""
                onClick={() => {
                  // Opening it is a move into the field behind it; closing it moves nowhere.
                  focusNoteOnOpen.current = !ownWordsOpen;
                  setOwnWordsOpen(!ownWordsOpen);
                }}
                type="button"
              >
                Add a note or answer in your own words
              </button>
              {ownWordsOpen ? (
                <div className="space-y-3 pt-3" id={`${answerFieldId}-disclosure`}>
                  {answerArea}
                </div>
              ) : null}
            </div>
          </>
        ) : (
          answerArea
        )}
        {answerFailure === null ? null : (
          <QueryError
            message={answerFailure.message}
            onRetry={answerFailure.retryable ? () => submitGuard.retryLast(mutation) : undefined}
            retrying={mutation.isPending}
          />
        )}
        {clarification.isError ? (
          <QueryError
            message="Could not send your clarification."
            onRetry={() => submitGuard.retryLast(clarification)}
            retrying={clarification.isPending}
          />
        ) : null}
      </form>
      <AskRecipients
        askId={displayedAsk.id}
        claimant={threadQuery.data?.claimant ?? null}
        followers={threadQuery.data?.followers ?? []}
        owner={displayedAsk}
        route={threadQuery.data?.route ?? null}
        routeHolder={threadQuery.data?.route_holder ?? null}
        routeStatus={threadQuery.data?.route_status ?? null}
      />
    </article>
  );
}
