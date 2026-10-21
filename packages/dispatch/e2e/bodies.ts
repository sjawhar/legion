/** A body of exactly `length` UTF-16 units whose text changes along its whole span, so a body the
 *  command, the server or the read-back cut anywhere fails the equality, not only its length. */
export function bodyOf(length: number): string {
  let text = "";
  for (let line = 0; text.length < length; line += 1) {
    text += `${line}: the quick brown fox jumps over the lazy dog\n`;
  }
  return text.slice(0, length);
}
