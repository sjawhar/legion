// The asks a gated Dispatch call opens for a person, read from the call's arguments as its
// --dry-run prints them: a `dispatch ask`; each ask block in the markdown a `dispatch doc-edit`
// inserts, a `dispatch issue` spec holds, or a `dispatch artifact` upload holds; and each block a
// `dispatch doc-edit` retypes into an ask. A `replace`'s `with` opens no block (`dispatch doc-edit
// --help`), and `dispatch edit-ask`, `comment`, `message` and `request-approval` open no new ask.
//
// The Dispatch server decides which lines of markdown are ask blocks, so the scan reads markdown as
// its parser does: goldmark's block structure (`parseBlocks`, `openBlocks` and the default block
// parsers of github.com/yuin/goldmark v1.8.6, with the tab and list fixes of
// packages/envoy/internal/dispatch/pmdoc/lines.go) and Dispatch's typed blocks
// (packages/envoy/internal/dispatch/pmdoc/directive.go). A `>` prefix, a list item's content
// column and a footnote definition's four columns are taken off a line before an opener, a closer
// or a fence is matched; a fence closes at the end of its container; an opener four columns past
// its container's lines is code or paragraph text; a typed block that opened with N colons closes
// only at a line of N colons or more, which closes it whatever block inside it the line would
// continue; and an opener's attributes are read as the server reads them, so a `#` inside a quoted
// value is no id and an opener whose attributes the server refuses opens nothing. A document's
// front matter is no markdown, and an empty list item or one numbered other than 1 opens no list
// where the browser editor's parser would not let it interrupt the block before it. HTML blocks,
// which the server refuses in a write, are not read. The server's table of inserts and the ask
// blocks it adds (packages/envoy/internal/dispatch/api/testdata/decision-block-inserts.json) holds
// this reading to the server's.

/** A new ask a call opens for a person. */
export interface NewAsk {
  /** A `dispatch ask`, an ask block in markdown the call writes, or a block it retypes into one. */
  kind: "ask" | "block" | "retype";
  /**
   * What the person is asked: a `dispatch ask`'s question, then its options as a bullet list; an
   * ask block's body as written; empty for a retype, whose block's text is in the document.
   */
  text: string;
  /** An ask block's own `#id`, or the block a retype names. */
  id?: string;
  /** The text's first words, which name the ask in a refusal. */
  opening: string;
  /** Set when the ask's text could not be read, so it was judged without it. */
  textMissing?: boolean;
}

/** How many code points of an ask's text name it: as many as `dispatch doc-read --blocks` shows. */
const OPENING_CODE_POINTS = 60;

/**
 * The new asks in a call's arguments. `currentIds` are the block ids the document an upload
 * replaces already holds: a `dispatch artifact` upload skips an ask block with one of them, since a
 * new version of a spec carries its old asks. Every other call ignores them.
 */
export function newAsks(
  toolName: string,
  input: Readonly<Record<string, unknown>>,
  currentIds?: ReadonlySet<string>
): NewAsk[] {
  switch (toolName) {
    case "dispatch_ask":
      return [questionAsk(input)];
    case "dispatch_issue":
      return typeof input.spec === "string" ? askBlocks(input.spec) : [];
    case "dispatch_artifact":
      return typeof input.content === "string"
        ? askBlocks(input.content).filter(
            (ask) => ask.id === undefined || currentIds?.has(ask.id) !== true
          )
        : [];
    case "dispatch_doc_edit":
      return (Array.isArray(input.ops) ? input.ops : []).flatMap((op: unknown): NewAsk[] => {
        if (typeof op !== "object" || op === null) return [];
        const { op: kind, markdown, type, block } = op as Record<string, unknown>;
        // Inserted text reads a closed front-matter block as front matter only where it lands at
        // the document's start (pmdoc.ParseFragment's opensDocument); a "start" anchor is that.
        if (kind === "insert" && typeof markdown === "string") {
          const { before, after } = op as Record<string, unknown>;
          return scanAsks(markdown, before === "start" || after === "start");
        }
        if (kind === "retype" && type === "ask" && typeof block === "string") {
          return [{ kind: "retype", id: block, text: "", opening: "" }];
        }
        return [];
      });
    default:
      return [];
  }
}

