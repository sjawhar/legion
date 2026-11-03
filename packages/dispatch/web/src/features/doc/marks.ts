import type { Node as ProseMirrorNode } from "prosemirror-model";

import type { MarkPlacement } from "../margin/useMarginItems";

const recordMarkTypes = ["proofComment", "proofSuggestion", "dispatchAsk"] as const;

type SelectionBarKind = "comment" | "suggest" | "ask";

export function markPlacements(
  doc: ProseMirrorNode,
  offsets: ReadonlyMap<string, number>
): Map<string, MarkPlacement> {
  const placements = new Map<string, MarkPlacement>();
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
 * and every `ResizeObserver` tick, is a forced layout of the whole document each time, and it
 * forces every block the browser had skipped (`content-visibility: auto`) to lay out one at a
 * time (dispatch://INFRA-341, requirement R11).
 */
export function blockOffsets(root: HTMLElement, placed: boolean): Map<string, number> {
  const offsets = new Map<string, number>();
  const rootTop = placed ? root.getBoundingClientRect().top : 0;
  for (const block of root.querySelectorAll<HTMLElement>("[data-block-id]")) {
    const blockId = block.dataset.blockId;
    if (blockId !== undefined && !offsets.has(blockId)) {
      offsets.set(blockId, placed ? block.getBoundingClientRect().top - rootTop : 0);
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
