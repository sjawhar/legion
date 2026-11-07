import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: this file sends the pointer's own clientX/clientY and a pointerout's relatedTarget, neither of which user-event's hover and unhover can set.
import { fireEvent, render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef, type ReactNode, type RefObject } from "react";

import { type AnchorRow, ViewportAnchor } from "./ViewportAnchor";

const ROW_HEIGHT = 50;
const LIST_TOP = 100;
const VIEWPORT_HEIGHT = 600;

const anchor = createRef<ViewportAnchor>();
const scrollerRef = createRef<HTMLDivElement>();

/** The row the next commit will anchor to, as `getSnapshotBeforeUpdate` will choose it. */
function chosen(): AnchorRow | null {
  const current = anchor.current?.current() ?? null;
  return current === null ? null : { id: current.id, via: current.via };
}

function List({
  enabled,
  groups,
  rows,
  scroller,
}: {
  enabled?: () => boolean;
  /** A row's group; rows default to one shared group. */
  groups?: Readonly<Record<string, string>>;
  rows: string[];
  /** When given, the rows scroll in this element instead of the document. */
  scroller?: RefObject<HTMLDivElement | null>;
}): ReactNode {
  const list = (
    <ViewportAnchor
      enabled={enabled}
      group="data-group"
      item="data-row"
      ref={anchor}
      scroller={scroller}
    >
      {rows.map((id) => (
        <div data-group={groups?.[id] ?? "one"} data-row={id} key={id} tabIndex={-1}>
          <button type="button">{id}</button>
        </div>
      ))}
    </ViewportAnchor>
  );
  return scroller === undefined ? (
    list
  ) : (
    <div data-scroller="" ref={scroller}>
      {list}
    </div>
  );
}

// A fixed geometry: rows are ROW_HEIGHT tall, stacked from `listTop` in DOM order, in a
// VIEWPORT_HEIGHT window that scrolls through `window.scrollY` - or, where the list is rendered
// in a scroller, through that element's `scrollTop`, the scroller itself filling the window and
// never moving. `listTop` starts at LIST_TOP; a test lowers it to shrink content above the rows
// without moving what scrolls them, which is what a card collapsing in a commit of its own does.
const rows = () => [...document.querySelectorAll<HTMLElement>("[data-row]")];
const viewportTop = (id: string) =>
  document.querySelector(`[data-row="${id}"]`)?.getBoundingClientRect().top;

let listTop = LIST_TOP;
let scrollYDescriptor: PropertyDescriptor | undefined;
let originalScrollBy: typeof window.scrollBy;
let restoreSpies: (() => void)[] = [];

function setScrollY(value: number): void {
  Object.defineProperty(window, "scrollY", { configurable: true, value, writable: true });
}

