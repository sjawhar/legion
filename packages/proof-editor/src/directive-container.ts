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
 * block at a lazy line as it did, and a closing fence over an open block quote marks its line lazy.
 * Two readings differ, both of them micromark's order of work rather than the block's grammar: a
 * document construct cannot open after an unclosed front matter opener, which micromark tries to
 * the end of the document while it opens no container, and a footnote definition inside a block
 * now defines its footnote for a call in the text before the block, which the flow construct, by
 * tokenizing the block's content after that text, did not.
 */

import { factorySpace } from "micromark-factory-space";
import { factoryWhitespace } from "micromark-factory-whitespace";
import {
  markdownLineEnding,
  markdownLineEndingOrSpace,
  markdownSpace,
  unicodePunctuation,
  unicodeWhitespace,
} from "micromark-util-character";
import type {
  Code,
  Construct,
  Effects,
  Event,
  Extension,
  ParseContext,
  State,
  Token,
  TokenizeContext,
  TokenType,
} from "micromark-util-types";

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
  /**
   * How many directive containers are open around `line`: the depth of the innermost block whose
   * continuation the line passed. The document tokenizer runs continuations outermost first and
   * stops at the first that fails, so the last one run on a line is the innermost directive
   * container still open around it. A start only reads this, so a start that micromark runs inside
   * `effects.check` and throws away cannot change it.
   */
  line: number;
  depth: number;
  /** The depth of the innermost directive container that is open and not closed. */
  open: number;
}

const parses = new WeakMap<ParseContext, ParseRecord>();

function parseRecord(context: TokenizeContext): ParseRecord {
  let record = parses.get(context.parser);
  if (record === undefined) {
    record = { line: 0, depth: 0, open: 0 };
    parses.set(context.parser, record);
  }
  return record;
}

