/**
 * Where a reader is in a document, and how the page keeps them there.
 *
 * Two surfaces need the same answer. The swap from the page's own draw to the editor reads the
 * reader's place before it and puts them back after it (`ProofDocument`'s `onBatch` and its
 * `handle.reveal()`), and the page keeps that place for as long as the editor is bound, because
 * what the page draws after the reader is in place still grows above them: a panel the page
 * held behind the editor's first batch (`first-batch.ts`), a decision card drawn into its
 * block's shell one task at a time (`useAskCardsDrawnInTasks`), a Mermaid diagram the worker
 * answers for, and a block the browser skipped painting at its own height in place of the one
 * `contain-intrinsic-size` estimated.
 *
 * A panel arriving must not shift what a reader is looking at. Where the panel's box is known
 * before its read answers, the panel reserves it and nothing moves; where its height is
 * whatever the answer turns out to be, this module is the answer, and
 * `packages/dispatch/AGENTS.md` ("Panels that arrive after the reader") says which is which.
 */

/** Where a reader is in a document drawn as a run of top-level blocks: the first block the top
 *  of the scrollport is in or above, and how far that block's own top sits from it. `undefined`
 *  where there is nothing to keep: no draw, or a reader still at the document's first block and
 *  above its top, whom any draw of it leaves where they are. */
export interface ReadersBlock {
  readonly index: number;
  readonly top: number;
}

/**
 * The first child of `drawn` the top of the scrollport is in or above, by a binary search over
 * the children rather than a scan of them: the run is in document order, so `bottom > 0` is
 * false for every block above the scrollport and true from the reader's own block on, and the
 * document this is asked about holds thousands of blocks. `undefined` where every block is
 * above the scrollport, which is a reader past the end of the document.
 */
function firstBlockOnScreen(drawn: HTMLElement): number | undefined {
  const blocks = drawn.children;
  let low = 0;
  let high = blocks.length - 1;
  let found: number | undefined;
  while (low <= high) {
    const middle = (low + high) >> 1;
    if (blocks[middle].getBoundingClientRect().bottom > 0) {
      found = middle;
      high = middle - 1;
    } else {
      low = middle + 1;
    }
  }
  return found;
}

export function readersBlock(drawn: HTMLElement | null): ReadersBlock | undefined {
  if (drawn === null) {
    return undefined;
  }
  const index = firstBlockOnScreen(drawn);
  if (index === undefined) {
    return undefined;
  }
  const { top } = drawn.children[index].getBoundingClientRect();
  return index === 0 && top >= 0 ? undefined : { index, top };
}

/** What scrolls `element`: the nearest ancestor that scrolls its own content, or the page
 *  itself. A document is the page's own scroll on its page and a column's scroll in a reading
 *  pane, and the reader's place is kept against whichever is carrying them. */
export function whatScrolls(element: HTMLElement): HTMLElement | null {
  for (let box = element.parentElement; box !== null; box = box.parentElement) {
    const overflow = window.getComputedStyle(box).overflowY;
    if ((overflow === "auto" || overflow === "scroll") && box.scrollHeight > box.clientHeight) {
      return box;
    }
  }
  return null;
}

/** How far down whatever carries `within` the reader is. */
function scrollOffset(within: HTMLElement): number {
  const scroller = whatScrolls(within);
  return scroller === null ? window.scrollY : scroller.scrollTop;
}

/** Scrolls whatever carries `within` by `moved` pixels, so what the reader is looking at stays
 *  on the same line of their screen while the page changes above it. */
export function moveTheScroll(within: HTMLElement, moved: number): void {
  if (moved === 0) {
    return;
  }
  const scroller = whatScrolls(within);
  if (scroller === null) {
    window.scrollBy(0, moved);
    return;
  }
  scroller.scrollTop += moved;
}

/** Puts the reader back where `place` left them, in a document `drawn` draws the same blocks of
 *  in the same order. A block the new draw does not have - a document edited between the two -
 *  leaves the page where it is, which is the browser's own scroll anchoring rather than a jump
 *  to the wrong words. */
export function putTheReaderBack(drawn: HTMLElement, place: ReadersBlock | undefined): void {
  const block = place === undefined ? undefined : drawn.children[place.index];
  if (block === undefined || place === undefined) {
    return;
  }
  moveTheScroll(drawn, block.getBoundingClientRect().top - place.top);
}

/** The block the reader is on and where its top sits on their screen: the element rather than
 *  its index, since the run it is in grows under them. */
