/**
 * Fragments the server cut out of a Markdown body before the client saw them: a search hit's
 * `ts_headline` snippet (24 words around the match) and a reply's `reply_body` (the parent's
 * first 160 runes). A cut can land inside a mark, and the half it leaves renders as its
 * characters. The balancer below drops exactly the delimiters that CommonMark would have paired
 * had the cut not happened and nothing else: a run the parser would read as literal text anyway
 * (`Use ** for bold`, `2 * 3`, `snake_case`) is the author's and stays, and nothing inside a
 * code span is ever a delimiter.
 */

/** How many runes of a parent's body a reply's event carries (`messageReplyPreview` in
 *  packages/envoy/internal/dispatch/api/messages.go: `HeadRunes(body, 160)`). A preview shorter
 *  than this is the whole body; one exactly this long is, or may be, cut. */
const REPLY_PREVIEW_RUNES = 160;

/** How many words a search hit's snippet holds when `ts_headline` cut the body to its window
 *  (`headlineOptions` in packages/envoy/internal/dispatch/api/search.go: `MaxWords=24`). A
 *  body that fits the window comes back whole and shorter. */
const SEARCH_SNIPPET_WORDS = 24;

/**
 * A reply's `reply_body` as a quote should render it: the whole parent when it was never cut
 * (under the limit, `HeadRunes` returns the body unchanged, and any delimiter in it is the
 * author's), and balanced when the server cut it (a body at the limit).
 */
export function replyPreviewMarkdown(replyBody: string | undefined): string {
  if (replyBody === undefined) {
    return "";
  }
  return Array.from(replyBody).length >= REPLY_PREVIEW_RUNES
    ? balanceCutMarkdown(replyBody)
    : replyBody;
}

/** A search hit's snippet as the palette should render it: the whole body when it fit the
 *  headline window, balanced when the window cut it (a snippet exactly the window wide). */
export function searchSnippetMarkdown(snippet: string): string {
  return snippet.split(/\s+/u).filter((word) => word !== "").length >= SEARCH_SNIPPET_WORDS
    ? balanceCutMarkdown(snippet)
    : snippet;
}

interface DelimiterRun {
  /** The delimiter character: `*`, `_`, `~`. */
  readonly char: string;
  readonly start: number;
  readonly end: number;
  readonly canOpen: boolean;
  readonly canClose: boolean;
}

const UNICODE_WHITESPACE = /\s/u;
const UNICODE_PUNCTUATION = /[\p{P}\p{S}]/u;

function isWhitespace(char: string | undefined): boolean {
  return char === undefined || UNICODE_WHITESPACE.test(char);
}

function isPunctuation(char: string | undefined): boolean {
  return char !== undefined && UNICODE_PUNCTUATION.test(char);
}

/** The spans of `text` that are code spans, as CommonMark reads them: a backtick run and the
 *  next run of the same length close it; a run with no closer is literal text. */
function codeSpans(text: string): Array<[number, number]> {
  const spans: Array<[number, number]> = [];
  let i = 0;
  while (i < text.length) {
    if (text[i] !== "`") {
      i += 1;
      continue;
    }
    let runEnd = i;
    while (text[runEnd] === "`") {
      runEnd += 1;
    }
    const fence = text.slice(i, runEnd);
    let close = text.indexOf(fence, runEnd);
    while (close !== -1 && text[close + fence.length] === "`") {
      // A longer run is not this span's closer; skip past it.
      let past = close;
      while (text[past] === "`") {
        past += 1;
      }
      close = text.indexOf(fence, past);
    }
    if (close === -1) {
      i = runEnd;
      continue;
    }
    spans.push([i, close + fence.length]);
    i = close + fence.length;
  }
  return spans;
}

/** CommonMark's delimiter runs of `*`, `_` and `~` outside code spans, each with whether it can
 *  open or close emphasis (the left- and right-flanking rules, with `_`'s word-internal
 *  restriction). A run that can do neither is literal text and is never a candidate. */
