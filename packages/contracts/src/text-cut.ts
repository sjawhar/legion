/**
 * `text` cut to at most `limit` UTF-16 units, for a limit that counts them, and one unit short of
 * it when the cut would split a surrogate pair: a character past U+FFFF (most emoji) is two units,
 * and the first left alone is no character at all, which a reader is shown as U+FFFD. Text within
 * the limit is returned as it is.
 */
export function cutUnits(text: string, limit: number): string {
  if (text.length <= limit) return text;
  if (limit <= 0) return "";
  const last = text.charCodeAt(limit - 1);
  return text.slice(0, last >= 0xd800 && last <= 0xdbff ? limit - 1 : limit);
}
