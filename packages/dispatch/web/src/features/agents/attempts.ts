import type { MessageDelivery } from "../../api/types";
import { rowAnsweredWithError } from "../conversation/delivery";
import type { TargetedMessageAttempt } from "../conversation/TargetedMessageCard";
import type { AgentPart } from "../refs/AgentName";

/** A message's delivery attempts as `TargetedMessageCard` shows them. Each names the session it
 *  reached (an agent part, drawn by `AgentName`), so an earlier attempt keeps naming that session
 *  after a role hands off; an attempt that recorded none names `targetName`. Shared by an agent
 *  card's conversation and by a broadcast's recipient list, so a delivery reads the same
 *  wherever it is shown. */
export function deliveryAttempts(
  deliveries: readonly MessageDelivery[],
  targetName: AgentPart
): TargetedMessageAttempt[] {
  return deliveries.map((attempt) => ({
    acceptedAs: attempt.accepted_as ?? null,
    attempt: attempt.attempt,
    answeredWithError: rowAnsweredWithError(attempt),
    createdAt: attempt.created_at,
    delivery: attempt.delivery,
    duplicate: attempt.duplicate,
    error: attempt.error,
    state: attempt.state,
    targetName:
      attempt.session_id === ""
        ? targetName
        : { sessionId: attempt.session_id, storedTitle: undefined },
  }));
}
