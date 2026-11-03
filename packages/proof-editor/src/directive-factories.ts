/*
 * `factoryName`, `factoryLabel` and `factoryAttributes`, copied from micromark-extension-directive
 * 4.0.0 (`lib/factory-name.js`, `lib/factory-label.js` and `lib/factory-attributes.js`, joined
 * here in that order, each after a comment naming its file), which does not export them;
 * `./directive-container` builds the container directive's opening fence from them. They are
 * kept whole: both of its calls pass `disallowEol = true`, and the branches for `false` stay as
 * the source has them. The only edits are the join and TypeScript's: the three files' imports are
 * one block above them, each JSDoc `@import` is an `import type`, the parameters and states carry
 * the types their JSDoc names, and the three `@type {TokenType}` casts are `as TokenType`.
 *
 * They are under that package's MIT license (its `license` file):
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

import type { Code, Effects, State, Token, TokenizeContext, TokenType } from 'micromark-util-types';
import { factorySpace } from 'micromark-factory-space';
import { factoryWhitespace } from 'micromark-factory-whitespace';
import { markdownLineEnding, markdownLineEndingOrSpace, markdownSpace, unicodePunctuation, unicodeWhitespace } from 'micromark-util-character';

// lib/factory-name.js

/**
 * @this {TokenizeContext}
 * @param {Effects} effects
 * @param {State} ok
 * @param {State} nok
 * @param {TokenType} type
 */
export function factoryName(this: TokenizeContext, effects: Effects, ok: State, nok: State, type: TokenType): State {
  const self = this;
  return start;

  /** @type {State} */
  function start(code: Code): State | undefined {
    if (code === null || markdownLineEnding(code) || unicodePunctuation(code) || unicodeWhitespace(code)) {
      return nok(code);
    }
    effects.enter(type);
    effects.consume(code);
    return name;
  }

  /** @type {State} */
  function name(code: Code): State | undefined {
    if (code === null || markdownLineEnding(code) || unicodeWhitespace(code) || unicodePunctuation(code) && code !== 45 && code !== 95) {
      effects.exit(type);
      return self.previous === 45 || self.previous === 95 ? nok(code) : ok(code);
    }
    effects.consume(code);
    return name;
  }
}

// lib/factory-label.js

// This is a fork of:
// <https://github.com/micromark/micromark/tree/main/packages/micromark-factory-label>
// to allow empty labels, balanced brackets (such as for nested directives),
// text instead of strings, and optionally disallows EOLs.

/**
 * @param {Effects} effects
 * @param {State} ok
 * @param {State} nok
 * @param {TokenType} type
 * @param {TokenType} markerType
 * @param {TokenType} stringType
 * @param {boolean | undefined} [disallowEol=false]
 */
export function factoryLabel(effects: Effects, ok: State, nok: State, type: TokenType, markerType: TokenType, stringType: TokenType, disallowEol?: boolean | undefined): State {
  let size = 0;
  let balance = 0;
  /** @type {Token | undefined} */
  let previous: Token | undefined;
  return start;

  /** @type {State} */
  function start(code: Code): State | undefined {
    effects.enter(type);
    effects.enter(markerType);
    effects.consume(code);
    effects.exit(markerType);
    return afterStart;
  }

  /** @type {State} */
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

  /** @type {State} */
  function lineStart(code: Code): State | undefined {
    if (code === 93 && !balance) {
      return atClosingBrace(code);
    }
    const token = effects.enter("chunkText", {
      _contentTypeTextTrailing: true,
      contentType: "text",
      previous
    });
    if (previous) previous.next = token;
    previous = token;
    return data(code);
  }

  /** @type {State} */
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

  /** @type {State} */
  function dataEscape(code: Code): State | undefined {
    if (code === 91 || code === 92 || code === 93) {
      effects.consume(code);
      size++;
      return data;
    }
    return data(code);
  }

  /** @type {State} */
  function atClosingBrace(code: Code): State | undefined {
    effects.exit(stringType);
    effects.enter(markerType);
    effects.consume(code);
    effects.exit(markerType);
    effects.exit(type);
    return ok;
  }
}

// lib/factory-attributes.js