function recordOpenAround(context: TokenizeContext, depth: number): void {
  const record = parseRecord(context);
  record.line = context.now().line;
  record.depth = depth;
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
    const line = self.now().line;
    if (record === undefined || markdownLineEnding(code)) return nok(code);
    if (record.open <= (record.line === line ? record.depth : 0)) return nok(code);
    self.parser.lazy[line] = true;
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
 * Here every container is on one stack, so:
 *
 * - A container inside a directive container that goes on, ended by a line (lazily, or by a
 *   `_closeFlow` such as a new list item's), ends where the directive's content starts on that
 *   line, after the prefixes of the containers around the directive; micromark puts it before
 *   them, right after the line before.
 * - An unclosed directive container ends, with the containers inside it, right after its last
 *   content, before any prefixes of the containers around it that follow: its content document
 *   ended there. When the line after was lazy for it (no container of the line reached it), the
 *   flow construct left that last line ending out of its content, so a last line that held nothing
 *   else was no line of it at all, and its own exit goes before that last line ending.
 * - A closed directive container ends right after its closing fence, where micromark only exits it
 *   on the next line.
 * - The directive container's own exit is the flow construct's, which `subtokenize` never moved,
 *   so it is no longer marked `_container` once placed, except where it goes before a last line
 *   ending: `subtokenize` puts it there.
 * - A line that opens a directive container first, where some container did not go on, was lazy
 *   for the flow construct, which opened no container there, and micromark puts a lazy line's
 *   exits right after the line before; so do the lines `lazyContentEnd` ends a block at.
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
    // The line after the last content, when that content ends in a line ending.
    const nextLine =
      chunk !== undefined && chunk.end.line !== chunk.start.line ? chunk.end.line : -1;
    // Whether that line was lazy: micromark found no container there, or it opens a directive
    // container first, which the flow construct, opening no container, read as a lazy line.
    let after = end;
    while (after < events.length && events[after][1].type === "linePrefix") after++;
    const lazyLine =
      nextLine !== -1 &&
      (context.parser.lazy[nextLine] === true ||
        (after < events.length &&
          events[after][0] === "enter" &&
          events[after][1]._directive !== undefined &&
          events[after][1].start.line === nextLine));
    // Whether the last content is only a line ending.
    const bareChunk =
      chunk !== undefined &&
      chunk.end.offset - chunk.start.offset <= 2 &&
      context.sliceStream(chunk).length === 1;
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
      let gap = end;
      if (block !== undefined && (own !== undefined || owner >= remaining) && block.closed) {
        // A closed directive container ends right after its closing fence, a line or less back.
        gap = index - 1;
        while (
          gap > 0 &&
          !(events[gap][0] === "exit" && events[gap][1].type === "directiveContainerFence")
        ) {
          gap--;
        }
        gap++;
        settled.push(exiting);
      } else if (block !== undefined && (own !== undefined || owner >= remaining)) {
        // An unclosed one ends with the containers inside it right after its last content. When the
        // line after was lazy for it, the flow construct left the last line ending out of its
        // content, so a last line that held nothing else (past the prefixes of the containers
        // around it) was no line of it at all; else its own exit goes before that line ending, where
        // `subtokenize` moves it.
        gap = contentEnd(events, block, index);
        const lazy = lazyLine && block.contentStarts[nextLine - block.line] === undefined;
        const bare =
          lazy &&
          bareChunk &&
          gap === lastChunk + 1 &&
          block.contentStarts[chunk.start.line - block.line] === chunk.start.offset;
        if (bare)
          gap = own !== undefined ? lastChunk - 1 : contentEnd(events, block, lastChunk - 1);
        if (own !== undefined && (bare || !lazy)) settled.push(exiting);
      } else if (block !== undefined && nextLine !== -1 && (lastChunk === index - 1 || lazyLine)) {
        // A container inside a directive container that goes on ends where the directive's content
        // starts on the next line. micromark puts a lazy line's exits right after the line before.
        const start = block.contentStarts[nextLine - block.line];
        if (start !== undefined) {
          gap = lastChunk + 1;
          while (gap < events.length) {
            if (gap === index) gap = end;
            if (gap === events.length) break;
            const token = events[gap][1];
            if (
              token.start.offset >= start ||
              token._container === true ||
              token._directive !== undefined
            )
              break;
            gap++;
          }
        }
      } else if (block === undefined && lazyLine) {
        // A container in no directive container that a lazy line ends ends right after the line
        // before, where micromark puts a lazy line's exits.
        gap = lastChunk + 1;
      }
      if (gap >= index && gap <= end) gap = end;
      if (at < end - 1) gap = Math.min(gap, gaps[at + 1 - index]);
      gaps[at - index] = gap;
    }

    // An exit moves back over prefixes and content only, never over another container's event.
    let valid = true;
    for (let at = Math.min(gaps[0], index); at < index && valid; at++) {
      valid = events[at][1]._container !== true && events[at][1]._directive === undefined;
    }
    let windowEnd = end;
    if (valid) {
      for (const token of settled) token._container = false;
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
    open.length = remaining;
    owners.length = remaining;
    index = windowEnd;
  }
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
  let opener: Token | undefined;
  return start;

  function start(code: Code): State | undefined {
    const around = parses.get(self.parser);
    depth = (around !== undefined && around.line === self.now().line ? around.depth : 0) + 1;
    if (depth > maxDirectiveNesting) return nok(code);
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
      line: self.now().line,
      closed: false,
      contentStarts: [],
    };
    if (self.containerState !== undefined) self.containerState.directive = block;
    if (opener !== undefined) opener._directive = block;
    // A start that micromark ran inside `effects.check` runs again at once, after any exits.
    parseRecord(self).open = depth;
    return ok(code);
  }
}

function tokenizeDirectiveContainerContinuation(
  this: TokenizeContext,
  effects: Effects,
  ok: State,
  nok: State
): State {
  const self = this;
  const state = self.containerState;
  const block = state?.directive;
  return start;

  function start(code: Code): State | undefined {
    if (state === undefined || block === undefined || block.closed) return nok(code);
    recordOpenAround(self, block.depth);
    // Only a line that opens with a colon or a blank can hold a closing fence, and an attempt on
    // every line at every level is what nesting costs.
    if (code !== 58 && !markdownSpace(code)) return content(code);
    return effects.attempt(closingFence, closed, unclosed)(code);
  }

  // A block quote inside the block goes on at no end of a document, so where the flow construct's
  // content document ended, its document tokenizer marked the line lazy, and a fenced code block
  // inside ends before the last line ending rather than taking it as a line of code.
  function closed(code: Code): State | undefined {
    if (state === undefined || block === undefined) return nok(code);
    block.closed = true;
    recordOpenAround(self, block.depth - 1);
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
    const indent = block?.indent ?? 0;
    return indent > 0
      ? factorySpace(effects, content, "listItemIndent", indent + 1)(code)
      : content(code);
  }

  function content(code: Code): State | undefined {
    if (block !== undefined) block.contentStarts[self.now().line - block.line] = self.now().offset;
    return ok(code);
  }
}

