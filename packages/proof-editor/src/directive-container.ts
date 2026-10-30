/**
 * The `:::name[label]{attributes}` container directive as a micromark document construct: a
 * container, as micromark's block quote and list item are (INFRA-581).
 *
 * micromark-extension-directive's own container is a flow construct whose content is a
 * sub-document, so the content of a block nested D deep was tokenized D + 1 times, and a body
 * nesting `:::` a few thousand deep froze a reader's tab for about a minute. Here the document
 * tokenizer holds each open block on its container stack and runs its continuation once per line,
 * outermost block first, so the content is tokenized once. The continuation is a closing fence when
 * the line holds, after at most three columns of indentation, a run of at least the opening count
 * of colons and nothing but whitespace: it consumes the fence and closes the block and everything
 * inside it, so a `:::` line closes the outermost block it can close. Any other line keeps the
 * block open, less up to the opener's own indentation.
 *
 * The opening fence is tokenized as the package's container tokenizes it, into the same token
 * types (`directiveContainer` and its fence, sequence, name, label and attribute tokens), so
 * `mdast-util-directive` builds the same `containerDirective` nodes from them.
 *
 * Each open block still costs one continuation on every line inside it, and micromark copies its
 * token stack in every attempt, so a line D blocks deep costs O(D²). Blocks nest at most
 * `maxDirectiveNesting` deep: an opener standing inside that many open directive containers does
 * not open, and its line is read as the text it is.
 *
 * A document reads as it did under the flow construct: `resolveDirectiveContainers` puts back the
 * line endings and container exits the flow construct emitted differently, `lazyContentEnd` ends a
 * block at a lazy line as it did and a closed block at the line after its fence, and a closing
 * fence over an open block quote, or an opener on a line some container did not reach, marks its
 * line lazy.
 * Three readings differ, all of them micromark's order of work rather than the block's grammar: a
 * document construct cannot open after an unclosed front matter opener, which micromark tries to
 * the end of the document while it opens no container; a footnote definition inside a block now
 * defines its footnote for a call in the text before the block; and a link reference definition
 * inside a block now defines its reference for a reference in the text before the block. The flow
 * construct, by tokenizing the block's content after that text, did neither: `[r]`, a blank line,
 * then `:::callout{}`, `[r]: https://u.example` and `:::` read `[r]` as text there and as a link
 * here. The document service refuses a link reference definition, so no document it stores reads
 * differently; a body that is only displayed can.
 */

import { factorySpace } from "micromark-factory-space";
import { markdownLineEnding, markdownSpace } from "micromark-util-character";
import type {
  Code,
  Construct,
  ContainerState,
  Effects,
  Event,
  Extension,
  ParseContext,
  State,
  Token,
  TokenizeContext,
} from "micromark-util-types";
import { openAround as blocksAround, maxBlockNesting, recordContinued } from "./block-nesting.js";
import { factoryAttributes, factoryLabel, factoryName } from "./directive-factories.js";

declare module "micromark-util-types" {
  interface TokenTypeMap {
    directiveContainer: "directiveContainer";
    directiveContainerAttributes: "directiveContainerAttributes";
    directiveContainerAttributesMarker: "directiveContainerAttributesMarker";
    directiveContainerAttribute: "directiveContainerAttribute";
    directiveContainerAttributeId: "directiveContainerAttributeId";
    directiveContainerAttributeIdValue: "directiveContainerAttributeIdValue";
    directiveContainerAttributeClass: "directiveContainerAttributeClass";
    directiveContainerAttributeClassValue: "directiveContainerAttributeClassValue";
    directiveContainerAttributeName: "directiveContainerAttributeName";
    directiveContainerAttributeInitializerMarker: "directiveContainerAttributeInitializerMarker";
    directiveContainerAttributeValueLiteral: "directiveContainerAttributeValueLiteral";
    directiveContainerAttributeValue: "directiveContainerAttributeValue";
    directiveContainerAttributeValueMarker: "directiveContainerAttributeValueMarker";
    directiveContainerAttributeValueData: "directiveContainerAttributeValueData";
    directiveContainerFence: "directiveContainerFence";
    directiveContainerLabel: "directiveContainerLabel";
    directiveContainerLabelMarker: "directiveContainerLabelMarker";
    directiveContainerLabelString: "directiveContainerLabelString";
    directiveContainerName: "directiveContainerName";
    directiveContainerSequence: "directiveContainerSequence";
  }

