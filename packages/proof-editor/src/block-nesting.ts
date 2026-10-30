/**
 * How deeply markdown blocks may nest, as the document service counts them, and micromark's `list`
 * and `blockQuote` held to it.
 *
 * The service refuses a document that opens a block inside `maxBlockNesting` blocks, where a
 * quote, a list, a list item, a typed block and a footnote definition each count one (`maxNesting`,
 * packages/envoy/internal/dispatch/pmdoc/nesting.go). micromark has no such bound: every open
 * container runs its continuation on every line, so a body of D nested items and N lines costs
 * D*N, and a chain of a few hundred openers on one line also overflows the stack of Milkdown's
 * parser and of prosemirror-model's `DOMSerializer`, which costs a reader the page rather than the
 * body (sjawhar/legion#2519). An opener past the bound therefore does not open here either; it
 * reads as the text it is, the way a `:::` opener past `maxDirectiveNesting` does
 * (./directive-container.ts). A body the service accepted nests within the bound, so no opener in
 * one is ever refused and it reads exactly as it read before.
 *
 * A list opener opens a list *and* its first item, so it takes two of the service's levels; a
 * later item of an open list stands where the first one did and takes none, as goldmark's list
 * items carry no guard of their own (nesting.go). A quote opener takes one. A `:::` container
 * takes one as well, which ./directive-container.ts records into the same count.
 *
 * Footnote definitions, which the service counts, are not counted here: the browser reads them
 * through micromark-extension-gfm-footnote, whose construct this module does not rewrite. So a
 * body whose blocks stand inside footnote definitions may draw here where the service would refuse
 * it, and never the other way round: everything the service stores still reads the same.
 *
 * The two constructs are rewritten in place rather than added as a micromark extension: an
 * extension can add a construct for a character but not replace one, and turning the originals off
 * by name (`disable`) also turns off the `effects.attempt(list, ...)` and
 * `effects.attempt(blockQuote, ...)` each of them runs on itself to read a later item or a deeper
 * `>` (micromark/lib/create-tokenizer.js refuses any construct whose name is disabled, whoever
 * passes it). Every markdown parse in this realm therefore reads the bound - the page bundle, the
 * parse worker and the pmdoc fixture generator, which imports `@legion/proof-editor/headless` -
 * down to a bare `unified().use(remarkParse)` processor, which shares the two objects with every
 * other parser in the tree. A second copy of `micromark-core-commonmark` installed beside this
 * one would escape the rewrite, which is why a test of this package parses through such a
 * processor. The rewrite also reaches the live editor's Milkdown parser, which is built by
 * `@milkdown/preset-commonmark` and passes through none of this package's unified processors.
 */

import { blockQuote, list } from "micromark-core-commonmark";
import type {
  Code,
  Construct,
  ContainerState,
  Effects,
  ParseContext,
  State,
  Token,
  TokenizeContext,
} from "micromark-util-types";

declare module "micromark-util-types" {
  interface Token {
    /** How many blocks the document service counts around this container's content. */
    _nestDepth?: number | undefined;
  }

  interface ContainerState {
    /** How many blocks the document service counts around this container's content. */
    _nestDepth?: number | undefined;
  }
}

/**
 * How many blocks a document may open inside one another: `maxNesting` in
 * packages/envoy/internal/dispatch/pmdoc/nesting.go, whose comment names this one. A quote, a
 * list, a list item, a typed block and a footnote definition each count one.
 */
export const maxBlockNesting = 100;

/**
 * Per parse, what the document tokenizer has found of the containers around its lines: the depths
 * recorded for the line it is on, and the directive containers ./directive-container.ts follows
 * across lines. The two modules count different containers over the same lines, so one record
 * holds both counts: a continuation records its depth once per line, and every start, here or
 * there, reads what the line stands inside.
 */
export interface ParseRecord {
  /** The line `blocks` and `directives` were last recorded on. */
  line: number;
  /**
   * How many blocks stand around what that line holds: the depth of the innermost container whose
   * continuation passed it. The document tokenizer runs continuations outermost first and stops
   * at the first that fails, so the last one to record on a line is the innermost container still
   * open around it; a continuation records only where it succeeded, so one that failed leaves the
   * line at its parent's depth.
   */
  blocks: number;
  /**
   * How many directive containers are open around that line, by the same reading. A start only
   * reads it, so a start that micromark runs inside `effects.check` and throws away cannot change
   * it.
   */
  directives: number;
  /** The depth of the innermost directive container that is open and not closed. */
  openDirectives: number;
  /**
   * The line on which the continuation of a closed directive container, still on the container
   * stack, last ran: every container around it went on there. It is 0 once that block exits.
   */
  reachedClosedDirective: number;
}

const parses = new WeakMap<ParseContext, ParseRecord>();

/** The record of `context`'s parse, made on the first call. */
export function parseRecord(context: TokenizeContext): ParseRecord {
  let record = parses.get(context.parser);
  if (record === undefined) {
    record = { line: 0, blocks: 0, directives: 0, openDirectives: 0, reachedClosedDirective: 0 };
    parses.set(context.parser, record);
  }
  return record;
}

