import { jest } from "bun:test";
import type { UserEvent } from "@testing-library/user-event";
import userEvent from "@testing-library/user-event";

/**
 * The user of a test that holds the clock still (`jest.useFakeTimers()`).
 *
 * user-event waits between the events of one interaction through `setTimeout`, so under a held
 * clock its wait never ends and the interaction never finishes. `advanceTimers` is what it calls
 * to let the clock through, and it has to move the same clock the test installed. A test that
 * installs the clock per test calls this after `useFakeTimers()`, since user-event reads the
 * clock's state when it is set up.
 *
 * A test on the real clock needs none of this: `userEvent.setup()` is its user.
 */
export function userOnFakeClock(): UserEvent {
  return userEvent.setup({ advanceTimers: (ms) => jest.advanceTimersByTime(ms) });
}
