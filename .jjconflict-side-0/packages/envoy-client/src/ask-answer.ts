/** Head length, in UTF-16 units, for one-line summaries of event text. */
const HEAD_LENGTH = 120;

/** The first HEAD_LENGTH units of `text` on one line (whitespace collapsed), with an ellipsis when cut. */
export function textHead(text: string): string {
  const flat = text.replace(/\s+/g, " ").trim();
  return flat.length > HEAD_LENGTH ? `${flat.slice(0, HEAD_LENGTH)}…` : flat;
}

/**
 * How an answer reads as one line: the text alone when nothing was selected, the selected
 * options alone when nothing was typed, and `<options> - <text>` when a human did both.
 */
export function askAnswerText(
  answer:
    | {
        readonly selected?: readonly string[] | null | undefined;
        readonly text?: string | null | undefined;
      }
    | null
    | undefined
): string {
  if (answer === null || answer === undefined) return "";
  const selected = (answer.selected ?? []).join(", ");
  const text = answer.text ?? "";
  if (text === "") return selected;
  if (selected === "") return text;
  return `${selected} - ${text}`;
}