  interface ContainerState {
    /** The open directive container this state is the document tokenizer's state of. */
    directive?: DirectiveBlock | undefined;
  }

  interface Token {
    /** The directive container a `directiveContainer` token opens. */
    _directive?: DirectiveBlock | undefined;
  }
}

/** One directive container, as its start and continuation found it. */
interface DirectiveBlock {
  /** How many colons its opening fence holds. */
  fenceSize: number;
  /** How many columns its opening fence is indented. */
  indent: number;
  /** How many directive containers stand around it, itself included. */
  depth: number;
  /**
   * How many blocks the document service counts around its content, itself included: quotes,
   * lists, list items, typed blocks and footnote definitions (`maxBlockNesting`,
   * ./block-nesting.ts).
   */
  blocks: number;
  /** The line its opening fence is on. */
  line: number;
  /** Whether a closing fence has closed it. */
  closed: boolean;
  /**
   * By line, counted from `line`, the offset at which its content starts on each line its
   * continuation passed without closing it: after the prefixes of the containers around it and its
   * own indentation.
   */
  contentStarts: number[];
}

/**
 * How many directive containers may stand inside one another. An opener standing inside this many
 * open directive containers does not open; the document service refuses a typed block nested that
 * deep.
 */
export const maxDirectiveNesting = 32;

/** Per parse, what its document tokenizer has found of the directive containers around its lines. */
interface ParseRecord {
  /** The line `depth` was last recorded on. */
  line: number;
  /**
   * How many directive containers are open around `line`: the depth of the innermost block whose
   * continuation the line passed. The document tokenizer runs continuations outermost first and
   * stops at the first that fails, so the last one run on a line is the innermost directive
   * container still open around it. A start only reads this, so a start that micromark runs inside
   * `effects.check` and throws away cannot change it.
   */
  depth: number;
  /** The depth of the innermost directive container that is open and not closed. */
  open: number;
  /**
   * The line on which the continuation of a closed directive container, still on the container
   * stack, last ran: every container around it went on there. It is 0 once that block exits.
   */
  reachedClosed: number;
}

const parses = new WeakMap<ParseContext, ParseRecord>();

function parseRecord(context: TokenizeContext): ParseRecord {
  let record = parses.get(context.parser);
  if (record === undefined) {
    record = { line: 0, depth: 0, open: 0, reachedClosed: 0 };
    parses.set(context.parser, record);
  }
  return record;
}

function recordOpenAround(context: TokenizeContext, depth: number): void {
  const record = parseRecord(context);
  record.line = context.now().line;
  record.depth = depth;
}

/** How many directive containers are open around the current line; it never creates a record. */
function openAround(context: TokenizeContext): number {
  const record = parses.get(context.parser);
  return record !== undefined && record.line === context.now().line ? record.depth : 0;
}

const directiveContainer: Construct = {
  name: "directiveContainer",
  tokenize: tokenizeDirectiveContainerStart,
  continuation: { tokenize: tokenizeDirectiveContainerContinuation },
  exit: exitDirectiveContainer,
  resolveAll: resolveDirectiveContainers,
};

/**
 * Ends the directive containers a line does not reach, the way the flow construct ended at a lazy
 * line: its content was a document of its own, never continued lazily, so a paragraph in it does
 * not take the line.
 *
 * The document tokenizer tries it as a container start on a line where some container did not go
 * on, inside `effects.check`. It succeeds when the innermost open directive container is deeper
 * than the innermost one the line reached, so the document tokenizer closes the flow and exits the
 * containers the line did not reach; tried again for real after those exits, it fails, so it opens
 * nothing. It marks the line lazy, as it was for the flow construct: a fenced code block then ends
 * before the last line ending, and `resolveContentExits` puts the exits where a lazy line's go. A
 * blank line cannot be taken lazily, so it is left to micromark, which also keeps this from
 * matching the end of a line on which a directive container just opened.
 *
 * A closed directive container stays on the container stack until the next line, whose
 * continuation fails on it. When every container around it went on there, the line is no lazy
 * line: the flow construct's block had ended with its fence, so this succeeds without marking it,
 * and the document tokenizer exits the closed block before it reads the line, as the line after a
 * block that has ended.
 */
