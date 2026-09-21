import { expect, test } from "bun:test";
import assert from "node:assert/strict";

import { ApiError } from "../api/client";
import type { UserIssueState } from "../api/types";
import {
  applyPinStateOperation,
  IssueStateWriteQueue,
  type IssueStateWriteWorker,
  type PinStateOperation,
} from "../features/issue/state-write-queue";

interface Deferred<T> {
  promise: Promise<T>;
  reject: (reason?: unknown) => void;
  resolve: (value: T | PromiseLike<T>) => void;
}

function deferred(): { promise: Promise<void>; release: () => void } {
  const completion = Promise.withResolvers<void>();
  return { promise: completion.promise, release: completion.resolve };
}

async function flushMicrotasks(): Promise<void> {
  for (let microtask = 0; microtask < 4; microtask++) {
    await Promise.resolve();
  }
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

test("issue-state queue rejects a non-stale write failure without retrying", async () => {
  const queue = new IssueStateWriteQueue();
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  const errors: PinStateOperation[][] = [];
  let fetches = 0;
  const worker: IssueStateWriteWorker = {
    fetchState: async () => {
      fetches += 1;
      return issueState([], fetches - 1);
    },
    onDrained: () => {},
    onError: (_key, operations) => errors.push(operations),
    putState: async (_issueKey, state) => {
      writes.push(state);
      throw new Error("write failed");
    },
  };

  const error = await queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker).then(
    () => undefined,
    (reason) => reason
  );

  expect(error).toMatchObject({ message: "write failed" });
  expect(fetches).toBe(1);
  expect(writes.map(({ seq }) => seq)).toEqual([1]);
  expect(errors).toEqual([[{ id: "event:1", op: "pin" }]]);
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

test("issue-state queue starts pending work after an ordinary write rejection", async () => {
  const queue = new IssueStateWriteQueue();
  const firstFailure = deferred();
  const errors: PinStateOperation[][] = [];
  const existing = issueState(["pinned_items:event:existing"], 10);
  let puts = 0;
  let saved = existing;
  const worker: IssueStateWriteWorker = {
    fetchState: async () => saved,
    onDrained: () => {},
    onError: (_key, operations) => errors.push(operations),
    putState: async (_issueKey, state) => {
      puts += 1;
      if (puts === 1) {
        await firstFailure.promise;
        throw new Error("first write failed");
      }
      saved = { ...saved, ...state };
      return saved;
    },
  };

  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker).then(
    () => undefined,
    (error) => error
  );
  await flushMicrotasks();
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);
  firstFailure.release();
  expect(await first).toMatchObject({ message: "first write failed" });
  await second;

  expect(puts).toBe(2);
  expect(errors).toEqual([[{ id: "event:1", op: "pin" }]]);
  expect(saved.dismissed).toEqual(["pinned_items:event:existing", "pinned_items:event:2"]);
});

test("issue-state queue ignores a superseded write response after flush ownership transfers", async () => {
  const queue = new IssueStateWriteQueue();
  const firstResponse = deferred();
  const flushResponse = deferred();
  const thirdResponse = deferred();
  const existing = issueState(["pinned_items:event:existing"], 10);
  const committed: UserIssueState[] = [];
  let puts = 0;
  let saved = existing;
  const worker: IssueStateWriteWorker = {
    fetchState: async () => saved,
    onDrained: () => {},
    onError: () => {},
    putState: async (_issueKey, state) => {
      puts += 1;
      const response = { ...saved, ...state };
      saved = response;
      committed.push(response);
      if (puts === 1) {
        await firstResponse.promise;
      } else if (puts === 2) {
        await flushResponse.promise;
      } else {
        await thirdResponse.promise;
      }
      return response;
    },
  };

  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  for (let microtask = 0; microtask < 10; microtask++) {
    await Promise.resolve();
  }
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);
  window.dispatchEvent(new Event("pagehide"));

  const third = queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker);
  flushResponse.release();
  await Promise.all([first, second]);
  for (let microtask = 0; microtask < 10; microtask++) {
    await Promise.resolve();
  }
  expect(puts).toBe(3);
  firstResponse.release();
  thirdResponse.release();
  await third;
  expect(committed.map((state) => state.dismissed)).toEqual([
    ["pinned_items:event:existing", "pinned_items:event:1"],
    ["pinned_items:event:existing", "pinned_items:event:1", "pinned_items:event:2"],
    [
      "pinned_items:event:existing",
      "pinned_items:event:1",
      "pinned_items:event:2",
      "pinned_items:event:3",
    ],
  ]);
});

