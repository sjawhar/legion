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
  running: boolean;
}

const slots = new Map<Element, FrameSlot>();

let watchers:
  | { readonly keep: IntersectionObserver; readonly start: IntersectionObserver }
  | undefined;

/**
 * How far the slot is from the screen, in pixels: zero while any of it is on screen. The ranking
 * pass reads it once per slot, and only when an observer reports a change, so scrolling alone
 * costs no layout read.
 */
function distanceFromScreen(element: Element): number {
  const rectangle = element.getBoundingClientRect();
  const screen = window.innerHeight || document.documentElement.clientHeight;
  if (rectangle.bottom >= 0 && rectangle.top <= screen) {
    return 0;
  }
  return rectangle.top > screen ? rectangle.top - screen : -rectangle.bottom;
}

/**
 * Chooses the frames that run: of the slots near the screen, and of those already running while
 * they stay within three screen heights, the four nearest. Every slot whose answer changed is
 * told once.
 */
function rankFrames(): void {
  const eligible = [...slots.values()].filter((slot) => slot.kept && (slot.near || slot.running));
  const distances = new Map(eligible.map((slot) => [slot, distanceFromScreen(slot.element)]));
  eligible.sort((left, right) => (distances.get(left) ?? 0) - (distances.get(right) ?? 0));
  const running = new Set(eligible.slice(0, MAX_RUNNING_FRAMES));
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

function watch(element: Element): void {
  if (watchers === undefined || !(watchers.start instanceof IntersectionObserver)) {
    watchers = {
      keep: observerOf(KEEP_MARGIN, (slot, inside) => {
        slot.kept = inside;
      }),
      start: observerOf(START_MARGIN, (slot, inside) => {
        slot.near = inside;
      }),
    };
  }
  watchers.start.observe(element);
  watchers.keep.observe(element);
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
  watchers?.start.unobserve(element);
  watchers?.keep.unobserve(element);
  if (slots.size === 0) {
    watchers?.start.disconnect();
    watchers?.keep.disconnect();
    watchers = undefined;
    return;
  }
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
  slots.set(element, { element, kept: false, near: false, onRunningChange, running: false });
  watch(element);
  return () => releaseViewFrame(element);
}

/** Forgets every slot and both observers, for a test that drives the budget itself. */
export function resetViewFrames(): void {
  watchers?.start.disconnect();
  watchers?.keep.disconnect();
  watchers = undefined;
  slots.clear();
}
