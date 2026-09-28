import { readFileSync } from "node:fs";
import { join } from "node:path";
import * as Y from "yjs";
import { yXmlFragmentToProsemirrorJSON } from "y-prosemirror";
import { createHeadlessProof } from "@legion/proof-editor/headless";

const blockSchema = JSON.parse(readFileSync(join(import.meta.dir, "..", "schema", "blocks.json"), "utf8"));
const { schema } = await createHeadlessProof({ blockSchema });
const ydoc = new Y.Doc();
Y.applyUpdate(ydoc, Buffer.from(process.argv[2], "base64"));
// The document the browser editor holds: y-prosemirror builds each node with the schema, so an
// attribute the live document lacks takes the schema's default.
const root = yXmlFragmentToProsemirrorJSON(ydoc.getXmlFragment("prosemirror"));
console.log(JSON.stringify(schema.nodeFromJSON(root).toJSON()));
