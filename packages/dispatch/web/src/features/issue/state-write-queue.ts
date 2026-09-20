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
  putState: (issueKey: string, dismissed: string[]) => Promise<UserIssueState>;
}

interface PendingOperation {
  operation: PinStateOperation;
  reject: (error: unknown) => void;
  resolve: () => void;
}

interface PendingIssueOperations {
  operations: PendingOperation[];
  flushing: boolean;
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
    }
  }

  enqueue(
    issueKey: string,
    operation: PinStateOperation,
    worker: IssueStateWriteWorker
  ): Promise<void> {
    const queued = this.pending.get(issueKey);
    const completion = new Promise<void>((resolve, reject) => {
      const pendingOperation = { operation, reject, resolve };
      if (queued === undefined) {
        this.pending.set(issueKey, { flushing: false, operations: [pendingOperation], worker });
      } else {
        queued.operations.push(pendingOperation);
      }
    });
    if (!this.running.has(issueKey)) {
      this.running.add(issueKey);
      void this.drain(issueKey);
    }
    return completion;
  }

  private async drain(issueKey: string): Promise<void> {
    const queued = this.pending.get(issueKey);
    if (queued === undefined) {
      this.running.delete(issueKey);
      return;
    }
    let state: UserIssueState;
    try {
      state = await queued.worker.fetchState(issueKey);
      if (queued.flushing) {
        return;
      }
    } catch (error) {
      if (!queued.flushing) {
        this.rejectPending(issueKey, queued, error, undefined);
        this.finishDrain(issueKey);
      }
      return;
    }
    try {
      while (queued.operations.length > 0) {
        const next = queued.operations[0];
        if (next === undefined) {
          break;
        }
        try {
          state = await queued.worker.putState(
            issueKey,
            applyPinStateOperation(state.dismissed, next.operation)
          );
        } catch {
          if (queued.flushing) {
            return;
          }
          try {
            state = await queued.worker.fetchState(issueKey);
            if (queued.flushing) {
              return;
            }
            state = await queued.worker.putState(
              issueKey,
              applyPinStateOperation(state.dismissed, next.operation)
            );
          } catch (error) {
            if (!queued.flushing) {
              this.rejectPending(issueKey, queued, error, state);
            }
            return;
          }
        }
        if (queued.flushing) {
          return;
        }
        queued.operations.shift();
        next.resolve();
      }
      this.pending.delete(issueKey);
      queued.worker.onDrained(issueKey, state);
    } finally {
      this.finishDrain(issueKey);
    }
  }

  private finishDrain(issueKey: string): void {
    if (this.pending.get(issueKey)?.flushing) {
      return;
    }
    this.running.delete(issueKey);
    if (this.pending.has(issueKey)) {
      this.running.add(issueKey);
      void this.drain(issueKey);
    }
  }

  private flushPending(): void {
    for (const [issueKey, queued] of this.pending) {
      if (queued.flushing) {
        continue;
      }
      queued.flushing = true;
      let write: Promise<UserIssueState>;
      try {
        write = queued.worker.putState(issueKey, queued.worker.optimisticState(issueKey).dismissed);
      } catch (error) {
        this.rejectPending(issueKey, queued, error, undefined);
        this.finishDrain(issueKey);
        continue;
      }
      void write
        .then((state) => {
          if (this.pending.get(issueKey) !== queued) {
            return;
          }
          this.pending.delete(issueKey);
          for (const operation of queued.operations) {
            operation.resolve();
          }
          queued.worker.onDrained(issueKey, state);
        })
        .catch((error) => {
          if (this.pending.get(issueKey) === queued) {
            this.rejectPending(issueKey, queued, error, undefined);
          }
        })
        .finally(() => this.finishDrain(issueKey));
    }
  }

  private rejectPending(
    issueKey: string,
    queued: PendingIssueOperations,
    error: unknown,
    state: UserIssueState | undefined
  ): void {
    this.pending.delete(issueKey);
    const operations = queued.operations.map((operation) => operation.operation);
    for (const operation of queued.operations) {
      operation.reject(error);
    }
    queued.worker.onError(issueKey, operations, state);
  }
}

/** One browser-wide serial queue keeps pin writes from every visible issue surface ordered. */
export const sharedIssueStateWrites = new IssueStateWriteQueue();
