// differential.ts - the browser editor's reading for the pmdoc differential (differential.sh).
//
//   bun differential.ts <in.jsonl> <out.jsonl> <field>...
//
// Each line of <in.jsonl> is a JSON object with an "id". For every named field holding a string,
// the fork's headless engine parses that string as markdown, as the browser editor imports a
// document, and the output line carries the field as {"pm": <ProseMirror JSON>} or {"err": ...};
// a field that is absent or null is written null. Lines keep their input order, so the Go side
// reads every file of one run in lockstep.
import { closeSync, openSync, readFileSync, writeSync } from "node:fs";
import { join } from "node:path";
import { setBlockIdGenerator } from "@legion/proof-editor";
import { createHeadlessProof } from "@legion/proof-editor/headless";

const [input, output, ...fields] = process.argv.slice(2);
if (!input || !output || fields.length === 0) {
  console.error("usage: bun differential.ts <in.jsonl> <out.jsonl> <field>...");
  process.exit(2);
}
const blockSchema = JSON.parse(
  readFileSync(join(import.meta.dir, "..", "schema", "blocks.json"), "utf8")
);
let blockNumber = 0;
setBlockIdGenerator(() => `b-${String(++blockNumber).padStart(6, "0")}`);
const engine = await createHeadlessProof({ blockSchema });

const out = openSync(output, "w");
for (const line of readFileSync(input, "utf8").split("\n")) {
  if (!line.trim()) continue;
  const record = JSON.parse(line);
  const read: Record<string, unknown> = { id: record.id };
  for (const field of fields) {
    const markdown = record[field];
    if (typeof markdown !== "string") {
      read[field] = null;
      continue;
    }
    blockNumber = 0;
    try {
      read[field] = { pm: engine.parseMarkdown(markdown).toJSON() };
    } catch (error) {
      read[field] = { err: String(error) };
    }
  }
  const text = `${JSON.stringify(read)}\n`;
  const length = Buffer.byteLength(text);
  // On a full disk writeSync writes what fits and returns the short count instead of throwing.
  const written = writeSync(out, text);
  if (written !== length) {
    throw new Error(`short write to ${output}: ${written} of ${length} bytes`);
  }
}
closeSync(out);
