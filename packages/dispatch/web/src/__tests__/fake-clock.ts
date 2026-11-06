import { jest } from "bun:test";

/** Lets every promise a write chained run as far as it goes without the clock moving. */
export async function settle(): Promise<void> {
  for (let turn = 0; turn < 30; turn += 1) await Promise.resolve();
}

/** Runs `body` on the fake clock, so a write that waits out a backoff waits no real time. The
 *  clock moves a millisecond before the real one comes back: a timer left at zero delay would
 *  otherwise hang a later test's `waitFor`. */
export async function onFakeClock(body: () => Promise<void>): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    jest.advanceTimersByTime(1);
    jest.useRealTimers();
  }
}
