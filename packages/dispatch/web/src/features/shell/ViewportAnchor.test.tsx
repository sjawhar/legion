import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { fireEvent, render } from "@testing-library/react";
import { createRef, type ReactNode } from "react";

import { type AnchorRow, ViewportAnchor } from "./ViewportAnchor";

const ROW_HEIGHT = 50;
const LIST_TOP = 100;
const VIEWPORT_HEIGHT = 600;

const anchor = createRef<ViewportAnchor>();

/** The row the next commit will anchor to, as `getSnapshotBeforeUpdate` will choose it. */
function chosen(): AnchorRow | null {
  const current = anchor.current?.current() ?? null;
  return current === null ? null : { id: current.id, via: current.via };
}

function List({
  enabled,
  groups,
  rows,
}: {
  enabled?: () => boolean;
  /** A row's group; rows default to one shared group. */
  groups?: Readonly<Record<string, string>>;
  rows: string[];
}): ReactNode {
  return (
    <ViewportAnchor enabled={enabled} group="data-group" item="data-row" ref={anchor}>
      {rows.map((id) => (
        <div data-group={groups?.[id] ?? "one"} data-row={id} key={id} tabIndex={-1}>
          <button type="button">{id}</button>
        </div>
      ))}
    </ViewportAnchor>
  );
}

// A fixed geometry: rows are ROW_HEIGHT tall, stacked from LIST_TOP in DOM order, in a
// VIEWPORT_HEIGHT window that scrolls through `window.scrollY`.
const rows = () => [...document.querySelectorAll<HTMLElement>("[data-row]")];
const viewportTop = (id: string) =>
  document.querySelector(`[data-row="${id}"]`)?.getBoundingClientRect().top;

let scrollYDescriptor: PropertyDescriptor | undefined;
let originalScrollBy: typeof window.scrollBy;
let restoreSpies: (() => void)[] = [];

function setScrollY(value: number): void {
  Object.defineProperty(window, "scrollY", { configurable: true, value, writable: true });
}

beforeEach(() => {
  scrollYDescriptor = Object.getOwnPropertyDescriptor(window, "scrollY");
  originalScrollBy = window.scrollBy;
  setScrollY(0);
  window.scrollBy = ((x: number | ScrollToOptions, y?: number) => {
    const top = typeof x === "number" ? y : x.top;
    if (top !== undefined) setScrollY(window.scrollY + top);
  }) as typeof window.scrollBy;
  Object.defineProperty(window, "innerHeight", {
    configurable: true,
    value: VIEWPORT_HEIGHT,
    writable: true,
  });
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const all = rows();
    const isList = !this.hasAttribute("data-row");
    const top = isList
      ? LIST_TOP - window.scrollY
      : LIST_TOP + all.indexOf(this as HTMLElement) * ROW_HEIGHT - window.scrollY;
    const height = isList ? all.length * ROW_HEIGHT : ROW_HEIGHT;
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
  const point = spyOn(document, "elementFromPoint").mockImplementation((_x, y) => {
    const index = Math.floor((window.scrollY + y - LIST_TOP) / ROW_HEIGHT);
    return rows()[index] ?? null;
  });
  restoreSpies = [() => rect.mockRestore(), () => point.mockRestore()];
});

afterEach(() => {
  for (const restore of restoreSpies) restore();
  window.scrollBy = originalScrollBy;
  if (scrollYDescriptor === undefined) {
    Reflect.deleteProperty(window, "scrollY");
  } else {
    Object.defineProperty(window, "scrollY", scrollYDescriptor);
  }
});

const ids = (count: number, from = 1) => Array.from({ length: count }, (_, i) => `r${from + i}`);

test("a scrolled reader keeps the row at the viewport centre in place when rows are inserted above", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    // Visible centre: (LIST_TOP - 100 + 600) / 2 = 300 → document y 400 → row index 6 → r7.
    const before = viewportTop("r7");
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    view.rerender(<List rows={["new-a", "new-b", ...ids(12)]} />);
    expect(viewportTop("r7")).toBe(before);
    expect(window.scrollY).toBe(200);
  } finally {
    view.unmount();
  }
});

