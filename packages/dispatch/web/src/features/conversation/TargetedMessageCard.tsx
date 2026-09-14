import type { ReactNode } from "react";

import type { MessageDeliveryMode } from "../../api/types";
import {
  secondaryButtonBorder,
  secondaryButtonText,
  surfaceMutedHoverBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";

export interface TargetedMessageAttempt {
  readonly attempt: number;
  readonly createdAt: string;
  readonly delivery: MessageDeliveryMode;
  readonly error?: string | null;
  readonly state: "sent" | "failed";
  readonly targetName?: string;
}

interface TargetedMessageCardProps {
  readonly answer?: { readonly author: string; readonly body: ReactNode };
  readonly body: ReactNode;
  readonly canBtw: boolean;
  readonly current?: boolean;
  readonly deliveries: readonly TargetedMessageAttempt[];
  readonly header: ReactNode;
  readonly isClosed: boolean;
  readonly lastSeq?: number;
  readonly onRetry?: (delivery: "btw" | "steer") => void;
  readonly register?: (element: HTMLLIElement | null) => void;
  readonly retrying?: boolean;
  readonly targetName: string;
  readonly turnID: string;
}

/** Shared targeted-message presentation for issue turns and agent-card conversations. */
export function TargetedMessageCard({
  answer,
  body,
  canBtw,
  current = false,
  deliveries,
  header,
  isClosed,
  lastSeq,
  onRetry,
  register,
  retrying = false,
  targetName,
  turnID,
}: TargetedMessageCardProps): ReactNode {
  const delivery = deliveries.at(-1);
  const failed = delivery?.state === "failed";
  const isBtw = delivery?.delivery === "btw";
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
      </div>
      <p className={`mt-2 text-sm font-semibold ${textPrimaryOnSurface}`}>
        {answer !== undefined
          ? `Answered by ${answer.author}`
          : failed
            ? `Failed: ${delivery?.error ?? "delivery failed"}`
            : isBtw
              ? `Asking ${targetName} (BTW) ·`
              : `Sent to ${targetName} (${delivery?.delivery ?? "steer"})`}
        {answer === undefined && isBtw && !failed ? (
          <Timestamp at={delivery?.createdAt ?? ""} />
        ) : null}
      </p>
      {answer === undefined ? null : <div className="mt-2">{answer.body}</div>}
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
      {answer === undefined && !isClosed && onRetry !== undefined ? (
        <div className="mt-3 flex flex-wrap gap-2">
          <button
            className={`min-h-11 rounded-lg border px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText}`}
            disabled={retrying || !canBtw}
            onClick={() => onRetry("btw")}
            title={canBtw ? undefined : `${targetName} does not advertise BTW`}
            type="button"
          >
            Ask BTW again
          </button>
          <button
            className={`min-h-11 rounded-lg border px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText}`}
            disabled={retrying}
            onClick={() => onRetry("steer")}
            type="button"
          >
            Send normally
          </button>
        </div>
      ) : null}
    </li>
  );
}
