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
import { duplicateText, isSafeRetry, safeRetryGuidance, withGuidance } from "./delivery";
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
  readonly attempt: number;
  readonly createdAt: string;
  readonly delivery: MessageDeliveryMode;
  /** The listener already held this message, so this attempt reached it and changed nothing. */
  readonly duplicate?: boolean;
  readonly error?: string | null;
  readonly state: "pending" | "sent" | "failed";
  readonly targetName?: string;
}

/** The headline one targeted message gets: who answered it, why it failed, that it is still
 *  going out, that it is a BTW waiting on an answer, or that it was delivered. `asking` is
 *  that BTW branch, whose line ends in a separator because it carries a timestamp: the two
 *  belong to one decision, so the caller reads it here rather than restating the condition.
 *
 *  A BTW is tested before `duplicate`: an outstanding BTW is still waiting on its answer even
 *  when the send that carried it changed nothing, so the human must keep seeing that one is
 *  expected. */
function deliveryHeadline(
  answeredBy: string | undefined,
  delivery: TargetedMessageAttempt | undefined,
  targetName: string,
  retryOffered: boolean
): { text: string; asking: boolean } {
  if (answeredBy !== undefined) return { text: `Answered by ${answeredBy}`, asking: false };
  if (delivery?.state === "failed") {
    const cause = `Failed: ${delivery.error ?? "delivery failed"}`;
    // The promise belongs to the button: it is only true of the Retry this card is actually
    // offering, and only while that Retry is offered at all.
    return {
      text: retryOffered ? withGuidance(cause, safeRetryGuidance("card", delivery.error)) : cause,
      asking: false,
    };
  }
  const mode = delivery?.delivery ?? "steer";
  if (delivery?.state === "pending")
    return { text: `Sending to ${targetName} (${mode})`, asking: false };
  if (mode === "btw") return { text: `Asking ${targetName} (BTW) ·`, asking: true };
  if (delivery?.duplicate === true) return { text: duplicateText, asking: false };
  return { text: `Sent to ${targetName} (${mode})`, asking: false };
}

/** One earlier attempt's line: what it did, and the name its own target resolved to when it
 *  was made, which a later role hand-off does not change. An earlier attempt is never what
 *  Retry re-sends, so it never carries the safe-retry promise. */
function attemptSummary(attempt: TargetedMessageAttempt, targetName: string): string {
  if (attempt.state === "failed") return `Failed: ${attempt.error ?? "delivery failed"}`;
  if (attempt.duplicate === true) return duplicateText;
  const verb = attempt.state === "pending" ? "Sending" : "Sent";
  return `${verb} to ${attempt.targetName ?? targetName} (${attempt.delivery})`;
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

/** What became of a targeted message: answered, failed, asking (BTW), or sent - then the
 *  earlier attempts, oldest first. Shared by the card and by a delivered reply in its thread. */
export function DeliveryStatus({
  answeredBy,
  deliveries,
  retryOffered = false,
  targetName,
}: {
  answeredBy?: string;
  deliveries: readonly TargetedMessageAttempt[];
  retryOffered?: boolean;
  targetName: string;
}): ReactNode {
  const delivery = deliveries.at(-1);
  const headline = deliveryHeadline(answeredBy, delivery, targetName, retryOffered);
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
 *  send carries the same idempotency key, so the stream drops it if the message already landed
 *  - but only inside the stream's duplicate window, and only for an attempt that actually
 *  failed. Offering it otherwise would be offering a second delivery under a promise of none.
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
              {targetName} does not support {mode}.
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
            Send as BTW instead
          </button>
          {canBtw ? null : (
            <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
              {targetName} does not support BTW.
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
            Send normally instead
          </button>
          {canSteer ? null : (
            <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
              {targetName} does not support normal delivery — use BTW.
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
  const retry = answeredBy === undefined && !isClosed ? onRetry : undefined;
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
        retryOffered={sameModeRetry}
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
