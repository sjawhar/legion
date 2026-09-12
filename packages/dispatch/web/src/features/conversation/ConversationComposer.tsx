import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  type SyntheticEvent,
  useEffect,
  useMemo,
  useState,
} from "react";

import { ApiError, api } from "../../api/client";
import type { Agent } from "../../api/types";
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
import { type Recipient, RecipientPicker, recipientForRoute } from "./RecipientPicker";

export function ConversationComposer({
  agents = [],
  envoyError,
  issueKey,
  onPickerOpenChange,
  onSent,
  recipientSlot,
  route,
}: {
  agents?: readonly Agent[];
  envoyError?: string;
  issueKey: string;
  onPickerOpenChange?: (open: boolean) => void;
  onSent: () => void;
  recipientSlot?: ReactNode;
  route?: string | null;
}): ReactNode {
  const queryClient = useQueryClient();
  const [body, setBody] = useState("");
  const [recipient, setRecipient] = useState<Recipient | null>(null);
  const [delivery, setDelivery] = useState<"btw" | "aside" | "steer">("steer");
  const defaultRecipient = useMemo(
    () =>
      recipientForRoute(agents, route) ??
      (route === null || route === undefined
        ? undefined
        : { target: route, title: route, capabilities: [], detail: "Envoy unavailable" }),
    [agents, route]
  );
  useEffect(() => {
    if (
      defaultRecipient !== undefined &&
      (recipient === null ||
        (recipient.target === defaultRecipient.target &&
          recipient.detail === "Envoy unavailable" &&
          defaultRecipient.detail !== "Envoy unavailable"))
    ) {
      setRecipient(defaultRecipient);
      setDelivery(defaultRecipient.capabilities.includes("btw") ? "btw" : "steer");
    }
  }, [defaultRecipient, recipient]);
  const guard = useSubmitGuard();
  const mutation = useMutation({
    mutationFn: (text: string) =>
      api.createMessage(issueKey, {
        body: text,
        ...(recipient === null ? {} : { target: recipient.target, delivery }),
      }),
    onSuccess: () => {
      setBody("");
      void queryClient.invalidateQueries({ queryKey: ["events", issueKey] });
      onSent();
    },
    onSettled: guard.release,
  });
  const canSubmit = body.trim() !== "" && !mutation.isPending;
  const submit = () => {
    if (!canSubmit) return;
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
      className={`sticky top-0 z-20 -mx-2 border-b px-2 pt-2 pb-2 ${borderDefault} ${surfaceBg}`}
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
      <div className="mt-2 flex flex-wrap items-center gap-2">
        {recipientSlot ?? (
          <RecipientPicker
            agents={agents}
            onChange={(next) => {
              setRecipient(next);
              setDelivery(next.capabilities.includes("btw") ? "btw" : "steer");
            }}
            onOpenChange={(open) => onPickerOpenChange?.(open)}
            value={recipient}
          />
        )}
        {recipient === null ? null : (
          <fieldset className="flex min-h-11 rounded-lg border p-0.5">
            <legend className="sr-only">Delivery mode</legend>
            {(["btw", "aside", "steer"] as const).map((mode) => {
              const supported = mode === "steer" || recipient.capabilities.includes(mode);
              const label = mode === "btw" ? "BTW" : mode === "aside" ? "Aside" : "Steer";
              return (
                <button
                  aria-pressed={delivery === mode}
                  className={`min-h-10 rounded px-3 text-sm font-medium ${
                    delivery === mode ? primaryButtonBg : textMutedOnSurface
                  } disabled:cursor-not-allowed disabled:opacity-50`}
                  disabled={!supported || mutation.isPending}
                  key={mode}
                  onClick={() => setDelivery(mode)}
                  title={supported ? undefined : `${recipient.title} does not advertise ${label}`}
                  type="button"
                >
                  {label}
                </button>
              );
            })}
          </fieldset>
        )}
        <button
          className={`ml-auto min-h-11 rounded-lg px-4 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
          disabled={!canSubmit}
          type="submit"
        >
          {mutation.isPending ? "Sending…" : "Send"}
        </button>
      </div>
      <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
        Enter to send · Shift+Enter for a new line
      </p>
      {envoyError === undefined ? null : (
        <p className={`mt-2 text-sm ${dangerText}`}>Envoy unreachable: {envoyError}</p>
      )}
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