const lazyContentEnd: Construct = {
  name: "directiveLazyContentEnd",
  tokenize: tokenizeLazyContentEnd,
};

/**
 * `:::name[label]{attributes}` container blocks, as a micromark extension. `lazyContentEnd` applies
 * to a line whatever it starts with, so it is registered for every code.
 */
export const directiveContainerSyntax: Extension = {
  document: { 58: [directiveContainer], null: [lazyContentEnd] },
};

function tokenizeLazyContentEnd(
  this: TokenizeContext,
  _effects: Effects,
  ok: State,
  nok: State
): State {
  const self = this;
  return start;

  function start(code: Code): State | undefined {
    const record = parses.get(self.parser);
    if (record === undefined || markdownLineEnding(code)) return nok(code);
    if (record.reachedClosed === self.now().line) return ok(code);
    if (record.open <= openAround(self)) return nok(code);
    self.parser.lazy[self.now().line] = true;
    return ok(code);
  }
}

/**
 * Puts back what micromark-extension-directive's flow construct read where a document construct
 * reads differently. It runs once the document is tokenized: every flow tokenizer has finished,
 * and its events are not yet merged into the document's (`subtokenize` does that after).
 */
function resolveDirectiveContainers(events: Event[], context: TokenizeContext): Event[] {
  resolveFenceLineEndings(events);
  resolveContentExits(events, context);
  return events;
}

/**
 * The line ending of a fence's line is a `lineEnding`, as the flow construct read it, not a
 * `lineEndingBlank`.
 *
 * A fence is document syntax here, so what is left of its line, only its line ending, reaches the
 * flow tokenizer as a line of its own, which flow reads as a blank line. The flow construct
 * consumed that line ending as part of the block, and `mdast-util-from-markdown` counts a blank
 * line inside a list item toward the list's `spread`, so a fence in a list item, or a block that
 * holds a list, would make a tight list loose. Each flow's first `chunkFlow` (the one with no
 * `previous`) names the tokenizer that read all of that flow's lines.
 */
function resolveFenceLineEndings(events: Event[]): void {
  const fenceEnds = new Set<number>();
  for (const [kind, token] of events) {
    if (kind === "exit" && token.type === "directiveContainerFence")
      fenceEnds.add(token.end.offset);
  }
  for (const [kind, token] of events) {
    if (kind !== "enter" || token._tokenizer === undefined || token.previous !== undefined)
      continue;
    for (const [flowKind, flowToken] of token._tokenizer.events) {
      if (
        flowKind === "enter" &&
        flowToken.type === "lineEndingBlank" &&
        fenceEnds.has(flowToken.start.offset)
      ) {
        flowToken.type = "lineEnding";
      }
    }
  }
}

/**
 * Puts container exits where the flow construct put them, so that the line endings between them
 * fall inside the same containers they did.
 *
 * That decides how a document reads: `subtokenize` moves a container's exit back over the line
 * endings, `linePrefix` and `listItemIndent` before it, but over nothing else (a
 * `blockQuotePrefix` stops it), and `mdast-util-from-markdown` counts a blank line inside a list
 * toward its `spread`, while a fenced code block takes the line endings inside it as code. The
 * flow construct's content was a document of its own, made of the rest of each line after the
 * prefixes of the containers around the block and its own indentation, and `subtokenize` put what
 * that document emitted between one line's content and the next after the next line's prefixes.
 * Here every container is on one stack, so this walks the events once, and at each run of exits
 * one rule below says where each of them goes: `exitsOfAClosedDirectiveContainer`,
 * `exitsOfAnUnclosedDirectiveContainer`, `exitsOfAContainerEndedByALine` or `exitsOfALazyLine`,
 * each carrying the end it reads as its own doc comment. `placeExits` then moves them, and
 * `settleExits` leaves a directive container's own exit where it is put.
 *
 * An exit moves over prefixes and content only, within a line or two, and never past another
 * container's event, so this stays linear in the events.
 */
