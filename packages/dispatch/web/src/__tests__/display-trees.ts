/**
 * The readings the server would have sent, for a test that builds its own records.
 *
 * A view draws the tree Go read a stored text into, so a record a test invents needs one too.
 * Where the text is the test's own, `displayTree` reads it with the page's own engine: the two
 * readings are the same document, which is what the pmdoc differential holds, and the shape Go
 * writes is pinned by the fixture `pmdoc`'s `TestWebDisplayFixture` writes
 * (`features/refs/__fixtures__/display-trees.json`).
 */

import { snippetSegments } from "@legion/contracts/dispatch-snippet";

import type { DisplayTree } from "../api/types";
import { loadedEngine } from "../features/refs/markdown-engine";

/** The reading of `markdown` the server would send. `warmMarkdownRenderer` must have resolved
 *  before this is called; `undefined` for a text the engine refuses, as the server's own
 *  reading is never. */
export function displayTree(markdown: string): DisplayTree | undefined {
  return loadedEngine()?.parse(markdown)?.toJSON() as DisplayTree | undefined;
}

/**
 * The tree of a search hit's snippet, with the words the server matched carrying the highlight
 * mark: the shape `pmdoc.DisplaySnippetJSON` writes for a snippet of plain words, built here
 * from the `<mark>` runs the test's own snippet names (`snippetSegments`, the encoding the
 * server's answer uses). A hit whose snippet holds formatting belongs in the Go fixture
 * instead, which reads it as the server does.
 */
export function snippetTree(snippet: string): DisplayTree {
  const content = snippetSegments(snippet)
    .filter((segment) => segment.text !== "")
    .map((segment) =>
      segment.mark
        ? { marks: [{ type: "searchHighlight" }], text: segment.text, type: "text" }
        : { text: segment.text, type: "text" }
    );
  return {
    content: [{ attrs: { blockId: "b-000001" }, content, type: "paragraph" }],
    type: "doc",
  };
}
