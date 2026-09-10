import type { ReactNode } from "react";

export interface QueryErrorProps {
  message: string;
  onRetry: () => void;
  /** Disables Retry while the retried request is in flight. */
  retrying?: boolean;
}

/**
 * A retryable error state for a failed query or write, matching the affordance
 * the issue log's pin/dismiss queue already had.
 */
export function QueryError({ message, onRetry, retrying = false }: QueryErrorProps): ReactNode {
  return (
    <div
      className="flex flex-wrap items-center gap-3 text-sm text-rose-700 dark:text-rose-400"
      role="alert"
    >
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
