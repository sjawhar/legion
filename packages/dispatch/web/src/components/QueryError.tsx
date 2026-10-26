import type { ReactNode } from "react";

import { AGENT_LIST_REFILLING, isAgentListRefilling } from "../api/client";
import { dangerText, textSecondaryOnSurface } from "../theme/classes";

export interface QueryErrorProps {
  /** The failure itself. An agent list that is refilling (`isAgentListRefilling`) is not a
   *  failure to report: the shared refilling notice replaces `message`, as status, not alert. */
  error?: unknown;
  message: string;
  /** Omitted for a failure a retry cannot get past; no Retry is offered then. */
  onRetry?: () => void;
  /** Disables Retry while the retried request is in flight. */
  retrying?: boolean;
}

/**
 * The line every view shows while Envoy's agent list refills, in place of any claim that an
 * agent is absent. One test id on every view, so a browser test finds it the same way on each.
 */
export function AgentListRefilling({ className }: { className?: string }): ReactNode {
  return (
    <p className={className} data-testid="agent-list-refilling" role="status">
      {AGENT_LIST_REFILLING}
    </p>
  );
}

/**
 * A retryable error state for a failed query or write, matching the Conversation
 * pin-state queue's affordance.
 */
export function QueryError({
  error,
  message,
  onRetry,
  retrying = false,
}: QueryErrorProps): ReactNode {
  const refilling = isAgentListRefilling(error);
  return (
    <div
      className={`flex flex-wrap items-center gap-3 text-sm ${refilling ? textSecondaryOnSurface : dangerText}`}
      role={refilling ? undefined : "alert"}
    >
      {refilling ? <AgentListRefilling /> : <p>{message}</p>}
      {onRetry === undefined ? null : (
        <button
          className="font-medium underline disabled:cursor-not-allowed disabled:opacity-50"
          disabled={retrying}
          onClick={onRetry}
          type="button"
        >
          Retry
        </button>
      )}
    </div>
  );
}
