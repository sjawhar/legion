// decode.ts - the ProseMirror JSON of the document the browser editor holds for a Yjs update.
//
//   bun decode.ts <in.jsonl> <out.jsonl>
//
// Each line of <in.jsonl> is a JSON object with an "id" and an "update", the base64 of a Yjs
// update v1. The output line carries the same id and {"pm": <ProseMirror JSON>}, in input order,
// so the Go side reads the two files in lockstep. One run answers every update: building the
// headless engine costs far more than a document's own decode, and a test over the fixtures used
// to pay it 142 times (genLines, update_test.go).
//
// The result goes to a file, never stdout (genResult, update_test.go).
import { closeSync, openSync, readFileSync, writeSync } from "node:fs";
import { join } from "node:path";
import { createHeadlessProof } from "@legion/proof-editor/headless";
import { yXmlFragmentToProsemirrorJSON } from "y-prosemirror";
import * as Y from "yjs";

const [input, output] = process.argv.slice(2);
if (!input || !output) {
  console.error("usage: bun decode.ts <in.jsonl> <out.jsonl>");
  process.exit(2);
}
const blockSchema = JSON.parse(
  readFileSync(join(import.meta.dir, "..", "schema", "blocks.json"), "utf8")
);
const { schema } = await createHeadlessProof({ blockSchema });

const out = openSync(output, "w");
for (const line of readFileSync(input, "utf8").split("\n")) {
  if (!line.trim()) continue;
  const record = JSON.parse(line);
  const ydoc = new Y.Doc();
  Y.applyUpdate(ydoc, Buffer.from(record.update, "base64"));
  // The document the browser editor holds: y-prosemirror builds each node with the schema, so an
  // attribute the live document lacks takes the schema's default.
  const root = yXmlFragmentToProsemirrorJSON(ydoc.getXmlFragment("prosemirror"));
  const text = `${JSON.stringify({ id: record.id, pm: schema.nodeFromJSON(root).toJSON() })}\n`;
  const length = Buffer.byteLength(text);
  // On a full disk writeSync writes what fits and returns the short count instead of throwing.
  const written = writeSync(out, text);
  if (written !== length) {
    throw new Error(`short write to ${output}: ${written} of ${length} bytes`);
  }
}
closeSync(out);
