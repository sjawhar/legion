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

interface FrameSlot {
  readonly element: Element;
  /** Told whenever this slot's frame starts or stops. */
  readonly onRunningChange: (running: boolean) => void;
  /** Within one screen height of the screen. */
  near: boolean;
  /** Within three screen heights of the screen. */
  kept: boolean;
  /** The block's frame is in the document and its page has not finished loading
   *  (`viewFrameLoading`). Such a slot keeps its place in the budget. */
  loading: boolean;
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
 * document. A frame whose page is still loading keeps the slot it holds, whatever is nearer:
 * taking it away removes the frame, which aborts the page's own navigation, and the block it
 * belonged to showed an empty frame from then on (dispatch://INFRA-682). Every slot whose
 * answer changed is told once.
 */
function rankFrames(): void {
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
  for (const entry of eligible) {
    if (entry.slot.running && entry.slot.loading) {
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
  if (!slots.delete(element)) {
    return;
  }
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
 * Says whether the block's frame is loading its page: true as the frame goes into the document,
 * false once that page has loaded, and false again whenever the block takes its frame away. A
 * slot that is loading holds its place in the budget until it answers (`rankFrames`), so no
 * ranking between the two calls can abort the navigation a block has started. A block that
 * starts no frame at all — one whose page route refused, or whose own effect answered before
 * the frame went in — never says true, so it holds nothing back.
 */
export function viewFrameLoading(element: Element, loading: boolean): void {
  const slot = slots.get(element);
  if (slot === undefined || slot.loading === loading) {
    return;
  }
  slot.loading = loading;
  if (!loading) {
    // The slot it was holding is free now: a block nearer the screen may take it.
    rankFrames();
  }
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
    element,
    kept: false,
    loading: false,
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