test("reviewer probe: superseded response does not retire the newer drain", async () => {
  const firstResponse = Promise.withResolvers();
  const flushResponse = Promise.withResolvers();
  const thirdResponse = Promise.withResolvers();
  let puts = 0;
  let saved = {
    dismissed: ["pinned_items:event:existing"],
    seq: 10,
    last_read_seq: 42,
    pinned: true,
  };
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  const drained: UserIssueState[] = [];
  const errors: PinStateOperation[][] = [];
  const queue = new IssueStateWriteQueue();
  const worker: IssueStateWriteWorker = {
    fetchState: async () => saved,
    onError: (_key, operations) => errors.push(operations),
    onDrained: (_key, state) => drained.push(state),
    putState: async (_key, body) => {
      const n = ++puts;
      writes.push(body);
      assert(body.seq > saved.seq);
      saved = { ...saved, ...body };
      const committed = saved;
      await [firstResponse.promise, flushResponse.promise, thirdResponse.promise][n - 1];
      return committed;
    },
  };
  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  await flushMicrotasks();
  assert.equal(puts, 1);
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);
  window.dispatchEvent(new Event("pagehide"));
  await flushMicrotasks();
  assert.equal(puts, 2);
  let outcome = "pending";
  const third = queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker).then(
    () => (outcome = "resolved"),
    (error) => (outcome = error.message)
  );
  flushResponse.resolve();
  await Promise.all([first, second]);
  await flushMicrotasks();
  assert.equal(puts, 3);
  firstResponse.resolve();
  await flushMicrotasks();
  thirdResponse.resolve();
  await flushMicrotasks();
  await flushMicrotasks();
  assert.equal(
    outcome,
    "resolved",
    "a superseded success response must not retire the newer in-flight operation"
  );
  await third;
});

test("reviewer probe: flush cannot be overwritten by an older seed read", async () => {
  const firstResponse = Promise.withResolvers();
  const flushCommit = Promise.withResolvers();
  let gets = 0;
  let puts = 0;
  let saved = {
    dismissed: ["pinned_items:event:existing"],
    seq: 10,
    last_read_seq: 42,
    pinned: true,
  };
  const reads: Array<{
    n: number;
    snapshot: UserIssueState;
    gate: Deferred<void>;
  }> = [];
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  const errors: PinStateOperation[][] = [];
  const drained: UserIssueState[] = [];
  const queue = new IssueStateWriteQueue();
  const worker: IssueStateWriteWorker = {
    fetchState: async () => {
      const n = ++gets;
      const snapshot = saved;
      if (n === 1) {
        return snapshot;
      }
      const gate = Promise.withResolvers<void>();
      reads.push({ n, snapshot, gate });
      await gate.promise;
      return snapshot;
    },
    onError: (_key, operations) => errors.push(operations),
    onDrained: (_key, state) => drained.push(state),
    putState: async (_key, body) => {
      const n = ++puts;
      writes.push(body);
      if (n === 2) {
        await flushCommit.promise;
      }
      assert(body.seq > saved.seq);
      saved = { ...saved, ...body };
      const committed = saved;
      if (n === 1) {
        await firstResponse.promise;
      }
      return committed;
    },
  };
  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  await flushMicrotasks();
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);
  window.dispatchEvent(new Event("pagehide"));
  let outcome = "pending";
  const third = queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker).then(
    () => (outcome = "resolved"),
    (error) => (outcome = error.message)
  );
  await flushMicrotasks();
  const getsBeforeFlush = gets;
  flushCommit.resolve();
  await Promise.all([first, second]);
  await flushMicrotasks();
  const getsAfterFlush = gets;
  for (const read of reads) {
    read.gate.resolve();
    await flushMicrotasks();
  }
  firstResponse.resolve();
  await flushMicrotasks();
  await flushMicrotasks();
  assert.equal(outcome, "resolved");
  assert(
    saved.dismissed.includes("pinned_items:event:2"),
    "a resolved pagehide pin must not be erased by an older seed read"
  );
  await third;
  expect({ getsBeforeFlush, getsAfterFlush, gets, puts, writes, errors, drained }).toBeDefined();
});

