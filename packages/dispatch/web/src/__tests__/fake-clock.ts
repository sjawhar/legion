import { jest } from "bun:test";
import { act } from "@testing-library/react";

/**
 * Moves the fake clock `ms` on in steps of at most `step`, letting each timer's continuation and
 * React's updates run as it fires. A test that waits a product timer out installs
 * `jest.useFakeTimers()` and moves the clock here rather than waiting for real time; one jump
 * would run every timer of the window at the same instant, so a wait that must really cross a
 * window - and a `Date.now()` the test reads after it - would measure the test rather than the
 * product. Every test that installs the fake clock restores the real one in its `finally` or its
 * file's `afterEach` (`realClock` below): a fake clock left behind stops the timers of every test
 * after it.
 */
export async function tick(ms: number, step = 500): Promise<void> {
  if (step <= 0) throw new RangeError("tick's step must be above zero; use jump for a single jump");
  for (let left = ms; left >= 0; left -= step) {
    await act(async () => {
      jest.advanceTimersByTime(Math.min(step, left));
      for (let turn = 0; turn < 20; turn += 1) await Promise.resolve();
    });
  }
}

/** Moves the fake clock `ms` on in one jump, letting every promise the move released run, and
 *  then runs a second round that advances nothing: for a wait that crosses one timer of the
 *  product, where no part of the span needs a round of its own. The second round fires, inside
 *  the same wait, whatever the first round's continuations scheduled at zero delay, so a product
 *  that should wait a timer of its own but follows up at once is caught: the retry row of
 *  `useDwell.test.tsx` fails if a failed write is retried without a new dwell. */
export async function jump(ms: number): Promise<void> {
  await tick(ms, Math.max(ms, 1));
}

/** Restores the real clock a test installed `jest.useFakeTimers()` for, first running whatever a
 *  zero-delay timer left on the fake clock would do: one left behind holds up the next test's
 *  `waitFor`, which moves the clock that test installed rather than the real one. */
export function realClock(): void {
  act(() => jest.advanceTimersByTime(1));
  jest.useRealTimers();
}

/** Runs `body` on the fake clock, putting the real one back after it whether `body` passed or
 *  threw (`realClock`, so a millisecond runs before the real clock comes back): the shape a test
 *  that waits a product timer out takes, so no later test inherits a clock that never moves. */
export async function onFakeClock(body: () => Promise<void>): Promise<void> {
  jest.useFakeTimers();
  try {
    await body();
  } finally {
    realClock();
  }
}