/** A `dispatch ask`: its question, then its options as the bullet list an ask block holds them in. */
function questionAsk(input: Readonly<Record<string, unknown>>): NewAsk {
  const question = typeof input.question === "string" ? input.question.trim() : "";
  const options = (Array.isArray(input.options) ? input.options : []).flatMap((option: unknown) => {
    if (typeof option !== "object" || option === null) return [];
    const { label, description } = option as Record<string, unknown>;
    return [
      typeof description === "string" && description !== ""
        ? `- ${String(label ?? "")}: ${description}`
        : `- ${String(label ?? "")}`,
    ];
  });
  return {
    kind: "ask",
    text: options.length === 0 ? question : `${question}\n\n${options.join("\n")}`,
    opening: openingOf(question),
  };
}

/** The first `OPENING_CODE_POINTS` code points of `text` with its whitespace runs made one space. */
function openingOf(text: string): string {
  return Array.from(text.replace(/\s+/g, " ").trim()).slice(0, OPENING_CODE_POINTS).join("");
}

/** A line that opens or closes a document's front matter. */
const FRONT_MATTER_FENCE = /^---[ \t]*$/;

/** Each ask block in `markdown`, in document order, as the Dispatch server reads a document. */
export function askBlocks(markdown: string): NewAsk[] {
  return scanAsks(markdown, true);
}

/** Each ask block in `markdown`, its front matter read first when `frontMatter` is set. */
function scanAsks(markdown: string, frontMatter: boolean): NewAsk[] {
  // A write's CR LF or lone carriage return is stored as a line feed.
  let lines = markdown.replace(/\r\n?/g, "\n").split("\n");
  let unclosedFrontMatter = false;
  // pmdoc's parseFrontmatterBlock: a first line of `---` and spaces or tabs opens front matter,
  // and the first later line that is the same closes it.
  if (frontMatter && FRONT_MATTER_FENCE.test(lines[0] ?? "")) {
    const close = lines.findIndex((line, index) => index > 0 && FRONT_MATTER_FENCE.test(line));
    if (close === -1) unclosedFrontMatter = true;
    else lines = lines.slice(close + 1);
  }
  const scan: Scan = {
    lines,
    index: 0,
    opened: [],
    drafts: [],
    emptyItemWithBlankLines: false,
    skipList: false,
    unclosedFrontMatter,
  };
  const document: Block = {
    kind: "document",
    parent: undefined,
    previous: undefined,
    children: 0,
    last: undefined,
  };
  for (const [index, text] of lines.entries()) {
    scan.index = index;
    readLine(scan, document, new Line(text));
  }
  return scan.drafts.map(({ id, lines: body }) => {
    const text = body.join("\n").trim();
    return { kind: "block", ...(id === undefined ? {} : { id }), text, opening: openingOf(text) };
  });
}

/** An ask block the scan has opened: its id, and the lines of its body as written in it. */
interface Draft {
  readonly id: string | undefined;
  readonly lines: string[];
}

/** What the scan carries from line to line, as goldmark's parser context carries it. */
interface Scan {
  /** The markdown's lines, and the index of the one being read. */
  readonly lines: readonly string[];
  index: number;
  /** The blocks open, outermost first, as `pc.OpenedBlocks()`; the document is not among them. */
  readonly opened: Block[];
  readonly drafts: Draft[];
  /** goldmark's `emptyListItemWithBlankLines`. */
  emptyItemWithBlankLines: boolean;
  /** goldmark's `skipListParserKey`: a list item closed at a sibling's marker. */
  skipList: boolean;
  /**
   * A front-matter opener no later line closes, after which no list, quote or footnote definition
   * opens at the document's level (pmdoc's frontmatterAttempt).
   */
  readonly unclosedFrontMatter: boolean;
}

interface BlockBase {
  readonly parent: Block | undefined;
  /** The block opened before it in its parent (goldmark's `PreviousSibling`). */
  readonly previous: Block | undefined;
  /** How many blocks opened inside this one, and the last of them. */
  children: number;
  last: Block | undefined;
}

