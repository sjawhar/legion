import type { UserIssueState } from "../../api/types";

export type PinStateOperation = { id: string; op: "pin" | "unpin" };

export interface IssueStateWriteWorker {
  fetchState: (issueKey: string) => Promise<UserIssueState>;
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
  worker: IssueStateWriteWorker;
}

export function applyPinStateOperation(
  dismissed: string[],
  operation: PinStateOperation
): string[] {
  const marker = `pinned_items:${operation.id}`;
  return operation.op === "pin"
    ? dismissed.includes(marker)
      ? dismissed
      : [...dismissed, marker]
    : dismissed.filter((item) => item !== marker);
}

export class IssueStateWriteQueue {
  private readonly pending = new Map<string, PendingIssueOperations>();
  private readonly running = new Set<string>();

  enqueue(
    issueKey: string,
    operation: PinStateOperation,
    worker: IssueStateWriteWorker
  ): Promise<void> {
    const queued = this.pending.get(issueKey);
    const completion = new Promise<void>((resolve, reject) => {
      const pendingOperation = { operation, reject, resolve };
      if (queued === undefined) {
        this.pending.set(issueKey, { operations: [pendingOperation], worker });
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
    } catch (error) {
      this.rejectPending(issueKey, queued, error, undefined);
      this.finishDrain(issueKey);
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
          try {
            state = await queued.worker.fetchState(issueKey);
            state = await queued.worker.putState(
              issueKey,
              applyPinStateOperation(state.dismissed, next.operation)
            );
          } catch (error) {
            this.rejectPending(issueKey, queued, error, state);
            return;
          }
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
    this.running.delete(issueKey);
    if (this.pending.has(issueKey)) {
      this.running.add(issueKey);
      void this.drain(issueKey);
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
