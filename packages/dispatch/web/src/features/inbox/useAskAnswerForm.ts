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
}

export function useAskAnswerForm({
  ask,
  answer,
  createReply,
  getAskThread,
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
      return createReply(ask.issue_key, { ask_id: ask.id, body: text });
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

  const displayedAsk = askChanged ? (threadQuery.data?.ask ?? ask) : ask;
  const hasOptions = displayedAsk.options.length > 0;
  const isApproval = displayedAsk.kind === "approval";
  const isAction = displayedAsk.kind === "action";
  const isSubmitting = mutation.isPending || clarification.isPending;
  const trimmedAnswer = answerText.trim();
  const canAnswer = isApproval
    ? selected.length > 0 && (!selected.includes("Request changes") || trimmedAnswer !== "")
    : isAction
      ? selected.length > 0 && (!selected.includes("Can't") || trimmedAnswer !== "")
      : hasOptions
        ? otherSelected
          ? trimmedAnswer !== ""
          : selected.length > 0 || isQuestionShapedAnswer(trimmedAnswer)
        : trimmedAnswer !== "";
  const answerPlaceholder =
    isApproval && selected.includes("Request changes")
      ? "Why? (required)"
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
    selectRealOption,
    selected,
    sendAnswer,
    sendClarification,
    setAnswerText,
    setQuestionChoice,
    submitGuard,
    submit,
    toggleOther,
    threadQuery,
    trimmedAnswer,
  };
}
