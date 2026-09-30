import type { MessageDelivery } from "../../api/types";
import type { TargetedMessageAttempt } from "../conversation/TargetedMessageCard";

/** A message's delivery attempts as `TargetedMessageCard` shows them, all aimed at one
 *  session. Shared by an agent card's conversation and by a broadcast's recipient list, so a
 *  delivery reads the same wherever it is shown. */
export function deliveryAttempts(
  deliveries: readonly MessageDelivery[],
  targetName: string
): TargetedMessageAttempt[] {
  return deliveries.map((attempt) => ({
    acceptedAs: attempt.accepted_as ?? null,
    attempt: attempt.attempt,
    createdAt: attempt.created_at,
    delivery: attempt.delivery,
    duplicate: attempt.duplicate,
    error: attempt.error,
    state: attempt.state,
    targetName,
  }));
}
