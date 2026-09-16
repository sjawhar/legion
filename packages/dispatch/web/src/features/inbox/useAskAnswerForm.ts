import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useId, useState } from "react";

import { ApiError, api } from "../../api/client";
import type {
  AnswerAskInput,
  Ask,
  AskRead,
  Comment,
  CreateCommentInput,
  InboxRow,
} from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { answerAskInput } from "./answer-ask";
import { isQuestionShapedAnswer } from "./question-shaped-answer";

interface UseAskAnswerFormOptions {
  ask: Ask;
  answer: (id: string, input: AnswerAskInput) => Promise<Ask>;
  createReply: (issueKey: string, input: CreateCommentInput) => Promise<Comment>;
  getAskThread: (id: string) => Promise<AskRead>;
  /** Called once the server has recorded the reader's answer from this card. */
  onAnswered?: (id: string) => void;
}

export function useAskAnswerForm({
  ask,
  answer,
  createReply,
  getAskThread,
  onAnswered,
}: UseAskAnswerFormOptions) {
  const queryClient = useQueryClient();
  // Each AskCard instance owns its answer field label so cards with the same
  // ask id never collide when a responsive transition briefly renders both.
  const answerFieldId = `${useId()}-answer`;
  const [selected, setSelected] = useState<string[]>([]);
  const [otherSelected, setOtherSelected] = useState(false);
  const [answerText, setAnswerText] = useState("");
  const [questionChoice, setQuestionChoice] = useState(false);
  const [justAnswered, setJustAnswered] = useState<Ask | null>(null);
  const [askChanged, setAskChanged] = useState(false);
  const submitGuard = useSubmitGuard();
  // Shared by this card, its edit-version history, its collapsed disclosure, and its inline
  // thread — one fetch instead of each consumer issuing its own.
  const threadQuery = useQuery<AskRead, Error>({
    queryKey: ["ask-thread", ask.id],
    queryFn: () => getAskThread(ask.id),
  });
  const edits = threadQuery.data?.edits ?? [];
  const mutation = useMutation({
    mutationFn: (input: AnswerAskInput) => answer(ask.id, input),
    onMutate: async () => {
      await queryClient.cancelQueries({ queryKey: ["inbox"] });
      const previous = queryClient.getQueryData<InboxRow[]>(["inbox"]);
      queryClient.setQueryData<InboxRow[]>(["inbox"], (current) =>
        current?.filter((currentAsk) => currentAsk.id !== ask.id)
      );
      return previous;
    },
    onError: (error, _input, previous) => {
      queryClient.setQueryData(["inbox"], previous);
      void queryClient.invalidateQueries({ queryKey: ["inbox"] });
      if (error instanceof ApiError && error.code === "ASK_EDITED") {
        setAskChanged(true);
        setSelected([]);
        setOtherSelected(false);
        void queryClient.invalidateQueries({ queryKey: ["ask-thread", ask.id] });
        void threadQuery.refetch();
      }
    },
    onSettled: () => {
      submitGuard.release();
    },
    onSuccess: (updatedAsk) => {
      setJustAnswered(updatedAsk);
      onAnswered?.(ask.id);
      invalidateOwnerReads();
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

  /** The reads that carry this ask besides its own thread: the Inbox and its owner's lists. */
  function invalidateOwnerReads(): void {
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
  }

  // The thread read is the truth once the question changed under the draft, and once the ask
  // was answered or resolved elsewhere while this card still holds its open row (an Inbox row
  // kept in place while the reader is on it): the card then shows the recorded outcome.
  const threadAsk = threadQuery.data?.ask;
  const displayedAsk =
    threadAsk !== undefined && (askChanged || threadAsk.state !== "open") ? threadAsk : ask;
  const hasOptions = displayedAsk.options.length > 0;
  const isApproval = displayedAsk.kind === "approval";
  const isAction = displayedAsk.kind === "action";
  const isSubmitting = mutation.isPending || clarification.isPending;
  const trimmedAnswer = answerText.trim();
  // The server rejects these two fixed options without text; the form says so instead of
  // leaving the submit button silently dead.
  const requiresReason = (label: string): boolean =>
    (isAction && label === "Can't") || (isApproval && label === "Request changes");
  const reasonRequiredFor = selected.find(requiresReason);
  const reasonRequired = reasonRequiredFor !== undefined;
  const canAnswer =
    isApproval || isAction
      ? selected.length > 0 && (!reasonRequired || trimmedAnswer !== "")
      : hasOptions
        ? otherSelected
          ? trimmedAnswer !== ""
          : selected.length > 0 || isQuestionShapedAnswer(trimmedAnswer)
        : trimmedAnswer !== "";
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
      const answerSelection = isApproval || isAction || hasOptions ? selected : [];
      mutation.mutate(answerAskInput(displayedAsk, answerSelection, text));
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
      !isAction &&
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
  const completed = justAnswered ?? (displayedAsk.state === "open" ? null : displayedAsk);

  return {
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
    submitGuard,
    submit,
    submitHint,
    toggleOther,
    threadQuery,
    trimmedAnswer,
  };
}
