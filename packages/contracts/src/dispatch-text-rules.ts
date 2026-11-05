/**
 * Six rules every TypeScript reader and writer of Dispatch text shares, so no copy drifts: the
 * session id an agent's conversation is addressed by, how many UTF-8 bytes a string holds, the
 * most bytes a reference's target id may hold, the caption a picture line carries, which uploads
 * are pictures, and which content types a browser draws in the page rather than hands over as a
 * download. The Go server holds the same session-id rule (`text.IsSessionID`) and target bound
 * (`text.MaxTargetBytes`), both pinned against the TypeScript readers by
 * `DISPATCH_TEXT_REFERENCES`, classifies an upload as a picture by the same four types, read from
 * its bytes, and serves the inline types from the same list (`contracts.InlineContentTypes`,
 * generated from the one below).
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
 * How many UTF-8 bytes `value` holds, as Go's `len` counts a string: counted by code point, a lone
 * surrogate being the three of U+FFFD, which is what UTF-8 encoding writes for one. Every byte
 * bound the server shares is counted with it, never with TextEncoder: Bun 1.3's `encode` writes a
 * non-ASCII Latin-1 character of a long string as one byte, so 2000 `a`s and an `é` come to 2001.
 */
export function utf8Bytes(value: string): number {
  let bytes = 0;
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0;
    bytes += code < 0x80 ? 1 : code < 0x800 ? 2 : code < 0x10000 ? 3 : 4;
  }
  return bytes;
}

/**
 * The most UTF-8 bytes a reference's target id may hold in text: the id the server's graph stores
 * the edge under (`text.TargetID`), in Postgres B-tree keys of at most 2704 bytes. It is an
 * issue's key, a document's `<issue key or project>/<slug>` (`spec` for an issue's spec) or
 * `agent/<session id>/<slug>`, an ask's, a comment's or a message's id (decoded), a component's
 * `<project>/<id>`, or a block's `<document's id>#<block id>`. The grammar bounds none of those
 * parts but a project key, and text none but a block id (MAX_BLOCK_ID_BYTES), so text citing a
 * target whose id is past this cites nothing (`referenceTargetFits`), and a live document page
 * whose fragment names a block past it is its document. The bound decides only what text indexes:
 * a document stored under a longer name is read and written all the same, and a request naming it
 * by its own reference reads it. Go's twin is `text.MaxTargetBytes`.
 */
export const MAX_REFERENCE_TARGET_BYTES = 2048;

/**
 * Whether `target`, a reference's target id as MAX_REFERENCE_TARGET_BYTES spells it, holds at most
 * that many UTF-8 bytes (`utf8Bytes`), as the server's `text.targetFits` counts `text.TargetID`.
 */
export function referenceTargetFits(target: string): boolean {
  return utf8Bytes(target) <= MAX_REFERENCE_TARGET_BYTES;
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

/** The picture types a model is shown and the server stores as `kind: image`: what their bytes
 *  say, never a name or a header. Everything else, an SVG included, is a file. */
export type PictureType = "image/png" | "image/jpeg" | "image/gif" | "image/webp";

const PICTURE_TYPES: Readonly<Record<string, true>> = {
  "image/png": true,
  "image/jpeg": true,
  "image/gif": true,
  "image/webp": true,
};

/** Whether a stated MIME type (no parameters) is one of the picture types. */
export function isPictureType(mime: string): mime is PictureType {
  return PICTURE_TYPES[mime] === true;
}

/**
 * The content types a browser draws where it is read, rather than handing the reader a download:
 * the four picture types and the two video containers every browser Dispatch supports plays. A
 * request for an upload of one of these is answered `Content-Disposition: inline`, and a
 * reference to one is drawn in the page instead of a link. Go's twin is
 * `contracts.InlineContentTypes`, generated from this list.
 */
export const INLINE_CONTENT_TYPES = [
  "image/png",
  "image/jpeg",
  "image/gif",
  "image/webp",
  "video/mp4",
  "video/webm",
] as const;

export type InlineContentType = (typeof INLINE_CONTENT_TYPES)[number];

const inlineContentTypes: ReadonlySet<string> = new Set(INLINE_CONTENT_TYPES);

/** Whether a stated MIME type (no parameters) is one a browser draws inline. */
export function isInlineContentType(mime: string): mime is InlineContentType {
  return inlineContentTypes.has(mime);
}

/**
 * The inline content types a browser plays rather than shows: what a renderer asks for, since it
 * draws a video in a player with controls and a picture in an image. Every one of them is an
 * INLINE_CONTENT_TYPES member; what is inline and not a video is a picture.
 */
export const VIDEO_CONTENT_TYPES = ["video/mp4", "video/webm"] as const;

export type VideoContentType = (typeof VIDEO_CONTENT_TYPES)[number];

const videoContentTypes: ReadonlySet<string> = new Set(VIDEO_CONTENT_TYPES);

/** Whether a stated MIME type (no parameters) is one of the video types. */
export function isVideoContentType(mime: string): mime is VideoContentType {
  return videoContentTypes.has(mime);
}
