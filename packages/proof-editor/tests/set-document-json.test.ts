import { expect, test } from "bun:test";
import type { DocumentJSON } from "../src/document-json";
import { createHeadlessProof } from "../src/lib-headless";
import { withMarksEditor } from "./marks-editor";

/** The reading another engine made of `markdown`, as JSON: the shape a host hands the editor
 *  when its server has already read the stored text for it. */
async function treeOf(markdown: string): Promise<DocumentJSON> {
  const { parseMarkdown } = await createHeadlessProof();
  return parseMarkdown(markdown).toJSON() as DocumentJSON;
}

/** `tree` with `mark` over the text node whose text is `quote`. */
function marked(tree: DocumentJSON, quote: string, mark: DocumentJSON["marks"]): DocumentJSON {
  if (tree.text === quote) return { ...tree, marks: mark };
  if (tree.content === undefined) return tree;
  return { ...tree, content: tree.content.map((child) => marked(child, quote, mark)) };
}

test("setDocumentJSON draws a tree the editor never parses, a picture's own attributes and all", async () => {
  const tree = await treeOf("![shot](shot.png)\n\nThe plan ships behind a canary today.");
  await withMarksEditor(
    "An older document.",
    ({ handle, view }) => {
      // The strict rebuild refuses this very tree: an image with no title carries `title: null`,
      // where the schema declares that attribute a string (./src/document-json.ts).
      expect(() => view.state.schema.nodeFromJSON(tree)).toThrow();

      handle.setDocumentJSON(tree);

      expect(view.dom.textContent).toBe("The plan ships behind a canary today.");
      expect(view.dom.querySelector("img")?.getAttribute("src")).toBe("shot.png");
    },
    { readOnly: true }
  );
});

test("setDocumentJSON keeps a mark the tree carries over its text", async () => {
  const tree = marked(
    await treeOf("The plan ships behind a canary today."),
    "The plan ships behind a canary today.",
    [{ attrs: { by: "user:alice", id: "comment-1" }, type: "proofComment" }]
  );
  await withMarksEditor(
    "An older document.",
    ({ handle, view }) => {
      handle.setDocumentJSON(tree);

      const span = view.dom.querySelector('span[data-proof="comment"]');
      expect(span?.getAttribute("data-id")).toBe("comment-1");
      expect(span?.textContent).toBe("The plan ships behind a canary today.");
    },
    { readOnly: true }
  );
});

test("setDocumentJSON refuses a type this editor has no schema for, rather than drawing nothing", async () => {
  await withMarksEditor(
    "An older document.",
    ({ handle, view }) => {
      expect(() => handle.setDocumentJSON({ content: [{ type: "sparkle" }], type: "doc" })).toThrow(
        'this editor has no node type "sparkle"'
      );
      expect(() =>
        handle.setDocumentJSON({
          content: [
            {
              content: [{ marks: [{ type: "sparkle" }], text: "text", type: "text" }],
              type: "paragraph",
            },
          ],
          type: "doc",
        })
      ).toThrow('this editor has no mark type "sparkle"');
      // The document the reader had is still the one on the page.
      expect(view.dom.textContent).toBe("An older document.");
    },
    { readOnly: true }
  );
});
