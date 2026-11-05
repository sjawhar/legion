import { documentFromJSON } from "@legion/proof-editor/document-json";
import type { HeadlessProofEditor } from "@legion/proof-editor/headless";
import { DOMSerializer, Schema } from "prosemirror-model";

import type { BlockSchema } from "../../api/types";
import type { MarkdownEngine, RenderSpecWithAttrs } from "./markdown-engine";
import type { DocumentJSON } from "./markdown-parse.worker";
import { buildDispatchReference, buildReferencePath, pictureReference } from "./routes";

/**
 * The half of the Markdown engine written against ProseMirror's own classes, behind the same
 * `import()` as the headless editor it serializes for (`markdown-engine.ts`, `loadEngine`): a
 * `DOMSerializer` cannot be built before that editor's schema exists, so parsing `prosemirror-
 * model` on the first route - a quarter of a megabyte, for a page that may render no Markdown at
 * all before the reader asks for some - buys nothing. The serializer's own `renderSpec`, which
 * the sliced painter opens a node with (`MarkdownBody`, `paintBlockInSlices`), travels on the
 * engine for the same reason: it is a static of that class, so a page that named it to call it
 * after the engine existed would parse the library before its first paint anyway. The display
 * schema a tree the server sent is drawn into (`displaySchemaOf`) and the drawing itself
 * (`buildEngine`'s `draw`) are built here for the same reason again: both name `Schema`, and
 * neither can run before the editor's own schema exists.
 */

/**
 * Proof's DOM serializer, except for a picture whose address is a Dispatch reference
 * (`pictureReference` in `routes.ts`, the reading `pictureView` shares): the browser cannot fetch
 * `dispatch://`, so an `image` node naming an artifact, at a version or not
 * (`![shot.png](dispatch://KEY/artifact/shot-png@v1)`, the syntax a pasted picture is written in,
 * or `![shot.png](dispatch://KEY/artifact/shot-png)`, its latest version) becomes an empty link to
 * the artifact's page, marked `data-dispatch-picture` and captioned by its `alt` or the
 * artifact's slug, for `collectPictureAnchors` to hand to a `DispatchPicture`
 * (`useRenderedMarkdown`). One that names something other than an artifact is a reference like
 * any other and becomes a link `collectReferenceAnchors` titles. A picture on another website, or
 * an address that is no reference, is serialized as Proof serializes it.
 */
function pictureSerializer(schema: Schema): DOMSerializer {
  const base = DOMSerializer.fromSchema(schema);
  const image = base.nodes.image;
  if (image === undefined) {
    return base;
  }
  return new DOMSerializer(
    {
      ...base.nodes,
      image: (node) => {
        const picture = pictureReference(node.attrs);
        if (picture === undefined) {
          return image(node);
        }
        const { named, route } = picture;
        if (named === undefined) {
          return ["a", { href: picture.source }];
        }
        return [
          "a",
          {
            "data-dispatch-picture": buildDispatchReference(route),
            "data-picture-caption": picture.caption,
            href: buildReferencePath(route),
          },
        ];
      },
    },
    base.marks
  );
}

/** The mark Go puts over the words a search matched in a snippet's tree
 *  (`pmdoc.DisplayHighlightMark`). It is a mark of the display tree alone: no document stores
 *  it, no write accepts it, and Proof's own schema therefore has no node for it. */
const DISPLAY_HIGHLIGHT_MARK = "searchHighlight";

/** Proof's schema with the search highlight beside its own marks, which is what a tree the
 *  server sent is drawn into: a snippet's tree carries that mark over every run the search
 *  matched, and `Node.fromJSON` refuses a mark its schema does not know. It draws as a `<mark>`
 *  the surface styles itself (`MarkdownPreview`'s `highlightClassName`), so one serializer
 *  serves every surface. Every other node and mark is Proof's own, so a document of either
 *  schema serializes identically. */
function displaySchemaOf(schema: Schema): Schema {
  return new Schema({
    marks: schema.spec.marks.addToEnd(DISPLAY_HIGHLIGHT_MARK, {
      toDOM: () => ["mark", { "data-search-highlight": "" }],
    }),
    nodes: schema.spec.nodes,
    topNode: schema.spec.topNode,
  });
}

export function buildEngine(proof: HeadlessProofEditor, blockSchema: BlockSchema): MarkdownEngine {
  const displaySchema = displaySchemaOf(proof.schema);
  return {
    blockSchema,
    displaySchema,
    schema: proof.schema,
    renderSpec: DOMSerializer.renderSpec as RenderSpecWithAttrs,
    serializer: pictureSerializer(displaySchema),
    draw(tree) {
      try {
        // Built node by node rather than through `Node.fromJSON`, for the reason
        // `@legion/proof-editor/document-json` gives: that checks every attribute against the
        // type its schema declares, and a value this page's own parser writes can fail the
        // check - milkdown writes `title: null` for an image with no title, where `image.title`
        // validates as a string. Go's reading of a text is that same document, so it carries
        // the same attribute, and the check is for JSON from somewhere else than these two
        // readers. `DisplayTree` is the same JSON `DocumentJSON` describes - one ProseMirror
        // node - typed openly because a response carries whatever the server's schema wrote.
        const node = tree as unknown as DocumentJSON;
        return documentFromJSON(displaySchema, node);
      } catch (error) {
        console.error("Drawing a stored text, the server's tree does not fit the schema", error);
        return undefined;
      }
    },
    parse(markdown, softBreaks = "space") {
      try {
        return proof.parseMarkdown(markdown, softBreaks);
      } catch {
        return undefined;
      }
    },
  };
}
