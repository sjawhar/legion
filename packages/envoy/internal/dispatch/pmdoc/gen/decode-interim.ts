import * as Y from "yjs";
import { yXmlFragmentToProseMirrorRootNode } from "y-prosemirror";
import { schema } from "./interim.ts";

const ydoc = new Y.Doc();
Y.applyUpdate(ydoc, Buffer.from(process.argv[2], "base64"));
console.log(JSON.stringify(yXmlFragmentToProseMirrorRootNode(ydoc.getXmlFragment("prosemirror"), schema).toJSON()));
