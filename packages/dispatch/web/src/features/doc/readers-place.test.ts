import { afterEach, expect, test } from "bun:test";

import { keepTheReadersPlace, readersBlock } from "./readers-place";

// happy-dom lays nothing out and has no ResizeObserver, so the geometry is this harness: a run
// of blocks `BLOCK_HEIGHT` tall whose first block starts `documentTop` under the top of the
// scrollport, a page that scrolls by `window.scrollY` and a `ResizeObserver` the rows fire.
const BLOCK_HEIGHT = 100;

interface Page {
  readonly drawn: HTMLElement;
  /** Grows the document by `pixels` above block `index`, as a panel landing there does. */
  grow(index: number, pixels: number): void;
  /** What the reader does: the page scrolls and tells whoever is listening. */
  scrollTo(top: number): void;
  /** Fires every live `ResizeObserver`, as the browser does after it has laid the page out. */
  resized(): void;
  readonly scrollY: () => number;
}

const observers = new Set<() => void>();
let restoreObserver: (() => void) | undefined;
let scrollListeners: ((event: Event) => void)[] = [];

function pageOf(blocks: number, documentTop: number): Page {
  let scrollY = 0;
  // How much taller than `BLOCK_HEIGHT` each block is, which is what a panel landing in it adds.
  const grown = new Map<number, number>();
  const topOf = (index: number) => {
    let top = documentTop;
    for (let above = 0; above < index; above += 1) {
      top += BLOCK_HEIGHT + (grown.get(above) ?? 0);
    }
    return top - scrollY;
  };
  const drawn = document.createElement("div");
  for (let index = 0; index < blocks; index += 1) {
    const block = document.createElement("section");
    block.getBoundingClientRect = () =>
      ({
        bottom: topOf(index) + BLOCK_HEIGHT + (grown.get(index) ?? 0),
        top: topOf(index),
      }) as DOMRect;
    drawn.append(block);
  }
  document.body.append(drawn);
  // How tall the page stands, which is what tells a scroll made to answer for a growth from
  // one the reader made (`pageHeight`).
  Object.defineProperty(document.documentElement, "scrollHeight", {
    configurable: true,
    get: () => topOf(blocks) + scrollY,
  });
  const page: Page = {
    drawn,
    grow(index, pixels) {
      grown.set(index, (grown.get(index) ?? 0) + pixels);
    },
    resized() {
      for (const fire of [...observers]) fire();
    },
    scrollTo(top) {
      scrollY = top;
      for (const listener of scrollListeners) listener(new Event("scroll"));
    },
    scrollY: () => scrollY,
  };
  installTheBrowser(page);
  return page;
}

/** The browser's own pieces this module reaches for: `ResizeObserver`, the page's scroll
 *  offset, `window.scrollBy` and the scroll listener it registers. */
function installTheBrowser(page: Page): void {
  const original = globalThis.ResizeObserver;
  const scrollBy = window.scrollBy;
  const scrollY = Object.getOwnPropertyDescriptor(window, "scrollY");
  const addListener = window.addEventListener;
  const removeListener = window.removeEventListener;
  scrollListeners = [];
  class ObserverStub {
    private readonly fire: () => void;
    constructor(callback: ResizeObserverCallback) {
      this.fire = () => callback([], this as unknown as ResizeObserver);
      observers.add(this.fire);
    }
    disconnect(): void {
      observers.delete(this.fire);
    }
    observe(): void {}
    unobserve(): void {}
  }
  globalThis.ResizeObserver = ObserverStub as unknown as typeof ResizeObserver;
  Object.defineProperty(window, "scrollY", { configurable: true, get: page.scrollY });
  window.scrollBy = ((_x: number, y: number) => page.scrollTo(page.scrollY() + y)) as never;
  window.addEventListener = ((type: string, listener: (event: Event) => void) => {
    if (type === "scroll") scrollListeners.push(listener);
  }) as never;
  window.removeEventListener = ((type: string, listener: (event: Event) => void) => {
    if (type === "scroll") scrollListeners = scrollListeners.filter((kept) => kept !== listener);
  }) as never;
  restoreObserver = () => {
    globalThis.ResizeObserver = original;
    window.scrollBy = scrollBy;
    window.addEventListener = addListener;
    window.removeEventListener = removeListener;
    if (scrollY !== undefined) Object.defineProperty(window, "scrollY", scrollY);
    page.drawn.remove();
  };
}

afterEach(() => {
  restoreObserver?.();
  restoreObserver = undefined;
  observers.clear();
  scrollListeners = [];
});

test("the reader's block is the first one the top of the scrollport is in or above", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);

  expect(readersBlock(page.drawn)).toEqual({ index: 12, top: -50 });
});

test("a reader still above the document's first block has no place to keep", () => {
  const page = pageOf(40, 80);

  expect(readersBlock(page.drawn)).toBeUndefined();
  expect(readersBlock(null)).toBeUndefined();
});

test("a panel landing above the reader leaves their words where they were", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  const release = keepTheReadersPlace(page.drawn);
  try {
    page.grow(3, 332);
    page.resized();

    expect(page.scrollY()).toBe(1_582);
    expect(readersBlock(page.drawn)).toEqual({ index: 12, top: -50 });
  } finally {
    release();
  }
});

test("a panel landing below the reader moves nothing", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  const release = keepTheReadersPlace(page.drawn);
  try {
    page.grow(20, 332);
    page.resized();

    expect(page.scrollY()).toBe(1_250);
  } finally {
    release();
  }
});

test("a reader who has scrolled is kept where they are now, never pulled back", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  const release = keepTheReadersPlace(page.drawn);
  try {
    page.scrollTo(2_500);
    page.grow(3, 332);
    page.resized();

    expect(page.scrollY()).toBe(2_832);
  } finally {
    release();
  }
});

// The browser's own scroll anchoring, the splice that takes the draw's blocks off the page as
// the editor's batches replace them, and a block link's focus all move the scroll themselves.
// Where they left the reader is where the reader is, so a scroll this module did not make is
// read as their place and nothing is answered for twice.
test("growth something else has already answered for is not answered for twice", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  const release = keepTheReadersPlace(page.drawn);
  try {
    page.grow(3, 332);
    page.scrollTo(1_582);
    page.resized();

    expect(page.scrollY()).toBe(1_582);
  } finally {
    release();
  }
});

test("a panel landing before the page got to a frame is still answered for", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  const release = keepTheReadersPlace(page.drawn);
  try {
    // The scroll is read as it is told, not in a frame of its own, so a growth that lands
    // before that frame is measured against where the reader is now.
    page.scrollTo(2_500);
    page.grow(3, 332);
    page.resized();

    expect(page.scrollY()).toBe(2_832);
  } finally {
    release();
  }
});

test("the page is left alone once the document has gone", () => {
  const page = pageOf(40, 0);
  page.scrollTo(1_250);
  keepTheReadersPlace(page.drawn)();

  page.grow(3, 332);
  page.resized();

  expect(page.scrollY()).toBe(1_250);
});
