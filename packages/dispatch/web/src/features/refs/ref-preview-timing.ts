// The hover card's timing, in a module that imports nothing, so the Playwright specs read the
// same values the card runs on.

/** How long the pointer rests on a reference before its card opens. */
export const REF_PREVIEW_OPEN_DELAY_MS = 300;

/** How long a card outlives the pointer leaving it, or leaving its reference for anywhere but
 * the card: the time a pointer has to cross the gap between the two. A move straight from a
 * reference onto its card starts no countdown. */
export const REF_PREVIEW_CLOSE_DELAY_MS = 150;
