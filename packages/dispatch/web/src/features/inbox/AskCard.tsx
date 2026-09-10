import { useMutation, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { api } from "../../api/client";
import type { AnswerAskInput, Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { actorLabel } from "../refs/actor";
import { Timestamp } from "../refs/Timestamp";
import { AskThread } from "./AskThread";

const answerAsk = (id: string, input: AnswerAskInput): Promise<Ask> => api.answerAsk(id, input);

export interface AskCardProps {
  ask: Ask;
  answerAsk?: (id: string, input: AnswerAskInput) => Promise<Ask>;
  /** Reply-thread fetch/write seams for tests; default to the real API. */
  getAskThread?: (id: string) => Promise<AskRead>;
  createReply?: (issueKey: string, input: CreateCommentInput) => Promise<Comment>;
}

const URGENCY_STYLES: Record<Ask["urgency"], string> = {
  blocking: "bg-rose-100 text-rose-800 dark:bg-rose-950 dark:text-rose-300",
  high: "bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300",
  low: "bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-300",
  med: "bg-sky-100 text-sky-800 dark:bg-sky-950 dark:text-sky-300",
};
const URGENCY_LABELS: Record<Ask["urgency"], string> = {
  blocking: "Blocking",
  high: "High",
  low: "Low",
  med: "Medium",
};

function AnsweredAsk({ ask }: { ask: Ask & { answer: NonNullable<Ask["answer"]> } }): ReactNode {
  const { answer } = ask;
  return (
    <article
      className="rounded-xl border border-emerald-200 bg-emerald-50 p-4 text-sm text-emerald-900 dark:border-emerald-900 dark:bg-emerald-950 dark:text-emerald-300"
      data-testid={`ask-${ask.id}`}
    >
      {ask.anchor === null ? null : (
        <blockquote className="mb-2 border-l-2 border-emerald-400 pl-3 text-emerald-800 dark:text-emerald-300">
          {ask.anchor.quote}
        </blockquote>
      )}
      <p className="font-medium text-slate-950 dark:text-slate-100">{ask.question}</p>
      <p className="mt-2">
        <span className="font-semibold">{answer.user}</span> answered
        {answer.selected.length > 0 ? `: ${answer.selected.join(", ")}` : ""}
      </p>
      {answer.text === null || answer.text === "" ? null : (
        <p className="mt-1 whitespace-pre-wrap">{answer.text}</p>
      )}
      <Timestamp
        at={answer.at}
        className="mt-2 block text-xs text-emerald-700 dark:text-emerald-400"
      />
    </article>
  );
}

export function AskCard({
  ask,
  answerAsk: answer = answerAsk,
  createReply: reply,
  getAskThread: getThread,
}: AskCardProps): ReactNode {
  const queryClient = useQueryClient();
  // The same open anchored ask can render simultaneously in more than one place (the issue
  // board and the margin both show it) - useId keeps this instance's answer field id/label
  // pairing unique across those mounts, instead of colliding on a shared ask.id-derived id.
  const answerFieldId = `${useId()}-answer`;
  const [selected, setSelected] = useState<string[]>([]);
  const [answerText, setAnswerText] = useState("");
  const [justAnswered, setJustAnswered] = useState<Ask | null>(null);
  const submitGuard = useSubmitGuard();
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
      void queryClient.invalidateQueries({ queryKey: ["issue", ask.issue_key] });
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const text = answerText.trim();
    submitGuard.guard(() => mutation.mutate(text === "" ? { selected } : { selected, text }));
  };
  const canSubmit = selected.length > 0 || answerText.trim() !== "";
  const tmuxTarget = ask.author.kind === "session" ? ask.author.origin?.tmux : undefined;
  const answered = justAnswered ?? (ask.state === "answered" ? ask : null);

  if (answered !== null && answered.answer !== null) {
    return (
      <>
        <AnsweredAsk ask={{ ...answered, answer: answered.answer }} />
        <AskThread ask={ask} createReply={reply} getAskThread={getThread} />
      </>
    );
  }

  return (
    <>
      <article
        className="rounded-xl border border-slate-200 bg-white p-4 shadow-sm dark:border-slate-800 dark:bg-slate-900"
        data-testid={`ask-${ask.id}`}
      >
        {ask.anchor === null ? null : (
          <blockquote className="mb-3 border-l-2 border-sky-400 pl-3 text-sm text-slate-600 dark:text-slate-400">
            {ask.anchor.quote}
          </blockquote>
        )}
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <p className="font-medium text-slate-950 dark:text-slate-100">{ask.question}</p>
            <p className="mt-1 text-sm text-slate-500 dark:text-slate-400">
              {actorLabel(ask.author)} · <Timestamp at={ask.created_at} />
            </p>
          </div>
          <span
            className={`rounded-full px-2.5 py-1 text-xs font-medium ${URGENCY_STYLES[ask.urgency]}`}
          >
            {URGENCY_LABELS[ask.urgency]}
          </span>
        </div>
        {tmuxTarget === undefined ? null : (
          <button
            className="mt-3 text-sm font-medium text-sky-700 hover:text-sky-900 dark:text-sky-400 dark:hover:text-sky-300"
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
                    className="flex cursor-pointer items-start gap-3 rounded-lg border border-slate-200 px-3 py-2 hover:border-sky-400 dark:border-slate-700"
                    key={option.label}
                  >
                    <input
                      checked={checked}
                      disabled={mutation.isPending}
                      name={`ask-${ask.id}`}
                      onChange={() => {
                        setSelected((current) => {
                          if (!ask.multiple) {
                            return [option.label];
                          }
                          return current.includes(option.label)
                            ? current.filter((label) => label !== option.label)
                            : [...current, option.label];
                        });
                      }}
                      type={ask.multiple ? "checkbox" : "radio"}
                    />
                    <span>
                      <span className="font-medium text-slate-800 dark:text-slate-200">
                        {option.label}
                      </span>
                      {option.description === undefined ? null : (
                        <span className="mt-0.5 block text-sm text-slate-500 dark:text-slate-400">
                          {option.description}
                        </span>
                      )}
                    </span>
                  </label>
                );
              })}
            </fieldset>
          )}
          <label
            className="block text-sm font-medium text-slate-700 dark:text-slate-300"
            htmlFor={answerFieldId}
          >
            Your answer
            <textarea
              className="mt-1 block w-full rounded-lg border border-slate-300 bg-white px-3 py-2 font-normal outline-none focus:border-sky-500 dark:border-slate-700 dark:bg-slate-950"
              disabled={mutation.isPending}
              id={answerFieldId}
              onChange={(event) => setAnswerText(event.target.value)}
              value={answerText}
            />
          </label>
          <button
            className="rounded-lg bg-sky-600 px-3 py-2 text-sm font-semibold text-white enabled:hover:bg-sky-700 disabled:cursor-not-allowed disabled:bg-slate-300"
            disabled={!canSubmit || mutation.isPending}
            type="submit"
          >
            {mutation.isPending ? "Submitting…" : "Submit answer"}
          </button>
          {mutation.isError ? (
            <QueryError
              message="Could not save your answer."
              onRetry={() => submitGuard.retryLast(mutation)}
              retrying={mutation.isPending}
            />
          ) : null}
        </form>
      </article>
      <AskThread ask={ask} createReply={reply} getAskThread={getThread} />
    </>
  );
}
