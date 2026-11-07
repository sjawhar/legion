import { expect, test } from "bun:test";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";

import { quoteAnchor } from "./quote-anchor";

const schema = new Schema({
  marks: { strong: {} },
  nodes: {
    doc: { content: "paragraph+" },
    hard_break: { group: "inline", inline: true },
    image: { atom: true, group: "inline", inline: true },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
  },
});

function doc(...paragraphs: (string | ProseMirrorNode[])[]): ProseMirrorNode {
  return schema.node(
    "doc",
    undefined,
    paragraphs.map((content) =>
      schema.node(
        "paragraph",
        undefined,
        typeof content === "string" ? (content === "" ? [] : [schema.text(content)]) : content
      )
    )
  );
}

/** The range of the `nth` match, from zero, of `quote` in `node`'s text, found through
 *  ProseMirror's own positions so a test names the text it selects. */
function rangeOf(node: ProseMirrorNode, quote: string, nth = 0): { from: number; to: number } {
  const found: number[] = [];
  node.descendants((child, pos) => {
    for (let index = child.text?.indexOf(quote) ?? -1; index >= 0; ) {
      found.push(pos + index);
      index = child.text?.indexOf(quote, index + 1) ?? -1;
    }
    return true;
  });
  const from = found[nth];
  if (from === undefined) throw new Error(`no match ${nth} of ${quote}`);
  return { from, to: from + quote.length };
}

test("a selection's occurrence counts the matches before it across the document", () => {
  const document = doc("brown fox brown", "brown");
  const first = rangeOf(document, "brown", 0);
  const second = rangeOf(document, "brown", 1);
  const third = rangeOf(document, "brown", 2);
  expect(quoteAnchor(document, first.from, first.to)).toEqual({ occurrence: 0, quote: "brown" });
  expect(quoteAnchor(document, second.from, second.to)).toEqual({ occurrence: 1, quote: "brown" });
  expect(quoteAnchor(document, third.from, third.to)).toEqual({ occurrence: 2, quote: "brown" });
});

test("overlapping matches count, as the server's search steps one unit at a time", () => {
  const document = doc("aaaa");
  // "aa" at offsets 0, 1 and 2: the selection of the last two letters is the third match.
  expect(quoteAnchor(document, 3, 5)).toEqual({ occurrence: 2, quote: "aa" });
});

test("text that does not run on reads as one space, never at the quote's ends", () => {
  const document = doc("one two", "", "three");
  const two = rangeOf(document, "two");
  const three = rangeOf(document, "three");
  // An empty paragraph between still makes a single space.
  expect(quoteAnchor(document, two.from, three.from + 3)).toEqual({
    occurrence: 0,
    quote: "two thr",
  });
  // A selection from the end of one paragraph into the next starts at the next one's text.
  expect(quoteAnchor(document, two.to, three.to)).toEqual({ occurrence: 0, quote: "three" });
  expect(quoteAnchor(document, two.from, three.from)).toEqual({ occurrence: 0, quote: "two" });
});

test("a hard break or an inline atom reads as a space; marks do not split the text", () => {
  const breaks = doc([schema.text("a"), schema.node("hard_break"), schema.text("b")]);
  expect(quoteAnchor(breaks, 1, 4)).toEqual({ occurrence: 0, quote: "a b" });
  const pictured = doc([schema.text("a"), schema.node("image"), schema.text("b")]);
  expect(quoteAnchor(pictured, 1, 4)).toEqual({ occurrence: 0, quote: "a b" });
  const marked = doc([schema.text("bro", [schema.mark("strong")]), schema.text("wn")]);
  expect(quoteAnchor(marked, 1, 6)).toEqual({ occurrence: 0, quote: "brown" });
});

test("U+0000 reads as U+FFFD, as every read the server serves does", () => {
  expect(quoteAnchor(doc("x\u0000y"), 1, 4)).toEqual({ occurrence: 0, quote: "x\uFFFDy" });
});

test("a heading marker at the quote's start is left out, so it is not read as a heading's title", () => {
  const document = doc("Plan", "## Plan");
  const marked = rangeOf(document, "## Plan");
  expect(quoteAnchor(document, marked.from, marked.to)).toEqual({ occurrence: 1, quote: "Plan" });
  // A quote that is nothing but a marker is matched as text.
  const bare = doc("## ");
  expect(quoteAnchor(bare, 1, 4)).toEqual({ occurrence: 0, quote: "## " });
});

test("a selection that holds no text has no quote", () => {
  const document = doc([schema.node("image")], "after");
  expect(quoteAnchor(document, 1, 2)).toBeUndefined();
  expect(quoteAnchor(document, 2, 2)).toBeUndefined();
});
