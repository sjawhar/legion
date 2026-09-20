import type { UserIssueState } from "../../api/types";
import { pinnedItemMarker } from "./pins";

export type PinStateOperation = { id: string; op: "pin" | "unpin" };

export interface IssueStateWriteWorker {
  fetchState: (issueKey: string) => Promise<UserIssueState>;
  optimisticState: (issueKey: string) => UserIssueState;
  onDrained: (issueKey: string, state: UserIssueState) => void;
  onError: (
    issueKey: string,
    operations: PinStateOperation[],
    state: UserIssueState | undefined
  ) => void;
  putState: (issueKey: string, state: UserIssueState) => Promise<UserIssueState>;
  staleState?: (error: unknown) => UserIssueState | undefined;
}

interface PendingOperation {
  operation: PinStateOperation;
  reject: (error: unknown) => void;
  resolve: () => void;
}

interface Flush {
  epoch: number;
  operations: PendingOperation[];
}

interface PendingIssueOperations {
  epoch: number;
  flush: Flush | undefined;
  nextSeq: number | undefined;
  operations: PendingOperation[];
  worker: IssueStateWriteWorker;
}

export function applyPinStateOperation(
  dismissed: string[],
  operation: PinStateOperation
): string[] {
  const marker = pinnedItemMarker(operation.id);
  return operation.op === "pin"
    ? dismissed.includes(marker)
      ? dismissed
      : [...dismissed, marker]
    : dismissed.filter((item) => item !== marker);
}

export class IssueStateWriteQueue {
  private readonly pending = new Map<string, PendingIssueOperations>();
  private readonly running = new Set<string>();

  constructor() {
    if (typeof window !== "undefined") {
      const flush = () => this.flushPending();
      window.addEventListener("pagehide", flush);
      window.addEventListener("beforeunload", flush);
      window.addEventListener("pageshow", (event) => {
        if (event.persisted) {
          this.resumeFromBackForwardCache();
        }
      });
    }
  }

  enqueue(
    issueKey: string,
    operation: PinStateOperation,
    worker: IssueStateWriteWorker
  ): Promise<void> {
    const completion = new Promise<void>((resolve, reject) => {
      const pendingOperation = { operation, reject, resolve };
      const queued = this.pending.get(issueKey);
      if (queued === undefined) {
        this.pending.set(issueKey, {
          epoch: 0,
          flush: undefined,
          nextSeq: undefined,
          operations: [pendingOperation],
          worker,
        });
      } else {
        queued.operations.push(pendingOperation);
      }
    });
    this.startDrain(issueKey);
    return completion;
  }

  private startDrain(issueKey: string): void {
    if (this.running.has(issueKey)) {
      return;
    }
    const queued = this.pending.get(issueKey);
    if (queued === undefined || queued.operations.length === 0) {
      return;
    }
    this.running.add(issueKey);
    void this.drain(issueKey, queued, queued.epoch);
  }

  private async drain(
    issueKey: string,
    queued: PendingIssueOperations,
    epoch: number
  ): Promise<void> {
    let state: UserIssueState;
    try {
      state = await queued.worker.fetchState(issueKey);
    } catch (error) {
      if (this.isActive(issueKey, queued, epoch) && queued.flush === undefined) {
        this.rejectPending(issueKey, queued, error, undefined);
      }
      this.finishDrain(issueKey, queued, epoch);
      return;
    }
    if (!this.isActive(issueKey, queued, epoch) || queued.flush !== undefined) {
      this.finishDrain(issueKey, queued, epoch);
      return;
    }
    this.seedSequence(queued, state);
    try {
      while (queued.operations.length > 0) {
        const next = queued.operations[0];
        if (next === undefined) {
          break;
        }
        let retried = false;
        for (;;) {
          try {
            state = await queued.worker.putState(
              issueKey,
              this.optimisticStateWithSequence(issueKey, queued)
            );
          } catch (error) {
            if (!this.isActive(issueKey, queued, epoch) || queued.flush !== undefined) {
              return;
            }
            if (retried) {
              this.rejectPending(issueKey, queued, error, state);
              return;
            }
            retried = true;
            const staleState = queued.worker.staleState?.(error);
            if (staleState !== undefined) {
              state = staleState;
            } else {
              try {
                state = await queued.worker.fetchState(issueKey);
              } catch (fetchError) {
                this.rejectPending(issueKey, queued, fetchError, state);
                return;
              }
              if (!this.isActive(issueKey, queued, epoch) || queued.flush !== undefined) {
                return;
              }
            }
            this.seedSequence(queued, state);
            continue;
          }
          if (!this.isActive(issueKey, queued, epoch) || queued.flush !== undefined) {
            return;
          }
          this.seedSequence(queued, state);
          queued.operations.shift();
          next.resolve();
          break;
        }
      }
      if (this.isActive(issueKey, queued, epoch) && queued.flush === undefined) {
        this.pending.delete(issueKey);
        queued.worker.onDrained(issueKey, state);
      }
    } finally {
      this.finishDrain(issueKey, queued, epoch);
    }
  }

