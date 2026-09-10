import { useMutation, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type { AnswerAskInput, Ask } from "../../api/types";
import { actorLabel } from "../refs/actor";
import { Timestamp } from "../refs/Timestamp";

const answerAsk = (id: string, input: AnswerAskInput): Promise<Ask> => api.answerAsk(id, input);

export interface AskCardProps {
  ask: Ask;
  answerAsk?: (id: string, input: AnswerAskInput) => Promise<Ask>;
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

export function AskCard({ ask, answerAsk: answer = answerAsk }: AskCardProps): ReactNode {
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<string[]>([]);
  const [answerText, setAnswerText] = useState("");
  const [optimisticallyAnswered, setOptimisticallyAnswered] = useState(false);
  const mutation = useMutation({
    mutationFn: (input: AnswerAskInput) => answer(ask.id, input),
    onMutate: async () => {
      await queryClient.cancelQueries({ queryKey: ["inbox"] });
      const previous = queryClient.getQueryData<Ask[]>(["inbox"]);
      setOptimisticallyAnswered(true);
      queryClient.setQueryData<Ask[]>(["inbox"], (current) =>
        current?.filter((currentAsk) => currentAsk.id !== ask.id)
      );
      return previous;
    },
    onError: (_error, _input, previous) => {
      setOptimisticallyAnswered(false);
      queryClient.setQueryData(["inbox"], previous);
      void queryClient.invalidateQueries({ queryKey: ["inbox"] });
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["inbox"] });
      void queryClient.invalidateQueries({ queryKey: ["issue", ask.issue_key] });
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const text = answerText.trim();
    mutation.mutate(text === "" ? { selected } : { selected, text });
  };
  const canSubmit = selected.length > 0 || answerText.trim() !== "";
  const tmuxTarget = ask.author.kind === "session" ? ask.author.origin?.tmux : undefined;

  if (ask.state === "answered" || optimisticallyAnswered) {
    return (
      <article className="rounded-xl border border-emerald-200 bg-emerald-50 p-4 text-sm text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950 dark:text-emerald-300">
        Answer recorded.
      </article>
    );
  }

  return (
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
          htmlFor={`ask-${ask.id}-answer`}
        >
          Your answer
          <textarea
            className="mt-1 block w-full rounded-lg border border-slate-300 bg-white px-3 py-2 font-normal outline-none focus:border-sky-500 dark:border-slate-700 dark:bg-slate-950"
            id={`ask-${ask.id}-answer`}
            onChange={(event) => setAnswerText(event.target.value)}
            value={answerText}
          />
        </label>
        <button
          className="rounded-lg bg-sky-600 px-3 py-2 text-sm font-semibold text-white enabled:hover:bg-sky-700 disabled:cursor-not-allowed disabled:bg-slate-300"
          disabled={!canSubmit || mutation.isPending}
          type="submit"
        >
          Submit answer
        </button>
        {mutation.isError ? (
          <p className="text-sm text-rose-700 dark:text-rose-400">Could not save your answer.</p>
        ) : null}
      </form>
    </article>
  );
}
