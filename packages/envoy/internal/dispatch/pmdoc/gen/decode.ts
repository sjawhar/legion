// decode.ts <base64 Yjs update> <out>: writes to <out> the ProseMirror JSON of the document the
// browser editor holds for the update. The result goes to a file, never stdout (genResult,
// update_test.go).
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { createHeadlessProof } from "@legion/proof-editor/headless";
import { yXmlFragmentToProsemirrorJSON } from "y-prosemirror";
import * as Y from "yjs";

const [update, out] = process.argv.slice(2);
if (!update || !out) {
  console.error("usage: bun decode.ts <base64 Yjs update> <out>");
  process.exit(2);
}
const blockSchema = JSON.parse(
  readFileSync(join(import.meta.dir, "..", "schema", "blocks.json"), "utf8")
);
const { schema } = await createHeadlessProof({ blockSchema });
const ydoc = new Y.Doc();
Y.applyUpdate(ydoc, Buffer.from(update, "base64"));
// The document the browser editor holds: y-prosemirror builds each node with the schema, so an
// attribute the live document lacks takes the schema's default.
const root = yXmlFragmentToProsemirrorJSON(ydoc.getXmlFragment("prosemirror"));
writeFileSync(out, JSON.stringify(schema.nodeFromJSON(root).toJSON()));