/**
 * @param {Effects} effects
 * @param {State} ok
 * @param {State} nok
 * @param {TokenType} attributesType
 * @param {TokenType} attributesMarkerType
 * @param {TokenType} attributeType
 * @param {TokenType} attributeIdType
 * @param {TokenType} attributeClassType
 * @param {TokenType} attributeNameType
 * @param {TokenType} attributeInitializerType
 * @param {TokenType} attributeValueLiteralType
 * @param {TokenType} attributeValueType
 * @param {TokenType} attributeValueMarker
 * @param {TokenType} attributeValueData
 * @param {boolean | undefined} [disallowEol=false]
 */
export function factoryAttributes(effects: Effects, ok: State, nok: State, attributesType: TokenType, attributesMarkerType: TokenType, attributeType: TokenType, attributeIdType: TokenType, attributeClassType: TokenType, attributeNameType: TokenType, attributeInitializerType: TokenType, attributeValueLiteralType: TokenType, attributeValueType: TokenType, attributeValueMarker: TokenType, attributeValueData: TokenType, disallowEol?: boolean | undefined): State {
  /** @type {TokenType} */
  let type: TokenType;
  /** @type {Code | undefined} */
  let marker: Code | undefined;
  return start;

  /** @type {State} */
  function start(code: Code): State | undefined {
    effects.enter(attributesType);
    effects.enter(attributesMarkerType);
    effects.consume(code);
    effects.exit(attributesMarkerType);
    return between;
  }

  /** @type {State} */
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
    if (code === null || markdownLineEnding(code) || unicodeWhitespace(code) || unicodePunctuation(code) && code !== 45 && code !== 95) {
      return end(code);
    }
    effects.enter(attributeType);
    effects.enter(attributeNameType);
    effects.consume(code);
    return name;
  }

  /** @type {State} */
  function shortcutStart(code: Code): State | undefined {
    // Assume it’s registered.
    const markerType = (type + 'Marker') as TokenType;
    effects.enter(attributeType);
    effects.enter(type);
    effects.enter(markerType);
    effects.consume(code);
    effects.exit(markerType);
    return shortcutStartAfter;
  }

  /** @type {State} */
  function shortcutStartAfter(code: Code): State | undefined {
    if (code === null || code === 34 || code === 35 || code === 39 || code === 46 || code === 60 || code === 61 || code === 62 || code === 96 || code === 125 || markdownLineEndingOrSpace(code)) {
      return nok(code);
    }

    // Assume it’s registered.
    const valueType = (type + 'Value') as TokenType;
    effects.enter(valueType);
    effects.consume(code);
    return shortcut;
  }

  /** @type {State} */
  function shortcut(code: Code): State | undefined {
    if (code === null || code === 34 || code === 39 || code === 60 || code === 61 || code === 62 || code === 96) {
      return nok(code);
    }
    if (code === 35 || code === 46 || code === 125 || markdownLineEndingOrSpace(code)) {
      // Assume it’s registered.
      const valueType = (type + 'Value') as TokenType;
      effects.exit(valueType);
      effects.exit(type);
      effects.exit(attributeType);
      return between(code);
    }
    effects.consume(code);
    return shortcut;
  }

  /** @type {State} */
  function name(code: Code): State | undefined {
    if (code === null || markdownLineEnding(code) || unicodeWhitespace(code) || unicodePunctuation(code) && code !== 45 && code !== 46 && code !== 58 && code !== 95) {
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

  /** @type {State} */
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

  /** @type {State} */
  function valueBefore(code: Code): State | undefined {
    if (code === null || code === 60 || code === 61 || code === 62 || code === 96 || code === 125 || disallowEol && markdownLineEnding(code)) {
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

  /** @type {State} */
  function valueUnquoted(code: Code): State | undefined {
    if (code === null || code === 34 || code === 39 || code === 60 || code === 61 || code === 62 || code === 96) {
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

  /** @type {State} */
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

  /** @type {State} */
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

  /** @type {State} */
  function valueQuoted(code: Code): State | undefined {
    if (code === marker || code === null || markdownLineEnding(code)) {
      effects.exit(attributeValueData);
      return valueQuotedBetween(code);
    }
    effects.consume(code);
    return valueQuoted;
  }

  /** @type {State} */
  function valueQuotedAfter(code: Code): State | undefined {
    return code === 125 || markdownLineEndingOrSpace(code) ? between(code) : end(code);
  }

  /** @type {State} */
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
