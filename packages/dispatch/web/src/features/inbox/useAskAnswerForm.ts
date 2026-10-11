import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";

import { ApiError, api, apiErrorMessage } from "../../api/client";
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
import { answerAskInput } from "./answer-ask";
import {
  clearPendingAskThreadInvalidation,
  hasPendingAskThreadInvalidation,
} from "./ask-thread-freshness";
import { isQuestionShapedAnswer } from "./question-shaped-answer";

interface UseAskAnswerFormOptions {
  ask: Ask;
  answer: (id: string, input: AnswerAskInput) => Promise<Ask>;
  createReply: (issueKey: string, input: CreateCommentInput) => Promise<Comment>;
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
  const [answerText, setAnswerText] = useState("");
  const [questionChoice, setQuestionChoice] = useState(false);
  const [justAnswered, setJustAnswered] = useState<Ask | null>(null);
  const [askChanged, setAskChanged] = useState(false);
  const [changing, setChanging] = useState(false);
  const replacing = useRef<AskAnswer | undefined>(undefined);
  const initialChangeConsumed = useRef(false);
  const submitGuard = useSubmitGuard();
  // Shared by this card, its edit-version history, its collapsed disclosure, and its inline
  // thread. A thread invalidated while its Inbox snapshot was in flight bypasses that snapshot.
  const refreshInitialThread =
    initialThread !== undefined && hasPendingAskThreadInvalidation(queryClient, ask.id);
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
  const mutation = useMutation({
    mutationFn: (input: AnswerAskInput) => answer(ask.id, input),
    onMutate: async () => {
      if (replacing.current !== undefined) return undefined;
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
      setJustAnswered(updatedAsk);
      if (changed) {
        if (updatedAsk.answer !== null) {
          const changedAnswer = updatedAsk.answer;
          queryClient.setQueryData<AskRead>(["ask-thread", ask.id], (current) =>
            current === undefined
              ? undefined
              : { ...current, answers: [...current.answers, changedAnswer], ask: updatedAsk }
          );
        }
        void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
        void queryClient.invalidateQueries({ queryKey: ["me", "answers"] });
      } else {
        onAnswered?.(ask.id);
      }
      invalidateOwnerReads();
    },
  });
  const clarification = useMutation({
    mutationFn: (text: string) => {
      if (ask.issue_key === null) {
        return api.createArtifactComment(documentArtifactId(), { ask_id: ask.id, body: text });
      }
      return createReply(ask.issue_key, { ask_id: ask.id, body: text });
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
    setAnswerText(current.text ?? "");
    setQuestionChoice(false);
    setChanging(true);
  };
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
  };
}
