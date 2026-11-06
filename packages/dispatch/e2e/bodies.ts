import { DISPATCH_BLOCK_UNITS } from "@legion/contracts/dispatch-tools";

/** Lines of the same sentence, each ending in `ending`, cut to exactly `length` UTF-16 units. The
 *  line number opens every line, so the text changes along its whole span. */
function filledTo(length: number, ending: string): string {
  let text = "";
  for (let line = 0; text.length < length; line += 1) {
    text += `${line}: the quick brown fox jumps over the lazy dog${ending}`;
  }
  return text.slice(0, length);
}

/** A body of exactly `length` UTF-16 units whose text changes along its whole span, so a body the
 *  command, the server or the read-back cut anywhere fails the equality, not only its length. */
export function bodyOf(length: number): string {
  return filledTo(length, "\n");
}

/** A body of exactly `length` UTF-16 units made of paragraphs, each short enough for the bound
 *  Dispatch holds one block to, so the body passes that bound however long the whole text is. */
export function paragraphsOf(length: number): string {
  return filledTo(length, ".\n\n");
}

/** How the server refuses a body whose first block, a paragraph, is `units` UTF-16 units long,
 *  past the bound it holds each block to (itemblocks.CapExceeded). */
export function blockCapRefusal(units: number): string {
  return (
    `body block 1 (paragraph) is ${units - DISPATCH_BLOCK_UNITS} characters over the ` +
    `${DISPATCH_BLOCK_UNITS}-character block limit (${units}/${DISPATCH_BLOCK_UNITS})`
  );
}
