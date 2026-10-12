import { afterEach, expect, jest, test } from "bun:test";
import { act, render } from "@testing-library/react";
import type { ReactNode } from "react";

import { installIntersectionObserver } from "../../__tests__/intersection-observer";
import { useDwell } from "./useDwell";

afterEach(() => {
  jest.useRealTimers();
});

function Watched({
  onDwell,
  shouldDwell,
}: {
  onDwell: (element: Element) => unknown;
  shouldDwell?: (element: Element) => boolean;
}): ReactNode {
  const ref = useDwell({ enabled: true, onDwell, resetKey: "one", shouldDwell });
  return (
    <p data-testid="watched" ref={ref}>
      Watched
    </p>
  );
}

test("an element three-quarters on screen for a second dwells, once", () => {
  jest.useFakeTimers();
  const screen = installIntersectionObserver();
  const dwelled: Element[] = [];
  const view = render(<Watched onDwell={(element) => dwelled.push(element)} />);
  try {
    const element = view.getByTestId("watched");
    expect(screen.observed()).toContain(element);
    act(() => screen.show(element));
    act(() => {
      jest.advanceTimersByTime(999);
    });
    expect(dwelled).toEqual([]);
    act(() => {
      jest.advanceTimersByTime(1);
    });
    // onDwell runs from a resolved promise, after the timer fires.
    return Promise.resolve().then(() => {
      expect(dwelled).toEqual([element]);
    });
  } finally {
    view.unmount();
    screen.restore();
  }
});

test("an element that leaves the screen before its second is up does not dwell", async () => {
  jest.useFakeTimers();
  const screen = installIntersectionObserver();
  const dwelled: Element[] = [];
  const view = render(<Watched onDwell={(element) => dwelled.push(element)} />);
  try {
    const element = view.getByTestId("watched");
    act(() => screen.show(element));
    act(() => {
      jest.advanceTimersByTime(600);
    });
    act(() => screen.hide(element));
    act(() => {
      jest.advanceTimersByTime(2_000);
    });
    await Promise.resolve();
    expect(dwelled).toEqual([]);
  } finally {
    view.unmount();
    screen.restore();
  }
});

test("an element that needs no dwell starts no timer", async () => {
  jest.useFakeTimers();
  const screen = installIntersectionObserver();
  const dwelled: Element[] = [];
  const view = render(
    <Watched onDwell={(element) => dwelled.push(element)} shouldDwell={() => false} />
  );
  try {
    act(() => screen.show(view.getByTestId("watched")));
    act(() => {
      jest.advanceTimersByTime(2_000);
    });
    await Promise.resolve();
    expect(dwelled).toEqual([]);
  } finally {
    view.unmount();
    screen.restore();
  }
});

// A write the server refuses is sent again while the element stays on screen, as the
// Conversation's issue read mark always retried a failed write.
test("a dwell whose write fails dwells again while the element stays on screen", async () => {
  jest.useFakeTimers();
  const screen = installIntersectionObserver();
  let calls = 0;
  const view = render(
    <Watched
      onDwell={() => {
        calls += 1;
        return calls === 1 ? Promise.reject(new Error("refused")) : Promise.resolve();
      }}
    />
  );
  try {
    act(() => screen.show(view.getByTestId("watched")));
    act(() => {
      jest.advanceTimersByTime(1_000);
    });
    // Let the failed write settle and re-arm the timer.
    for (let flush = 0; flush < 5; flush += 1) await Promise.resolve();
    expect(calls).toBe(1);
    act(() => {
      jest.advanceTimersByTime(1_000);
    });
    for (let flush = 0; flush < 5; flush += 1) await Promise.resolve();
    expect(calls).toBe(2);
  } finally {
    view.unmount();
    screen.restore();
  }
});
