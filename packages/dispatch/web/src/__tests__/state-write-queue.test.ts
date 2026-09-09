import { expect, test } from "bun:test";

import type { UserIssueState } from "../api/types";
import {
  IssueStateWriteQueue,
  type IssueStateWriteWorker,
} from "../features/issue/state-write-queue";

function deferred(): { promise: Promise<void>; release: () => void } {
  let release: () => void = () => {};
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

function issueState(dismissed: string[]): UserIssueState {
  return { dismissed, last_read_seq: 0, pinned: false };
}

test("issue-state queue applies rapid operations against server state and publishes only when drained", async () => {
  const queue = new IssueStateWriteQueue();
  const started = [deferred(), deferred(), deferred()];
  const responseGates = [deferred(), deferred(), deferred()];
  const writes: string[][] = [];
  const published: UserIssueState[] = [];
  const worker: IssueStateWriteWorker = {
    fetchState: async () => issueState([]),
    onDrained: (_issueKey, state) => published.push(state),
    onError: () => {},
    putState: async (_issueKey, dismissed) => {
      const index = writes.push(dismissed) - 1;
      started[index]?.release();
      await responseGates[index]?.promise;
      return issueState(dismissed);
    },
  };

  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "dismiss" }, worker);
  const third = queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker);

  await started[0]?.promise;
  expect(writes).toEqual([["pinned_items:event:1"]]);
  responseGates[0]?.release();
  await started[1]?.promise;
  expect(writes).toEqual([["pinned_items:event:1"], ["pinned_items:event:1", "event:2"]]);
  expect(published).toEqual([]);
  responseGates[1]?.release();
  await started[2]?.promise;
  expect(writes[2]).toEqual(["pinned_items:event:1", "event:2", "pinned_items:event:3"]);
  expect(published).toEqual([]);
  responseGates[2]?.release();

  await Promise.all([first, second, third]);
  expect(published).toEqual([
    issueState(["pinned_items:event:1", "event:2", "pinned_items:event:3"]),
  ]);
});

test("issue-state queue drains operations enqueued by completion callbacks", async () => {
  const queue = new IssueStateWriteQueue();
  const writes: string[][] = [];
  let second: Promise<void> | undefined;
  let secondResolved = false;
  const worker: IssueStateWriteWorker = {
    fetchState: async () => issueState([]),
    onDrained: (issueKey) => {
      if (second === undefined) {
        second = queue.enqueue(issueKey, { id: "event:2", op: "dismiss" }, worker);
        second.then(() => {
          secondResolved = true;
        });
      }
    },
    onError: () => {},
    putState: async (_issueKey, dismissed) => {
      writes.push(dismissed);
      return issueState(dismissed);
    },
  };

  await queue.enqueue("CORE-1", { id: "event:1", op: "dismiss" }, worker);
  for (let microtask = 0; microtask < 10; microtask++) {
    await Promise.resolve();
  }

  expect(secondResolved).toBe(true);
  expect(writes).toEqual([["event:1"], ["event:2"]]);
});

test("retries a failed operation from fetched state and keeps an operation queued during failure", async () => {
  const queue = new IssueStateWriteQueue();
  const secondStarted = deferred();
  const failSecond = deferred();
  const writes: string[][] = [];
  const published: UserIssueState[] = [];
  const errors: (UserIssueState | undefined)[] = [];
  const fetched = [issueState([]), issueState(["pinned_items:event:1"])];
  let fetchCount = 0;
  const worker: IssueStateWriteWorker = {
    fetchState: async () => fetched[fetchCount++] ?? issueState([]),
    onDrained: (_issueKey, state) => published.push(state),
    onError: (_issueKey, _operations, state) => errors.push(state),
    putState: async (_issueKey, dismissed) => {
      const attempt = writes.push(dismissed);
      if (attempt === 2) {
        secondStarted.release();
        await failSecond.promise;
        throw new Error("write failed");
      }
      return issueState(dismissed);
    },
  };

  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "dismiss" }, worker);
  await secondStarted.promise;
  const third = queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker);
  failSecond.release();

  await Promise.all([first, second, third]);
  expect(writes).toEqual([
    ["pinned_items:event:1"],
    ["pinned_items:event:1", "event:2"],
    ["pinned_items:event:1", "event:2"],
    ["pinned_items:event:1", "event:2", "pinned_items:event:3"],
  ]);
  expect(published).toEqual([
    issueState(["pinned_items:event:1", "event:2", "pinned_items:event:3"]),
  ]);
  expect(errors).toEqual([]);
});

test("a second failed write rejects queued operations, restores fetched state, and clears the queue", async () => {
  const queue = new IssueStateWriteQueue();
  const secondStarted = deferred();
  const failSecond = deferred();
  const writes: string[][] = [];
  const published: UserIssueState[] = [];
  const errors: (UserIssueState | undefined)[] = [];
  const fetched = [
    issueState([]),
    issueState(["pinned_items:event:1"]),
    issueState(["pinned_items:event:1"]),
  ];
  let fetchCount = 0;
  const worker: IssueStateWriteWorker = {
    fetchState: async () => fetched[fetchCount++] ?? issueState([]),
    onDrained: (_issueKey, state) => published.push(state),
    onError: (_issueKey, _operations, state) => errors.push(state),
    putState: async (_issueKey, dismissed) => {
      const attempt = writes.push(dismissed);
      if (attempt === 2) {
        secondStarted.release();
        await failSecond.promise;
      }
      if (attempt === 2 || attempt === 3) {
        throw new Error("write failed");
      }
      return issueState(dismissed);
    },
  };

  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "dismiss" }, worker);
  const secondResult = second.then(
    () => undefined,
    (error) => error
  );
  await secondStarted.promise;
  const third = queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker);
  const thirdResult = third.then(
    () => undefined,
    (error) => error
  );
  failSecond.release();

  await first;
  const [secondError, thirdError] = await Promise.all([secondResult, thirdResult]);
  expect(secondError).toBeInstanceOf(Error);
  expect(thirdError).toBe(secondError);
  expect(writes).toEqual([
    ["pinned_items:event:1"],
    ["pinned_items:event:1", "event:2"],
    ["pinned_items:event:1", "event:2"],
  ]);
  expect(published).toEqual([]);
  expect(errors).toEqual([issueState(["pinned_items:event:1"])]);

  await queue.enqueue("CORE-1", { id: "event:4", op: "dismiss" }, worker);
  expect(writes.at(-1)).toEqual(["pinned_items:event:1", "event:4"]);
});