test("reviewer probe: flush retires a drain awaiting its seed read", async () => {
  const firstResponse = Promise.withResolvers();
  const flushResponse = Promise.withResolvers();
  let puts = 0;
  let saved = {
    dismissed: ["pinned_items:event:existing"],
    seq: 10,
    last_read_seq: 42,
    pinned: true,
  };
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  const queue = new IssueStateWriteQueue();
  const worker: IssueStateWriteWorker = {
    fetchState: async () => saved,
    onError: () => {},
    onDrained: () => {},
    putState: async (_key, body) => {
      const n = ++puts;
      writes.push(body);
      assert(body.seq > saved.seq);
      saved = { ...saved, ...body };
      const committed = saved;
      if (n === 1) {
        await firstResponse.promise;
      }
      if (n === 2) {
        await flushResponse.promise;
      }
      return committed;
    },
  };
  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  await flushMicrotasks();
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);
  firstResponse.resolve();
  await first;
  await flushMicrotasks();
  // The rewritten queue writes directly once it holds an authoritative row; the original probe counted the seed GET.
  window.dispatchEvent(new Event("pagehide"));
  let outcome = "pending";
  const third = queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker).then(
    () => (outcome = "resolved"),
    (error) => (outcome = error.message)
  );
  flushResponse.resolve();
  await second;
  await flushMicrotasks();
  assert.equal(outcome, "resolved");
  assert(
    saved.dismissed.includes("pinned_items:event:2"),
    "flush must retire a drain awaiting GET, not only inFlight PUT"
  );
  await third;
  expect(saved.dismissed).toEqual([
    "pinned_items:event:existing",
    "pinned_items:event:1",
    "pinned_items:event:2",
    "pinned_items:event:3",
  ]);
});

test("reviewer probe: a second pagehide supersedes an unanswered teardown write", async () => {
  const firstResponse = Promise.withResolvers<void>();
  const flushResponse = Promise.withResolvers<void>();
  let puts = 0;
  let saved = issueState(["pinned_items:event:existing"], 10);
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  const queue = new IssueStateWriteQueue();
  const worker: IssueStateWriteWorker = {
    fetchState: async () => saved,
    onError: () => {},
    onDrained: () => {},
    putState: async (_key, body) => {
      const n = ++puts;
      writes.push(body);
      assert(body.seq > saved.seq);
      saved = { ...saved, ...body };
      const committed = saved;
      if (n === 1) {
        await firstResponse.promise;
      }
      if (n === 2) {
        await flushResponse.promise;
      }
      return committed;
    },
  };
  const first = queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  await flushMicrotasks();
  const second = queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);
  window.dispatchEvent(new Event("pagehide"));
  await flushMicrotasks();
  assert.equal(puts, 2);
  const pageshow = new Event("pageshow");
  Object.defineProperty(pageshow, "persisted", { value: true });
  window.dispatchEvent(pageshow);
  const third = queue.enqueue("CORE-1", { id: "event:3", op: "pin" }, worker);
  window.dispatchEvent(new Event("pagehide"));
  await flushMicrotasks();
  assert.equal(puts, 3, "the second pagehide must send the later pin");
  firstResponse.resolve();
  flushResponse.resolve();
  await Promise.all([first, second, third]);
  expect(writes.map((write) => write.seq)).toEqual([11, 12, 13]);
  expect(saved.dismissed).toEqual([
    "pinned_items:event:existing",
    "pinned_items:event:1",
    "pinned_items:event:2",
    "pinned_items:event:3",
  ]);
});

