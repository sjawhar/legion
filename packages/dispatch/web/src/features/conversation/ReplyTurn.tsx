import type { ReactNode } from "react";

import { secondaryButtonBorder, textSecondaryOnSurface } from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { Avatar } from "./Avatar";
import type { Author } from "./authors";
import { ReplyButton } from "./ReplyButton";
import { ReplyQuote } from "./ReplyQuote";
import { DeliveryRetry, DeliveryStatus, type TargetedMessageAttempt } from "./TargetedMessageCard";

/** A turn's action icons - copy-reference, Reply, Pin - beside its body: a row on desktop, a
 *  column on the phone so three 44 px targets do not squeeze the text to a sliver. The column
 *  is reversed so the last icon (Pin) keeps the turn's top-right corner on both layouts. */
export function TurnActions({ children }: { children: ReactNode }): ReactNode {
  return (
    <div className="flex shrink-0 flex-col-reverse items-center self-start sm:flex-row">
      {children}
    </div>
  );
}

/** The replies beneath a thread's root, oldest first, indented under a rule so the exchange
 *  reads as one thread. */
export function ThreadReplies({ children }: { children: ReactNode }): ReactNode {
  return (
    <ol
      aria-label="Replies"
      className={`mt-2 ml-4 flex flex-col gap-1 border-l-2 pl-3 sm:ml-8 ${secondaryButtonBorder}`}
    >
      {children}
    </ol>
  );
}

interface ReplyDelivery {
  readonly answeredBy?: string;
  readonly attempts: readonly TargetedMessageAttempt[];
  readonly retry?: {
    readonly canBtw: boolean;
    readonly canSteer: boolean;
    readonly onRetry: (delivery: "btw" | "steer") => void;
    readonly retrying: boolean;
  };
  readonly targetName: string;
}

/** One reply in a thread: author, time, the quoted parent, the body, and - when the reply was
 *  itself delivered to a session - what became of that delivery. `actions` sit beside the Reply
 *  button (the Conversation adds copy-reference). */
export function ReplyTurn({
  actions,
  at,
  author,
  body,
  current = false,
  delivery,
  onReply,
  quote,
  turnID,
}: {
  actions?: ReactNode;
  at: string;
  author: Author;
  body: ReactNode;
  current?: boolean;
  delivery?: ReplyDelivery;
  onReply?: () => void;
  quote?: { readonly text: string; readonly to?: string };
  turnID: string;
}): ReactNode {
  return (
    <li
      aria-current={current ? "true" : undefined}
      className="flex gap-3 rounded-lg py-1"
      data-reply=""
      data-turn={turnID}
    >
      <Avatar author={author} />
      <div className="min-w-0 flex-1">
        <p className={`flex items-baseline gap-2 text-sm ${textSecondaryOnSurface}`}>
          <span className="font-semibold">{author.label}</span>
          <Timestamp at={at} />
        </p>
        {quote === undefined ? null : (
          <ReplyQuote className="mb-1" to={quote.to}>
            {quote.text}
          </ReplyQuote>
        )}
        {body}
        {delivery === undefined ? null : (
          <>
            <DeliveryStatus
              answeredBy={delivery.answeredBy}
              deliveries={delivery.attempts}
              targetName={delivery.targetName}
            />
            {delivery.answeredBy === undefined && delivery.retry !== undefined ? (
              <DeliveryRetry
                canBtw={delivery.retry.canBtw}
                canSteer={delivery.retry.canSteer}
                onRetry={delivery.retry.onRetry}
                retrying={delivery.retry.retrying}
                targetName={delivery.targetName}
              />
            ) : null}
          </>
        )}
      </div>
      <TurnActions>
        {actions}
        {onReply === undefined ? null : <ReplyButton onClick={onReply} />}
      </TurnActions>
    </li>
  );
}
