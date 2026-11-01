import { DOMSerializer, type Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { NodeView, ViewMutationRecord } from "@milkdown/kit/prose/view";

import { releaseViewFrame } from "../view/frame-budget";
import { removeEditorViewSlot, setEditorViewSlot } from "../view/ViewBlock";
import { renderViewBlockShell } from "./typed-block-drawing";

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
export class ViewBlockView implements NodeView {
  readonly dom: HTMLElement;
  readonly contentDOM: HTMLElement;
  private readonly slot: HTMLElement;
  private readonly key = `view-editor:${nextViewKey++}`;
  private node: ProseMirrorNode;

  constructor(node: ProseMirrorNode, document: Document) {
    this.node = node;
    const { contentDOM, dom } = DOMSerializer.renderSpec(document, renderViewBlockShell(node));
    this.dom = dom as HTMLElement;
    const slot = this.dom.querySelector<HTMLElement>(":scope > [data-dispatch-view-frame]");
    if (!(contentDOM instanceof HTMLElement) || slot === null) {
      throw new Error("the view shell has no caption hole or frame slot");
    }
    this.contentDOM = contentDOM;
    this.slot = slot;
    // The block's identity is the editor's to stamp, as `AskBlockView` stamps it.
    const blockId = node.attrs.blockId;
    if (typeof blockId === "string" && blockId !== "") {
      this.dom.dataset.blockId = blockId;
    }
    this.publish();
  }

  private publish(): void {
    setEditorViewSlot({
      caption: this.node.textContent.trim(),
      data: String(this.node.attrs.data ?? ""),
      host: this.slot,
      key: this.key,
      page: String(this.node.attrs.page ?? ""),
    });
  }

  update(node: ProseMirrorNode): boolean {
    if (node.type !== this.node.type || node.attrs.page !== this.node.attrs.page) {
      return false;
    }
    this.node = node;
    this.slot.setAttribute("data-dispatch-view-data", String(node.attrs.data ?? ""));
    this.publish();
    return true;
  }

  ignoreMutation(mutation: ViewMutationRecord): boolean {
    return !this.contentDOM.contains(mutation.target);
  }

  stopEvent(event: Event): boolean {
    return event.target instanceof Node && !this.contentDOM.contains(event.target);
  }

  destroy(): void {
    removeEditorViewSlot(this.key);
    releaseViewFrame(this.slot);
  }
}
