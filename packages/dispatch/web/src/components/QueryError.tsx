import type { ReactNode } from "react";

import { dangerText } from "../theme/classes";

export interface QueryErrorProps {
  message: string;
  onRetry: () => void;
  /** Disables Retry while the retried request is in flight. */
  retrying?: boolean;
}

/**
 * A retryable error state for a failed query or write, matching the Conversation
 * pin-state queue's affordance.
 */
export function QueryError({ message, onRetry, retrying = false }: QueryErrorProps): ReactNode {
  return (
    <div className={`flex flex-wrap items-center gap-3 text-sm ${dangerText}`} role="alert">
      <p>{message}</p>
      <button
        className="font-medium underline disabled:cursor-not-allowed disabled:opacity-50"
        disabled={retrying}
        onClick={onRetry}
        type="button"
      >
        Retry
      </button>
    </div>
  );
}
