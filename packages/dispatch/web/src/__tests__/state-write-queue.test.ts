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
