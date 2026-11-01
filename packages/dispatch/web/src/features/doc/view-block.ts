import { DOMSerializer, type Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { NodeView } from "@milkdown/kit/prose/view";

import { releaseViewFrame } from "../view/frame-budget";
import { removeEditorViewSlot, setEditorViewSlot } from "../view/ViewBlock";
import { renderViewBlockShell, VIEW_FRAME_SLOT } from "./typed-block-drawing";

let nextViewKey = 0;

/**
 * The `view` block's ProseMirror node view, in the live editor and in a version view. It owns
 * only the shell (`renderViewBlockShell`, the one spec a comment's body draws too): the header,
 * the caption hole, which is the node's editable content, and the frame slot beside it. The slot
 * is registered with `features/view/ViewBlock.tsx`, which portals a `ViewBlock` into it, because
 * a node view is not React and the frame reads the same queries a body's block reads.
 *
 * Two things make a plain `toDOM` spec unusable here. ProseMirror reads a DOM change inside a
 * spec-rendered node as a suspect edit and draws the node again from its state, which would
 * build a new iframe each time the page writes its height, so `ignoreMutation` keeps every
 * change outside the caption hole out of the editor's hands; and the slot is
 * `contenteditable="false"`, with `stopEvent` keeping a press inside the page's frame away from
 * the editor's keymaps. `update` keeps the view, and so the frame, while the node is a `view`
 * whose `page` is unchanged: a decoration, a caption edit and a new `data` attribute all leave
 * the frame running. A new `page` is a new view and a new frame.
 */
export function viewBlockView(node: ProseMirrorNode, document: Document): NodeView {
  const key = `view-editor:${nextViewKey++}`;
  const rendered = DOMSerializer.renderSpec(document, renderViewBlockShell(node));
  const dom = rendered.dom as HTMLElement;
  const contentDOM = rendered.contentDOM;
  const slot = dom.querySelector<HTMLElement>(`:scope > ${VIEW_FRAME_SLOT}`);
  if (!(contentDOM instanceof HTMLElement) || slot === null) {
    throw new Error("the view shell has no caption hole or frame slot");
  }
  // The block's identity is the editor's to stamp, as `AskBlockView` stamps it.
  const blockId = node.attrs.blockId;
  if (typeof blockId === "string" && blockId !== "") {
    dom.dataset.blockId = blockId;
  }
  let current = node;
  const publish = (): void => {
    setEditorViewSlot({
      caption: current.textContent.trim(),
      data: String(current.attrs.data ?? ""),
      host: slot,
      key,
      page: String(current.attrs.page ?? ""),
    });
  };
  publish();
  return {
    contentDOM,
    destroy: () => {
      removeEditorViewSlot(key);
      releaseViewFrame(slot);
    },
    dom,
    ignoreMutation: (mutation) => !contentDOM.contains(mutation.target),
    stopEvent: (event) => event.target instanceof Node && !contentDOM.contains(event.target),
    update: (next) => {
      if (next.type !== current.type || next.attrs.page !== current.attrs.page) {
        return false;
      }
      current = next;
      slot.dataset.dispatchViewData = String(next.attrs.data ?? "");
      publish();
      return true;
    },
  };
}
