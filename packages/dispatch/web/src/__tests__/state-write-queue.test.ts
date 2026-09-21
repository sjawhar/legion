import { expect, test } from "bun:test";

import { ApiError } from "../api/client";
import type { UserIssueState } from "../api/types";
import {
  IssueStateWriteQueue,
  type IssueStateWriteWorker,
  type PinStateOperation,
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

test("issue-state queue writes one merged snapshot for rapid operations", async () => {
  const queue = new IssueStateWriteQueue();
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  const optimistic = issueState([
    "pinned_items:event:1",
    "pinned_items:event:2",
    "pinned_items:event:3",
  ]);
  const worker: IssueStateWriteWorker = {
    fetchState: async () => issueState([]),
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      writes.push(state);
      return { ...optimistic, ...state };
    },
  };

  await Promise.all([
    queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker),
    queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker),
    queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker),
  ]);

  expect(writes).toEqual([{ dismissed: optimistic.dismissed, seq: 1 }]);
});

test("issue-state queue merges stale state without overwriting unrelated fields", async () => {
  const queue = new IssueStateWriteQueue();
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  const optimistic = issueState(["pinned_items:event:1"]);
  const stale = { ...issueState(["pinned_items:event:other"], 4), last_read_seq: 42, pinned: true };
  let drained: UserIssueState | undefined;
  const worker: IssueStateWriteWorker = {
    fetchState: async () => issueState([]),
    onDrained: (_issueKey, state) => {
      drained = state;
    },
    onError: () => {},
    putState: async (_issueKey, state) => {
      writes.push(state);
      if (writes.length === 1) {
        throw new ApiError(409, { code: "STATE_STALE", state: stale });
      }
      return { ...stale, ...state };
    },
    staleState: (error) => (error instanceof ApiError ? error.state : undefined),
  };

  await queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);

  expect(writes).toEqual([
    { dismissed: optimistic.dismissed, seq: 1 },
    { dismissed: ["pinned_items:event:other", "pinned_items:event:1"], seq: 5 },
  ]);
  expect(drained).toMatchObject({ last_read_seq: 42, pinned: true });
  expect(writes.every((state) => !("pinned" in state) && !("last_read_seq" in state))).toBe(true);
});

test("issue-state queue retains its one retry after a non-stale write failure", async () => {
  const queue = new IssueStateWriteQueue();
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  const optimistic = issueState(["pinned_items:event:1"]);
  let fetches = 0;
  const worker: IssueStateWriteWorker = {
    fetchState: async () => {
      fetches += 1;
      return issueState([], fetches - 1);
    },
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      writes.push(state);
      if (writes.length === 1) {
        throw new Error("write failed");
      }
      return { ...optimistic, ...state };
    },
  };

  await queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);

  expect(fetches).toBe(2);
  expect(writes.map(({ seq }) => seq)).toEqual([1, 2]);
});

test("issue-state queue reports an unsaved operation when teardown has no authoritative state", async () => {
  const queue = new IssueStateWriteQueue();
  const initialRead = deferred();
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  const errors: PinStateOperation[][] = [];
  const optimistic = issueState(["pinned_items:event:1"]);
  const worker: IssueStateWriteWorker = {
    fetchState: async () => {
      await initialRead.promise;
      return issueState([]);
    },
    onDrained: () => {},
    onError: (_issueKey, operations) => errors.push(operations),
    putState: async (_issueKey, state) => {
      writes.push(state);
      return { ...optimistic, ...state };
    },
  };

  const outcome = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker).then(
    () => undefined,
    (error) => error
  );
  window.dispatchEvent(new Event("pagehide"));

  const error = await outcome;
  expect(error).toMatchObject({
    message: "Cannot save pinned items before their current state is loaded.",
  });
  expect(writes).toEqual([]);
  expect(errors).toEqual([[{ id: "event:1", op: "pin" }]]);

  const retried = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  initialRead.release();
  await retried;
  expect(writes).toEqual([{ dismissed: ["pinned_items:event:1"], seq: 1 }]);
});

