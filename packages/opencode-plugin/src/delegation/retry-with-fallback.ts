import type { FallbackChain } from "../overlays/types";

export interface FallbackAttempt {
  model: string;
  error: string;
}

export interface FallbackResult<T> {
  /** The model that succeeded. */
  model: string;
  /** The result from the successful model. */
  result: T;
  /** Total number of attempts made (including the successful one). */
  attempts: number;
}

export interface RetryWithFallbackOptions {
  /** Delay in milliseconds between attempts. Default: 0. */
  delayMs?: number;
}

export class AllModelsFailed extends Error {
  /** Structured details of each failed attempt. */
  readonly attempts: FallbackAttempt[];

  constructor(attempts: FallbackAttempt[]) {
    const count = attempts.length;
    const models = attempts.map((a) => a.model).join(", ");
    super(
      `All ${count} model(s) failed. Tried: ${models}. ` +
        attempts.map((a) => `${a.model}: ${a.error}`).join("; ")
    );
    this.name = "AllModelsFailed";
    this.attempts = attempts;
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Execute an operation with model fallback. Tries each model in the chain
 * in order; returns the first success or throws AllModelsFailed with details
 * of every attempt.
 */
export async function createRetryWithFallback<T>(
  chain: FallbackChain,
  operation: (model: string) => Promise<T>,
  options?: RetryWithFallbackOptions
): Promise<FallbackResult<T>> {
  const failedAttempts: FallbackAttempt[] = [];
  const delayMs = options?.delayMs ?? 0;
  let attemptCount = 0;

  for (const model of chain) {
    if (attemptCount > 0 && delayMs > 0) {
      await sleep(delayMs);
    }
    attemptCount++;
    try {
      const result = await operation(model);
      return { model, result, attempts: attemptCount };
    } catch (err) {
      failedAttempts.push({
        model,
        error: err instanceof Error ? err.message : String(err),
      });
    }
  }

  throw new AllModelsFailed(failedAttempts);
}
