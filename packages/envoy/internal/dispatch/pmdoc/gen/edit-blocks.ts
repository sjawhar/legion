// edit-blocks.ts <base64 Yjs update> <out>: loads the live document as the browser editor's sync
// plugin does (initProseMirrorDoc), types "x" at the end of the first header cell, "y" at the end
// of the first body cell and "z" at the end of the first code block, sets the first image's alt
// text to "alt2", writes the edit back as that plugin does (updateYFragment, with the mapping that
// keeps untouched nodes), and writes the resulting update's bytes to <out>, never stdout
// (genResult, update_test.go). The plugin rewrites each node an edit touches with the attributes
// the editor holds, so each of those carries its schema's defaults into the live document.
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { createHeadlessProof } from "@legion/proof-editor/headless";
import { Transform } from "prosemirror-transform";
import { initProseMirrorDoc, updateYFragment } from "y-prosemirror";
import * as Y from "yjs";

const [update, out] = process.argv.slice(2);
if (!update || !out) {
  console.error("usage: bun edit-blocks.ts <base64 Yjs update> <out>");
  process.exit(2);
}
const blockSchema = JSON.parse(
  readFileSync(join(import.meta.dir, "..", "schema", "blocks.json"), "utf8")
);
const { schema } = await createHeadlessProof({ blockSchema });
const ydoc = new Y.Doc();
Y.applyUpdate(ydoc, Buffer.from(update, "base64"));
const fragment = ydoc.getXmlFragment("prosemirror");
const { doc, meta } = initProseMirrorDoc(fragment, schema);
const tr = new Transform(doc);

// The end of the text of the first node of type: a textblock's last position, or a cell's
// paragraph's.
const textEnd = (type: string) => {
  let end = -1;
  tr.doc.descendants((child, position) => {
    if (end < 0 && child.type.name === type) {
      end = position + child.nodeSize - (child.isTextblock ? 1 : 2);
    }
    return end < 0;
  });
  if (end < 0) {
    throw new Error(`no ${type} in the document`);
  }
  return end;
};
tr.insert(textEnd("table_header"), schema.text("x"));
tr.insert(textEnd("table_cell"), schema.text("y"));
tr.insert(textEnd("code_block"), schema.text("z"));
let image = -1;
tr.doc.descendants((child, position) => {
  if (image < 0 && child.type.name === "image") {
    image = position;
  }
  return image < 0;
});
if (image < 0) {
  throw new Error("no image in the document");
}
tr.setNodeMarkup(image, undefined, { ...tr.doc.nodeAt(image)?.attrs, alt: "alt2" });
ydoc.transact(() => updateYFragment(ydoc, fragment, tr.doc, meta));
writeFileSync(out, Y.encodeStateAsUpdate(ydoc));