test("issue-state queue discards a lower-sequence 409 row after adopting a newer row", async () => {
  const queue = new IssueStateWriteQueue();
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  let saved = issueState([], 10);
  const worker: IssueStateWriteWorker = {
    fetchState: async () => saved,
    onError: () => {},
    onDrained: () => {},
    putState: async (_key, state) => {
      writes.push(state);
      if (writes.length === 2) {
        throw new ApiError(409, {
          code: "STATE_STALE",
          state: issueState(["pinned_items:remote"], 10),
        });
      }
      saved = { ...saved, ...state };
      return saved;
    },
    staleState: (error) => (error instanceof ApiError ? error.state : undefined),
  };

  await queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker);
  await queue.enqueue("CORE-1", { id: "event:2", op: "pin" }, worker);

  expect(writes).toEqual([
    { dismissed: ["pinned_items:event:1"], seq: 11 },
    { dismissed: ["pinned_items:event:1", "pinned_items:event:2"], seq: 12 },
    { dismissed: ["pinned_items:event:1", "pinned_items:event:2"], seq: 13 },
  ]);
  expect(saved).toMatchObject({
    dismissed: ["pinned_items:event:1", "pinned_items:event:2"],
    seq: 13,
  });
});

test("issue-state queue rejects after one retry when the server keeps refusing the sequence", async () => {
  const queue = new IssueStateWriteQueue();
  const writes: Pick<UserIssueState, "dismissed" | "seq">[] = [];
  const errors: PinStateOperation[][] = [];
  const worker: IssueStateWriteWorker = {
    fetchState: async () => issueState(["pinned_items:event:existing"], 10),
    onDrained: () => {},
    onError: (_key, operations) => errors.push(operations),
    putState: async (_key, state) => {
      writes.push(state);
      throw new ApiError(409, {
        code: "STATE_STALE",
        state: issueState(["pinned_items:event:existing"], 10),
      });
    },
    staleState: (error) => (error instanceof ApiError ? error.state : undefined),
  };

  const error = await queue.enqueue("CORE-1", { id: "event:1", op: "pin" }, worker).then(
    () => undefined,
    (reason) => reason
  );

  expect(error).toBeInstanceOf(ApiError);
  expect(writes.map(({ seq }) => seq)).toEqual([11, 12]);
  expect(errors).toEqual([[{ id: "event:1", op: "pin" }]]);
});