function resolveContentExits(events: Event[], context: TokenizeContext): void {
  // The open containers, outermost first, and the position in `open` of the innermost directive
  // container around each, or -1.
  const open: Token[] = [];
  const owners: number[] = [];
  // The index of the last `chunkFlow` exit: the end of the last line's content.
  let lastChunk = -1;
  let index = 0;
  while (index < events.length) {
    const [kind, token] = events[index];
    if (token.type === "chunkFlow" && kind === "exit") lastChunk = index;
    if (token._container !== true) {
      index++;
      continue;
    }
    if (kind === "enter") {
      const below = open.length - 1;
      owners.push(below < 0 ? -1 : open[below]._directive !== undefined ? below : owners[below]);
      open.push(token);
      index++;
      continue;
    }

    // A run of exits, innermost first. Where each goes is a gap: before the event at that index.
    let end = index;
    while (end < events.length && events[end][0] === "exit" && events[end][1]._container === true) {
      end++;
    }
    const remaining = open.length - (end - index);
    const chunk = lastChunk === -1 ? undefined : events[lastChunk][1];
    const run: ExitRun = {
      events,
      context,
      index,
      end,
      lastChunk,
      chunk,
      // Whether the last content is only a line ending. The stream is read here, once for the
      // run: the rule that reads it runs once per exit of the run, and a run ending several
      // nested containers would otherwise slice the same chunk again for each of them.
      bareChunk:
        chunk !== undefined &&
        chunk.end.offset - chunk.start.offset <= 2 &&
        context.sliceStream(chunk).length === 1,
      // The line after the last content, when that content ends in a line ending.
      nextLine: chunk !== undefined && chunk.end.line !== chunk.start.line ? chunk.end.line : -1,
      lazyLine: false,
    };
    // Where a lazy line's exits go, and whether the line after the last content is one: the rules
    // below all read that.
    const lazyLine = exitsOfALazyLine(run);
    run.lazyLine = lazyLine.lazy;
    const gaps: number[] = new Array(end - index);
    // The directive containers whose exit goes exactly where it is put: `subtokenize` must not move
    // it back over line endings, as nothing moved the flow construct's own exit.
    const settled: Token[] = [];
    // Outermost first, so an inner exit never goes after an outer one.
    for (let at = end - 1; at >= index; at--) {
      const position = open.length - 1 - (at - index);
      const exiting = open[position];
      const own = exiting._directive;
      const owner = owners[position];
      const block = own ?? (owner === -1 ? undefined : open[owner]._directive);
      // Whether the exit is a directive container's own, or one inside a directive container that
      // exits in this run too.
      const blockEnds = block !== undefined && (own !== undefined || owner >= remaining);
      let placement: ExitPlacement = { gap: end, settled: false };
      if (blockEnds && block.closed) {
        placement = exitsOfAClosedDirectiveContainer(run);
      } else if (blockEnds) {
        placement = exitsOfAnUnclosedDirectiveContainer(run, block, own);
      } else if (
        block !== undefined &&
        run.nextLine !== -1 &&
        (lastChunk === index - 1 || run.lazyLine)
      ) {
        placement = exitsOfAContainerEndedByALine(run, block);
      } else if (block === undefined && run.lazyLine) {
        placement = lazyLine;
      }
      if (placement.settled) settled.push(exiting);
      let gap = placement.gap;
      if (gap >= index && gap <= end) gap = end;
      if (at < end - 1) gap = Math.min(gap, gaps[at + 1 - index]);
      gaps[at - index] = gap;
    }

    const moved = placeExits(run, gaps, settled);
    lastChunk = moved.lastChunk;
    open.length = remaining;
    owners.length = remaining;
    index = moved.windowEnd;
  }
}

/** One run of container exits, innermost first, and the line after the content before it. */
interface ExitRun {
  events: Event[];
  context: TokenizeContext;
  /** The first exit of the run. */
  index: number;
  /** The gap after its last exit. */
  end: number;
  /** The index of the last `chunkFlow` exit: the end of the last line's content. */
  lastChunk: number;
  /** That content, or undefined where no line has ended yet. */
  chunk: Token | undefined;
  /** Whether that content is only a line ending. */
  bareChunk: boolean;
  /** The line after it, when it ends in a line ending, or -1. */
  nextLine: number;
  /** Whether that line was lazy for the flow construct (`exitsOfALazyLine`). */
  lazyLine: boolean;
}

/** Where one exit of a run goes. */
interface ExitPlacement {
  /** The gap it goes in: before the event at this index. */
  gap: number;
  /** Whether it settles where it is put (`settleExits`). */
  settled: boolean;
}

