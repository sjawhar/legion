import { expect, test } from "bun:test";

import { IssueStateWriteQueue } from "../features/issue/state-write-queue";

test("issue-state writes for one issue start in the order they were enqueued", async () => {
  const queue = new IssueStateWriteQueue();
  const calls: string[] = [];
  let releaseFirst: () => void = () => {};
  const firstGate = new Promise<void>((resolve) => {
    releaseFirst = resolve;
  });

  const first = queue.enqueue("CORE-1", async () => {
    calls.push("first");
    await firstGate;
  });
  const second = queue.enqueue("CORE-1", async () => {
    calls.push("second");
  });

  await Promise.resolve();
  expect(calls).toEqual(["first"]);

  releaseFirst();
  await first;
  await second;
  expect(calls).toEqual(["first", "second"]);
});
