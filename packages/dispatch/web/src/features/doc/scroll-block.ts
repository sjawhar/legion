import { refocusAfterDiagrams } from "./refocus-after-diagrams";

/** The attribute `@legion/proof-editor` stamps a block's id onto (`BLOCK_ID_DOM_ATTR`), written
 * out rather than imported: a value import from the editor library would pull it out of
 * `editor.ts`'s lazy chunk into every page that loads this module, as `ask-block.ts` explains.
 * Exported, so a reader of the drawn document names the attribute the same way. */
export const BLOCK_ID_ATTRIBUTE = "data-block-id";

/**
 * Scrolls `container` so `element` sits at its top. A container already showing it there is not
 * moved, the two rect tops being equal.
 *
 * This is the whole of the placement `scrollBlockToTop` performs, and the reading column's
 * `Next change` performs it too, for a block it has found by its mark rather than by its id.
 * That caller is deliberately not `scrollBlockToTop`: it wants this scroll alone, without the
 * re-run after the diagrams draw and without cancelling a pending one.
 */
export function scrollElementToTop(element: Element, container: HTMLElement): void {
  container.scrollTop +=
    element.getBoundingClientRect().top - container.getBoundingClientRect().top;
}

/** The dispose of the latest scroll's re-run after the diagrams draw. One waits at a time, as the
 * live document's focus does (`focusInEditor` in `ProofDocument.tsx`): a newer scroll drops an
 * older one's re-run, so a reader is never pulled back to the block they have left. */
let disposeRerun: (() => void) | undefined;

/**
 * Scrolls `container` so the block with `blockId` sits at its top, and answers whether the
 * container held that block. A container that already shows it there is not moved.
 *
 * It runs again once the page's mermaid diagrams have drawn (`refocusAfterDiagrams`, the module
 * the live document's focus re-runs through): a document's diagrams draw after the editor opened,
 * in Dispatch's Mermaid worker, and each one above the block pushes it down by the diagram's
 * height. The re-run reads the block afresh, so a node view redrawn in between is found again,
 * and it is dropped if the block has gone or the reader has pressed, scrolled, touched or typed.
 *
 * This is not `focusBlock` on the editor handle: that centres the block and pulses it, which is
 * what the document page wants of a link. A reading pane beside a list wants the block the reader
 * chose at the top of the column, with the document under it to read on into.
 */
export function scrollBlockToTop(blockId: string, container: HTMLElement): boolean {
  disposeRerun?.();
  disposeRerun = undefined;
  const selector = `[${BLOCK_ID_ATTRIBUTE}="${CSS.escape(blockId)}"]`;
  if (container.querySelector(selector) === null) {
    return false;
  }
  const scroll = () => {
    const block = container.querySelector(selector);
    if (block === null) {
      return;
    }
    scrollElementToTop(block, container);
  };
  scroll();
  disposeRerun = refocusAfterDiagrams(scroll);
  return true;
}
