import type { UserIssueState } from "../../api/types";
import { pinnedItemMarker } from "./pins";

export type PinStateOperation = { id: string; op: "pin" | "unpin" };

export interface IssueStateWriteWorker {
  fetchState: (issueKey: string) => Promise<UserIssueState>;
  onDrained: (issueKey: string, state: UserIssueState) => void;
  onError: (
    issueKey: string,
    operations: PinStateOperation[],
    state: UserIssueState | undefined
  ) => void;
  putState: (
    issueKey: string,
    state: Pick<UserIssueState, "dismissed" | "seq">
  ) => Promise<UserIssueState>;
  staleState?: (error: unknown) => UserIssueState | undefined;
}

interface PendingOperation {
  operation: PinStateOperation;
  reject: (error: unknown) => void;
  resolve: () => void;
}

interface PendingWrite {
  epoch: number;
  operations: PendingOperation[];
}

interface PendingIssueOperations {
  authoritative: UserIssueState | undefined;
  epoch: number;
  flush: PendingWrite | undefined;
  inFlight: PendingWrite | undefined;
  nextSeq: number | undefined;
  run: number;
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
  private readonly running = new Map<string, number>();

  constructor() {
    if (typeof window !== "undefined") {
      const flush = () => this.flushPending();
      window.addEventListener("pagehide", flush);
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
          authoritative: undefined,
          epoch: 0,
          flush: undefined,
          inFlight: undefined,
          nextSeq: undefined,
          operations: [pendingOperation],
          run: 0,
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
    const queued = this.pending.get(issueKey);
    if (queued === undefined || queued.flush !== undefined || queued.operations.length === 0) {
      return;
    }
    if (this.running.has(issueKey)) {
      return;
    }
    queued.run += 1;
    this.running.set(issueKey, queued.run);
    void this.drain(issueKey, queued, queued.epoch, queued.run);
  }

  private async drain(
    issueKey: string,
    queued: PendingIssueOperations,
    epoch: number,
    run: number
  ): Promise<void> {
    let state: UserIssueState;
    try {
      state = await queued.worker.fetchState(issueKey);
    } catch (error) {
      if (this.isActive(issueKey, queued, epoch) && queued.flush === undefined) {
        this.rejectAll(issueKey, queued, error);
      }
      this.finishDrain(issueKey, queued, epoch, run);
      return;
    }
    if (!this.isActive(issueKey, queued, epoch)) {
      this.finishDrain(issueKey, queued, epoch, run);
      return;
    }
    this.adoptState(queued, state);
    if (queued.flush !== undefined) {
      this.finishDrain(issueKey, queued, epoch, run);
      return;
    }
    const write: PendingWrite = { epoch, operations: queued.operations.splice(0) };
    if (write.operations.length === 0) {
      this.finishDrain(issueKey, queued, epoch, run);
      return;
    }
    queued.inFlight = write;
    let retried = false;
    for (;;) {
      try {
        state = await queued.worker.putState(
          issueKey,
          this.mergedStateWithSequence(queued, write.operations)
        );
      } catch (error) {
        if (!this.isActive(issueKey, queued, epoch) || queued.flush !== undefined) {
          this.finishDrain(issueKey, queued, epoch, run);
          return;
        }
        if (retried) {
          this.rejectAll(issueKey, queued, error);
          this.finishDrain(issueKey, queued, epoch, run);
          return;
        }
        retried = true;
        const staleState = queued.worker.staleState?.(error);
        if (staleState !== undefined) {
          this.adoptState(queued, staleState);
          continue;
        }
        try {
          state = await queued.worker.fetchState(issueKey);
        } catch (fetchError) {
          if (!this.isActive(issueKey, queued, epoch) || queued.flush !== undefined) {
            this.finishDrain(issueKey, queued, epoch, run);
            return;
          }
          this.rejectAll(issueKey, queued, fetchError);
          this.finishDrain(issueKey, queued, epoch, run);
          return;
        }
        if (!this.isActive(issueKey, queued, epoch) || queued.flush !== undefined) {
          this.finishDrain(issueKey, queued, epoch, run);
          return;
        }
        this.adoptState(queued, state);
        continue;
      }
      if (!this.isActive(issueKey, queued, epoch) || queued.flush !== undefined) {
        this.finishDrain(issueKey, queued, epoch, run);
        return;
      }
      queued.inFlight = undefined;
      this.adoptState(queued, state);
      this.resolveOperations(write.operations);
      if (queued.operations.length === 0) {
        this.running.delete(issueKey);
        this.pending.delete(issueKey);
        queued.worker.onDrained(issueKey, state);
      }
      this.finishDrain(issueKey, queued, epoch, run);
      return;
    }
  }

  private finishDrain(
    issueKey: string,
    queued: PendingIssueOperations,
    epoch: number,
    run: number
  ): void {
    if (!this.isActive(issueKey, queued, epoch) || this.running.get(issueKey) !== run) {
      return;
    }
    this.running.delete(issueKey);
    if (queued.flush === undefined) {
      this.startDrain(issueKey);
    }
  }

  private flushPending(): void {
    for (const [issueKey, queued] of this.pending) {
      if (queued.flush !== undefined) {
        continue;
      }
      if (queued.authoritative === undefined) {
        this.rejectAll(
          issueKey,
          queued,
          new Error("Cannot save pinned items before their current state is loaded.")
        );
        continue;
      }
      const operations = [...(queued.inFlight?.operations ?? []), ...queued.operations.splice(0)];
      if (operations.length === 0) {
        continue;
      }
      if (queued.inFlight !== undefined) {
        queued.epoch += 1;
        this.running.delete(issueKey);
        queued.inFlight = undefined;
      }
      const flush: PendingWrite = { epoch: queued.epoch, operations };
      queued.flush = flush;
      let write: Promise<UserIssueState>;
      try {
        write = queued.worker.putState(
          issueKey,
          this.mergedStateWithSequence(queued, flush.operations)
        );
      } catch (error) {
        this.rejectFlush(issueKey, queued, flush, error);
        continue;
      }
      void write
        .then((state) => {
          if (!this.isActive(issueKey, queued, flush.epoch) || queued.flush !== flush) {
            return;
          }
          queued.flush = undefined;
          queued.inFlight = undefined;
          this.adoptState(queued, state);
          this.resolveOperations(flush.operations);
          if (queued.operations.length === 0) {
            this.running.delete(issueKey);
            this.pending.delete(issueKey);
            queued.worker.onDrained(issueKey, state);
          } else {
            this.startDrain(issueKey);
          }
        })
        .catch((error) => this.rejectFlush(issueKey, queued, flush, error))
        .finally(() => {});
    }
  }

  private mergedStateWithSequence(
    queued: PendingIssueOperations,
    operations: PendingOperation[]
  ): Pick<UserIssueState, "dismissed" | "seq"> {
    const base = queued.authoritative;
    if (base === undefined) {
      throw new Error("Queued state write requires an authoritative state.");
    }
    const dismissed = operations.reduce(
      (current, operation) => applyPinStateOperation(current, operation.operation),
      base.dismissed
    );
    const seq = Math.max(queued.nextSeq ?? 1, base.seq + 1);
    queued.nextSeq = seq + 1;
    return { dismissed, seq };
  }

  private adoptState(queued: PendingIssueOperations, state: UserIssueState): void {
    queued.authoritative = state;
    queued.nextSeq = Math.max(queued.nextSeq ?? 1, state.seq + 1);
  }

  private isActive(issueKey: string, queued: PendingIssueOperations, epoch: number): boolean {
    return this.pending.get(issueKey) === queued && queued.epoch === epoch;
  }

  private rejectFlush(
    issueKey: string,
    queued: PendingIssueOperations,
    flush: PendingWrite,
    error: unknown
  ): void {
    if (!this.isActive(issueKey, queued, flush.epoch) || queued.flush !== flush) {
      return;
    }
    queued.flush = undefined;
    queued.inFlight = undefined;
    this.rejectOperations(issueKey, queued, flush.operations, error, queued.authoritative);
    this.startDrain(issueKey);
  }

  private rejectAll(issueKey: string, queued: PendingIssueOperations, error: unknown): void {
    if (!this.isActive(issueKey, queued, queued.epoch)) {
      return;
    }
    queued.epoch += 1;
    this.running.delete(issueKey);
    this.pending.delete(issueKey);
    this.rejectOperations(
      issueKey,
      queued,
      [...(queued.inFlight?.operations ?? []), ...queued.operations],
      error,
      queued.authoritative
    );
  }

  private resolveOperations(operations: PendingOperation[]): void {
    for (const operation of operations) {
      operation.resolve();
    }
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
