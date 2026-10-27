// Checks and re-lays out the semantic line breaks of a markdown file, so concurrent edits to it
// touch different lines: one sentence per line (R1), a blank line before each list item (R2), and
// no list item or table row longer than one screen (R3). It is not a CI gate:
// packages/envoy/AGENTS.md and packages/envoy/cmd/dispatch/README.md keep this layout, and
// `--write` fixes it by changing only whitespace.
//
// Lengths are UTF-16 code units (JavaScript string length), and a tab counts as one column.
import { readFile, writeFile } from "node:fs/promises";

const USAGE = "usage: bun scripts/semantic-line-breaks.ts [--write] <file.md>...\n";

/** A list item or table row longer than this many screen rows fails R3. */
const MAX_SCREEN_ROWS = 50;
const SCREEN_WIDTH = 100;
/** `--write` breaks a line longer than this after `; ` or `: `, or before ` —`. */
const MAX_LINE = 160;
/** Neither part of such a break may be shorter than this, indentation excluded. */
const MIN_PART = 40;

const BLANK = /^[ \t]*$/;
const INDENT = /^[ \t]*/;
const HARD_BREAK = /(?: {2,}|\\)$/;
const FENCE_OPEN = /^(`{3,}|~{3,})(.*)$/;
const FENCE_CLOSE = /^[ \t]*(`{3,}|~{3,})[ \t]*$/;
const ATX_HEADING = /^#{1,6}(?:[ \t]|$)/;
const SETEXT_UNDERLINE = /^(?:=+|-+)[ \t]*$/;
const THEMATIC_BREAK = /^([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
const ITEM_MARKER = /^(?:([-*+])|(\d{1,9})[.)])(?=[ \t]|$)/;
/** A link reference definition: its destination, if on this line, and what follows it. */
const LINK_DEFINITION = /^\[[^\]]+\]:[ \t]*(?:(<[^>]*>|[^ \t]+)[ \t]*)?(.*)$/;
/** A link destination at the start of a definition's second line, and what follows it. */
const LINK_DESTINATION = /^(?:<[^>]*>|[^ \t]+)[ \t]*(.*)$/;
const TITLE_CLOSERS: Partial<Record<string, string>> = { '"': '"', "'": "'", "(": ")" };
/** A GFM table delimiter row, trimmed, if it holds a `|`: micromark reads `---` as an underline. */
const DELIMITER_ROW = /^\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)*\|?$/;
/** CommonMark HTML block kinds 1 to 5: what starts each and the text that ends it. */
const HTML_BLOCKS: readonly (readonly [RegExp, RegExp])[] = [
  [/^<(?:script|pre|style|textarea)(?:[ \t>]|$)/i, /<\/(?:script|pre|style|textarea)>/i],
  [/^<!--/, /-->/],
  [/^<\?/, /\?>/],
  [/^<![A-Za-z]/, />/],
  [/^<!\[CDATA\[/, /\]\]>/],
];
/** HTML block kind 6, which like kinds 1 to 5 may interrupt a paragraph; a blank line ends it. */
const HTML_BLOCK_TAG =
  /^<\/?(?:address|article|aside|base|basefont|blockquote|body|caption|center|col|colgroup|dd|details|dialog|dir|div|dl|dt|fieldset|figcaption|figure|footer|form|frame|frameset|h[1-6]|head|header|hr|html|iframe|legend|li|link|main|menu|menuitem|nav|noframes|ol|optgroup|option|p|param|search|section|summary|table|tbody|td|tfoot|th|thead|title|tr|track|ul)(?:[ \t]|\/?>|$)/i;