test("issue-state queue starts a retry after a pre-GET rejection", async () => {
  const queue = new IssueStateWriteQueue();
  const initialRead = deferred();
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  const optimistic = issueState(["pinned_items:event:1"]);
  const worker: IssueStateWriteWorker = {
    fetchState: async () => {
      await initialRead.promise;
      return issueState([]);
    },
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      writes.push(state);
      return { ...optimistic, ...state };
    },
  };

  const rejected = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker).then(
    () => undefined,
    (error) => error
  );
  window.dispatchEvent(new Event("pagehide"));
  expect(await rejected).toBeInstanceOf(Error);

  const retried = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  initialRead.release();
  await retried;
  expect(writes).toEqual([{ dismissed: ["pinned_items:event:1"], seq: 1 }]);
});

test("issue-state queue rejects a delayed lower sequence after a teardown snapshot wins", async () => {
  const queue = new IssueStateWriteQueue();
  const firstStarted = deferred();
  const firstResponse = deferred();
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  let saved = issueState([]);
  const worker: IssueStateWriteWorker = {
    fetchState: async () => issueState([]),
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
      saved = { ...saved, ...state };
      return saved;
    },
    staleState: (error) => (error instanceof ApiError ? error.state : undefined),
  };

  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  await firstStarted.promise;
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);
  window.dispatchEvent(new Event("pagehide"));

  await Promise.resolve();
  firstResponse.release();
  await Promise.all([first, second]);
  expect(writes.map(({ seq }) => seq)).toEqual([1, 2]);
  expect(saved.dismissed).toEqual(["pinned_items:event:1", "pinned_items:event:2"]);
});

test("issue-state queue ignores an obsolete retry after a newer queue starts", async () => {
  const queue = new IssueStateWriteQueue();
  const oldRetry = Promise.withResolvers<UserIssueState>();
  const newRead = Promise.withResolvers<UserIssueState>();
  const existing = issueState(["pinned_items:event:existing"], 10);
  let gets = 0;
  let puts = 0;
  let saved = existing;
  const worker: IssueStateWriteWorker = {
    fetchState: async () => {
      gets += 1;
      if (gets === 2) {
        return oldRetry.promise;
      }
      if (gets === 3) {
        return newRead.promise;
      }
      return saved;
    },
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      puts += 1;
      if (puts === 1) {
        throw new Error("first write failed");
      }
      saved = { ...saved, ...state };
      return saved;
    },
  };

  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  for (let microtask = 0; microtask < 10; microtask++) {
    await Promise.resolve();
  }
  expect(gets).toBe(2);
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);
  window.dispatchEvent(new Event("pagehide"));
  await Promise.all([first, second]);

  const third = queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker);
  for (let microtask = 0; microtask < 10; microtask++) {
    await Promise.resolve();
  }
  expect(gets).toBe(3);
  oldRetry.reject(new Error("obsolete retry GET failed"));
  newRead.resolve(saved);
  await third;

  expect(puts).toBe(3);
  expect(saved.dismissed).toEqual([
    "pinned_items:event:existing",
    "pinned_items:event:1",
    "pinned_items:event:2",
    "pinned_items:event:3",
  ]);
});

test("issue-state queue ignores a superseded write response after flush ownership transfers", async () => {
  const queue = new IssueStateWriteQueue();
  const firstResponse = deferred();
  const flushResponse = deferred();
  const thirdResponse = deferred();
  const existing = issueState(["pinned_items:event:existing"], 10);
  let puts = 0;
  let saved = existing;
  const worker: IssueStateWriteWorker = {
    fetchState: async () => saved,
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      puts += 1;
      saved = { ...saved, ...state };
      if (puts === 1) {
        await firstResponse.promise;
      } else if (puts === 2) {
        await flushResponse.promise;
      } else {
        await thirdResponse.promise;
      }
      return saved;
    },
  };

  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  for (let microtask = 0; microtask < 10; microtask++) {
    await Promise.resolve();
  }
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);
  window.dispatchEvent(new Event("pagehide"));
  flushResponse.release();
  await Promise.all([first, second]);

  const third = queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker);
  for (let microtask = 0; microtask < 10; microtask++) {
    await Promise.resolve();
  }
  expect(puts).toBe(3);
  firstResponse.release();
  thirdResponse.release();
  await third;
  expect(saved.dismissed).toEqual([
    "pinned_items:event:existing",
    "pinned_items:event:1",
    "pinned_items:event:2",
    "pinned_items:event:3",
  ]);
});
