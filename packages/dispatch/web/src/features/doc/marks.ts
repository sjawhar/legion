import type { Node as ProseMirrorNode } from "prosemirror-model";

import type { MarkPlacement } from "../margin/useMarginItems";

const recordMarkTypes = ["proofComment", "proofSuggestion", "dispatchAsk"] as const;

type SelectionBarKind = "comment" | "suggest" | "ask";

/**
 * Where each record mark of `doc` is: its first position, and the pixel `offsets` measured it
 * at. A mark `offsets` does not name is left out, so an empty `offsets` is an empty answer and
 * the walk is skipped - a document with no comment, suggestion or ask on it, which is most of
 * them, would otherwise pay a walk of every text node of itself in every published placement,
 * and the placements are published in the task the editor opens in and again on every resize
 * tick after it (dispatch://INFRA-341, R11).
 */
export function markPlacements(
  doc: ProseMirrorNode,
  offsets: ReadonlyMap<string, number>
): Map<string, MarkPlacement> {
  const placements = new Map<string, MarkPlacement>();
  if (offsets.size === 0) {
    return placements;
  }
  doc.descendants((node, pos) => {
    if (!node.isText) {
      return true;
    }
    for (const mark of node.marks) {
      if (
        recordMarkTypes.includes(mark.type.name as (typeof recordMarkTypes)[number]) &&
        typeof mark.attrs.id === "string" &&
        !placements.has(mark.attrs.id)
      ) {
        const top = offsets.get(mark.attrs.id);
        if (top !== undefined) {
          placements.set(mark.attrs.id, { pos, top });
        }
      }
    }
    return true;
  });
  return placements;
}

/**
 * Where each of `root`'s blocks sits, by its id, in pixels below the top of `root`.
 *
 * `placed` says whether anything on the page places a card or a badge against those pixels right
 * now (`ProofDocument`). With nothing placing them each block answers 0: the ids are still every
 * block the document draws, which is what membership is read from, but no box is measured. One
 * `getBoundingClientRect` per block over a document of a thousand blocks, on every deep change
 * and every `ResizeObserver` tick, is a forced layout of the whole document each time
 * (dispatch://INFRA-341, requirement R11).
 *
 * One box per top-level block, never one per block. The document's top-level blocks are the
 * elements the browser skips off screen (`content-visibility: auto`, `styles.css`), and a read
 * of such an element's own box costs nothing extra - 1,000 of them in 10 ms at 4x CPU throttling
 * against 26 ms unskipped - while a read of anything inside one forces that whole block to
 * render, and the next read pays a layout again. The 406,319-character document R11 measures
 * holds 986 top-level blocks and about 7,000 elements carrying a `data-block-id`, the rest of
 * them nested, so reading each of those in turn forced every block of the document to render one
 * at a time: 4.4 s of layout and 1.8 s of style, in the one task the margin's placements are
 * first published in, which is the task the editor opens in. A nested block is answered with its
 * top-level block's top instead. That number is a fallback either way: a margin card stands at
 * the box its own mark or block is read at in the DOM, and falls back on this only where that
 * element cannot be found or measured (`ThreadList`'s `topFor`, `DecisionList`'s `measure`).
 */
export function blockOffsets(root: HTMLElement, placed: boolean): Map<string, number> {
  const offsets = new Map<string, number>();
  const rootTop = placed ? root.getBoundingClientRect().top : 0;
  const remember = (block: HTMLElement, top: number): void => {
    const blockId = block.dataset.blockId;
    if (blockId !== undefined && !offsets.has(blockId)) {
      offsets.set(blockId, top);
    }
  };
  for (const child of root.children) {
    const block = child as HTMLElement;
    const top = placed ? block.getBoundingClientRect().top - rootTop : 0;
    remember(block, top);
    for (const inside of block.querySelectorAll<HTMLElement>("[data-block-id]")) {
      remember(inside, top);
    }
  }
  return offsets;
}

export function blockPlacements(
  doc: ProseMirrorNode,
  offsets: ReadonlyMap<string, number>
): Map<string, MarkPlacement> {
  const placements = new Map<string, MarkPlacement>();
  doc.descendants((node, pos) => {
    const blockId = node.attrs.blockId;
    if (typeof blockId === "string" && !placements.has(blockId)) {
      const top = offsets.get(blockId);
      if (top !== undefined) {
        placements.set(blockId, { pos, top });
      }
    }
    return true;
  });
  return placements;
}

export function composerKindFor(kind: SelectionBarKind): "comment" | "suggestion" | "ask" {
  if (kind === "suggest") {
    return "suggestion";
  }
  return kind;
}

export function selectionBarKindFor(kind: "comment" | "suggestion" | "ask"): SelectionBarKind {
  return kind === "suggestion" ? "suggest" : kind;
}
