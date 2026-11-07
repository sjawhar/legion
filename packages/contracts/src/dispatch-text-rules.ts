/**
 * Eight rules every TypeScript reader and writer of Dispatch text shares, so no copy drifts: the
 * session id an agent's conversation is addressed by, whether an id is written short, how many
 * UTF-8 bytes a string holds, the most bytes a reference's target id may hold, the caption a
 * picture line carries, the syntax that line is written in, which uploads are pictures, and the
 * lines a person's message is compared by when a session says it delivered it. The Go server
 * holds the same session-id rule (`text.IsSessionID`), short-id rule (`text.IsIDPrefix`) and
 * target bound (`text.MaxTargetBytes`), all pinned against the TypeScript readers by
 * `DISPATCH_TEXT_REFERENCES`, and classifies an upload as a picture by the same four types, read
 * from its bytes.
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

/** An id written short: a hex digit and seven or more hex digits and hyphens, read without case.
 *  A whole uuid is no prefix of anything - an exact id needs no resolving and always wins - in
 *  either spelling an id prefix's characters allow: canonical, and 32 hex digits. */
const idPrefixPattern = /^[0-9a-f][0-9a-f-]{7,}$/i;
const wholeIdPattern =
  /^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/i;

/**
 * Whether `value` is an id written short, the one rule every TypeScript caller asks: the
 * dashboard, which resolves such an id against the reference graph before it reads anything
 * under it, and `dispatch`'s own executor, which resolves one against the items of the owner a
 * reference names. Go's twin is `text.IsIDPrefix`.
 */
export function isIdPrefix(value: string): boolean {
  return idPrefixPattern.test(value) && !wholeIdPattern.test(value);
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

/**
 * The pieces a picture line is written from, each repetition bounded so that a reader scans a
 * text once however it is written. Unbounded, the caption and the title had a body of many `![`
 * rescanned from each one of them: quadratic work on text a person writes (CodeQL
 * `js/polynomial-redos`). Every bound sits past what a writer can reach:
 *
 * - a caption is a file name (`pictureCaption`), which no file system Dispatch takes an upload
 *   from carries past 255 bytes and which escaping at most doubles, so 512 characters is past
 *   any of them; a line break ends a picture in markdown, so no caption holds one;
 * - an address is `dispatch://` and a target id, which holds at most
 *   MAX_REFERENCE_TARGET_BYTES (2048) UTF-8 bytes, so 2048 characters is at least as much;
 * - a version is a positive int64, whose largest value is 19 digits;
 * - a title is the short quoted string a writer may put after the address, past which 256
 *   characters is; at most four spaces or one line break and an indent separate the two.
 */
const PICTURE_CAPTION = String.raw`(?:\\.|[^\\\]\n]){0,512}`;
const PICTURE_ADDRESS = String.raw`dispatch://[^\s()]{1,2048}@v[1-9][0-9]{0,18}`;
const PICTURE_TITLE = String.raw`(?:\s{1,4}"(?:\\.|[^\\"]){0,256}")?`;
const PICTURE_SYNTAX = String.raw`!\[${PICTURE_CAPTION}\]\((${PICTURE_ADDRESS})${PICTURE_TITLE}\)`;

/**
 * The syntax a picture line is written in, as a regular expression of its own:
 * `![<caption>](<address>)` for a Dispatch address that names a version, escaped characters
 * inside the caption, and an optional quoted title after the address. The address is the first
 * capture group, which is how `pictureAddresses` reads the pictures a text shows; a reader that
 * takes the markdown back out replaces every match with nothing.
 *
 * Each call answers a fresh regular expression, because the `g` flag `matchAll` requires and
 * `replace` needs to reach every match carries `lastIndex` from one call to the next: one shared
 * object would have each reader start where another left off.
 */
export function pictureSyntax(): RegExp {
  return new RegExp(PICTURE_SYNTAX, "g");
}

/**
 * The lines a text is read as: each line trimmed, and the empty ones dropped. The line step of
 * `comparableLines`, exported because the Agents page reads a streamed row's side by it after
 * it has spent the body's own pictures against what the row carries (`rowLines` in
 * `packages/dispatch/web/src/features/agent-view/conversation.ts`), which is a strip of its
 * own that the body's side does not share. One step with both callers, so a later change to it
 * - CRLF, say - moves the two sides together; a copy of it on one side would leave every
 * honest row unmatched and every message shown twice (INFRA-1026).
 */
export function trimmedLines(text: string): string[] {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
}

/**
 * The lines a person's message is compared by, where a session's streamed row of it is read
 * against the body Dispatch stored: the pictures the body embeds taken out (`pictureSyntax`),
 * and what is left read as lines (`trimmedLines`).
 *
 * One rule with two readers, so a person's message is shown once rather than twice: the Claude
 * Code plugin tags the row with the Dispatch message it carries by it (`observe` in
 * `packages/claude-envoy/src/user-turn-router.ts`), and the Agents page hides Dispatch's stored
 * copy of a tagged message by it (`turnSaysBody` in
 * `packages/dispatch/web/src/features/agent-view/conversation.ts`, which reads the body's side
 * by this rule and the streamed row's side by its line step, once its own check of what the row
 * claims has passed). If the two drifted, one of them would stop recognising the row and the
 * person would read their own message twice.
 *
 * The pictures come out because a message carrying one reaches the session as its text and a
 * block of its own, so the markdown the body held is no word of the text the session says it
 * delivered: measured on Claude Code 2.1.310, a pane answering
 * `What colour is this picture? ![blue](dispatch://agent/<id>/artifact/i3-blue96-png@v1)` drew
 * the person's text with the markdown gone (INFRA-1026).
 */
export function comparableLines(text: string): string[] {
  return trimmedLines(text.replace(pictureSyntax(), ""));
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
