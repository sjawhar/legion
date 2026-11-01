import type { HeadlessProofEditor } from "@legion/proof-editor/headless";
import { DOMSerializer, type Schema } from "prosemirror-model";

import type { MarkdownEngine } from "./markdown-engine";
import { buildDispatchReference, buildReferencePath, pictureReference } from "./routes";

/**
 * The half of the Markdown engine written against ProseMirror's own classes, behind the same
 * `import()` as the headless editor it serializes for (`markdown-engine.ts`, `loadEngine`): a
 * `DOMSerializer` cannot be built before that editor's schema exists, so parsing `prosemirror-
 * model` on the first route - a quarter of a megabyte, for a page that may render no Markdown at
 * all before the reader asks for some - buys nothing.
 */

/**
 * Proof's DOM serializer, except for a picture whose address is a Dispatch reference
 * (`pictureReference` in `routes.ts`, the reading `pictureView` shares): the browser cannot fetch
 * `dispatch://`, so an `image` node naming an artifact at a version
 * (`![shot.png](dispatch://KEY/artifact/shot-png@v1)`, the syntax a pasted picture is written in)
 * becomes an empty link to the artifact's page, marked `data-dispatch-picture` and captioned by
 * its `alt` or the artifact's slug, for `collectPictureAnchors` to hand to a `DispatchPicture`
 * (`useRenderedMarkdown`). One that pins no version, or names something other than an artifact,
 * is a reference like any other and becomes a link `collectReferenceAnchors` titles. A picture on
 * another website, or an address that is no reference, is serialized as Proof serializes it.
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
        if (named?.version === undefined) {
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

export function buildEngine(proof: HeadlessProofEditor): MarkdownEngine {
  return {
    serializer: pictureSerializer(proof.schema),
    parse(markdown, softBreaks = "space") {
      try {
        return proof.parseMarkdown(markdown, softBreaks);
      } catch {
        return undefined;
      }
    },
  };
}
