import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  type FormEvent,
  type ReactNode,
  type SyntheticEvent,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { ApiError, api } from "../../api/client";
import type { Agent, CreateMessageInput, Message, MessageDeliveryMode } from "../../api/types";
import { submitOnModifiedEnter } from "../../hooks/submitOnModifiedEnter";
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

type DeliveryMode = "btw" | "aside" | "steer";

function DeliveryModeControl({
  delivery,
  disabled,
  onChange,
  recipient,
}: {
  delivery: DeliveryMode;
  disabled: boolean;
  onChange: (delivery: DeliveryMode) => void;
  recipient: Recipient;
}): ReactNode {
  return (
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
            disabled={!supported || disabled}
            key={mode}
            onClick={() => onChange(mode)}
            title={supported ? undefined : `${recipient.title} does not advertise ${label}`}
            type="button"
          >
            {label}
          </button>
        );
      })}
    </fieldset>
  );
}

type TargetedMessageInput = {
  readonly body: string;
  readonly delivery: MessageDeliveryMode;
  readonly target: string;
};

interface ConversationComposerCommonProps {
  readonly agents?: readonly Agent[];
  readonly defaultDelivery?: "btw" | "steer";
  readonly embedded?: boolean;
  readonly envoyError?: string;
  readonly onPickerOpenChange?: (open: boolean) => void;
  readonly onSent: () => void;
  readonly recipientSlot?: ReactNode;
  readonly route?: string | null;
}

type ConversationComposerProps = ConversationComposerCommonProps &
  (
    | { readonly issueKey: string; readonly onSend?: never }
    | {
        readonly issueKey?: never;
        readonly onSend: (input: TargetedMessageInput) => Promise<Message>;
      }
  );

export function ConversationComposer(props: ConversationComposerProps): ReactNode {
  const {
    agents = [],
    defaultDelivery,
    embedded = false,
    envoyError,
    onPickerOpenChange,
    onSent,
    recipientSlot,
    route,
  } = props;
  const queryClient = useQueryClient();
  const [body, setBody] = useState("");
  const [recipient, setRecipient] = useState<Recipient | null>(null);
  const [delivery, setDelivery] = useState<DeliveryMode>("steer");
  const [pickerOpen, setPickerOpen] = useState(false);
  const defaultRecipient = useMemo(
    () =>
      recipientForRoute(agents, route) ??
      (route === null || route === undefined
        ? undefined
        : { target: route, title: route, capabilities: [], detail: "Envoy unavailable" }),
    [agents, route]
  );
  const previousDefaultDelivery = useRef(defaultDelivery);
  useEffect(() => {
    const deliveryChanged = previousDefaultDelivery.current !== defaultDelivery;
    previousDefaultDelivery.current = defaultDelivery;
    if (
      defaultRecipient !== undefined &&
      (recipient === null ||
        deliveryChanged ||
        (recipient.target === defaultRecipient.target &&
          recipient.detail === "Envoy unavailable" &&
          defaultRecipient.detail !== "Envoy unavailable"))
    ) {
      setRecipient(defaultRecipient);
      setDelivery(
        defaultDelivery === "steer" || !defaultRecipient.capabilities.includes("btw")
          ? "steer"
          : "btw"
      );
    }
  }, [defaultDelivery, defaultRecipient, recipient]);
  const guard = useSubmitGuard();
  const mutation = useMutation({
    mutationFn: (text: string) => {
      if (props.onSend !== undefined) {
        if (recipient === null) {
          throw new Error("targeted message composer requires a recipient");
        }
        return props.onSend({ body: text, delivery, target: recipient.target });
      }
      const input: CreateMessageInput = {
        body: text,
        ...(recipient === null ? {} : { target: recipient.target, delivery }),
      };
      return api.createMessage(props.issueKey, input);
    },
    onSuccess: () => {
      setBody("");
      if (props.onSend === undefined) {
        void queryClient.invalidateQueries({ queryKey: ["events", props.issueKey] });
      }
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
  const onSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    submit();
  };
  const errorMessage =
    mutation.error instanceof ApiError ? mutation.error.message : "network error";

  return (
    <form
      aria-label="Message composer"
      className={
        embedded
          ? `border-t pt-3 ${borderDefault}`
          : `${pickerOpen ? "z-20" : "z-[8]"} fixed inset-x-0 bottom-16 border-t px-4 py-2 sm:sticky sm:top-0 sm:z-20 sm:-mx-2 sm:border-b sm:px-2 ${borderDefault} ${surfaceBg}`
      }
      onSubmit={onSubmit}
    >
      <textarea
        aria-label="Message"
        className={`min-h-11 w-full resize-none rounded-lg border px-3 py-2 leading-5 ${inputClasses(false)} ${textPrimaryOnSurface}`}
        disabled={mutation.isPending}
        onChange={(event) => setBody(event.target.value)}
        onInput={resize}
        onKeyDown={(event) => submitOnModifiedEnter(event)}
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
            onOpenChange={(open) => {
              setPickerOpen(open);
              onPickerOpenChange?.(open);
            }}
            value={recipient}
          />
        )}
        {recipient === null ? null : (
          <DeliveryModeControl
            delivery={delivery}
            disabled={mutation.isPending}
            onChange={setDelivery}
            recipient={recipient}
          />
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
        Ctrl/Cmd+Enter to send · Enter for a new line
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
