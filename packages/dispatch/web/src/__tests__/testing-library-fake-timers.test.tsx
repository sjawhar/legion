import { afterEach, expect, jest, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import { type ReactNode, useEffect, useState } from "react";

afterEach(() => {
  jest.advanceTimersByTime(1);
  jest.useRealTimers();
});

/** A node that arrives only when a timer fires, which is the whole product this test needs. */
function Later(): ReactNode {
  const [shown, setShown] = useState(false);
  useEffect(() => {
    const timer = setTimeout(() => setShown(true), 100);
    return () => clearTimeout(timer);
  }, []);
  return <div>{shown ? "later" : "not yet"}</div>;
}

// What this test holds in place is the global `jest` that `setup.ts` assigns. Testing Library
// asks two things before it moves a clock: first `typeof jest !== 'undefined'`, then whether
// `setTimeout` carries a `clock` property or an `_isMockFunction` flag - the two halves of
// `jestFakeTimersAreEnabled` in @testing-library/dom@10.4.1 `dist/helpers.js`. The second half is
// the one a later Bun could break without a single wait failing anywhere, because a suite that
// stays green shows nothing of it, so it is asserted here rather than left to be noticed. The
// `findBy` at the end is the behaviour the preload's line exists for; the two assertions above it
// are what turn a removal of that line, or a change in Bun, into a red test that names the cause
// rather than a timeout somewhere else that names nothing.
test("a Testing Library wait advances the fake clock a test installed", async () => {
  jest.useFakeTimers();

  expect(typeof (globalThis as { jest?: unknown }).jest).toBe("object");

  // Measured in this pod under `jest.useFakeTimers()`: `clock` is an own property of `setTimeout`
  // and `_isMockFunction` is `undefined`, so either mark satisfies Testing Library's second half.
  expect(
    Object.hasOwn(setTimeout, "clock") ||
      (setTimeout as unknown as { _isMockFunction?: boolean })._isMockFunction === true
  ).toBe(true);

  const view = render(<Later />);
  try {
    expect(screen.getByText("not yet")).toBeDefined();
    expect(await screen.findByText("later")).toBeDefined();
  } finally {
    view.unmount();
  }
});
