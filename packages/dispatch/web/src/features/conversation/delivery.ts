import { DELIVERY_DUPLICATE_WINDOW_MS, RECEIPT_TIMEOUT_CAUSE } from "@legion/contracts";

/**
 * The one rule every delivery surface applies, so the dashboard cannot promise on one card what
 * it contradicts on another. Four surfaces render a delivery attempt — the targeted-message
 * card, the comment thread's "Mention deliveries" list, the issue event feed, and the broadcast
 * page's row for a send nobody is carrying — and each of them asks these functions rather than
 * spelling the condition or the sentence again. What the promise rests on is stated once, on
 * `DELIVERY_DUPLICATE_WINDOW_MS` in `@legion/contracts`.
 */

/** The shape every delivery attempt shares, whatever its row type spells the fields. */
export interface DeliveryOutcome {
  readonly state: "pending" | "sent" | "failed";
  /** ISO-8601, as both `MessageDelivery.created_at` and `CommentDelivery.created_at` carry it. */
  readonly createdAt: string;
  readonly duplicate?: boolean;
  /** The session the attempt went to answered it with an error instead of a reply (a BTW whose
   *  side turn failed, a frame the host refused), as `rowAnsweredWithError` or
   *  `receiptAnsweredWithError` reads it off the attempt. Absent means it did not. */
  readonly answeredWithError?: boolean;
}

/**
 * Whether an attempt row records its session's error answer. Dispatch writes an envelope id only
 * for a send the listener accepted (`sendResolvedDelivery`), and every failure Dispatch records
 * itself leaves it null, so a failed row that carries one was failed by the session's own error
 * reply after Dispatch recorded the send. A session that refuses the frame before Dispatch records
 * the send leaves a row no different from a failed send, which a row cannot tell apart; the
 * receipt can (`receiptAnsweredWithError`).
 */
export function rowAnsweredWithError(row: {
  readonly state: string;
  readonly envelope_id: string | null;
}): boolean {
  return row.state === "failed" && row.envelope_id !== null;
}

/**
 * Whether a delivery receipt records its session's error answer: the session's error reply
 * appends the attempt's receipt itself, as the session the attempt went to, whichever of it and
 * Dispatch's own record of the send landed first. Every other receipt is appended by whoever sent
 * the attempt.
 */
export function receiptAnsweredWithError(receipt: {
  readonly actor: { readonly kind: string; readonly id: string };
  readonly payload: { readonly state: string; readonly session_id: string | null };
}): boolean {
  return (
    receipt.payload.state === "failed" &&
    receipt.actor.kind === "session" &&
    receipt.actor.id === receipt.payload.session_id
  );
}

/**
 * The one case the promise misses inside the window: a host holds the keys it was handed in
 * memory, so one that restarted after the first send (`claude --resume`, a resumed pod) no
 * longer recognises its repeat.
 */
const unlessRestarted = "unless the session restarted since it was sent";

/**
 * Whether a send made at `createdAt` can still be recognised as a repeat, so re-sending it in its
 * own mode cannot deliver it twice.
 *
 * A NEGATIVE age is not rejected. `createdAt` is stamped by Postgres and `now` is the browser's
 * clock; nothing reconciles them, so a client running behind the server sees a just-failed
 * attempt as dated in the future. That attempt is the youngest there is and certainly inside the
 * window — and because this decides whether the same-mode Retry renders at all, rejecting it
 * would hide the safe action and leave only the mode-change buttons, which are a genuine second
 * delivery. `Number.isFinite` still rejects an unparseable stamp.
 */
function insideDuplicateWindow(createdAt: string, now: number): boolean {
  const age = now - Date.parse(createdAt);
  return Number.isFinite(age) && age < DELIVERY_DUPLICATE_WINDOW_MS;
}