/**
 * - A closed directive container ends right after its closing fence, where micromark only exits it
 *   on the next line.
 */
function exitsOfAClosedDirectiveContainer({ events, index }: ExitRun): ExitPlacement {
  // The fence is a line or less back.
  let gap = index - 1;
  while (
    gap > 0 &&
    !(events[gap][0] === "exit" && events[gap][1].type === "directiveContainerFence")
  ) {
    gap--;
  }
  return { gap: gap + 1, settled: true };
}

/**
 * - An unclosed directive container ends, with the containers inside it, right after its last
 *   content, before any prefixes of the containers around it that follow: its content document
 *   ended there. When the line after was lazy for it (no container of the line reached it), the
 *   flow construct left that last line ending out of its content, so a last line that held nothing
 *   else was no line of it at all, and its own exit goes before that last line ending.
 *
 * A last line held nothing else when it held nothing past the prefixes of the containers around
 * the block. It was no line of the block either when a flow token, such as a fenced code block
 * that took the line ending before it, ends in it, but an exit before it would fall inside that
 * token, so the exits go where they go after a line of content.
 */
function exitsOfAnUnclosedDirectiveContainer(
  run: ExitRun,
  block: DirectiveBlock,
  own: DirectiveBlock | undefined
): ExitPlacement {
  const { events, index, lastChunk, chunk, nextLine } = run;
  let gap = contentEnd(events, block, index);
  const lazy = run.lazyLine && block.contentStarts[nextLine - block.line] === undefined;
  const bare =
    lazy &&
    chunk !== undefined &&
    run.bareChunk &&
    gap === lastChunk + 1 &&
    block.contentStarts[chunk.start.line - block.line] === chunk.start.offset &&
    !flowTokenEndsIn(chunk);
  if (bare) gap = own !== undefined ? lastChunk - 1 : contentEnd(events, block, lastChunk - 1);
  return { gap, settled: own !== undefined && (bare || !lazy) };
}

/**
 * - A container inside a directive container that goes on, ended by a line (lazily, or by a
 *   `_closeFlow` such as a new list item's), ends where the directive's content starts on that
 *   line, after the prefixes of the containers around the directive; micromark puts it before
 *   them, right after the line before.
 */
function exitsOfAContainerEndedByALine(
  { events, index, end, lastChunk, nextLine }: ExitRun,
  block: DirectiveBlock
): ExitPlacement {
  const start = block.contentStarts[nextLine - block.line];
  if (start === undefined) return { gap: end, settled: false };
  let gap = lastChunk + 1;
  while (gap < events.length) {
    if (gap === index) gap = end;
    if (gap === events.length) break;
    const token = events[gap][1];
    if (token.start.offset >= start || token._container === true || token._directive !== undefined)
      break;
    gap++;
  }
  return { gap, settled: false };
}

/**
 * - A line that opens a directive container first, where some container did not go on, was lazy
 *   for the flow construct, which opened no container there, and micromark puts a lazy line's
 *   exits right after the line before; so do the lines `lazyContentEnd` ends a block at.
 *
 * `lazy` is whether the line after the run's last content is such a line, which the rules above
 * read as well; the gap is where the exits of a container in no directive container go.
 */
function exitsOfALazyLine({
  events,
  context,
  end,
  lastChunk,
  nextLine,
}: ExitRun): ExitPlacement & { lazy: boolean } {
  let after = end;
  while (after < events.length && events[after][1].type === "linePrefix") after++;
  const lazy =
    nextLine !== -1 &&
    (context.parser.lazy[nextLine] === true ||
      (after < events.length &&
        events[after][0] === "enter" &&
        events[after][1]._directive !== undefined &&
        events[after][1].start.line === nextLine));
  return { lazy, gap: lastChunk + 1, settled: false };
}

/**
 * - The directive container's own exit is the flow construct's, which `subtokenize` never moved,
 *   so it is no longer marked `_container` once placed, except where it goes before a last line
 *   ending: `subtokenize` puts it there.
 */
function settleExits(settled: Token[]): void {
  for (const token of settled) token._container = false;
}

/**
 * Moves each exit of the run to the gap its rule chose, and settles the exits that go exactly
 * where they are put (`settleExits`), unless an exit would move over another container's event.
 * It answers where the driver goes on: the last `chunkFlow` exit, which a move can have shifted,
 * and the end of the window it rebuilt.
 */