/** A block as the scan holds it open. A `leaf` is one that closes at the next line. */
type Block =
  | (BlockBase & { readonly kind: "document" | "quote" | "footnote" | "leaf" })
  | (BlockBase & {
      readonly kind: "paragraph";
      /** Its lines past its containers, and the index of its last. */
      readonly rows: string[];
      lastLine: number;
    })
  | (BlockBase & {
      readonly kind: "code";
      /** The index of its last line that is not blank, and whether a blank line came before it. */
      lastLine: number;
      readonly blankBefore: boolean;
    })
  | (BlockBase & { readonly kind: "list"; readonly marker: string })
  | (BlockBase & {
      readonly kind: "item";
      /** The columns the item's content stands past its container's. */
      readonly offset: number;
    })
  | (BlockBase & {
      readonly kind: "typed";
      /** The colons it opened with: only a line of as many or more closes it. */
      readonly fence: number;
      /** The columns its opening line's text stands past its container's, taken off its lines. */
      readonly indent: number;
      readonly draft: Draft | undefined;
    })
  | (BlockBase & {
      readonly kind: "fence";
      readonly char: string;
      readonly length: number;
    });

/** What a block's continuation does with a line, as goldmark's `State`. */
type Continuation = "close" | "continue" | "children";

/** One line of markdown, read from the column the containers around the block have taken. */
class Line {
  readonly text: string;
  /** The column each character starts at, a tab reaching the next multiple of four, then the end's. */
  readonly #starts: number[];
  #pos = 0;
  #column = 0;

  constructor(text: string) {
    this.text = text;
    const starts = [0];
    let column = 0;
    for (let index = 0; index < text.length; index++) {
      column += text[index] === "\t" ? 4 - (column % 4) : 1;
      starts.push(column);
    }
    this.#starts = starts;
  }

  /** The column taken so far. */
  get column(): number {
    return this.#column;
  }

  /** The column the character at `index` starts at. */
  columnAt(index: number): number {
    return this.#starts[index];
  }

