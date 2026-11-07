// Run with bun, not by Playwright: Node can't load the proof editor's TypeScript dependencies
// from node_modules. Reads markdown on stdin and prints the headless engine's reading of it, the
// `DocumentReading` the pmdoc fixture table stores for every corpus file: each table's rows, as
// the text of their cells, and each paragraph's text with the text of the marks drawn in it.
import { readFileSync } from "node:fs";

import { createHeadlessProof } from "@legion/proof-editor/headless";
import { readDocument } from "@legion/proof-editor/reading";
import { Window } from "happy-dom";

const blockSchema = JSON.parse(
  readFileSync(
    new URL("../../envoy/internal/dispatch/pmdoc/schema/blocks.json", import.meta.url),
    "utf8"
  )
);
const engine = await createHeadlessProof({ blockSchema });
// The reading is the serialized document's DOM, so this command brings a document of its own. It
// is also the global one, because the commonmark preset's inline `html` node draws itself from
// `globalThis.document` rather than from the document the serializer hands its `toDOM`.
const window = new Window();
const document = window.document as unknown as Document;
Object.assign(globalThis, { document });
const reading = readDocument(engine.parseMarkdown(readFileSync(0, "utf8")), document);
process.stdout.write(JSON.stringify(reading));
await window.happyDOM.close();
