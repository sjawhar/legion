import { readFileSync } from "node:fs";
import { join } from "node:path";
import * as Y from "yjs";
import { yXmlFragmentToProsemirrorJSON } from "y-prosemirror";
import { createHeadlessProof } from "@sjawhar/proof-editor/headless";

const blockSchema = JSON.parse(readFileSync(join(import.meta.dir, "..", "schema", "blocks.json"), "utf8"));
const { schema } = await createHeadlessProof({ blockSchema });
const ydoc = new Y.Doc();
Y.applyUpdate(ydoc, Buffer.from(process.argv[2], "base64"));
const root = yXmlFragmentToProsemirrorJSON(ydoc.getXmlFragment("prosemirror"));
schema.nodeFromJSON(root);
console.log(JSON.stringify(root));
