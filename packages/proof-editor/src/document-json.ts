/**
 * A document as JSON, and the tolerant way this library rebuilds one.
 *
 * It is what `setMarkdown`'s counterpart, `setDocumentJSON`, takes: a document some other reader
 * of the same text has already built - Dispatch's Go reader of a stored text
 * (`pmdoc.ReadForDisplay`), or this library's own headless parser through `Node.toJSON()` - so
 * the editor draws it with no parser of its own.
 *
 * `Schema.nodeFromJSON` is not what builds it. That checks every attribute against the type its
 * schema declares, and both readers write values those declarations refuse: the commonmark
 * preset's image parser passes mdast's `title` through as `null` where `image.title` is declared
 * `{ default: '', validate: 'string' }`, and Go's reading of the same document carries that same
 * attribute, so a text holding a picture could never be rebuilt though every reader of it agrees
 * on what it says. That check is for JSON from somewhere else. `./editor/schema/leaf-marks.ts`
 * avoids `Node.fromJSON` and `doc.check()` for the same reason, and the Dispatch dashboard's own
 * builder of a tree the server sent (`features/refs/markdown-engine.ts`) is tolerant in the same
 * way.
 *
 * A node or mark type the schema does not carry is refused by name: a host that cannot draw a
 * document must say so, where an empty editor would read as an empty document.
 */
import type { Node as ProseMirrorNode, Schema } from '@milkdown/kit/prose/model';

/** One mark of a text node, as `Mark.toJSON()` writes it. */
export type DocumentMarkJSON = {
  readonly type: string;
  readonly attrs?: Record<string, unknown>;
};

/** One node of a document, as `Node.toJSON()` writes it. */
export type DocumentJSON = {
  readonly type: string;
  readonly attrs?: Record<string, unknown>;
  readonly content?: readonly DocumentJSON[];
  readonly marks?: readonly DocumentMarkJSON[];
  readonly text?: string;
};

/** The document `json` describes, as a document of `schema`, built node by node rather than
 *  checked against the schema's attribute declarations (see this module's doc). Throws for a node
 *  or mark type `schema` does not carry. */
export function documentFromJSON(schema: Schema, json: DocumentJSON): ProseMirrorNode {
  const marks = json.marks?.map((mark) => {
    const markType = schema.marks[mark.type];
    if (markType === undefined) throw new RangeError(`this editor has no mark type "${mark.type}"`);
    return markType.create(mark.attrs);
  });
  if (json.type === 'text') return schema.text(json.text ?? '', marks);
  const nodeType = schema.nodes[json.type];
  if (nodeType === undefined) throw new RangeError(`this editor has no node type "${json.type}"`);
  const content = json.content?.map((child) => documentFromJSON(schema, child));
  return nodeType.create(json.attrs, content, marks);
}