/**
 * The record to record a depth of the current line into: a record of an earlier line holds no
 * container of this one, so both depths start again at 0.
 */
function recordToWrite(context: TokenizeContext): ParseRecord {
  const record = parseRecord(context);
  const line = context.now().line;
  if (record.line !== line) {
    record.line = line;
    record.blocks = 0;
    record.directives = 0;
  }
  return record;
}

/** Records that a container whose content stands inside `depth` blocks went on at the current line. */
export function recordContinued(context: TokenizeContext, depth: number): void {
  recordToWrite(context).blocks = depth;
}

/** Records that `depth` directive containers are open around the current line. */
export function recordDirectivesAround(context: TokenizeContext, depth: number): void {
  recordToWrite(context).directives = depth;
}

/**
 * How many blocks stand around a container opening now: the one that opened before it on this
 * line, or else the innermost one whose continuation passed the line.
 */
export function blocksAround(context: TokenizeContext): number {
  const opened = openedOnLine(context);
  if (opened !== undefined) return opened._nestDepth ?? 0;
  const record = parses.get(context.parser);
  return record !== undefined && record.line === context.now().line ? record.blocks : 0;
}

/** How many directive containers are open around the current line; it never makes a record. */
export function directivesAround(context: TokenizeContext): number {
  const record = parses.get(context.parser);
  return record !== undefined && record.line === context.now().line ? record.directives : 0;
}

/**
 * The token of the innermost container that opened on the current line and has not closed, or
 * undefined where none did. The walk stops at the flow chunk of the line before, so it reads only
 * this line's events: the containers it opened, each stamped with its depth, and the exits of the
 * ones it did not continue, which the document tokenizer appends before a container opens.
 *
 * A start reads the events rather than what the last start recorded because micromark runs the
 * first start of a line twice, once inside the `check` of its `checkNewContainers`, whose events
 * it throws away, and once for real; both runs then read the same depth.
 */
function openedOnLine(context: TokenizeContext): Token | undefined {
  const events = context.events;
  let closed = 0;
  for (let index = events.length - 1; index >= 0; index--) {
    const [kind, token] = events[index];
    if (kind === "exit" && token.type === "chunkFlow") return undefined;
    if (token._container !== true) continue;
    if (kind === "exit") closed++;
    else if (closed > 0) closed--;
    else return token;
  }
  return undefined;
}

/**
 * Holds micromark's `list` and `blockQuote` to `maxBlockNesting`. It runs once; a second call does
 * nothing, so every import of the module that installs it leaves the two constructs rewritten
 * exactly once.
 */
export function boundBlockNesting(): void {
  bound(list, 2, (state) => state.type === undefined);
  bound(blockQuote, 1, (state) => state.open !== true);
}

/** Whether a construct already carries the rewrite below. */
const rewritten = new WeakSet<object>();

/** The container state micromark set before it ran a container's start or continuation. */
function containerStateOf(context: TokenizeContext): ContainerState {
  const state = context.containerState;
  if (state === undefined) {
    throw new Error("micromark ran a container construct with no container state");
  }
  return state;
}

/**
 * Rewrites `construct` so its start refuses an opener that would stand past the bound and stamps
 * the container it opens with its depth, and its continuation records that depth for the line it
 * went on at. `opens` is how many blocks the opener opens; `isFresh` says whether a start opens a
 * container rather than reading a later part of an open one (a later item of a list, a deeper `>`
 * of a quote), which the service never refuses.
 */
function bound(
  construct: Construct,
  opens: number,
  isFresh: (state: ContainerState) => boolean
): void {
  if (rewritten.has(construct)) return;
  rewritten.add(construct);

  const start = construct.tokenize;
  construct.tokenize = function boundStart(
    this: TokenizeContext,
    effects: Effects,
    ok: State,
    nok: State
  ): State {
    const state = containerStateOf(this);
    let depth = 0;
    const inner = start.call(
      this,
      effects,
      (code: Code) => {
        if (depth > 0) {
          const opened = openedOnLine(this);
          if (opened !== undefined) opened._nestDepth = depth;
        }
        return ok(code);
      },
      nok
    );
    return (code: Code) => {
      depth = 0;
      if (isFresh(state)) {
        depth = blocksAround(this) + opens;
        if (depth > maxBlockNesting) return nok(code);
        state._nestDepth = depth;
      }
      return inner(code);
    };
  };

  const continuation = construct.continuation;
  if (continuation === undefined) return;
  const onLine = continuation.tokenize;
  continuation.tokenize = function boundContinuation(
    this: TokenizeContext,
    effects: Effects,
    ok: State,
    nok: State
  ): State {
    const state = containerStateOf(this);
    return onLine.call(
      this,
      effects,
      (code: Code) => {
        if (state._nestDepth !== undefined) recordContinued(this, state._nestDepth);
        return ok(code);
      },
      nok
    );
  };
}
