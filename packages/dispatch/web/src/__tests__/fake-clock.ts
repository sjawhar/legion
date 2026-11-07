import { jest } from "bun:test";
import { act } from "@testing-library/react";

/**
 * Moves the fake clock `ms` on in steps of at most `step`, letting each timer's continuation and
 * React's updates run as it fires. A test that waits a product timer out installs
 * `jest.useFakeTimers()` and moves the clock here rather than waiting for real time; one jump
 * would run every timer of the window at the same instant, so a wait that must really cross a
 * window - and a `Date.now()` the test reads after it - would measure the test rather than the
 * product. Every test that installs the fake clock restores the real one in a `finally`
 * (`realClock` below): a fake clock left behind stops the timers of every test after it.
 */
export async function tick(ms: number, step = 500): Promise<void> {
  for (let left = ms; left >= 0; left -= step) {
    await act(async () => {
      jest.advanceTimersByTime(Math.min(step, left));
      for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
    });
  }
}

/** Restores the real clock a test installed `jest.useFakeTimers()` for, first running whatever a
 *  zero-delay timer left on the fake clock would do: one left behind holds up the next test's
 *  `waitFor`, which moves the clock that test installed rather than the real one. That last step
 *  runs inside `act`, since a write waiting on such a timer continues into React's state as it
 *  fires, and a test rendering a component would otherwise update outside `act`. */
export function realClock(): void {
  act(() => jest.advanceTimersByTime(1));
  jest.useRealTimers();
}

/** Lets every promise a write chained run as far as it goes without the clock moving. */
export async function settle(): Promise<void> {
  for (let turn = 0; turn < 30; turn += 1) await Promise.resolve();
}

/** Runs `body` on the fake clock, so a write that waits out a backoff waits no real time, and
 *  brings the real clock back through `realClock` however `body` ends. */
export async function onFakeClock(body: () => Promise<void>): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    realClock();
  }
}
