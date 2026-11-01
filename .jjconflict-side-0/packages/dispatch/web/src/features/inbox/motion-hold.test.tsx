import { afterEach, expect, jest, test } from "bun:test";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { useRef } from "react";

import { useMotionHold } from "./motion-hold";

function Harness({ data, settleMs = 400 }: { data: string; settleMs?: number }) {
  const root = useRef<HTMLDivElement>(null);
  const listed = useMotionHold(data, root, settleMs);
  return (
    <div data-testid="motion-root" ref={root}>
      {listed}
    </div>
  );
}

afterEach(() => {
  jest.useRealTimers();
});

test("holds new data until the pointer has settled", () => {
  jest.useFakeTimers();
  const view = render(<Harness data="first" />);

  try {
    const root = screen.getByTestId("motion-root");
    fireEvent.pointerMove(root);
    view.rerender(<Harness data="second" />);
    expect(root.textContent).toBe("first");

    act(() => {
      jest.advanceTimersByTime(399);
    });
    expect(root.textContent).toBe("first");
    act(() => {
      jest.advanceTimersByTime(1);
    });
    expect(root.textContent).toBe("second");
  } finally {
    view.unmount();
  }
});

test("a later pointer move re-arms the hold", () => {
  jest.useFakeTimers();
  const view = render(<Harness data="first" />);

  try {
    const root = screen.getByTestId("motion-root");
    fireEvent.pointerMove(root);
    view.rerender(<Harness data="second" />);
    act(() => {
      jest.advanceTimersByTime(200);
    });
    fireEvent.pointerMove(root);
    act(() => {
      jest.advanceTimersByTime(200);
    });
    expect(root.textContent).toBe("first");
    act(() => {
      jest.advanceTimersByTime(200);
    });
    expect(root.textContent).toBe("second");
  } finally {
    view.unmount();
  }
});

test("pointerleave adopts held data immediately", () => {
  jest.useFakeTimers();
  const view = render(<Harness data="first" />);

  try {
    const root = screen.getByTestId("motion-root");
    fireEvent.pointerMove(root);
    view.rerender(<Harness data="second" />);
    expect(root.textContent).toBe("first");
    fireEvent.pointerLeave(root);
    expect(root.textContent).toBe("second");
  } finally {
    view.unmount();
  }
});

test("a press after movement adopts data immediately", () => {
  jest.useFakeTimers();
  const view = render(<Harness data="first" />);

  try {
    const root = screen.getByTestId("motion-root");
    fireEvent.pointerMove(root);
    view.rerender(<Harness data="second" />);
    expect(root.textContent).toBe("first");
    fireEvent.pointerDown(root);
    expect(root.textContent).toBe("second");
  } finally {
    view.unmount();
  }
});
test("without pointer movement, the next render adopts data synchronously", () => {
  const view = render(<Harness data="first" />);

  try {
    const root = screen.getByTestId("motion-root");
    view.rerender(<Harness data="second" />);
    expect(root.textContent).toBe("second");
  } finally {
    view.unmount();
  }
});
