import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  type SyntheticEvent,
  useState,
} from "react";

import { ApiError, api } from "../../api/client";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  borderDefault,
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  surfaceBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";

export function ConversationComposer({
  issueKey,
  onSent,
  recipientSlot,
}: {
  issueKey: string;
  onSent: () => void;
  recipientSlot?: ReactNode;
}): ReactNode {
  const queryClient = useQueryClient();
  const [body, setBody] = useState("");
  const guard = useSubmitGuard();
  const mutation = useMutation({
    mutationFn: (text: string) => api.createMessage(issueKey, { body: text }),
    onSuccess: () => {
      setBody("");
      void queryClient.invalidateQueries({ queryKey: ["events", issueKey] });
      onSent();
    },
    onSettled: () => {
      guard.release();
    },
  });
  const canSubmit = body.trim() !== "" && !mutation.isPending;

  const submit = () => {
    if (!canSubmit) {
      return;
    }
    guard.guard(() => mutation.mutate(body));
  };

  const resize = (event: SyntheticEvent<HTMLTextAreaElement>) => {
    const textarea = event.currentTarget;
    textarea.style.height = "auto";
    const lineHeight = Number.parseFloat(window.getComputedStyle(textarea).lineHeight);
    textarea.style.height = `${Math.min(textarea.scrollHeight, 8 * lineHeight)}px`;
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
      event.preventDefault();
      submit();
    }
  };

  const onSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    submit();
  };
  const errorMessage =
    mutation.error instanceof ApiError ? mutation.error.message : "network error";

  return (
    <form
      aria-label="Message composer"
      className={`sticky bottom-16 z-10 -mx-2 mt-auto border-t px-2 pt-3 pb-3 xl:bottom-0 ${borderDefault} ${surfaceBg}`}
      onSubmit={onSubmit}
    >
      <textarea
        aria-label="Message"
        className={`min-h-11 w-full resize-none rounded-lg border px-3 py-2 leading-5 ${inputClasses(false)} ${textPrimaryOnSurface}`}
        disabled={mutation.isPending}
        onChange={(event) => setBody(event.target.value)}
        onInput={resize}
        onKeyDown={onKeyDown}
        rows={1}
        value={body}
      />
      <div className="mt-2 flex min-h-11 items-center justify-between gap-3">
        <div className="min-w-0">{recipientSlot}</div>
        <button
          className={`min-h-11 rounded-lg px-4 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
          disabled={!canSubmit}
          type="submit"
        >
          {mutation.isPending ? "Sending…" : "Send"}
        </button>
      </div>
      <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
        Enter to send · Shift+Enter for a new line
      </p>
      {mutation.isError ? (
        <div className={`mt-2 flex items-center gap-3 ${dangerText}`} role="alert">
          <span>Couldn&apos;t send — {errorMessage}</span>
          <button
            className="font-medium underline disabled:cursor-not-allowed disabled:opacity-50"
            disabled={mutation.isPending}
            onClick={() => guard.retryLast(mutation)}
            type="button"
          >
            Retry
          </button>
        </div>
      ) : null}
    </form>
  );
}
