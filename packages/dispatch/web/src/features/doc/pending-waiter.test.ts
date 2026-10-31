import { expect, jest, test } from "bun:test";

import type { PendingState } from "./pending-sync";
import { createPendingWaiter, type PendingWaiter } from "./pending-waiter";

const DOCUMENT = "artifact-1";

const nothingPending: PendingState = {
  count: 0,
  readOnly: false,
  rebuiltAt: undefined,
  stored: true,
};

/** Runs `body` on a fake clock: a wait's bound is advanced rather than waited out. */
async function onFakeClock(body: () => Promise<void>): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    jest.useRealTimers();
  }
}

/** Lets the microtasks a report resolves through run, so what a wait answered reads true. */
async function settle(): Promise<void> {
  for (let turn = 0; turn < 10; turn += 1) await Promise.resolve();
}

/** A wait on `DOCUMENT` and whether it has been answered, read after each `settle`. */
function waitFor(waiter: PendingWaiter, timeoutMs = 5_000) {
  const state = { answered: false };
  void waiter.whenSent(DOCUMENT, timeoutMs).then(() => {
    state.answered = true;
  });
  return state;
}

// The mark the margin has just written is an update of its own, so a report follows it: the wait
// is for one taken after it begins, never for the report the document happened to make before -
// a document with nothing else pending reports nothing pending there the whole time.
test("the report that answers a wait is one taken after it begins", async () => {
  await onFakeClock(async () => {
    const waiter = createPendingWaiter();
    waiter.report(DOCUMENT, nothingPending);

    const waiting = waitFor(waiter);
    await settle();
    expect(waiting.answered).toBe(false);

    waiter.report(DOCUMENT, nothingPending);
    await settle();
    expect(waiting.answered).toBe(true);
  });
});

// Rows this browser still holds, and rows no admission here can send, are no answer: the margin's
// own bound is what ends a wait on those.
test("a report with rows pending, or one a read-only admission cannot send, answers nothing", async () => {
  await onFakeClock(async () => {
    const waiter = createPendingWaiter();
    const waiting = waitFor(waiter);

    waiter.report(DOCUMENT, { ...nothingPending, count: 2 });
    waiter.report(DOCUMENT, { ...nothingPending, count: 1, readOnly: true });
    await settle();
    expect(waiting.answered).toBe(false);

    waiter.report(DOCUMENT, nothingPending);
    await settle();
    expect(waiting.answered).toBe(true);
  });
});

// The page can move to another document while a wait is out: that document's rows say nothing
// about the mark this wait is about.
test("a report of another document does not answer a wait on this one", async () => {
  await onFakeClock(async () => {
    const waiter = createPendingWaiter();
    const waiting = waitFor(waiter);

    waiter.report("artifact-2", nothingPending);
    await settle();
    expect(waiting.answered).toBe(false);

    waiter.report(DOCUMENT, nothingPending);
    await settle();
    expect(waiting.answered).toBe(true);
  });
});

// A document that reports nothing at all - a socket in reconnect backoff - must not hold the
// margin's retry for ever: the bound ends the wait, and the margin sends anyway.
test("the bound ends a wait nothing reports", async () => {
  await onFakeClock(async () => {
    const waiter = createPendingWaiter();
    const waiting = waitFor(waiter, 5_000);

    jest.advanceTimersByTime(4_999);
    await settle();
    expect(waiting.answered).toBe(false);

    jest.advanceTimersByTime(1);
    await settle();
    expect(waiting.answered).toBe(true);

    // The wait is gone with its bound: a report after it answers nobody and throws nothing.
    waiter.report(DOCUMENT, nothingPending);
  });
});
