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
    /** How many colons the block's opening fence holds. */
    directiveFenceSize?: number | undefined;
    /** How many columns the block's opening fence is indented. */
    directiveIndent?: number | undefined;
    /** How many directive containers stand around the block, itself included. */
    directiveDepth?: number | undefined;
    /** Whether a closing fence has closed the block. */
    directiveClosed?: boolean | undefined;
  }
}

/**
 * How many directive containers may stand inside one another. An opener standing inside this many
 * open directive containers does not open; the document service refuses a typed block nested that
 * deep.
 */
export const maxDirectiveNesting = 32;

/**
 * Per parse, how many directive containers are open around the line its document tokenizer is on:
 * the depth of the innermost block whose continuation the line passed. The document tokenizer runs
 * continuations outermost first and stops at the first that fails, so the last one run on a line is
 * the innermost directive container still open around it. A start only reads this, so a start that
 * micromark runs inside `effects.check` and throws away cannot change it.
 */
const openAround = new WeakMap<ParseContext, { line: number; depth: number }>();

function recordOpenAround(context: TokenizeContext, depth: number): void {
  const line = context.now().line;
  const around = openAround.get(context.parser);
  if (around === undefined) {
    openAround.set(context.parser, { line, depth });
  } else {
    around.line = line;
    around.depth = depth;
  }
}

/** The container directive, registered as `{ document: { 58: [directiveContainer] } }`. */
export const directiveContainer: Construct = {
  name: "directiveContainer",
  tokenize: tokenizeDirectiveContainerStart,
  continuation: { tokenize: tokenizeDirectiveContainerContinuation },
  exit: exitDirectiveContainer,
};

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
  return start;

  function start(code: Code): State | undefined {
    const around = openAround.get(self.parser);
    depth = (around !== undefined && around.line === self.now().line ? around.depth : 0) + 1;
    if (depth > maxDirectiveNesting) return nok(code);
    effects.enter("directiveContainer", { _container: true });
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
    const state = self.containerState ?? {};
    state.directiveFenceSize = sizeOpen;
    state.directiveIndent = initialSize;
    state.directiveDepth = depth;
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
  const state = self.containerState ?? {};
  const depth = state.directiveDepth ?? 0;
  const indent = state.directiveIndent ?? 0;
  return start;

  function start(code: Code): State | undefined {
    if (state.directiveClosed) return nok(code);
    recordOpenAround(self, depth);
    // Only a line that opens with a colon or a blank can hold a closing fence, and an attempt on
    // every line at every level is what nesting costs.
    if (code !== 58 && !markdownSpace(code)) return ok(code);
    return effects.attempt(closingFence, closed, unclosed)(code);
  }

  function closed(code: Code): State | undefined {
    state.directiveClosed = true;
    recordOpenAround(self, depth - 1);
    state._closeFlow = true;
    return ok(code);
  }

  // The indentation is stripped as `listItemIndent`, the type micromark's list item gives the
  // indentation its own continuation strips: a list opening on the line reads a `linePrefix` just
  // before it as its own indentation, which would count this block's twice, and micromark and
  // mdast-util-from-markdown step over a `listItemIndent` where they step over a `linePrefix`
  // when they settle where a container or a list item ends.
  function unclosed(code: Code): State | undefined {
    return indent > 0 ? factorySpace(effects, ok, "listItemIndent", indent + 1)(code) : ok(code);
  }
}

function tokenizeClosingFence(
  this: TokenizeContext,
  effects: Effects,
  ok: State,
  nok: State
): State {
  const sizeOpen = this.containerState?.directiveFenceSize ?? 0;
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
  effects.exit("directiveContainer");
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
