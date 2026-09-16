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
import { ReplyQuote, replyQuoteText } from "./ReplyQuote";

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
  readonly in_reply_to?: string;
  readonly target: string;
};

/** The message a composer in reply mode answers. `thread` is set when the thread's root was
 *  targeted: the reply then goes to that session the way the thread was last delivered, unless
 *  the picker is changed. */
export interface ReplyTarget {
  readonly author: string;
  readonly excerpt: string;
  readonly id: string;
  readonly thread?: {
    readonly delivery: MessageDeliveryMode;
    readonly target: string;
    readonly title: string;
  };
  /** Deep link to the parent's turn, when it has one. */
  readonly to?: string;
}

interface ConversationComposerCommonProps {
  readonly agents?: readonly Agent[];
  readonly defaultDelivery?: "btw" | "steer";
  readonly embedded?: boolean;
  readonly envoyError?: string;
  readonly onCancelReply?: () => void;
  readonly onPickerOpenChange?: (open: boolean) => void;
  readonly onSent: () => void;
  readonly recipientSlot?: ReactNode;
  readonly replyTo?: ReplyTarget | null;
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
    onCancelReply,
    onPickerOpenChange,
    onSent,
    recipientSlot,
    replyTo = null,
    route,
  } = props;
  const textarea = useRef<HTMLTextAreaElement>(null);
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
  // Entering reply mode focuses the field and, on a targeted thread, adopts the thread's
  // recipient and delivery mode. Leaving it - or moving to a reply on a plain message - returns
  // the composer to its own default, so an inherited recipient never outlives its thread.
  const previousReply = useRef<string | undefined>(undefined);
  useEffect(() => {
    const replyId = replyTo?.id;
    if (previousReply.current === replyId) {
      return;
    }
    const wasReplying = previousReply.current !== undefined;
    previousReply.current = replyId;
    if (replyTo !== null) {
      textarea.current?.focus();
    }
    const thread = replyTo?.thread;
    if (thread === undefined) {
      if (wasReplying) {
        setRecipient(defaultRecipient ?? null);
        setDelivery(
          defaultRecipient !== undefined &&
            defaultDelivery !== "steer" &&
            defaultRecipient.capabilities.includes("btw")
            ? "btw"
            : "steer"
        );
      }
      return;
    }
    const next = recipientForRoute(agents, thread.target) ?? {
      target: thread.target,
      title: thread.title,
      capabilities: [],
      detail: "Envoy unavailable",
    };
    setRecipient(next);
    setDelivery(
      thread.delivery === "steer" || next.capabilities.includes(thread.delivery)
        ? thread.delivery
        : "steer"
    );
  }, [agents, defaultDelivery, defaultRecipient, replyTo]);
  const guard = useSubmitGuard();
  const mutation = useMutation({
    mutationFn: (text: string) => {
      const inReplyTo = replyTo === null ? {} : { in_reply_to: replyTo.id };
      if (props.onSend !== undefined) {
        if (recipient === null) {
          throw new Error("targeted message composer requires a recipient");
        }
        return props.onSend({ body: text, delivery, target: recipient.target, ...inReplyTo });
      }
      const input: CreateMessageInput = {
        body: text,
        ...inReplyTo,
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
      {replyTo === null ? null : (
        <div className="mb-2 flex items-center gap-1">
          <ReplyQuote className="min-w-0 flex-1" to={replyTo.to}>
            {replyQuoteText(replyTo.author, replyTo.excerpt)}
          </ReplyQuote>
          <button
            aria-label="Cancel reply"
            className={`inline-flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg text-lg leading-none md:min-h-8 md:min-w-8 ${textMutedOnSurface}`}
            onClick={onCancelReply}
            title="Cancel reply"
            type="button"
          >
            ×
          </button>
        </div>
      )}
      <textarea
        aria-label="Message"
        className={`min-h-11 w-full resize-none rounded-lg border px-3 py-2 leading-5 ${inputClasses(false)} ${textPrimaryOnSurface}`}
        disabled={mutation.isPending}
        onChange={(event) => setBody(event.target.value)}
        onInput={resize}
        onKeyDown={(event) => {
          if (event.key === "Escape" && replyTo !== null) {
            event.preventDefault();
            onCancelReply?.();
            return;
          }
          submitOnModifiedEnter(event);
        }}
        ref={textarea}
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
