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
