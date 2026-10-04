/**
 * A fragment the server cut out of a Markdown body before the client saw it: a search hit's
 * `ts_headline` snippet (24 words around the match) or a reply's `reply_body` (the parent's
 * first 160 runes). A cut can land inside a mark, and an unmatched `**`, backtick or `[link](`
 * renders as those characters. Every mark left unmatched on either side is dropped, so the
 * words render plain where their formatting was cut away rather than showing its syntax; a
 * whole body is never passed through this, since its marks are balanced as written.
 */
export function balanceCutMarkdown(fragment: string): string {
  let text = fragment;
  // A link cut inside its URL or its text, at either end of the fragment: keep the words.
  text = text.replace(/\[([^[\]]*)\]\([^()\s]*$/u, "$1");
  text = text.replace(/\[([^[\]]*)$/u, "$1");
  text = text.replace(/^([^[\]]*)\]\([^()\s]*\)/u, "$1");
  // Paired delimiters: an odd count means one side was cut away; the last one is the stray.
  for (const delimiter of ["**", "__", "~~", "`"]) {
    const count = text.split(delimiter).length - 1;
    if (count % 2 === 1) {
      const at = text.lastIndexOf(delimiter);
      text = text.slice(0, at) + text.slice(at + delimiter.length);
    }
  }
  return text;
}
