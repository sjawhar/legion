import { expect, test } from "bun:test";
import type { DocumentJSON } from "@legion/proof-editor";

import { embedHighlight } from "./highlight";

/** A document of one paragraph holding `runs`, each a text node with the marks it names. */
function paragraph(...runs: readonly DocumentJSON[]): DocumentJSON {
  return { content: [{ content: runs, type: "paragraph" }], type: "doc" };
}

function text(value: string, ...marks: readonly string[]): DocumentJSON {
  return marks.length === 0
    ? { text: value, type: "text" }
    : { marks: marks.map((type) => ({ type })), text: value, type: "text" };
}

const comment = { attrs: { by: "user:alice", id: "c-1" }, type: "proofComment" };

test("embedHighlight marks a unique quote in the tree and keeps the marks it already carries", () => {
  expect(
    embedHighlight(paragraph(text("The quick brown fox")), {
      by: "user:alice",
      id: "c-1",
      quote: "brown",
    })
  ).toEqual(
    paragraph(text("The quick "), { marks: [comment], text: "brown", type: "text" }, text(" fox"))
  );

  // The quote crosses a link, as a reader's selection does: the run under it keeps its own mark.
  expect(
    embedHighlight(paragraph(text("see "), text("CORE-1", "link"), text(" now")), {
      by: "user:alice",
      id: "c-1",
      quote: "see CORE-1 no",
    })
  ).toEqual(
    paragraph(
      { marks: [comment], text: "see ", type: "text" },
      { marks: [{ type: "link" }, comment], text: "CORE-1", type: "text" },
      { marks: [comment], text: " no", type: "text" },
      text("w")
    )
  );
});

test("embedHighlight refuses a quote the document does not hold exactly once", () => {
  const twice: DocumentJSON = {
    content: [
      { content: [text("o o")], type: "paragraph" },
      { content: [text("one more o")], type: "paragraph" },
    ],
    type: "doc",
  };
  expect(embedHighlight(twice, { by: "u", id: "c", quote: "o" })).toBeUndefined();
  expect(
    embedHighlight(paragraph(text("text")), { by: "u", id: "c", quote: "gone" })
  ).toBeUndefined();
  // Two paragraphs are two rows: words running across their boundary are not a quote the
  // document holds.
  expect(embedHighlight(twice, { by: "u", id: "c", quote: "o oone" })).toBeUndefined();
});

test("embedHighlight leaves a document holding no quote exactly as it came", () => {
  const tree = paragraph(text("nothing to mark"));
  expect(embedHighlight(tree, { by: "u", id: "c", quote: "absent" })).toBeUndefined();
  expect(tree).toEqual(paragraph(text("nothing to mark")));
});