function delimiterRuns(text: string, code: Array<[number, number]>): DelimiterRun[] {
  const runs: DelimiterRun[] = [];
  let i = 0;
  while (i < text.length) {
    const inCode = code.find(([start, end]) => i >= start && i < end);
    if (inCode !== undefined) {
      i = inCode[1];
      continue;
    }
    const char = text[i];
    if (char !== "*" && char !== "_" && char !== "~") {
      i += 1;
      continue;
    }
    let end = i;
    while (text[end] === char) {
      end += 1;
    }
    const before = text[i - 1];
    const after = text[end];
    const leftFlanking =
      !isWhitespace(after) &&
      (!isPunctuation(after) || isWhitespace(before) || isPunctuation(before));
    const rightFlanking =
      !isWhitespace(before) &&
      (!isPunctuation(before) || isWhitespace(after) || isPunctuation(after));
    let canOpen = leftFlanking;
    let canClose = rightFlanking;
    if (char === "_") {
      canOpen = leftFlanking && (!rightFlanking || isPunctuation(before));
      canClose = rightFlanking && (!leftFlanking || isPunctuation(after));
    }
    if (canOpen || canClose) {
      runs.push({ canClose, canOpen, char, end, start: i });
    }
    i = end;
  }
  return runs;
}

/**
 * The fragment with every delimiter the cut left unmatched removed. Runs are paired as the
 * parser would pair them (a closer with the nearest earlier opener of the same character, the
 * run lengths consumed in order); whatever run, or part of a run, is left over on either side
 * could only have been matched across the cut, so it goes. A link cut inside its text or URL at
 * either edge keeps its words.
 */
export function balanceCutMarkdown(fragment: string): string {
  let text = fragment;
  text = text.replace(/\[([^[\]]*)\]\([^()\s]*$/u, "$1");
  text = text.replace(/\[([^[\]]*)$/u, "$1");
  text = text.replace(/^([^[\]]*)\]\([^()\s]*\)/u, "$1");

  const code = codeSpans(text);
  const runs = delimiterRuns(text, code);
  // Remaining unmatched length per run, consumed as pairs are made.
  const remaining = runs.map((run) => run.end - run.start);
  const openers: number[] = [];
  runs.forEach((run, index) => {
    if (run.canClose) {
      for (let o = openers.length - 1; o >= 0 && remaining[index] > 0; o -= 1) {
        const opener = runs[openers[o]];
        if (opener.char !== run.char || remaining[openers[o]] === 0) {
          continue;
        }
        // `~~` pairs only two at a time; `*` and `_` pair one or two.
        const take = Math.min(remaining[openers[o]], remaining[index], run.char === "~" ? 2 : 2);
        remaining[openers[o]] -= take;
        remaining[index] -= take;
        if (remaining[openers[o]] === 0) {
          openers.splice(o, 1);
        }
      }
    }
    if (run.canOpen && remaining[index] > 0) {
      openers.push(index);
    }
  });

  // Rebuild the text without the leftover characters, from the end so offsets hold.
  const removals: Array<[number, number]> = [];
  runs.forEach((run, index) => {
    if (remaining[index] > 0) {
      // Drop the unmatched characters from the side nearer the cut: an opener keeps its tail,
      // a closer its head. Either way the run's leftover is gone.
      removals.push(
        run.canOpen && !run.canClose
          ? [run.start, run.start + remaining[index]]
          : [run.end - remaining[index], run.end]
      );
    }
  });
  for (const [start, end] of removals.sort((a, b) => b[0] - a[0])) {
    text = text.slice(0, start) + text.slice(end);
  }

  // A backtick run with no closer is a code span the cut opened when the span it opened would
  // run to the fragment's end: the cut took its closer. The text after it is the code the cut
  // left - a path, an identifier - with no space-separated prose in it (`\`dispatch-deploy.y`).
  // A lone tick followed by ordinary words (`press the \` key then`) is the author's, as
  // CommonMark reads it.
  const closed = codeSpans(text);
  const lastRun = /`+(?!.*`)/su.exec(text);
  if (
    lastRun !== null &&
    !closed.some(([start, end]) => lastRun.index >= start && lastRun.index < end) &&
    !/\s\S+\s\S/u.test(text.slice(lastRun.index + lastRun[0].length))
  ) {
    text = text.slice(0, lastRun.index) + text.slice(lastRun.index + lastRun[0].length);
  }
  return text;
}