/** HTML block kind 7: a complete open or closing tag alone on the line; a blank line ends it. */
const HTML_TAG_LINE =
  /^<(?!\/?(?:script|pre|style|textarea)\b)(?:[A-Za-z][A-Za-z0-9-]*(?:[ \t]+[A-Za-z_:][\w.:-]*(?:[ \t]*=[ \t]*(?:[^ \t"'=<>`]+|'[^']*'|"[^"]*"))?)*[ \t]*\/?|\/[A-Za-z][A-Za-z0-9-]*[ \t]*)>[ \t]*$/i;

/**
 * Text a continuation line may not start with, because it would open a block or end the
 * paragraph, or a whole line that could be a setext underline, a rule or a table delimiter row.
 */
const CONTINUATION_BLOCK_START =
  /^(?:[-+*](?:[ \t]|$)|#{1,6}(?:[ \t]|$)|[>|<]|\d{1,9}[.)](?:[ \t]|$)|`{3,}|~{3,}|[-=*_|: \t]+$)/;

const SENTENCE_MARKS = ".!?";
const SENTENCE_CLOSERS = ")]\"'*_”’";
const SENTENCE_START = /^(?:[\p{Lu}a-z0-9`["“]|\*\*)/u;
const ABBREVIATIONS = [
  "e.g.",
  "E.g.",
  "i.e.",
  "I.e.",
  "etc.",
  "vs.",
  "cf.",
  "ex.",
  "eg.",
  "ie.",
  "No.",
  "Fig.",
  "Mr.",
  "Dr.",
  "U.S.",
];
const LETTER_OR_DIGIT = /[\p{L}\p{N}]/u;
/** Text up to a `.` that numbers an item or heading, not a sentence end: `1.`, `- 12.`, `## 3.`. */
const LIST_MARKER_PREFIX = /^[ \t]*(?:#{1,6}[ \t]+)?(?:(?:[-*+]|\d{1,9}[.)])[ \t]+)*\d{1,9}\.$/;
/** The whitespace `tr -s '[:space:]' ' '` folds. */
const WHITESPACE_RUN = /[ \t\n\v\f\r]+/g;

type LineKind = "blank" | "fence" | "code" | "table" | "heading" | "html" | "text" | "other";

interface Paragraph {
  first: number;
  last: number;
  continuationIndent: string;
}

interface LineInfo {
  kind: LineKind;
  /** The line holds a list item's marker. */
  itemStart: boolean;
  paragraph?: Paragraph;
}

interface ListItem {
  first: number;
  /** The item's last non-blank line. */
  last: number;
  contentColumn: number;
}

interface Blocks {
  lines: LineInfo[];
  items: ListItem[];
}

interface Fence {
  char: string;
  length: number;
}

interface ItemMarker {
  bullet: boolean;
  start: number;
  empty: boolean;
  contentStart: number;
  contentColumn: number;
}

/** What the next line of a link reference definition may still hold. */
type DefinitionTail = "destination" | "title" | { closer: string };

type BlockStart =
  | { type: "item"; marker: ItemMarker }
  | { type: "fence"; fence: Fence }
  /** `end` is the text whose line ends the block, undefined when the start line already did. */
  | { type: "html"; end: RegExp | undefined }
  | { type: "definition"; tail: DefinitionTail | undefined }
  | { type: "code" | "table" | "heading" | "other" };

interface Finding {
  line: number;
  rule: 1 | 2 | 3;
  message: string;
}

function isBlank(line: string): boolean {
  return BLANK.test(line);
}

function indentLength(line: string): number {
  return INDENT.exec(line)?.[0].length ?? 0;
}

function splitLines(text: string): { lines: string[]; trailingNewline: boolean } {
  const trailingNewline = text.endsWith("\n");
  return { lines: (trailingNewline ? text.slice(0, -1) : text).split("\n"), trailingNewline };
}

function parseItemMarker(line: string, at: number): ItemMarker | undefined {
  const match = ITEM_MARKER.exec(line.slice(at));
  if (!match) return undefined;
  const markerEnd = at + match[0].length;
  const spaces = indentLength(line.slice(markerEnd));
  const empty = markerEnd + spaces === line.length;
  // Five or more spaces after the marker start indented code one column past it.
  const contentColumn = empty || spaces > 4 ? markerEnd + 1 : markerEnd + spaces;
  return {
    bullet: match[1] !== undefined,
    start: Number(match[2] ?? 0),
    empty,
    contentStart: empty ? line.length : contentColumn,
    contentColumn,
  };
}

/** Index of the first `char` at or after `from` that no backslash escapes, or -1. */
function unescapedIndex(text: string, char: string, from: number): number {
  for (let at = from; at < text.length; at++) {
    if (text[at] === "\\") at++;
    else if (text[at] === char) return at;
  }
  return -1;
}

/** How many cells a table row holds: an outer `|` on either side does not separate two. */
function tableCells(row: string): number {
  const text = row.trim();
  let cells = 1;
  for (let at = unescapedIndex(text, "|", 0); at !== -1; at = unescapedIndex(text, "|", at + 1)) {
    cells++;
  }
  if (text.startsWith("|")) cells--;
  if (text.length > 1 && text.endsWith("|") && !text.endsWith("\\|")) cells--;
  return cells;
}

/** What may follow a link reference definition whose line holds `rest` after its destination. */
function definitionTail(rest: string): DefinitionTail | undefined {
  if (rest === "") return "title";
  const closer = TITLE_CLOSERS[rest.charAt(0)];
  return closer !== undefined && unescapedIndex(rest, closer, 1) === -1 ? { closer } : undefined;
}

/**
 * The block the line opens at `at`, `rel` columns past its container's content column, or
 * undefined for paragraph text. `interrupts` is set when the line would otherwise continue an open
 * paragraph in the same container, which only some blocks may interrupt; `next` is the line after
 * it, which makes it a table header when it is a delimiter row.
 */
function blockStart(
  line: string,
  at: number,
  rel: number,
  inParagraph: boolean,
  interrupts: boolean,
  next: string | undefined
): BlockStart | undefined {
  const rest = line.slice(at);
  if (rel >= 4) return inParagraph ? undefined : { type: "code" };
  const fence = FENCE_OPEN.exec(rest);
  const run = fence?.[1] ?? "";
  if (fence && !(run.startsWith("`") && (fence[2] ?? "").includes("`"))) {
    return { type: "fence", fence: { char: run.charAt(0), length: run.length } };
  }
  if (ATX_HEADING.test(rest)) return { type: "heading" };
  if (interrupts && SETEXT_UNDERLINE.test(rest)) return { type: "other" };
  if (THEMATIC_BREAK.test(rest)) return { type: "other" };
  const marker = parseItemMarker(line, at);
  if (marker && (!interrupts || (!marker.empty && (marker.bullet || marker.start === 1)))) {
    return { type: "item", marker };
  }
  if (rest.startsWith(">")) return { type: "other" };
  // A table starts only at a header the next line's delimiter row matches, as in micromark: a
  // `|`-led line without one is paragraph text. A table header may interrupt a paragraph.
  const delimiter = next?.trim() ?? "";
  if (
    delimiter.includes("|") &&
    DELIMITER_ROW.test(delimiter) &&
    tableCells(rest) === tableCells(delimiter)
  ) {
    return { type: "table" };
  }
  if (rest.startsWith("<")) {
    const kind = HTML_BLOCKS.find(([opens]) => opens.test(rest));
    if (kind) return { type: "html", end: kind[1].test(rest) ? undefined : kind[1] };
    if (HTML_BLOCK_TAG.test(rest) || (!inParagraph && HTML_TAG_LINE.test(rest))) {
      return { type: "html", end: BLANK };
    }
  }
  const definition = inParagraph ? null : LINK_DEFINITION.exec(rest);
  if (definition) {
    const destination = definition[1];
    const tail = destination === undefined ? "destination" : definitionTail(definition[2] ?? "");
    return { type: "definition", tail };
  }
  return undefined;
}

/**
 * Sorts each line into the block it belongs to, CommonMark-style: fences, indented code, tables,
 * HTML blocks, link reference definitions, paragraphs (lazy continuation lines included), list
 * items and the lines that are none of these. Check and write both read the document through this.
 */
function scanBlocks(source: readonly string[]): Blocks {
  const lines: LineInfo[] = [];
  const items: ListItem[] = [];
  const open: ListItem[] = [];
  let paragraph: Paragraph | undefined;
  let fence: Fence | undefined;
  /** What ends the open HTML block; for BLANK (kinds 6 and 7) the blank line is outside it. */
  let html: RegExp | undefined;
  let definition: DefinitionTail | undefined;
  let table = false;

  const info = (kind: LineKind, itemStart = false): LineInfo => ({ kind, itemStart });

  const startParagraph = (index: number, continuationIndent: string): LineInfo => {
    paragraph = { first: index, last: index, continuationIndent };
    return { kind: "text", itemStart: false, paragraph };
  };

  const openBlock = (start: Exclude<BlockStart, { type: "item" }>): LineInfo => {
    switch (start.type) {
      case "fence":
        fence = start.fence;
        return info("fence");
      case "html":
        html = start.end;
        return info("html");
      case "definition":
        definition = start.tail;
        return info("other");
      case "table":
        table = true;
        return info("table");
      case "code":
      case "heading":
        return info(start.type);
      default:
        return info("other");
    }
  };

  // A marker line may hold nested markers, then any block, then paragraph text.
  const openItems = (
    index: number,
    line: string,
    first: ItemMarker,
    next: string | undefined
  ): LineInfo => {
    let marker = first;
    for (;;) {
      const item = { first: index, last: index, contentColumn: marker.contentColumn };
      open.push(item);
      items.push(item);
      if (marker.empty) return info("other", true);
      const at = marker.contentStart + indentLength(line.slice(marker.contentStart));
      const inner = blockStart(line, at, at - marker.contentColumn, false, false, next);
      if (inner?.type === "item") {
        marker = inner.marker;
        continue;
      }
      if (inner) return { ...openBlock(inner), itemStart: true };
      return { ...startParagraph(index, " ".repeat(marker.contentColumn)), itemStart: true };
    }
  };

  const blockLine = (index: number, line: string, next: string | undefined): LineInfo => {
    const at = indentLength(line);
    let matched = 0;
    while (matched < open.length && at >= (open[matched]?.contentColumn ?? 0)) matched++;
    const container = open[matched - 1]?.contentColumn ?? 0;
    const inParagraph = paragraph !== undefined;
    const start = blockStart(
      line,
      at,
      at - container,
      inParagraph,
      inParagraph && matched === open.length,
      next
    );
    // Inside a table, indented text and a delimiter row are rows too.
    if (
      table &&
      (start === undefined || start.type === "code" || DELIMITER_ROW.test(line.trim()))
    ) {
      return info("table");
    }
    if (start === undefined) {
      if (paragraph) {
        paragraph.last = index;
        return { kind: "text", itemStart: false, paragraph };
      }
      open.length = matched;
      return startParagraph(index, open.length > 0 ? line.slice(0, at) : "");
    }
    // A table line inside a paragraph keeps the paragraph's items open, as a lazy line would.
    if (!(start.type === "table" && inParagraph)) open.length = matched;
    paragraph = undefined;
    table = false;
    return start.type === "item" ? openItems(index, line, start.marker, next) : openBlock(start);
  };

  /** Whether the line goes on with the link reference definition above it, a title or its rest. */
  const continuesDefinition = (tail: DefinitionTail, line: string): boolean => {
    const text = line.trim();
    definition = undefined;
    if (tail === "destination") {
      definition = definitionTail(LINK_DESTINATION.exec(text)?.[1] ?? "");
      return true;
    }
    if (tail === "title") {
      const closer = TITLE_CLOSERS[text.charAt(0)];
      if (closer === undefined) return false;
      const end = unescapedIndex(text, closer, 1);
      if (end === -1) definition = { closer };
      // Text after the closing quote makes it no title: the line is a paragraph's.
      return end === -1 || isBlank(text.slice(end + 1));
    }
    if (unescapedIndex(text, tail.closer, 0) === -1) definition = tail;
    return true;
  };

  for (const [index, line] of source.entries()) {
    // A fence or HTML block inside a list item ends with the item.
    const innermost = open.at(-1);
    if (
      (fence || html) &&
      innermost &&
      !isBlank(line) &&
      indentLength(line) < innermost.contentColumn
    ) {
      fence = undefined;
      html = undefined;
    }
    if (fence) {
      const run = FENCE_CLOSE.exec(line)?.[1] ?? "";
      if (run.startsWith(fence.char) && run.length >= fence.length) fence = undefined;
      lines.push(info("fence"));
    } else if (html && (html !== BLANK || !isBlank(line))) {
      if (html.test(line)) html = undefined;
      lines.push(info("html"));
    } else if (isBlank(line)) {
      paragraph = undefined;
      html = undefined;
      definition = undefined;
      table = false;
      lines.push(info("blank"));
    } else if (definition !== undefined && continuesDefinition(definition, line)) {
      lines.push(info("other"));
    } else {
      lines.push(blockLine(index, line, source[index + 1]));
    }
    if (!isBlank(line)) for (const item of open) item.last = index;
  }
  return { lines, items };
}

/** Index of the next run of exactly `length` backticks at or after `from`, or -1. */
function findBacktickRun(line: string, from: number, length: number): number {
  let at = from;
  while (at < line.length) {
    if (line[at] !== "`") {
      at++;
      continue;
    }
    const run = backtickRunLength(line, at);
    if (run === length) return at;
    at += run;
  }
  return -1;
}

function backtickRunLength(line: string, at: number): number {
  let end = at;
  while (line[end] === "`") end++;
  return end - at;
}

/** Index of the `)` that closes a link destination starting at `from`, or -1. */
function destinationEnd(line: string, from: number): number {
  let depth = 0;
  for (let at = from; at < line.length; at++) {
    const char = line[at];
    if (char === "\\") at++;
    else if (char === "(") depth++;
    else if (char === ")") {
      if (depth === 0) return at;
      depth--;
    }
  }
  return -1;
}

/**
 * Which characters of the line are prose: outside inline code spans and link destinations. A
 * backtick run with no equal-length run after it on the line is literal text.
 */
function proseMask(line: string): boolean[] {
  const prose = new Array<boolean>(line.length).fill(true);
  let at = 0;
  while (at < line.length) {
    const char = line[at];
    if (char === "\\") {
      at += 2;
    } else if (char === "`") {
      const run = backtickRunLength(line, at);
      const close = findBacktickRun(line, at + run, run);
      const end = close === -1 ? at + run : close + run;
      if (close !== -1) prose.fill(false, at, end);
      at = end;
    } else if (char === "]" && line[at + 1] === "(") {
      const end = destinationEnd(line, at + 2);
      if (end === -1) {
        at++;
      } else {
        prose.fill(false, at + 2, end);
        at = end;
      }
    } else {
      at++;
    }
  }
  return prose;
}

/**
 * The index of each space on the line that separates two sentences (R1): a sentence mark, its
 * closers, then this one space, then a sentence start. A sentence that starts with a lowercase
 * word (`ygo's`, `envoy-dispatch`) follows only a bare `.`, maybe emphasised: after `?`, `!`, a
 * quote, a parenthesis or an ellipsis such a word goes on with the sentence (`asks "why?" and`,
 * `./... in`). Check mode reports these and `--write` turns them into line breaks.
 */
function sentenceBreaks(line: string): number[] {
  const prose = proseMask(line);
  const breaks: number[] = [];
  for (let mark = 0; mark < line.length; mark++) {
    if (!prose[mark] || !SENTENCE_MARKS.includes(line.charAt(mark))) continue;
    let space = mark + 1;
    while (prose[space] && SENTENCE_CLOSERS.includes(line.charAt(space))) space++;
    if (line[space] !== " " || !prose[space]) continue;
    if (!SENTENCE_START.test(line.slice(space + 1, space + 3))) continue;
    const sentence = line.slice(0, mark + 1);
    const abbreviation = ABBREVIATIONS.find((word) => sentence.endsWith(word));
    const before = sentence.charAt(sentence.length - (abbreviation?.length ?? 0) - 1);
    if (abbreviation !== undefined && !LETTER_OR_DIGIT.test(before)) continue;
    if (LIST_MARKER_PREFIX.test(sentence)) continue;
    const lowercase = /[a-z]/.test(line.charAt(space + 1));
    if (lowercase && (!/^\.[*_]*$/.test(line.slice(mark, space)) || sentence.endsWith(".."))) {
      continue;
    }
    breaks.push(space);
  }
  return breaks;
}

/**
 * The index of each space a line longer than MAX_LINE may break at: after `; ` or `: `, or before
 * ` —`.
 */
function longLineBreaks(line: string): number[] {
  const prose = proseMask(line);
  const breaks: number[] = [];
  for (let space = 1; space < line.length - 1; space++) {
    if (line[space] !== " " || !prose[space] || !prose[space - 1]) continue;
    const before = line.charAt(space - 1);
    const after = line.charAt(space + 1);
    const afterClause = (before === ";" || before === ":") && after !== " " && after !== "\t";
    const beforeDash = after === "—" && before !== " " && before !== "\t" && before !== "\\";
    if (afterClause || beforeDash) breaks.push(space);
  }
  return breaks;
}

/**
 * The line cut at each of the given spaces, the spaces dropped. A cut that would start a line with
 * text that opens a block or ends the paragraph is skipped.
 */
function cutLine(line: string, cuts: readonly number[]): string[] {
  let kept = [...cuts];
  for (;;) {
    const pieces = [0, ...kept.map((cut) => cut + 1)].map((start, n) =>
      line.slice(start, kept[n] ?? line.length)
    );
    const bad = pieces.findIndex((piece, n) => n > 0 && CONTINUATION_BLOCK_START.test(piece));
    if (bad === -1) return pieces;
    kept = kept.filter((_, n) => n !== bad - 1);
  }
}

/** Breaks a line longer than MAX_LINE, left to right, while what is left is still too long. */
function wrapLongLine(line: string, indent: string): string[] {
  if (line.length <= MAX_LINE) return [line];
  const lead = indentLength(line);
  const cuts: number[] = [];
  let start = lead;
  for (const cut of longLineBreaks(line)) {
    if ((start === lead ? lead : indent.length) + line.length - start <= MAX_LINE) break;
    const rest = line.slice(cut + 1);
    if (
      cut - start >= MIN_PART &&
      rest.length >= MIN_PART &&
      !CONTINUATION_BLOCK_START.test(rest)
    ) {
      cuts.push(cut);
      start = cut + 1;
    }
  }
  return cutLine(line, cuts).map((piece, n) => (n === 0 ? piece : indent + piece));
}

/**
 * The lines of a paragraph joined into logical lines, kept apart at hard breaks. A line that starts
 * with `<` inside a paragraph is text, not an HTML block (the scanner ends the paragraph before
 * one), and is joined like any other: it may continue a code span the line before opened.
 */
function joinParagraph(source: readonly string[]): string[] {
  const logical: string[] = [];
  for (const line of source) {
    const previous = logical.at(-1);
    if (previous === undefined || HARD_BREAK.test(previous)) {
      logical.push(line);
    } else {
      logical[logical.length - 1] =
        `${previous.replace(/[ \t]+$/, "")} ${line.replace(INDENT, "")}`;
    }
  }
  return logical;
}

/** R2: a list item that starts right after a non-blank line. */
function needsBlankBefore(
  lines: readonly string[],
  info: LineInfo | undefined,
  index: number
): boolean {
  return info?.itemStart === true && index > 0 && !isBlank(lines[index - 1] ?? "");
}

/** The document with each paragraph re-laid out and a blank line before each list item. */
function relayout(text: string): string {
  const { lines, trailingNewline } = splitLines(text);
  const blocks = scanBlocks(lines);
  const out: string[] = [];
  for (let index = 0; index < lines.length; index++) {
    const info = blocks.lines[index];
    if (needsBlankBefore(lines, info, index)) out.push("");
    const paragraph = info?.paragraph;
    if (paragraph?.first === index) {
      const source = lines.slice(paragraph.first, paragraph.last + 1);
      const indent = paragraph.continuationIndent;
      for (const logical of joinParagraph(source)) {
        for (const [n, sentence] of cutLine(logical, sentenceBreaks(logical)).entries()) {
          out.push(...wrapLongLine(n === 0 ? sentence : indent + sentence, indent));
        }
      }
      index = paragraph.last;
    } else {
      out.push(lines[index] ?? "");
    }
  }
  return out.join("\n") + (trailingNewline ? "\n" : "");
}

/** Where the two texts differ once whitespace runs are folded to one space, or undefined. */
function foldedDifference(before: string, after: string): string | undefined {
  const a = before.replace(WHITESPACE_RUN, " ");
  const b = after.replace(WHITESPACE_RUN, " ");
  if (a === b) return undefined;
  let at = 0;
  while (a[at] === b[at]) at++;
  const show = (text: string) => JSON.stringify(text.slice(Math.max(0, at - 20), at + 40));
  return `first difference at folded offset ${at}: ${show(a)} became ${show(b)}`;
}

function screenRows(lines: readonly string[]): number {
  return lines.reduce((rows, line) => rows + Math.max(1, Math.ceil(line.length / SCREEN_WIDTH)), 0);
}

function excerpt(line: string, at: number): string {
  const start = Math.max(0, at - 40);
  const end = Math.min(line.length, start + 80);
  return `${start > 0 ? "…" : ""}${line.slice(start, end).trim()}${end < line.length ? "…" : ""}`;
}

function check(text: string): Finding[] {
  const { lines } = splitLines(text);
  const blocks = scanBlocks(lines);
  const findings: Finding[] = [];
  for (const [index, line] of lines.entries()) {
    const info = blocks.lines[index];
    if (info === undefined || info.kind === "fence" || info.kind === "code") continue;
    // R1 reads a paragraph whole, so a code span a line break cuts in two still masks its text.
    // Outside paragraphs it reads only headings and table rows, never lines `--write` keeps as
    // they are for another reason: HTML blocks, link reference definitions, quotes.
    const paragraph = info.paragraph;
    if (paragraph === undefined) {
      const prose = info.kind === "heading" || info.kind === "table";
      for (const space of prose ? sentenceBreaks(line) : []) {
        findings.push({ line: index, rule: 1, message: excerpt(line, space) });
      }
    } else if (paragraph.first === index) {
      const source = lines.slice(paragraph.first, paragraph.last + 1);
      const joined = source.join("\n");
      for (const space of sentenceBreaks(joined)) {
        const offset = joined.slice(0, space).split("\n").length - 1;
        const column = space - (joined.lastIndexOf("\n", space) + 1);
        const message = excerpt(source[offset] ?? "", column);
        findings.push({ line: paragraph.first + offset, rule: 1, message });
      }
    }
    if (needsBlankBefore(lines, info, index)) {
      findings.push({ line: index, rule: 2, message: excerpt(line, 0) });
    }
    if (info.kind === "table") {
      const rows = screenRows([line]);
      if (rows > MAX_SCREEN_ROWS) {
        findings.push({
          line: index,
          rule: 3,
          message: `${rows} screen rows: ${excerpt(line, 0)}`,
        });
      }
    }
  }
  for (const item of blocks.items) {
    const rows = screenRows(lines.slice(item.first, item.last + 1));
    if (rows > MAX_SCREEN_ROWS) {
      const message = `${rows} screen rows: ${excerpt(lines[item.first] ?? "", 0)}`;
      findings.push({ line: item.first, rule: 3, message });
    }
  }
  return findings.sort((a, b) => a.line - b.line || a.rule - b.rule);
}

async function readText(path: string): Promise<string | undefined> {
  try {
    return await readFile(path, "utf8");
  } catch (error) {
    process.stderr.write(`${path}: ${error instanceof Error ? error.message : String(error)}\n`);
    return undefined;
  }
}

async function checkFiles(paths: readonly string[]): Promise<number> {
  let status = 0;
  for (const path of paths) {
    const text = await readText(path);
    if (text === undefined) return 2;
    for (const finding of check(text)) {
      process.stdout.write(`${path}:${finding.line + 1}: R${finding.rule}: ${finding.message}\n`);
      status = 1;
    }
  }
  return status;
}

async function writeFiles(paths: readonly string[]): Promise<number> {
  let status = 0;
  for (const path of paths) {
    const text = await readText(path);
    if (text === undefined) return 2;
    const next = relayout(text);
    const difference = foldedDifference(text, next);
    if (difference !== undefined) {
      process.stderr.write(
        `${path}: refusing to write: the re-layout would change more than whitespace (${difference})\n`
      );
      status = 2;
    } else if (next !== text) {
      await writeFile(path, next);
    }
  }
  return status;
}

// `bun scripts/semantic-line-breaks.ts [--write] <file.md>...` prints `file:line: R<n>: <excerpt>`
// for each finding and exits 1 when there is one; with `--write` it re-lays out each file in place
// and exits 2, writing nothing for that file, when the result would differ in more than whitespace.
if (import.meta.main) {
  const args = process.argv.slice(2);
  const write = args[0] === "--write";
  const paths = write ? args.slice(1) : args;
  if (paths.length === 0 || paths.some((path) => path.startsWith("-"))) {
    process.stderr.write(USAGE);
    process.exit(2);
  }
  process.exit(write ? await writeFiles(paths) : await checkFiles(paths));
}