interface TheirPlace {
  readonly block: Element;
  readonly top: number;
}

function theirPlace(drawn: HTMLElement): TheirPlace | undefined {
  const where = readersBlock(drawn);
  if (where === undefined) {
    return undefined;
  }
  const block = drawn.children[where.index];
  return block === undefined ? undefined : { block, top: where.top };
}

/**
 * Keeps the block the reader is on at the same line of their screen for as long as the document
 * `drawn` is on the page, whatever grows or shrinks above it. Returns the dispose.
 *
 * The correction is made where the growth is seen - a `MutationObserver`'s microtask, a
 * `ResizeObserver`'s callback - and never a frame later, so nothing between the growth and the
 * paint that shows it holds a page the reader did not ask to move. It is not the watch
 * `refocusAfterDiagrams` runs, which re-runs a focus once the page has settled for 300 ms:
 * that is what a landing needs to end on its target, and 300 ms too late for a reader who is
 * simply reading. The two live side by side on a block link - the focus lands the reader, this
 * keeps them there - and the triggers below say which growths this answers and why.
 *
 * Nothing is read while the page is quiet: the page itself says when it has changed.
 *
 * It corrects only what nothing else has. The scroll it leaves behind is remembered, and a
 * scroll it did not make - the reader's own, a focus, the splice that takes the draw's blocks
 * off the page as the editor's batches replace them, or the browser's own scroll anchoring,
 * which moves the page for this same reason wherever it picks an anchor above the growth - is
 * read as the place being somebody else's now: the block the reader is on is taken afresh and
 * that frame corrects nothing. So two corrections never add up to twice the movement, and a
 * reader who scrolls is never pulled back to where they were.
 *
 * A reader still at the document's first block and above its top is not kept, as `readersBlock`
 * says: they are at the top of a page, where what arrives is theirs to see land.
 */
export function keepTheReadersPlace(drawn: HTMLElement): () => void {
  let place = theirPlace(drawn);
  /** Where this module left the scroll, so a scroll it did not make is told from its own. */
  let left = scrollOffset(drawn);
  const take = () => {
    place = theirPlace(drawn);
    left = scrollOffset(drawn);
  };
  const check = () => {
    const offset = scrollOffset(drawn);
    const kept = place;
    if (kept === undefined || !drawn.contains(kept.block) || offset !== left) {
      take();
      return;
    }
    const moved = kept.block.getBoundingClientRect().top - kept.top;
    if (moved !== 0) {
      moveTheScroll(drawn, moved);
      left = scrollOffset(drawn);
    }
  };
  // A scroll is the reader, or somebody placing them - a block link's focus and its re-run, the
  // splice that takes the draw's blocks off the page as the editor's batches replace them, the
  // browser's own scroll anchoring - and none of those is this module's to undo: where they
  // left the reader is where the reader is now. It is read here rather than in a frame of its
  // own, at the cost of one box read per scrolled frame, because a growth landing between the
  // scroll and that frame would be measured against a place the reader had already left, and
  // answered by doing nothing: that is how the page kept a panel's 64 px for 844 ms.
  const scrolled = () => {
    const kept = place;
    if (kept === undefined || !drawn.contains(kept.block)) {
      take();
      return;
    }
    const { bottom, top } = kept.block.getBoundingClientRect();
    if (bottom <= 0 || top >= window.innerHeight) {
      take();
      return;
    }
    place = { block: kept.block, top };
    left = scrollOffset(drawn);
  };
  // Every growth is answered where it is seen, never a frame later. A mutation is answered in
  // the microtask after the task that made it, before anything can read the page's layout and
  // before it is painted, and the layout it reads is the one that task owed anyway. A resize is
  // answered in the `ResizeObserver` callback, which the browser runs after that layout and
  // before it paints. Asking for an animation frame instead is what failed: on this pod the
  // frame that answered the Subscribers panel's arrival came 73 ms after it, and 844 ms when
  // the thread was busy, and a page that has moved for that long has moved under its reader.
  const mutations = new MutationObserver(check);
  // The whole page, not the document alone, since a panel landing in the issue header moves the
  // document without touching it; and attributes and character data with it, because a panel
  // that arrives need not add an element - a count written into a span, a class that unhides a
  // row the page already held - and either changes the height above the reader just as much.
  mutations.observe(document.body, {
    attributes: true,
    characterData: true,
    childList: true,
    subtree: true,
  });
  // What changes height with no mutation of its own: a block the browser skipped painting at
  // its own height in place of the one `contain-intrinsic-size` estimated, a picture or a font
  // that loaded, a window resized. The document's own box, the page's, and the box carrying the
  // document where that is a column of its own rather than the page.
  const sizes = new ResizeObserver(check);
  sizes.observe(drawn);
  sizes.observe(document.body);
  const scroller = whatScrolls(drawn);
  if (scroller !== null) {
    sizes.observe(scroller);
  }
  // `capture`, because a column's scroll does not bubble; `passive`, because this reads the
  // offset and moves nothing.
  window.addEventListener("scroll", scrolled, { capture: true, passive: true });
  return () => {
    mutations.disconnect();
    sizes.disconnect();
    window.removeEventListener("scroll", scrolled, true);
  };
}

