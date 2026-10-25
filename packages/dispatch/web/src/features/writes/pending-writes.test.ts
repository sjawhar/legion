import { expect, jest, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";

import { keptTab } from "../../__tests__/kept-sends-harness";
import {
  AnswerNotKeptError,
  ApiError,
  api,
  RequestTimeoutError,
  type WriteBounds,
} from "../../api/client";
import { SendInterruptedError, SendStoppedError } from "./keyed-write";
import { PendingWrites } from "./pending-writes";
import { KEPT_SEND_RESUME_MS } from "./write-storage";

/** A page's query client, signed in: the queue stamps every kept write with this login and
 *  resumes only this login's, as the app's does. */
function signedIn(login = "alice"): QueryClient {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login });
  return client;
}

/** Lets every promise the queue chained run as far as it goes without the clock moving. */
async function settle(): Promise<void> {
  for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
}

/** Runs `body` on the fake clock, which moves a millisecond before the real one comes back: a
 *  timer left at zero delay would otherwise hang a later test's `waitFor`. */
async function onFakeClock(body: () => Promise<void>): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
  }
}

/** What the reader did, as the queue is handed it: a status pick on CORE-1. */
function statusWrite(writes: PendingWrites): Promise<unknown> {
  return writes.run({
    label: "CORE-1 set to done",
    write: (bounds) => api.patchIssue("CORE-1", { status: "done" }, bounds),
  });
}

/** What `api.patchIssue` does to its bounds on the real path: it tells `sent` the request as it
 *  goes out, which is what the queue keeps for a reload. `answer` settles the attempt. */
function attempt(
  answer: Promise<unknown>
): (key: string, input: unknown, bounds: WriteBounds) => Promise<never> {
  return (key, input, bounds) => {
    bounds.sent?.({ body: input, method: "PATCH", path: `/api/v1/issues/${key}` });
    return answer as Promise<never>;
  };
}

const refusal = new ApiError(503, { code: "UNAVAILABLE", error: "the server is down" });
const verdict = new ApiError(422, { code: "KEY_REUSED", error: "the key names another write" });
const inFlight = new ApiError(409, { code: "KEY_IN_FLIGHT", error: "still running" });

// One press is one key, and every attempt of that press carries it, so however many attempts
// reach Dispatch it is one write there. Only a failure short of Dispatch's verdict is tried
// again: a fetch that failed, an attempt that timed out, a 5xx, a 429, and the 409 that says
// Dispatch is still running the first request under this key.
test("one key per press, reused by every attempt the queue makes", async () => {
  await onFakeClock(async () => {
    const patchIssue = spyOn(api, "patchIssue")
      .mockRejectedValueOnce(new TypeError("Failed to fetch"))
      .mockRejectedValueOnce(new RequestTimeoutError("/api/v1/issues/CORE-1", 15_000))
      .mockRejectedValueOnce(refusal)
      .mockRejectedValueOnce(new ApiError(429, { code: "RATE_LIMITED", error: "slow down" }))
      .mockRejectedValueOnce(inFlight)
      .mockResolvedValueOnce({ key: "CORE-1" } as never);
    const writes = new PendingWrites(signedIn(), keptTab().page());

    try {
      const saved = statusWrite(writes);
      for (const wait of [1_000, 2_000, 4_000, 8_000, 16_000]) {
        await settle();
        jest.advanceTimersByTime(wait);
      }
      await settle();
      expect(await saved).toEqual({ key: "CORE-1" });

      const keys = patchIssue.mock.calls.map(([, , bounds]) => (bounds as WriteBounds).key);
      expect(keys).toHaveLength(6);
      expect(new Set(keys).size).toBe(1);
      expect(writes.rows()).toEqual([]);

      // A second press is a second action, under a key of its own.
      patchIssue.mockResolvedValueOnce({ key: "CORE-1" } as never);
      await statusWrite(writes);
      expect(patchIssue.mock.calls.at(-1)?.[2]?.key).not.toBe(keys[0]);
    } finally {
      patchIssue.mockRestore();
    }
  });
});

