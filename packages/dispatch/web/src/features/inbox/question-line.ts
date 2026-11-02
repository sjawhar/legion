/** A link or an image as its own words: `[text](target)` is its text, `![alt](src)` its alt. */
const LINK = /!?\[([^\]]*)\]\([^)]*\)/g;
/** Inline code, as the code it holds: one or more backticks around it, which both go. */
const CODE = /`+([^`]*)`+/g;
/** A leading heading marker, which markdown allows up to three spaces of indent before. */
const HEADING = /^ {0,3}#{1,6}\s+/;
/** `**bold**` and `__bold__`, which stand wherever they are written. */
const STRONG = /(\*\*|__)(?=\S)([\s\S]*?\S)\1/g;
/** `*emphasis*`, which stands wherever it is written. */
const STAR_EMPHASIS = /\*(?=\S)([^*]*?\S)\*/g;
/** `_emphasis_`, which markdown reads only between words: `snake_case_name` is one word. */
const UNDERSCORE_EMPHASIS = /(?<![\w_])_(?=\S)([^_]*?\S)_(?!\w)/g;

/**
 * A question's first line of plain text: what a short row in the Inbox's list column shows of it.
 *
 * The row has one line to say what the question is, and a question is markdown - a heading, a
 * sentence with a bold word, a link to the document it is about. Rendering it would need the
 * markdown engine for every row of the list and would draw block-level decoration into a cell
 * one line tall, so the row takes the first line that holds anything and strips the decoration
 * markdown writes around its words: a heading marker, emphasis, a link's target (the text stays,
 * the target goes), and inline code's backticks. Nothing is escaped or re-encoded - the result is
 * the question's own words, as text, with its spacing collapsed to one line.
 */
export function questionLine(question: string): string {
  for (const line of question.split("\n")) {
    const plain = line
      .replace(HEADING, "")
      .replace(LINK, "$1")
      .replace(CODE, "$1")
      .replace(STRONG, "$2")
      .replace(STAR_EMPHASIS, "$1")
      .replace(UNDERSCORE_EMPHASIS, "$1")
      .replace(/\s+/g, " ")
      .trim();
    if (plain !== "") {
      return plain;
    }
  }
  return "";
}
