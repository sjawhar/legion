import { expect, test } from "bun:test";

import { ApiError } from "../api/client";
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

function issueState(dismissed: string[], seq = 0): UserIssueState {
  return { dismissed, last_read_seq: 0, pinned: false, seq };
}

test("issue-state queue writes the full optimistic snapshot for each queued operation", async () => {
  const queue = new IssueStateWriteQueue();
  const writes: UserIssueState[] = [];
  const optimistic = issueState([
    "pinned_items:event:1",
    "pinned_items:event:2",
    "pinned_items:event:3",
  ]);
  const worker: IssueStateWriteWorker = {
    fetchState: async () => issueState([]),
    optimisticState: () => optimistic,
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      writes.push(state);
      return state;
    },
  };

  await Promise.all([
    queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker),
    queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker),
    queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker),
  ]);

  expect(writes.map(({ dismissed, seq }) => ({ dismissed, seq }))).toEqual([
    { dismissed: optimistic.dismissed, seq: 1 },
    { dismissed: optimistic.dismissed, seq: 2 },
    { dismissed: optimistic.dismissed, seq: 3 },
  ]);
});

test("issue-state queue retries a stale snapshot from the returned state", async () => {
  const queue = new IssueStateWriteQueue();
  const writes: UserIssueState[] = [];
  const optimistic = issueState(["pinned_items:event:1"]);
  const stale = issueState(["pinned_items:event:other"], 4);
  const worker: IssueStateWriteWorker = {
    fetchState: async () => issueState([]),
    optimisticState: () => optimistic,
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      writes.push(state);
      if (writes.length === 1) {
        throw new ApiError(409, { code: "STATE_STALE", state: stale });
      }
      return state;
    },
    staleState: (error) => (error instanceof ApiError ? error.state : undefined),
  };

  await queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);

  expect(writes.map(({ dismissed, seq }) => ({ dismissed, seq }))).toEqual([
    { dismissed: optimistic.dismissed, seq: 1 },
    { dismissed: optimistic.dismissed, seq: 5 },
  ]);
});

test("issue-state queue retains its one retry after a non-stale write failure", async () => {
  const queue = new IssueStateWriteQueue();
  const writes: UserIssueState[] = [];
  const optimistic = issueState(["pinned_items:event:1"]);
  let fetches = 0;
  const worker: IssueStateWriteWorker = {
    fetchState: async () => {
      fetches += 1;
      return issueState([], fetches - 1);
    },
    optimisticState: () => optimistic,
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      writes.push(state);
      if (writes.length === 1) {
        throw new Error("write failed");
      }
      return state;
    },
  };

  await queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);

  expect(fetches).toBe(2);
  expect(writes.map(({ seq }) => seq)).toEqual([1, 2]);
});

test("issue-state queue flushes optimistic state when the page hides during its initial read", async () => {
  const queue = new IssueStateWriteQueue();
  const initialRead = deferred();
  const writes: UserIssueState[] = [];
  const optimistic = issueState(["pinned_items:event:1"]);
  const worker: IssueStateWriteWorker = {
    fetchState: async () => {
      await initialRead.promise;
      return issueState([]);
    },
    optimisticState: () => optimistic,
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      writes.push(state);
      return state;
    },
  };

  const saved = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  window.dispatchEvent(new Event("pagehide"));

  await saved;
  expect(writes.map(({ dismissed, seq }) => ({ dismissed, seq }))).toEqual([
    { dismissed: optimistic.dismissed, seq: 1 },
  ]);
  initialRead.release();
});

test("issue-state queue rejects a delayed lower sequence after a teardown snapshot wins", async () => {
  const queue = new IssueStateWriteQueue();
  const firstStarted = deferred();
  const firstResponse = deferred();
  const writes: UserIssueState[] = [];
  let optimistic = issueState(["pinned_items:event:1"]);
  let saved = issueState([]);
  const worker: IssueStateWriteWorker = {
    fetchState: async () => issueState([]),
    optimisticState: () => optimistic,
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      writes.push(state);
      if (state.seq === 1) {
        firstStarted.release();
        await firstResponse.promise;
      }
      if (state.seq <= saved.seq) {
        throw new ApiError(409, { code: "STATE_STALE", state: saved });
      }
      saved = state;
      return state;
    },
    staleState: (error) => (error instanceof ApiError ? error.state : undefined),
  };

  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  await firstStarted.promise;
  optimistic = issueState(["pinned_items:event:1", "pinned_items:event:2"]);
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);
  window.dispatchEvent(new Event("pagehide"));

  await Promise.resolve();
  firstResponse.release();
  await Promise.all([first, second]);
  expect(writes.map(({ seq }) => seq)).toEqual([1, 2]);
  expect(saved.dismissed).toEqual(["pinned_items:event:1", "pinned_items:event:2"]);
});

test("issue-state queue sends a new restored-page operation after its older flush", async () => {
  const queue = new IssueStateWriteQueue();
  const initialRead = deferred();
  const flushResponse = deferred();
  const writes: UserIssueState[] = [];
  let optimistic = issueState(["pinned_items:event:1"]);
  let saved = issueState([]);
  const worker: IssueStateWriteWorker = {
    fetchState: async () => {
      await initialRead.promise;
      return saved;
    },
    optimisticState: () => optimistic,
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      writes.push(state);
      if (state.seq === 1) {
        await flushResponse.promise;
      }
      if (state.seq <= saved.seq) {
        throw new ApiError(409, { code: "STATE_STALE", state: saved });
      }
      saved = state;
      return state;
    },
    staleState: (error) => (error instanceof ApiError ? error.state : undefined),
  };

  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  window.dispatchEvent(new Event("pagehide"));
  const restored = new Event("pageshow");
  Object.defineProperty(restored, "persisted", { value: true });
  window.dispatchEvent(restored);
  optimistic = issueState(["pinned_items:event:1", "pinned_items:event:2"]);
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);

  flushResponse.release();
  await first;
  initialRead.release();
  await second;
  expect(writes.map(({ dismissed, seq }) => ({ dismissed, seq }))).toEqual([
    { dismissed: ["pinned_items:event:1"], seq: 1 },
    { dismissed: ["pinned_items:event:1", "pinned_items:event:2"], seq: 2 },
  ]);
});