// Dispatch's verdict is the end of the write: a 4xx it answered is not tried again, the row stays
// for the reader with that refusal, and Discard drops it.
test("a terminal 4xx refuses at once and waits for the reader", async () => {
  await onFakeClock(async () => {
    const patchIssue = spyOn(api, "patchIssue").mockRejectedValue(verdict);
    const writes = new PendingWrites(signedIn(), keptTab().page());

    try {
      const refused = statusWrite(writes).catch((error: unknown) => error);
      await settle();
      expect(await refused).toBe(verdict);
      expect(patchIssue).toHaveBeenCalledTimes(1);
      const [row] = writes.rows();
      expect(row?.status).toBe("refused");
      expect(row?.label).toBe("CORE-1 set to done");
      expect(row?.refusal).toBe(verdict);

      jest.advanceTimersByTime(30 * 60_000);
      await settle();
      expect(patchIssue).toHaveBeenCalledTimes(1);

      expect(writes.discard(row?.id ?? 0)).toBe(true);
      expect(writes.rows()).toEqual([]);
    } finally {
      patchIssue.mockRestore();
    }
  });
});

// Past its deadline a write is late, and the reader may stop it: no attempt follows the one in
// flight, the write ends with `SendStoppedError`, and Retry asks again under the same key, since
// that first attempt may have reached Dispatch. A Retry after Dispatch's own refusal mints a new
// key instead, because Dispatch replays the refusal it stored under the old one.
test("Stop trying ends a late write, and Retry keeps its key until Dispatch has refused", async () => {
  await onFakeClock(async () => {
    const patchIssue = spyOn(api, "patchIssue").mockRejectedValue(refusal);
    const writes = new PendingWrites(signedIn(), keptTab().page());

    try {
      const stopped = statusWrite(writes).catch((error: unknown) => error);
      for (const wait of [1_000, 2_000, 4_000, 8_000, 16_000]) {
        await settle();
        jest.advanceTimersByTime(wait);
      }
      await settle();
      const late = writes.rows()[0];
      expect(late?.status).toBe("late");
      const attemptsBeforeStop = patchIssue.mock.calls.length;
      const key = patchIssue.mock.calls[0]?.[2]?.key;

      expect(writes.stop(late?.id ?? 0)).toBe(true);
      await settle();
      jest.advanceTimersByTime(30 * 60_000);
      await settle();
      expect(await stopped).toBeInstanceOf(SendStoppedError);
      expect(patchIssue).toHaveBeenCalledTimes(attemptsBeforeStop);
      const [row] = writes.rows();
      expect(row?.status).toBe("refused");

      // Retry after a stop: the same key, since Dispatch gave no verdict.
      patchIssue.mockRejectedValueOnce(verdict);
      writes.retry(row?.id ?? 0);
      await settle();
      expect(patchIssue.mock.calls.at(-1)?.[2]?.key).toBe(key);

      // Retry after Dispatch's refusal: a key of its own.
      patchIssue.mockResolvedValueOnce({ key: "CORE-1" } as never);
      writes.retry(writes.rows()[0]?.id ?? 0);
      await settle();
      expect(patchIssue.mock.calls.at(-1)?.[2]?.key).not.toBe(key);
      expect(writes.rows()).toEqual([]);
    } finally {
      patchIssue.mockRestore();
    }
  });
});