  /** The index of the first character from `index` on that is no space or tab. */
  #afterSpaces(index: number): number {
    let end = index;
    while (end < this.text.length && (this.text[end] === " " || this.text[end] === "\t")) end++;
    return end;
  }

  /** The columns of the spaces and tabs that start at `index`. */
  spacesAt(index: number): number {
    return this.columnAt(this.#afterSpaces(index)) - this.columnAt(index);
  }

  /** The columns of space and tab from the column taken, and the index of the character after them. */
  indent(): { width: number; at: number } {
    const at = this.#afterSpaces(this.#pos);
    return { width: this.columnAt(at) - this.#column, at };
  }

  /** Whether nothing but spaces and tabs is left. */
  blank(): boolean {
    return this.indent().at === this.text.length;
  }

  /** Takes `columns` columns of the spaces and tabs ahead, a tab partly if it spans more. */
  skip(columns: number): void {
    this.#column += columns;
    while (this.#pos < this.text.length && this.columnAt(this.#pos + 1) <= this.#column)
      this.#pos++;
  }

  /** Takes the line up to the character at `index`. */
  advanceTo(index: number): void {
    this.#pos = index;
    this.#column = this.columnAt(index);
  }

  /** What is left of the line, a tab partly taken written as the columns it has left. */
  rest(): string {
    const start = this.columnAt(this.#pos);
    if (this.#pos < this.text.length && start < this.#column) {
      return (
        " ".repeat(this.columnAt(this.#pos + 1) - this.#column) + this.text.slice(this.#pos + 1)
      );
    }
    return this.text.slice(this.#pos);
  }
}

/** One line through the open blocks, then the blocks it opens: goldmark's `parseBlocks` loop body. */
function readLine(scan: Scan, document: Block, line: Line): void {
  if (scan.opened.length === 0) {
    if (!line.blank()) openBlocks(scan, document, line);
    return;
  }
  const opened = [...scan.opened];
  const lastIndex = opened.length - 1;
  for (const [index, block] of opened.entries()) {
    // A paragraph is continued by openBlocks, when the line opens no block that interrupts it.
    if (block.kind !== "paragraph") {
      const continuation = continueBlock(scan, block, line);
      if (continuation === "children" && index === lastIndex) {
        openBlocks(scan, block, line);
        return;
      }
      if (continuation !== "close") continue;
    }
    // The block closes here, or the line continues the paragraph last opened lazily.
    if (openBlocks(scan, opened[index - 1] ?? document, line) !== "paragraph") {
      scan.opened.splice(index, lastIndex - index + 1);
    }
    return;
  }
}

/** goldmark's `openBlocks`: the blocks `line` opens inside `parent`, else a paragraph's continuation. */
function openBlocks(scan: Scan, parent: Block, line: Line): "opened" | "paragraph" | "none" {
  const continuable = scan.opened.at(-1)?.kind === "paragraph";
  let result: "opened" | "none" = "none";
  for (let into = parent; ; ) {
    const { width, at } = line.indent();
    if (at === line.text.length) break;
    const char = line.text.charAt(at);
    const interrupting = continuable && result === "none";
    let opened: { block: Block; children: boolean } | undefined;
    for (const parser of BLOCK_PARSERS) {
      if (parser.triggers !== undefined && !parser.triggers.includes(char)) continue;
      if (interrupting && !parser.interrupts) continue;
      if (width > 3 && !parser.indented) continue;
      opened = parser.open(scan, into, line, width, at);
      if (opened !== undefined) break;
    }
    if (opened === undefined) break;
    into.children++;
    into.last = opened.block;
    scan.opened.push(opened.block);
    result = "opened";
    if (!opened.children) break;
    into = opened.block;
  }
  const last = scan.opened.at(-1);
  if (result === "none" && last?.kind === "paragraph" && !line.blank()) {
    last.rows.push(line.rest());
    last.lastLine = scan.index;
    line.advanceTo(line.text.length);
    return "paragraph";
  }
  return result;
}

/** A block parser: goldmark's `BlockParser` as the scan needs it. */
interface BlockParser {
  /** The characters a line's text starts with for this parser to try it; any, when undefined. */
  readonly triggers?: string;
  /** Whether the block may open where it would interrupt a paragraph (`CanInterruptParagraph`). */
  readonly interrupts: boolean;
  /** Whether the block may open four columns or more into its container (`CanAcceptIndentedLine`). */
  readonly indented: boolean;
  open(
    scan: Scan,
    parent: Block,
    line: Line,
    width: number,
    at: number
  ): { block: Block; children: boolean } | undefined;
}

/** A block's place in the tree, empty, inside `parent`. */
function under(parent: Block): BlockBase {
  return { parent, previous: parent.last, children: 0, last: undefined };
}

/** A block that holds nothing and closes at the next line: a heading, a rule, a refused directive. */
function leafAt(parent: Block, line: Line): { block: Block; children: boolean } {
  line.advanceTo(line.text.length);
  return { block: { kind: "leaf", ...under(parent) }, children: false };
}

/**
 * Whether an unclosed front-matter opener keeps a list, quote or footnote definition from opening
 * in `parent`: the browser editor's parser reads the whole document as front matter, where no
 * container opens at the document's level (pmdoc's frontmatterAttempt).
 */
function frontMatterHolds(scan: Scan, parent: Block): boolean {
  return scan.unclosedFrontMatter && parent.kind === "document";
}

// goldmark's default block parsers and Dispatch's own, in goldmark's order of priority: setext
// heading (100), thematic break (200), list (300), list item (400), indented code (500), ATX
// heading (600), fenced code (700), blockquote (800), Dispatch's refused directive (900) and typed
// block (950), footnote definition (999), paragraph (1000).
const BLOCK_PARSERS: readonly BlockParser[] = [
  {
    triggers: "-=",
    interrupts: true,
    indented: false,
    open(scan, parent, line, _width, at) {
      const last = scan.opened.at(-1);
      if (last?.kind !== "paragraph" || last.parent !== parent) return undefined;
      return setextUnderline(line.text.slice(at)) ? leafAt(parent, line) : undefined;
    },
  },
  {
    triggers: "-*_",
    interrupts: true,
    indented: false,
    open: (_scan, parent, line, _width, at) =>
      thematicBreak(line.text.slice(at)) ? leafAt(parent, line) : undefined,
  },
  {
    triggers: "-+*0123456789",
    interrupts: true,
    indented: false,
    open(scan, parent, line) {
      const last = scan.opened.at(-1);
      if (last?.kind === "list" || scan.skipList) {
        scan.skipList = false;
        return undefined;
      }
      const marker = listMarker(line);
      if (marker === undefined || frontMatterHolds(scan, parent)) return undefined;
      // An empty item, or one numbered other than a lone `1`, opens no list where it would
      // interrupt a paragraph or an indented code block (pmdoc's emptyItemGuard, stricter than
      // goldmark, which lets `01.` interrupt a paragraph and anything interrupt code).
      if (marker.empty || (marker.number !== undefined && marker.number !== "1")) {
        if (last?.kind === "paragraph" && last.parent === parent) return undefined;
        if (interruptsOpenBlock(scan, parent)) return undefined;
      }
      scan.emptyItemWithBlankLines = false;
      return { block: { kind: "list", marker: marker.char, ...under(parent) }, children: true };
    },
  },
  {
    triggers: "-+*0123456789",
    interrupts: true,
    indented: false,
    open(scan, parent, line) {
      if (parent.kind !== "list") return undefined;
      const previous = parent.last?.kind === "item" ? parent.last.offset : 0;
      const marker = listMarker(line);
      if (marker === undefined || marker.indent - previous > 3) return undefined;
      scan.emptyItemWithBlankLines = false;
      // The content's columns past the marker; one, when the item opens with indented code.
      const after = line.spacesAt(marker.end);
      const spacing = marker.empty || after > 4 ? 1 : after;
      const offset = line.columnAt(marker.end) - line.column + spacing;
      const item: Block = { kind: "item", offset, ...under(parent) };
      if (marker.empty) return { block: item, children: false };
      line.advanceTo(marker.end);
      line.skip(spacing);
      return { block: item, children: true };
    },
  },
  {
    interrupts: false,
    indented: true,
    open(scan, parent, line, width) {
      if (width < 4) return undefined;
      line.skip(4);
      const previous = scan.lines[scan.index - 1];
      const block: Block = {
        kind: "code",
        lastLine: scan.index,
        blankBefore: previous === undefined || /^[ \t]*$/.test(previous),
        ...under(parent),
      };
      return { block, children: false };
    },
  },
  {
    triggers: "#",
    interrupts: true,
    indented: false,
    open: (_scan, parent, line, _width, at) =>
      /^#{1,6}(?:[ \t]|$)/.test(line.text.slice(at)) ? leafAt(parent, line) : undefined,
  },
  {
    triggers: "`~",
    interrupts: true,
    indented: false,
    open(_scan, parent, line, _width, at) {
      const char = line.text.charAt(at);
      const length = runOf(line.text, at, char);
      if (length < 3) return undefined;
      // A backtick fence's info string holds no backtick: such a line is inline code.
      if (char === "`" && line.text.slice(at + length).includes("`")) return undefined;
      line.advanceTo(line.text.length);
      return { block: { kind: "fence", char, length, ...under(parent) }, children: false };
    },
  },
  {
    triggers: ">",
    interrupts: true,
    indented: false,
    open: (scan, parent, line) =>
      !frontMatterHolds(scan, parent) && quoteMarker(line)
        ? { block: { kind: "quote", ...under(parent) }, children: true }
        : undefined,
  },
  {
    triggers: ":",
    interrupts: true,
    indented: false,
    open: (_scan, parent, line, _width, at) =>
      refusedDirective(line.text.slice(at)) ? leafAt(parent, line) : undefined,
  },
  {
    triggers: ":",
    interrupts: true,
    indented: false,
    open(scan, parent, line, width, at) {
      const opener = typedOpener(line.text.slice(at));
      if (opener === undefined) return undefined;
      let draft: Draft | undefined;
      if (opener.name === "ask") {
        draft = { id: opener.id, lines: [] };
        scan.drafts.push(draft);
      }
      line.advanceTo(line.text.length);
      const block: Block = {
        kind: "typed",
        fence: opener.colons,
        indent: width,
        draft,
        ...under(parent),
      };
      return { block, children: true };
    },
  },
  {
    triggers: "[",
    interrupts: true,
    indented: false,
    open(scan, parent, line, _width, at) {
      const definition = /^\[\^([^\]]*)\]:/.exec(line.text.slice(at));
      if (definition === null || /^\s*$/.test(definition[1] ?? "")) return undefined;
      if (frontMatterHolds(scan, parent)) return undefined;
      line.advanceTo(at + definition[0].length);
      return { block: { kind: "footnote", ...under(parent) }, children: true };
    },
  },
  {
    interrupts: false,
    indented: false,
    open: (scan, parent, line) => {
      const block: Block = {
        kind: "paragraph",
        rows: [line.rest()],
        lastLine: scan.index,
        ...under(parent),
      };
      line.advanceTo(line.text.length);
      return { block, children: false };
    },
  },
];

/**
 * pmdoc's interruptsOpenBlock: whether a list opening in `parent` interrupts a block the browser
 * editor's parser holds open there, other than a paragraph `parent` holds: a paragraph that ends on
 * the line before, reached across the containers the line opens first and forming no table, or an
 * indented code block with only blank lines after it, unless that code stands right after a list,
 * or right after a quote with no blank line between them.
 */
function interruptsOpenBlock(scan: Scan, parent: Block): boolean {
  let previous = parent.last;
  const opensContainers = previous === undefined;
  let node: Block | undefined = parent;
  while (previous === undefined && node !== undefined && node.kind !== "document") {
    previous = node.previous;
    node = node.parent;
  }
  if (previous?.kind === "paragraph") {
    return (
      opensContainers &&
      !previous.rows.slice(1).some((row) => TABLE_DELIMITER_ROW.test(row)) &&
      scan.index - previous.lastLine <= 1
    );
  }
  if (previous?.kind !== "code") return false;
  if (previous.previous?.kind === "list") return false;
  if (previous.previous?.kind === "quote" && !previous.blankBefore) return false;
  return scan.lines.slice(previous.lastLine + 1, scan.index).every((row) => /^[ \t>]*$/.test(row));
}

/** A table's delimiter row, which makes the paragraph above it a table (GFM's tables). */
const TABLE_DELIMITER_ROW =
  /^(?=.*\|)[ \t]*\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)*\|?[ \t]*$/;

/** goldmark's `Continue` of each block the scan holds open. */
function continueBlock(scan: Scan, block: Block, line: Line): Continuation {
  switch (block.kind) {
    case "quote":
      return quoteMarker(line) ? "children" : "close";
    case "list":
      return continueList(scan, block, line);
    case "item":
      return continueItem(scan, block, line);
    case "footnote": {
      if (line.blank()) return "children";
      const { width } = line.indent();
      if (width < 4) return "close";
      line.skip(4);
      return "children";
    }
    case "typed": {
      const { width, at } = line.indent();
      // A line of at least the fence's colons closes the block, whatever block inside it the line
      // would otherwise continue (directive.go, typedDirectiveParser.Continue).
      if (width < 4 && at < line.text.length && fenceColons(line.text.slice(at)) >= block.fence) {
        line.advanceTo(line.text.length);
        return "close";
      }
      if (at < line.text.length) line.skip(Math.min(width, block.indent));
      block.draft?.lines.push(line.rest());
      return "children";
    }
    case "fence": {
      const { width, at } = line.indent();
      if (width < 4) {
        const length = runOf(line.text, at, block.char);
        if (length >= block.length && /^[ \t]*$/.test(line.text.slice(at + length))) {
          line.advanceTo(line.text.length);
          return "close";
        }
      }
      line.advanceTo(line.text.length);
      return "continue";
    }
    case "code": {
      if (line.blank()) return "continue";
      if (line.indent().width < 4) return "close";
      block.lastLine = scan.index;
      line.advanceTo(line.text.length);
      return "continue";
    }
    default:
      // A heading, a rule or a refused directive holds one line; the document never closes.
      return block.kind === "document" ? "children" : "close";
  }
}

/** goldmark's `listParser.Continue`. */
function continueList(scan: Scan, list: Block & { kind: "list" }, line: Line): Continuation {
  const last = list.last;
  const lastIsEmpty = last === undefined || last.children === 0;
  if (line.blank()) {
    if (lastIsEmpty) scan.emptyItemWithBlankLines = true;
    return "children";
  }
  const offset = last?.kind === "item" ? last.offset : 0;
  const { width, at } = line.indent();
  if (width < offset || lastIsEmpty) {
    if (width < 4) {
      const marker = listMarker(line);
      if (marker !== undefined && marker.indent - offset < 4) {
        if (marker.char !== list.marker) return "close";
        // A thematic break outranks a list item, unless it underlines a paragraph as a heading.
        const fromMarker = line.text.slice(at);
        const heading = scan.opened.at(-1)?.kind === "paragraph" && setextUnderline(fromMarker);
        if (thematicBreak(fromMarker) && !heading) return "close";
        return "children";
      }
    }
    if (!lastIsEmpty) return "close";
  }
  if (lastIsEmpty && width < offset) return "close";
  // Content cannot follow an empty item and blank lines in the same list.
  if (scan.emptyItemWithBlankLines) return "close";
  return "children";
}

/** goldmark's `listItemParser.Continue`, a blank line taking no more than the item's columns. */
function continueItem(scan: Scan, item: Block & { kind: "item" }, line: Line): Continuation {
  const { width } = line.indent();
  if (line.blank()) {
    line.skip(Math.min(width, item.offset));
    return "children";
  }
  const isEmpty = item.children === 0 && scan.emptyItemWithBlankLines;
  if ((isEmpty || width < item.offset) && width < 4) {
    if (listMarker(line) !== undefined) {
      scan.skipList = true;
      return "close";
    }
    if (!isEmpty) return "close";
  }
  line.skip(Math.min(width, item.offset));
  return "children";
}

/** A list item's marker opening what is left of a line. */
interface ListMarker {
  /** The columns of indentation before it. */
  readonly indent: number;
  /** The index after it. */
  readonly end: number;
  /** The character a list continues only with: the bullet, or the ordered delimiter. */
  readonly char: string;
  /** An ordered marker's number as written. */
  readonly number: string | undefined;
  /** Whether nothing but spaces and tabs follows it. */
  readonly empty: boolean;
}

/** goldmark's `parseListItem`, indentation measured in the columns its tabs span (pmdoc's tabIndented). */
function listMarker(line: Line): ListMarker | undefined {
  const { width, at } = line.indent();
  if (width > 3) return undefined;
  const { text } = line;
  const first = text.charAt(at);
  let end = at;
  let number: string | undefined;
  if (first === "-" || first === "*" || first === "+") {
    end = at + 1;
  } else {
    while (end < text.length && end - at <= 9 && /[0-9]/.test(text.charAt(end))) end++;
    const delimiter = text.charAt(end);
    if (end === at || end - at > 9 || (delimiter !== "." && delimiter !== ")")) return undefined;
    number = text.slice(at, end);
    end++;
  }
  const after = text.charAt(end);
  if (end < text.length && after !== " " && after !== "\t") return undefined;
  return {
    indent: width,
    end,
    char: text.charAt(end - 1),
    number,
    empty: /^[ \t]*$/.test(text.slice(end)),
  };
}

/** goldmark's blockquote `process`: a `>`, then one column of the space or tab after it. */
function quoteMarker(line: Line): boolean {
  const { width, at } = line.indent();
  if (width > 3 || line.text.charAt(at) !== ">") return false;
  line.advanceTo(at + 1);
  const next = line.text.charAt(at + 1);
  if (next === " " || next === "\t") line.skip(1);
  return true;
}

/** goldmark's `isThematicBreak`: three or more of one of `*`, `-`, `_`, with spaces between. */
function thematicBreak(text: string): boolean {
  const marks = text.replace(/[ \t]/g, "");
  return marks.length >= 3 && /^(?:\*+|-+|_+)$/.test(marks);
}

/** goldmark's `matchesSetextHeadingBar`: a run of `=` or of `-`, then spaces. */
function setextUnderline(text: string): boolean {
  return /^(?:=+|-+)[ \t]*$/.test(text);
}

/** The number of `char` in a row from `index`. */
function runOf(text: string, index: number, char: string): number {
  let end = index;
  while (text.charAt(end) === char) end++;
  return end - index;
}

/** directive.go's `fenceColons`: the colons `text` holds when they are all it holds but spaces. */
function fenceColons(text: string): number {
  const trimmed = text.replace(/^[ \t]+|[ \t]+$/g, "");
  return /^:+$/.test(trimmed) ? trimmed.length : 0;
}

/** directive.go's `directiveNameByte`. */
function nameChar(char: string): boolean {
  return /^[A-Za-z0-9_-]$/.test(char);
}

/**
 * directive.go's `parseTypedDirectiveOpen`: three colons or more, a name, then `{` at once, and
 * the line ends with `}`, the attributes between them read as the server reads them.
 */
function typedOpener(
  text: string
): { name: string; id: string | undefined; colons: number } | undefined {
  const colons = runOf(text, 0, ":");
  if (colons < 3) return undefined;
  const rest = text.slice(colons);
  let nameEnd = 0;
  while (nameEnd < rest.length && nameChar(rest.charAt(nameEnd))) nameEnd++;
  if (nameEnd === 0 || rest.charAt(nameEnd) !== "{" || !rest.endsWith("}")) return undefined;
  const attributes = directiveAttributes(rest.slice(nameEnd + 1, -1));
  if (attributes === undefined) return undefined;
  return { name: rest.slice(0, nameEnd), id: attributes.id, colons };
}

/** directive.go's `unsupportedDirectiveReason`: a line of directive syntax the server refuses. */
function refusedDirective(text: string): boolean {
  if (text.startsWith(":::")) return text !== ":::" && typedOpener(text) === undefined;
  if (text.startsWith("::")) return text.length > 2 && nameChar(text.charAt(2));
  return text.length > 1 && nameChar(text.charAt(1)) && text.includes("{");
}

/** The whitespace Go's `unicode.IsSpace` finds in a byte of the attributes: ASCII's. */
function attributeSpace(char: string): boolean {
  return char === " " || (char >= "\t" && char <= "\r");
}

/** The key that holds a block's id among a directive's attributes (pmdoc's `BlockIDAttr`). */
const BLOCK_ID_ATTRIBUTE = "blockId";

/**
 * directive.go's `parseDirectiveAttributes`, for the block's id alone: `#id` or `blockId="…"`,
 * undefined when the server refuses the attributes. A quoted id holding an escape or a character
 * reference is not decoded, and reads as no id.
 */
function directiveAttributes(value: string): { id: string | undefined } | undefined {
  const named = new Set<string>();
  let id: string | undefined;
  let offset = 0;
  const name = () => {
    const start = offset;
    while (offset < value.length && nameChar(value.charAt(offset))) offset++;
    return value.slice(start, offset);
  };
  while (offset < value.length) {
    while (offset < value.length && attributeSpace(value.charAt(offset))) offset++;
    if (offset === value.length) break;
    const sigil = value.charAt(offset);
    if (sigil === "#") {
      offset++;
      const read = name();
      if (read === "" || named.has(BLOCK_ID_ATTRIBUTE)) return undefined;
      named.add(BLOCK_ID_ATTRIBUTE);
      id = read;
    } else if (sigil === ".") {
      offset++;
      if (name() === "") return undefined;
      named.add("class");
    } else {
      const key = name();
      if (key === "" || named.has(key)) return undefined;
      named.add(key);
      // A name alone is an empty value; the browser editor reads a name up to a space.
      if (offset === value.length || value.charAt(offset) === " ") continue;
      if (value.charAt(offset) !== "=" || value.charAt(offset + 1) !== '"') return undefined;
      offset += 2;
      const start = offset;
      let escaped = false;
      while (offset < value.length && (value.charAt(offset) !== '"' || escaped)) {
        escaped = value.charAt(offset) === "\\" && !escaped;
        offset++;
      }
      if (offset === value.length) return undefined;
      const quoted = value.slice(start, offset);
      if (!goQuoted(quoted)) return undefined;
      if (key === BLOCK_ID_ATTRIBUTE) id = /[\\&]/.test(quoted) ? undefined : quoted;
      offset++;
    }
    if (offset < value.length && !attributeSpace(value.charAt(offset))) return undefined;
  }
  return { id };
}

/** Whether Go's `strconv.Unquote` reads `body` between double quotes: its escapes, and no line feed. */
function goQuoted(body: string): boolean {
  return /^(?:[^\\\n"]|\\(?:[abfnrtv\\"]|x[0-9A-Fa-f]{2}|[0-3][0-7]{2}|u[0-9A-Fa-f]{4}|U[0-9A-Fa-f]{8}))*$/.test(
    body
  );
}
