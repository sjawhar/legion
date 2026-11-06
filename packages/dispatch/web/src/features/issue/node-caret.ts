import { BLOCK_ID_ATTRIBUTE } from "../doc/scroll-block";

/**
 * Putting the caret in a node's own text, where that text lives (dispatch://INFRA-1196 R9,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t17`).
 *
 * The edit key draws the node's document in the row (`document-stage.tsx`) and then calls this,
 * which places the caret at the start of that node's own block of it. It writes a DOM selection
 * inside the editor and nothing else: the editor's own observer reads the selection back as the
 * document's, so this touches neither the room nor the editor.
 *
 * `focusBlock` on the editor handle is not this: it scrolls the block into the middle of the
 * screen and pulses it, which is what a link into a document wants, and it places no caret
 * (`@legion/proof-editor`'s `lib.ts`). A row wants the caret, because editing is what the row
 * opened the document for.
 */
export function caretInBlock(host: HTMLElement, blockId: string | undefined): boolean {
  const editable = host.querySelector<HTMLElement>('[contenteditable="true"]');
  if (editable === null) return false;
  editable.focus();
  const block =
    blockId === undefined
      ? null
      : host.querySelector<HTMLElement>(`[${BLOCK_ID_ATTRIBUTE}="${CSS.escape(blockId)}"]`);
  const start = firstEditableText(block ?? editable);
  if (start === null) return true;
  const range = document.createRange();
  range.setStart(start, 0);
  range.collapse(true);
  const selection = window.getSelection();
  selection?.removeAllRanges();
  selection?.addRange(range);
  return true;
}

/** The first text of `root` a caret can enter: an atom's text and a collaborator's cursor label
 *  sit in `contenteditable="false"`, where a caret cannot go. */
function firstEditableText(root: HTMLElement): Node | null {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node.parentElement?.isContentEditable === true) return node;
  }
  return null;
}
