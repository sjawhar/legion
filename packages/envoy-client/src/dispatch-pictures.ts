/**
 * Pictures in Dispatch text. A message, comment, reply, ask question, answer or resolution reason
 * shows a picture with CommonMark's image syntax whose address is the picture's own Dispatch
 * reference, versioned: `![<file name>](dispatch://<owner>/artifact/<slug>@v<N>)`, the owner being
 * an issue key, a project key, or `agent/<session id>` for an agent's conversation on the Agents
 * page. The Dispatch tools write that syntax for the pictures they send, and show the pictures it
 * names to the model as image blocks within the limits below.
 */

/** The most bytes one upload carries: Dispatch refuses a larger file. */
export const PICTURE_UPLOAD_MAX_BYTES = 25 * 1024 * 1024;

/** The largest picture a model is shown; the model providers refuse a larger one, so it is
 *  described instead. */
export const PICTURE_SHOWN_MAX_BYTES = 5 * 1024 * 1024;

/** How many pictures one read or one delivery shows at most. */
export const PICTURES_SHOWN_MAX = 8;

/** How many bytes of pictures one read or one delivery shows at most. */
export const PICTURES_SHOWN_MAX_BYTES = 10 * 1024 * 1024;

/** The picture types a model is shown, as their bytes say rather than as a name or header does. */
export type PictureType = "image/png" | "image/jpeg" | "image/gif" | "image/webp";

const PICTURE_TYPES: Readonly<Record<string, true>> = {
  "image/png": true,
  "image/jpeg": true,
  "image/gif": true,
  "image/webp": true,
};

/** Whether a stated MIME type (no parameters) is one of the picture types a model is shown. */
export function isPictureType(mime: string): mime is PictureType {
  return PICTURE_TYPES[mime] === true;
}

/** A picture as the hosts hand it to a model: base64 bytes and their type. */
export interface ToolImage {
  readonly data: string;
  readonly mimeType: string;
}

/** How many leading bytes `sniffPictureType` reads. */
export const PICTURE_SNIFF_BYTES = 12;

function bytesAt(bytes: Uint8Array, offset: number, expected: readonly number[]): boolean {
  return expected.every((byte, index) => bytes[offset + index] === byte);
}

/** The picture type `bytes` begin with (PNG, JPEG, GIF or WebP), or undefined for anything else. */
export function sniffPictureType(bytes: Uint8Array): PictureType | undefined {
  if (bytesAt(bytes, 0, [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])) return "image/png";
  if (bytesAt(bytes, 0, [0xff, 0xd8, 0xff])) return "image/jpeg";
  // GIF87a and GIF89a.
  if (
    bytesAt(bytes, 0, [0x47, 0x49, 0x46, 0x38]) &&
    (bytes[4] === 0x37 || bytes[4] === 0x39) &&
    bytes[5] === 0x61
  ) {
    return "image/gif";
  }
  // RIFF <size> WEBP.
  if (bytesAt(bytes, 0, [0x52, 0x49, 0x46, 0x46]) && bytesAt(bytes, 8, [0x57, 0x45, 0x42, 0x50])) {
    return "image/webp";
  }
  return undefined;
}

/** `bytes` as the image block a model is shown, or undefined when they are no picture it takes. */
export function toolImage(bytes: Uint8Array): ToolImage | undefined {
  const mimeType = sniffPictureType(bytes);
  if (mimeType === undefined || bytes.length > PICTURE_SHOWN_MAX_BYTES) return undefined;
  return { data: Buffer.from(bytes).toString("base64"), mimeType };
}

/** The line a sent picture is written as. The file name is its caption, with the characters that
 *  would end CommonMark's image text escaped and a line break read as a space. */
export function pictureLine(name: string, address: string): string {
  const caption = name.replace(/[\\[\]]/g, "\\$&").replace(/[\r\n]+/g, " ");
  return `![${caption}](${address})`;
}

/** `text` with the picture lines appended: one blank line, then one line per picture. Text that is
 *  only whitespace becomes the lines alone. */
export function withPictureLines(text: string, lines: readonly string[]): string {
  if (lines.length === 0) return text;
  const head = text.trimEnd();
  return head === "" ? lines.join("\n") : `${head}\n\n${lines.join("\n")}`;
}

// The image syntax with a Dispatch address that names a version: escaped characters inside the
// caption, and an optional quoted title after the address.
const PICTURE_SYNTAX =
  /!\[(?:\\.|[^\\\]])*\]\((dispatch:\/\/[^\s()]+@v[1-9][0-9]*)(?:\s+"(?:\\.|[^\\"])*")?\)/g;

/** The Dispatch addresses of the pictures `text` shows, in the order they appear. */
export function pictureAddresses(text: string): string[] {
  return [...text.matchAll(PICTURE_SYNTAX)].map((match) => match[1] as string);
}

/** One shown text and when it was written. */
export interface DatedText {
  readonly text: string;
  readonly at: string;
}

/** The addresses of the pictures the texts show, newest text first (in writing order within one
 *  text), each address once. */
export function picturesNewestFirst(texts: readonly DatedText[]): string[] {
  const ordered = texts
    .map((item, index) => ({ item, index, at: Date.parse(item.at) }))
    .sort(
      (left, right) =>
        (Number.isNaN(right.at) ? 0 : right.at) - (Number.isNaN(left.at) ? 0 : left.at) ||
        left.index - right.index
    );
  return [...new Set(ordered.flatMap(({ item }) => pictureAddresses(item.text)))];
}