function placeExits(
  run: ExitRun,
  gaps: number[],
  settled: Token[]
): { lastChunk: number; windowEnd: number } {
  const { events, index, end } = run;
  let lastChunk = run.lastChunk;
  // An exit moves back over prefixes and content only, never over another container's event.
  let valid = true;
  for (let at = Math.min(gaps[0], index); at < index && valid; at++) {
    valid = events[at][1]._container !== true && events[at][1]._directive === undefined;
  }
  let windowEnd = end;
  if (valid) {
    settleExits(settled);
  }
  if (valid && (gaps[0] !== end || gaps[gaps.length - 1] !== end)) {
    const windowStart = Math.min(index, gaps[0]);
    windowEnd = Math.max(end, gaps[gaps.length - 1]);
    const placed: Event[] = [];
    let next = 0;
    for (let at = windowStart; at <= windowEnd; at++) {
      while (next < gaps.length && gaps[next] === at) {
        const exit = events[index + next];
        if (at < index) exit[1].end = { ...events[at - 1][1].end };
        placed.push(exit);
        next++;
      }
      if (at < windowEnd && (at < index || at >= end)) placed.push(events[at]);
    }
    for (let at = 0; at < placed.length; at++) events[windowStart + at] = placed[at];
    if (lastChunk >= windowStart) {
      lastChunk = windowEnd - 1;
      while (!(events[lastChunk][0] === "exit" && events[lastChunk][1].type === "chunkFlow"))
        lastChunk--;
    }
  }
  return { lastChunk, windowEnd };
}

/**
 * The gap right after the last event of `block`'s content before `from`: back over the prefixes
 * of the containers around it, the events that start before its content does on their line, and
 * over whole lines its continuation did not reach, never past its opening fence.
 */
function contentEnd(events: Event[], block: DirectiveBlock, from: number): number {
  let gap = from;
  while (gap > 0) {
    const token = events[gap - 1][1];
    if (
      token._container === true ||
      token._directive !== undefined ||
      token.type === "chunkFlow" ||
      token.type === "directiveContainerFence"
    ) {
      break;
    }
    const start = block.contentStarts[token.start.line - block.line];
    if (start !== undefined && token.start.offset >= start) break;
    gap--;
  }
  return gap;
}

/**
 * Whether a flow token that opens before `chunk`, the last chunk of its flow, closes in it.
 * `subtokenize` gives a chunk the flow's events after the line ending that ends the chunk before
 * it, so such a token's exit falls in `chunk`, whatever offset it ends at. A directive container
 * ends where micromark closes the flow, so the last chunk of its content is the last of its flow,
 * and only that chunk's events and the line ending before it are read.
 */
function flowTokenEndsIn(chunk: Token): boolean {
  const flowEvents = chunk._tokenizer?.events ?? [];
  for (let index = flowEvents.length - 1; index >= 0; index--) {
    const [kind, token] = flowEvents[index];
    if (token.end.offset < chunk.start.offset) return false;
    if (kind === "exit" && token.start.offset < chunk.start.offset)
      return token.type !== "lineEnding" && token.type !== "lineEndingBlank";
  }
  return false;
}

const label: Construct = { tokenize: tokenizeLabel, partial: true };
const attributes: Construct = { tokenize: tokenizeAttributes, partial: true };
const closingFence: Construct = { tokenize: tokenizeClosingFence, partial: true };

