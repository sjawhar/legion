// edit-cells.ts <base64 Yjs update>: loads the live document as the browser editor's sync plugin
// does (initProseMirrorDoc), types "x" at the end of the first header cell and "y" at the end of
// the first body cell, writes the edit back as that plugin does (updateYFragment, with the mapping
// that keeps untouched nodes), and prints the resulting update.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { Transform } from "prosemirror-transform";
import * as Y from "yjs";
import { initProseMirrorDoc, updateYFragment } from "y-prosemirror";
import { createHeadlessProof } from "@legion/proof-editor/headless";

const blockSchema = JSON.parse(readFileSync(join(import.meta.dir, "..", "schema", "blocks.json"), "utf8"));
const { schema } = await createHeadlessProof({ blockSchema });
const ydoc = new Y.Doc();
Y.applyUpdate(ydoc, Buffer.from(process.argv[2], "base64"));
const fragment = ydoc.getXmlFragment("prosemirror");
const { doc, meta } = initProseMirrorDoc(fragment, schema);

// The end of the first cell of each type's text: its paragraph's last position.
const cellEnd = (node: typeof doc, type: string) => {
  let end = -1;
  node.descendants((child, position) => {
    if (end < 0 && child.type.name === type) {
      end = position + child.nodeSize - 2;
    }
    return end < 0;
  });
  if (end < 0) {
    throw new Error(`no ${type} in the document`);
  }
  return end;
};
const tr = new Transform(doc);
tr.insert(cellEnd(tr.doc, "table_header"), schema.text("x"));
tr.insert(cellEnd(tr.doc, "table_cell"), schema.text("y"));
ydoc.transact(() => updateYFragment(ydoc, fragment, tr.doc, meta));
console.log(Buffer.from(Y.encodeStateAsUpdate(ydoc)).toString("base64"));
