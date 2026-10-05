/**
 * Two rules every TypeScript reader and writer of Dispatch text shares, so no copy drifts: the
 * session id an agent's conversation is addressed by, and the caption a picture line carries. The
 * Go server holds the same session-id rule (`text.IsSessionID`), pinned against the TypeScript
 * readers by `DISPATCH_TEXT_REFERENCES`.
 */

/**
 * The characters a session id may hold, as a regular-expression source for the `u` flag: anything
 * but `/`, `?` and `#` (which structure a reference), whitespace and every separator (`\p{Z}`,
 * which covers U+FEFF), a control character, or a character that ends a reference in running
 * text (`<>"'`[]|`). Embed it in a larger pattern; `isSessionId` tests a whole value.
 */
export const SESSION_ID_PATTERN = "[^/?#\\s\\p{Z}\\p{Cc}<>\"'`\\[\\]|]+";

const sessionIdOnly = new RegExp(`^${SESSION_ID_PATTERN}$`, "u");

/** Whether `value` is a session id a reference can name: non-empty and within the pattern. */
export function isSessionId(value: string): boolean {
  return sessionIdOnly.test(value);
}

/**
 * A file name as the caption of its picture line, `![<caption>](<address>)`. A caption is
 * CommonMark link text: a bracket or backslash would end it or escape what follows, a backtick
 * would open a code span across it, and a line break would end the picture, so those are escaped
 * and every run of whitespace reads as one space. The file keeps its name; only the caption
 * changes.
 */
export function pictureCaption(name: string): string {
  return name.replace(/[\\[\]`]/g, "\\$&").replace(/\s+/g, " ");
}
