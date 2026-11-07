import { afterEach, expect, jest, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: the hold is armed by raw pointermove/pointerleave/pointerdown on the list root, a hover-intent timer user-event's hover/unhover cannot send.
import { act, fireEvent, render, screen } from "@testing-library/react";
import { useRef } from "react";

import { useMotionHold } from "./motion-hold";

/**
 * `card` is what the one hover card is doing while the harness renders: none, the card a
 * reference in this list opened (`RefPreview` marks that reference with `aria-describedby`
 * while its card is shown), or a card some other surface opened. The card itself is never
 * inside the list - it is `fixed` over the page - so it is rendered beside the root here too.
 */
function Harness({
  card = "none",
  data,
  settleMs = 400,
}: {
  card?: "none" | "other" | "own";
  data: string;
  settleMs?: number;
}) {
  const root = useRef<HTMLDivElement>(null);
  const listed = useMotionHold(data, root, settleMs);
  return (
    <>
      <div data-testid="motion-root" ref={root}>
        <a
          aria-describedby={card === "own" ? "ref-preview" : undefined}
          data-dispatch-ref="dispatch://CORE-1"
          href="/issues/CORE-1"
        >
          owner
        </a>
        <span data-testid="motion-data">{listed}</span>
      </div>
      {card === "none" ? null : <div data-testid="ref-card" id="ref-preview" role="tooltip" />}
    </>
  );
}

/** What the list is showing: the data the hold has adopted. */
function shown(): string | null {
  return screen.getByTestId("motion-data").textContent;
}

afterEach(() => {
  jest.useRealTimers();
});

test("holds new data until the pointer has settled", () => {
  jest.useFakeTimers();
  const view = render(<Harness data="first" />);

  try {
    const root = screen.getByTestId("motion-root");
    // user-event has no bare pointermove: hover sends a fixed mouse sequence, not the one move this hold arms on.
    fireEvent.pointerMove(root);
    view.rerender(<Harness data="second" />);
    expect(shown()).toBe("first");

    act(() => {
      jest.advanceTimersByTime(399);
    });
    expect(shown()).toBe("first");
    act(() => {
      jest.advanceTimersByTime(1);
    });
    expect(shown()).toBe("second");
  } finally {
    view.unmount();
  }
});

test("a later pointer move re-arms the hold", () => {
  jest.useFakeTimers();
  const view = render(<Harness data="first" />);

  try {
    const root = screen.getByTestId("motion-root");
    // user-event has no bare pointermove: hover sends a fixed mouse sequence, not the one move this hold arms on.
    fireEvent.pointerMove(root);
    view.rerender(<Harness data="second" />);
    act(() => {
      jest.advanceTimersByTime(200);
    });
    // user-event has no bare pointermove: a second move is what re-arms the hold, and hover would send a whole sequence.
    fireEvent.pointerMove(root);
    act(() => {
      jest.advanceTimersByTime(200);
    });
    expect(shown()).toBe("first");
    act(() => {
      jest.advanceTimersByTime(200);
    });
    expect(shown()).toBe("second");
  } finally {
    view.unmount();
  }
});

test("pointerleave adopts held data immediately", () => {
  jest.useFakeTimers();
  const view = render(<Harness data="first" />);

  try {
    const root = screen.getByTestId("motion-root");
    // user-event has no bare pointermove: hover sends a fixed mouse sequence, not the one move this hold arms on.
    fireEvent.pointerMove(root);
    view.rerender(<Harness data="second" />);
    expect(shown()).toBe("first");
    // user-event has no bare pointerleave: unhover sends a mouse sequence and can set no relatedTarget.
    fireEvent.pointerLeave(root);
    expect(shown()).toBe("second");
  } finally {
    view.unmount();
  }
});

test("a press after movement adopts data immediately", () => {
  jest.useFakeTimers();
  const view = render(<Harness data="first" />);

  try {
    const root = screen.getByTestId("motion-root");
    // user-event has no bare pointermove: hover sends a fixed mouse sequence, not the one move this hold arms on.
    fireEvent.pointerMove(root);
    view.rerender(<Harness data="second" />);
    expect(shown()).toBe("first");
    // user-event has no bare pointerdown: click would follow it with mouseup, click and a focus the hold never sees.
    fireEvent.pointerDown(root);
    expect(shown()).toBe("second");
  } finally {
    view.unmount();
  }
});
test("without pointer movement, the next render adopts data synchronously", () => {
  const view = render(<Harness data="first" />);

  try {
    view.rerender(<Harness data="second" />);
    expect(shown()).toBe("second");
  } finally {
    view.unmount();
  }
});

test("the pointer moving onto the card a row opened keeps the hold until it settles", () => {
  jest.useFakeTimers();
  const view = render(<Harness card="own" data="first" />);

  try {
    const root = screen.getByTestId("motion-root");
    const card = screen.getByTestId("ref-card");
    // user-event has no bare pointermove: hover sends a fixed mouse sequence, not the one move this hold arms on.
    fireEvent.pointerMove(root);
    view.rerender(<Harness card="own" data="second" />);
    expect(shown()).toBe("first");

    // The card opened under the resting pointer, so the next move lands inside it and the list
    // is left. The reader left nothing: the card is the row's own.
    // user-event has no bare pointerleave: unhover sends a mouse sequence and can set no relatedTarget.
    fireEvent.pointerLeave(root, { relatedTarget: card });
    expect(shown()).toBe("first");

    // Nothing more is heard from the pointer while it is over the card, so the settle ends the
    // hold as it ends any other.
    act(() => {
      jest.advanceTimersByTime(399);
    });
    expect(shown()).toBe("first");
    act(() => {
      jest.advanceTimersByTime(1);
    });
    expect(shown()).toBe("second");
  } finally {
    view.unmount();
  }
});

test("a card no row of the list opened is somewhere else, and the hold is let go", () => {
  jest.useFakeTimers();
  const view = render(<Harness card="other" data="first" />);

  try {
    const root = screen.getByTestId("motion-root");
    const card = screen.getByTestId("ref-card");
    // user-event has no bare pointermove: hover sends a fixed mouse sequence, not the one move this hold arms on.
    fireEvent.pointerMove(root);
    view.rerender(<Harness card="other" data="second" />);
    expect(shown()).toBe("first");
    // user-event has no bare pointerleave: unhover sends a mouse sequence and can set no relatedTarget.
    fireEvent.pointerLeave(root, { relatedTarget: card });
    expect(shown()).toBe("second");
  } finally {
    view.unmount();
  }
});
