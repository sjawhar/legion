import { DELIVERY_DUPLICATE_WINDOW_MS, RECEIPT_TIMEOUT_CAUSE } from "@legion/contracts";

/**
 * The one rule every delivery surface applies, so the dashboard cannot promise on one card what
 * it contradicts on another. Three surfaces render a delivery attempt — the targeted-message
 * card, the comment thread's "Mention deliveries" list, and the issue event feed — and each of
 * them asks these functions rather than spelling the condition again.
 */

/** The shape every delivery attempt shares, whatever its row type spells the fields. */
export interface DeliveryOutcome {
  readonly state: "pending" | "sent" | "failed";
  /** ISO-8601, as both `MessageDelivery.created_at` and `CommentDelivery.created_at` carry it. */
  readonly createdAt: string;
  readonly duplicate?: boolean;
}
/**
 * Whether re-sending this attempt in its OWN mode can still be promised not to deliver twice.
 *
 * It can only be promised while the notification stream still recognises the repeat. The key a
 * retry carries is scoped to (message, mode, recipient), so inside the stream's duplicate window
 * a same-mode retry of a send that landed is dropped before the agent's subject sees it. Past
 * that window the stream holds neither the message nor its MsgId, and the same retry publishes a
 * second frame — the very defect this promise exists to rule out.
 *
 * An attempt already recorded `duplicate` is excluded for a different reason: it reached the
 * listener and changed nothing, so there is no failure left to retry.
 *
 * A NEGATIVE age is not rejected. `createdAt` is stamped by Postgres and `now` is the browser's
 * clock; nothing reconciles them, so a client running behind the server sees a just-failed
 * attempt as dated in the future. That attempt is the youngest there is and certainly inside the
 * window — and because this predicate decides whether the same-mode Retry renders at all,
 * rejecting it would hide the safe action and leave only the mode-change buttons, which are a
 * genuine second delivery. `Number.isFinite` still rejects an unparseable stamp.
 */
export function isSafeRetry(attempt: DeliveryOutcome, now: number = Date.now()): boolean {
  if (attempt.state !== "failed" || attempt.duplicate === true) return false;
  const age = now - Date.parse(attempt.createdAt);
  return Number.isFinite(age) && age < DELIVERY_DUPLICATE_WINDOW_MS;
}

/** What a delivered-but-duplicate attempt reads as: it reached the listener and added nothing. */
export const duplicateText =
  "Delivered; the listener already had this message, so it wasn't sent again";

/**
 * The guidance a failed attempt earns while a same-mode retry is still safe.
 *
 * "Retry won't deliver it twice" holds for any in-window failure: if the send landed the stream
 * drops the repeat, and if it never landed there is nothing to duplicate. The second clause does
 * not. "Sending in a different mode delivers it again" is only true of a send that may already
 * have reached the recipient — a receipt timeout — so it is keyed on that cause. It is also
 * withheld on the mention surface, which has no mode-change button to name.
 */
export function safeRetryGuidance(surface: "card" | "mention", cause?: string | null): string {
  const safe = "Retry won't deliver it twice.";
  if (surface === "mention" || cause !== RECEIPT_TIMEOUT_CAUSE) return safe;
  return "Retry won't deliver it twice; sending in a different mode delivers it again.";
}

/**
 * Join a recorded cause to the guidance that follows it. Only the receipt-timeout cause ends in
 * a sentence, so joining with a bare space would run every other cause into the guidance
 * ("Failed: no live session Retry won't deliver it twice.").
 */
export function withGuidance(cause: string, guidance: string): string {
  return /[.!?]$/.test(cause) ? `${cause} ${guidance}` : `${cause}. ${guidance}`;
}
