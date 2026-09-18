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
  readonly error?: string | null;
  readonly state: "sent" | "failed";
  readonly targetName?: string;
}

/** What became of a targeted message: answered, failed, asking (BTW), or sent - then the
 *  earlier attempts, oldest first. Shared by the card and by a delivered reply in its thread. */
export function DeliveryStatus({
  answeredBy,
  deliveries,
  targetName,
}: {
  answeredBy?: string;
  deliveries: readonly TargetedMessageAttempt[];
  targetName: string;
}): ReactNode {
  const delivery = deliveries.at(-1);
  const failed = delivery?.state === "failed";
  const isBtw = delivery?.delivery === "btw";
  return (
    <>
      <p className={`mt-2 text-sm font-semibold ${textPrimaryOnSurface}`}>
        {answeredBy !== undefined
          ? `Answered by ${answeredBy}`
          : failed
            ? `Failed: ${delivery?.error ?? "delivery failed"}`
            : isBtw
              ? `Asking ${targetName} (BTW) ·`
              : `Sent to ${targetName} (${delivery?.delivery ?? "steer"})`}
        {answeredBy === undefined && isBtw && !failed ? (
          <Timestamp at={delivery?.createdAt ?? ""} />
        ) : null}
      </p>
      {deliveries.length > 1 ? (
        <div className={`mt-2 flex flex-col gap-1 text-xs ${textMutedOnSurface}`}>
          {deliveries.slice(0, -1).map((attempt) => (
            <span key={attempt.attempt}>
              Attempt {attempt.attempt}:{" "}
              {attempt.state === "failed"
                ? `Failed: ${attempt.error ?? "delivery failed"}`
                : `Sent to ${attempt.targetName ?? targetName} (${attempt.delivery})`}
            </span>
          ))}
        </div>
      ) : null}
    </>
  );
}

/** The retry row an unanswered targeted message keeps while its issue is open. A disabled
 *  button's reason renders as visible text beneath it - not a `title` - so it reaches a phone,
 *  where a tooltip on a disabled control is unreachable. */
export function DeliveryRetry({
  canBtw,
  canSteer,
  onRetry,
  retrying,
  targetName,
}: {
  canBtw: boolean;
  canSteer: boolean;
  onRetry: (delivery: "btw" | "steer") => void;
  retrying: boolean;
  targetName: string;
}): ReactNode {
  return (
    <div className="mt-3 flex flex-wrap gap-3">
      <div>
        <button
          className={`min-h-11 rounded-lg border px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText}`}
          disabled={retrying || !canBtw}
          onClick={() => onRetry("btw")}
          type="button"
        >
          Ask BTW again
        </button>
        {canBtw ? null : (
          <p className={`mt-1 text-xs ${textMutedOnSurface}`}>{targetName} does not support BTW.</p>
        )}
      </div>
      <div>
        <button
          className={`min-h-11 rounded-lg border px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText}`}
          disabled={retrying || !canSteer}
          onClick={() => onRetry("steer")}
          type="button"
        >
          Send normally
        </button>
        {canSteer ? null : (
          <p className={`mt-1 text-xs ${textMutedOnSurface}`}>
            {targetName} does not support normal delivery — use BTW.
          </p>
        )}
      </div>
    </div>
  );
}

interface TargetedMessageCardProps {
  /** Who answered the message, once a session did; the card then reads "Answered by". */
  readonly answeredBy?: string;
  readonly body: ReactNode;
  readonly canBtw: boolean;
  readonly canSteer: boolean;
  readonly current?: boolean;
  readonly deliveries: readonly TargetedMessageAttempt[];
  readonly header: ReactNode;
  readonly isClosed: boolean;
  readonly lastSeq?: number;
  readonly onReply?: () => void;
  readonly onRetry?: (delivery: "btw" | "steer") => void;
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
      <DeliveryStatus answeredBy={answeredBy} deliveries={deliveries} targetName={targetName} />
      {answeredBy === undefined && !isClosed && onRetry !== undefined ? (
        <DeliveryRetry
          canBtw={canBtw}
          canSteer={canSteer}
          onRetry={onRetry}
          retrying={retrying}
          targetName={targetName}
        />
      ) : null}
      {thread}
    </li>
  );
}
