/**
 * How many `view` frames run at once on a Dispatch page, and which: the four nearest the screen
 * (dispatch://INFRA-682, "At most four frames"). Each frame is a whole page with its own drawing
 * code, so an Inbox or a long document that holds many blocks would otherwise run them all.
 *
 * Two shared `IntersectionObserver`s watch every registered slot. The first starts a frame when
 * the block comes within one screen height of the screen; the second keeps a running frame while
 * the block stays within three screen heights, and the block loses its frame past that. A
 * percentage `rootMargin` resolves against the viewport, so a resize needs no recomputation and
 * the two margins are written once here. The precedent for one shared observer behind many
 * callers is `features/shell/useDwell.ts`.
 *
 * The registry is keyed by the slot element, which outlives every repaint of the body around it
 * (`features/refs/MarkdownBody.tsx` splices rather than replaces), so a block keeps its place in
 * the budget while a comment streams or an edit lands.
 */

/** The most frames that run at once on one Dispatch page. */
export const MAX_RUNNING_FRAMES = 4;

/** A block within one screen height of the screen may start its frame. */
const START_MARGIN = "100% 0px";

/** A block more than three screen heights from the screen loses its frame. */
const KEEP_MARGIN = "300% 0px";

/**
 * How long a `view` block waits on one page, the one deadline two sides of the same wait share:
 * here, how long a frame whose page has not loaded keeps the budget slot it holds, whatever is
 * nearer; in `ViewBlock.tsx`, how long a page that has loaded may say nothing before the block
 * shows `this page sent no ready message in 10 seconds`. One constant, exported from the module
 * the block already imports, so the two windows cannot drift apart.
 *
 * The bound is what keeps a navigation that never completes from holding a slot for the life of
 * the page. A stalled connection on the page route is no error page, so the frame fires no
 * `load`, so nothing clears `loading`: without this, four such frames would stop every other
 * block on the page for ever, and the block's own stall protection does not cover that window,
 * since its timer starts at the `load` that never comes. When it fires the slot is evictable
 * again, and that is all it costs: a block that loses its frame to a nearer one and comes back
 * builds a new frame and asks for its page again, as a block scrolled past and back does.
 */
export const PAGE_DEADLINE_MS = 10_000;

interface FrameSlot {
  readonly element: Element;
  /** Told whenever this slot's frame starts or stops. */
  readonly onRunningChange: (running: boolean) => void;
  /** Within one screen height of the screen. */
  near: boolean;
  /** Within three screen heights of the screen. */
  kept: boolean;
  /** The block's frame is in the document and its page has not finished loading
   *  (`setViewFrameLoading`). Such a slot keeps its place in the budget, for `PAGE_DEADLINE_MS`. */
  loading: boolean;
  /** When this slot started loading, as `Date.now()`; read only while `loading`. */
  loadingSince: number;
  /** The one re-ranking this slot's load deadline has scheduled, or zero for none: a page that
   *  hangs resizes nothing and crosses no margin, so without it the deadline above would be
   *  reached and never read. */
  deadline: number;
  running: boolean;
}

const slots = new Map<Element, FrameSlot>();

/** The two observers, made with the first slot and dropped with the last. */
let watchers: IntersectionObserver[] = [];

/** Watches every slot's own box, made and dropped with those. A frame that starts, stops or is
 *  given a height by its page resizes its slot, which moves every block under it, so the four
 *  nearest the screen are not the four the last report ranked. Without it a block the reader is
 *  looking at can wait forever behind one that has scrolled off, which is what a `view` block in
 *  a reply did: the four frames above it shrank from their 400 px to the height their pages
 *  reported after it registered, and nothing crossed either margin again, so the ranking that
 *  left it out was never re-made (dispatch://INFRA-682). */
let sizes: ResizeObserver | undefined;

/**
 * How far the slot is from the screen, in pixels: zero while any of it is on screen. The ranking
 * pass reads it once per slot, and only when an observer reports a change — a block crossing
 * either margin, or any slot's box changing size — so scrolling alone costs no layout read.
 */
function distanceFromScreen(element: Element): number {
  const rectangle = element.getBoundingClientRect();
  const screen = window.innerHeight;
  if (rectangle.bottom >= 0 && rectangle.top <= screen) {
    return 0;
  }
  return rectangle.top > screen ? rectangle.top - screen : -rectangle.bottom;
}

/**
 * Where one slot's block stands against another's in the document: before it, or after it.
 * Every block on screen is one distance — zero — so nearly every ranking of a page of blocks is
 * decided by this tie-break. Left to the `slots` Map's own order the tie fell to the order the
 * blocks registered in, which is the order their bodies happened to mount rather than the order
 * a reader sees: a page drawing a comment, its reply and a message at once registers them as
 * each body arrives, so two rankings of one unchanged page could choose two different sets of
 * four, and the frame dropped between them was torn out mid-load (dispatch://INFRA-682).
 */
function documentOrder(left: Element, right: Element): number {
  // Nothing else of the answer is read: a pair of blocks is two separate elements, so one of
  // the two directions is always set, and it is the same answer whichever way the pair is given.
  return (left.compareDocumentPosition(right) & Node.DOCUMENT_POSITION_FOLLOWING) === 0 ? 1 : -1;
}