function tokenizeDirectiveContainerStart(
  this: TokenizeContext,
  effects: Effects,
  ok: State,
  nok: State
): State {
  const self = this;
  const tail = self.events[self.events.length - 1];
  const initialSize =
    tail !== undefined && tail[1].type === "linePrefix"
      ? tail[2].sliceSerialize(tail[1], true).length
      : 0;
  let sizeOpen = 0;
  let depth = 0;
  let blocks = 0;
  let opener: Token;
  return start;

  function start(code: Code): State | undefined {
    depth = openAround(self) + 1;
    if (depth > maxDirectiveNesting) return nok(code);
    blocks = blocksAround(self) + 1;
    if (blocks > maxBlockNesting) return nok(code);
    opener = effects.enter("directiveContainer", { _container: true });
    effects.enter("directiveContainerFence");
    effects.enter("directiveContainerSequence");
    return sequenceOpen(code);
  }

  function sequenceOpen(code: Code): State | undefined {
    if (code === 58) {
      effects.consume(code);
      sizeOpen++;
      return sequenceOpen;
    }
    if (sizeOpen < 3) return nok(code);
    effects.exit("directiveContainerSequence");
    return factoryName.call(self, effects, afterName, nok, "directiveContainerName")(code);
  }

  function afterName(code: Code): State | undefined {
    return code === 91 ? effects.attempt(label, afterLabel, afterLabel)(code) : afterLabel(code);
  }

  function afterLabel(code: Code): State | undefined {
    return code === 123
      ? effects.attempt(attributes, afterAttributes, afterAttributes)(code)
      : afterAttributes(code);
  }

  function afterAttributes(code: Code): State | undefined {
    return factorySpace(effects, openAfter, "whitespace")(code);
  }

  function openAfter(code: Code): State | undefined {
    if (code !== null && !markdownLineEnding(code)) return nok(code);
    effects.exit("directiveContainerFence");
    const block: DirectiveBlock = {
      fenceSize: sizeOpen,
      indent: initialSize,
      depth,
      blocks,
      line: self.now().line,
      closed: false,
      contentStarts: [],
    };
    const state = self.containerState;
    if (state === undefined) {
      throw new Error("micromark opened a directive container with no container state");
    }
    state.directive = block;
    opener._directive = block;
    opener._nestDepth = blocks;
    if (someContainerEnded(self, opener)) self.parser.lazy[block.line] = true;
    // A start that micromark ran inside `effects.check` runs again at once, after any exits.
    parseRecord(self).open = depth;
    return ok(code);
  }
}

/**
 * Whether the line a directive container opens on is one where some open container did not go on
 * and no other container opened first: a lazy line for the flow construct, which opened no
 * container there. Marked lazy before micromark closes the flow, as it was then, so that the
 * flow's last fenced code, HTML or indented code block ends before the last line ending rather
 * than taking it.
 *
 * The document tokenizer tries containers on such a line from its check for new containers, and
 * there leaves `interrupt` unset: where every container went on, it sets it first, or the line goes
 * to the flow untried. A list item's continuation also unsets it when the line starts a new item
 * of its list, which ends the item but is no lazy line. So a container entered, or a list item
 * prefix, after the last line's content means the line is not one: a container opened on it first,
 * or a list went on with a new item.
 */
function someContainerEnded(context: TokenizeContext, opener: Token): boolean {
  if (context.interrupt !== undefined) return false;
  for (let index = context.events.length - 1; index >= 0; index--) {
    const [kind, token] = context.events[index];
    if (kind === "exit" && token.type === "chunkFlow") return true;
    if (kind !== "enter" || token === opener) continue;
    if (token._container === true || token.type === "listItemPrefix") return false;
  }
  return false;
}

function tokenizeDirectiveContainerContinuation(
  this: TokenizeContext,
  effects: Effects,
  ok: State,
  nok: State
): State {
  const self = this;
  const state = directiveState(self);
  const block = state.directive;
  return start;

  function start(code: Code): State | undefined {
    if (block.closed) {
      parseRecord(self).reachedClosed = self.now().line;
      return nok(code);
    }
    recordOpenAround(self, block.depth);
    recordContinued(self, block.blocks);
    // Only a line that opens with a colon or a blank can hold a closing fence, and an attempt on
    // every line at every level is what nesting costs.
    if (code !== 58 && !markdownSpace(code)) return content(code);
    return effects.attempt(closingFence, closed, unclosed)(code);
  }

  // A block quote inside the block goes on at no end of a document, so where the flow construct's
  // content document ended, its document tokenizer marked the line lazy, and a fenced code block
  // inside ends before the last line ending rather than taking it as a line of code.
  function closed(code: Code): State | undefined {
    block.closed = true;
    recordOpenAround(self, block.depth - 1);
    recordContinued(self, block.blocks - 1);
    parseRecord(self).open = block.depth - 1;
    if (blockQuoteOpenInside(self.events, block)) self.parser.lazy[self.now().line] = true;
    state._closeFlow = true;
    return ok(code);
  }

  // The indentation is stripped as `listItemIndent`, the type micromark's list item gives the
  // indentation its own continuation strips: a list opening on the line reads a `linePrefix` just
  // before it as its own indentation, which would count this block's twice, and micromark and
  // mdast-util-from-markdown step over a `listItemIndent` where they step over a `linePrefix`
  // when they settle where a container or a list item ends.
  function unclosed(code: Code): State | undefined {
    const indent = block.indent;
    return indent > 0
      ? factorySpace(effects, content, "listItemIndent", indent + 1)(code)
      : content(code);
  }

  function content(code: Code): State | undefined {
    const point = self.now();
    block.contentStarts[point.line - block.line] = point.offset;
    return ok(code);
  }
}

