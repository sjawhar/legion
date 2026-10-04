import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";

/** Text covered by the mark id across every text run, joined in document order. */
export function markedText(doc: ProseMirrorNode, id: string): string {
  let text = "";
  doc.descendants((node) => {
    if (node.isText && node.marks.some((mark) => mark.attrs.id === id)) text += node.text;
    return true;
  });
  return text;
}
