import { expect, test } from "bun:test";
import type { EditorView } from "@milkdown/kit/prose/view";
import { landOnBlock } from "../src/editor-highlights";

/** A block whose place in the document the test sets, recording each scroll aimed at it. */
interface BlockDouble {
  top: number;
  readonly aims: ScrollIntoViewOptions[];
  readonly element: HTMLElement;
}

function blockDouble(top: number): BlockDouble {
  const block: BlockDouble = {
    aims: [],
    element: {
      getBoundingClientRect: () => ({ top: block.top }),
      scrollIntoView: (options: ScrollIntoViewOptions) => void block.aims.push(options),
    } as unknown as HTMLElement,
    top,
  };
  return block;
}

/** The window a landing reads: its clock, its reader input listeners and one ResizeObserver,
 *  whose callback the test runs where a browser would after a layout change. */
interface HostDouble {
  now: number;
  readonly listeners: Map<string, EventListener>;
  observer?: { readonly callback: () => void; disconnected: boolean };
  readonly view: EditorView;
}

function hostDouble(): HostDouble {
  const host: HostDouble = { listeners: new Map(), now: 0, view: undefined as never };
  class HeldResizeObserver {
    constructor(callback: () => void) {
      host.observer = { callback, disconnected: false };
    }
    observe() {}
    disconnect() {
      if (host.observer !== undefined) host.observer.disconnected = true;
    }
  }
  const window = {
    addEventListener: (type: string, listener: EventListener) => host.listeners.set(type, listener),
    performance: { now: () => host.now },
    removeEventListener: (type: string) => host.listeners.delete(type),
    ResizeObserver: HeldResizeObserver,
  };
  const dom = { getBoundingClientRect: () => ({ top: 0 }), ownerDocument: { defaultView: window } };
  Object.assign(host, { view: { dom, isDestroyed: false } });
  return host;
}

const CENTER: ScrollIntoViewOptions = { behavior: "smooth", block: "center" };

test("a block landing aims again each time the document's layout moves the block, and only then", () => {
  const host = hostDouble();
  const block = blockDouble(2000);
  landOnBlock(host.view, () => block.element);
  expect(block.aims).toEqual([CENTER]);

  // A resize below the block leaves it where the scroll was aimed.
  host.observer?.callback();
  expect(block.aims).toEqual([CENTER]);

  // Reference titles drawn above it push it down the document.
  block.top = 2500;
  host.now = 100;
  host.observer?.callback();
  expect(block.aims).toEqual([CENTER, CENTER]);
});

test("a block landing aims at the element its finder answers now, since a redraw replaces it", () => {
  const host = hostDouble();
  const first = blockDouble(2000);
  const redrawn = blockDouble(2400);
  let current = first;
  landOnBlock(host.view, () => current.element);
  current = redrawn;
  host.observer?.callback();
  expect(first.aims).toEqual([CENTER]);
  expect(redrawn.aims).toEqual([CENTER]);
});

test("the reader's own input ends a block landing", () => {
  for (const type of ["keydown", "pointerdown", "touchstart", "wheel"]) {
    const host = hostDouble();
    const block = blockDouble(2000);
    landOnBlock(host.view, () => block.element);
    host.listeners.get(type)?.(new Event(type));
    expect(host.observer?.disconnected).toBe(true);
    expect(host.listeners.size).toBe(0);
  }
});

test("a block landing stops following its block once the document has had time to settle", () => {
  const host = hostDouble();
  const block = blockDouble(2000);
  const end = landOnBlock(host.view, () => block.element);
  block.top = 2500;
  host.now = 2001;
  host.observer?.callback();
  expect(block.aims).toEqual([CENTER]);
  expect(host.observer?.disconnected).toBe(true);
  expect(host.listeners.size).toBe(0);
  end();
});

test("a block landing for a block the document does not hold scrolls nowhere", () => {
  const host = hostDouble();
  landOnBlock(host.view, () => null);
  expect(host.observer).toBeUndefined();
});