function tokenizeClosingFence(
  this: TokenizeContext,
  effects: Effects,
  ok: State,
  nok: State
): State {
  const sizeOpen = this.containerState?.directive?.fenceSize ?? 0;
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
  const block = this.containerState?.directive;
  if (block !== undefined) {
    const record = parseRecord(this);
    record.open = Math.min(record.open, block.depth - 1);
  }
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

/*
 * `factoryName`, `factoryLabel` and `factoryAttributes` below are copied from
 * micromark-extension-directive 4.0.0 (`lib/factory-name.js`, `lib/factory-label.js`,
 * `lib/factory-attributes.js`), which does not export them, typed and formatted for this file and
 * otherwise unchanged. They are under that package's MIT license (its `license` file):
 *
 * (The MIT License)
 *
 * Copyright (c) Titus Wormer <tituswormer@gmail.com>
 *
 * Permission is hereby granted, free of charge, to any person obtaining
 * a copy of this software and associated documentation files (the
 * 'Software'), to deal in the Software without restriction, including
 * without limitation the rights to use, copy, modify, merge, publish,
 * distribute, sublicense, and/or sell copies of the Software, and to
 * permit persons to whom the Software is furnished to do so, subject to
 * the following conditions:
 *
 * The above copyright notice and this permission notice shall be
 * included in all copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED 'AS IS', WITHOUT WARRANTY OF ANY KIND,
 * EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
 * MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
 * IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY
 * CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT,
 * TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE
 * SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
 */

function factoryName(
  this: TokenizeContext,
  effects: Effects,
  ok: State,
  nok: State,
  type: TokenType
): State {
  const self = this;
  return start;

  function start(code: Code): State | undefined {
    if (
      code === null ||
      markdownLineEnding(code) ||
      unicodePunctuation(code) ||
      unicodeWhitespace(code)
    ) {
      return nok(code);
    }
    effects.enter(type);
    effects.consume(code);
    return name;
  }

  function name(code: Code): State | undefined {
    if (
      code === null ||
      markdownLineEnding(code) ||
      unicodeWhitespace(code) ||
      (unicodePunctuation(code) && code !== 45 && code !== 95)
    ) {
      effects.exit(type);
      return self.previous === 45 || self.previous === 95 ? nok(code) : ok(code);
    }
    effects.consume(code);
    return name;
  }
}

// This is a fork of:
// <https://github.com/micromark/micromark/tree/main/packages/micromark-factory-label>
// to allow empty labels, balanced brackets (such as for nested directives),
// text instead of strings, and optionally disallows EOLs.
function factoryLabel(
  effects: Effects,
  ok: State,
  nok: State,
  type: TokenType,
  markerType: TokenType,
  stringType: TokenType,
  disallowEol?: boolean | undefined
): State {
  let size = 0;
  let balance = 0;
  let previous: Token | undefined;
  return start;

  function start(code: Code): State | undefined {
    effects.enter(type);
    effects.enter(markerType);
    effects.consume(code);
    effects.exit(markerType);
    return afterStart;
  }

  function afterStart(code: Code): State | undefined {
    if (code === 93) {
      effects.enter(markerType);
      effects.consume(code);
      effects.exit(markerType);
      effects.exit(type);
      return ok;
    }
    effects.enter(stringType);
    return lineStart(code);
  }

  function lineStart(code: Code): State | undefined {
    if (code === 93 && !balance) {
      return atClosingBrace(code);
    }
    const token = effects.enter("chunkText", {
      _contentTypeTextTrailing: true,
      contentType: "text",
      previous,
    });
    if (previous) previous.next = token;
    previous = token;
    return data(code);
  }

  function data(code: Code): State | undefined {
    if (code === null || size > 999) {
      return nok(code);
    }
    if (code === 91 && ++balance > 32) {
      return nok(code);
    }
    if (code === 93 && !balance--) {
      effects.exit("chunkText");
      return atClosingBrace(code);
    }
    if (markdownLineEnding(code)) {
      if (disallowEol) {
        return nok(code);
      }
      effects.consume(code);
      effects.exit("chunkText");
      return lineStart;
    }
    effects.consume(code);
    return code === 92 ? dataEscape : data;
  }

  function dataEscape(code: Code): State | undefined {
    if (code === 91 || code === 92 || code === 93) {
      effects.consume(code);
      size++;
      return data;
    }
    return data(code);
  }

  function atClosingBrace(code: Code): State | undefined {
    effects.exit(stringType);
    effects.enter(markerType);
    effects.consume(code);
    effects.exit(markerType);
    effects.exit(type);
    return ok;
  }
}

function factoryAttributes(
  effects: Effects,
  ok: State,
  nok: State,
  attributesType: TokenType,
  attributesMarkerType: TokenType,
  attributeType: TokenType,
  attributeIdType: TokenType,
  attributeClassType: TokenType,
  attributeNameType: TokenType,
  attributeInitializerType: TokenType,
  attributeValueLiteralType: TokenType,
  attributeValueType: TokenType,
  attributeValueMarker: TokenType,
  attributeValueData: TokenType,
  disallowEol?: boolean | undefined
): State {
  let type: TokenType;
  let marker: Code | undefined;
  return start;

  function start(code: Code): State | undefined {
    effects.enter(attributesType);
    effects.enter(attributesMarkerType);
    effects.consume(code);
    effects.exit(attributesMarkerType);
    return between;
  }

  function between(code: Code): State | undefined {
    if (code === 35) {
      type = attributeIdType;
      return shortcutStart(code);
    }
    if (code === 46) {
      type = attributeClassType;
      return shortcutStart(code);
    }
    if (disallowEol && markdownSpace(code)) {
      return factorySpace(effects, between, "whitespace")(code);
    }
    if (!disallowEol && markdownLineEndingOrSpace(code)) {
      return factoryWhitespace(effects, between)(code);
    }
    if (
      code === null ||
      markdownLineEnding(code) ||
      unicodeWhitespace(code) ||
      (unicodePunctuation(code) && code !== 45 && code !== 95)
    ) {
      return end(code);
    }
    effects.enter(attributeType);
    effects.enter(attributeNameType);
    effects.consume(code);
    return name;
  }

  function shortcutStart(code: Code): State | undefined {
    // Assume it’s registered.
    const markerType = `${type}Marker` as TokenType;
    effects.enter(attributeType);
    effects.enter(type);
    effects.enter(markerType);
    effects.consume(code);
    effects.exit(markerType);
    return shortcutStartAfter;
  }

  function shortcutStartAfter(code: Code): State | undefined {
    if (
      code === null ||
      code === 34 ||
      code === 35 ||
      code === 39 ||
      code === 46 ||
      code === 60 ||
      code === 61 ||
      code === 62 ||
      code === 96 ||
      code === 125 ||
      markdownLineEndingOrSpace(code)
    ) {
      return nok(code);
    }

    // Assume it’s registered.
    const valueType = `${type}Value` as TokenType;
    effects.enter(valueType);
    effects.consume(code);
    return shortcut;
  }

  function shortcut(code: Code): State | undefined {
    if (
      code === null ||
      code === 34 ||
      code === 39 ||
      code === 60 ||
      code === 61 ||
      code === 62 ||
      code === 96
    ) {
      return nok(code);
    }
    if (code === 35 || code === 46 || code === 125 || markdownLineEndingOrSpace(code)) {
      // Assume it’s registered.
      const valueType = `${type}Value` as TokenType;
      effects.exit(valueType);
      effects.exit(type);
      effects.exit(attributeType);
      return between(code);
    }
    effects.consume(code);
    return shortcut;
  }

  function name(code: Code): State | undefined {
    if (
      code === null ||
      markdownLineEnding(code) ||
      unicodeWhitespace(code) ||
      (unicodePunctuation(code) && code !== 45 && code !== 46 && code !== 58 && code !== 95)
    ) {
      effects.exit(attributeNameType);
      if (disallowEol && markdownSpace(code)) {
        return factorySpace(effects, nameAfter, "whitespace")(code);
      }
      if (!disallowEol && markdownLineEndingOrSpace(code)) {
        return factoryWhitespace(effects, nameAfter)(code);
      }
      return nameAfter(code);
    }
    effects.consume(code);
    return name;
  }

  function nameAfter(code: Code): State | undefined {
    if (code === 61) {
      effects.enter(attributeInitializerType);
      effects.consume(code);
      effects.exit(attributeInitializerType);
      return valueBefore;
    }

    // Attribute w/o value.
    effects.exit(attributeType);
    return between(code);
  }

  function valueBefore(code: Code): State | undefined {
    if (
      code === null ||
      code === 60 ||
      code === 61 ||
      code === 62 ||
      code === 96 ||
      code === 125 ||
      (disallowEol && markdownLineEnding(code))
    ) {
      return nok(code);
    }
    if (code === 34 || code === 39) {
      effects.enter(attributeValueLiteralType);
      effects.enter(attributeValueMarker);
      effects.consume(code);
      effects.exit(attributeValueMarker);
      marker = code;
      return valueQuotedStart;
    }
    if (disallowEol && markdownSpace(code)) {
      return factorySpace(effects, valueBefore, "whitespace")(code);
    }
    if (!disallowEol && markdownLineEndingOrSpace(code)) {
      return factoryWhitespace(effects, valueBefore)(code);
    }
    effects.enter(attributeValueType);
    effects.enter(attributeValueData);
    effects.consume(code);
    marker = undefined;
    return valueUnquoted;
  }

  function valueUnquoted(code: Code): State | undefined {
    if (
      code === null ||
      code === 34 ||
      code === 39 ||
      code === 60 ||
      code === 61 ||
      code === 62 ||
      code === 96
    ) {
      return nok(code);
    }
    if (code === 125 || markdownLineEndingOrSpace(code)) {
      effects.exit(attributeValueData);
      effects.exit(attributeValueType);
      effects.exit(attributeType);
      return between(code);
    }
    effects.consume(code);
    return valueUnquoted;
  }

  function valueQuotedStart(code: Code): State | undefined {
    if (code === marker) {
      effects.enter(attributeValueMarker);
      effects.consume(code);
      effects.exit(attributeValueMarker);
      effects.exit(attributeValueLiteralType);
      effects.exit(attributeType);
      return valueQuotedAfter;
    }
    effects.enter(attributeValueType);
    return valueQuotedBetween(code);
  }

  function valueQuotedBetween(code: Code): State | undefined {
    if (code === marker) {
      effects.exit(attributeValueType);
      return valueQuotedStart(code);
    }
    if (code === null) {
      return nok(code);
    }

    // Note: blank lines can’t exist in content.
    if (markdownLineEnding(code)) {
      return disallowEol ? nok(code) : factoryWhitespace(effects, valueQuotedBetween)(code);
    }
    effects.enter(attributeValueData);
    effects.consume(code);
    return valueQuoted;
  }

  function valueQuoted(code: Code): State | undefined {
    if (code === marker || code === null || markdownLineEnding(code)) {
      effects.exit(attributeValueData);
      return valueQuotedBetween(code);
    }
    effects.consume(code);
    return valueQuoted;
  }

  function valueQuotedAfter(code: Code): State | undefined {
    return code === 125 || markdownLineEndingOrSpace(code) ? between(code) : end(code);
  }

  function end(code: Code): State | undefined {
    if (code === 125) {
      effects.enter(attributesMarkerType);
      effects.consume(code);
      effects.exit(attributesMarkerType);
      effects.exit(attributesType);
      return ok;
    }
    return nok(code);
  }
}
