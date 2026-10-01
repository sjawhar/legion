import type { ReactNode } from "react";

import type { Agent, MessageDeliveryMode } from "../../api/types";
import {
  secondaryButtonBorder,
  secondaryButtonText,
  surfaceMutedHoverBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import {
  answeredWithErrorGuidance,
  duplicateText,
  isSafeRetry,
  MODE_LABELS,
  safeRetryGuidance,
  withGuidance,
} from "./delivery";
import { ReplyButton } from "./ReplyButton";

/** The current live capabilities behind a stored delivery target - a bare session, or a role
 *  re-resolved to whoever holds it now, since a role's holder can change between a recorded
 *  attempt and a retry rendered from it. `undefined` means unknown (no live registration),
 *  which every caller treats as permissive rather than as a reason to disable a control. Every
 *  retry control must resolve through this from the delivery's own stored target - never from
 *  the attempt's recorded session, and never from an enclosing agent/session context, both of
 *  which can go stale after a role hands off. */
export function capabilitiesForTarget(
  target: string | null | undefined,
  agents: readonly Agent[]
): readonly string[] | undefined {
  if (target === null || target === undefined) return undefined;
  if (target.startsWith("session:")) {
    return agents.find((agent) => `session:${agent.session_id}` === target)?.capabilities;
  }
  if (target.startsWith("role:")) {
    const role = target.slice("role:".length);
    return agents.find((agent) => agent.roles.includes(role))?.capabilities;
  }
  return undefined;
}

export interface TargetedMessageAttempt {
  /** The session took this attempt's message as its user's own turn and told Dispatch so: it
   *  answers in its own conversation, so no Dispatch reply is awaited. */
  readonly acceptedAs?: "user_turn" | null;
  readonly attempt: number;
  /** The session answered this attempt with an error (`DeliveryOutcome.answeredWithError`). */
  readonly answeredWithError?: boolean;
  readonly createdAt: string;
  readonly delivery: MessageDeliveryMode;
  /** The listener already held this message, so this attempt reached it and changed nothing. */
  readonly duplicate?: boolean;
  readonly error?: string | null;
  readonly state: "pending" | "sent" | "failed";
  readonly targetName?: string;
}

/** Whether the session took a targeted message as its user's own turn: its latest attempt says
 *  so, as the session recorded it with Dispatch. Such a message has nothing to retry, and that
 *  outranks a `failed` the send recorded afterwards, since the session already said it took it. */
export function takenAsUserTurn(deliveries: readonly TargetedMessageAttempt[]): boolean {
  return deliveries.at(-1)?.acceptedAs === "user_turn";
}

/** The headline one targeted message gets: who answered it, that it reached the session's own
 *  conversation, why it failed, that it is still going out, that it is a BTW waiting on an
 *  answer, or that it was delivered, naming the mode as the composer does (`MODE_LABELS`).
 *  `asking` is that BTW branch, whose line ends in a separator because it carries a timestamp:
 *  the two belong to one decision, so the caller reads it here rather than restating the
 *  condition.
 *
 *  A BTW is tested before `duplicate`: an outstanding BTW is still waiting on its answer even
 *  when the send that carried it changed nothing, so the human must keep seeing that one is
 *  expected. */
function deliveryHeadline(
  answeredBy: string | undefined,
  delivery: TargetedMessageAttempt | undefined,
  targetName: string,
  retryRow: boolean
): { text: string; asking: boolean } {
  if (answeredBy !== undefined) return { text: `Answered by ${answeredBy}`, asking: false };
  if (delivery?.acceptedAs === "user_turn") {
    return {
      text: `Delivered to ${targetName}'s conversation (${MODE_LABELS[delivery.delivery]})`,
      asking: false,
    };
  }
  if (delivery?.state === "failed") {
    const cause = `Failed: ${delivery.error ?? "delivery failed"}`;
    // Each sentence belongs to a button in the retry row, and is said only while that row is
    // shown: the promise to the same-mode Retry, and the pointer at the mode-change actions to
    // an attempt the session answered with an error, which gets no Retry.
    if (!retryRow) return { text: cause, asking: false };
    if (isSafeRetry(delivery)) {
      return {
        text: withGuidance(cause, safeRetryGuidance("card", delivery.error)),
        asking: false,
      };
    }
    if (delivery.answeredWithError === true) {
      return { text: withGuidance(cause, answeredWithErrorGuidance("card")), asking: false };
    }
    return { text: cause, asking: false };
  }
  const mode = delivery?.delivery ?? "steer";
  if (delivery?.state === "pending")
    return { text: `Sending to ${targetName} (${MODE_LABELS[mode]})`, asking: false };
  if (mode === "btw") return { text: `Asking ${targetName} (${MODE_LABELS.btw}) ·`, asking: true };
  if (delivery?.duplicate === true) return { text: duplicateText, asking: false };
  return { text: `Sent to ${targetName} (${MODE_LABELS[mode]})`, asking: false };
}

/** One earlier attempt's line: what it did, and the name its own target resolved to when it
 *  was made, which a later role hand-off does not change. An earlier attempt is never what
 *  Retry re-sends, so it never carries the safe-retry promise. */
function attemptSummary(attempt: TargetedMessageAttempt, targetName: string): string {
  if (attempt.state === "failed") return `Failed: ${attempt.error ?? "delivery failed"}`;
  if (attempt.duplicate === true) return duplicateText;
  const verb = attempt.state === "pending" ? "Sending" : "Sent";
  return `${verb} to ${attempt.targetName ?? targetName} (${MODE_LABELS[attempt.delivery]})`;
}

/** Whether this card is offering the same-mode Retry the safe-retry promise describes: the
 *  attempt has to be retryable at all, and the surface has to be showing the button. */
export function offersSafeRetry(
  deliveries: readonly TargetedMessageAttempt[],
  retryAvailable: boolean
): boolean {
  const latest = deliveries.at(-1);
  return retryAvailable && latest !== undefined && isSafeRetry(latest);
}

/** What became of a targeted message: answered, taken into the session's conversation as the
 *  person's own turn, failed, asking (BTW), or sent - then the earlier attempts, oldest first.
 *  Shared by the card and by a delivered reply in its thread. `retryRow` is whether the surface
 *  shows `DeliveryRetry` beneath it, whose buttons the failure's guidance names. */
export function DeliveryStatus({
  answeredBy,
  deliveries,
  retryRow = false,
  targetName,
}: {
  answeredBy?: string;
  deliveries: readonly TargetedMessageAttempt[];
  retryRow?: boolean;
  targetName: string;
}): ReactNode {
  const delivery = deliveries.at(-1);
  const headline = deliveryHeadline(answeredBy, delivery, targetName, retryRow);
  return (
    <>
      <p className={`mt-2 text-sm font-semibold ${textPrimaryOnSurface}`}>
        {headline.text}
        {headline.asking ? <Timestamp at={delivery?.createdAt ?? ""} /> : null}
      </p>
      {deliveries.length > 1 ? (
        <div className={`mt-2 flex flex-col gap-1 text-xs ${textMutedOnSurface}`}>
          {deliveries.slice(0, -1).map((attempt) => (
            <span key={attempt.attempt}>
              Attempt {attempt.attempt}: {attemptSummary(attempt, targetName)}
            </span>
          ))}
        </div>
      ) : null}
    </>
  );
}

/** The retry row an unanswered targeted message keeps while its issue is open. A disabled
 *  button's reason renders as visible text beneath it - not a `title` - so it reaches a phone,
 *  where a tooltip on a disabled control is unreachable.
 *
 *  Retry re-sends in the attempt's OWN mode, and appears only while `isSafeRetry` holds: that
 *  send carries the same idempotency key, so if the message already landed the repeat is
 *  recognised and dropped - but only inside `DELIVERY_DUPLICATE_WINDOW_MS`, and only for an
 *  attempt that actually failed and that the session did not answer with an error. Offering it
 *  otherwise would be offering a second delivery, or no delivery, under a promise of one.
 *  The mode-change actions have no such limit: they are a different key and are honestly
 *  labelled "instead". */
export function DeliveryRetry({
  canAside,
  canBtw,
  canSteer,
  mode,
  onRetry,
  retrying,
  sameModeRetry,
  targetName,
}: {
  canAside: boolean;
  canBtw: boolean;
  canSteer: boolean;
  mode: MessageDeliveryMode;
  onRetry: (delivery: MessageDeliveryMode) => void;
  retrying: boolean;
  /** `offersSafeRetry` for this surface: the same decision the safe-retry sentence is made on. */
  sameModeRetry: boolean;
  targetName: string;
}): ReactNode {
  const supports: Record<MessageDeliveryMode, boolean> = {
    aside: canAside,
    btw: canBtw,
    steer: canSteer,
  };
  return (
    <div className="mt-3 flex flex-wrap gap-3">
      {!sameModeRetry ? null : (
        <div>
          <button
            className={`min-h-11 rounded-lg border px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText}`}
            disabled={retrying || !supports[mode]}
            onClick={() => onRetry(mode)}
            type="button"
          >
            Retry
          </button>
          {supports[mode] ? null : (
            <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
              {targetName} does not support {MODE_LABELS[mode]}.
            </p>
          )}
        </div>
      )}
      {mode === "btw" ? null : (
        <div>
          <button
            className={`min-h-11 rounded-lg border px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText}`}
            disabled={retrying || !canBtw}
            onClick={() => onRetry("btw")}
            type="button"
          >
            Use {MODE_LABELS.btw} instead
          </button>
          {canBtw ? null : (
            <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
              {targetName} does not support {MODE_LABELS.btw}.
            </p>
          )}
        </div>
      )}
      {mode === "steer" ? null : (
        <div>
          <button
            className={`min-h-11 rounded-lg border px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText}`}
            disabled={retrying || !canSteer}
            onClick={() => onRetry("steer")}
            type="button"
          >
            Use {MODE_LABELS.steer} instead
          </button>
          {canSteer ? null : (
            <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
              {targetName} does not support {MODE_LABELS.steer}
              {canBtw ? ` — use ${MODE_LABELS.btw}.` : "."}
            </p>
          )}
        </div>
      )}
    </div>
  );
}

interface TargetedMessageCardProps {
  /** Who answered the message, once a session did; the card then reads "Answered by". */
  readonly answeredBy?: string;
  readonly body: ReactNode;
  readonly canAside: boolean;
  readonly canBtw: boolean;
  readonly canSteer: boolean;
  readonly current?: boolean;
  readonly deliveries: readonly TargetedMessageAttempt[];
  readonly header: ReactNode;
  readonly isClosed: boolean;
  readonly lastSeq?: number;
  readonly onReply?: () => void;
  readonly onRetry?: (delivery: MessageDeliveryMode) => void;
  readonly register?: (element: HTMLLIElement | null) => void;
  readonly retrying?: boolean;
  readonly targetName: string;
  /** The replies beneath the message - the answer and every follow-up - as a nested list. */
  readonly thread?: ReactNode;
  readonly turnID: string;
}

/** Shared targeted-message presentation for issue turns and agent-card conversations. */
export function TargetedMessageCard({
  answeredBy,
  body,
  canAside,
  canBtw,
  canSteer,
  current = false,
  deliveries,
  header,
  isClosed,
  lastSeq,
  onReply,
  onRetry,
  register,
  retrying = false,
  targetName,
  thread,
  turnID,
}: TargetedMessageCardProps): ReactNode {
  // Narrowed once, so the render below needs no second test of the same condition.
  const retry =
    answeredBy === undefined && !isClosed && !takenAsUserTurn(deliveries) ? onRetry : undefined;
  const sameModeRetry = offersSafeRetry(deliveries, retry !== undefined);
  return (
    <li
      aria-current={current ? "true" : undefined}
      className={`my-2 rounded-lg border p-3 ${surfaceMutedHoverBg} ${secondaryButtonBorder}`}
      data-event-seq={lastSeq}
      data-turn={turnID}
      ref={register}
    >
      <div className="flex gap-3">
        {header}
        <div className="min-w-0 flex-1">{body}</div>
        {onReply === undefined || isClosed ? null : (
          <ReplyButton className="self-start" onClick={onReply} />
        )}
      </div>
      <DeliveryStatus
        answeredBy={answeredBy}
        deliveries={deliveries}
        retryRow={retry !== undefined}
        targetName={targetName}
      />
      {retry === undefined ? null : (
        <DeliveryRetry
          canAside={canAside}
          canBtw={canBtw}
          canSteer={canSteer}
          mode={deliveries.at(-1)?.delivery ?? "steer"}
          onRetry={retry}
          retrying={retrying}
          sameModeRetry={sameModeRetry}
          targetName={targetName}
        />
      )}
      {thread}
    </li>
  );
}