/** Every box `keepWhatStandsUnder` is watching, with how tall each last stood, and where the
 *  last correction left the scroll. One state for all of them, because two boxes that grow in
 *  one frame are one growth to answer: a guard per box would read the first box's own
 *  correction as somebody else's and leave the second box's growth on the reader. */
const boxesKept = new Map<HTMLElement, number>();
let keptTheScrollAt: number | undefined;

/** Answers every kept box's growth at once, and nothing another keeper has already answered. */
function keepWhatStandsUnderThem(): void {
  const [first] = boxesKept.keys();
  if (first === undefined) {
    return;
  }
  const offset = scrollOffset(first);
  if (offset !== keptTheScrollAt) {
    // The reader scrolled, or the document's own keeper answered this growth first: where
    // they are now is where they are, and nothing is answered for twice.
    for (const box of boxesKept.keys()) {
      boxesKept.set(box, box.getBoundingClientRect().height);
    }
    keptTheScrollAt = offset;
    return;
  }
  let grew = 0;
  for (const box of boxesKept.keys()) {
    const { height, top } = box.getBoundingClientRect();
    // Only where the box stands above the top of the reader's screen. A reader who can see it
    // is watching it land, and moving the page under them would take what they are looking at
    // off it; that is the line `readersBlock` draws for a reader still above the document's
    // first block.
    if (top < 0) {
      grew += height - (boxesKept.get(box) ?? height);
    }
    boxesKept.set(box, height);
  }
  if (grew !== 0) {
    moveTheScroll(first, grew);
  }
  keptTheScrollAt = scrollOffset(first);
}

/**
 * Keeps whatever stands under `box` where the reader has it, for as long as `box` is on the
 * page, and returns the dispose.
 *
 * The twin of `keepTheReadersPlace`, for a part of the page that is not a document: a box the
 * page keeps where its chrome will stand, which the chrome then lands in taller than the space
 * it replaced (`features/doc/AfterTheFirstBatch.tsx`). The document under it must not move,
 * and the chrome may land over several commits - its chunk's fallback first, the part itself
 * after it - so what is answered is every change of the box's own height rather than one
 * commit of it.
 *
 * It corrects only what nothing else has, by the same rule `keepTheReadersPlace` reads: a
 * scroll these keepers did not make - the reader's own, or the document's own keeper answering
 * the same growth - says the reader is placed where they are now. So the two run side by side
 * over one page.
 */
export function keepWhatStandsUnder(box: HTMLElement): () => void {
  boxesKept.set(box, box.getBoundingClientRect().height);
  keptTheScrollAt ??= scrollOffset(box);
  // A mutation is answered in the microtask after the task that made it, before anything can
  // read the page's layout and before it is painted; a resize - a font loaded, the window
  // resized, a row of the chrome wrapping - in the `ResizeObserver` callback, which the
  // browser runs after that layout and before it paints. Neither is a frame late, so nothing
  // between the growth and the paint that shows it holds a page the reader did not ask to move.
  const mutations = new MutationObserver(keepWhatStandsUnderThem);
  mutations.observe(box, {
    attributes: true,
    characterData: true,
    childList: true,
    subtree: true,
  });
  const sizes = new ResizeObserver(keepWhatStandsUnderThem);
  sizes.observe(box);
  // Where the reader has scrolled to is where they are, and a growth that lands in a later
  // frame is measured against that rather than against where they were.
  const scrolled = () => {
    keptTheScrollAt = scrollOffset(box);
  };
  window.addEventListener("scroll", scrolled, { capture: true, passive: true });
  return () => {
    mutations.disconnect();
    sizes.disconnect();
    window.removeEventListener("scroll", scrolled, true);
    boxesKept.delete(box);
    if (boxesKept.size === 0) {
      keptTheScrollAt = undefined;
    }
  };
}
