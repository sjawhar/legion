import type { MessageDelivery } from "../../api/types";

/**
 * The fields the server writes on every delivery attempt that a fixture has no value of its own
 * for: whether the session took the message as its user's own turn and when it said so
 * (`accepted_as`, `accepted_at`), whether the listener already held the message (`duplicate`),
 * and who asked for the attempt (`requested_by`, null for the send that made the message).
 *
 * Spread it first and let the fixture's own fields win; a fixture that names one of these keeps
 * its own value.
 */
export function messageDeliveryFields(): Pick<
  MessageDelivery,
  "accepted_as" | "accepted_at" | "duplicate" | "requested_by"
> {
  return { accepted_as: null, accepted_at: null, duplicate: false, requested_by: null };
}
