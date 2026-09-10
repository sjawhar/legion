import * as Y from "yjs";
import { yXmlFragmentToProsemirrorJSON } from "y-prosemirror";
import { createHeadlessProof } from "@sjawhar/proof-editor/headless";

const { schema } = await createHeadlessProof();
const ydoc = new Y.Doc();
Y.applyUpdate(ydoc, Buffer.from(process.argv[2], "base64"));
const root = yXmlFragmentToProsemirrorJSON(ydoc.getXmlFragment("prosemirror"));
schema.nodeFromJSON(root);
console.log(JSON.stringify(root));
