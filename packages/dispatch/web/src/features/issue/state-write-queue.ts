/**
 * Per-key state machine:
 * - `authoritative` only advances to equal-or-higher sequence rows.
 * - `pending` holds operations not owned by the single live `run`.
 * - A run owns its token, phase, and operations; pending work waits for it.
 * - Every I/O continuation first matches that token or does nothing.
 * - `finish` is the only terminal transition and starts pending work next.
 * - A pagehide without an authoritative row rejects every owned operation.
 * - Otherwise pagehide supersedes any run and writes its operations plus pending.
 * - A later pagehide supersedes an unanswered flush with a higher sequence write.
 * - Successful writes adopt their returned row and resolve only their own operations.
 * - A normal 409 adopts its row and retries the same operations once; other errors reject.
 * - A teardown write has no retry window: its error rejects its owned operations.
 */
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

type RunKind = "seed-get" | "write" | "flush";

interface QueueRun {
  kind: RunKind;
  operations: PendingOperation[];
  token: number;
}

interface PendingIssueOperations {
  authoritative: UserIssueState | undefined;
  nextSeq: number;
  pending: PendingOperation[];
  run: QueueRun | undefined;
  worker: IssueStateWriteWorker;
}

type FinishOutcome = { type: "success"; state: UserIssueState } | { error: unknown; type: "error" };

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
  private readonly entries = new Map<string, PendingIssueOperations>();
  private nextToken = 1;

  constructor() {
    if (typeof window !== "undefined") {
      window.addEventListener("pagehide", () => this.flushPending());
    }
  }

  enqueue(
    issueKey: string,
    operation: PinStateOperation,
    worker: IssueStateWriteWorker
  ): Promise<void> {
    const completion = Promise.withResolvers<void>();
    const pendingOperation: PendingOperation = {
      operation,
      reject: completion.reject,
      resolve: completion.resolve,
    };
    let entry = this.entries.get(issueKey);
    if (entry === undefined) {
      entry = {
        authoritative: undefined,
        nextSeq: 1,
        pending: [],
        run: undefined,
        worker,
      };
      this.entries.set(issueKey, entry);
    }
    entry.pending.push(pendingOperation);
    this.start(issueKey, entry);
    return completion.promise;
  }

  private start(issueKey: string, entry: PendingIssueOperations): void {
    if (entry.run !== undefined || entry.pending.length === 0) {
      return;
    }
    const run: QueueRun = {
      kind: entry.authoritative === undefined ? "seed-get" : "write",
      operations: entry.pending.splice(0),
      token: this.nextToken++,
    };
    entry.run = run;
    if (run.kind === "seed-get") {
      this.fetchState(issueKey, entry, run.token);
      return;
    }
    this.writeState(issueKey, entry, run.token, false);
  }

  private fetchState(issueKey: string, entry: PendingIssueOperations, token: number): void {
    let request: Promise<UserIssueState>;
    try {
      request = entry.worker.fetchState(issueKey);
    } catch (error) {
      this.handleFetchError(issueKey, entry, token, error);
      return;
    }
    void request.then(
      (state) => this.handleFetchSuccess(issueKey, entry, token, state),
      (error) => this.handleFetchError(issueKey, entry, token, error)
    );
  }

  private handleFetchSuccess(
    issueKey: string,
    entry: PendingIssueOperations,
    token: number,
    state: UserIssueState
  ): void {
    const run = entry.run;
    if (run?.token !== token) {
      return;
    }
    this.adoptState(entry, state);
    run.operations.push(...entry.pending.splice(0));
    run.kind = "write";
    this.writeState(issueKey, entry, token, false);
  }

  private handleFetchError(
    issueKey: string,
    entry: PendingIssueOperations,
    token: number,
    error: unknown
  ): void {
    if (entry.run?.token !== token) {
      return;
    }
    this.finish(issueKey, entry, token, { error, type: "error" });
  }

  private writeState(
    issueKey: string,
    entry: PendingIssueOperations,
    token: number,
    retried: boolean
  ): void {
    const run = entry.run;
    if (run?.token !== token) {
      return;
    }
    let request: Promise<UserIssueState>;
    try {
      request = entry.worker.putState(issueKey, this.nextState(entry, run.operations));
    } catch (error) {
      this.handleWriteError(issueKey, entry, token, retried, error);
      return;
    }
    void request.then(
      (state) => this.handleWriteSuccess(issueKey, entry, token, state),
      (error) => this.handleWriteError(issueKey, entry, token, retried, error)
    );
  }

  private handleWriteSuccess(
    issueKey: string,
    entry: PendingIssueOperations,
    token: number,
    state: UserIssueState
  ): void {
    if (entry.run?.token !== token) {
      return;
    }
    this.finish(issueKey, entry, token, { state, type: "success" });
  }

  private handleWriteError(
    issueKey: string,
    entry: PendingIssueOperations,
    token: number,
    retried: boolean,
    error: unknown
  ): void {
    const run = entry.run;
    if (run?.token !== token) {
      return;
    }
    const staleState = entry.worker.staleState?.(error);
    if (staleState !== undefined) {
      this.adoptState(entry, staleState);
      if (run.kind !== "flush" && !retried) {
        this.writeState(issueKey, entry, token, true);
        return;
      }
    }
    this.finish(issueKey, entry, token, { error, type: "error" });
  }

  private flushPending(): void {
    for (const [issueKey, entry] of this.entries) {
      const operations = [...(entry.run?.operations ?? []), ...entry.pending.splice(0)];
      if (operations.length === 0) {
        continue;
      }
      const run: QueueRun = { kind: "flush", operations, token: this.nextToken++ };
      entry.run = run;
      if (entry.authoritative === undefined) {
        this.finish(issueKey, entry, run.token, {
          error: new Error("Cannot save pinned items before their current state is loaded."),
          type: "error",
        });
        continue;
      }
      this.writeState(issueKey, entry, run.token, false);
    }
  }

  private finish(
    issueKey: string,
    entry: PendingIssueOperations,
    token: number,
    outcome: FinishOutcome
  ): void {
    const run = entry.run;
    if (run?.token !== token) {
      return;
    }
    entry.run = undefined;
    if (outcome.type === "success") {
      this.adoptState(entry, outcome.state);
      this.resolveOperations(run.operations);
    } else {
      this.rejectOperations(issueKey, entry, run.operations, outcome.error);
    }
    if (entry.pending.length > 0) {
      this.start(issueKey, entry);
      return;
    }
    this.entries.delete(issueKey);
    if (outcome.type === "success") {
      entry.worker.onDrained(issueKey, entry.authoritative ?? outcome.state);
    }
  }

  private nextState(
    entry: PendingIssueOperations,
    operations: PendingOperation[]
  ): Pick<UserIssueState, "dismissed" | "seq"> {
    const base = entry.authoritative;
    if (base === undefined) {
      throw new Error("Queued state write requires an authoritative state.");
    }
    const dismissed = operations.reduce(
      (current, operation) => applyPinStateOperation(current, operation.operation),
      base.dismissed
    );
    const seq = Math.max(entry.nextSeq, base.seq + 1);
    entry.nextSeq = seq + 1;
    return { dismissed, seq };
  }

  private adoptState(entry: PendingIssueOperations, state: UserIssueState): boolean {
    if (entry.authoritative !== undefined && state.seq < entry.authoritative.seq) {
      return false;
    }
    entry.authoritative = state;
    entry.nextSeq = Math.max(entry.nextSeq, state.seq + 1);
    return true;
  }

  private resolveOperations(operations: PendingOperation[]): void {
    for (const operation of operations) {
      operation.resolve();
    }
  }

  private rejectOperations(
    issueKey: string,
    entry: PendingIssueOperations,
    operations: PendingOperation[],
    error: unknown
  ): void {
    for (const operation of operations) {
      operation.reject(error);
    }
    entry.worker.onError(
      issueKey,
      operations.map((operation) => operation.operation),
      entry.authoritative
    );
  }
}

/** One browser-wide serial queue keeps pin writes from every visible issue surface ordered. */
export const sharedIssueStateWrites = new IssueStateWriteQueue();
