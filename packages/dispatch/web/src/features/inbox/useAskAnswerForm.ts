import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";

import { ApiError, api, apiErrorMessage, type WriteBounds } from "../../api/client";
import { inboxQuery, projectsQuery, whoAmIQuery } from "../../api/queries";
import type {
  AnswerAskInput,
  Ask,
  AskAnswer,
  AskRead,
  Comment,
  CreateCommentInput,
  InboxRow,
} from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { isViewer } from "../refs/actor";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { answerAskInput } from "./answer-ask";
import { useAnswerDraft } from "./answer-drafts";
import {
  clearPendingAskThreadInvalidation,
  hasPendingAskThreadInvalidation,
} from "./ask-thread-freshness";
import { markAnswerOutstanding } from "./outstanding-answers";
import { isQuestionShapedAnswer } from "./question-shaped-answer";

interface UseAskAnswerFormOptions {
  ask: Ask;
  answer: (id: string, input: AnswerAskInput, bounds: WriteBounds) => Promise<Ask>;
  createReply: (
    issueKey: string,
    input: CreateCommentInput,
    bounds: WriteBounds
  ) => Promise<Comment>;
  getAskThread: (id: string) => Promise<AskRead>;
  /** Data the Inbox response already hydrated for this card's first render. */
  initialThread?: AskRead;
  /** Timestamp of the Inbox snapshot that supplied initialThread. */
  initialThreadUpdatedAt?: number;
  /** Called once the server has recorded the reader's answer from this card. */
  onAnswered?: (id: string) => void;
  /** Opens the form for an answer the viewer may change, instead of its completion record. */
  initiallyChanging?: boolean;
}

/** A failed answer, as the card shows it. */
interface AskAnswerFailure {
  message: string;
  /** False for a refusal the same answer never gets past. */
  retryable: boolean;
}

/**
 * The refusals the same answer is refused with again however often it is sent, and what the card
 * says for each: the ask was answered or closed elsewhere, its question changed (the card reloads
 * it, so the next answer is to the new wording), or writing even its server state would leave the
 * document's live state past what the server can decode. Any other failure may pass on a retry.
 */
function answerFailure(error: Error): AskAnswerFailure {
  if (error instanceof ApiError) {
    switch (error.code) {
      case "ASK_ANSWER_CHANGED":
        return {
          message: "The answer changed since you opened it; here is the current one.",
          retryable: false,
        };
      case "NOT_ANSWERER":
        return {
          message: "Only the person who answered can change this answer.",
          retryable: false,
        };
      case "ASK_APPROVAL_REVIEW":
        return {
          message: "An approval is a review; record a new review on the document.",
          retryable: false,
        };
      case "ASK_CLOSED":
        return {
          message: "This ask was already answered, so your answer was not saved.",
          retryable: false,
        };
      case "ASK_RESOLVED":
        return { message: "This ask was closed, so your answer was not saved.", retryable: false };
      case "ASK_EDITED":
        return {
          message: "Your answer was not saved, because the question changed.",
          retryable: false,
        };
      case "CAP_EXCEEDED":
        return {
          message: apiErrorMessage(
            error,
            "Your answer was not saved, because its document would be too large to load."
          ),
          retryable: false,
        };
    }
  }
  return { message: "Could not save your answer.", retryable: true };
}

