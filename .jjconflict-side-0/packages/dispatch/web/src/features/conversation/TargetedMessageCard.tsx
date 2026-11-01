import type { ReactNode } from "react";

import type { Actor, Agent, MessageDeliveryMode } from "../../api/types";
import {
  secondaryButtonBorder,
  secondaryButtonText,
  surfaceMutedHoverBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { type AgentPart, AgentParts } from "../refs/AgentName";
import { Timestamp } from "../refs/Timestamp";
import type { DwellRef } from "../shell/useDwell";
import { duplicateText, failureGuidance, isSafeRetry, MODE_LABELS, withGuidance } from "./delivery";
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
  /** The session this attempt reached (an agent part, so it is drawn by `AgentName`), else the
   *  address it was aimed at; a role address is text. */
  readonly targetName?: AgentPart;
}

/** Whether the session took a targeted message as its user's own turn: its latest attempt says
 *  so, as the session recorded it with Dispatch. Such a message has nothing to retry, and that
 *  outranks a `failed` the send recorded afterwards, since the session already said it took it. */
export function takenAsUserTurn(deliveries: readonly TargetedMessageAttempt[]): boolean {
  return deliveries.at(-1)?.acceptedAs === "user_turn";
}

/** The headline one targeted message gets: who answered it, that it reached the session's own
 *  conversation, why it failed, that it is still going out, that it is a BTW waiting on an
 *  answer, or that it was delivered, naming the mode as the composer does (`MODE_LABELS`). It is
 *  `AgentPart`s, so the agents it names keep their identity until `AgentParts` draws them.
 *  `asking` is that BTW branch, whose line ends in a separator because it carries a timestamp:
 *  the two belong to one decision, so the caller reads it here rather than restating the
 *  condition.
 *
 *  A BTW is tested before `duplicate`: an outstanding BTW is still waiting on its answer even
 *  when the send that carried it changed nothing, so the human must keep seeing that one is
 *  expected. */
function deliveryHeadline(
  answeredBy: Actor | undefined,
  delivery: TargetedMessageAttempt | undefined,
  targetName: AgentPart,
  retryRow: boolean
): { parts: AgentPart[]; asking: boolean } {
  if (answeredBy !== undefined)
    return { parts: ["Answered by ", { actor: answeredBy }], asking: false };
  if (delivery?.acceptedAs === "user_turn") {
    return {
      parts: ["Delivered to ", targetName, `'s conversation (${MODE_LABELS[delivery.delivery]})`],
      asking: false,
    };
  }
  if (delivery?.state === "failed") {
    const cause = `Failed: ${delivery.error ?? "delivery failed"}`;
    // Each sentence names a control in the retry row, so it is said only while that row is shown.
    const guidance = retryRow ? failureGuidance(delivery, "card") : undefined;
    return {
      parts: [guidance === undefined ? cause : withGuidance(cause, guidance)],
      asking: false,
    };
  }
  const mode = delivery?.delivery ?? "steer";
  if (delivery?.state === "pending")
    return { parts: ["Sending to ", targetName, ` (${MODE_LABELS[mode]})`], asking: false };
  if (mode === "btw") {
    return { parts: ["Asking ", targetName, ` (${MODE_LABELS.btw}) ·`], asking: true };
  }
  if (delivery?.duplicate === true) return { parts: [duplicateText], asking: false };
  return { parts: ["Sent to ", targetName, ` (${MODE_LABELS[mode]})`], asking: false };
}

/** One earlier attempt's line: what it did, and the session it reached when it was made, which
 *  a later role hand-off does not change. An earlier attempt is never what Retry re-sends, so it
 *  never carries the safe-retry promise. */
