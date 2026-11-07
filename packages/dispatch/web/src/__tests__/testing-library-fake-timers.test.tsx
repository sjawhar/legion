import { afterEach, expect, jest, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import { type ReactNode, useEffect, useState } from "react";
import { realClock } from "./fake-clock";

afterEach(realClock);

/** A node that arrives only when a timer fires, which is the whole product this test needs. */
function Later(): ReactNode {
  const [shown, setShown] = useState(false);
  useEffect(() => {
    const timer = setTimeout(() => setShown(true), 100);
    return () => clearTimeout(timer);
  }, []);
  return <div>{shown ? "later" : "not yet"}</div>;
}

// What this holds in place is the global `jest` of `setup.ts`. Testing Library asks
// `typeof jest !== 'undefined'` to decide whether to advance the clock inside `waitFor` and
// `findBy*` (`jestFakeTimersAreEnabled`, @testing-library/dom@10.4.1,
// dist/@testing-library/dom.esm.js:171), and Bun's `jest` is a module binding from `bun:test`,
// not a global. Without the line the library takes its real-clock path, which polls with timers
// a held clock need not fire, so a wait that needs the clock to move can never end: that is the
// 16 failures dispatch://INFRA-983 measured over four of its files on a 2-core pod.
//
// Measured here, and the reason this test is shaped as it is: the jest-global assertion is the
// half that discriminates. With the line removed it fails naming the missing global; the `findBy`
// below passes either way on this pod, because something outside the held clock still delivers
// the effect's timer here (a timer scheduled under `jest.useFakeTimers()` does not fire on its
// own: a probe of one ran to its 20 s deadline). So the wait below is the behaviour the line
// exists for rather than a discriminator on this machine, and the jest-global assertion is what
// turns a removal into a red test instead of a timeout that names nothing.
//
// `jestFakeTimersAreEnabled` has a second half, and the `setTimeout` assertion below is it: after
// the global, the library asks whether `setTimeout` carries a `clock` property or an
// `_isMockFunction` flag (@testing-library/dom@10.4.1, dist/helpers.js). Measured here under
// `jest.useFakeTimers()`: `clock` is an own property of `setTimeout` and `_isMockFunction` is
// `undefined`. That half is the one a later Bun could break with no wait failing anywhere, so a
// green suite would show nothing of it, and it is asserted rather than left to be noticed.
test("a Testing Library wait advances the fake clock a test installed", async () => {
  jest.useFakeTimers();

  expect(typeof (globalThis as { jest?: unknown }).jest).toBe("object");
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