export function useAskAnswerForm({
  answer,
  ask,
  createReply,
  getAskThread,
  initialThread,
  initialThreadUpdatedAt,
  initiallyChanging = false,
  onAnswered,
}: UseAskAnswerFormOptions) {
  const queryClient = useQueryClient();
  const viewer = useQuery(whoAmIQuery()).data?.login;
  // Each AskCard instance owns its answer field label so cards with the same
  // ask id never collide when a responsive transition briefly renders both.
  const answerFieldId = `${useId()}-answer`;
  const [selected, setSelected] = useState<string[]>([]);
  const [otherSelected, setOtherSelected] = useState(false);
  const [questionChoice, setQuestionChoice] = useState(false);
  const [justAnswered, setJustAnswered] = useState<Ask | null>(null);
  const [askChanged, setAskChanged] = useState(false);
  const [changing, setChanging] = useState(false);
  // The reader's unsent answer, which outlives this form: the ask's own Inbox row can leave
  // under it (withdrawn, resolved, or answered by somebody else), and the reading column then
  // keeps the question with this text under it rather than taking both off the screen.
  const [draftAnswer, setDraftAnswer] = useAnswerDraft(ask.id);
  // The text of a recorded answer the reader is changing. It is this form's own and never the
  // shared draft: an answer the server has recorded is not unsent text, so a change they cancel
  // or walk away from leaves nothing behind for another card to show them as one
  // (`unsentAnswer`) or for the reading column to hold the question by (`Inbox.tsx`, `paneAsk`).
  // Cancelling is what puts the draft that stood before the change back in the box.
  const [changeAnswer, setChangeAnswer] = useState("");
  const answerText = changing ? changeAnswer : draftAnswer;
  const setAnswerText = changing ? setChangeAnswer : setDraftAnswer;
  const replacing = useRef<AskAnswer | undefined>(undefined);
  const initialChangeConsumed = useRef(false);
  const submitGuard = useSubmitGuard();
  // Shared by this card, its edit-version history, its collapsed disclosure, and its inline
  // thread. A thread invalidated while its Inbox snapshot was in flight bypasses that snapshot,
  // and so does every thread on an issue whose claim or route moved while it was in flight.
  const refreshInitialThread =
    initialThread !== undefined && hasPendingAskThreadInvalidation(queryClient, ask);
  const threadQuery = useQuery<AskRead, Error>({
    initialData: refreshInitialThread ? undefined : initialThread,
    initialDataUpdatedAt: refreshInitialThread ? undefined : initialThreadUpdatedAt,
    queryKey: ["ask-thread", ask.id],
    queryFn: () => getAskThread(ask.id),
    staleTime: Number.POSITIVE_INFINITY,
  });
  useEffect(() => {
    if (refreshInitialThread && threadQuery.data !== undefined) {
      clearPendingAskThreadInvalidation(queryClient, ask.id);
    }
  }, [ask.id, queryClient, refreshInitialThread, threadQuery.data]);
  const edits = threadQuery.data?.edits ?? [];
  const answers = threadQuery.data?.answers ?? [];
  // The card says why an answer was refused, in the ask's own words, beside the draft it holds;
  // the shell's indicator shows the answer only while it is out.
  const mutation = useKeyedWrite({
    inline: true,
    label: () => `Answer to “${ask.question}”`,
    write: (input: AnswerAskInput, bounds) => answer(ask.id, input, bounds),
    onMutate: async () => {
      if (replacing.current !== undefined) return undefined;
      // Registered before the row goes, never a commit after it: the reading column reads this
      // at the commit the removal below makes, and a hold arriving later would reach it once the
      // card about to show a refusal had been unmounted with the question.
      markAnswerOutstanding(ask.id, true);
      await queryClient.cancelQueries({ queryKey: inboxQuery().queryKey });
      const previous = queryClient.getQueryData<InboxRow[]>(inboxQuery().queryKey);
      queryClient.setQueryData<InboxRow[]>(inboxQuery().queryKey, (current) =>
        current?.filter((currentAsk) => currentAsk.id !== ask.id)
      );
      return previous;
    },
    onError: (error, _input, previous) => {
      if (replacing.current === undefined) {
        queryClient.setQueryData(inboxQuery().queryKey, previous);
        void queryClient.invalidateQueries({ queryKey: inboxQuery().queryKey });
      }
      if (error instanceof ApiError && error.code === "ASK_EDITED") {
        setAskChanged(true);
        setSelected([]);
        setOtherSelected(false);
        void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
      }
      if (
        error instanceof ApiError &&
        (error.code === "ASK_ANSWER_CHANGED" ||
          error.code === "NOT_ANSWERER" ||
          error.code === "ASK_APPROVAL_REVIEW")
      ) {
        // Each refusal meets the same change again, so the form it was sent from closes and the
        // card shows the thread's current answer, not one this card recorded earlier.
        void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
        replacing.current = undefined;
        setJustAnswered(null);
        setChanging(false);
      }
    },
    onSettled: () => {
      submitGuard.release();
    },
    onSuccess: (updatedAsk) => {
      const changed = replacing.current !== undefined;
      replacing.current = undefined;
      setChanging(false);
      // The text is the recorded answer now, not an unsent draft: a later card for this ask
      // starts empty, and nothing offers to copy what the server already took.
      setAnswerText("");
      // Answered either way; with no answer to give, the card reads the ask afresh instead of
      // showing the one Dispatch would have handed back.
      setJustAnswered(updatedAsk);
      if (changed || updatedAsk === null) {
        if (updatedAsk?.answer != null) {
          const changedAnswer = updatedAsk.answer;
          queryClient.setQueryData<AskRead>(["ask-thread", ask.id], (current) =>
            current === undefined
              ? undefined
              : { ...current, answers: [...current.answers, changedAnswer], ask: updatedAsk }
          );
        }
        void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
        void queryClient.invalidateQueries({ queryKey: ["me", "answers"] });
      }
      if (!changed) onAnswered?.(ask.id);
      invalidateOwnerReads();
    },
  });
  // How long the Inbox's reading column keeps the question: while this answer is the reader's to
  // finish - in flight, or refused with the card still holding that refusal under what they
  // answered - and no longer. The same `pending || failed` liveness `SnoozeControl` reports its
  // own write by, let go when the server decides the answer, when the form is reset, and when
  // the card goes.
  const answerLive = mutation.isPending || mutation.isError;
  useEffect(() => {
    if (!answerLive) return;
    markAnswerOutstanding(ask.id, true);
    return () => markAnswerOutstanding(ask.id, false);
  }, [answerLive, ask.id]);
  const clarification = useKeyedWrite({
    inline: true,
    label: () => `Question back on “${ask.question}”`,
    write: (text: string, bounds) => {
      if (ask.issue_key === null) {
        return api.createArtifactComment(
          documentArtifactId(),
          { ask_id: ask.id, body: text },
          bounds
        );
      }
      return createReply(ask.issue_key, { ask_id: ask.id, body: text }, bounds);
    },
    onSettled: () => {
      submitGuard.release();
    },
    onSuccess: () => {
      setAnswerText("");
      setQuestionChoice(false);
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
      // A clarification hands the turn to the asker: every surface that renders this ask from
      // its owner's list (a decision block reads `["asks", key]`) must see the new `waiting_on`,
      // not just the Inbox.
      invalidateOwnerReads();
    },
  });

  /** A document ask's artifact; the server always sets it, so its absence is a contract error. */
  function documentArtifactId(): string {
    if (ask.artifact_id === null || ask.artifact_id === undefined) {
      throw new Error("document ask is missing its artifact id");
    }
    return ask.artifact_id;
  }

  /** The reads that carry this ask besides its own thread: the Inbox and its owner's lists. */
  function invalidateOwnerReads(): void {
    void queryClient.invalidateQueries({ queryKey: inboxQuery().queryKey });
    if (ask.issue_key === null) {
      void queryClient.invalidateQueries({ queryKey: ["artifact", documentArtifactId()] });
      void queryClient.invalidateQueries({ queryKey: projectsQuery().queryKey });
      return;
    }
    void queryClient.invalidateQueries({ queryKey: ["asks", ask.issue_key] });
    void queryClient.invalidateQueries({ queryKey: ["issue", ask.issue_key] });
    void queryClient.invalidateQueries({ queryKey: ["issues"] });
  }

  // The thread read is the truth once the question changed under the draft, and once the ask
  // was answered or resolved elsewhere while this card still holds its open row (an Inbox row
  // kept in place while the reader is on it): the card then shows the recorded outcome.
  const threadAsk = threadQuery.data?.ask;
  const displayedAsk =
    threadAsk !== undefined && (askChanged || threadAsk.state !== "open") ? threadAsk : ask;
  const recordedAsk = justAnswered ?? (displayedAsk.state === "open" ? null : displayedAsk);
  const canChangeAnswer =
    recordedAsk !== null &&
    recordedAsk.kind !== "approval" &&
    recordedAsk.state === "answered" &&
    recordedAsk.answer !== null &&
    isViewer({ id: recordedAsk.answer.user, kind: "user" }, viewer);
  const startChanging = () => {
    if (!canChangeAnswer || recordedAsk?.answer === null || recordedAsk === null) return;
    const current = recordedAsk.answer;
    const realSelected = current.selected.filter((label) =>
      recordedAsk.options.some((option) => option.label === label)
    );
    replacing.current = current;
    mutation.reset();
    setSelected(realSelected);
    setOtherSelected(realSelected.length === 0 && current.text !== null && current.text !== "");
    // `changing` is still false this render, so the recorded text goes straight to the form's
    // own state rather than through `setAnswerText` into the shared draft.
    setChangeAnswer(current.text ?? "");
    setQuestionChoice(false);
    setChanging(true);
  };
  // Cancelling leaves the recorded answer where it was recorded: `answerText` falls back to the
  // draft that stood before the change, which is empty on a card the reader only read.
  const cancelChanging = () => {
    replacing.current = undefined;
    mutation.reset();
    setChanging(false);
  };
  // No dependency list on purpose: it runs after every render until the viewer and the recorded
  // answer have both resolved, then once (the ref is the guard), so `mode="change"` opens the form
  // as soon as the card knows the viewer may change the answer.
  useEffect(() => {
    if (!initiallyChanging || initialChangeConsumed.current) return;
    if (viewer === undefined || recordedAsk === null) return;
    initialChangeConsumed.current = true;
    if (canChangeAnswer) startChanging();
  });
  const hasOptions = displayedAsk.options.length > 0;
  const isApproval = displayedAsk.kind === "approval";
  const isSubmitting = mutation.isPending || clarification.isPending;
  const trimmedAnswer = answerText.trim();
  // The server rejects Request changes without text; the form says so instead of leaving the
  // submit button silently dead.
  const requiresReason = (label: string): boolean => isApproval && label === "Request changes";
  const reasonRequiredFor = selected.find(requiresReason);
  const reasonRequired = reasonRequiredFor !== undefined;
  // The four submit rules: an approval needs a choice (and a reason when the choice demands
  // one); free text needs text; "Other" needs text; options need a choice or a question typed.
  function canSubmitAnswer(): boolean {
    if (isApproval) {
      return selected.length > 0 && (!reasonRequired || trimmedAnswer !== "");
    }
    if (!hasOptions) {
      return trimmedAnswer !== "";
    }
    if (otherSelected) {
      return trimmedAnswer !== "";
    }
    return selected.length > 0 || isQuestionShapedAnswer(trimmedAnswer);
  }
  const canAnswer = canSubmitAnswer();
  const submitHint =
    reasonRequired && trimmedAnswer === ""
      ? `Add a reason to send ${reasonRequiredFor}`
      : undefined;
  const answerPlaceholder = reasonRequired
    ? "Why?"
    : isApproval && selected.includes("Approve")
      ? "Note (optional)"
      : selected.length > 0
        ? "Add a note (optional)"
        : "Answer in your own words, or ask a question back";
  const sendAnswer = (text: string) => {
    submitGuard.guard(() => {
      const answerSelection = isApproval || hasOptions ? selected : [];
      mutation.mutate(answerAskInput(displayedAsk, answerSelection, text, replacing.current));
    });
  };

  const selectRealOption = (label: string) => {
    setQuestionChoice(false);
    if (displayedAsk.multiple) {
      setSelected((current) =>
        current.includes(label)
          ? current.filter((currentLabel) => currentLabel !== label)
          : [...current, label]
      );
      return;
    }
    setSelected([label]);
    setOtherSelected(false);
  };

  const toggleOther = () => {
    setQuestionChoice(false);
    if (displayedAsk.multiple) {
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
    if (!canAnswer) return;
    if (
      !isApproval &&
      !otherSelected &&
      selected.length === 0 &&
      isQuestionShapedAnswer(trimmedAnswer)
    ) {
      setQuestionChoice(true);
      return;
    }
    sendAnswer(answerText);
  };
  const sendClarification = () => {
    const text = answerText.trim();
    if (text === "") return;
    submitGuard.guard(() => clarification.mutate(text));
  };
  const completed = changing ? null : recordedAsk;
  // What the reader typed and no form will ever send: the question closed under them - withdrawn,
  // resolved, or answered by somebody else - while their answer was still a draft. The card keeps
  // it where they can read and copy it, since the box it was in has gone. Their own answer is not
  // this: it left as a draft the moment the server recorded it.
  const unsentAnswer = completed !== null && justAnswered === null ? answerText : "";

  return {
    answerFailure: mutation.error === null ? null : answerFailure(mutation.error),
    answerFieldId,
    answerPlaceholder,
    answers,
    answerText,
    askChanged,
    canAnswer,
    canChangeAnswer,
    cancelChanging,
    changing,
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
    startChanging,
    submitGuard,
    submit,
    submitHint,
    toggleOther,
    threadQuery,
    trimmedAnswer,
    unsentAnswer,
  };
}