// A write is kept from its first attempt, so a reload takes it up again under its key - which
// Dispatch answers with what the first request did - and makes it once, not once per load. One
// pressed longer ago than five minutes is not made again on its own: it comes back refused, for
// the reader to retry under its key or discard.
test("a write survives a reload and resumes once, and an old one comes back refused", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const unanswered = Promise.withResolvers<never>().promise;
    const patchIssue = spyOn(api, "patchIssue").mockImplementation(attempt(unanswered));
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    const before = new PendingWrites(signedIn(), tab.page());

    try {
      void statusWrite(before).catch(() => undefined);
      await settle();
      const key = patchIssue.mock.calls[0]?.[2]?.key ?? "";
      expect(key).not.toBe("");
      expect(tab.kept(key)).toMatchObject({ kind: "write", login: "alice", state: "sending" });

      const after = new PendingWrites(signedIn(), tab.page());
      after.resume("alice");
      await settle();
      expect(send).toHaveBeenCalledTimes(1);
      expect(send.mock.calls[0]?.[0]).toEqual({
        body: { status: "done" },
        method: "PATCH",
        path: "/api/v1/issues/CORE-1",
      });
      expect(send.mock.calls[0]?.[1]?.key).toBe(key);
      await waitFor(() => expect(after.rows()).toEqual([]));
      expect(tab.kept(key)).toBeUndefined();

      // Another write, pressed longer ago than the resume bound: asked about, never sent.
      const old = new PendingWrites(signedIn(), tab.page());
      void statusWrite(old).catch(() => undefined);
      await settle();
      const oldKey = patchIssue.mock.calls.at(-1)?.[2]?.key ?? "";
      const entry = tab.kept(oldKey) as { pressedAt: number };
      tab.storage.setItem(
        `dispatch.write:${oldKey}`,
        JSON.stringify({ ...entry, pressedAt: Date.now() - KEPT_SEND_RESUME_MS - 1 })
      );

      const later = new PendingWrites(signedIn(), tab.page());
      later.resume("alice");
      await settle();
      expect(send).toHaveBeenCalledTimes(1);
      expect(later.rows()[0]?.refusal).toBeInstanceOf(SendInterruptedError);
      expect(later.rows()[0]?.label).toBe("CORE-1 set to done");
    } finally {
      patchIssue.mockRestore();
      send.mockRestore();
      tab.close();
    }
  });
});

// A kept write belongs to the person who made it: another login's is dropped unmade, so a second
// person signing in at the same browser never sends what the first did.
test("a write of another login is dropped rather than resumed", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const patchIssue = spyOn(api, "patchIssue").mockImplementation(
      attempt(Promise.withResolvers<never>().promise)
    );
    const send = spyOn(api, "send").mockResolvedValue({ key: "CORE-1" } as never);
    const alice = new PendingWrites(signedIn("alice"), tab.page());

    try {
      void statusWrite(alice).catch(() => undefined);
      await settle();
      const key = patchIssue.mock.calls[0]?.[2]?.key ?? "";
      expect(tab.kept(key)).toMatchObject({ login: "alice" });

      const bob = new PendingWrites(signedIn("bob"), tab.page());
      bob.resume("bob");
      await settle();
      expect(send).not.toHaveBeenCalled();
      expect(bob.rows()).toEqual([]);
      expect(tab.kept(key)).toBeUndefined();
    } finally {
      patchIssue.mockRestore();
      send.mockRestore();
      tab.close();
    }
  });
});

// A surface that shows its own refusal - a form the reader is still in front of - keeps the write
// in the indicator only while it is out, so one refusal is never said twice.
test("an inline write leaves the indicator once it is refused", async () => {
  await onFakeClock(async () => {
    const patchIssue = spyOn(api, "patchIssue").mockRejectedValue(verdict);
    const writes = new PendingWrites(signedIn(), keptTab().page());

    try {
      const refused = writes
        .run({
          inline: true,
          label: "CORE-1's title saved",
          write: (bounds) => api.patchIssue("CORE-1", { title: "Ship" }, bounds),
        })
        .catch((error: unknown) => error);
      expect(writes.rows().map((row) => row.label)).toEqual(["CORE-1's title saved"]);
      await settle();

      expect(await refused).toBe(verdict);
      expect(writes.rows()).toEqual([]);
    } finally {
      patchIssue.mockRestore();
    }
  });
});

