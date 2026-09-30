import type { Message, MessageDelivery } from "../../api/types";
import type { TargetedMessageAttempt } from "../conversation/TargetedMessageCard";

/** A message's delivery attempts as `TargetedMessageCard` shows them, all aimed at one
 *  session. Shared by an agent card's conversation and by a broadcast's recipient list, so a
 *  delivery reads the same wherever it is shown. */
export function deliveryAttempts(
  deliveries: readonly MessageDelivery[],
  targetName: string
): TargetedMessageAttempt[] {
  return deliveries.map((attempt) => ({
    attempt: attempt.attempt,
    createdAt: attempt.created_at,
    delivery: attempt.delivery,
    duplicate: attempt.duplicate,
    error: attempt.error,
    state: attempt.state,
    targetName,
  }));
}

/**
 * Whether `message`, in the thread `root` opens, reached `sessionID` as the person's own user
 * turn rather than as a card: a person's Send or Aside with no issue, in a conversation aimed at
 * that session that no broadcast sent, whose latest attempt was sent. These are the facts an Oh My
 * Pi session confirms with Dispatch before it takes one, and it then answers in its own
 * conversation rather than with a Dispatch reply. A Dispatch too old to say whether a broadcast
 * sent the message is not taken as saying no.
 */
export function sentAsUserTurn(message: Message, root: Message, sessionID: string): boolean {
  const latest = message.deliveries.at(-1);
  return (
    message.author.kind === "user" &&
    message.issue_key === null &&
    message.broadcast_id === null &&
    root.broadcast_id === null &&
    root.target === `session:${sessionID}` &&
    latest?.state === "sent" &&
    (latest.delivery === "aside" || latest.delivery === "steer")
  );
}
