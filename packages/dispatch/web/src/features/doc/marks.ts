import type { Node as ProseMirrorNode } from "prosemirror-model";

import type { MarkPlacement } from "../margin/useMarginItems";

export const recordMarkTypes = ["proofComment", "proofSuggestion", "dispatchAsk"] as const;

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
