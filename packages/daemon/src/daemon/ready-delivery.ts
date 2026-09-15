import { boundedWait } from "./runtime";

export const READY_DELIVERY_RETRY_DELAYS_MS = [5_000, 15_000, 45_000, 90_000, 180_000] as const;
const READY_DELIVERY_CYCLE_GAP_MS =
  READY_DELIVERY_RETRY_DELAYS_MS[READY_DELIVERY_RETRY_DELAYS_MS.length - 1];
export const READY_DELIVERY_ATTEMPTS_PER_CYCLE = READY_DELIVERY_RETRY_DELAYS_MS.length + 1;

/** A connect, prompt, or turn-observation transport failure that the ready retry may repeat. */
export class ReadyDeliveryTransportError extends Error {
  constructor(cause: unknown) {
    super(cause instanceof Error ? cause.message : String(cause), { cause });
    this.name = "ReadyDeliveryTransportError";
  }
}

export interface ReadyDeliveryRetrierOptions {
  /** Logs caller-specific context before each cycle's first generic retry record. */
  onFirstFailure(error: unknown): void;
  /** Charges or gives up on a cycle whose `READY_DELIVERY_ATTEMPTS_PER_CYCLE` attempts all
   * failed; `lastError` is the final attempt's failure. The caller owns that cycle's one log line. */
  onCycleExhausted(
    cycle: number,
    lastError: ReadyDeliveryTransportError
  ): Promise<"stop" | "continue">;
}

/** Runs the shared bounded retry schedule for ready-time connect and prompt delivery. Only a
 * `ReadyDeliveryTransportError` is retried; any other failure propagates from `run` at once. */
export class ReadyDeliveryRetrier {
  private readonly waits = new Set<{ cancel: () => void }>();

  constructor(
    private readonly disposed: () => boolean,
    private readonly sleep?: (ms: number) => Promise<void>
  ) {}

  cancelAll(): void {
    for (const { cancel } of this.waits) cancel();
    this.waits.clear();
  }

  private async wait(ms: number): Promise<boolean> {
    if (this.disposed()) return false;
    const wait = boundedWait(ms, this.sleep);
    this.waits.add(wait);
    try {
      await wait.timedOut;
    } finally {
      this.waits.delete(wait);
    }
    return !this.disposed();
  }

  async run(
    describe: string,
    attempt: (attemptNumber: number, cycle: number) => Promise<void>,
    options: ReadyDeliveryRetrierOptions
  ): Promise<void> {
    for (let cycle = 1; ; cycle += 1) {
      for (
        let attemptNumber = 1;
        attemptNumber <= READY_DELIVERY_ATTEMPTS_PER_CYCLE;
        attemptNumber += 1
      ) {
        if (this.disposed()) return;
        try {
          await attempt(attemptNumber, cycle);
          return;
        } catch (error) {
          if (!(error instanceof ReadyDeliveryTransportError)) throw error;
          if (attemptNumber === 1) options.onFirstFailure(error);
          if (attemptNumber === READY_DELIVERY_ATTEMPTS_PER_CYCLE) {
            if (this.disposed()) return;
            if ((await options.onCycleExhausted(cycle, error)) === "stop") return;
            if (!(await this.wait(READY_DELIVERY_CYCLE_GAP_MS))) return;
            break;
          }
          const delay = READY_DELIVERY_RETRY_DELAYS_MS[attemptNumber - 1];
          console.error(
            `[legion] ${describe} failed (attempt ${attemptNumber}/${READY_DELIVERY_ATTEMPTS_PER_CYCLE}, cycle ${cycle}); retrying in ${delay / 1000}s: ${error.message}`
          );
          if (!(await this.wait(delay))) return;
        }
      }
    }
  }
}
