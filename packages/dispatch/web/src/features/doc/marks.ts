import type { Node as ProseMirrorNode } from "prosemirror-model";

export const recordMarkTypes = ["proofComment", "proofSuggestion", "dispatchAsk"] as const;

type SelectionBarKind = "comment" | "suggest" | "ask";

export function markPositions(doc: ProseMirrorNode): Map<string, number> {
  const positions = new Map<string, number>();
  doc.descendants((node, pos) => {
    if (!node.isText) {
      return true;
    }
    for (const mark of node.marks) {
      if (
        recordMarkTypes.includes(mark.type.name as (typeof recordMarkTypes)[number]) &&
        typeof mark.attrs.id === "string" &&
        !positions.has(mark.attrs.id)
      ) {
        positions.set(mark.attrs.id, pos);
      }
    }
    return true;
  });
  return positions;
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
