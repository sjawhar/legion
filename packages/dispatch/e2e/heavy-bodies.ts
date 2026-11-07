// The heavy bodies this suite measures, and the generators that spell them. `heavy-bodies.e2e.ts`
// draws seven of them in a browser and `nested-directive.e2e.ts` the two bare-directive ones, so
// every body here is drawn by one spec or the other. Eight of the nine are written as documents
// through the API by Go, which spells them again in
// `packages/envoy/internal/dispatch/pmdoc/heavybodies`, where `pmdoc`'s own tests and
// `internal/dispatch/api`'s heavy-document-write test read them; Go spells three bodies more that
// no spec here draws, and `nestedThenTextLines` is this side's alone. The bodies both sides spell
// must be the very same bytes - a shape changed on one alone would leave the other comparing a
// different body against the same numbers (sjawhar/legion#2519) - and what holds them to that is
// the digest table `heavybodies/testdata/shared-bodies.json`: every body both sides spell, by its
// id, with its length in bytes and the SHA-256 of those bytes, and every body one side has alone.
// This side is held to it by `e2e/__tests__/heavy-bodies-digests.test.ts`, which walks every
// string export of this module, and Go's by `heavybodies/digests_test.go`, so neither side can
// gain, lose, rename or respell a body on its own. This module imports neither Playwright nor a
// browser module, so a spec loads it for its shapes alone.
import { DISPATCH_BLOCK_UNITS } from "@legion/contracts/dispatch-tools";

/** `head` followed by `filler` repeated, cut to exactly `length` characters. */
export function padTo(head: string, filler: string, length: number): string {
  const padding = filler.repeat(Math.ceil((length - head.length) / filler.length));
  return (head + padding).slice(0, length);
}

/** `count` list items, each nested one level deeper than the last by its indentation. */
export function nestedByIndentation(count: number): string {
  let body = "";
  for (let item = 0; item < count; item += 1) {
    body += `${"  ".repeat(item)}- a\n`;
  }
  return body;
}

/** 8,000 list items: the shape a reader meets most, and the one no parse fix brings under a
 *  second on the page's own thread. */
export const plainList = "- x\n".repeat(8_000);
export const nestedThenBlankLines = padTo(nestedByIndentation(100), "\n", DISPATCH_BLOCK_UNITS);
export const nestedThenTextLines = padTo(nestedByIndentation(100), "x\n", DISPATCH_BLOCK_UNITS);
export const lazyQuote = `> x\n${"x\n".repeat(15_998)}`;
export const bracketRun = `${"[".repeat(15_000)}${"]".repeat(15_000)}`;
export const calloutBracketRun = `:::callout[${"[".repeat(15_000)}${"]".repeat(14_989)}`;
export const markerChain = `${"- ".repeat(15_999)}x`;
/** 2,000 unclosed `:::a` openers and then a 21,000-character line: with the body below, what
 *  froze a reader's tab for about a minute and 15 s (INFRA-581). Both are drawn by
 *  `nested-directive.e2e.ts`, which spells the row titles they are measured under. */
export const directiveOpenersThenLongLine = `${":::a\n".repeat(2_000)}${"x".repeat(21_000)}`;
/** 1,000 nested `:::a` openers and then as many closing lines. */
export const directiveOpenersThenClosers = `${":::a\n".repeat(1_000)}${":::\n".repeat(1_000)}`;

/** Every body above under the id the digest table pins it to - the `ID` of the Go `Shape` or
 *  `NestedDirective` spelling the same bytes, or, for a body only this side has, the id the
 *  table lists it under. The specs import the named exports; `heavy-bodies-digests.test.ts`
 *  requires every string export of this module to stand here, so a body this side gains or
 *  loses without the table fails there. */
export const heavyBodies: Readonly<Record<string, string>> = {
  "plain-list": plainList,
  "lazy-quote": lazyQuote,
  "bracket-run": bracketRun,
  "callout-bracket-run": calloutBracketRun,
  "marker-chain": markerChain,
  "nested-100-items-then-blank-lines": nestedThenBlankLines,
  "nested-100-items-then-text-lines": nestedThenTextLines,
  "directive-openers-then-long-line": directiveOpenersThenLongLine,
  "directive-openers-then-closers": directiveOpenersThenClosers,
};