function tokenizeClosingFence(
  this: TokenizeContext,
  effects: Effects,
  ok: State,
  nok: State
): State {
  const sizeOpen = directiveState(this).directive.fenceSize;
  let size = 0;
  return factorySpace(
    effects,
    prefixAfter,
    "linePrefix",
    this.parser.constructs.disable.null?.includes("codeIndented") ? undefined : 4
  );

  function prefixAfter(code: Code): State | undefined {
    effects.enter("directiveContainerFence");
    effects.enter("directiveContainerSequence");
    return sequence(code);
  }

  function sequence(code: Code): State | undefined {
    if (code === 58) {
      effects.consume(code);
      size++;
      return sequence;
    }
    if (size < sizeOpen) return nok(code);
    effects.exit("directiveContainerSequence");
    return factorySpace(effects, end, "whitespace")(code);
  }

  function end(code: Code): State | undefined {
    if (code !== null && !markdownLineEnding(code)) return nok(code);
    effects.exit("directiveContainerFence");
    return ok(code);
  }
}

function exitDirectiveContainer(this: TokenizeContext, effects: Effects): undefined {
  const block = directiveState(this).directive;
  const record = parseRecord(this);
  record.open = Math.min(record.open, block.depth - 1);
  if (block.closed) record.reachedClosed = 0;
  effects.exit("directiveContainer");
}

/** Whether a block quote is open inside `block`, at the end of the document's events so far. */
function blockQuoteOpenInside(events: Event[], block: DirectiveBlock): boolean {
  let closed = 0;
  for (let index = events.length - 1; index >= 0; index--) {
    const [kind, token] = events[index];
    if (token._directive === block) return false;
    if (token._container !== true) continue;
    if (kind === "exit") closed++;
    else if (closed > 0) closed--;
    else if (token.type === "blockQuote") return true;
  }
  return false;
}

/** The container state of a directive container that opened: `openAfter` put its block on it. */
interface DirectiveState extends ContainerState {
  directive: DirectiveBlock;
}

/**
 * The state of the directive container whose continuation, closing fence or exit `context` runs.
 * micromark sets a container's state before it runs any of them, and `openAfter` puts the block
 * on that state when the container opens, so a state without one is a broken invariant rather
 * than a line to read some other way: a fence size of 0, say, would let a blank line close it.
 */
function directiveState(context: TokenizeContext): DirectiveState {
  const state = context.containerState;
  if (state?.directive === undefined) {
    throw new Error("micromark ran a directive container whose state holds no block");
  }
  // Checked just above: TypeScript narrows the property, not the state that holds it.
  return state as DirectiveState;
}

function tokenizeLabel(this: TokenizeContext, effects: Effects, ok: State, nok: State): State {
  // Always a `[`.
  return factoryLabel(
    effects,
    ok,
    nok,
    "directiveContainerLabel",
    "directiveContainerLabelMarker",
    "directiveContainerLabelString",
    true
  );
}

function tokenizeAttributes(this: TokenizeContext, effects: Effects, ok: State, nok: State): State {
  // Always a `{`.
  return factoryAttributes(
    effects,
    ok,
    nok,
    "directiveContainerAttributes",
    "directiveContainerAttributesMarker",
    "directiveContainerAttribute",
    "directiveContainerAttributeId",
    "directiveContainerAttributeClass",
    "directiveContainerAttributeName",
    "directiveContainerAttributeInitializerMarker",
    "directiveContainerAttributeValueLiteral",
    "directiveContainerAttributeValue",
    "directiveContainerAttributeValueMarker",
    "directiveContainerAttributeValueData",
    true
  );
}
