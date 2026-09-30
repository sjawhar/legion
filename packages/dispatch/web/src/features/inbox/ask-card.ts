/**
 * Where an ask card's own keys end and a page's begin.
 *
 * A card's options are radios and checkboxes, and its actions are buttons - none of which takes
 * typed text, so the keyboard registry passes single keys straight through to whatever scope the
 * page has pushed (`shell/keymap.ts`'s editable policy). That permission carries an obligation:
 * a page whose key writes something must say for itself where the reader may press it, and the
 * answer on every surface that renders an `AskCard` is the same - not inside the card. The Inbox
 * asks it of `h`, and the issue page of `0`-`3` and `Shift+P`.
 */

/** An ask card anywhere on the page, wherever it is rendered. */
const ASK_CARD = "[data-ask-card]";

/** Whether the reader is outside every ask card, so a page-level key is the page's to act on. */
export function outsideAskCard(): boolean {
  return document.activeElement?.closest(ASK_CARD) == null;
}
