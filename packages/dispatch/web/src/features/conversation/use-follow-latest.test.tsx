import { expect, spyOn, test } from "bun:test";
import { act, render } from "@testing-library/react";
import type { ReactNode } from "react";

import { useFollowLatest } from "./use-follow-latest";

// The composer sits at document top 0; the newest turn (topmost `[data-event-seq]`) sits below
// it. `useFollowLatest` reads both elements' `getBoundingClientRect()`, so the harness renders
// stand-ins for them rather than a real layout engine.
const COMPOSER_HEIGHT = 140;
const NEWEST_TURN_DOC_TOP = 300;
const RESTING_SCROLL_Y = NEWEST_TURN_DOC_TOP - COMPOSER_HEIGHT;

function installScrollLayoutMocks(): { restore: () => void } {
  const rectSpy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const isComposer = this.getAttribute("aria-label") === "Message composer";
    const isTurn = this.hasAttribute("data-event-seq");
    const docTop = isComposer ? 0 : isTurn ? NEWEST_TURN_DOC_TOP : 0;
    const height = isComposer ? COMPOSER_HEIGHT : isTurn ? 50 : 0;
    const top = docTop - window.scrollY;
    return {
      bottom: top + height,
      height,
      left: 0,
      right: 100,
      toJSON: () => ({}),
      top,
      width: 100,
      x: 0,
      y: top,
    };
  });
  const originalScrollTo = window.scrollTo;
  window.scrollTo = (options?: ScrollToOptions | number, y?: number) => {
    const top = typeof options === "number" ? y : options?.top;
    if (top !== undefined) {
      Object.defineProperty(window, "scrollY", { configurable: true, value: top, writable: true });
    }
  };
  return {
    restore: () => {
      rectSpy.mockRestore();
      window.scrollTo = originalScrollTo;
    },
  };
}

function Harness({
  itemSeqs,
  ownSendCount,
}: {
  itemSeqs: number[];
  ownSendCount: number;
}): ReactNode {
  useFollowLatest({ enabled: true, itemSeqs, ownSendCount });
  return (
    <div>
      <form aria-label="Message composer" />
      {itemSeqs.map((seq) => (
        <div data-event-seq={seq} key={seq} />
      ))}
    </div>
  );
}

test("scrolls to the newest turn on mount", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    unmount = render(<Harness itemSeqs={[1]} ownSendCount={0} />).unmount;

    expect(window.scrollY).toBe(RESTING_SCROLL_Y);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("scrolls to reveal an own send even when the seq change lands on a later render", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    const view = render(<Harness itemSeqs={[1]} ownSendCount={0} />);
    unmount = view.unmount;
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);

    // The composer's mutation bumps `ownSendCount` in the same tick it invalidates the events
    // query; the invalidated refetch — and the new `latestItemSeq` it produces — lands on a
    // later render. This render observes only the `ownSendCount` change; `itemSeqs` is unchanged.
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });
    act(() => {
      view.rerender(<Harness itemSeqs={[1]} ownSendCount={1} />);
    });

    // The own send follows regardless of scroll position, even before the refetch lands.
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("does not mistake a later arrival for the send once ownSendCount is recorded", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    const view = render(<Harness itemSeqs={[1]} ownSendCount={0} />);
    unmount = view.unmount;
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);

    // The SSE stream can invalidate the events query independently of the mutation's own
    // callback, sometimes landing the seq change *before* `ownSendCount` catches up. While the
    // reader is still pinned this just follows, exactly like any other arrival.
    act(() => {
      view.rerender(<Harness itemSeqs={[2, 1]} ownSendCount={0} />);
    });
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);

    // The reader scrolls away, then `ownSendCount` finally catches up to the send that already
    // landed above — this must not be treated as a second, still-pending own send.
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });
    act(() => {
      view.rerender(<Harness itemSeqs={[2, 1]} ownSendCount={1} />);
    });
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);

    // A stranger's turn arriving next must not be mistaken for the already-recorded send.
    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });
    act(() => {
      view.rerender(<Harness itemSeqs={[3, 2, 1]} ownSendCount={1} />);
    });
    expect(window.scrollY).toBe(4_500);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});

test("does not follow another turn arriving while the reader is away", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const layout = installScrollLayoutMocks();
  let unmount: (() => void) | undefined;

  try {
    const view = render(<Harness itemSeqs={[1]} ownSendCount={0} />);
    unmount = view.unmount;
    expect(window.scrollY).toBe(RESTING_SCROLL_Y);

    Object.defineProperty(window, "scrollY", { configurable: true, value: 4_500, writable: true });
    act(() => {
      window.dispatchEvent(new Event("scroll"));
    });

    // A turn from someone else arrives; `ownSendCount` never changes.
    act(() => {
      view.rerender(<Harness itemSeqs={[2, 1]} ownSendCount={0} />);
    });

    expect(window.scrollY).toBe(4_500);
  } finally {
    unmount?.();
    layout.restore();
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});
