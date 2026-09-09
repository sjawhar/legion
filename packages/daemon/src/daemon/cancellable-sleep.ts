export interface CancellableSleep {
  sleep(delayMs: number): Promise<void>;
  /** Resolves any in-flight `sleep` immediately and clears its timer; a no-op if none is pending. */
  cancel(): void;
}

/**
 * A `setTimeout`-backed sleep whose pending wait can be cut short. Shared by `nats-transport.ts`
 * (so `close()` never leaves a durable-consumer backoff timer alive past shutdown) and
 * `processes.ts` (so a graceful `stopProcess` that wins its race doesn't leave a stray timer
 * running for the rest of the configured stop timeout).
 */
export function createCancellableSleep(): CancellableSleep {
  let timer: ReturnType<typeof setTimeout> | undefined;
  let resolvePending: (() => void) | undefined;
  return {
    sleep(delayMs: number): Promise<void> {
      const { promise, resolve } = Promise.withResolvers<void>();
      resolvePending = resolve;
      timer = setTimeout(() => {
        timer = undefined;
        resolvePending = undefined;
        resolve();
      }, delayMs);
      return promise;
    },
    cancel(): void {
      if (timer === undefined) return;
      clearTimeout(timer);
      timer = undefined;
      resolvePending?.();
      resolvePending = undefined;
    },
  };
}
