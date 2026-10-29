export interface ReferenceSpan {
  start: number;
  value: string;
}

/**
 * The reference-shaped spans in text: `dispatch://` references and `http(s)://` URLs, each ending
 * at whitespace (Unicode space separators included), an angle or square bracket, a quote, a
 * backtick or a pipe, then trimmed by `trimReference`. The one bracket a span may hold is a
 * bracketed IPv6 host right after `http(s)://`, so a server at an IPv6 literal keeps its dashboard
 * URLs as references. The whitespace is spelled out rather than written `\s`, which in JavaScript
 * also matches a vertical tab and U+FEFF, where Go's `\s` matches neither. It is the rule the
 * server's `text.ExtractAt` indexes mentions by, and `DISPATCH_TEXT_REFERENCES` in
 * `@legion/contracts` is the table both are tested against.
 */
const referencePattern =
  /(?:dispatch:\/\/|https?:\/\/(?:\[[0-9A-Fa-f:.]+\])?)[^\t\n\f\r \p{Z}<>"'`[\]|]+/gu;

/**
 * Every reference span in text that starts with `prefix`, trimmed; a span starting otherwise is
 * omitted from the result. The spans are the same whatever the prefix, so a `dispatch://` inside a
 * URL is never a span of its own, as the server reads it.
 */
export function referenceSpans(text: string, prefix = ""): ReferenceSpan[] {
  const spans: ReferenceSpan[] = [];
  for (const match of text.matchAll(referencePattern)) {
    if (match[0].startsWith(prefix)) {
      spans.push({ start: match.index, value: trimReference(match[0]) });
    }
  }
  return spans;
}

/**
 * Drops what trails a reference in prose: sentence punctuation, the `*`, `_` and `~` that close
 * emphasis and strikethrough around it (GFM's autolinks drop the same characters, keeping them
 * inside a link), and a closing parenthesis that opens nowhere in the reference. It counts the
 * parentheses once and walks back from the end, so a run of closers costs one pass.
 */
function trimReference(value: string): string {
  let opened = 0;
  let closed = 0;
  for (let index = 0; index < value.length; index += 1) {
    const char = value.charAt(index);
    if (char === "(") opened += 1;
    else if (char === ")") closed += 1;
  }
  let end = value.length;
  for (; end > 0; end -= 1) {
    const char = value.charAt(end - 1);
    if (char === ")") {
      if (opened >= closed) break;
      closed -= 1;
    } else if (!".,;:!?*_~".includes(char)) {
      break;
    }
  }
  return value.slice(0, end);
}
