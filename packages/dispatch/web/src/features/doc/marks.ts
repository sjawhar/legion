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

export function blockOffsets(root: HTMLElement): Map<string, number> {
  const offsets = new Map<string, number>();
  const rootTop = root.getBoundingClientRect().top;
  for (const block of root.querySelectorAll<HTMLElement>("[data-block-id]")) {
    const blockId = block.dataset.blockId;
    if (blockId !== undefined && !offsets.has(blockId)) {
      offsets.set(blockId, block.getBoundingClientRect().top - rootTop);
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

export function setActiveMarkClass(root: HTMLElement, markIds: readonly string[]): void {
  const activeMarkIds = new Set(markIds);
  for (const mark of root.querySelectorAll<HTMLElement>("[data-id]")) {
    mark.classList.toggle("dispatch-mark-active", activeMarkIds.has(mark.dataset.id ?? ""));
  }
}

export function setActiveBlockClass(root: HTMLElement, blockIds: readonly string[]): void {
  const activeBlockIds = new Set(blockIds);
  for (const block of root.querySelectorAll<HTMLElement>("[data-block-id]")) {
    block.classList.toggle(
      "dispatch-block-active",
      activeBlockIds.has(block.dataset.blockId ?? "")
    );
  }
}