/**
 * Whether re-sending this attempt in its OWN mode can still be promised not to deliver twice.
 *
 * A same-mode retry repeats the dedupe key of the send before it, and a repeat is recognised only
 * inside `DELIVERY_DUPLICATE_WINDOW_MS` (`insideDuplicateWindow`). Past the window nothing
 * remembers the key, and the same retry is a second delivery - the very defect this promise
 * exists to rule out.
 *
 * An attempt already recorded `duplicate` is excluded for a different reason: it reached the
 * listener and changed nothing, so there is no failure left to retry.
 *
 * So is an attempt the session answered with an error, because its Retry could not be promised
 * to arrive. The notification stream stored the first frame under that key, so it stores the
 * Retry no more than any other repeat, and a session the listener pushes to from the stream is
 * never handed it while the attempt reads as a duplicate. The dashboard cannot tell that session
 * from one whose host takes the Retry, so it points at a mode change, a new key, for both
 * (`answeredWithErrorGuidance`).
 */
export function isSafeRetry(attempt: DeliveryOutcome, now: number = Date.now()): boolean {
  return (
    attempt.state === "failed" &&
    attempt.duplicate !== true &&
    attempt.answeredWithError !== true &&
    insideDuplicateWindow(attempt.createdAt, now)
  );
}

/** What a duplicate attempt reads as: an earlier attempt landed, so this one added nothing. */
export const duplicateText = `Delivered by an earlier attempt, so not delivered again ${unlessRestarted}`;

/**
 * The guidance a failed attempt earns while a same-mode retry is still safe.
 *
 * "Retry won't deliver it twice" holds for any in-window failure but a restarted session's: if
 * the send landed, its repeat is recognised and dropped, and if it never landed there is nothing
 * to duplicate. The second clause does not. "Sending in a different mode delivers it again" is
 * only true of a send that may already have reached the recipient — a receipt timeout — so it is
 * keyed on that cause. It is also withheld on the mention surface, which has no mode-change
 * button to name.
 */
export function safeRetryGuidance(surface: "card" | "mention", cause?: string | null): string {
  const safe = `Retry won't deliver it twice ${unlessRestarted}`;
  if (surface === "mention" || cause !== RECEIPT_TIMEOUT_CAUSE) return `${safe}.`;
  return `${safe}; sending in a different mode delivers it again.`;
}

/**
 * The guidance an attempt the session answered with an error earns in place of a Retry: send it
 * again under a new key. On a card that is one of the mode-change actions beside it; the mention
 * surface has none, so a new comment is its new key.
 */
export function answeredWithErrorGuidance(surface: "card" | "mention"): string {
  const answered = "The session answered with an error";
  return surface === "card"
    ? `${answered}; send it in another mode instead.`
    : `${answered}; mention it in a new comment to send it again.`;
}

/**
 * What the broadcast page says under the Retry of a send nobody is carrying: a pending attempt
 * whose claim lapsed, made at `createdAt`, or a recipient no attempt was recorded for at all
 * (`createdAt` undefined), which has nothing to repeat. The Retry re-sends in `mode`, the
 * attempt's own, so the promise holds only while its repeat is still recognised.
 */
export function strandedRetryGuidance(
  mode: string,
  createdAt: string | undefined,
  now: number = Date.now()
): string {
  const retry = `Nobody is carrying this send. Retry uses ${mode} again`;
  if (createdAt === undefined || insideDuplicateWindow(createdAt, now)) {
    return `${retry}, which cannot deliver it twice ${unlessRestarted}.`;
  }
  return `${retry}, and may deliver it twice: the send is older than the window in which a repeat is recognised.`;
}

/**
 * Join a recorded cause to the guidance that follows it. Only the receipt-timeout cause ends in
 * a sentence, so joining with a bare space would run every other cause into the guidance
 * ("Failed: no live session Retry won't deliver it twice.").
 */
export function withGuidance(cause: string, guidance: string): string {
  return /[.!?]$/.test(cause) ? `${cause} ${guidance}` : `${cause}. ${guidance}`;
}