/**
 * Chooses the frames that run: of the slots near the screen, and of those already running while
 * they stay within three screen heights, the four nearest, the nearer of a tie first in the
 * document. A frame whose page is still loading keeps the slot it holds, whatever is nearer, for
 * `PAGE_DEADLINE_MS`: taking it away removes the frame, which aborts the page's own navigation,
 * and the block it belonged to showed an empty frame from then on (dispatch://INFRA-682). Past
 * that deadline the slot is evictable again, which is what bounds a page that never answers.
 * Every slot whose answer changed is told once.
 */
function rankFrames(): void {
  const now = Date.now();
  const eligible: { distance: number; slot: FrameSlot }[] = [];
  for (const slot of slots.values()) {
    if (slot.kept && (slot.near || slot.running)) {
      eligible.push({ distance: distanceFromScreen(slot.element), slot });
    }
  }
  eligible.sort(
    (left, right) =>
      left.distance - right.distance || documentOrder(left.slot.element, right.slot.element)
  );
  const running = new Set<FrameSlot>();
  // This pass adds without counting, and still cannot pass the budget: it re-adds only slots a
  // previous ranking chose, since the loop at the end of this function is the one writer of
  // `slot.running` and `registerViewFrame` seeds every slot `running: false`. At most
  // MAX_RUNNING_FRAMES slots carry `running === true` at any moment, so at most that many are
  // re-added here. An edit that wrote `running` from anywhere else would take that away, and
  // this pass would need a bound of its own.
  for (const entry of eligible) {
    if (
      entry.slot.running &&
      entry.slot.loading &&
      now - entry.slot.loadingSince < PAGE_DEADLINE_MS
    ) {
      running.add(entry.slot);
    }
  }
  for (const entry of eligible) {
    if (running.size >= MAX_RUNNING_FRAMES) {
      break;
    }
    running.add(entry.slot);
  }
  for (const slot of [...slots.values()]) {
    const next = running.has(slot);
    if (next !== slot.running) {
      slot.running = next;
      slot.onRunningChange(next);
    }
  }
}

/** An observer whose reports write one flag of every slot it watches, then rank the frames. */
function observerOf(rootMargin: string, write: (slot: FrameSlot, inside: boolean) => void) {
  return new IntersectionObserver(
    (entries) => {
      for (const entry of entries) {
        const slot = slots.get(entry.target);
        if (slot !== undefined) {
          write(slot, entry.isIntersecting);
        }
      }
      rankFrames();
    },
    { rootMargin }
  );
}

/**
 * Takes one slot out of the budget: the frame of the block that held it stops, and another
 * block near the screen may take its place. A slot that is not under the budget is left alone,
 * so the editor's node view and the mounted block can both call it.
 */
export function releaseViewFrame(element: Element): void {
  const released = slots.get(element);
  if (released === undefined) {
    return;
  }
  window.clearTimeout(released.deadline);
  slots.delete(element);
  const last = slots.size === 0;
  for (const watcher of watchers) {
    if (last) {
      watcher.disconnect();
    } else {
      watcher.unobserve(element);
    }
  }
  if (last) {
    watchers = [];
    sizes?.disconnect();
    sizes = undefined;
    return;
  }
  sizes?.unobserve(element);
  rankFrames();
}

/**
 * Writes whether the block's frame is loading its page: true as the frame goes into the
 * document, false once that page has loaded, and false again whenever the block takes its frame
 * away. A slot that is loading holds its place in the budget until it answers, or until
 * `PAGE_DEADLINE_MS` is up (`rankFrames`), so no ranking between the two calls can abort the
 * navigation a block has started. A block that starts no frame at all — one whose page route
 * refused, or whose own effect answered before the frame went in — never says true, so it holds
 * nothing back. Only the end of a load re-ranks: the start takes nothing from anybody.
 */
export function setViewFrameLoading(element: Element, loading: boolean): void {
  const slot = slots.get(element);
  if (slot === undefined || slot.loading === loading) {
    return;
  }
  slot.loading = loading;
  window.clearTimeout(slot.deadline);
  if (loading) {
    slot.loadingSince = Date.now();
    // The one report a hanging page makes: it resizes nothing and crosses no margin, so this is
    // what has the deadline above read at the moment it is reached rather than whenever the
    // reader next moves.
    slot.deadline = window.setTimeout(rankFrames, PAGE_DEADLINE_MS);
    return;
  }
  slot.deadline = 0;
  // The slot it was holding is free now: a block nearer the screen may take it.
  rankFrames();
}

/**
 * Puts one `view` block's frame slot under the budget and answers the call that takes it out
 * again. `onRunningChange` is called whenever the answer changes: true once the block is near
 * enough and among the four nearest, false once a nearer block takes its place or the reader
 * scrolls more than three screen heights away. A slot starts outside the running set, so a block
 * shows its caption and "waiting" until the budget says otherwise.
 */
export function registerViewFrame(
  element: Element,
  onRunningChange: (running: boolean) => void
): () => void {
  slots.set(element, {
    deadline: 0,
    element,
    kept: false,
    loading: false,
    loadingSince: 0,
    near: false,
    onRunningChange,
    running: false,
  });
  if (watchers.length === 0) {
    watchers = [
      observerOf(START_MARGIN, (slot, inside) => {
        slot.near = inside;
      }),
      observerOf(KEEP_MARGIN, (slot, inside) => {
        slot.kept = inside;
      }),
    ];
  }
  sizes ??= new ResizeObserver(rankFrames);
  for (const watcher of watchers) {
    watcher.observe(element);
  }
  sizes.observe(element);
  return () => releaseViewFrame(element);
}
