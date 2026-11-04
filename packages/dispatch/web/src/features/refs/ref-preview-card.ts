// How a surface recognises the one hover card and the reference it belongs to, in a module that
// imports nothing, so a list judging a pointer does not pull the card's own module in with it.
// The card itself (`RefPreview`) is `fixed` and portaled into `document.body`, outside every
// list that opened it, and the DOM is the whole tie: the card carries the id below while it is
// shown, and its reference carries that same id as its `aria-describedby`.

/** The id the one hover card carries, and the `aria-describedby` of the reference whose card it
 *  is, for as long as it is open (`RefPreview`). */
export const REF_PREVIEW_CARD_ID = "ref-preview";

/** Whether `node` is the hover card or something inside it. */
export function refPreviewCardContains(node: EventTarget | null): boolean {
  return node instanceof Element && node.closest(`#${REF_PREVIEW_CARD_ID}`) !== null;
}

/** Whether the open card is the doing of a reference inside `root`: a pointer that moved from
 *  `root` onto that card has not left what `root` draws, however far outside it the card sits. */
export function refPreviewOpenedIn(root: Element): boolean {
  return root.querySelector(`[aria-describedby~="${REF_PREVIEW_CARD_ID}"]`) !== null;
}