  private finishDrain(issueKey: string, queued: PendingIssueOperations, epoch: number): void {
    if (queued.epoch !== epoch) {
      return;
    }
    this.running.delete(issueKey);
    if (this.pending.get(issueKey) === queued && queued.flush === undefined) {
      this.startDrain(issueKey);
    }
  }

  private flushPending(): void {
    for (const [issueKey, queued] of this.pending) {
      if (queued.flush !== undefined || queued.operations.length === 0) {
        continue;
      }
      const flush: Flush = {
        epoch: queued.epoch,
        operations: queued.operations.splice(0),
      };
      queued.flush = flush;
      let write: Promise<UserIssueState>;
      try {
        write = queued.worker.putState(
          issueKey,
          this.optimisticStateWithSequence(issueKey, queued)
        );
      } catch (error) {
        this.rejectFlush(issueKey, queued, flush, error);
        continue;
      }
      void write
        .then((state) => {
          if (this.pending.get(issueKey) !== queued || queued.flush !== flush) {
            return;
          }
          queued.flush = undefined;
          this.seedSequence(queued, state);
          for (const operation of flush.operations) {
            operation.resolve();
          }
          if (queued.operations.length === 0) {
            this.pending.delete(issueKey);
            queued.worker.onDrained(issueKey, state);
          } else {
            this.startDrain(issueKey);
          }
        })
        .catch((error) => this.rejectFlush(issueKey, queued, flush, error))
        .finally(() => this.finishDrain(issueKey, queued, flush.epoch));
    }
  }

  private resumeFromBackForwardCache(): void {
    for (const [issueKey, queued] of this.pending) {
      queued.epoch += 1;
      this.running.delete(issueKey);
      this.startDrain(issueKey);
    }
  }

  private optimisticStateWithSequence(
    issueKey: string,
    queued: PendingIssueOperations
  ): UserIssueState {
    const state = queued.worker.optimisticState(issueKey);
    const seq = Math.max(queued.nextSeq ?? 1, state.seq + 1);
    queued.nextSeq = seq + 1;
    return { ...state, seq };
  }

  private seedSequence(queued: PendingIssueOperations, state: UserIssueState): void {
    queued.nextSeq = Math.max(queued.nextSeq ?? 1, state.seq + 1);
  }

  private isActive(issueKey: string, queued: PendingIssueOperations, epoch: number): boolean {
    return this.pending.get(issueKey) === queued && queued.epoch === epoch;
  }

  private rejectFlush(
    issueKey: string,
    queued: PendingIssueOperations,
    flush: Flush,
    error: unknown
  ): void {
    if (this.pending.get(issueKey) !== queued || queued.flush !== flush) {
      return;
    }
    queued.flush = undefined;
    this.rejectOperations(issueKey, queued, flush.operations, error, undefined);
    this.startDrain(issueKey);
  }

  private rejectPending(
    issueKey: string,
    queued: PendingIssueOperations,
    error: unknown,
    state: UserIssueState | undefined
  ): void {
    this.pending.delete(issueKey);
    this.rejectOperations(issueKey, queued, queued.operations, error, state);
  }

  private rejectOperations(
    issueKey: string,
    queued: PendingIssueOperations,
    operations: PendingOperation[],
    error: unknown,
    state: UserIssueState | undefined
  ): void {
    for (const operation of operations) {
      operation.reject(error);
    }
    queued.worker.onError(
      issueKey,
      operations.map((operation) => operation.operation),
      state
    );
  }
}

/** One browser-wide serial queue keeps pin writes from every visible issue surface ordered. */
export const sharedIssueStateWrites = new IssueStateWriteQueue();
