import type { MessageDelivery } from "../../api/types";
import { deliveryMode, deliveryOutcomeState, rowAnsweredWithError } from "../conversation/delivery";
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
    // The route table carries `accepted_as` as a plain string; `user_turn` is the one word that
    // says the session took the message as its user's own turn, and no other reads as one.
    acceptedAs: attempt.accepted_as === "user_turn" ? "user_turn" : null,
    attempt: attempt.attempt,
    answeredWithError: rowAnsweredWithError(attempt),
    createdAt: attempt.created_at,
    // A mode the route table leaves a plain string, narrowed to the closed list the picker
    // offers; Dispatch writes no other, and the card's own default for a missing mode is Send.
    delivery: deliveryMode(attempt.delivery) ?? "steer",
    duplicate: attempt.duplicate,
    error: attempt.error,
    state: deliveryOutcomeState(attempt.state),
    targetName:
      attempt.session_id === ""
        ? targetName
        : { sessionId: attempt.session_id, storedTitle: undefined },
  }));
}