test("a reader at the top sees new rows arrive without the view following the centre row", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    expect(chosen()).toBeNull();
    view.rerender(<List rows={["new-a", ...ids(12)]} />);
    expect(window.scrollY).toBe(0);
    expect(viewportTop("new-a")).toBe(LIST_TOP);
  } finally {
    view.unmount();
  }
});

test("the row with focus within is the current node even at the top, ahead of the pointer and the centre", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    const focused = document.querySelector<HTMLElement>('[data-row="r3"] button');
    if (focused === null) throw new Error("r3 button missing");
    focused.focus();
    const hovered = document.querySelector<HTMLElement>('[data-row="r5"]');
    if (hovered === null) throw new Error("r5 missing");
    fireEvent.pointerOver(hovered);
    const focusedTop = viewportTop("r3");
    const hoveredTop = viewportTop("r5");

    // Two rows land above r3 and one between r3 and r5.
    expect(chosen()).toEqual({ id: "r3", via: "focus" });
    view.rerender(<List rows={["a", "b", "r1", "r2", "r3", "r4", "between", ...ids(8, 5)]} />);

    expect(viewportTop("r3")).toBe(focusedTop);
    expect(viewportTop("r5")).toBe((hoveredTop ?? 0) + ROW_HEIGHT);
    expect(document.activeElement).toBe(focused);
  } finally {
    view.unmount();
  }
});

test("the row under the pointer is the current node ahead of the centre, and is released when the pointer leaves it", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    const hovered = document.querySelector<HTMLElement>('[data-row="r2"]');
    if (hovered === null) throw new Error("r2 missing");
    fireEvent.pointerOver(hovered);
    const hoveredTop = viewportTop("r2");
    const centreTop = viewportTop("r7");

    // One row above r2, one between r2 and the centre row r7.
    expect(chosen()).toEqual({ id: "r2", via: "pointer" });
    view.rerender(<List rows={["a", "r1", "r2", "r3", "between", ...ids(9, 4)]} />);
    expect(viewportTop("r2")).toBe(hoveredTop);
    expect(viewportTop("r7")).toBe((centreTop ?? 0) + ROW_HEIGHT);

    fireEvent.pointerOut(hovered, { relatedTarget: document.body });
    expect(chosen()?.via).toBe("center");
  } finally {
    view.unmount();
  }
});

test("a disabled anchor measures nothing and leaves the scroll alone", () => {
  const view = render(<List enabled={() => false} rows={ids(12)} />);
  try {
    setScrollY(100);
    const rect = spyOn(Element.prototype, "getBoundingClientRect");
    view.rerender(<List enabled={() => false} rows={["new-a", ...ids(12)]} />);
    expect(rect.mock.calls.length).toBe(0);
    expect(window.scrollY).toBe(100);
  } finally {
    view.unmount();
  }
});

test("a current node that left the list scrolls nothing", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    view.rerender(<List rows={ids(12).filter((id) => id !== "r7")} />);
    expect(window.scrollY).toBe(100);
  } finally {
    view.unmount();
  }
});

test("a current node that changed group was relocated, not shifted: it is not followed and the view stays", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    // r7 leaves its section for the end of the list.
    view.rerender(
      <List groups={{ r7: "other" }} rows={[...ids(12).filter((id) => id !== "r7"), "r7"]} />
    );
    expect(window.scrollY).toBe(100);
    expect(viewportTop("r8")).toBe(LIST_TOP + 6 * ROW_HEIGHT - 100);
  } finally {
    view.unmount();
  }
});

test("a current node reordered within its group is still followed", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    const before = viewportTop("r7");
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    // r7 is promoted two places up its section.
    view.rerender(<List rows={[...ids(4), "r7", "r5", "r6", ...ids(5, 8)]} />);
    expect(viewportTop("r7")).toBe(before);
    expect(window.scrollY).toBe(0);
  } finally {
    view.unmount();
  }
});
