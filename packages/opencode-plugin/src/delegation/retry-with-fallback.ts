import type { RetryConfig } from "../config";

/** Default retry config values matching config/index.ts DEFAULT_CONFIG. */
const RETRY_DEFAULTS = {
  maxRetries: 1,
  delayMs: 2000,
} as const;

/** HTTP status codes considered transient (retryable). */
const TRANSIENT_STATUS_CODES = new Set([429, 500, 502, 503, 504, 529]);

/** Error message patterns indicating transient failures. */
const TRANSIENT_MESSAGE_PATTERNS = [/overloaded/i, /rate.?limit/i, /too many requests/i];

/**
 * Determine if an error is transient (worth retrying).
 *
 * Transient: 429, 5xx, timeouts, network errors, overloaded messages.
 * Not transient: 400, 401, 403, 404, 422 and other 4xx client errors.
 */
export function isTransientError(error: unknown): boolean {
  if (!(error instanceof Error)) return false;

  // Check for HTTP status code on the error object
  const status = (error as Error & { status?: number }).status;
  if (typeof status === "number") {
    return TRANSIENT_STATUS_CODES.has(status);
  }

  // Check error name for timeout/network errors
  if (error.name === "TimeoutError") return true;
  if (error.name === "TypeError" && error.message.includes("fetch failed")) return true;
  if (error.name === "AbortError") return true;

  // Check message patterns
  for (const pattern of TRANSIENT_MESSAGE_PATTERNS) {
    if (pattern.test(error.message)) return true;
  }

  return false;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export interface RetryWithFallback {
  /**
   * Execute a function with retry logic for transient errors.
   * Non-transient errors are thrown immediately without retry.
   */
  execute<T>(fn: () => Promise<T>): Promise<T>;

  /**
   * Execute with retry logic, then fall back to a secondary function on exhaustion.
   * Only falls back if the primary failed with transient errors after all retries.
   * Non-transient errors are thrown immediately without fallback.
   */
  executeWithFallback<T>(primaryFn: () => Promise<T>, fallbackFn?: () => Promise<T>): Promise<T>;

  /** Number of retries performed during the last execute/executeWithFallback call. */
  getLastRetryCount(): number;
}

/**
 * Create a retry-with-fallback executor.
 *
 * Retries transient errors (429, 5xx, timeouts) up to maxRetries times
 * with delayMs between attempts. Non-transient errors (400, 401, 403, etc.)
 * are thrown immediately.
 *
 * When used with executeWithFallback, exhausted retries on transient errors
 * trigger the fallback function (e.g., to try a different model).
 */
export function createRetryWithFallback(config: RetryConfig): RetryWithFallback {
  const maxRetries = config.maxRetries ?? RETRY_DEFAULTS.maxRetries;
  const delayMs = config.delayMs ?? RETRY_DEFAULTS.delayMs;
  let lastRetryCount = 0;

  async function executeWithRetries<T>(fn: () => Promise<T>): Promise<T> {
    lastRetryCount = 0;
    let lastError: Error | undefined;

    for (let attempt = 0; attempt <= maxRetries; attempt++) {
      try {
        return await fn();
      } catch (err) {
        const error = err instanceof Error ? err : new Error(String(err));

        // Non-transient errors: throw immediately, no retry
        if (!isTransientError(error)) {
          throw error;
        }

        lastError = error;

        // If we have retries left, wait and try again
        if (attempt < maxRetries) {
          lastRetryCount++;
          await delay(delayMs);
        }
      }
    }

    // All retries exhausted — lastError is guaranteed to be set because
    // maxRetries >= 0 means at least one iteration ran and caught an error.
    throw lastError as Error;
  }

  return {
    execute<T>(fn: () => Promise<T>): Promise<T> {
      return executeWithRetries(fn);
    },

    async executeWithFallback<T>(
      primaryFn: () => Promise<T>,
      fallbackFn?: () => Promise<T>
    ): Promise<T> {
      try {
        return await executeWithRetries(primaryFn);
      } catch (primaryError) {
        // Only fall back on transient errors (retries exhausted)
        // Non-transient errors were already thrown from executeWithRetries
        if (fallbackFn && isTransientError(primaryError)) {
          return await fallbackFn();
        }
        throw primaryError;
      }
    },

    getLastRetryCount(): number {
      return lastRetryCount;
    },
  };
}
