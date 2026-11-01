/**
 * Pictures in Dispatch text. A message, comment, reply, ask question, answer or resolution reason
 * shows a picture with CommonMark's image syntax whose address is the picture's own Dispatch
 * reference, versioned: `![<file name>](dispatch://<owner>/artifact/<slug>@v<N>)`, the owner being
 * an issue key, a project key, or `agent/<session id>` for an agent's conversation on the Agents
 * page. The Dispatch tools write that syntax for the pictures they send, and show the pictures it
 * names to the model as image blocks within the limits below.
 */

import { type PictureType, pictureCaption } from "@legion/contracts";

/** The most bytes one upload carries: Dispatch refuses a larger file. */
export const PICTURE_UPLOAD_MAX_BYTES = 25 * 1024 * 1024;

/** The largest picture a model is shown, as raw bytes. The providers bound an image by its
 *  base64 encoding (Anthropic refuses one over 5 MB of base64, and the refused block stays in the
 *  session's history, so every later request fails the same way); 3,750,000 raw bytes encode to
 *  5,000,000. A larger picture is described instead of shown. */
export const PICTURE_SHOWN_MAX_BYTES = 3_750_000;

/** How a refusal names that cap: the raw bytes, and the rule they come from. */
const pictureShownMaxText = `${PICTURE_SHOWN_MAX_BYTES.toLocaleString("en-US")} bytes (5 MB of base64)`;

/** Why a picture of `bytes` named `name` is described instead of shown: it is over that cap. */
export function oversizePictureText(name: string, bytes: number): string {
  return `${name} is ${bytes.toLocaleString("en-US")} bytes, over the ${pictureShownMaxText} a model is shown`;
}

/** How many pictures one read or one delivery shows at most. */
export const PICTURES_SHOWN_MAX = 8;

/** How many bytes of pictures one read or one delivery shows at most. */
export const PICTURES_SHOWN_MAX_BYTES = 10 * 1024 * 1024;

export { isPictureType, type PictureType } from "@legion/contracts";

/** A picture as the hosts hand it to a model: base64 bytes and their type. */
export interface ToolImage {
  readonly data: string;
  readonly mimeType: PictureType;
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

/** A picture as a host's content block: the shape Oh My Pi's `ImageContent` and MCP's image
 *  content share, so a result's pictures spread into either after its text. */
export interface ImageBlock extends ToolImage {
  readonly type: "image";
}

/** `images` as the image blocks that follow a result's text, in order. */
export function imageBlocks(images: readonly ToolImage[]): ImageBlock[] {
  return images.map(({ data, mimeType }) => ({ type: "image", data, mimeType }));
}

/** The line a sent picture is written as, its file name the caption (`pictureCaption`). */
export function pictureLine(name: string, address: string): string {
  return `![${pictureCaption(name)}](${address})`;
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
