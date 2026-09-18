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
  /** Data the Inbox response already hydrated for this card's first render. */
  initialThread?: AskRead;
  /** Called once the server has recorded the reader's answer from this card. */
  onAnswered?: (id: string) => void;
}

export function useAskAnswerForm({
  answer,
  ask,
  createReply,
  getAskThread,
  initialThread,
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
  // thread. An Inbox row initializes it; invalidation refreshes this same targeted query.
  const threadQuery = useQuery<AskRead, Error>({
    initialData: initialThread,
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
    void queryClient.invalidateQueries({ queryKey: ["inbox"] });
    if (ask.issue_key === null) {
      void queryClient.invalidateQueries({ queryKey: ["artifact", documentArtifactId()] });
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
