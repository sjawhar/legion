import { expect, spyOn, test } from "bun:test";
import { render } from "@testing-library/react";
import type { ReactNode } from "react";

import { ReaderPosition } from "./ReaderPosition";

function Conversation({ items }: { items: number[] }): ReactNode {
  return (
    <ReaderPosition className="" shouldCompensate={() => true}>
      {items.map((seq) => (
        <div data-event-seq={seq} key={seq} />
      ))}
    </ReaderPosition>
  );
}

test("preserves the current reader anchor when a turn arrives before the scroll event", () => {
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const originalScrollBy = window.scrollBy;
  const rectSpy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this.getAttribute("aria-label") === "Conversation") {
      return {
        bottom: 600,
        height: 600,
        left: 0,
        right: 100,
        toJSON: () => ({}),
        top: 0,
        width: 100,
        x: 0,
        y: 0,
      };
    }
    const items = [...document.querySelectorAll<HTMLElement>("[data-event-seq]")];
    const index = items.indexOf(this as HTMLElement);
    const top = 100 + index * 50 - window.scrollY;
    return {
      bottom: top + 50,
      height: 50,
      left: 0,
      right: 100,
      toJSON: () => ({}),
      top,
      width: 100,
      x: 0,
      y: top,
    };
  });
  const pointSpy = spyOn(document, "elementFromPoint").mockImplementation((_x, y) => {
    const items = [...document.querySelectorAll<HTMLElement>("[data-event-seq]")];
    const index = Math.floor((window.scrollY + y - 100) / 50);
    return items[index] ?? null;
  });
  let unmount: (() => void) | undefined;

  try {
    Object.defineProperty(window, "scrollY", { configurable: true, value: 0, writable: true });
    window.scrollBy = ((x: number | ScrollToOptions, y?: number) => {
      const top = typeof x === "number" ? y : x.top;
      if (top !== undefined) {
        Object.defineProperty(window, "scrollY", {
          configurable: true,
          value: window.scrollY + top,
          writable: true,
        });
      }
    }) as typeof window.scrollBy;
    const initialItems = Array.from({ length: 12 }, (_, index) => 12 - index);
    const view = render(<Conversation items={initialItems} />);
    unmount = view.unmount;
    Object.defineProperty(window, "scrollY", { configurable: true, value: 100, writable: true });
    const readerAnchor = document.querySelector<HTMLElement>('[data-event-seq="6"]');
    const before = readerAnchor?.getBoundingClientRect().top;

    view.rerender(<Conversation items={[13, ...initialItems]} />);

    expect(readerAnchor?.getBoundingClientRect().top).toBe(before);
  } finally {
    unmount?.();
    pointSpy.mockRestore();
    rectSpy.mockRestore();
    window.scrollBy = originalScrollBy;
    if (scrollY === undefined) {
      Reflect.deleteProperty(window, "scrollY");
    } else {
      Object.defineProperty(window, "scrollY", scrollY);
    }
  }
});
