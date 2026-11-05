// The heavy bodies this suite measures, and the generators that spell them. Two specs read them:
// `heavy-bodies.e2e.ts` draws them in a browser, `heavy-document-writes.e2e.ts` writes them
// through the API, and the two must measure the very same bytes - a shape changed in one spec
// alone would leave the other comparing a different body against the same numbers
// (sjawhar/legion#2519). This module imports neither Playwright nor a browser module, so a spec
// loads it for its shapes alone.
import { DISPATCH_BODY_MAX } from "@legion/contracts/dispatch-tools";

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
export const nestedThenBlankLines = padTo(nestedByIndentation(100), "\n", DISPATCH_BODY_MAX);
export const nestedThenTextLines = padTo(nestedByIndentation(100), "x\n", DISPATCH_BODY_MAX);
export const lazyQuote = `> x\n${"x\n".repeat(15_998)}`;
export const lazyListItem = `- x\n${"x\n".repeat(15_998)}`;
export const bracketRun = `${"[".repeat(15_000)}${"]".repeat(15_000)}`;
export const calloutBracketRun = `:::callout[${"[".repeat(15_000)}${"]".repeat(14_989)}`;
export const markerChain = `${"- ".repeat(15_999)}x`;
/** 49 items nested on one line, which Go accepts (49 lists and 49 items is 98 of its 100
 *  blocks), then blank lines to the cap. */
export const nested49ThenBlankLines = padTo(`${"- ".repeat(49)}x\n`, "\n", DISPATCH_BODY_MAX);