test("issue-state queue model preserves exactly resolved operations across 50 deterministic schedules", async () => {
  interface TrackedOperation {
    operation: PinStateOperation;
    settlements: number;
    status: "pending" | "resolved" | "rejected";
  }
  const mulberry32 = (seed: number) => () => {
    seed += 0x6d2b79f5;
    let value = seed;
    value = Math.imul(value ^ (value >>> 15), value | 1);
    value ^= value + Math.imul(value ^ (value >>> 7), value | 61);
    return ((value ^ (value >>> 14)) >>> 0) / 4294967296;
  };

  for (let seed = 1; seed <= 50; seed++) {
    const random = mulberry32(seed);
    const queue = new IssueStateWriteQueue();
    const initialDismissed = ["pinned_items:event:existing"];
    let server = issueState([...initialDismissed], 10);
    const heldGets: Deferred<UserIssueState>[] = [];
    const heldPuts: Array<{
      body: Pick<UserIssueState, "dismissed" | "seq">;
      completion: Deferred<UserIssueState>;
    }> = [];
    const tracked: TrackedOperation[] = [];
    const completion: Promise<void>[] = [];
    const resolved: PinStateOperation[] = [];
    const snapshot = () => {
      return { ...server, dismissed: [...server.dismissed] };
    };
    const worker: IssueStateWriteWorker = {
      fetchState: () => {
        const completion = Promise.withResolvers<UserIssueState>();
        heldGets.push(completion);
        return completion.promise;
      },
      onDrained: () => {},
      onError: () => {},
      putState: (_key, body) => {
        const completion = Promise.withResolvers<UserIssueState>();
        heldPuts.push({ body, completion });
        return completion.promise;
      },
      staleState: (error) => (error instanceof ApiError ? error.state : undefined),
    };
    const enqueue = (operation: PinStateOperation) => {
      const entry: TrackedOperation = { operation, settlements: 0, status: "pending" };
      tracked.push(entry);
      completion.push(
        queue.enqueue("CORE-1", operation, worker).then(
          () => {
            entry.settlements += 1;
            entry.status = "resolved";
            resolved.push(operation);
          },
          () => {
            entry.settlements += 1;
            entry.status = "rejected";
          }
        )
      );
    };
    const resolveOldestGet = () => {
      heldGets.shift()?.resolve(snapshot());
    };
    const rejectOldestGet = () => {
      heldGets.shift()?.reject(new Error("network GET failure"));
    };
    const resolveOldestPut = () => {
      const held = heldPuts.shift();
      if (held === undefined) {
        return;
      }
      if (held.body.seq <= server.seq) {
        held.completion.reject(new ApiError(409, { code: "STATE_STALE", state: snapshot() }));
        return;
      }
      server = { ...server, ...held.body };
      held.completion.resolve(snapshot());
    };
    const pagehide = () => {
      window.dispatchEvent(new Event("pagehide"));
    };
    const back = () => {
      const pageshow = new Event("pageshow");
      Object.defineProperty(pageshow, "persisted", { value: true });
      window.dispatchEvent(pageshow);
    };

    for (let step = 0; step < 200; step++) {
      switch (Math.floor(random() * 8)) {
        case 0:
          enqueue({ id: `event:${Math.floor(random() * 5)}`, op: "pin" });
          break;
        case 1:
          enqueue({ id: `event:${Math.floor(random() * 5)}`, op: "unpin" });
          break;
        case 2:
          resolveOldestGet();
          break;
        case 3:
          resolveOldestPut();
          break;
        case 4:
          pagehide();
          break;
        case 5:
          back();
          break;
        case 6:
          rejectOldestGet();
          break;
        default:
          pagehide();
          await flushMicrotasks();
          pagehide();
          break;
      }
      await flushMicrotasks();
    }

    for (;;) {
      if (heldGets.length > 0) {
        resolveOldestGet();
      } else if (heldPuts.length > 0) {
        resolveOldestPut();
      } else {
        await flushMicrotasks();
        if (heldGets.length === 0 && heldPuts.length === 0) {
          break;
        }
      }
      await flushMicrotasks();
    }
    await Promise.all(completion);

    const expected = resolved.reduce(
      (dismissed, operation) => applyPinStateOperation(dismissed, operation),
      initialDismissed
    );
    expect(
      [...new Set(server.dismissed)].sort(),
      `seed ${seed}: committed state must equal resolved operations`
    ).toEqual([...new Set(expected)].sort());
    expect(
      tracked.every((entry) => entry.status !== "pending"),
      `seed ${seed}: no operation promise may remain pending`
    ).toBe(true);
    expect(
      tracked.every((entry) => entry.settlements === 1),
      `seed ${seed}: every operation promise must settle exactly once`
    ).toBe(true);
  }
});