function attemptSummary(attempt: TargetedMessageAttempt, targetName: AgentPart): AgentPart[] {
  if (attempt.state === "failed") return [`Failed: ${attempt.error ?? "delivery failed"}`];
  if (attempt.duplicate === true) return [duplicateText];
  const verb = attempt.state === "pending" ? "Sending" : "Sent";
  return [`${verb} to `, attempt.targetName ?? targetName, ` (${MODE_LABELS[attempt.delivery]})`];
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
  /** The session that answered, once one did. */
  answeredBy?: Actor;
  deliveries: readonly TargetedMessageAttempt[];
  retryRow?: boolean;
  /** Who the message went to: the session its newest attempt reached, else its address. */
  targetName: AgentPart;
}): ReactNode {
  const delivery = deliveries.at(-1);
  const headline = deliveryHeadline(answeredBy, delivery, targetName, retryRow);
  return (
    <>
      <p className={`mt-2 text-sm font-semibold ${textPrimaryOnSurface}`}>
        <AgentParts parts={headline.parts} />
        {headline.asking ? <Timestamp at={delivery?.createdAt ?? ""} /> : null}
      </p>
      {deliveries.length > 1 ? (
        <div className={`mt-2 flex flex-col gap-1 text-xs ${textMutedOnSurface}`}>
          {deliveries.slice(0, -1).map((attempt) => (
            <span key={attempt.attempt}>
              Attempt {attempt.attempt}: <AgentParts parts={attemptSummary(attempt, targetName)} />
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
 *  Retry re-sends in the attempt's OWN mode, and appears only while `isSafeRetry` holds for
 *  `latest`, the attempt the failure guidance above it describes: that send carries the same
 *  idempotency key, so if the message already landed the repeat is recognised and dropped - but
 *  only inside `DELIVERY_DUPLICATE_WINDOW_MS`, and only for an attempt that actually failed and
 *  that the session did not answer with an error. Offering it otherwise would be offering a
 *  second delivery, or no delivery, under a promise of one. The mode-change actions have no such
 *  limit: they are a different key and are honestly labelled "instead". */
export function DeliveryRetry({
  canAside,
  canBtw,
  canSteer,
  latest,
  onRetry,
  retrying,
  targetName,
}: {
  canAside: boolean;
  canBtw: boolean;
  canSteer: boolean;
  /** The attempt this row retries; a message with none retries as a Send. */
  latest: TargetedMessageAttempt | undefined;
  onRetry: (delivery: MessageDeliveryMode) => void;
  retrying: boolean;
  targetName: AgentPart;
}): ReactNode {
  const mode = latest?.delivery ?? "steer";
  const sameModeRetry = latest !== undefined && isSafeRetry(latest);
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
              <AgentParts parts={[targetName, ` does not support ${MODE_LABELS[mode]}.`]} />
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
              <AgentParts parts={[targetName, ` does not support ${MODE_LABELS.btw}.`]} />
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
              <AgentParts
                parts={[
                  targetName,
                  ` does not support ${MODE_LABELS.steer}`,
                  canBtw ? ` — use ${MODE_LABELS.btw}.` : ".",
                ]}
              />
            </p>
          )}
        </div>
      )}
    </div>
  );
}

interface TargetedMessageCardProps {
  /** The session that answered the message, once one did; the card then reads "Answered by". */
  readonly answeredBy?: Actor;
  readonly body: ReactNode;
  readonly canAside: boolean;
  readonly canBtw: boolean;
  readonly canSteer: boolean;
  readonly current?: boolean;
  readonly deliveries: readonly TargetedMessageAttempt[];
  readonly header: ReactNode;
  readonly isClosed: boolean;
  readonly onReply?: () => void;
  readonly onRetry?: (delivery: MessageDeliveryMode) => void;
  /** Holds the Reply button while the composer it would address is busy with a send. */
  readonly replyDisabled?: boolean;
  readonly register?: DwellRef;
  readonly retrying?: boolean;
  /** Who the message went to (`DeliveryStatus`). */
  readonly targetName: AgentPart;
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
  onReply,
  onRetry,
  register,
  replyDisabled = false,
  retrying = false,
  targetName,
  thread,
  turnID,
}: TargetedMessageCardProps): ReactNode {
  // Narrowed once, so the render below needs no second test of the same condition.
  const retry =
    answeredBy === undefined && !isClosed && !takenAsUserTurn(deliveries) ? onRetry : undefined;
  return (
    <li
      aria-current={current ? "true" : undefined}
      className={`my-2 rounded-lg border p-3 ${surfaceMutedHoverBg} ${secondaryButtonBorder}`}
      data-turn={turnID}
      ref={register}
    >
      <div className="flex gap-3">
        {header}
        <div className="min-w-0 flex-1">{body}</div>
        {onReply === undefined || isClosed ? null : (
          <ReplyButton className="self-start" disabled={replyDisabled} onClick={onReply} />
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
          latest={deliveries.at(-1)}
          onRetry={retry}
          retrying={retrying}
          targetName={targetName}
        />
      )}
      {thread}
    </li>
  );
}