beforeEach(() => {
  scrollYDescriptor = Object.getOwnPropertyDescriptor(window, "scrollY");
  originalScrollBy = window.scrollBy;
  listTop = LIST_TOP;
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
    // Whatever scrolls the rows carries them: the window, or the scroller when one is rendered.
    const offset = scrollerRef.current?.scrollTop ?? window.scrollY;
    const isScroller = this.hasAttribute("data-scroller");
    const isRow = this.hasAttribute("data-row");
    const top = isScroller
      ? 0
      : isRow
        ? listTop + all.indexOf(this as HTMLElement) * ROW_HEIGHT - offset
        : listTop - offset;
    const height = isScroller ? VIEWPORT_HEIGHT : isRow ? ROW_HEIGHT : all.length * ROW_HEIGHT;
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
    const offset = scrollerRef.current?.scrollTop ?? window.scrollY;
    const index = Math.floor((offset + y - listTop) / ROW_HEIGHT);
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

test("the row with focus within is the current node even at the top, ahead of the pointer and the centre", async () => {
  const user = userEvent.setup();
  const view = render(<List rows={ids(12)} />);
  try {
    const focused = document.querySelector<HTMLElement>('[data-row="r3"] button');
    if (focused === null) throw new Error("r3 button missing");
    focused.focus();
    const hovered = document.querySelector<HTMLElement>('[data-row="r5"]');
    if (hovered === null) throw new Error("r5 missing");
    await user.hover(hovered);
    const focusedTop = viewportTop("r3");
    const hoveredTop = viewportTop("r5");

    // Two rows land above r3 and one between r3 and r5.
    expect(chosen()).toEqual({ id: "r3", via: "focus" });
    view.rerender(<List rows={["a", "b", "r1", "r2", "r3", "r4", "between", ...ids(8, 5)]} />);

    expect(viewportTop("r3")).toBe(focusedTop);
    expect(viewportTop("r5")).toBe((hoveredTop ?? 0) + ROW_HEIGHT);
    expect(focused).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("the row under the pointer is the current node ahead of the centre, and is released when the pointer leaves it", async () => {
  const user = userEvent.setup();
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    const hovered = document.querySelector<HTMLElement>('[data-row="r2"]');
    if (hovered === null) throw new Error("r2 missing");
    await user.hover(hovered);
    const hoveredTop = viewportTop("r2");
    const centreTop = viewportTop("r7");

    // One row above r2, one between r2 and the centre row r7.
    expect(chosen()).toEqual({ id: "r2", via: "pointer" });
    view.rerender(<List rows={["a", "r1", "r2", "r3", "between", ...ids(9, 4)]} />);
    expect(viewportTop("r2")).toBe(hoveredTop);
    expect(viewportTop("r7")).toBe((centreTop ?? 0) + ROW_HEIGHT);

    await user.unhover(hovered);
    expect(chosen()?.via).toBe("center");
  } finally {
    view.unmount();
  }
});

test("the pointer moving onto the card a row opened keeps that row the current one, until the card closes", async () => {
  const user = userEvent.setup();
  const view = render(<List rows={ids(12)} />);
  // The one hover card (`features/refs/RefPreview.tsx`): `fixed` over the page and portaled into
  // the body, so it is never inside the list whose row opened it.
  const card = document.createElement("div");
  card.id = "ref-preview";
  document.body.append(card);
  try {
    setScrollY(100);
    const hovered = document.querySelector<HTMLElement>('[data-row="r2"]');
    const reference = hovered?.querySelector("button") ?? null;
    if (hovered === null || reference === null) throw new Error("r2 missing");
    await user.hover(reference);
    expect(chosen()).toEqual({ id: "r2", via: "pointer" });

    // The card opened over the resting pointer. `RefPreview` names its reference by
    // `aria-describedby` for as long as it is shown, which is how the row is known to own it.
    reference.setAttribute("aria-describedby", "ref-preview");
    const hoveredTop = viewportTop("r2");
    // user-event's unhover can set no relatedTarget, which is what names the card the pointer went to.
    fireEvent.pointerOut(reference, { relatedTarget: card });

    // A row arriving above r2 while the pointer reads r2's card leaves r2 where it was.
    expect(chosen()).toEqual({ id: "r2", via: "pointer" });
    view.rerender(<List rows={["a", ...ids(12)]} />);
    expect(viewportTop("r2")).toBe(hoveredTop);

    // The pointer left the card for somewhere else, and the card closed itself: the hand is off
    // the row, and the centre has it again.
    reference.removeAttribute("aria-describedby");
    card.remove();
    expect(chosen()?.via).toBe("center");
  } finally {
    view.unmount();
    card.remove();
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

test("rows scrolling in an element are pinned by that element's scrollTop, and the window stays", () => {
  const view = render(<List rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 100;
    // The same geometry as the window case: the centre of the scroller's box is r7.
    const before = viewportTop("r7");
    expect(chosen()).toEqual({ id: "r7", via: "center" });
    view.rerender(<List rows={["new-a", "new-b", ...ids(12)]} scroller={scrollerRef} />);
    expect(viewportTop("r7")).toBe(before);
    expect(scroller.scrollTop).toBe(200);
    expect(window.scrollY).toBe(0);
  } finally {
    view.unmount();
  }
});

// The Inbox drawer's defect: the row above the hand shrank inside its own card, a commit this
// anchor never sees, and the browser re-dispatched the boundary events at the pointer's own
// unchanged position. Reading the row's place again there would take the drift for the place
// to keep, and the next commit would pin the row where it had already slid to.
test("a row moving under a pointer that has not moved does not become the place it is kept at", () => {
  const view = render(<List rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 100;
    const hovered = document.querySelector<HTMLElement>('[data-row="r4"]');
    if (hovered === null) throw new Error("r4 missing");
    // user-event's hover sends no clientX/clientY of its own, and the pointer's place is what this row measures.
    fireEvent.pointerOver(hovered, { clientX: 20, clientY: 250 });
    const held = viewportTop("r4");

    // Content above the hand's row loses 50 px with no commit of this anchor's, so the row
    // slides up under a pointer that has not moved, and the browser says so again. The box's
    // own scrollTop does not move: the content shrank, the reader did not scroll.
    listTop -= ROW_HEIGHT;
    expect(viewportTop("r4")).toBe((held ?? 0) - ROW_HEIGHT);
    // The browser's own re-dispatch at the unchanged position, which user-event has no form for.
    fireEvent.pointerOver(hovered, { clientX: 20, clientY: 250 });

    view.rerender(<List rows={["a", ...ids(12)]} scroller={scrollerRef} />);
    expect(viewportTop("r4")).toBe(held);
  } finally {
    view.unmount();
  }
});

// A commit landing between a scroll and the scroll event it raises: following a reference from
// a Conversation card scrolls the page, and the thread-open commit arrives before the event
// does. Where the document scrolls the rows nothing is kept for that commit to restore, so the
// reader stays where they put themselves.
test("a window scroll whose event has not arrived yet is not undone by the next commit", () => {
  const view = render(<List rows={ids(12)} />);
  try {
    setScrollY(100);
    const hovered = document.querySelector<HTMLElement>('[data-row="r4"]');
    if (hovered === null) throw new Error("r4 missing");
    // user-event's hover sends no clientX/clientY of its own, and the pointer's place is what this row measures.
    fireEvent.pointerOver(hovered, { clientX: 20, clientY: 250 });

    // The page moved to the reference's target and the `scroll` event has not run yet.
    setScrollY(250);
    const at = viewportTop("r4");

    // A row arrives below the hand's row, which leaves that row where it is.
    view.rerender(<List rows={[...ids(4), "new", ...ids(8, 5)]} />);
    expect(window.scrollY).toBe(250);
    expect(viewportTop("r4")).toBe(at);
  } finally {
    view.unmount();
  }
});

// The reader gave the row under their hand another priority while the Inbox is grouped by it:
// the row left its section in an earlier commit, and the place kept for it is where it used to
// sit. A row that changed group was relocated, not shifted, so the view does not follow it.
test("a held row that changed group is not followed to the section it moved to", () => {
  const view = render(<List rows={ids(12)} scroller={scrollerRef} />);
  try {
    const scroller = scrollerRef.current;
    if (scroller === null) throw new Error("scroller missing");
    scroller.scrollTop = 100;
    const hovered = document.querySelector<HTMLElement>('[data-row="r4"]');
    if (hovered === null) throw new Error("r4 missing");
    // user-event's hover sends no clientX/clientY of its own, and the pointer's place is what this row measures.
    fireEvent.pointerOver(hovered, { clientX: 20, clientY: 250 });

    // r4 leaves its section for the end of the list. The commit that moves it finds it in a
    // group it was not measured in, and leaves the view alone.
    const moved = [...ids(12).filter((id) => id !== "r4"), "r4"];
    view.rerender(<List groups={{ r4: "other" }} rows={moved} scroller={scrollerRef} />);
    expect(scroller.scrollTop).toBe(100);

    // The next commit finds it in its new section, where the kept place is no longer its own.
    view.rerender(
      <List groups={{ r4: "other" }} rows={[...moved, "r13"]} scroller={scrollerRef} />
    );
    expect(scroller.scrollTop).toBe(100);
    expect(viewportTop("r4")).toBe(LIST_TOP + 11 * ROW_HEIGHT - 100);
  } finally {
    view.unmount();
  }
});
