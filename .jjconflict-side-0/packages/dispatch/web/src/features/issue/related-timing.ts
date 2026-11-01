// The related-item check's timing, in a module that imports nothing, so the Playwright specs
// read the same values the dialog runs on.

/** How long the title rests before the check is asked: typing sends no request per keystroke. */
export const RELATED_DEBOUNCE_MS = 400;

/** Shorter than this, every item in the project would match, so nothing is asked. */
export const RELATED_TITLE_MIN = 2;
