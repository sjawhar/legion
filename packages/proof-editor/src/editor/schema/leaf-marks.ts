/**
 * Marks live on text alone.
 *
 * An inline *leaf* - a node that is inline, holds no content of its own and is not text:
 * `html`, `image`, `hardbreak`, `footnote_reference` - may carry no mark, however many
 * `**`, `*`, `~~` or `[...](...)` wrap it in the markdown. The rule is structural rather than a
 * list of type names, because that is the shape of the two readers this engine must agree with:
 * Go's reader of the same document, which stores every Dispatch document, refuses a mark on any
 * node that is not text (`packages/envoy/internal/dispatch/pmdoc/node.go`), and the editor's own
 * collaborative store (y-prosemirror) drops such a mark on the next round trip. Without this, one
 * document draws one way in the editor and another way in a comment or a past version.
 *
 * Every parse seam applies it (`src/lib-headless.ts`'s `parseMarkdown`, and `src/lib.ts`'s shared
 * `parserCtx` and `setMarkdown`), beside `withBlockIds` rather than inside it: `withBlockIds`
 * returns its input unchanged when no block was missing an id, so a tree this walk had changed
 * would be dropped there.
 *
 * The walk builds its result with `node.mark` and `node.copy`, and never through
 * `Node.fromJSON` or `doc.check()`: both validate attributes, and the commonmark preset's own
 * image parser passes mdast's `title` through as `null` where its schema declares
 * `title: { default: '', validate: 'string' }`, so validating here would throw
 * `Expected value of type string for attribute title on type image, got null` for thousands of
 * ordinary documents - and a display surface turns such a throw into a body drawn as its literal
 * source.
 */
import { Fragment, type Node as ProseMirrorNode } from '@milkdown/kit/prose/model';

/**
 * `doc` with every inline leaf's marks removed; the input is returned unchanged - the same node,
 * allocating nothing - when it carries none, which is almost every document.
 */
export function withoutLeafMarks(doc: ProseMirrorNode): ProseMirrorNode {
  // A node that may hold no mark: inline, with no content of its own, and not text.
  if (doc.isLeaf) {
    return doc.isInline && !doc.isText && doc.marks.length > 0 ? doc.mark([]) : doc;
  }
  // Children are copied only once a child below has changed: up to that point the walk holds no
  // array at all, and a node whose descendants are all unchanged is returned as it came in.
  let children: ProseMirrorNode[] | null = null;
  for (let index = 0; index < doc.childCount; index += 1) {
    const child = doc.child(index);
    const next = withoutLeafMarks(child);
    if (next === child) {
      children?.push(child);
      continue;
    }
    if (children === null) {
      children = [];
      for (let earlier = 0; earlier < index; earlier += 1) children.push(doc.child(earlier));
    }
    children.push(next);
  }
  return children === null ? doc : doc.copy(Fragment.fromArray(children));
}
