/**
 * A `view` block's frame slot as everything outside the block reads it: finding the slots of a
 * painted body or an editor (`collectViewSlots`), and keeping the ones a repaint must not move
 * (`spliceViewFrames`). Both are on every page, since they are what says whether the renderer
 * is needed at all, and neither reaches anything of the block itself.
 */
import { VIEW_FRAME_SLOT } from "../doc/typed-block-drawing";

/** One `view` block's frame slot and what the block says about itself: the `page` and `data`
 *  attributes as the author wrote them, and the caption, which is the frame's title. */
export interface ViewBlockSlot {
  readonly caption: string;
  readonly data: string;
  readonly host: HTMLElement;
  readonly key: string;
  readonly page: string;
}

/** Every frame slot the engine drew under `root` (`renderViewBlockShell`), with the attributes
 *  and caption its `ViewBlock` renders from. The caption is the block's content hole, the slot's
 *  own previous sibling, so a caption edit reaches the block without touching the frame.
 *
 *  This much of the feature is on every page: finding a block's slot is what says whether the
 *  renderer is needed at all (`ViewPortals`), and it reaches nothing of it. */
export function collectViewSlots(root: HTMLElement): ViewBlockSlot[] {
  const slots: ViewBlockSlot[] = [];
  for (const host of root.querySelectorAll<HTMLElement>(VIEW_FRAME_SLOT)) {
    const page = host.dataset.dispatchViewPage ?? "";
    const hole = host.previousElementSibling;
    slots.push({
      caption:
        hole?.matches("[data-proof-block-content]") === true ? (hole.textContent ?? "").trim() : "",
      data: host.dataset.dispatchViewData ?? "",
      host,
      key: `view:${page}:${slots.length}`,
      page,
    });
  }
  return slots;
}

/** Whether this node is a frame slot or holds one, and so must stay in the document. */
function holdsFrameSlot(node: Node): node is Element {
  return (
    node instanceof Element &&
    (node.matches(VIEW_FRAME_SLOT) || node.querySelector(VIEW_FRAME_SLOT) !== null)
  );
}

/** What a repaint pairs a live element with a fresh one by: its tag name, then the `page` of
 *  every frame slot at or under it, in document order. A block's own `#id` is only what its
 *  author wrote and can repeat, so it names nothing here. */
function slotKey(element: Element): string {
  const slots = element.matches(VIEW_FRAME_SLOT)
    ? [element]
    : element.querySelectorAll(VIEW_FRAME_SLOT);
  let key = element.tagName;
  for (const slot of slots) {
    key += ` ${slot.getAttribute("data-dispatch-view-page") ?? ""}`;
  }
  return key;
}

/** Writes the fresh element's attributes onto the live one it replaces. A frame slot keeps the
 *  attributes the fresh paint does not name, because the mounted block stamps its own; every
 *  other element is made to carry exactly what the fresh paint says. */
function copyAttributes(live: Element, fresh: Element): void {
  for (const { name, value } of fresh.attributes) {
    if (live.getAttribute(name) !== value) {
      live.setAttribute(name, value);
    }
  }
  if (live.matches(VIEW_FRAME_SLOT)) {
    return;
  }
  for (const name of live.getAttributeNames()) {
    if (!fresh.hasAttribute(name)) {
      live.removeAttribute(name);
    }
  }
}

/**
 * Writes the freshly painted children over the live ones, keeping every element that holds a
 * `view` block's frame slot where it is. An `<iframe>` that leaves the document, even for an
 * instant, loads again, and a second load reads as "the page moved away", which is permanent; so
 * a repaint must move no frame and must take none out.
 *
 * At each level the children that hold a slot are paired in order by `slotKey`. On a pair, the
 * fresh attributes are written onto the live element and its own children are spliced the same
 * way, and a kept element is never passed to `insertBefore`. Every other fresh child is inserted
 * around that kept chain. Any mismatch falls back to replacing the level's children, whose worst
 * case is one reload.
 */
function spliceChildren(parent: Element, incoming: readonly Node[]): void {
  const liveKept = [...parent.childNodes].filter(holdsFrameSlot);
  const freshKept = incoming.filter(holdsFrameSlot);
  if (
    liveKept.length !== freshKept.length ||
    liveKept.some((live, index) => slotKey(live) !== slotKey(freshKept[index]))
  ) {
    parent.replaceChildren(...incoming);
    return;
  }
  for (const child of [...parent.childNodes]) {
    if (!holdsFrameSlot(child)) {
      child.remove();
    }
  }
  let keptIndex = 0;
  for (const node of incoming) {
    if (!holdsFrameSlot(node)) {
      parent.insertBefore(node, liveKept[keptIndex] ?? null);
      continue;
    }
    const live = liveKept[keptIndex];
    keptIndex += 1;
    copyAttributes(live, node);
    // The slot itself is React's: no pass of the repaint writes inside it.
    if (!live.matches(VIEW_FRAME_SLOT)) {
      spliceChildren(live, [...node.childNodes]);
    }
  }
}

/**
 * The one call both painted-body paths make (`features/refs/MarkdownBody.tsx`): where the live
 * element holds a `view` block's frame slot, the children of `built` are written over it in
 * place (`spliceChildren`) and this answers true; where it holds none, it writes nothing and
 * answers false, and the caller fills the element its own way - at once for a short body, a
 * batch at a time for a long one.
 *
 * The question and the splice are one call because they are one rule: whether a repaint may
 * replace a body's children at all. Keeping a running frame in the document is what the answer
 * protects, and this module is where a frame slot is read.
 */
export function spliceViewFrames(element: Element, built: Node): boolean {
  if (element.querySelector(VIEW_FRAME_SLOT) === null) {
    return false;
  }
  spliceChildren(element, [...built.childNodes]);
  return true;
}