// A PUT or a DELETE is the same write however often it is made, so it carries no key, but it runs
// the same loop: a pin Dispatch could not take (a 503) is tried again and lands.
test("a PUT goes through the same loop, tried again until Dispatch takes it", async () => {
  await onFakeClock(async () => {
    const putIssueState = spyOn(api, "putIssueState")
      .mockRejectedValueOnce(refusal)
      .mockResolvedValueOnce({ pinned: true } as never);
    const writes = new PendingWrites(signedIn(), keptTab().page());

    try {
      const pinned = writes.run({
        label: "CORE-1 pinned",
        write: (bounds) => api.putIssueState("CORE-1", { pinned: true }, bounds),
      });
      await settle();
      expect(putIssueState).toHaveBeenCalledTimes(1);
      jest.advanceTimersByTime(1_000);
      await settle();
      expect((await pinned).pinned).toBe(true);
      expect(putIssueState).toHaveBeenCalledTimes(2);
      expect(writes.rows()).toEqual([]);
    } finally {
      putIssueState.mockRestore();
    }
  });
});

// The reader was stopping a write when the page reloaded, so its last attempt went with the page.
// The reload asks once more under its key - its stop already given - and makes no attempt after
// that one: it lands if Dispatch takes it, and otherwise ends stopped, with Retry and Discard.
test("a write the reader was stopping resumes with exactly one attempt", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const patchIssue = spyOn(api, "patchIssue").mockImplementation(
      attempt(Promise.reject(refusal))
    );
    const send = spyOn(api, "send").mockRejectedValue(refusal);
    const before = new PendingWrites(signedIn(), tab.page());

    try {
      void statusWrite(before).catch(() => undefined);
      for (const wait of [1_000, 2_000, 4_000, 8_000, 16_000]) {
        await settle();
        jest.advanceTimersByTime(wait);
      }
      await settle();
      const late = before.rows()[0];
      expect(late?.status).toBe("late");
      expect(before.stop(late?.id ?? 0)).toBe(true);
      const key = patchIssue.mock.calls[0]?.[2]?.key ?? "";
      expect(tab.kept(key)).toMatchObject({ state: "stopping" });

      const after = new PendingWrites(signedIn(), tab.page());
      after.resume("alice");
      await settle();
      jest.advanceTimersByTime(30 * 60_000);
      await settle();
      expect(send).toHaveBeenCalledTimes(1);
      expect(send.mock.calls[0]?.[1]?.key).toBe(key);
      expect(after.rows()[0]?.status).toBe("refused");
      expect(after.rows()[0]?.refusal).toBeInstanceOf(SendStoppedError);
    } finally {
      patchIssue.mockRestore();
      send.mockRestore();
      tab.close();
    }
  });
});

// The write landed, and its answer was lost before Dispatch stored it: a repeat under the key is
// answered `409 KEY_COMMITTED`, and Dispatch will not run it again - after a dropped attempt or
// a 5xx alike. The loop takes that as landed: no further attempt, no refusal, the entry gone from
// the queue and from what a reload keeps, and every read refreshed, since there is no answer to
// merge.
test("409 KEY_COMMITTED is a landing: no refusal, nothing kept, and every read refreshed", async () => {
  await onFakeClock(async () => {
    const tab = keptTab();
    const client = signedIn();
    const invalidate = spyOn(client, "invalidateQueries");
    const patchIssue = spyOn(api, "patchIssue")
      .mockImplementationOnce(attempt(Promise.reject(refusal)))
      .mockRejectedValueOnce(new ApiError(409, { code: "KEY_COMMITTED", error: "already made" }));
    const writes = new PendingWrites(client, tab.page());

    try {
      const landed = statusWrite(writes).catch((error: unknown) => error);
      await settle();
      const key = patchIssue.mock.calls[0]?.[2]?.key ?? "";
      expect(tab.kept(key)).toMatchObject({ state: "sending" });
      jest.advanceTimersByTime(1_000);
      await settle();

      expect(await landed).toBeInstanceOf(AnswerNotKeptError);
      expect(patchIssue).toHaveBeenCalledTimes(2);
      expect(patchIssue.mock.calls[1]?.[2]?.key).toBe(key);
      expect(writes.rows()).toEqual([]);
      expect(tab.kept(key)).toBeUndefined();
      expect(invalidate).toHaveBeenCalledWith();

      jest.advanceTimersByTime(30 * 60_000);
      await settle();
      expect(patchIssue).toHaveBeenCalledTimes(2);
    } finally {
      patchIssue.mockRestore();
      tab.close();
    }
  });
});
